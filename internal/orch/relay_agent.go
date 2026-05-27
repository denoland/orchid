package orch

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/divy/orchid/cfrelaytun/go/cfrelaytun"
)

// RelayDeps bundles the callbacks the agent uses to push/serve local state.
type RelayDeps struct {
	URL, Token, HTTPSecret, LocalAddr string
	AllowedLogins                     []string
	Handler                           http.Handler
	StateWake                         <-chan struct{}
	StatePush                         func() []byte
	SnapRead                          func() []byte
	SnapWrite                         func([]byte) error
	PaneVM                            func(string) *VMBlock
}

// liveClient is the most recently constructed relay client. pushAllowedLogins
// + any future Send() callers use it to talk to the connected agent without
// threading the handle through every caller.
var liveClient atomic.Pointer[cfrelaytun.Client]

// allowedLoginsProvider returns the current allow list. Set at startup so
// reconnects pick up edits made through /api/config without a restart.
var allowedLoginsProvider func() []string

// runRelayAgent boots the cfrelaytun client wired with orchid-specific
// extension frames (pane mux, snap, state-update, subs, config) and blocks
// until ctx is cancelled. Reconnect/backoff is handled inside the lib.
func runRelayAgent(parent context.Context, d RelayDeps) {
	handler := d.Handler
	if d.HTTPSecret != "" {
		handler = injectBearer(handler, d.HTTPSecret)
	}

	panes := &paneMux{vmLookup: d.PaneVM, panes: map[string]*paneSub{}}
	var subsActive atomic.Bool
	subsActive.Store(true)
	subsCh := make(chan int, 1)

	c := cfrelaytun.New(cfrelaytun.Config{
		URL:         d.URL,
		Token:       d.Token,
		Handler:     handler,
		OnWSUpgrade: makeLocalWSDialer(d.LocalAddr, d.HTTPSecret),
		Logger:      log.Printf,
	})

	c.OnCtl("pane-sub", func(ctx context.Context, raw json.RawMessage) {
		var f struct {
			PaneID     string `json:"paneId"`
			Cols, Rows int
		}
		if json.Unmarshal(raw, &f) == nil && f.PaneID != "" {
			panes.start(ctx, c, f.PaneID, f.Cols, f.Rows)
		}
	})
	c.OnCtl("pane-unsub", func(_ context.Context, raw json.RawMessage) {
		var f struct {
			PaneID string `json:"paneId"`
		}
		if json.Unmarshal(raw, &f) == nil && f.PaneID != "" {
			panes.stop(f.PaneID)
		}
	})
	c.OnCtl("subs", func(_ context.Context, raw json.RawMessage) {
		var f struct {
			Count int `json:"count"`
		}
		if json.Unmarshal(raw, &f) == nil {
			subsActive.Store(f.Count > 0)
			select {
			case subsCh <- f.Count:
			default:
			}
		}
	})
	c.OnCtl("snap-put", func(_ context.Context, raw json.RawMessage) {
		if d.SnapWrite == nil {
			return
		}
		var f struct {
			Snap json.RawMessage `json:"snap"`
		}
		if json.Unmarshal(raw, &f) == nil && len(f.Snap) > 0 {
			_ = d.SnapWrite(f.Snap)
		}
	})

	c.OnConnect(func(ctx context.Context, sess cfrelaytun.Session) {
		logins := d.AllowedLogins
		if allowedLoginsProvider != nil {
			logins = allowedLoginsProvider()
		}
		_ = sess.SendCtl(ctx, "config", map[string]any{"allowed_logins": logins})
		if d.SnapRead != nil {
			if b := d.SnapRead(); len(b) > 0 && json.Valid(b) {
				_ = sess.SendCtl(ctx, "snap", map[string]json.RawMessage{
					"snap": json.RawMessage(b),
				})
			}
		}
		// New session = fresh subscriber count. Push next state on demand.
		subsActive.Store(true)
	})

	liveClient.Store(c)
	defer liveClient.CompareAndSwap(c, nil)

	if d.StateWake != nil && d.StatePush != nil {
		go statePushLoop(parent, c, d, &subsActive, subsCh)
	}

	_ = c.Run(parent)
}

// pushAllowedLogins writes a fresh config frame to the live agent so an
// allow-list edit applies without waiting for the next disconnect.
func pushAllowedLogins(logins []string) {
	c := liveClient.Load()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Send(ctx, "config", map[string]any{"allowed_logins": logins})
}

// statePushLoop publishes /api/state pushes to the relay. Coalesces wakes
// at 2s, deduplicates by hash, and skips when no subscribers are connected.
func statePushLoop(
	ctx context.Context,
	c *cfrelaytun.Client,
	d RelayDeps,
	subsActive *atomic.Bool,
	subsCh <-chan int,
) {
	var lastHash uint64
	emit := func(force bool) {
		if !force && !subsActive.Load() {
			return
		}
		body := d.StatePush()
		h := fnv64(string(body))
		if h == lastHash && !force {
			return
		}
		lastHash = h
		c.Send(ctx, "state-update", map[string]json.RawMessage{
			"state": json.RawMessage(body),
		})
	}
	emit(false)
	throttle := time.NewTicker(2 * time.Second)
	defer throttle.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.StateWake:
			pending = true
		case n := <-subsCh:
			if n > 0 {
				pending = false
				emit(true)
			}
		case <-throttle.C:
			if pending {
				pending = false
				emit(false)
			}
		}
	}
}

// injectBearer wraps h so every tunneled request carries the orch
// http_secret in Authorization, satisfying orch's per-endpoint auth gate
// without exposing the secret to public callers.
func injectBearer(h http.Handler, secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+secret)
		h.ServeHTTP(w, r)
	})
}

// ─── local WS dialer ──────────────────────────────────────────────────

// makeLocalWSDialer returns an OnWSUpgrade handler that bridges the relay
// tunnel to a WS dialed against the local orch listener. Strips hop-by-hop
// headers, injects bearer auth.
func makeLocalWSDialer(localAddr, secret string) cfrelaytun.WSHandler {
	if localAddr == "" {
		return nil
	}
	return func(ctx context.Context, req *http.Request, conn cfrelaytun.WSConn) error {
		host := localAddr
		if strings.HasPrefix(host, ":") {
			host = "127.0.0.1" + host
		}
		hdr := http.Header{}
		for k, vs := range req.Header {
			switch strings.ToLower(k) {
			case "upgrade", "connection", "sec-websocket-key",
				"sec-websocket-version", "sec-websocket-extensions", "host":
				continue
			}
			for _, v := range vs {
				hdr.Add(k, v)
			}
		}
		if secret != "" {
			hdr.Set("Authorization", "Bearer "+secret)
		}
		path := req.URL.RequestURI()
		local, _, err := websocket.Dial(ctx, "ws://"+host+path, &websocket.DialOptions{HTTPHeader: hdr})
		if err != nil {
			return err
		}
		defer local.CloseNow()

		bridgeCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		errCh := make(chan error, 2)

		// local → relay
		go func() {
			for {
				mt, data, err := local.Read(bridgeCtx)
				if err != nil {
					errCh <- err
					return
				}
				switch mt {
				case websocket.MessageText:
					if e := conn.WriteText(bridgeCtx, string(data)); e != nil {
						errCh <- e
						return
					}
				case websocket.MessageBinary:
					if e := conn.WriteBinary(bridgeCtx, data); e != nil {
						errCh <- e
						return
					}
				}
			}
		}()
		// relay → local
		go func() {
			for {
				kind, data, err := conn.NextFrame(bridgeCtx)
				if err != nil {
					errCh <- err
					return
				}
				switch kind {
				case "text":
					if e := local.Write(bridgeCtx, websocket.MessageText, data); e != nil {
						errCh <- e
						return
					}
				case "binary":
					if e := local.Write(bridgeCtx, websocket.MessageBinary, data); e != nil {
						errCh <- e
						return
					}
				case "close":
					errCh <- nil
					return
				}
			}
		}()
		<-errCh
		return nil
	}
}

// ─── pane streams ─────────────────────────────────────────────────────

type paneSub struct {
	refs   int
	cancel context.CancelFunc
}

type paneMux struct {
	mu       sync.Mutex
	vmLookup func(string) *VMBlock
	panes    map[string]*paneSub
}

func (p *paneMux) start(ctx context.Context, c *cfrelaytun.Client, id string, cols, rows int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.panes[id]; ok {
		existing.refs++
		return
	}
	paneCtx, cancel := context.WithCancel(ctx)
	p.panes[id] = &paneSub{refs: 1, cancel: cancel}
	go p.runCapture(paneCtx, c, id, cols, rows)
}

func (p *paneMux) stop(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.panes[id]
	if !ok {
		return
	}
	s.refs--
	if s.refs <= 0 {
		s.cancel()
		delete(p.panes, id)
	}
}

func validPaneID(id string) bool {
	for _, c := range id {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return id != ""
}

// runCapture spawns `tmux capture-pane` in a loop on the target VM,
// gzipping each frame and sending it over the relay tunnel. Frames are
// delimited by 0x1e (record separator); duplicates are dropped.
func (p *paneMux) runCapture(ctx context.Context, c *cfrelaytun.Client, id string, cols, rows int) {
	if !validPaneID(id) || p.vmLookup == nil {
		return
	}
	vm := p.vmLookup(id)
	if vm == nil {
		return
	}
	if cols < 40 {
		cols = 80
	}
	if rows < 10 {
		rows = 24
	}
	_, _, _ = sshExec(*vm, fmt.Sprintf("tmux resize-window -t %s -x %d -y %d 2>/dev/null", id, cols, rows))

	script := fmt.Sprintf(
		`while :; do tmux capture-pane -p -e -t %s -S -200 2>&1; printf '\x1e'; sleep 0.2; done`,
		id,
	)
	var cmd *exec.Cmd
	if isLocal(*vm) {
		cmd = exec.CommandContext(ctx, "bash", "-c", script)
	} else {
		cmd = exec.CommandContext(ctx, "ssh", append(sshArgs(*vm), script)...)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	rd := bufio.NewReader(stdout)
	var buf strings.Builder
	gzbuf := new(bytes.Buffer)
	gzw := gzip.NewWriter(gzbuf)
	var last string

	for ctx.Err() == nil {
		b, err := rd.ReadByte()
		if err != nil {
			return
		}
		if b != 0x1e {
			buf.WriteByte(b)
			continue
		}
		snap := buf.String()
		buf.Reset()
		if snap == last {
			continue
		}
		last = snap
		gzbuf.Reset()
		gzw.Reset(gzbuf)
		_, _ = gzw.Write([]byte(snap))
		_ = gzw.Close()
		data := "z:" + base64.StdEncoding.EncodeToString(gzbuf.Bytes())
		if !c.Send(ctx, "pane", map[string]any{
			"paneId": id,
			"data":   data,
		}) {
			return
		}
	}
}


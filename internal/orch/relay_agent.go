package orch

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
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

// runRelayAgent dials the relay and runs sessions in a loop with backoff.
// Session >30s resets backoff; faster failures double it up to 15s.
func runRelayAgent(parent context.Context, d RelayDeps) {
	backoff := 500 * time.Millisecond
	for parent.Err() == nil {
		start := time.Now()
		err := relaySession(parent, d)
		if time.Since(start) > 30*time.Second {
			backoff = 500 * time.Millisecond
		} else if backoff < 15*time.Second {
			backoff *= 2
		}
		if err != nil {
			log.Printf("relay: %v (reconnecting in %s)", err, backoff)
		}
		select {
		case <-parent.Done():
			return
		case <-time.After(backoff):
		}
	}
}

func relaySession(ctx context.Context, d RelayDeps) error {
	u, err := url.Parse(d.URL)
	if err != nil {
		return fmt.Errorf("parse relay url: %w", err)
	}
	q := u.Query()
	q.Set("token", d.Token)
	u.RawQuery = q.Encode()

	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Agent-Token": []string{d.Token},
			"User-Agent":    []string{"orchid-agent/1.0"},
		},
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	log.Printf("relay: connected to %s", d.URL)

	a := &agent{
		conn:    conn,
		deps:    d,
		streams: map[uint32]*agentStream{},
		ws:      map[uint32]*wsStream{},
		panes:   map[string]*paneSub{},
		subs:    make(chan int, 1),
	}
	a.subsActive.Store(true)

	logins := d.AllowedLogins
	if allowedLoginsProvider != nil {
		logins = allowedLoginsProvider()
	}
	_ = a.sendCtl(ctx, ctlFrame{T: "config", AllowedLogins: logins})

	setLiveAgent(conn)
	defer clearLiveAgent(conn)

	if d.SnapRead != nil {
		if b := d.SnapRead(); len(b) > 0 && json.Valid(b) {
			_ = a.sendCtl(ctx, ctlFrame{T: "snap", Snap: json.RawMessage(b)})
		}
	}

	pingCtx, cancelPing := context.WithCancel(ctx)
	defer cancelPing()
	go a.pingLoop(pingCtx)
	if d.StateWake != nil && d.StatePush != nil {
		go a.statePushLoop(pingCtx)
	}

	return a.loop(ctx)
}

// ─── agent ────────────────────────────────────────────────────────────

type agent struct {
	conn *websocket.Conn
	deps RelayDeps

	subs       chan int
	subsActive atomic.Bool

	mu      sync.Mutex
	streams map[uint32]*agentStream
	ws      map[uint32]*wsStream

	panesMu sync.Mutex
	panes   map[string]*paneSub
}

type agentStream struct {
	bodyR *io.PipeReader
	bodyW *io.PipeWriter
}
type wsStream struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
}
type paneSub struct {
	refs   int
	cancel context.CancelFunc
}

// ctlFrame is the JSON envelope on the control channel. Binary frames carry
// stream bodies and start with a 4-byte big-endian stream id.
type ctlFrame struct {
	T             string          `json:"t"`
	ID            uint32          `json:"id,omitempty"`
	Method        string          `json:"method,omitempty"`
	Path          string          `json:"path,omitempty"`
	Headers       [][]string      `json:"headers,omitempty"`
	HasBody       bool            `json:"hasBody,omitempty"`
	Status        int             `json:"status,omitempty"`
	UserID        int             `json:"userId,omitempty"`
	Login         string          `json:"login,omitempty"`
	Stream        bool            `json:"streaming,omitempty"`
	Data          string          `json:"data,omitempty"`
	Code          int             `json:"code,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	Count         int             `json:"count,omitempty"`
	PaneID        string          `json:"paneId,omitempty"`
	Cols          int             `json:"cols,omitempty"`
	Rows          int             `json:"rows,omitempty"`
	State         json.RawMessage `json:"state,omitempty"`
	Snap          json.RawMessage `json:"snap,omitempty"`
	AllowedLogins []string        `json:"allowed_logins,omitempty"`
}

func (a *agent) sendCtl(ctx context.Context, f ctlFrame) error {
	b, _ := json.Marshal(f)
	return a.conn.Write(ctx, websocket.MessageText, b)
}

func (a *agent) sendBin(ctx context.Context, id uint32, data []byte) error {
	out := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(out[:4], id)
	copy(out[4:], data)
	return a.conn.Write(ctx, websocket.MessageBinary, out)
}

func (a *agent) pingLoop(ctx context.Context) {
	t := time.NewTicker(25 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_ = a.conn.Ping(pctx)
			cancel()
		}
	}
}

// statePushLoop publishes /api/state pushes to the relay. Coalesces wakes
// at 2s, deduplicates by hash, and skips when no subscribers are connected.
func (a *agent) statePushLoop(ctx context.Context) {
	var lastHash uint64
	emit := func(force bool) {
		if !force && !a.subsActive.Load() {
			return
		}
		body := a.deps.StatePush()
		h := fnv64(string(body))
		if h == lastHash && !force {
			return
		}
		lastHash = h
		_ = a.sendCtl(ctx, ctlFrame{T: "state-update", State: json.RawMessage(body)})
	}
	emit(false)
	throttle := time.NewTicker(2 * time.Second)
	defer throttle.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.deps.StateWake:
			pending = true
		case n := <-a.subs:
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

func (a *agent) loop(ctx context.Context) error {
	for {
		mt, data, err := a.conn.Read(ctx)
		if err != nil {
			return err
		}
		switch mt {
		case websocket.MessageText:
			var f ctlFrame
			if err := json.Unmarshal(data, &f); err == nil {
				a.onCtl(ctx, &f)
			}
		case websocket.MessageBinary:
			if len(data) < 4 {
				continue
			}
			sid := binary.BigEndian.Uint32(data[:4])
			a.mu.Lock()
			s, ws := a.streams[sid], a.ws[sid]
			a.mu.Unlock()
			switch {
			case ws != nil:
				_ = ws.conn.Write(ctx, websocket.MessageBinary, data[4:])
			case s != nil:
				_, _ = s.bodyW.Write(data[4:])
			}
		}
	}
}

func (a *agent) onCtl(ctx context.Context, f *ctlFrame) {
	switch f.T {
	case "hello":
		log.Printf("relay: hello as %s (uid %d)", f.Login, f.UserID)
	case "req":
		pr, pw := io.Pipe()
		s := &agentStream{bodyR: pr, bodyW: pw}
		a.mu.Lock()
		a.streams[f.ID] = s
		a.mu.Unlock()
		if !f.HasBody {
			_ = pw.Close()
		}
		go a.handleReq(ctx, f, s)
	case "req-end":
		if s := a.takeStream(f.ID, false); s != nil && s.bodyW != nil {
			_ = s.bodyW.Close()
		}
	case "cancel":
		if s := a.takeStream(f.ID, true); s != nil && s.bodyW != nil {
			_ = s.bodyW.CloseWithError(io.ErrUnexpectedEOF)
		}
	case "ws-open":
		go a.handleWSOpen(ctx, f)
	case "ws-text":
		a.mu.Lock()
		ws := a.ws[f.ID]
		a.mu.Unlock()
		if ws != nil {
			_ = ws.conn.Write(ctx, websocket.MessageText, []byte(f.Data))
		}
	case "ws-close":
		a.mu.Lock()
		ws, ok := a.ws[f.ID]
		if ok {
			delete(a.ws, f.ID)
		}
		a.mu.Unlock()
		if ok {
			code := websocket.StatusNormalClosure
			if f.Code != 0 {
				code = websocket.StatusCode(f.Code)
			}
			_ = ws.conn.Close(code, f.Reason)
			ws.cancel()
		}
	case "pane-sub":
		if f.PaneID != "" {
			a.startPane(ctx, f.PaneID, f.Cols, f.Rows)
		}
	case "pane-unsub":
		if f.PaneID != "" {
			a.stopPane(f.PaneID)
		}
	case "subs":
		a.subsActive.Store(f.Count > 0)
		select {
		case a.subs <- f.Count:
		default:
		}
	case "snap-put":
		if a.deps.SnapWrite != nil && len(f.Snap) > 0 {
			_ = a.deps.SnapWrite([]byte(f.Snap))
		}
	}
}

func (a *agent) takeStream(id uint32, remove bool) *agentStream {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.streams[id]
	if remove {
		delete(a.streams, id)
	}
	return s
}

// ─── HTTP tunnel ─────────────────────────────────────────────────────

func (a *agent) handleReq(ctx context.Context, f *ctlFrame, s *agentStream) {
	defer func() {
		a.mu.Lock()
		delete(a.streams, f.ID)
		a.mu.Unlock()
	}()

	req, err := http.NewRequestWithContext(ctx, f.Method, f.Path, s.bodyR)
	if err != nil {
		_ = a.sendCtl(ctx, ctlFrame{T: "res-head", ID: f.ID, Status: 500})
		_ = a.sendCtl(ctx, ctlFrame{T: "res-end", ID: f.ID})
		return
	}
	for _, h := range f.Headers {
		if len(h) >= 2 {
			req.Header.Add(h[0], h[1])
		}
	}
	if a.deps.HTTPSecret != "" {
		req.Header.Set("Authorization", "Bearer "+a.deps.HTTPSecret)
	}

	rw := &relayWriter{id: f.ID, agent: a, ctx: ctx, header: http.Header{}, status: 200}
	a.deps.Handler.ServeHTTP(rw, req)
	rw.finish()
}

// relayWriter streams handler responses back as one head frame + N binary
// body frames + a res-end. Streaming flag set on first Flush so the relay
// forwards chunks immediately (SSE).
type relayWriter struct {
	id     uint32
	agent  *agent
	ctx    context.Context
	header http.Header
	status int
	sent   bool
	stream bool
	mu     sync.Mutex
}

func (w *relayWriter) Header() http.Header { return w.header }
func (w *relayWriter) WriteHeader(s int)   { w.status = s }
func (w *relayWriter) Write(b []byte) (int, error) {
	w.flushHead()
	if err := w.agent.sendBin(w.ctx, w.id, b); err != nil {
		return 0, err
	}
	return len(b), nil
}
func (w *relayWriter) Flush() { w.stream = true; w.flushHead() }
func (w *relayWriter) flushHead() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sent {
		return
	}
	w.sent = true
	hdrs := make([][]string, 0, len(w.header))
	for k, vs := range w.header {
		for _, v := range vs {
			hdrs = append(hdrs, []string{k, v})
		}
	}
	_ = w.agent.sendCtl(w.ctx, ctlFrame{
		T: "res-head", ID: w.id, Status: w.status,
		Headers: hdrs, Stream: w.stream,
	})
}
func (w *relayWriter) finish() {
	w.flushHead()
	_ = w.agent.sendCtl(w.ctx, ctlFrame{T: "res-end", ID: w.id})
}

// ─── WS tunnel ───────────────────────────────────────────────────────

// handleWSOpen dials orch's local listener and bridges frames between the
// local WS and the relay tunnel. Bearer auth is injected so orch's gate
// passes for tunneled connections.
func (a *agent) handleWSOpen(parent context.Context, f *ctlFrame) {
	if a.deps.LocalAddr == "" {
		_ = a.sendCtl(parent, ctlFrame{T: "ws-close", ID: f.ID, Code: 1011, Reason: "no local addr"})
		return
	}
	host := a.deps.LocalAddr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	hdr := http.Header{}
	for _, h := range f.Headers {
		if len(h) < 2 {
			continue
		}
		switch strings.ToLower(h[0]) {
		case "upgrade", "connection", "sec-websocket-key",
			"sec-websocket-version", "sec-websocket-extensions", "host":
			continue
		}
		hdr.Add(h[0], h[1])
	}
	if a.deps.HTTPSecret != "" {
		hdr.Set("Authorization", "Bearer "+a.deps.HTTPSecret)
	}

	dialCtx, cancel := context.WithCancel(parent)
	conn, _, err := websocket.Dial(dialCtx, "ws://"+host+f.Path, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		cancel()
		_ = a.sendCtl(parent, ctlFrame{T: "ws-close", ID: f.ID, Code: 1011, Reason: err.Error()})
		return
	}

	a.mu.Lock()
	a.ws[f.ID] = &wsStream{conn: conn, cancel: cancel}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.ws, f.ID)
		a.mu.Unlock()
		_ = conn.CloseNow()
		cancel()
	}()

	for {
		mt, data, err := conn.Read(dialCtx)
		if err != nil {
			code := 1000
			if cs := websocket.CloseStatus(err); cs != -1 {
				code = int(cs)
			}
			_ = a.sendCtl(parent, ctlFrame{T: "ws-close", ID: f.ID, Code: code})
			return
		}
		switch mt {
		case websocket.MessageText:
			_ = a.sendCtl(parent, ctlFrame{T: "ws-text", ID: f.ID, Data: string(data)})
		case websocket.MessageBinary:
			_ = a.sendBin(parent, f.ID, data)
		}
	}
}

// ─── pane streams ─────────────────────────────────────────────────────

func (a *agent) startPane(ctx context.Context, id string, cols, rows int) {
	a.panesMu.Lock()
	defer a.panesMu.Unlock()
	if p, ok := a.panes[id]; ok {
		p.refs++
		return
	}
	paneCtx, cancel := context.WithCancel(ctx)
	a.panes[id] = &paneSub{refs: 1, cancel: cancel}
	go a.runPaneCapture(paneCtx, id, cols, rows)
}

func (a *agent) stopPane(id string) {
	a.panesMu.Lock()
	defer a.panesMu.Unlock()
	p, ok := a.panes[id]
	if !ok {
		return
	}
	p.refs--
	if p.refs <= 0 {
		p.cancel()
		delete(a.panes, id)
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

// runPaneCapture spawns `tmux capture-pane` in a loop on the target VM,
// gzipping each frame and sending it over the relay tunnel. Frames are
// delimited by 0x1e (record separator); duplicates are dropped.
func (a *agent) runPaneCapture(ctx context.Context, id string, cols, rows int) {
	if !validPaneID(id) || a.deps.PaneVM == nil {
		return
	}
	vm := a.deps.PaneVM(id)
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
		if err := a.sendCtl(ctx, ctlFrame{T: "pane", PaneID: id, Data: data}); err != nil {
			return
		}
	}
}

// ─── live agent handle ────────────────────────────────────────────────

// allowedLoginsProvider returns the current allow list. Set at startup so
// reconnects pick up edits made through /api/config without a restart.
var allowedLoginsProvider func() []string

var (
	liveMu sync.Mutex
	live   *websocket.Conn
)

func setLiveAgent(c *websocket.Conn) { liveMu.Lock(); live = c; liveMu.Unlock() }
func clearLiveAgent(c *websocket.Conn) {
	liveMu.Lock()
	if live == c {
		live = nil
	}
	liveMu.Unlock()
}

// pushAllowedLogins writes a fresh config frame to the live agent so an
// allow-list edit applies without waiting for the next disconnect.
func pushAllowedLogins(logins []string) {
	liveMu.Lock()
	c := live
	liveMu.Unlock()
	if c == nil {
		return
	}
	b, _ := json.Marshal(ctlFrame{T: "config", AllowedLogins: logins})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, b)
}

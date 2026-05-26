package main

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

func runRelayAgent(parent context.Context, relayURL, token, httpSecret, localAddr string, allowedLogins []string, handler http.Handler, stateWake <-chan struct{}, statePush func() []byte, snapRead func() []byte, snapWrite func([]byte) error, paneVM func(string) *VMBlock) {
	backoff := 500 * time.Millisecond
	for {
		if parent.Err() != nil {
			return
		}
		sessionStart := time.Now()
		err := relayAgentSession(parent, relayURL, token, httpSecret, localAddr, allowedLogins, handler, stateWake, statePush, snapRead, snapWrite, paneVM)
		if time.Since(sessionStart) > 30*time.Second {
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

func relayAgentSession(ctx context.Context, relayURL, token, httpSecret, localAddr string, allowedLogins []string, handler http.Handler, stateWake <-chan struct{}, statePush func() []byte, snapRead func() []byte, snapWrite func([]byte) error, paneVM func(string) *VMBlock) error {
	u, err := url.Parse(relayURL)
	if err != nil {
		return fmt.Errorf("parse relay url: %w", err)
	}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()

	current := allowedLogins
	if allowedLoginsProvider != nil {
		current = allowedLoginsProvider()
	}
	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{
			"X-Agent-Token": []string{token},
			"User-Agent":    []string{"orchid-agent/1.0"},
		},
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()

	log.Printf("relay: connected to %s", relayURL)

	cfgFrame, _ := json.Marshal(map[string]any{
		"t":              "config",
		"allowed_logins": current,
	})
	_ = conn.Write(ctx, websocket.MessageText, cfgFrame)

	liveAgentMu.Lock()
	liveAgentConn = conn
	liveAgentMu.Unlock()
	defer func() {
		liveAgentMu.Lock()
		if liveAgentConn == conn {
			liveAgentConn = nil
		}
		liveAgentMu.Unlock()
	}()

	a := &agent{
		conn:       conn,
		handler:    handler,
		httpSecret: httpSecret,
		localAddr:  localAddr,
		streams:    map[uint32]*agentStream{},
		wsStreams:  map[uint32]*wsStream{},
		snapWrite:  snapWrite,
		subs:       make(chan int, 1),
		panes:      map[string]*paneSub{},
		paneVM:     paneVM,
	}
	a.subsActive.Store(true)

	if snapRead != nil {
		if b := snapRead(); len(b) > 0 && json.Valid(b) {
			_ = a.sendCtl(ctx, ctlFrame{T: "snap", Snap: json.RawMessage(b)})
		}
	}

	pingCtx, cancelPing := context.WithCancel(ctx)
	defer cancelPing()
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(pingCtx, 10*time.Second)
				_ = conn.Ping(pctx)
				cancel()
			}
		}
	}()

	if stateWake != nil && statePush != nil {
		go func() {
			var lastHash uint64
			emit := func(force bool) {
				if !force && !a.subsActive.Load() {
					return
				}
				body := statePush()
				h := fnv64(string(body))
				if h == lastHash && !force {
					return
				}
				lastHash = h
				_ = a.sendCtl(pingCtx, ctlFrame{T: "state-update", State: json.RawMessage(body)})
			}
			emit(false)
			throttle := time.NewTicker(2 * time.Second)
			defer throttle.Stop()
			pending := false
			for {
				select {
				case <-pingCtx.Done():
					return
				case <-stateWake:
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
		}()
	}

	return a.loop(ctx)
}

type agentStream struct {
	bodyR *io.PipeReader
	bodyW *io.PipeWriter
}

type agent struct {
	conn       *websocket.Conn
	handler    http.Handler
	httpSecret string
	localAddr  string
	snapWrite  func([]byte) error

	subs       chan int
	subsActive atomic.Bool

	mu        sync.Mutex
	streams   map[uint32]*agentStream
	wsStreams map[uint32]*wsStream

	panesMu sync.Mutex
	panes   map[string]*paneSub
	paneVM  func(string) *VMBlock
}

type paneSub struct {
	refs   int
	cancel context.CancelFunc
}

var allowedLoginsProvider func() []string

// liveAgentConn holds the active outbound WS to the relay (when one
// exists). pushAllowedLogins writes a fresh `config` frame to it so
// allow-list edits apply without waiting for the next disconnect.
var (
	liveAgentMu   sync.Mutex
	liveAgentConn *websocket.Conn
)

func pushAllowedLogins(logins []string) {
	liveAgentMu.Lock()
	conn := liveAgentConn
	liveAgentMu.Unlock()
	if conn == nil {
		return
	}
	frame, _ := json.Marshal(map[string]any{
		"t":              "config",
		"allowed_logins": logins,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageText, frame)
}

type wsStream struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
}

type ctlFrame struct {
	T       string          `json:"t"`
	ID      uint32          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Path    string          `json:"path,omitempty"`
	Headers [][]string      `json:"headers,omitempty"`
	HasBody bool            `json:"hasBody,omitempty"`
	Status  int             `json:"status,omitempty"`
	UserID  int             `json:"userId,omitempty"`
	Login   string          `json:"login,omitempty"`
	Stream  bool            `json:"streaming,omitempty"`
	Data    string          `json:"data,omitempty"`
	Code    int             `json:"code,omitempty"`
	Reason  string          `json:"reason,omitempty"`
	Count   int             `json:"count,omitempty"`
	PaneID string `json:"paneId,omitempty"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	State json.RawMessage `json:"state,omitempty"`
	Snap json.RawMessage `json:"snap,omitempty"`
}

func (a *agent) sendCtl(ctx context.Context, f ctlFrame) error {
	b, _ := json.Marshal(f)
	return a.conn.Write(ctx, websocket.MessageText, b)
}

func (a *agent) sendBin(ctx context.Context, streamID uint32, data []byte) error {
	out := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(out[:4], streamID)
	copy(out[4:], data)
	return a.conn.Write(ctx, websocket.MessageBinary, out)
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
			if err := json.Unmarshal(data, &f); err != nil {
				continue
			}
			a.onCtl(ctx, &f)
		case websocket.MessageBinary:
			if len(data) < 4 {
				continue
			}
			sid := binary.BigEndian.Uint32(data[:4])
			a.mu.Lock()
			s := a.streams[sid]
			ws := a.wsStreams[sid]
			a.mu.Unlock()
			if ws != nil {
				_ = ws.conn.Write(ctx, websocket.MessageBinary, data[4:])
			} else if s != nil {
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
		a.mu.Lock()
		s, ok := a.streams[f.ID]
		a.mu.Unlock()
		if ok && s.bodyW != nil {
			_ = s.bodyW.Close()
		}
	case "cancel":
		a.mu.Lock()
		s, ok := a.streams[f.ID]
		if ok {
			delete(a.streams, f.ID)
		}
		a.mu.Unlock()
		if ok && s.bodyW != nil {
			_ = s.bodyW.CloseWithError(io.ErrUnexpectedEOF)
		}
	case "ws-open":
		go a.handleWSOpen(ctx, f)
	case "ws-text":
		a.mu.Lock()
		ws := a.wsStreams[f.ID]
		a.mu.Unlock()
		if ws != nil {
			_ = ws.conn.Write(ctx, websocket.MessageText, []byte(f.Data))
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
		active := f.Count > 0
		a.subsActive.Store(active)
		select {
		case a.subs <- f.Count:
		default:
		}
	case "snap-put":
		if a.snapWrite != nil && len(f.Snap) > 0 {
			_ = a.snapWrite([]byte(f.Snap))
		}
	case "ws-close":
		a.mu.Lock()
		ws, ok := a.wsStreams[f.ID]
		if ok {
			delete(a.wsStreams, f.ID)
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
	}
}

// handleWSOpen dials orch's local listener and bridges messages between
// that local WS and the relay tunnel. Bearer auth is added so orch's gate
// passes for tunneled connections.
func (a *agent) handleWSOpen(parent context.Context, f *ctlFrame) {
	if a.localAddr == "" {
		_ = a.sendCtl(parent, ctlFrame{T: "ws-close", ID: f.ID, Code: 1011, Reason: "no local addr"})
		return
	}
	host := a.localAddr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	dialURL := "ws://" + host + f.Path
	hdr := http.Header{}
	for _, h := range f.Headers {
		if len(h) >= 2 {
			lk := strings.ToLower(h[0])
			if lk == "upgrade" || lk == "connection" || lk == "sec-websocket-key" ||
				lk == "sec-websocket-version" || lk == "sec-websocket-extensions" ||
				lk == "host" {
				continue
			}
			hdr.Add(h[0], h[1])
		}
	}
	if a.httpSecret != "" {
		hdr.Set("Authorization", "Bearer "+a.httpSecret)
	}

	dialCtx, cancel := context.WithCancel(parent)
	conn, _, err := websocket.Dial(dialCtx, dialURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		cancel()
		_ = a.sendCtl(parent, ctlFrame{T: "ws-close", ID: f.ID, Code: 1011, Reason: err.Error()})
		return
	}

	a.mu.Lock()
	a.wsStreams[f.ID] = &wsStream{conn: conn, cancel: cancel}
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.wsStreams, f.ID)
		a.mu.Unlock()
		_ = conn.CloseNow()
		cancel()
	}()

	for {
		mt, data, err := conn.Read(dialCtx)
		if err != nil {
			closeStatus := websocket.CloseStatus(err)
			code := 1000
			if closeStatus != -1 {
				code = int(closeStatus)
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
	if a.httpSecret != "" {
		req.Header.Set("Authorization", "Bearer "+a.httpSecret)
	}

	rw := newRelayResponseWriter(f.ID, a, ctx)
	a.handler.ServeHTTP(rw, req)
	rw.finish()
}

// relayResponseWriter streams the handler's response back through the
// agent's WS. Each Write becomes a binary frame; head is sent on first
// flush so SSE / chunked responses arrive incrementally.
type relayResponseWriter struct {
	id     uint32
	agent  *agent
	ctx    context.Context
	header http.Header
	status int
	sent   bool
	stream bool
	mu     sync.Mutex
}

func newRelayResponseWriter(id uint32, a *agent, ctx context.Context) *relayResponseWriter {
	return &relayResponseWriter{
		id: id, agent: a, ctx: ctx,
		header: http.Header{}, status: 200,
	}
}

func (w *relayResponseWriter) Header() http.Header { return w.header }
func (w *relayResponseWriter) WriteHeader(s int)   { w.status = s }
func (w *relayResponseWriter) Write(b []byte) (int, error) {
	w.ensureHead()
	if err := w.agent.sendBin(w.ctx, w.id, b); err != nil {
		return 0, err
	}
	return len(b), nil
}
func (w *relayResponseWriter) Flush() {
	w.stream = true
	w.ensureHead()
}
func (w *relayResponseWriter) ensureHead() {
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
func (w *relayResponseWriter) finish() {
	w.ensureHead()
	_ = w.agent.sendCtl(w.ctx, ctlFrame{T: "res-end", ID: w.id})
}

func (a *agent) startPane(ctx context.Context, id string, cols, rows int) {
	a.panesMu.Lock()
	if p, ok := a.panes[id]; ok {
		p.refs++
		a.panesMu.Unlock()
		return
	}
	paneCtx, cancel := context.WithCancel(ctx)
	a.panes[id] = &paneSub{refs: 1, cancel: cancel}
	a.panesMu.Unlock()
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

func (a *agent) runPaneCapture(ctx context.Context, id string, cols, rows int) {
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return
		}
	}
	if a.paneVM == nil {
		return
	}
	vm := a.paneVM(id)
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

	remote := fmt.Sprintf(
		`while :; do tmux capture-pane -p -e -t %s -S -200 2>&1; printf '\x1e'; sleep 0.2; done`,
		id,
	)
	var cmd *exec.Cmd
	if isLocal(*vm) {
		cmd = exec.CommandContext(ctx, "bash", "-c", remote)
	} else {
		cmd = exec.CommandContext(ctx, "ssh", append(sshArgs(*vm), remote)...)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	rd := bufio.NewReader(stdout)
	var buf strings.Builder
	gzbuf := new(bytes.Buffer)
	gzw := gzip.NewWriter(gzbuf)
	var last string

	for {
		if ctx.Err() != nil {
			return
		}
		b, err := rd.ReadByte()
		if err != nil {
			return
		}
		if b == 0x1e {
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
		} else {
			buf.WriteByte(b)
		}
	}
}

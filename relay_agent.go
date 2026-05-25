package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// runRelayAgent dials the Orchid relay endpoint and proxies any HTTP
// requests it receives over the tunnel into the local httpHandler. Frame
// protocol matches relay/src/frames.ts.
//
// relayURL example: wss://orchid.com/agent
// token: per-user agent token minted by the relay on signup.
//
// stateWake fires whenever orch's state changes; statePush returns the
// current /api/state body. Together they drive the push-based state
// stream that lets the dashboard drop its 3s poll loop — relay
// broadcasts each new body to all subscribed browsers and a hibernated
// DO costs nothing when nothing's changing.
//
// snapRead returns the current snap.json bytes for the initial push
// after every reconnect; snapWrite receives layout payloads from the
// dashboard and persists them. Together they replace the /api/snap
// HTTP path so card drags don't fan out into one DO+tunnel round-trip
// per debounce.
func runRelayAgent(parent context.Context, relayURL, token, httpSecret, localAddr string, allowedLogins []string, handler http.Handler, stateWake <-chan struct{}, statePush func() []byte, snapRead func() []byte, snapWrite func([]byte) error) {
	backoff := 500 * time.Millisecond
	for {
		if parent.Err() != nil {
			return
		}
		sessionStart := time.Now()
		err := relayAgentSession(parent, relayURL, token, httpSecret, localAddr, allowedLogins, handler, stateWake, statePush, snapRead, snapWrite)
		// A long-lived session means the connection was healthy — reset
		// the backoff so a single hiccup later doesn't park us at 30s.
		// Otherwise grow exponentially (capped) to avoid hammering CF
		// during outages.
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

func relayAgentSession(ctx context.Context, relayURL, token, httpSecret, localAddr string, allowedLogins []string, handler http.Handler, stateWake <-chan struct{}, statePush func() []byte, snapRead func() []byte, snapWrite func([]byte) error) error {
	u, err := url.Parse(relayURL)
	if err != nil {
		return fmt.Errorf("parse relay url: %w", err)
	}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()

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

	// Push the operator's allow-list to the relay so it can grant access
	// to invited GitHub logins without code changes on the worker side.
	cfgFrame, _ := json.Marshal(map[string]any{
		"t":              "config",
		"allowed_logins": allowedLogins,
	})
	_ = conn.Write(ctx, websocket.MessageText, cfgFrame)

	a := &agent{
		conn:       conn,
		handler:    handler,
		httpSecret: httpSecret,
		localAddr:  localAddr,
		streams:    map[uint32]*agentStream{},
		wsStreams:  map[uint32]*wsStream{},
		snapWrite:  snapWrite,
		subs:       make(chan int, 1),
	}
	// Assume subscribers until the relay tells us otherwise. The relay
	// pushes a `subs` frame immediately on agent connect, but if it
	// arrives late we'd rather waste one or two state-update frames
	// than starve a dashboard that loaded right before the agent did.
	a.subsActive.Store(true)

	// Bootstrap the relay's snap cache so newly-arriving dashboard tabs
	// can render the canvas layout without a separate HTTP fetch.
	if snapRead != nil {
		if b := snapRead(); len(b) > 0 && json.Valid(b) {
			_ = a.sendCtl(ctx, ctlFrame{T: "snap", Snap: json.RawMessage(b)})
		}
	}

	// Keep the relay connection warm. Cloudflare closes idle WebSockets
	// after ~100s; without traffic the tunnel goes silent and subsequent
	// proxied requests hang.
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

	// State pusher. Every saveState wakes us via stateWake; we send the
	// current snapshot to the relay which fans it out to subscribed
	// browser WSs. Throttled to once per 500ms so a burst of saves
	// coalesces — the dashboard never needed sub-second granularity.
	// Initial frame on connect bootstraps any client that subscribed
	// before orch came back up.
	if stateWake != nil && statePush != nil {
		go func() {
			// Skip emits when the marshaled body matches the last one
			// we sent — most ticks redo the same work (poll → no
			// change → saveState → identical JSON). Hashing dodges
			// both the WS write and the resulting DO wake on relay.
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
			// 2s throttle keeps the dashboard near-real-time while
			// folding sub-second bursts into a single frame.
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
					// Subscriber count crossed a threshold. On a
					// rising edge (0 → N) force a fresh push so the
					// new dashboard renders immediately. On a falling
					// edge (→ 0) the gate inside emit() will short-
					// circuit subsequent attempts.
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
	localAddr  string // host:port of orch's local http listener, used for WS bridging
	snapWrite  func([]byte) error

	// Sub-count signal from the relay. When zero, the state-pusher
	// goroutine pauses emits — no point shipping state-update frames
	// across the WS when nothing on the other end is going to read
	// them. The channel coalesces (cap 1) and emits on every transition.
	subs       chan int
	subsActive atomic.Bool

	mu        sync.Mutex
	streams   map[uint32]*agentStream
	wsStreams map[uint32]*wsStream
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
	// State carries an /api/state payload on a "state-update" frame.
	// Raw so we don't pay for double-JSON-encoding a body that's
	// already JSON.
	State json.RawMessage `json:"state,omitempty"`
	// Snap carries the dashboard layout payload on "snap" (orch →
	// relay → browser) and "snap-put" (browser → relay → orch) frames.
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
		// Register the stream synchronously so subsequent body/req-end
		// frames can find it before the dispatch goroutine starts.
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
	case "subs":
		// Relay's running sub count. Pause emits when nobody is
		// watching; resume + force an immediate fresh push when the
		// first subscriber arrives so they don't sit blank.
		active := f.Count > 0
		a.subsActive.Store(active)
		select {
		case a.subs <- f.Count:
		default:
		}
	case "snap-put":
		// Layout push from a dashboard tab via the events WS. Persist
		// it to disk; the relay has already broadcast to other tabs
		// in the same DO so we don't fan out further.
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
			// Skip hop-by-hop and upgrade-specific headers; the dial
			// will set its own.
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
	// Relay-tunneled requests come from the public dashboard; orch's auth
	// gate expects the operator's http_secret. Inject as Bearer so the
	// downstream handler sees an authenticated request.
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

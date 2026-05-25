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
	"time"

	"github.com/coder/websocket"
)

// runRelayAgent dials the Orchid relay endpoint and proxies any HTTP
// requests it receives over the tunnel into the local httpHandler. Frame
// protocol matches relay/src/frames.ts.
//
// relayURL example: wss://orchid.com/agent
// token: per-user agent token minted by the relay on signup.
func runRelayAgent(parent context.Context, relayURL, token, httpSecret, localAddr string, allowedLogins []string, handler http.Handler) {
	backoff := 500 * time.Millisecond
	for {
		if parent.Err() != nil {
			return
		}
		sessionStart := time.Now()
		err := relayAgentSession(parent, relayURL, token, httpSecret, localAddr, allowedLogins, handler)
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

func relayAgentSession(ctx context.Context, relayURL, token, httpSecret, localAddr string, allowedLogins []string, handler http.Handler) error {
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

	mu        sync.Mutex
	streams   map[uint32]*agentStream
	wsStreams map[uint32]*wsStream
}

type wsStream struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
}

type ctlFrame struct {
	T       string     `json:"t"`
	ID      uint32     `json:"id,omitempty"`
	Method  string     `json:"method,omitempty"`
	Path    string     `json:"path,omitempty"`
	Headers [][]string `json:"headers,omitempty"`
	HasBody bool       `json:"hasBody,omitempty"`
	Status  int        `json:"status,omitempty"`
	UserID  int        `json:"userId,omitempty"`
	Login   string     `json:"login,omitempty"`
	Stream  bool       `json:"streaming,omitempty"`
	Data    string     `json:"data,omitempty"`
	Code    int        `json:"code,omitempty"`
	Reason  string     `json:"reason,omitempty"`
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

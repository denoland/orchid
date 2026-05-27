// Package cfrelaytun implements the client side of a Cloudflare Workers +
// Durable Object HTTP tunnel. A Client dials the relay over WebSocket,
// receives forwarded HTTP (and optionally WS) requests as control frames,
// and streams responses back through the same socket.
//
// Minimal use:
//
//	err := cfrelaytun.Run(ctx, cfrelaytun.Config{
//	    URL:     "wss://tun.example.com/agent",
//	    Token:   os.Getenv("AGENT_TOKEN"),
//	    Handler: myHandler,
//	})
//
// Custom control frames (project-specific extensions) are supported via
// Client.OnCtl + Client.SendCtl on a constructed Client.
package cfrelaytun

import (
	"context"
	"encoding/json"
	"net/http"
)

// Config bundles the parameters needed to run a tunnel client.
type Config struct {
	// URL is the wss:// endpoint of the relay's /agent route.
	URL string
	// Token is sent both as ?token= and as X-Agent-Token. Must match the
	// token configured on the Worker side.
	Token string
	// Handler serves HTTP requests forwarded from the relay. Required.
	Handler http.Handler
	// OnWSUpgrade, if non-nil, is invoked for each public WebSocket
	// upgrade. The implementation should read from the public side via
	// conn.NextFrame and write back via conn.WriteText / conn.WriteBinary,
	// and call conn.Close when done. If nil, ws-open frames are rejected
	// with code 1011.
	OnWSUpgrade WSHandler
	// Logger, if non-nil, receives one-line connection/error diagnostics.
	Logger func(format string, args ...any)
}

// WSHandler handles a single public WebSocket session forwarded from the
// relay. The req mirrors the incoming HTTP request (path, headers) and the
// conn is a bidirectional WS-like adapter that multiplexes its frames over
// the agent's relay socket.
type WSHandler func(ctx context.Context, req *http.Request, conn WSConn) error

// WSConn is the per-stream WebSocket adapter passed to OnWSUpgrade.
type WSConn interface {
	WriteText(ctx context.Context, s string) error
	WriteBinary(ctx context.Context, b []byte) error
	// NextFrame returns the next frame from the public side. Returns
	// kind == "close" when the public peer closes.
	NextFrame(ctx context.Context) (kind string, data []byte, err error)
	Close(code int, reason string) error
}

// CtlHandler is invoked for an inbound control frame whose `t` field
// doesn't match any of the lib's built-in types. Used by consumers to
// implement project-specific frames (e.g. state pushes, pubsub).
type CtlHandler func(ctx context.Context, payload json.RawMessage)

// Client is a long-lived tunnel client. Construct with New, register
// handlers, then call Run.
type Client struct {
	cfg       Config
	extra     map[string]CtlHandler
	onConnect []func(context.Context, Session)
	live      *agent // current session (nil between reconnects)
}

// New constructs a Client without starting it.
func New(cfg Config) *Client {
	return &Client{
		cfg:   cfg,
		extra: map[string]CtlHandler{},
	}
}

// OnCtl registers a handler for a custom control frame type. The handler
// receives the raw JSON body of the frame.
func (c *Client) OnCtl(t string, fn CtlHandler) {
	c.extra[t] = fn
}

// OnConnect registers a callback fired immediately after a successful
// agent handshake. Use it to push initial state to the relay (e.g.
// config, snapshots) on every reconnect.
func (c *Client) OnConnect(fn func(ctx context.Context, sess Session)) {
	c.onConnect = append(c.onConnect, fn)
}

// Session is the per-connection handle handed to OnConnect callbacks and
// exposed via Client.Send. It survives only for the lifetime of one
// websocket session.
type Session interface {
	// SendCtl writes a custom control frame on the live session.
	SendCtl(ctx context.Context, t string, payload any) error
}

// Send writes a control frame on the live session, if any. Returns false
// if no session is currently connected.
func (c *Client) Send(ctx context.Context, t string, payload any) bool {
	a := c.live
	if a == nil {
		return false
	}
	return a.sendExtra(ctx, t, payload) == nil
}

// Run blocks, dialing the relay and serving forwarded requests with
// exponential backoff between disconnects. Returns when ctx is cancelled.
func (c *Client) Run(ctx context.Context) error {
	return runAgent(ctx, c)
}

// Run is a convenience wrapper for the common case of "construct, then
// run with no extension frames". Equivalent to New(cfg).Run(ctx).
func Run(ctx context.Context, cfg Config) error {
	return New(cfg).Run(ctx)
}

# cfrelaytun

Self-hosted ngrok-style HTTP tunnel built on Cloudflare Workers + Durable
Objects. The public side runs on your Workers account; the local side is any
process that can speak WebSocket. A request hitting the public URL is
multiplexed across one WS to your local `http.Handler` and the response
streams back the same way.

Two halves, used together or independently:

- **`go/`** — Go client. `cfrelaytun.Run(ctx, Config{URL, Token, Handler})`
  and you're done.
- **`worker/`** — TypeScript Worker lib. `makeRelayApp({...})` returns a
  [Hono](https://hono.dev) app; export the `TunnelSession` Durable Object
  alongside it.

Both routing modes are supported:

- **subdomain**: `https://<tunnel>.tun.example.com/...` (needs a wildcard
  DNS record + Worker route on a Cloudflare-managed zone)
- **path-prefix**: `https://tun.example.workers.dev/<tunnel>/...` (no DNS
  setup, works on `workers.dev`)

## Quick start

### 1. Deploy the Worker

```bash
cd cfrelaytun/worker
cp wrangler.example.jsonc wrangler.jsonc
# edit wrangler.jsonc: set name, routes, AGENT_TOKEN
npx wrangler deploy
```

`example/worker.ts` is the minimal entry point — copy it as your starting
Worker.

### 2. Run a Go client

```bash
cd cfrelaytun/go/example
go run . -url wss://tun.example.com/agent -token "$AGENT_TOKEN" -addr :3000
```

That exposes whatever's serving on `:3000` at the public URL.

Or, from your own Go code:

```go
import "github.com/divy/orchid/cfrelaytun/go/cfrelaytun"

err := cfrelaytun.Run(ctx, cfrelaytun.Config{
    URL:     "wss://tun.example.com/agent",
    Token:   os.Getenv("AGENT_TOKEN"),
    Handler: myHandler,
})
```

## Protocol

WS frames are either control (JSON text) or stream body (binary, 4-byte
big-endian stream id prefix + chunk). See `go/frame.go` and
`worker/src/session.ts` for the canonical envelope.

Control frame types:

| `t`         | Direction      | Meaning                                  |
|-------------|----------------|------------------------------------------|
| `hello`     | server → agent | First frame, includes server info        |
| `req`       | server → agent | Public HTTP request inbound              |
| `req-end`   | server → agent | Request body finished                    |
| `cancel`    | server → agent | Public client gave up                    |
| `res-head`  | agent → server | Status + headers ready                   |
| `res-end`   | agent → server | Response body finished                   |
| `ws-open`   | server → agent | Public WS upgrade inbound                |
| `ws-text`   | both           | WS text payload for stream `id`          |
| `ws-close`  | both           | Close WS stream `id`                     |
| `pong`      | both           | Heartbeat reply                          |

Extensions (project-specific frames) ride the same WS — see
`OnCtl(name, fn)` on the Go side and the `extraHandler` hook on the
Worker side.

## Caveats

- One agent WS per tunnel name. New connect replaces old.
- Durable Object holds the WS. DO restart kills in-flight requests.
  Workers's `state.acceptWebSocket` keeps the socket alive across
  hibernation.
- Streamed response bodies cross the WS, not native HTTP. Fine for small
  + medium responses; not the right tool for multi-GB downloads.

## License

MIT — see `LICENSE`.

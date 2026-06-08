# {{illust:branch-tree}} Architecture

A high-level map of what runs where and how the pieces talk.

## Components

Two processes you actually run:

1. **`orch`** — the Go binary on your machine. Polls GitHub, spawns
   tmux sessions, relays reviews back, and serves the dashboard on a
   local port (`:8000` by default).
2. **Claude sessions** — `claude --dangerously-skip-permissions`
   inside tmux. Each session owns one issue; orch pastes
   bootstrap prompt + review summaries via `tmux load-buffer`.

Plus one **optional** process:

3. **The relay** (`cf/`) — a Cloudflare Worker that fronts the
   dashboard with a public subdomain + GitHub OAuth, routing each
   user's traffic to their personal Durable Object. The DO holds a
   live WebSocket to the agent and does hibernated multiplexing of
   dashboard subscribers. Deploy it to your own Cloudflare account if
   you want public access; skip it otherwise.

## Why a relay at all

So you don't have to expose a public IP. When deployed, the agent
opens one outbound WS to the relay; the DO proxies dashboard fetches +
WS upgrades back over that tunnel. You can skip the relay entirely and
hit the orch HTTP server directly on `:8000` from the LAN or via
Tailscale — that's the default self-host path.

## Single binary, embedded SPA

`orch` ships the dashboard SPA inside the binary via `go:embed`
(`internal/orch/embed-dist`). When you upgrade, one `scp` + restart
replaces both the daemon and the UI. The build pipeline is just:

```bash
cd www && bun run build              # → internal/orch/embed-dist
go build -o orch ./cmd/orch          # embeds + compiles
```

## Storage

State is a single SQLite file at `orchestrator.state_db`. It tracks:

- live jobs (issue → branch → PR mapping, last-seen review IDs)
- mention cursor (so we don't re-process the same @mention)
- maintainer cache (Org membership snapshot, refreshed hourly)
- usage_daily (token + cost rollup from Claude's statusline JSONL)
- snap.json (dashboard canvas positions + ink strokes)

WAL mode, single-writer. Survives orch restarts; the legacy JSON
files in the same dir get auto-imported once and renamed
`.migrated`.

## Auth surfaces

- **Dashboard / API** — `Authorization: Bearer <http_secret>` for
  direct hits, or the relay session cookie when fronted by the
  optional relay.
- **Capture intake** — `X-Capture-Token: <auth_token>` (separate
  from `http_secret` so leaking it doesn't grant dashboard access).
- **Worker join** — central `http_secret` doubles as the bearer the
  worker's `orch join vm` presents.
- **Agent ↔ relay** — when the relay is deployed, a one-time agent
  token authenticates the outbound WS; rotate it via Settings → Revoke.


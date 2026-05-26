# Dashboard

{{illust:spray}}

{{mockup:dashboard}}

Each orchid user has a personal dashboard at
`<your-handle>.orchid.littledivy.com`. It's a single-page React app
served by the relay; the data behind it streams over a single
WebSocket per tab.

## Layout

- **Canvas** — every active session shows up as a card. Drag to
  rearrange; cards persist across reloads. The view doubles as a
  whiteboard: drop notes, ink strokes, link cards, GitHub snippets.
- **List view** — same data, dense table. Toggle with the icon in
  the header.
- **Composer** — file inbox issues without leaving the browser; fans
  out across multiple targets in one click.
- **Pane viewer** — click a card to see the live tmux pane. Frames
  are gzipped and streamed over the same WS; off-screen panes pause
  automatically to save bandwidth.

## Card states

{{diagram:card-states}}

| State | Meaning |
|-------|---------|
| **Spawning** | tmux session starting, claude booting. |
| **Working** | Claude has the prompt, hasn't opened a PR yet. |
| **PR open** | PR exists; CI may still be running. |
| **Needs you** | Claude is blocked on a permission/plan dialog. Click in. |
| **Quiet** | Claude is idle. Usually waiting for review feedback. |
| **Merged** | PR merged; session tearing down. |

## Settings

{{mockup:settings}}

The gear icon opens Settings. Highlights:

- **Access** — extra GitHub logins allowed to view your dashboard.
  Hot-applies, no restart.
- **VMs** — worker hosts, capacity, join tokens. See
  [Workers](/docs/workers).
- **Targets** — label-to-repo routing. See [Targets](/docs/targets).
- **Capture** — the macOS / iOS draft intake token + endpoint URL.
- **Usage** — daily/weekly/monthly cost charts, per-session and
  per-repo donuts. Pulls from local Claude statusline JSONL.

## Keyboard shortcuts

| Key | Action |
|-----|--------|
| `?` | Open shortcut help. |
| `/` | Focus the Composer. |
| `s` | Open Settings. |
| `v` | Toggle canvas / list view. |
| `Esc` | Close any open modal. |

## Mobile

The dashboard reflows for phones — header chips replace the side
nav, cards stack, the canvas falls back to list mode below 640px.

## Multi-user

Add logins to `orchestrator.allowed_logins` in `swarm.hcl` (or
through Settings → Access). They sign in with GitHub at the apex,
get redirected to your subdomain, and see the same dashboard. They
can review PRs and chat to sessions; only the owner can change
config or revoke the agent token.

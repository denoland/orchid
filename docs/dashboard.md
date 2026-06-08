# {{illust:spray}} Dashboard

{{mockup:dashboard}}

`orch` serves the dashboard itself at `http://<host>:8000` (the
`orchestrator.http_addr` port). It's a single-page React app embedded
in the binary; the data behind it streams over a single WebSocket per
tab. If you deploy the optional relay, the same dashboard is also
reachable on your public subdomain.

## Tabs

The dashboard is a row of tabs in the header:

- **Sessions** — the live list: one row per session with PR / CI status,
  repo, agent, and a left-edge accent when a session is blocked on you.
- **Machines** — worker VMs as a table: health, agent, host, and a load
  bar (running / capacity). See [VMs](/docs/vms).
- **Analytics** — per-account quota (5h + weekly burn — the real
  constraint), token throughput over time, by-model / by-repo splits, and
  a live context-window readout. Subscriptions aren't billed per token, so
  there are no dollar figures; throughput counts input + output + cache
  writes (cache reads excluded).
- **Memory** — a tree browser over the swarm's shared knowledge base, with
  search, markdown rendering, and backlinks. See [Memory](/docs/memory).
- **Settings** — every `swarm.hcl` field, editable in place.

**Composer** — file inbox issues without leaving the browser; fans out
across multiple targets in one click.

**Pane viewer** — click a session to see the live tmux pane. Frames are
gzipped and streamed over the WS; off-screen panes pause to save bandwidth.

## Card states

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

The **Settings** tab edits everything in `swarm.hcl`:

- **GitHub / Orchestrator** — inbox repo, poll interval, bot identity,
  pacing.
- **Access** — extra GitHub logins allowed to view your dashboard.
  Hot-applies, no restart.
- **Targets** — label-to-repo routing. See [Targets](/docs/targets).
- **VMs** — worker hosts, capacity, join tokens. See [VMs](/docs/vms).
- **Capture** — the macOS / iOS draft intake token + endpoint URL.

Most changes need an `orch restart`; `allowed_logins` hot-applies.

## Keyboard shortcuts

| Key | Action |
|-----|--------|
| `?` | Open shortcut help. |
| `/` | Focus the Composer. |
| `Esc` | Close any open modal. |

## Mobile

The dashboard reflows for phones — the tab row stays, the title hides,
rows and tables stack below 640px.

## Multi-user

Add logins to `orchestrator.allowed_logins` in `swarm.hcl` (or
through Settings → Access) to grant teammates read access. Over the
optional relay they sign in with GitHub and land on the same
dashboard; on a bare self-host you share the tokened URL (or a
Tailscale address). They can review PRs and chat to sessions; only the
owner can change config or revoke the agent token.

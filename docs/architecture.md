# Architecture

orchid is one headless coordinator (`divybot`) driving bare `claude` / `codex`
agents across a pool of hosts. It is a thin layer over the agents — it doesn't wrap
them, it schedules and supervises them.

## What runs where

- **`divybot`** — one Go file in `cmd/divybot`. Headless, no UI of its own. Polls a
  GitHub **inbox repo**, and for each labelled issue with budget, spawns an agent on
  a host and supervises the PR it produces. Holds the canonical credentials and
  pushes them to every host.
- **`herdr`** — runs on every host. The per-host runtime, perception layer, and UI.
  `divybot` drives it over plain SSH (`ssh <host> herdr <cmd>`, JSON out) to spawn
  agents, read their status, and send them messages. Attach with `herdr --remote
  <host>`. A host is usable iff it's on the tailnet and its herdr answers — no join.
- **Agents** — bare `claude --dangerously-skip-permissions` or `codex` (via
  opencode), one per issue, each in its own herdr workspace. No wrapper.

## What orchid adds on top of the agent

- **Fan-out by label.** An inbox issue's label maps to a target (work repo + which
  agent). One issue = one agent on a free, capable host. For wide parallelism, file
  one issue per slice.
- **Quota pacing.** A governor reads each account's real usage meter and caps
  concurrency per account so the swarm spends evenly, hard-pausing near the weekly
  ceiling. claude and codex pace independently; work spills claude→codex when capped.
  See [throttling](throttling.md).
- **PR supervision.** divybot forwards only *new* reviews, CI failures, and conflicts
  into the agent (via herdr), and squash-merges green PRs on `automerge` targets.
- **Shared memory.** A git-backed `memory/` subtree the agents maintain themselves,
  synced to every host so knowledge accumulates. See [memory](memory.md).
- **Central auth.** One source of truth for creds, pushed to every host on spawn and
  periodically — no per-host stale-token failures.

## Replaces

The previous orchid was ~13k LOC across 25 files: an SSH+tmux shim, capture-pane
scraping, paste-buffer pokes, a `www/` dashboard, a Cloudflare relay, and
per-workspace clawpatrol. All collapsed into native herdr calls and central
auth-sync — ~1.2k LOC, one file.

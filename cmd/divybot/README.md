# divybot — the new orchid (single Go file)

A GitHub-issue → PR swarm coordinator over a **herdr fabric**. One file
(`main.go`, stdlib only). Replaces the old multi-file orchid: **no** SSH+tmux
shim, **no** capture-pane scrape, **no** paste-buffer pokes, **no** dashboard,
**no** clawpatrol.

## How it works

```
poll GitHub issues → governor-admit → push creds → herdr spawn (BARE claude/codex)
   → inject goal → supervise via herdr agent_status → poll PRs, relay reviews/CI/
   conflicts via herdr send → teardown
```

- **Transport** is native herdr over plain SSH (`ssh <host> herdr <cmd>`, JSON
  out). herdr is the per-host runtime + perception layer; the coordinator is a
  thin client. No tmux, no shim.
- **Perception** is herdr's native `agent_status` (`idle|working|blocked|done|
  unknown`), one structured call per host per tick — not a 5Hz pane scrape.
  `blocked` → escalate (ntfy); `idle/done` with no PR → re-inject the goal
  (fixes the gcp strandings); a `Invalid bearer`/`not trusted` read → resync
  creds + respawn.
- **Auth is central.** The coordinator owns the canonical `claude` oauth +
  `codex` auth + gh token and pushes them to every host on spawn and every
  20 min (before the oauth access token expires). This kills the entire
  per-host stale-credential 401/JWT class that bit vultr-claude and vultr-codex.
- **Governor kept.** Curls Anthropic's API with the synced oauth token, reads
  the unified rate-limit headers (5h/weekly used%), and caps concurrent agents
  so the swarm never blows the Max weekly quota. Slim threshold control
  (pause above ceiling, throttle within slack, else MaxActive); fail-open.
- **PR review/CI/conflict polling is unchanged** from orchid in spirit (`diff()`
  forwards only NEW reviews/comments/CI-failures/conflicts) — only the delivery
  channel changed (herdr `agent send`, not paste-buffer).

## Run

```sh
go build -o divybot ./cmd/divybot
GH_TOKEN=$(gh auth token) ./divybot -config divybot.json
./divybot -config divybot.json -once   # single tick, for testing
```

## Prerequisites (cutover)

1. **Each host's herdr server runs as the agent user**, and the coordinator
   ssh-targets that user: `orchid@…` on the VMs, `divy@…` on the mac. (On vultr
   that means herdr-as-orchid + `ssh orchid@localhost`, not root.) Bare claude
   refuses `--dangerously-skip-permissions` as root, so the agent user must be
   unprivileged.
2. **The coordinator host holds the canonical creds**: `~/.claude/.credentials.json`,
   `~/.codex/auth.json`, and a gh token (`GH_TOKEN` or `gh auth token`). These
   are the single source of truth pushed to every host. Keep them logged-in.
3. **herdr ≥ 0.7.0** on every host with the claude/codex integrations installed
   (`herdr integration install claude` / `codex`).
4. No tailnet "join" — a host is usable iff it's on the tailnet and its herdr
   answers. Add/remove boxes by editing `hosts` in the config.

## Cutover

```sh
systemctl stop orchid            # stop the old multi-file orchid
# (old workers keep running in their herdr workspaces; divybot adopts live
#  labels by name on its first tick and supervises them)
./divybot -config divybot.json   # or install as a systemd unit
```

## What's gone vs orchid

| orchid (old)                               | divybot (new) |
|--------------------------------------------|---------------|
| SSH ControlMaster + tmux-shim + capture-pane + paste-buffer | native `herdr` over ssh |
| dashboard (`www/`, `http_api.go`, cf relay) | `herdr --remote <host>` is the UI |
| clawpatrol per-workspace                    | bare claude/codex + central auth-sync |
| `join_managed` / `bootstrapVM` / vm-keys gating | tailnet pool, no join |
| ~13k LOC across 25 files                    | ~1.2k LOC, one file |

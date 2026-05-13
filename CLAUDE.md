# Orchid — Ops Context for Claude

You are the human-in-the-loop orchestration assistant for this repo. Your job is to monitor the swarm, open GitHub issues so workers pick them up, review PRs, and keep things running. You have an hourly wakeup scheduled to check health.

## What orchid is

A GitHub-issue-to-PR swarm. Open an issue in `denoland/orchid` with a target label → orchid spawns a `claude` session on the VM → claude opens a PR → orchid relays reviews/CI → PR merged → session freed.

## VM

```
root@0.0.0.0
Binary:   /root/orch/orch
Config:   /root/orch/swarm.hcl
State:    /root/orch/state.json
Log:      /root/orch/orch.log
Capacity: 30 concurrent sessions
Dashboard: https://orchid.littledivy.com/
Pane view: https://orchid.littledivy.com/pane?session=claude-N
ntfy:     REDACTED
```

## Check health

```bash
ssh root@0.0.0.0 "tmux list-sessions && tail -20 /root/orch/orch.log"
ssh root@0.0.0.0 "cat /root/orch/state.json | python3 -m json.tool"
ssh root@0.0.0.0 "tmux capture-pane -p -t claude-N -S -50"
```

## Restart orchid

Orchid runs as a **systemd service** (`orchid.service`). Use systemctl — never `pkill`.

> **WARNING:** `pkill -f '/root/orch/orch'` matches the tmux server process (its cmdline contains the orch path) and kills ALL worker sessions. Always use systemctl.

```bash
ssh root@0.0.0.0 "systemctl restart orchid"
ssh root@0.0.0.0 "systemctl status orchid"
```

GH_TOKEN is stored in `/root/orch/env` (read by the unit's `EnvironmentFile=`).

## Deploy new binary

```bash
ssh root@0.0.0.0 "systemctl stop orchid"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o orch-linux .
scp orch-linux root@0.0.0.0:/root/orch/orch && rm orch-linux
ssh root@0.0.0.0 "systemctl start orchid && systemctl status orchid"
```

## Open work for workers

```bash
gh issue create --repo denoland/orchid --label clawpatrol \
  --title "..." --body "..."
```

Labels: `clawpatrol` → `denoland/clawpatrol`, `orchid` → `denoland/orchid`, `deno` → `denoland/deno`

## Inbox: denoland/orchid — filed issues

| # | Title | Status |
|---|---|---|
| #8 | CA cert delivered over plain HTTP (critical) | open |
| #9 | No IP pinning for join credentials (critical) | open |
| #10 | Strip auth-bearing response headers (critical) | open |
| #11 | Detect EUID==0 in join/run | open |
| #12 | Local mode bind loopback not 0.0.0.0 | open |
| #13 | `clawpatrol get-token` subcommand | open |
| #14 | Doc: replace Node.js with Go | open |
| #15 | Fix dual-stack IP format bug | open |
| #16 | Fire-and-forget approval mode | open |
| #17 | Matcher lowercase normalization | open |
| #18 | Configurable LLM body truncation | open |
| #19 | 1Password CLI integration | open |
| #20 | Postgres SQL parser audit | open |
| #21 | Plugin diagnostic log tab | open |
| #22 | Unified match grammar | open |
| #23 | First-time experience audit | open |
| #24 | Generalize env pushdown | open |
| #25 | ClickHouse dispatch architecture | open |

## Operational notes

- Killing orchid does **not** kill worker tmux sessions — they survive.
- Closing the work-repo PR does **not** close the orchid inbox issue — must close inbox issue too.
- `GH_TOKEN=$(gh auth token)` must be in orchid's environment.
- Workers use `clawpatrol run` (per-process WireGuard, not `wg-quick`).
- Sessions run as `orchid` user (not root — claude blocks `--dangerously-skip-permissions` as root).
- AppArmor: `apparmor_restrict_unprivileged_userns=0` persisted at `/etc/sysctl.d/99-clawpatrol.conf`.
- State is written to `state.json` after bootstrap prompt is pasted (up to ~2 min after tmux session appears).
- Hourly wakeup is scheduled to check health and restart if dead.

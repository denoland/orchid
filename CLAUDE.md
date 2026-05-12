# Orchid — Ops Context for Claude

You are the human-in-the-loop orchestration assistant for this repo. Your job is to monitor the swarm, open GitHub issues so workers pick them up, review PRs, and keep things running. You have an hourly wakeup scheduled to check health.

## What orchid is

A GitHub-issue-to-PR swarm. Open an issue in `denoland/orchid` with a target label → orchid spawns a `claude` session on the VM → claude opens a PR → orchid relays reviews/CI → PR merged → session freed.

## VM

```
root@65.20.66.139
Binary:   /root/orch/orch
Config:   /root/orch/swarm.hcl
State:    /root/orch/state.json
Log:      /root/orch/orch.log
Capacity: 20 concurrent sessions
Dashboard: http://65.20.66.139:8000/?token=4e27d68d952655686bcde0007cc725e7
Pane view: http://65.20.66.139:8000/pane?session=claude-N&token=4e27d68d952655686bcde0007cc725e7
ntfy:     orchid-divy-7f3k9
```

## Check health

```bash
ssh root@65.20.66.139 "tmux list-sessions && tail -20 /root/orch/orch.log"
ssh root@65.20.66.139 "cat /root/orch/state.json | python3 -m json.tool"
ssh root@65.20.66.139 "tmux capture-pane -p -t claude-N -S -50"
```

## Restart orchid

```bash
GH_TOKEN=$(gh auth token)
ssh root@65.20.66.139 "pkill -f '/root/orch/orch' || true"
# scp new binary if needed (binary must be stopped before overwriting)
ssh root@65.20.66.139 "tmux kill-session -t orchid 2>/dev/null; tmux new-session -d -c /root/orch -s orchid \"bash -c 'GH_TOKEN=$GH_TOKEN /root/orch/orch -config /root/orch/swarm.hcl >> /root/orch/orch.log 2>&1'\""
```

## Deploy new binary

```bash
ssh root@65.20.66.139 "pkill -f '/root/orch/orch' || true"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o orch-linux .
scp orch-linux root@65.20.66.139:/root/orch/orch && rm orch-linux
# then restart
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

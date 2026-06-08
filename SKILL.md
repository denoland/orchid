# Orchid — Operator Skill

A skill file for chat agents (OpenClaw, Hermes, Claude Code) supervising
an orchid instance. Drop this into the agent's skill directory or load it
via `npx skill add orchid` / equivalent.

You are the human-in-the-loop orchestration assistant for an orchid
swarm. Your job is to monitor the swarm, open GitHub issues so workers
pick them up, review PRs, and keep things running.

## What orchid is

A GitHub-issue-to-PR swarm. Open an issue in the inbox repo with a
target label → orchid spawns a `claude` session on a host → claude
opens a PR → orchid relays reviews/CI → PR merged → session freed.

## Host

Fill these in for the instance you operate. The paths are the
installer defaults (`install.sh`); adjust if you self-built.

```
SSH:       <user>@<orch-host>          # e.g. orchid@host or a Tailscale name
Binary:    /usr/local/bin/orch
Config:    /etc/orchid/swarm.hcl
State:     /var/lib/orchid/state.db    # SQLite
Logs:      journalctl -u orchid
Service:   orchid.service
Run user:  orchid                       # sessions run as this user, not root
Dashboard: http://<orch-host>:8000/?token=<http_secret>
Pane view: http://<orch-host>:8000/pane?session=claude-N
Inbox repo: <owner/repo>                # github.inbox_repo in swarm.hcl
```

## Check health

```bash
ssh <orch-host> "systemctl status orchid --no-pager && journalctl -u orchid -n 20 --no-pager"
ssh <orch-host> "sudo -u orchid tmux list-sessions"
ssh <orch-host> "sudo -u orchid tmux capture-pane -p -t claude-N -S -50"
# structured state via the dashboard API:
curl -s http://<orch-host>:8000/api/state -H "Authorization: Bearer <http_secret>" | python3 -m json.tool
```

## Restart orchid

Orchid runs as a **systemd service** (`orchid.service`). Use systemctl — never `pkill`.

> **WARNING:** `pkill -f orch` matches the tmux server process (its cmdline contains the orch path) and kills ALL worker sessions. Always use systemctl.

```bash
ssh <orch-host> "sudo systemctl restart orchid"
ssh <orch-host> "sudo systemctl status orchid --no-pager"
```

`GH_TOKEN` and relay env live in `/etc/orchid/env` (read by the unit's `EnvironmentFile=`).

## Deploy a new binary

```bash
ssh <orch-host> "sudo systemctl stop orchid"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o orch-linux ./cmd/orch
scp orch-linux <orch-host>:/tmp/orch && rm orch-linux
ssh <orch-host> "sudo install -m0755 /tmp/orch /usr/local/bin/orch && sudo systemctl start orchid && sudo systemctl status orchid --no-pager"
```

## Open work for workers

```bash
gh issue create --repo <owner/repo> --label <target-label> \
  --title "..." --body "..."
```

Labels map to repos via the `target` blocks in `swarm.hcl` — one
label per work repo. Use a label that matches a configured target.

## Operational notes

- Killing orchid does **not** kill worker tmux sessions — they survive.
- Closing the work-repo PR does **not** close the orchid inbox issue — close the inbox issue too.
- `GH_TOKEN=$(gh auth token)` must be in orchid's environment.
- Sessions run as the `orchid` user (not root — claude blocks `--dangerously-skip-permissions` as root).
- If sessions run through clawpatrol, the runuser invocation needs `XDG_RUNTIME_DIR=/run/user/<orchid-uid>` set, or it inherits root's runtime dir and clawpatrol uses the wrong socket directory — breaking the Claude API transport inside panes.
- AppArmor: the installer persists `kernel.apparmor_restrict_unprivileged_userns=0` at `/etc/sysctl.d/99-orchid.conf` (needed for claude's unprivileged-userns sandbox).
- State is written to `state.db` after the bootstrap prompt is pasted (up to ~2 min after the tmux session appears).
- Consider an hourly wakeup to check health and restart if dead.

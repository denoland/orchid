---
name: orchid-operator
description: Operate and supervise an orchid/divybot agent swarm — open issues as work, watch workers over herdr, relay-free PR supervision, governor pacing, credential health, deploy. Use when asked to check the swarm, open work, why a worker is stuck, scale/pause a target, deploy the coordinator, or fix a fleet-wide auth failure.
---

# Orchid Operator

You are the human-in-the-loop operator of an **orchid** swarm. The coordinator is
`divybot` — one headless Go process that turns labelled GitHub issues into merged
pull requests by driving agent harnesses (`claude` / `codex`) on a pool of hosts
through a **herdr** fabric over SSH. There is no dashboard, no web UI, no tmux:
herdr is the runtime, the perception layer, and the UI (`herdr --remote <host>`).

Your job: keep the swarm fed and healthy. Open issues so workers pick them up,
watch workers when something looks wrong, supervise/merge PRs, pace the swarm
against quota, and fix the failure classes below before they cascade.

## Mental model — how divybot thinks

divybot runs one loop on `poll_interval` (default 30s). Each tick: feed (mirror
bot-assigned upstream issues into the inbox) → sweep-merge green bot PRs →
poll issues → teardown jobs whose issue closed → fleet-status every host →
respawn vanished agents → admit new work in priority order against per-account
governor budget. Internalize these invariants — they explain almost every
behaviour you'll see:

- **One issue = one worker.** divybot does NOT auto-fan-out. To get N parallel
  workers on a big effort, file N issues (one per subsystem/slice). A single fat
  issue gets a single agent.
- **Inbox issue is the unit of life.** A worker lives as long as its inbox issue
  is open. Close the inbox issue to retire a worker; closing only the work-repo
  PR does nothing. Tearing down happens when the issue is gone.
- **Never-strand guard.** If a `gh` issue list errors, divybot skips teardown
  that tick — a flaky poll must never read as "all closed" and wipe the swarm.
- **Respawn is debounced.** A job whose host is up but whose agent vanished is
  respawned only after several consecutive status misses, so one herdr status
  flicker can't mass-respawn a host.
- **Credentials are central.** The coordinator host holds the only real creds and
  pushes them to every host on spawn and every ~20 min. Most "the fleet died"
  incidents are a credential problem at the coordinator, not the hosts.
- **Agents pace independently.** claude and codex each meter against their own
  real quota; a hot claude window does not throttle codex. Work spills claude→codex
  when claude is capped (target `agents: ["claude","codex"]`).

## Instance facts (fill in for your swarm)

```
Coordinator SSH:  <user>@<coordinator>     # holds canonical creds; runs divybot
Binary:           /usr/local/bin/divybot
Config:           <path>/divybot.json      # see cmd/divybot/divybot.example.json
State:            <state_file from config> # plain JSON, e.g. /root/divybot/state.json
Service unit:     orchid.service           # systemd; confirm with `systemctl list-units | grep -i orch`
Logs:             journalctl -u orchid
Inbox repo:       <owner/repo>             # "inbox" in config
Hosts:            herdr ≥ 0.7.0, agent user (orchid@vm / divy@mac), on the tailnet
```

The hosts, targets (label→repo→agent), and governor knobs all live in the config
JSON. Read it first — it is the ground truth for what's wired up.

## Health check (start here for "how's the swarm?")

```bash
ssh <coordinator> "systemctl status orchid --no-pager && journalctl -u orchid -n 40 --no-pager"
# coordinator's own view of jobs (issue → host → agent → branch → PR):
ssh <coordinator> "cat <state_file>" | python3 -m json.tool
# per-host live agents, straight from herdr (JSON):
ssh <host> "herdr agent list"
```

Cross-check: every open inbox issue should map to a live agent (state file) and a
herdr agent on its host. Gaps = stranded or dead workers — see Troubleshooting.

## Watch a worker (when one looks stuck)

herdr's native `agent_status` is the truth (`idle|working|blocked|done|unknown`) —
agent-agnostic, works for claude and codex. Don't scrape footers.

```bash
ssh <host> "herdr agent get <target>"          # status + cwd + pane
ssh <host> "herdr pane capture <pane> -n 60"   # read recent output
herdr --remote <host>                          # attach interactively to drive/inspect
```

Read `blocked` as "needs a human" (divybot escalates these via ntfy). `idle`/`done`
with no PR is a strand — divybot re-injects the goal automatically; if it persists,
the agent is wedged (respawn it by closing+reopening the issue, or close the
workspace and let the next tick respawn).

## Open work (the main lever)

```bash
gh issue create --repo <inbox-repo> --label <target-label> --title "..." --body "..."
```

The label must match a configured target. Heuristics from experience:

- **Scope the body like a goal, not a ticket.** State the end condition concretely
  ("port the whole X API surface and land it in one PR", "make baseline cell N
  green"). Vague issues yield vague PRs. Per-target `prompt_hint` already shapes
  scope — don't fight it.
- **Parallelism = more issues.** For a wide effort, file one issue per subsystem so
  each gets its own worker. Don't expect one issue to fan out.
- **Jump the queue** with `priority: N` on the target (higher admits first) so a
  small high-value target isn't starved behind a big backlog.

## Supervise & merge

divybot forwards only *new* reviews, CI failures, and merge conflicts into the
worker's pane (via `herdr agent send`) — you don't relay by hand. On targets with
`automerge: true` it squash-merges a green, mergeable, non-draft PR itself, then
either retires the worker or lets it continue/fan-out the rest. Your job is to
review PRs you care about and decide automerge per target. For a partial PR that
merged, divybot files sibling issues for the remainder — expect follow-up workers.

## Pace the swarm (governor)

The governor caps concurrent agents per account against the real usage meter,
hard-pausing new work at `weekly_ceiling_pct` (default 92). To run hotter or
cooler, edit `governor` in the config and restart:

- `max_active` — ceiling on the adaptive per-account cap (raise to scale up).
- `weekly_ceiling_pct` / `slack_pct` — how close to the weekly cap you'll push.
- Per-host `capacity` — hard slot cap per box.

Check pacing in the logs (governor decisions are logged each tick). If workers
aren't spawning despite open issues, suspect: governor at ceiling, no host with
the required `need_cap`, or every account out of budget this tick.

## Tune targets without code

All live in the config `targets[]`, restart to apply:

- `disabled: true` — pause NEW spawns for a target; live jobs keep running and
  issues are still polled (so they aren't torn down). Use to halt a target without
  losing in-flight work.
- `agents: ["claude","codex"]` — overflow order; spills to codex when claude caps.
- `automerge: true` — let divybot merge green bot PRs (only where it has rights and
  unreviewed bot merges are acceptable).
- `priority`, `prompt_hint`, `need_cap` — admission order, scope guidance, host gating.

## Add / remove a host

No "join". Edit `hosts[]` in the config and restart. A host is usable iff it's on
the tailnet and its herdr answers. Requirements: herdr ≥ 0.7.0 with the claude/codex
integrations installed (`herdr integration install claude` / `codex`), an
unprivileged agent user (bare claude refuses `--dangerously-skip-permissions` as
root), and the capabilities the target's `need_cap` demands. Pin codex to
residential hosts via `"agents": ["claude"]` on datacenter boxes — codex's oauth
TUI gets Cloudflare-challenged from datacenter IPs.

## Deploy a new coordinator binary

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o divybot-linux ./cmd/divybot
scp divybot-linux <coordinator>:/tmp/divybot && rm divybot-linux
ssh <coordinator> "sudo install -m0755 /tmp/divybot /usr/local/bin/divybot && sudo systemctl restart orchid && sudo systemctl status orchid --no-pager"
```

Restarting sheds nothing — live workers keep running in their herdr workspaces and
divybot re-adopts them by label on its next tick. Use systemctl, **never `pkill`**
(its cmdline matches herdr/agent processes and takes the whole host down).

## Troubleshooting (the recurring failure classes)

- **Whole fleet 401 / "Invalid bearer" / "not trusted".** The coordinator's Max
  oauth *refresh* token died (often after a local re-login orphaned it). Fix at the
  coordinator, not the hosts: copy fresh creds up and let divybot re-push.
  ```bash
  # from a machine that's logged in (e.g. your mac keychain):
  security find-generic-password -s "Claude Code-credentials" -w   # macOS
  # write that JSON to the coordinator's ~/.claude/.credentials.json, then:
  ssh <coordinator> "sudo systemctl restart orchid"
  ```
  Then close/respawn the workers that died looping on 401.
- **Workers not spawning, issues open.** Governor at ceiling (check logs), no host
  with `need_cap`, account out of budget, or target `disabled`. Confirm with the
  state file + `herdr agent list` per host.
- **Agent stranded idle/done, no PR.** divybot re-injects the goal; if wedged, close
  the herdr workspace (`herdr workspace close <ws>`) and let the next tick respawn,
  or close+reopen the inbox issue.
- **Green PR sitting unmerged.** Only happens off-automerge or when no live job
  tracks the branch; the per-tick sweep merges orphan green bot PRs on automerge
  targets. If it's not an automerge target, merge it yourself.
- **Private work-repo clone fails on a host.** The agent user needs a working git
  credential helper / gh token for private repos — verify on the host.

## Shared memory

The swarm shares a git-backed `memory/` subtree on the inbox repo; each host clones
it and the agent's `autoMemoryDirectory` points there, so workers read the union and
write locally (merge=union, no clobber). When you learn a durable operational fact
(a build incantation, a host quirk, a maintainer preference), it belongs in memory
so the swarm stops re-deriving it.

## Cadence

Keep a light recurring health check (e.g. hourly): coordinator service up, every
open issue has a live agent, governor not stuck at ceiling, no host dark. Escalate
`blocked` agents; restart a dead coordinator; resync creds on a fleet-wide 401.

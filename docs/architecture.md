# {{illust:branch-tree}} Architecture

A high-level map of what runs where and how the pieces talk. orchid is a single
**headless** coordinator (`divybot`) driving a fleet of agent harnesses over SSH
through a **herdr** fabric, using GitHub issues as the work queue and pull
requests as the unit of output. There is no dashboard binary, no embedded SPA,
and no relay — `herdr --remote <host>` is the UI.

## The shape in one paragraph

`divybot` (one Go file, stdlib only, ~1.2k LOC) polls a GitHub **inbox repo** for
labelled issues. Each label maps to a **target** (a work repo + which agent runs
it). For every open issue with budget, the coordinator pushes credentials, picks
a **host**, asks that host's **herdr** server to spawn a bare agent harness
(`claude` or `codex`/`opencode`) in an isolated workspace, and injects a goal
prompt. The agent works the issue and opens a PR. The coordinator then supervises
that PR — relaying new reviews, CI failures, and conflicts back to the agent, and
(where enabled) squash-merging it when green. A **governor** paces how many agents
run at once against each account's real usage meter. A git-backed **shared
memory** lets the swarm avoid re-deriving the same facts.

This is the *new* orchid. It replaces a ~13k-LOC, 25-file predecessor that used
an SSH+tmux shim, capture-pane scraping, paste-buffer pokes, a `www/` dashboard,
and per-workspace clawpatrol. All of that is gone — collapsed into native herdr
calls and central auth-sync.

## Components

One process you actually run:

1. **`divybot`** — the coordinator, the single Go file in `cmd/divybot`. It owns
   the whole control loop: GitHub polling, credential push, host selection,
   governor, agent spawn, and PR supervision. It is a **thin herdr client** —
   headless, no HTTP server of its own (it only POSTs to ntfy for escalation).
   Single-writer state in a local JSON file (`state_file`).

On each machine:

2. **`herdr`** (≥ 0.7.0) — the per-host runtime, perception layer, **and UI**.
   `divybot` drives it entirely through `herdr` subcommands over plain SSH
   (`ssh <host> herdr <cmd>`, JSON out): `workspace create`, `pane run`,
   `agent list|get|send`. herdr owns the terminal/TUI and reports a native,
   agent-agnostic status (`idle|working|blocked|done|unknown`) — one structured
   call per host per tick, not a 5Hz pane scrape. To watch or drive a host
   directly, run `herdr --remote <host>`. There is no "join": a host is usable
   iff it's on the tailnet and its herdr answers.

3. **Agent harnesses** — `claude --dangerously-skip-permissions` or `codex` (via
   the `opencode` Go-TUI against its ChatGPT-plan oauth). Bare — no clawpatrol
   wrapper. Each agent gets its **own single-pane herdr workspace** and owns
   exactly one issue; herdr launches it in the workspace's root pane via
   `pane run`. The agent user must be unprivileged (bare claude refuses
   `--dangerously-skip-permissions` as root).

4. **Shared memory** — a git-backed knowledge base (a `memory/` subtree on the
   inbox repo). Each host keeps a clone; the agent's `autoMemoryDirectory` points
   at it, so workers read the union and write locally. The coordinator syncs
   hosts **serially** (one committer at a time, no push race) and
   `memory/.gitattributes` sets `* merge=union` so concurrent writes merge
   instead of clobbering.

## Control loop

`divybot` runs one coordinator (`Coord`) loop on the poll interval (default
`30s`). Each tick, in order:

1. **Feed** — mirror newly bot-assigned upstream issues into the inbox.
2. **Sweep merge** — squash-merge any green, ready bot PR on an auto-merge
   target, even one no live job tracks (workers pipeline follow-up PRs on fresh
   branches; jobs die and orphan green PRs otherwise).
3. **Poll issues** — list open issues per target. If any target's `gh` list
   errors, the tick marks the poll unreliable and **skips teardown** — a flaky
   list must never read as "all issues closed" and wipe the swarm.
4. **Teardown** — for jobs whose inbox issue is gone, tear down (with a one-tick
   *fan-out grace* so a worker can split remaining work into sibling issues).
5. **Fleet status** — ask every host's herdr which agents are alive. Respawn jobs
   whose host is up but whose agent vanished — debounced over several consecutive
   misses so one status flicker can't mass-respawn a host.
6. **Admit** — compute per-account budget (`governor cap − running`), then spawn
   new agents for open issues in **priority order** (high-priority targets first,
   then lowest issue number), spilling to the next agent in a target's overflow
   list when the preferred account is throttled.

## Routing: issue → agent → host

- **Target** maps an inbox label to a work repo, an agent (or an ordered
  agent-overflow preference), an optional required host capability (`need_cap`),
  auto-merge opt-in, admission priority, a per-target prompt hint, and a
  `disabled` pause switch.
- **Agent selection** walks the target's agent preference and picks the first
  account with governor budget — so claude work spills to codex when claude is
  capped.
- **Host selection** filters hosts by capability (`need_cap`), by whether the
  host is allowed to place that agent (`Agents` — codex is pinned to residential
  hosts because its oauth TUI gets Cloudflare-challenged from datacenter IPs),
  and by free capacity, then places the agent there.

## Perception & delivery

Everything the coordinator knows about a live agent comes from herdr's native
`agent_status`, not screen scraping. `blocked` → escalate via ntfy; `idle`/`done`
with no PR yet → re-inject the goal (fixes strandings); an `Invalid bearer` /
`not trusted` read → resync creds and respawn. Reviews, CI failures, and
conflicts are forwarded through `herdr agent send` (only *new* items — the
coordinator diffs the PR view against last-seen state), not a paste buffer.

## Governor (quota pacing)

The governor paces the swarm against the subscription quota so it spends evenly
instead of blowing the window early. It reads each account's **real usage meter**
(claude's rate-limit headers / codex's rollout token_count) on a sample interval,
estimates a burn rate over a lookback window, and runs a burn-rate adaptive cap
**per account** — claude and codex pace independently, so a hot claude window
never throttles codex. New work hard-pauses at the weekly ceiling (default 92%
used); it fails open.

## Auth & state

- **State** — a single local JSON file (issue → branch → PR mapping, last-seen
  review IDs, mention cursor). Single-writer, survives restarts.
- **GitHub** — `gh` CLI with a resolved token; the coordinator is the only
  GitHub writer.
- **Agent credentials are central.** The coordinator host holds the single source
  of truth — `~/.claude/.credentials.json`, `~/.codex/auth.json`, and a gh token
  — and pushes them to every host on spawn and every ~20 min (before the oauth
  access token expires). This eliminates the whole per-host stale-credential
  401/JWT failure class.

## Build & run

```bash
go build -o divybot ./cmd/divybot
GH_TOKEN=$(gh auth token) ./divybot -config divybot.json
./divybot -config divybot.json -once    # single tick, for testing
```

Cross-compile + ship for the Linux coordinator:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o divybot-linux ./cmd/divybot
scp divybot-linux <coordinator>:/tmp/divybot
ssh <coordinator> "sudo install -m0755 /tmp/divybot /usr/local/bin/divybot && sudo systemctl restart orchid"
```

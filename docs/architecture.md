# {{illust:branch-tree}} Architecture

A high-level map of what runs where and how the pieces talk. The system is a
single coordinator process driving a fleet of agent harnesses over SSH, using
GitHub issues as the work queue and pull requests as the unit of output.

## The shape in one paragraph

`divybot` (one Go binary) polls a GitHub **inbox repo** for labelled issues. Each
label maps to a **target** (a work repo + which agent runs it). For every open
issue with budget, the coordinator picks a **host**, asks that host's **herdr**
server to spawn an agent harness (`claude` or `opencode`) in an isolated
workspace, and injects a goal prompt. The agent works the issue and opens a PR.
The coordinator then supervises that PR — relaying reviews and CI failures back
to the agent, and (where enabled) squash-merging it when green. A **governor**
paces how many agents run at once against each account's real usage meter. A
git-backed **shared memory** lets the swarm avoid re-deriving the same facts.

## Components

One process you actually run:

1. **`divybot`** — the coordinator, the Go binary in `cmd/divybot`. It owns the
   whole control loop: GitHub polling, host selection, governor, PR supervision,
   credential sync, and the embedded dashboard. Single-writer state in a local
   file. Everything below is something `divybot` talks to.

On each machine:

2. **`herdr`** — the per-host agent driver. This is the big simplification: a
   host is "anything on the tailnet running a herdr server," and `divybot` drives
   it entirely through `herdr` subcommands over SSH (`workspace create`,
   `pane run`, `agent list|get|send`). herdr owns the terminal/TUI, returns
   structured JSON, and reports a native, agent-agnostic status
   (`idle|working|blocked|done`). There is no "join" handshake and no raw `tmux`
   choreography anymore — if the herdr answers, the host is usable.

3. **Agent harnesses** — `claude --dangerously-skip-permissions` or `opencode`
   (the Go-TUI harness that runs codex against its ChatGPT-plan oauth). Each
   agent gets its **own single-pane herdr workspace** and owns exactly one issue.
   herdr launches the agent in the workspace's root pane via `pane run` so the
   workspace stays exactly one pane.

Plus optional pieces:

4. **Shared memory** — a git-backed knowledge base (a `memory/` subtree on the
   inbox repo). Each host keeps a clone; the agent's `autoMemoryDirectory` points
   at it, so workers read the union and write locally. The coordinator syncs
   hosts **serially** (one committer at a time, no push race) and
   `memory/.gitattributes` sets `* merge=union` so concurrent writes merge
   instead of clobbering.

5. **The relay** (`cfrelaytun/relay/`) — a Cloudflare Worker that fronts the
   dashboard with a public subdomain + GitHub OAuth, so you don't expose a public
   IP. Skip it and hit the coordinator's HTTP server directly over the LAN or
   Tailscale — that's the default self-host path.

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
   misses so one screen-scrape flicker can't mass-respawn a host.
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

## Governor (quota pacing)

The governor paces the swarm against the subscription quota so it spends evenly
instead of blowing the window early. It reads each account's **real usage meter**
(claude's statusline `rate_limits`, codex's rollout `token_count`) on a sample
interval, estimates a burn rate over a lookback window, and runs a burn-rate
adaptive cap **per account** — claude and codex pace independently, so a hot
claude window never throttles codex. New work hard-pauses at the weekly ceiling
(default 92% used).

## PR supervision

For each live job the coordinator pulls the PR view and diffs it against
last-seen state: new reviews and failing CI checks get summarized and injected
back into the agent's pane as a follow-up goal. On a green, mergeable, non-draft
PR on an auto-merge target it squash-merges, then either tears the job down or —
when the merged PR was a partial — lets the worker **continue** or **fan out**
the rest into sibling issues.

## Storage & auth

- **State** — single local file (issue → branch → PR mapping, last-seen review
  IDs, mention cursor, usage rollups, dashboard canvas). Single-writer, survives
  restarts.
- **GitHub** — `gh` CLI with a resolved token; the coordinator is the only
  GitHub writer.
- **Agent credentials** — the coordinator holds canonical Claude/codex creds and
  **syncs them to every host** (and refreshes locally), so a token refresh on one
  box doesn't strand the fleet.

## Single binary, embedded dashboard

`divybot` ships the dashboard SPA inside the binary via `go:embed`
(`internal/orch/embed-dist`). One build embeds both:

```bash
cd www && npm run build                 # → internal/orch/embed-dist
go build -o divybot ./cmd/divybot       # embeds + compiles
```

One `scp` + restart replaces the daemon and the UI together.

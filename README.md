# orchid

High-velocity agent orchestration. orchid turns labelled GitHub issues into merged
pull requests by running a fleet of coding agents (`claude` / `codex`) across a pool
of machines, and paces them against your subscription quota.

The coordinator is **`divybot`** — one headless Go file (`cmd/divybot`, stdlib only)
that drives every host through a **herdr** fabric over plain SSH. No dashboard, no
tmux, no relay: herdr is the runtime, the perception layer, and the UI
(`herdr --remote <host>`).

- **Scaling** — from a few agents to hundreds, fanned across whatever compute you have.
- **Usage-limit throttle** — adaptive pacing against your 5-hour and weekly quota,
  per account, so you never run dry mid-flight.
- **Shared memory** — a git-backed knowledge base the swarm maintains itself, so it
  stops re-deriving build incantations, test recipes, and maintainer preferences.
- **Load balancing** — agents dispatch to whichever host has a free slot, over SSH.
- **Central auth** — the coordinator holds the canonical credentials and pushes them
  to every host, killing the per-host stale-token failure class.

Get started by asking your agent to `Setup orchid https://github.com/denoland/orchid`.

## How it works

```
poll GitHub issues → governor-admit → push creds → herdr spawn (bare claude/codex)
   → inject goal → supervise via herdr agent_status → poll PRs, relay reviews/CI/
   conflicts via herdr send → squash-merge green PRs → teardown
```

An inbox issue's **label** maps to a **target** (a work repo + which agent runs it).
For every open issue with budget, divybot picks a host, asks its herdr to spawn a
bare agent in an isolated workspace, injects a goal, and supervises the resulting PR.
One issue = one worker; for wide parallelism, file one issue per slice.

See [docs/architecture.md](docs/architecture.md) for the full picture.

## Cluster

A machine is anything on the tailnet running `herdr` (≥ 0.7.0, with the claude/codex
integrations) as an unprivileged agent user. There is no "join" — add or remove a box
by editing `hosts` in the config. Capabilities (`need_cap`) gate routing so a build
that needs a beefy box doesn't land on a phone.

## Throttle

The governor reads each account's real usage meter and runs a burn-rate adaptive cap
**per account**, hard-pausing new work near the weekly ceiling. claude and codex pace
independently; per-target `priority` jumps the admission queue. See
[docs/throttling.md](docs/throttling.md).

## Memory

A persistent, git-backed knowledge base shared across the cluster — Andrej Karpathy's
["LLM wiki" pattern](https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f):
plain interlinked markdown the agents maintain themselves. See
[docs/memory.md](docs/memory.md).

## Configuration

A single JSON file: inbox repo, hosts, targets, governor knobs, memory. See the
annotated [cmd/divybot/divybot.example.json](cmd/divybot/divybot.example.json).

## Run

```bash
go build -o divybot ./cmd/divybot
GH_TOKEN=$(gh auth token) ./divybot -config divybot.json
./divybot -config divybot.json -once   # single tick, for testing
```

## Operate

Supervising a running swarm — health checks, opening work, watching workers,
governor tuning, deploys, and the recurring failure classes — is covered in
[SKILL.md](SKILL.md). Point any agent harness (Claude Code, etc.) at it and it
operates the swarm the way a seasoned operator would.

## License

MIT License

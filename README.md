# orchid

Run many `claude` / `codex` agents in parallel and turn GitHub issues into merged
PRs. orchid is a single headless Go binary (`divybot`) that drives agents across a
pool of machines through [herdr](https://github.com/denoland/herdr) over SSH.

What it adds on top of a bare agent:

- **Fan-out** — label a GitHub issue, get an agent on a free host working it; scale
  from a few to hundreds across your machines.
- **Quota pacing** — a governor caps concurrency against your real 5h/weekly usage,
  per account, so you don't blow the window early. Spills claude→codex when capped.
- **PR supervision** — forwards new reviews / CI failures / conflicts to the agent,
  and squash-merges green PRs on opt-in targets.
- **Shared memory** — a git-backed wiki the swarm maintains itself, so it stops
  re-deriving build/test/maintainer facts.
- **Central auth** — coordinator holds the creds and pushes them to every host; no
  per-host token rot.

No dashboard, no tmux, no relay — `herdr --remote <host>` is the UI.

## Run

```bash
go build -o divybot ./cmd/divybot
GH_TOKEN=$(gh auth token) ./divybot -config divybot.json
```

Config is one JSON file (inbox repo, hosts, targets, governor) — see
[divybot.example.json](cmd/divybot/divybot.example.json). Open work with
`gh issue create --label <target>`.

## More

- [docs/architecture.md](docs/architecture.md) — what runs where
- [docs/throttling.md](docs/throttling.md) — the governor
- [docs/memory.md](docs/memory.md) — shared memory
- [SKILL.md](SKILL.md) — operating a live swarm

MIT License

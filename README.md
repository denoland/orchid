### orchid

High velocity coding agent orchestration

<img width="601" height="419" alt="image" src="https://github.com/user-attachments/assets/0bc63b69-6f92-406c-befa-aba122fb31d2" />

## What does it do?

File a GitHub issue, orchid spins up a coding agent to ship the PR — then
relays reviews and CI back into the session until it merges. Scale that
from one session to a whole fleet.

**Scale** — from a couple of sessions to hundreds, fanned across every core
you give it.

**Load balancing** — run sessions across a cluster of machines over plain
SSH. Add a box, it joins the pool.

**Mix harnesses** — Claude, Codex, Pi, or opencode, side by side in the same
swarm.

**Usage-limit throttle** — adaptive pacing against your weekly quota so the
swarm never runs out of tokens mid-flight.

<img src="docs/img/feat-throttle.png" width="240" alt="usage and pacing">

**Shared memory** — Karpathy-style memory notes the whole cluster reads and
writes.

**Git-native** — prioritize and manage work through GitHub issues and PRs,
nothing else to learn.

### Setup

The orch daemon runs on Linux (systemd + tmux + ssh). To self-host:

```bash
curl -fsSL https://orchid.littledivy.com/install.sh | bash
```

It builds the `orch` binary, writes a starter `swarm.hcl`, registers a
user-level systemd service, and prints your dashboard URL:

```
http://localhost:8000/?token=<secret>
```

## Documentation

https://orchid.littledivy.com/

## Configuration

See example [./swarm.hcl](swarm.hcl)

## Chat with your orchid

Point any chat-agent runtime (OpenClaw, Hermes, Claude Code) at
<https://orchid.littledivy.com/skill.md> and you get a
Telegram/Slack/Discord bot that knows your swarm. One paste:

```bash
npx -y @openclaw/cli@latest skill install https://orchid.littledivy.com/skill.md
```

See [docs/SUPERVISION.md](docs/SUPERVISION.md).

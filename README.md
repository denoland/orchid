## orchid 

High velocity agent orchestration

It integrates with Github issues for scheduling work and opening PRs while trying to solve the speed and scaling problems with agents:

- **Scaling**: from a couple of sessions to hundreds, fanned across bit of compute.
- **Usage-limit throttle**: adaptive pacing against your weekly quota.
- **Shared memory**: Karpathy-style memory notes shared across the cluster.
- **Load balancing**: run sessions across a cluster of machines over plain SSH.
- **Security**: Works well with [clawpatrol](https://clawpatrol.dev) security firewall

Get started at https://orchid.littledivy.com/docs/getting-started

```
curl -fsSL https://orchid.littledivy.com/install.sh | bash
```

### Cluster

A machine is anything that runs `tmux` + your agent over SSH. orchid drives
them all, with no agent or inbound ports on the workers. Sessions dispatch to
whichever host has a free slot, and each host can run multiple agent harnesses
(`claude`, `codex`, …). See the [VMs docs](https://orchid.littledivy.com/docs/vms).

<img height="319" alt="dashboard" src="https://github.com/user-attachments/assets/0bc63b69-6f92-406c-befa-aba122fb31d2" />

### Usage-limit throttle

The governor paces the swarm against your 5-hour and weekly quota, braking
velocity as you approach the cap so you never run dry mid-flight. Each agent
account (claude / codex) meters independently against its own
quota; per-issue `priority = N` jumps the queue. See the
[Throttling docs](https://orchid.littledivy.com/docs/throttling).

### Memory

A persistent, git-backed knowledge base the swarm shares across the cluster, so it
stops re-deriving the same things (build incantations, test recipes, maintainer
preferences). It's [Andrej Karpathy's "LLM wiki"
pattern](https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f): 
plain interlinked markdown the agents maintain themselves.

### Configuration

See example [./swarm.hcl](swarm.hcl)

### Chat with your orchid

Point any agent harness (OpenClaw, Hermes, Claude Code) at
<https://orchid.littledivy.com/skill.md> and you get a
Telegram/Slack/Discord bot that knows your swarm. One paste:

```bash
npx -y @openclaw/cli@latest skill install https://orchid.littledivy.com/skill.md
```

See [docs/supervision.md](docs/SUPERVISION.md).


### orchid

Orchestration and project management with coding agents

Visit https://orchid.littledivy.com to get started

<img width="535" height="412" alt="image" src="https://github.com/user-attachments/assets/bea3cf08-5322-4a1c-954e-98ff2eccd8c5" />




### Setup

The orch daemon runs on Linux (systemd + tmux + ssh). To self-host:

```bash
curl -fsSL https://orchid.littledivy.com/install.sh | bash
```

Or sign in at https://orchid.littledivy.com and run `orch join` to attach
this machine as a worker — the daemon stays in the cloud and your local
box (Linux or macOS) acts as the agent host:

```bash
orch join wss://username.orchid.littledivy.com/agent <TOKEN>
```

Then access your dashboard at https://username.orchid.littledivy.com

## Configuration

See [./swarm.hcl](swarm.hcl)

## Chat with your orchid

Point any chat-agent runtime (OpenClaw, Hermes, Claude Code) at
<https://orchid.littledivy.com/skill.md> and you get a
Telegram/Slack/Discord bot that knows your swarm. One paste:

```bash
npx -y @openclaw/cli@latest skill install https://orchid.littledivy.com/skill.md
```

See [docs/SUPERVISION.md](docs/SUPERVISION.md).

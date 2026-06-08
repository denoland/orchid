### orchid

High velocity coding agent orchestration

<img width="601" height="419" alt="image" src="https://github.com/user-attachments/assets/0bc63b69-6f92-406c-befa-aba122fb31d2" />

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

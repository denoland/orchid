### orchid

Orchestration and project management with coding agents

Visit https://orchid.littledivy.com to get started

<img width="535" height="412" alt="image" src="https://github.com/user-attachments/assets/bea3cf08-5322-4a1c-954e-98ff2eccd8c5" />




### Setup

On a Linux/macOS machine run:
```
curl -fsSL https://orchid.littledivy.com/install.sh | sh
```

Login https://orchid.littledivy.com and copy your setup token
```
orch join wss://username.orchid.littledivy.com/agent <TOKEN>
```

Access your dashboard at https://username.orchid.littledivy.com

## Configuration

See [./swarm.hcl](swarm.hcl)

## Supervising from your phone

Orchid pairs nicely with a self-hosted chat agent (OpenClaw, Hermes Agent).
The agent reads `CLAUDE.md` as its skill file and talks to orchid over SSH
+ `gh` so you can check status, file work, or restart the swarm from
Telegram/Slack/Discord. See [docs/SUPERVISION.md](docs/SUPERVISION.md).

### orchid

Orchestration and project management with coding agents.

Orchid is a self-hosted swarm of coding agents that ship pull requests.
You file issues; orchid spawns a `claude` (or codex) session for each,
relays reviews and CI, and tears down once the PR merges. It runs
entirely on your own machines — no accounts, no third-party service.

<img width="601" height="419" alt="image" src="https://github.com/user-attachments/assets/0bc63b69-6f92-406c-befa-aba122fb31d2" />

### Setup

The orch daemon runs on Linux (systemd + tmux + ssh). One-liner on the
box that will host the swarm:

```bash
curl -fsSL https://orchid.littledivy.com/install.sh | bash
```

It builds the `orch` binary, writes a starter `swarm.hcl`, registers a
user-level systemd service, and prints your dashboard URL:

```
http://<host>:8000/?token=<secret>
```

The daemon serves its own dashboard, polls the inbox repo, and drives
sessions over plain SSH. See [docs/getting-started.md](docs/getting-started.md).

## Configuration

See [./swarm.hcl](swarm.hcl) and [docs/configuration.md](docs/configuration.md).

## Remote access

The dashboard binds a local port. To reach it from outside the LAN
without exposing a public IP, use [Tailscale](docs/tailscale.md), or
deploy the optional relay worker in [`cf/`](cf/) to your own Cloudflare
account + domain.

## Chat with your orchid

Point any chat-agent runtime (OpenClaw, Hermes, Claude Code) at the
skill file and you get a Telegram/Slack/Discord bot that knows your
swarm. One paste:

```bash
npx -y @openclaw/cli@latest skill install https://orchid.littledivy.com/skill.md
```

See [docs/SUPERVISION.md](docs/SUPERVISION.md).

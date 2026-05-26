# Supervising orchid with a chat agent

Orchid does not ship a built-in supervisor. Instead, run a general-purpose
self-hosted agent (OpenClaw or Hermes) alongside it. The agent gets
`SKILL.md` as its skill file and talks to GitHub + the orchid host on your
behalf. You chat with the agent over Telegram, Slack, Discord, etc., and it
tells you what the swarm is doing — or kicks it back to life when needed.

This pattern works because orchid's surface is already a few well-defined
endpoints: SSH to the host for tmux + systemd, the `gh` CLI for issues and
PRs, and the `/api/state` JSON dump for live job status. Any agent that
can run shell commands can manage orchid.

## What the agent needs

Give it:

1. **The skill file.** `SKILL.md` at the repo root is the operator
   handbook. It lists the VM address, binary paths, log location, dashboard
   URL, restart command, and how to file issues.
2. **SSH access to the orch host.** A non-root account with `tmux` and
   `systemctl --user` privileges is enough. The agent uses this to read
   logs, capture panes, and restart the service.
3. **A GitHub token.** Scoped to the inbox repo (`denoland/orchid` in the
   default config) and any target repos. The agent opens issues to enqueue
   work, comments on PRs, and closes inbox issues when work merges.
4. **The dashboard URL + bearer token.** Optional but useful — the agent
   can call `/api/state` for a structured view instead of scraping panes.

## Path A — OpenClaw

OpenClaw is Node.js, ships native adapters for Telegram, Slack, Discord,
WhatsApp, iMessage, etc. Heaviest install but most platforms out of the
box.

```bash
# 1. install
npm install -g @openclaw/cli   # or use the curl|sh from openclaw.ai

# 2. drop the orchid skill into the workspace
mkdir -p ~/.openclaw/workspace/skills/orchid
cp /path/to/orchid/SKILL.md ~/.openclaw/workspace/skills/orchid/SKILL.md

# 3. configure messaging adapter (Telegram shown here)
openclaw gateway setup
#   → paste TELEGRAM_BOT_TOKEN from @BotFather
#   → set TELEGRAM_ALLOWED_USERS to your numeric chat id

# 4. wire SSH + gh
#    OpenClaw picks up ~/.ssh/config and the host's gh login automatically.
#    Add an entry for the orch host:
cat >> ~/.ssh/config <<'EOF'
Host orchid-host
  HostName 65.20.66.139
  User root
EOF

# 5. start it
openclaw gateway start
```

Now message the bot: `what's happening on orchid?` — the agent reads
`SKILL.md`, SSHs into `orchid-host`, runs the diagnostic commands listed
there, and replies.

Docs: <https://docs.openclaw.ai>

## Path B — Hermes Agent

Hermes is Python (`pipx install hermes-agent`), MIT-licensed, single
process connects to 10+ messaging platforms simultaneously with shared
memory.

```bash
# 1. install
pipx install hermes-agent

# 2. skill file
mkdir -p ~/.hermes/skills/orchid
cp /path/to/orchid/SKILL.md ~/.hermes/skills/orchid/SKILL.md

# 3. env vars (~/.hermes/.env)
cat > ~/.hermes/.env <<'EOF'
ANTHROPIC_API_KEY=sk-ant-...
# or: skip this and pre-run `claude login` to use the Max subscription

TELEGRAM_BOT_TOKEN=...
TELEGRAM_ALLOWED_USERS=123456789
TELEGRAM_HOME_CHANNEL=-1001234567890

GH_TOKEN=ghp_...
EOF

# 4. start the gateway
hermes gateway start
```

Hermes picks up SSH keys from `~/.ssh/` automatically. Add the orch host
to `~/.ssh/config` as shown in Path A.

Docs: <https://hermes-agent.nousresearch.com/docs>

## Useful prompts

Drop these in the skill file or send them ad-hoc:

- `Check orchid health and tell me if anything is stuck.`
- `Open an issue in denoland/orchid asking the swarm to fix X.`
- `Tail the orch.log on the host and summarise the last 200 lines.`
- `Restart orchid if no PR has been opened in the last hour.`
- `Show me the pane for claude-232 and explain what it's waiting on.`

## Why not built-in?

Orchid intentionally stops at "orchestrate coding agents over GitHub
issues". A supervisor is a chat-bot harness — a different problem with
mature solutions. Bundling either OpenClaw or Hermes would double the
codebase, force a Node.js or Python dep on every orch host, and replicate
work already covered by those projects. Use whichever fits your stack.

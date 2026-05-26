# Chat with your orchid

{{illust:chat-vine}}

Orchid hosts a skill file at <https://orchid.littledivy.com/skill.md>. Point
any chat-agent runtime at it and you get a Telegram/Slack/Discord bot that
knows how to check on your swarm and kick it back to life.

## TL;DR

Tell the agent:

> I have orchid running at `mybox.example.com`. Use the skill at
> <https://orchid.littledivy.com/skill.md>.

That's the whole setup, as long as the agent can fetch URLs, SSH, and run
`gh`.

## OpenClaw (one paste)

```bash
npx -y @openclaw/cli@latest skill install https://orchid.littledivy.com/skill.md
npx -y @openclaw/cli@latest gateway start
```

Run `openclaw gateway setup` first to wire your Telegram/Slack token if you
haven't already. Docs: <https://docs.openclaw.ai>

## Hermes Agent (one paste)

```bash
pipx install hermes-agent
hermes skills add https://orchid.littledivy.com/skill.md
hermes gateway start
```

Tokens live in `~/.hermes/.env` (`TELEGRAM_BOT_TOKEN`, `GH_TOKEN`,
`ANTHROPIC_API_KEY` or a pre-existing `claude login`). Docs:
<https://hermes-agent.nousresearch.com/docs>

## What the agent needs to reach

- **SSH** to the orch host. Non-root account with `tmux` + `systemctl --user`.
- **`gh` CLI** logged in with a token scoped to the inbox repo + target repos.
- **Dashboard bearer token** (optional). Lets the agent hit `/api/state` for
  a structured view instead of scraping panes.

The skill file tells the agent which commands to run on each — you don't
need to paste them yourself.

## Things you can say

- `Check orchid health and tell me if anything is stuck.`
- `Open an issue in denoland/orchid asking the swarm to fix X.`
- `Tail orch.log and summarise the last 200 lines.`
- `Restart orchid if no PR has been opened in the last hour.`
- `Show me pane claude-232 and explain what it's waiting on.`

## Why not built-in?

Bot harnesses are a different problem with mature solutions. Bundling
either OpenClaw or Hermes would force a Node.js or Python dep on every
orch host. Use whichever you already trust.

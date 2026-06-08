# Getting started

Orchid is a self-hosted swarm of coding agents that ship pull requests.
You file issues; orchid spawns a `claude` (or codex) session for each
one, relays reviews and CI, and tears down once the PR merges. This
guide gets you from zero to your first merged PR in about five minutes.

It all runs on your own hardware — there's no account to create and
nothing phones home.

## 1. Install on a machine

Pick the machine that will run the swarm. A Linux VPS, a Mac with
plenty of RAM, or a spare workstation all work. From that machine:

```bash
curl -fsSL https://orchid.littledivy.com/install.sh | bash
```

The installer is `bash`-only, runs as your normal user, and uses
`sudo` only to install package deps + enable `loginctl` linger. It
will:

- fetch a Go toolchain if one isn't already installed
- clone `denoland/orchid` into `$HOME/.orch/src`
- build the `orch` binary into `$HOME/.local/bin/orch`
- write a starter `$HOME/.orch/swarm.hcl`
- register a user-level systemd service that survives logout

Prefer to build it yourself? Clone the repo and `go build ./cmd/orch`
— the installer is just a convenience wrapper. Re-run it any time to
update.

## 2. Open the dashboard

When it finishes, the installer prints your dashboard URL with a bearer
token baked in:

```
http://<host>:8000/?token=<http_secret>
```

That's it — `orch` serves its own dashboard on a local port. Open it
from the LAN, or see [Remote access](#5-remote-access) below to reach
it from anywhere.

## 3. Connect GitHub

In the dashboard, open **Settings → Integrations** and click **Connect
GitHub**. This gives the swarm the token it needs to poll issues, push
branches, and open PRs as your bot account.

## 4. Open your first issue

In the inbox repo (the repo you point `github.inbox_repo` at), file an
issue with a label that matches a `target` block — e.g. `clawpatrol`,
`deno`, or whatever you defined. Orchid sees the label on the next
30-second poll, picks a free VM slot, clones the target repo, and
starts a Claude session.

Watch progress on the dashboard. The session card flips through
*spawning → working → PR opened → reviewing → merged*. When you
review the PR (or CI fails), orchid pastes the feedback back into the
running pane so Claude can address it.

## 5. Remote access

The dashboard binds a local port (`orchestrator.http_addr`, default
`:8000`). To reach it from outside the LAN without exposing a public
IP, you have two options:

- **[Tailscale](/docs/tailscale)** — put the host on your tailnet and
  hit it from any device. No third-party domain, no port forwarding.
- **The relay (`cf/`)** — an optional Cloudflare Worker in the repo
  that gives you a public subdomain + GitHub OAuth in front of the
  dashboard. Deploy it to your own Cloudflare account and domain.

## What next

- [Configuration](/docs/configuration) — the `swarm.hcl` reference
- [VMs](/docs/vms) — scale to multiple VMs
- [Targets](/docs/targets) — route different labels to different repos
- [Memory](/docs/memory) — the shared knowledge base the swarm builds up
- [Supervision](/docs/supervision) — chat with your orchid from Telegram

# {{illust:config-knot}} Configuration

{{mockup:config}}

Orchid reads a single HCL file at `$HOME/.orch/swarm.hcl` (or wherever
you point `--config` at). The installer writes a starter with sensible
defaults; edit it through the dashboard's **Settings** panel or
directly on disk.

Changes to `allowed_logins` hot-apply; everything else needs an
`orch restart` (or `systemctl --user restart orchid`).

## Top-level blocks

```hcl
github {
  inbox_repo = "denoland/orchid"
}

orchestrator {
  poll_interval = "30s"
  state_db      = "/home/divy/.orch/state.db"
  branch_prefix = "orch/issue-"
  workdir_root  = "/home/divy/.orch/work"
  http_addr     = ":8000"
  http_secret   = "<32-hex>"
  bot_login     = "divybot"
  ntfy_topic    = "orchid-..."
  allowed_logins = ["alice", "bob"]   # extra GitHub logins that can see the dashboard
}
```

| Field | Meaning |
|-------|---------|
| `github.inbox_repo` | Repo where you open issues to dispatch work. |
| `orchestrator.poll_interval` | How often orch polls GitHub for new issues. |
| `orchestrator.state_db` | SQLite path; survives restarts. |
| `orchestrator.branch_prefix` | Per-issue branch name prefix. |
| `orchestrator.workdir_root` | Where each Claude session clones into. |
| `orchestrator.http_addr` | Local bind for the dashboard server. |
| `orchestrator.http_secret` | Bearer token for the dashboard + capture endpoint. |
| `orchestrator.bot_login` | GitHub login Claude commits as. |
| `orchestrator.ntfy_topic` | Optional ntfy.sh topic for PR-merged push notifications. |
| `orchestrator.allowed_logins` | GitHub usernames that can read your dashboard via the relay. |

## Targets

A target maps an issue label to a work repo:

```hcl
target "deno" {
  label = "deno"
  repo  = "denoland/deno"
}

target "clawpatrol" {
  label = "clawpatrol"
  repo  = "denoland/clawpatrol"
}
```

Issue in the inbox labeled `clawpatrol` → orch clones `denoland/clawpatrol`,
Claude opens the PR there. See [Targets](/docs/targets).

## VMs

Each `vm` block declares a host with capacity:

```hcl
vm "local" {
  host         = "localhost"
  capacity     = 20
  session_cmd  = "runuser -u orchid -- claude --dangerously-skip-permissions"
  session_home = "/home/orchid"
}

vm "worker-1" {
  host = "user@worker.example.com"
  capacity = 10
}
```

See [Workers](/docs/workers) for adding remote VMs via `orch join vm`.

## Bootstrap prompt

The system prompt pasted into each fresh Claude session lives in
`bootstrap_prompt`. Placeholders use `{{...}}` (not `${...}`):

| Placeholder | Substituted with |
|-------------|------------------|
| `{{issue.number}}`, `{{issue.title}}`, `{{issue.body}}` | The inbox issue. |
| `{{target.repo}}` | Target repo from the matching label. |
| `{{branch}}` | Per-issue branch name. |
| `{{workdir}}` | Local clone path. |
| `{{inbox.repo}}` | The inbox repo (for "Closes #N" references). |

Use it to set tone, scoring rules, or your team's PR style guide.

## Capture

If you want the macOS / iOS Orchid Capture apps to deposit drafts as
issues, add a `capture` block:

```hcl
capture {
  auth_token  = "<32-hex>"
  assets_dir  = "/home/divy/.orch/captures"
  public_url  = "https://<sub>.orchid.littledivy.com"
}
```

See [Capture](/docs/capture) for the apps and draft payload.

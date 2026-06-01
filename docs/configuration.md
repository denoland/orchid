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

## Machines

A `machine` block declares one host and the agent slots that run on it.
Each nested `agent` block becomes a runnable VM slot (capacity, account,
auth). Host-level fields (`host`, `user`, `key`, `session_home`,
`workdir_root`, `join_managed`, `sccache`) are shared by every slot — you
write them once instead of repeating a `vm` block per agent:

```hcl
machine "mac-mini" {
  host         = "0.0.0.0"
  user         = "divy"
  key          = "/root/orch/vm-keys/mac-mini"
  session_home = "~"
  workdir_root = "/Users/divy/orch-work"
  join_managed = true

  agent "claude" { capacity = 7 }
  agent "codex"  { capacity = 2 }
  agent "codex"  {                       # second codex on a different account
    account    = "codex-mini"
    capacity   = 1
    codex_home = "$HOME/.codex-mini"
  }
}
```

Each slot expands to a VM named `<machine>-<account>` (e.g. `mac-mini-claude`,
`mac-mini-codex`, `mac-mini-codex-mini`). Set `name = "..."` on an `agent`
block to pin a slot's VM name — useful when migrating an existing `vm`-block
config so running sessions keep matching their slot.

| Agent field | Meaning |
|-------------|---------|
| `account` | Credential account for this slot (default = the agent name). |
| `capacity` | Max concurrent sessions on this slot. |
| `codex_home` | `CODEX_HOME` for a codex slot (isolates a second codex account). |
| `session_cmd` | Override the launch command for this slot. |
| `name` | Pin the expanded VM name (default `<machine>-<account>`). |

The older `vm "name" { ... }` block (one block per host+agent) is still
supported and can be mixed with `machine` blocks. See [VMs](/docs/vms) for
adding remote hosts via `orch join vm`.

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

## Pacing (throttle + governor)

On a metered plan an unthrottled swarm burns the weekly quota in days. The
`throttle` block (inside `orchestrator`) paces work to land near your limit at
reset instead of exhausting it early:

```hcl
orchestrator {
  # …
  throttle {
    enabled           = true
    governor_enabled  = true   # adaptive concurrency cap from measured burn rate
    duty_cycle        = true   # pause lowest-priority sessions to shed load
    max_active        = 20
    poke_min_interval = "15m"  # debounce review/CI re-pokes (each is a full-context turn)
  }
}
```

The **governor** measures burn against a linear pace target and lowers the
admission cap when you're ahead of pace; **duty-cycle** pauses (SIGSTOP) the
lowest-priority running sessions and resumes them when there's headroom.
Per-issue `priority = N` in an issue's toml frontmatter floats important work to
the front of the queue. Each account (claude / codex) paces independently — the
**Analytics** tab shows each account's 5h/weekly burn and projected
end-of-week.

See [Throttling & pacing](/docs/throttling) for the full mechanism and every knob.

## Memory

A git-backed shared knowledge base the swarm reads and writes across sessions.
Add a `memory` block inside `orchestrator`:

```hcl
orchestrator {
  # …
  memory {
    enabled       = true
    repo          = "denoland/orchid"  # default: github.inbox_repo
    branch        = "main"
    dir           = "memory"
    sync_interval = "5m"
  }
}
```

See [Memory](/docs/memory) for how it works and the Memory dashboard tab.

## Credentials

How agent (claude / codex) auth reaches each worker. A pluggable provider —
default `local` keeps the creds in orch and writes them onto every VM at spawn,
so you never log in per-box or copy `auth.json` around.

```hcl
orchestrator {
  # …
  credentials {
    provider = "local"  # default; or a provider plugin like "clawpatrol"
  }
}
```

Creds are keyed by **account** (a VM's `account` field). Add one with
`orch creds import <account> --agent <claude|codex> --from <dir>`. See
[Credentials](/docs/credentials).

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

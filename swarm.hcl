# orchid swarm config — example.
#
# Copy to where you run `orch -config` (the installer writes
# /etc/orchid/swarm.hcl). Required blocks are uncommented; optional knobs are
# shown commented out with their defaults. Full reference:
# https://orchid.littledivy.com/docs/configuration

github {
  inbox_repo = "your-org/your-inbox" # issues filed here drive the swarm
}

orchestrator {
  poll_interval = "30s"
  state_db      = "/var/lib/orchid/state.db"
  branch_prefix = "orch/issue-"
  workdir_root  = "/var/lib/orchid/orch-work"

  http_addr   = ":8000"     # dashboard bind address
  http_secret = "change-me" # dashboard bearer token — `openssl rand -hex 16`

  # GitHub identity the agent commits and pushes as. Defaults to the
  # gh-authenticated user on the host.
  # bot_login = "mybot"
  # bot_email = "mybot@users.noreply.github.com"

  # Extra GitHub logins allowed to view the dashboard (via the optional relay).
  # allowed_logins = ["teammate1", "teammate2"]

  # Git-backed shared memory: agents' notes are committed to a repo and shared
  # across the swarm, browsable in the dashboard's Memory tab. Off by default.
  # memory {
  #   enabled       = true
  #   repo          = "your-org/your-inbox" # default: inbox_repo
  #   branch        = "main"
  #   dir           = "memory"
  #   sync_interval = "5m"
  # }

  # Usage-limit throttle + pacing governor. Off by default (reactive only).
  # Paces the swarm against your 5h/weekly quota so it never runs dry.
  # throttle {
  #   governor_enabled   = true
  #   weekly_ceiling_pct = 92   # hard stop as the weekly quota nears this
  #   max_active         = 8    # adaptive ceiling on concurrent sessions
  #   duty_cycle         = true # SIGSTOP/SIGCONT pacing of running sessions
  #   # sample_interval    = "90s"
  #   # max_context_tokens = 500000 # /clear a session past this (token saver)
  # }

  # Auto-spawn sessions from @mentions in the inbox org. Off by default.
  # mentions {
  #   org         = "your-org"
  #   acknowledge = true
  # }
}

# Targets map an inbox issue label → a work repo. An issue labeled `deno` makes
# orch clone the repo, run a session, and open a PR there. Add one per repo.
target "deno" {
  label = "deno"
  repo  = "denoland/deno"
}

# target "myrepo" {
#   label = "myrepo"
#   repo  = "your-org/myrepo"
# }

# Machines run the agent sessions, driven over SSH. "localhost" runs in-process;
# add more `machine` blocks (host = "user@box" or a Tailscale name) to scale out.
machine "local" {
  host         = "localhost"
  session_home = "/home/orchid"

  # One `agent` block per agent/account on this machine. The label is the agent
  # CLI (`claude` or `codex`); capacity = max concurrent sessions.
  agent "claude" {
    capacity = 8

    # session_cmd is the exact command tmux launches per session. The default
    # runs `claude --dangerously-skip-permissions` as the session_home user.
    # Override to set the run-user, inject git identity / env, or wrap the agent
    # (e.g. pipe it through clawpatrol for egress control).
    # session_cmd = "runuser -u orchid -- claude --dangerously-skip-permissions"
  }

  # A second account on the same box — e.g. codex alongside claude:
  # agent "codex" {
  #   capacity   = 4
  #   account    = "codex"
  #   codex_home = "$HOME/.codex" # isolate this codex login's auth + telemetry
  # }
}

# machine "fra1" {
#   host         = "orchid@worker.fra1.example.com" # or a Tailscale name
#   session_home = "/home/orchid"
#   agent "claude" { capacity = 16 }
# }

# Prompt pasted into every fresh session. Placeholders use {{...}} (not ${...})
# to avoid HCL interpolation.
bootstrap_prompt = <<EOT
You are implementing GitHub issue #{{issue.number}} from {{inbox.repo}}: "{{issue.title}}"

Work repo: {{target.repo}}
Clone: {{workdir}}
Branch: {{branch}}
Git remote `origin` is already authenticated via SSH.

--- issue body ---
{{issue.body}}
--- end issue body ---

## Memory — check it FIRST

Past sessions on this repo left notes in your memory (build/test recipes,
environment quirks, maintainer preferences, dead-ends). Before reading the
codebase or building, CONSULT YOUR MEMORY — don't re-derive what's already known.
When you learn something durable and reusable, SAVE it so the next session
inherits it.

## Your job

You are running FULLY AUTONOMOUSLY — no human is watching this session. Never
ask the user a question and never open an interactive prompt or plan-mode menu
(AskUserQuestion / ExitPlanMode): there is nobody to answer, so it strands the
session. When you hit a fork or an ambiguous decision, pick the best option
yourself from the issue's goal and proceed. Ship your judgment in the PR — the
human reviews it there, not mid-session.

Implement this fully. Read the codebase, understand it deeply, make the change.
Large refactors are expected — do not avoid them. If the right fix touches 10
files, touch 10 files. If it requires redesigning a data structure, redesign it.

Do NOT stop early. Do NOT mark anything done without shipping a PR. The only
acceptable outcome is a merged PR or an open PR awaiting review.

If something is hard, work through it. Read more code, try a different approach,
break the problem into smaller pieces — but keep going. Giving up and exiting
without a PR wastes the entire session.

If you get blocked on one approach, try another. Partial implementations that
compile and pass tests are better than nothing — ship what you have and note
what remains in the PR description.

## When done

Commit, push to `{{branch}}`, then:

```
gh pr create --repo {{target.repo}} \
  --title "..." \
  --body "..."
```

Reference the inbox issue in the PR body: "Closes {{inbox.repo}}#{{issue.number}}"
(cross-repo closes don't auto-link, the orchestrator handles teardown).

If your fix needs a change in an upstream/dependency repo, open that PR too and
reference it in this PR's description (e.g. "Upstream: owner/repo#123"). The
orchestrator tracks linked upstream PRs and relays their reviews/CI back to you.

Then stop and wait. The orchestrator sends a follow-up when reviews, comments,
or CI results arrive. Address them, push fixes, stop again.
The session ends automatically when the PR merges or closes.
EOT

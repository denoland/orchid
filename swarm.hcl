github {
  inbox_repo = "denoland/orchid"
}

orchestrator {
  poll_interval = "30s"
  state_db      = "/root/orch/state.db"
  branch_prefix = "orch/issue-"
  workdir_root  = "/home/orchid/orch-work"
  http_addr     = ":8000"
  http_secret   = "123"
  bot_login     = "divybot"
  ntfy_topic    = "REDACTED"

  # Git-backed shared memory. Agents' auto-memory is redirected per target repo
  # into a clone of this repo (memory/<owner>/<repo>/*.md); orch commits + pushes
  # on sync_interval, so the swarm's accumulated knowledge is durable, versioned,
  # browsable on GitHub, and shared across boxes. Dashboard "Memory" tab reads it.
  memory {
    enabled       = true
    repo          = "denoland/orchid" # default: inbox_repo
    branch        = "main"
    dir           = "memory"
    sync_interval = "5m"
  }
}

# Each target maps an issue label (in the inbox repo) to a work repo.
# Issue labeled `deno` → orch clones denoland/deno, claude opens PR there.
target "deno" {
  label = "deno"
  repo  = "denoland/deno"
}

target "orchid" {
  label = "orchid"
  repo  = "denoland/orchid"
}

target "clawpatrol" {
  label = "clawpatrol"
  repo  = "denoland/clawpatrol"
}

target "sui" {
  label = "sui"
  repo  = "denoland/sui"
}

target "fastwebsockets" {
  label = "fastwebsockets"
  repo  = "denoland/fastwebsockets"
}

target "clawpatrol-deno" {
  label = "clawpatrol-deno"
  repo  = "denoland/clawpatrol-deno"
}

# Placeholders use {{...}} (not ${...}) to avoid HCL interpolation.
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

machine "local" {
  host         = "localhost"
  session_home = "/home/orchid"

  agent "claude" {
    capacity    = 30
    session_cmd = "runuser -u orchid -- env XDG_RUNTIME_DIR=/run/user/1001 GIT_AUTHOR_NAME=divybot GIT_AUTHOR_EMAIL=divybot@users.noreply.github.com GIT_COMMITTER_NAME=divybot GIT_COMMITTER_EMAIL=divybot@users.noreply.github.com claude --dangerously-skip-permissions"
  }
}

target "deno_core" {
  label = "deno_core"
  repo  = "denoland/deno"
}

github {
  inbox_repo = "denoland/orchid"
  token_env  = "GITHUB_TOKEN"
}

orchestrator {
  poll_interval = "30s"
  state_file    = "/root/orch/state.json"
  branch_prefix = "orch/issue-"
  workdir_root  = "/home/orchid/orch-work"
  http_addr     = ":8000"
  bot_login     = "divybot"
  bot_email     = "divybot@users.noreply.github.com"
  ntfy_topic    = "REDACTED"
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

Then stop and wait. The orchestrator sends a follow-up when reviews, comments,
or CI results arrive. Address them, push fixes, stop again.
The session ends automatically when the PR merges or closes.
EOT

# vm "tiger-fusion" {
#   host = "tiger-fusion.exe.xyz"
#   user = "exedev"
#   key  = "~/.ssh/id_ed25519"
# }

# vm "divybot1" {
#   host = "divybot1.exe.xyz"
#   user = "exedev"
#   key  = "~/.ssh/id_ed25519"
#   # Per-VM identity override; falls back to orchestrator.bot_login/bot_email if unset.
#   # bot_login = "divybot1"
#   # bot_email = "divybot1@users.noreply.github.com"
# }

# vm "divybot2" {
#   host = "divybot2.exe.xyz"
#   user = "exedev"
#   key  = "~/.ssh/id_ed25519"
# }

# vm "divybot3" {
#   host = "divybot3.exe.xyz"
#   user = "exedev"
#   key  = "~/.ssh/id_ed25519"
# }

# vm "divybot4" {
#   host = "divybot4.exe.xyz"
#   user = "exedev"
#   key  = "~/.ssh/id_ed25519"
# }

vm "local" {
  host        = "localhost"
  capacity    = 6
  session_cmd  = "runuser -u orchid -- clawpatrol run -- claude --dangerously-skip-permissions"
  session_home = "/home/orchid"
}

github {
  repo      = "denoland/orchid"
  label     = "claude"
  token_env = "GITHUB_TOKEN"
}

orchestrator {
  poll_interval = "30s"
  state_file    = "/home/exedev/orch/state.json"
  branch_prefix = "orch/issue-"
}

# Placeholders use {{...}} (not ${...}) to avoid HCL interpolation.
bootstrap_prompt = <<EOT
You are shepherding GitHub issue #{{issue.number}}: {{issue.title}}

--- issue body ---
{{issue.body}}
--- end issue body ---

Plan, implement, and push to branch `{{branch}}`. Open a PR that closes this
issue. Then stop and wait — the orchestrator will send a follow-up message
each time there is a new review, comment, CI status change, or push.

When you receive a follow-up, address it (push fixes if needed) and stop again.
The session ends automatically when the PR is merged or closed.
EOT

vm "tiger-fusion" {
  host = "tiger-fusion.exe.xyz"
  user = "exedev"
  key  = "~/.ssh/id_ed25519"
}

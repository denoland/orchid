github {
  inbox_repo = "denoland/orchid"
  token_env  = "GITHUB_TOKEN"
}

orchestrator {
  poll_interval = "30s"
  state_file    = "/home/exedev/orch/state.json"
  branch_prefix = "orch/issue-"
  workdir_root  = "/home/exedev/orch-work"
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
You are shepherding GitHub issue #{{issue.number}} from the inbox repo
{{inbox.repo}}: "{{issue.title}}"

The work repo for this issue is {{target.repo}}. You are running inside a
fresh clone of {{target.repo}} at {{workdir}}, with the git remote `origin`
pointing at it (SSH auth is already configured).

--- issue body ---
{{issue.body}}
--- end issue body ---

Plan, implement, commit, and push to branch `{{branch}}` on origin. Open a PR
against {{target.repo}} that closes the issue (use `gh pr create --repo
{{target.repo}}` and reference the inbox issue with a link, e.g. "Closes
{{inbox.repo}}#{{issue.number}}" — note GitHub doesn't auto-close cross-repo,
that's fine, the orchestrator handles teardown).

Then stop and wait — the orchestrator will send a follow-up message each time
there is a new review, comment, CI status change, or push to your PR.

When you receive a follow-up, address it (push fixes if needed) and stop again.
The session ends automatically when the PR is merged or closed.
EOT

vm "tiger-fusion" {
  host = "tiger-fusion.exe.xyz"
  user = "exedev"
  key  = "~/.ssh/id_ed25519"
}

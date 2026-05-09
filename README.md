# orchid

A minimal exe.dev VM swarm orchestrator for `claude` sessions.

You open an issue in the inbox repo, label it for a target work repo, and orchid picks it up: SSHes into a free VM, clones the work repo, launches a clawpatrol-wrapped `claude` session in tmux, and watches the resulting PR. When new reviews / comments / CI events appear, orchid pokes the pane with a one-line summary so claude can iterate. When the PR is merged or closed (or the inbox issue is closed), orchid kills the session and frees the VM.

Single Go file. HCL config. No webhooks. Polls GitHub via `gh`, drives VMs via `ssh` + `tmux`.

---

## Quick start

```sh
go build -o orch ./orch.go
./orch -config swarm.hcl
```

Daemonize however you like. Recommended: a tmux session with a restart loop:

```sh
mkdir -p logs
tmux new -d -s orchd 'while :; do ./orch -config swarm.hcl; sleep 3; done 2>&1 | tee -a logs/orch.log'
tail -f logs/orch.log     # live log
tmux a -t orchd           # attach (ctrl+b d to detach)
tmux kill-session -t orchd # stop
```

State persists in `state.json` (path from config). Restart-safe: orchid resumes existing jobs from disk on startup.

---

## Prerequisites

### On the orch host (where you run `./orch`)

| Tool | Why |
|---|---|
| `gh`, authenticated | List issues, view PRs, fetch PR events |
| `ssh` | Reach VMs |
| `tmux` (optional) | Run the daemon in a session |

GitHub auth: `gh auth login` or `GH_TOKEN` env var. The token needs `repo` scope and access to both the inbox repo and every target work repo.

### On each VM (in the swarm)

| Tool | Where it must be on PATH |
|---|---|
| `tmux`, `git`, `jq` | default non-interactive shell PATH |
| `clawpatrol` | login-shell PATH (e.g. `~/.local/bin`) |
| `claude` | login-shell PATH |

orchid's spawn script wraps the pane command in `bash -lc` so login-shell PATH is in scope. Outbound github auth is provisioned automatically at startup: orchid copies the same SSH key it uses to reach the VM into `~/.ssh/id_ed25519` on the VM and primes `~/.ssh/known_hosts` for `github.com`. The same key must be authorized for the bot github account.

GitHub API auth on the VM (for `gh` invoked by claude during PR creation / iteration) is handled by clawpatrol via secret injection — claude always runs as `clawpatrol run -- claude --dangerously-skip-permissions`.

---

## How a job flows

```
You open issue in inbox repo, add a target label
                  │
                  ▼  (orchid tick, every poll_interval)
  orchid picks free VM ─────► bootstrapVM (idempotent ssh key push)
                  │
                  ▼  (one ssh call: bash -s)
  per-issue workdir created → git clone target repo → git config (bot identity)
                            → branch checked out from origin/main
                            → ~/.claude.json trust-stamped for the workdir
                            → tmux new-session: clawpatrol run -- claude
                  │
                  ▼  (wait for TUI idle, up to 60s)
  Bootstrap prompt pasted into the pane via tmux load-buffer / paste-buffer
                  │
                  ▼
  Claude implements, commits, pushes branch, opens PR on the target repo
                  │
                  ▼  (orchid tick)
  orchid finds the PR by branch name, starts watching it
                  │
                  ▼  (each tick)
  diff PR state vs `state.json`: new review IDs, new comment IDs,
  new CI conclusions, new headRefOid (push)
  if anything new AND pane idle → load-buffer + paste-buffer + Enter
                  │
                  ▼
  PR merged / PR closed / inbox issue closed → kill tmux session, drop state
```

---

## Config (`swarm.hcl`)

```hcl
github {
  inbox_repo = "denoland/orchid"   # where issues live
  token_env  = "GITHUB_TOKEN"
}

orchestrator {
  poll_interval = "30s"
  state_file    = "/home/exedev/orch/state.json"
  branch_prefix = "orch/issue-"
  workdir_root  = "/home/exedev/orch-work"   # per-VM root for clones
}

# One target per work repo. The label scopes which inbox issues route here.
target "deno" {
  label = "deno"
  repo  = "denoland/deno"
}

target "orchid" {
  label = "orchid"
  repo  = "denoland/orchid"
}

bootstrap_prompt = <<EOT
You are shepherding GitHub issue #{{issue.number}} from {{inbox.repo}}: "{{issue.title}}"

The work repo is {{target.repo}}. You are running inside a fresh clone at
{{workdir}}, with `origin` pointing at it (SSH auth configured).

--- issue body ---
{{issue.body}}
--- end issue body ---

Plan, implement, commit, push to branch `{{branch}}`, then open a PR against
{{target.repo}} that closes the issue. Then stop and wait — orchid will send a
follow-up message each time there is a new review, comment, CI status change,
or push to your PR.
EOT

vm "tiger-fusion" {
  host = "tiger-fusion.exe.xyz"
  user = "exedev"
  key  = "~/.ssh/id_ed25519"
}
```

Placeholders in `bootstrap_prompt` use `{{...}}` (not `${...}`) to avoid HCL interpolation.

---

## Opening a job (human workflow)

1. Open an issue in the inbox repo.
2. Add the label that matches the work repo you want claude to touch (e.g. `deno`, `orchid`).
3. Wait one `poll_interval` — orchid spawns a session on a free VM.
4. Watch the resulting PR (linked from the branch `orch/issue-N`) on the work repo. Review it like any human-authored PR.
5. To stop work mid-flight: close the PR **and** close the inbox issue (or remove the routing label).
6. To merge: just merge the PR. orchid tears down the session on the next tick.

---

## Inspecting & debugging

```sh
# orchid daemon
tail -f logs/orch.log
cat state.json | jq

# live claude pane on a VM (read-only attach is safer)
ssh tiger-fusion.exe.xyz tmux ls
ssh tiger-fusion.exe.xyz tmux a -t claude-3 -r

# work tree on the VM
ssh tiger-fusion.exe.xyz ls /home/exedev/orch-work/issue-3
```

When something goes wrong, the log line tells you which phase: `vm tiger-fusion: bootstrap FAILED`, `issue #N: spawn failed`, `issue #N: pr view failed`, `issue #N: pane busy, deferring poke`, `issue #N: torn down`.

---

## Limitations & known gaps

- **One job per VM** at a time. Free VMs are picked deterministically (alphabetical by name).
- **Closing the work-repo PR doesn't close the inbox issue** (cross-repo `Closes #N` doesn't auto-close). After PR close, orchid tears down — but if the inbox issue is still open with the routing label, the next tick will respawn. Close the inbox issue or remove the label to truly stop.
- **Per-line review thread comment bodies are not surfaced in the poke message.** `gh pr view --json reviewThreads` doesn't exist; only available via GraphQL. The review *event* is detected (so claude is woken up); claude then queries the threads itself. Adding a small `gh api graphql` call would close this gap.
- **Idle heuristic on the pane is a string match** for "bypass permissions" + absence of "esc to interrupt". Fragile across claude TUI versions.
- **No locking on state.json.** Single orch instance only.
- **`bootstrapVM` reuses the orch host's SSH key** as the VM's outbound github key, assuming both authorize the same bot account. If you need separate keys, add an explicit `github.ssh_key` field to the config.

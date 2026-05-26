# Targets

A **target** in orchid is a routing rule: when an issue in the inbox
repo carries a specific label, the matching target's repo is what
Claude actually works on. One swarm can drive any number of work repos
without each needing its own inbox.

## How routing works

1. Orch polls `github.inbox_repo` for open issues every
   `orchestrator.poll_interval`.
2. For each new issue, it scans the issue's labels.
3. The **first label** that matches a target's `label` field wins.
4. That target's `repo` is cloned, a `claude` session spawns in tmux,
   and the bootstrap prompt is pasted with `{{target.repo}}` filled
   in.
5. Claude opens its PR against that target repo. Reviews on the PR are
   relayed back into the running session.

Issues with no matching label sit idle. Multiple-matching labels
resolve to the first declared target — order matters.

## Declaring targets

```hcl
target "deno" {
  label = "deno"
  repo  = "denoland/deno"
}

target "clawpatrol" {
  label = "clawpatrol"
  repo  = "denoland/clawpatrol"
}

target "myproject" {
  label = "myproject"
  repo  = "your-org/your-repo"
}
```

The block name (`"deno"`, `"clawpatrol"`) is a human-readable id used
in logs + the dashboard. The `label` is what GitHub sees. They don't
have to match.

## Cross-repo close behaviour

When the PR merges (or closes), orch tears the session down and *also*
closes the original inbox issue, since GitHub's "Closes #N" syntax
doesn't auto-resolve across repos. If you close the work-repo PR
manually, orch leaves the inbox issue alone — that gives you a way to
say *"discard this one but keep the issue open for retry"*.

## Filing issues from the CLI

```bash
gh issue create --repo your-org/orchid-inbox \
  --label clawpatrol \
  --title "fix: panic on empty input" \
  --body  "Steps to reproduce: ..."
```

Orch sees it on the next poll tick (≤ `poll_interval`). The dashboard
also has a **Composer** that fans an issue out across multiple targets
in one shot — useful for "ship this feature on every fork".

## Tips

- **Use specific labels.** A generic `bug` label that exists on every
  target makes routing ambiguous. Prefer `<project>-bug`.
- **Bot-author the inbox.** Filing issues from the macOS Capture app
  (see [Capture](/docs/capture)) is the lowest-friction path — voice
  → draft → labeled inbox issue.
- **Inbox != work repo.** Keep the inbox repo private if you want;
  the work PRs land wherever `target.repo` points.

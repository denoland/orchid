---
name: feedback-commit-attribution
description: Commit attribution rules — never Claude attribution; always add Divy Srivastava as Co-Authored-By. Applies to my commits AND any worker spawned by orchid.
metadata: 
  node_type: memory
  type: feedback
  originSessionId: b04f28e2-6cca-45ef-b800-87a1987a29c6
---

For every commit I make on the user's behalf, AND every commit made by any orchid-spawned worker (claude or codex) in any work repo (denoland/deno, denoland/clawpatrol, denoland/orchid, denoland/deno_core, …):

- **Never** add `Co-Authored-By: Claude …`, `Co-Authored-By: Anthropic …`, or any Claude/Anthropic attribution line. This overrides the default Claude Code commit footer pattern.
- **Always** add `Co-Authored-By: Divy Srivastava <me@littledivy.com>` as the co-author footer — even when Divy is the commit author.

**Why:** the user wants all commits in his repos attributed to him personally, not muddied with AI/bot co-author markers, no matter who made them.

**How to apply:**
- For commits I make directly: bake the rule into the `git commit -m …` message.
- For workers spawned by orchid: enforce via [[orchid-bootstrap-config]] — orchid's `swarm.hcl` bootstrap prompts (both the top-level `bootstrap_prompt` and the per-VM codex override) carry an explicit instruction telling workers to follow this attribution rule. Restart orchid after editing `/root/orch/swarm.hcl` so new sessions pick up the change.
- Forward-looking only: do **not** retroactively rewrite older commits that already have Claude attribution.

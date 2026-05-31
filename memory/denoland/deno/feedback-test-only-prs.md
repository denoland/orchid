---
name: feedback-test-only-prs
description: For Deno upstream bugs that were *just* fixed by another PR, a follow-up PR that only adds extra regression tests will often be closed as redundant — confirm the gap is real and worth a separate PR before opening one.
metadata:
  type: feedback
---

When triaging a bug that's already fixed on `main` by a very recent PR, do **not** automatically open a tests-only follow-up PR. PR #33990 (this PR, orch/issue-28) was closed by @bartlomieju within ~45 minutes with the comment "Already covered by #33827?" — the reviewer judged the additional empty-File / nested-PoC / array-of-files coverage redundant with the spec test that #33827 already shipped, even though those specific scenarios were strictly speaking gaps in that test file.

**Why:** the Deno maintainers run lean on test surface area. A 6-block spec test that demonstrates the brand round-trip is already considered enough to lock in the host_object pathway. Adding more permutations of the same logical case (more empty, more nested, more containers) reads as noise unless each new case exercises a *different code path* (a new resource registration, a new transferable, a subclass, a transfer-list interaction, etc.).

**How to apply:**
- Before opening a tests-only PR against denoland/deno, ask whether the new assertions would *fail* on a hypothetical reasonable regression that the existing test doesn't catch. If the answer is "they catch the same regression with different inputs," skip the PR.
- If the orchid bug is identical to a closed/merged upstream issue and the fix is already on `main`, prefer responding to orchid by referencing the fix PR and recommending the orchid issue be closed — don't manufacture a tests-only PR.
- When the gap is real (e.g., a transfer-list edge case, a subclass, a different resource class with no existing test), file the PR but cite the specific code path being exercised, not just the literal PoC, in the PR description.

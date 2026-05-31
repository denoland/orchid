---
name: pr-creation-auth
description: gh CLI fails with GH_TOKEN=ghp_clawpatrol_placeholder_do_not_use so `gh pr create` returns 401 from this session; SSH git push still works.
metadata:
  type: feedback
---

In this session, `GH_TOKEN` / `GITHUB_TOKEN` are set to `ghp_clawpatrol_placeholder_do_not_use`. The clawpatrol MITM proxy is supposed to substitute the real `github-pat` credential transparently (its example gateway HCL configures exactly that), but in this session it didn't — `gh auth status`, `gh api user`, and direct `curl https://api.github.com/...` all return `Bad credentials (HTTP 401)`. The placeholder string is sent verbatim and GitHub rejects it.

**Why:** the persistent wg interface (`clawpatrol` device) is not up — `clawpatrol status` shows `✗ wg-quick interface up`. Only the ephemeral `wg0` from `CLAWPATROL_EPHEMERAL_ADDR` is up, and that path doesn't carry the github-pat credential profile.

**How to apply:** `git push origin <branch>` over SSH works fine (the SSH key is loaded). Use it to publish the branch. `gh pr create` will fail in this session — don't waste time trying `unset GH_TOKEN`, `gh auth login`, `gh api`, `curl ... --cacert $CURL_CA_BUNDLE`, or running under `clawpatrol run`; all paths hit the same 401. Leave the PR creation to the orchestrator (it appears to detect the pushed `orch/issue-NN` branch and create the PR out-of-band — recent divybot PRs like #33987 show the orchestrator can do this even when the session itself can't).

If the orchestrator instructions explicitly say to run `gh pr create`, attempt it once for paper-trail, note the 401, and stop. Re-issuing the same command in a follow-up turn doesn't help unless the proxy state changes.

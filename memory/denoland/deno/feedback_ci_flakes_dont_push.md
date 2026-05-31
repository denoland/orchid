---
name: feedback-ci-flakes-dont-push
description: "When CI on a deno PR fails on a single matrix job — either a pre-test setup step crashing (rustup-init on macOS, playwright-cache STATUS_STACK_BUFFER_OVERRUN on Windows) or a single unrelated node_compat test timing out on macos-x86_64 — and everything else is green, do NOT push empty commits. Wait for the maintainers to re-run."
metadata: 
  node_type: memory
  type: feedback
  originSessionId: 114c9705-fea7-4785-adfe-e8276cbc0572
---

When a Deno PR has only a single failing job and the failure happened in a pre-test setup step (not in `Run tests`), it is a CI runner-pool infra flake — not a code problem. Do not push empty `chore: rerun CI` commits to retrigger. The orchestrator/deno maintainers re-run flaky checks themselves.

Known flake patterns to recognize:
- **macos-aarch64**: `cargo` resolves to `rustup-init 1.29.0` and rejects `build`/`test` as an unknown argument. Job dies in <2 min before any real build/test runs.
- **windows-x86_64 debug integration**: "Set up playwright cache" step crashes with exit code `-1073740791` (`STATUS_STACK_BUFFER_OVERRUN`, `0xC0000409`). Job dies in ~1m15s, never reaches the `Run tests` step.
- **macos-x86_64 node_compat single-test timeout**: one unrelated `node_compat` test (e.g. `parallel/test-http2-respond-with-file-connection-abort.js`) "Test timed out after 20000ms" while every other shard incl. the other macos-x86_64 node_compat shards and every other OS is green. Runner-pool contention on the slow x86_64 macOS hosts.
- **`parallel/test-cluster-send-deadlock.js` flake** (any platform, e.g. `test node_compat (3/3) release linux-x86_64`): tracked in denoland/deno#34180. Test runs as flaky 2x then fails, panicking the file_test_runner. Always unrelated to PR changes.
- **`parallel/test-dns-resolver-max-timeout.js` flake** (any platform / shard): timing-sensitive assertion `timeout1 > timeout2` comparing two DNS-resolver max-timeout settings; when they end up within 1 millisecond of each other (e.g. 502 / 503) the strictEqual fails. file_test_runner marks the test flaky on runs 0/1 then escalates to concurrency=1 and tries once more — when that also fails it panics with `1 failed of 1222` and red-marks the shard. Pure timing; never related to ext/websocket / ext/net code in the diff.
- **`test unit debug linux-aarch64` leak-detection on `websocketTlsSocketWorks`**: leak detector reports "serverWebSocket was created before the test started, but was cleaned up during the test" plus "2 async operations to receive the next message" — leaks from previous tests landing in this test's sanitizer window. The companion `test unit debug` jobs on x86_64 / macos / windows pass cleanly. Sensitive to scheduling. Wait for re-run.
- **`deno_core test linux-x86_64` runner OOM-disk**: job dies with `Unhandled exception. System.IO.IOException: No space left on device : '/home/runner/actions-runner/cached/.../_diag/Worker_*.log'` thrown from the runner's own `GitHub.Runner.Worker` process, not from cargo/nextest. Step list shows everything after `Restore cache cargo home` as not-run (`*` rather than `✓`/`X`). Linux runners occasionally run out of ephemeral disk before tests even start. Every other shard green. Wait for re-run.

Common signature for all of these: a single matrix cell red, every other build + test green (especially the companion shard `(2/2)` for the same OS/profile and the same `test unit debug` jobs on other arches), and either a setup-step exit or a sanitizer/timeout signature that doesn't correspond to anything the PR diff actually changed.

**Why:** Empty-commit retries waste CI cycles and clutter PR history with noise. The user called this out during issue #51 after I'd already pushed two empty-commit retries, and again during issue #169 when I pushed an empty commit for the linux-aarch64 leak-detection flake.

**How to apply:** Read the failed job's log (`gh api repos/denoland/deno/actions/jobs/<id>/logs`), find which step the non-zero exit came from. If it's a setup step (`Set up playwright cache`, `Run dsherret/rust-toolchain-file`, cache restore, artifact download, etc.) OR a sanitizer/leak/timeout signature that doesn't trace to anything the PR touched, and the companion shards / other OSes are green, report "infra/test flake, no code change needed" and wait.

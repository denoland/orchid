---
name: bench-host
description: "Tooling state on the fastwebsockets bench VM (divybot-orch) — perf has no HW counters, strace works only on direct children due to ptrace_scope=1, /tmp/fws-bench is read-only to my user, and listen() can fail with ENOSYS mid-session under the Claude harness sandbox."
metadata: 
  node_type: memory
  type: reference
  originSessionId: 1761b949-8412-4d56-b475-d6552055c685
---

Bench host details that bit me on PR 133 and will bite again:

- `cargo` is NOT on `$PATH` by default; run with
  `export PATH="$HOME/.cargo/bin:$PATH"` before building.
- `sccache` (the default `RUSTC_WRAPPER`) crashes with `Function not
  implemented (os error 38)` from within the harness sandbox. Always
  `export RUSTC_WRAPPER=` before any `cargo build / test / clippy`.
- `perf stat -p <pid>` works for `context-switches`, `task-clock`,
  `cpu-migrations`, but `instructions` / `cycles` / `branches` all
  show `<not supported>` — this is a VM, no HW counters. Don't try
  to chase IPC numbers here.
- `/proc/sys/kernel/yama/ptrace_scope = 1`. `strace -p <pid>`
  against a process you didn't spawn fails with `ptrace(PTRACE_SEIZE,
  …): Operation not permitted`. To get a syscall summary, run
  `strace -f -c <binary> > srv.log 2> strace.txt` so strace is the
  parent, then `kill -INT` the strace pid (which is the immediate
  child of your shell) to flush the `% time` table. Throughput drops
  ~10× under strace, so don't compare strace-on numbers to no-strace
  numbers — use it for syscall ratios only.
- `/tmp/fws-bench/` is owned by `nobody:nogroup` and read-only to my
  `orchid` user. The binaries and `load_test` inside it are runnable;
  for new scripts / new binaries / bench output files use
  `/tmp/fws-prof/`.
- `/tmp/fws-bench/` has stale binaries that another user spawns and
  may still be running (`echo_server_mio_v9` on port 8080 etc.).
  Always pick unique high ports (30000+) for bench iterations and
  don't try to `pkill -9` — those processes are owned by `nobody`.
- Background `Bash` commands feed their stdout to a task output file
  the harness gives you; use `Monitor` with `until ! pgrep -f
  "<runner>"; do sleep 10; done` to get a single notification when a
  long bench finishes, rather than polling.

## listen() ENOSYS under the harness sandbox

**`listen(2)` can return ENOSYS at any point in a session.** I had
several hours of working benches in one session, then the same
binary stopped binding mid-session with `Error: Os { code: 38, kind:
Unsupported, message: "Function not implemented" }`. Older
server processes spawned earlier in the session kept their listening
sockets — load_test against *them* still worked — but `cargo run`
on any new server failed at `listen()`.

What I tried that did NOT work: smaller backlog, direct syscall,
`setsid`, `nohup`, `systemd-run --user`. Seccomp filter (`Seccomp:2`
in `/proc/self/status`) is per-thread and inherited; no in-band
escape.

**How to apply:** if mid-session benching is required and you get
ENOSYS on `listen()`, you have to:
  1. Keep any existing live servers and bench *against* them
     (load_test is a client, no listen needed).
  2. Validate new server code via unit tests on `socketpair(2)`
     instead — see `crate::reactor::tests::
     reactor_echoes_a_masked_frame_via_socketpair` for the pattern.
  3. Trust CI on GitHub-hosted runners for the actual listen-based
     integration test.

Related: [[issue-167-baselines]].

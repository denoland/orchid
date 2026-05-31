---
name: pr-133-reactor
description: "Public `crate::reactor::Reactor` API added in PR 133 commit b8ad742 for the many-connection / high-payload echo path. Use it when the tokio per-conn-task model bottoms out."
metadata: 
  node_type: memory
  type: project
  originSessionId: 1761b949-8412-4d56-b475-d6552055c685
---

PR 133 commit `b8ad742` added `crate::reactor::Reactor` (Linux,
behind the `reactor` feature). It is the steady-state event loop of
`examples/echo_server_mio.rs` lifted into a public library API.

**Why:** the per-conn tokio adapter (`echo_server_tokio_fast.rs`)
hits a structural ceiling at 200+ active connections on payloads
≥ 16 KiB. Profiler at 500/16384 over 5 s under load:
  - tokio_fast v4: 76 ctx-switches, 60.5 frames per epoll_wait,
    52 k msg/s
  - mio_v11:    11 730 ctx-switches, 491 frames per epoll_wait,
    64 k msg/s
Same per-frame syscall mix; the gap is per-task scheduling and
batching. AsyncFd / try_read / try_write don't close it. The fix is
"one task drives many fds," which the reactor exposes.

**How to apply:**
- For per-connection-task workloads (typical Deno op shape, up to
  ~200 conns): use `echo_server_tokio_fast.rs` as a recipe.
  `ServerEngine` in a tokio task, `read().await` + `try_write`. Beats
  uWS on 3/5 cases.
- For many-conn fan-out (500+ conns, big payloads): use
  `crate::reactor::Reactor` directly. `r.bind(addr)?; r.run_echo()?`
  for the canonical shape, or
  `r.add_session(mio::net::TcpStream)?` to embed behind your own
  HTTP layer. Matches `mio_v11`'s 1.05–1.08× uWS on all 5 cases
  because it is `mio_v11`'s loop.

Both paths share `crate::sync_server::ServerEngine` so the
per-frame parse / unmask / response-synthesis hot path is the
same. The reactor is Linux-only; non-Linux builds get a stub
example so `cargo build --all-targets` still works.

Related: [[issue-167-baselines]], [[tokio-fast-path-lessons]].

---
name: tokio-fast-path-lessons
description: "What worked and what didn't when chasing per-frame syscall and future overhead in fastwebsockets PR 133's Tokio echo adapter, and where the structural ceiling is."
metadata: 
  node_type: memory
  type: feedback
  originSessionId: 1761b949-8412-4d56-b475-d6552055c685
---

When tuning a Tokio-on-loopback adapter against `ServerEngine`:

- **Use `read().await` for the read side, not `readable().await + try_read`.**
  `tokio::net::TcpStream::read` correctly clears the runtime's internal
  readiness flag on WouldBlock; `try_read` alone doesn't, and the
  natural fallback `stream.readable().await` allocates a future per
  WouldBlock. `strace` showed v3 (try_read style) allocating ~480
  readable() futures/s at 100 conns and ~1080/s at 200 conns — those
  futures were the source of the v3 regression (5–8% slower than v2
  on every bench case).
  **Why:** verified by `strace -c` against the three variants over a
  5 s window. WouldBlock is the dominant cost on loopback because the
  client gates send-then-wait-for-echo and the server frequently
  catches an empty queue.
  **How to apply:** keep the await on read; optimize the write side.

- **Use `try_write` for the steady-state single-segment Echo, not
  `write_vectored().await`.** `writev` is 13–17% slower per syscall
  than `send` on loopback under strace (15 vs 13 µs at 100/20; 16 vs
  15 µs at 200/16384), AND `AsyncWrite::write_vectored` builds a
  future per call. `try_write` is a direct `send()` syscall when the
  kernel has room (the steady-state case on loopback).
  **Why:** `ServerEngine::process_into` produces one in-place segment
  for every typical echo (masked input + payload < 64 KiB), so the
  multi-segment path is the exception.
  **How to apply:** branch on `segs.len() == 1` and call `try_write`
  in a loop; fall back to `try_write_vectored` for multi-segment;
  only `writable().await` when WouldBlock returns.

- **Don't ship a scratch-buffer size change without ≥3-run head-to-
  head benches.** I burned an hour on a 17/24/32/64 KiB sweep where
  two-run benches suggested 24 KiB was 6–7% faster on 200/16384.
  A three-run repeat put 24 KiB and 64 KiB inside ±2% — pure noise.
  **Why:** the bench has ≈1–2k msg/s std-dev on 500/16384 and ≈3–5k
  on 200/16384. Anything inside that band needs more samples.
  **How to apply:** don't merge a knob until the head-to-head margin
  is >2× the std-dev across 3+ runs.

- **At 200+ active connections, the tokio adapter cannot match mio
  inside the per-conn-task model.** `perf stat -p $pid` over 5s on
  500/16384:
  - tokio_fast v4: 76 ctx-switches (the process almost never blocks
    because there's always *some* runnable task) at 99.7% CPU.
  - mio_v11: 11 730 ctx-switches at 96.8% CPU.
  `strace -c -f`:
  - tokio_fast v4: 60.5 frames per epoll_wait
  - mio_v11: 491 frames per epoll_wait (~8× the batching)
  Same per-frame syscall mix; the gap is per-task user-space
  scheduling overhead and the structural batching loss from one fd
  per task. AsyncFd / try_read / try_write tricks just relocate the
  cost.
  **Why:** verified at 200/16384 and 500/16384.
  **How to apply:** if you need parity vs uWS at high fd counts,
  route users at the `crate::reactor::Reactor` API (added in PR 133
  commit `b8ad742`). It's the steady-state loop of
  `examples/echo_server_mio.rs` lifted into the library: one task
  drives many fds, no per-frame Future, beats uWS on all 5 bench
  cases.

Related: [[issue-167-baselines]], [[bench-host]].

---
name: issue-167-baselines
description: Saved binaries and msg/s baselines on the bench host for fastwebsockets PR 133 (single-core echo vs uWS). Use these to compare new variants without re-running uWS.
metadata: 
  node_type: memory
  type: project
  originSessionId: 1761b949-8412-4d56-b475-d6552055c685
---

PR 133 ("fastwebsockets: beat uWebSockets echo-server throughput") tracks
single-core echo throughput vs uWebSockets across five `load_test` cases:
`100/20`, `10/1024`, `10/16384`, `200/16384`, `500/16384` (connections /
payload bytes).

**Why:** the bench is noisy across runs (≈1–2k msg/s on the 500/16384
case, ≈5k on 200/16384). Always re-bench head-to-head with the same
runner rather than comparing a new run against a saved number.

**How to apply:** when you need a baseline number, prefer the saved
binary on the bench host over re-benching uWS. `/tmp/fws-bench/` is
read-only to my user but the binaries inside it are runnable:

- uWS: `/tmp/fws-bench/EchoServer` (run with `UWS_PORT=…`)
- uWS averages (`/tmp/fws-bench/uws_x5.txt`): 120 224 / 116 169 / 77 800
  / 73 166 / 60 042 msg/s.
- mio_v11 (`echo_server_mio` head): 114 187 / 120 751 / 81 595 / 77 217
  / 64 847 (`mio_v11_x3.txt`). Beats uWS on all 5.
- tokio_fast v2 (writev path, pre-commit `5978bbf`): 103 017 / 114 045
  / 78 211 / 59 711 / 48 836 (`tokio_fast_v2_x3.txt`). Beats uWS only
  on 10/16384.
- tokio_fast v3 (try_read+try_write+readable().await — REGRESSED):
  97 382 / 103 960 / 74 146 / 57 653 / 52 422 (`tokio_fast_v3_x3.txt`).
- Bench client + script: `/tmp/fws-bench/load_test`, `bench_v2.sh`,
  `bench_v3.sh`. I keep my own runner in `/tmp/fws-prof/bench.sh`
  because the `/tmp/fws-bench` dir is not writable to me.

Bench host: `divybot-orch` VM, Intel Xeon Cascadelake, 6 cores. Run
single-threaded current_thread runtime; the user explicitly wants
single-core wins.

Related: [[tokio-fast-path-lessons]], [[bench-host]].

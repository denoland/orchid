---
name: project-startup-perf-issue-240
description: Findings from the Deno startup-perf investigation (orchid
metadata: 
  node_type: memory
  type: project
  originSessionId: 2e3f12d4-0a8b-42b3-9dba-4b2531f7e7c1
---

Working on `denoland/orchid#240` "Make `deno run` startup competitive with Bun"
via PR `denoland/deno#34400` on branch `orch/divybot-240`.

**Why:** Cold `deno eval 'console.log("hi")'` is 60–70 ms on a modern host; bun
runs the same in ~5 ms; node is ~45 ms. The dominant cost is V8 snapshot
deserialization, which scales with snapshot heap size.

**How to apply:** Future work in this area should know what's *already*
tried, what the floor is, and where the remaining wins likely live.

### What the V8 deser breakdown actually looks like

`DENO_V8_FLAGS=--profile-deserialization deno eval 'console.log("hi")'`
on this host (debug build, post my lazifications):

```
read-only space (~814 KB):  ~1.5 ms
isolate     (~5.85 MB):    ~14–22 ms  (high variance ±5 ms)
context #1  (~1.28 MB):    ~10–15 ms  (high variance ±3 ms)
```

V8 reports a `context #N` per context in the snapshot. Removing the
`deno_node` vm contextify template (`with_runtime_cb: None` in
`runtime/snapshot.rs`) dropped the count from 2 contexts to 1.

`startup_bench` harness (`cli/examples/startup_bench.rs`) wall-clock noise
is ~±2 ms in debug; release-lite is tighter (~±0.5 ms).

### Wins that landed

| Commit | What | Snapshot Δ |
|---|---|---|
| `25a04b06` | telemetry/cron/surface/webgpu/webstorage/prompt: lazy_loaded | V8 -144 KB |
| `27dfac15` | cron internals defineLazyInternal, webstorage descriptor set() | (CI fix) |
| `d2dcae0d` | drop vm contextify template from snapshot, fresh-context fallback in `ext/node/ops/vm::create_v8_context` | V8 -62 KB |
| `488744de` | `98_global_scope_worker.js` → lazy_loaded_js | V8 -9 KB |
| `1f044c11` | `finalDenoNs` lazy via `getFinalDenoNs()` | context -6.5 KB |

End-to-end: `startup_bench` mean ~42 ms → ~38 ms in debug. Release-lite
similar (~28 ms → ~26 ms). All gains are real and compound.

### Things that *don't* help

- **`skip_op_registration: true` in snapshot config**: routes through
  `upgrade_snapshotted_ops_with_fast_calls` instead of
  `initialize_deno_core_ops_bindings`. Context deser went *up* ~5 ms.
  Don't flip without understanding how V8 14.9+ bakes ops in the snapshot.

- **Defer warmup-only IC trace calls** in `99_main.js` (the two no-op
  `bootstrapMainRuntime(undefined, true)` + the `Event("warmup")`
  dispatch). Saves a handful of bytes; loses IC trace; net wall-clock
  zero. Not worth the risk.

- **Lazifying leaf globals (URLPattern, Headers, FileReader, etc.) in
  `98_global_scope_shared.js`**: each saves few KB; the code gets
  fragile; some (Headers) are on the fetch hot path anyway.

### Architectural plays I *didn't* execute (still on the table)

These are the ones that could plausibly move the V8 isolate deser
number meaningfully (currently 14–22 ms). Each is multi-file, risky,
not a 30-minute change.

1. **Convert `99_main.js`/`90_deno_ns.js`/`98_global_scope_{shared,window}.js`
   to `lazy_loaded_js` IIFEs**, have Rust load `99_main.js` via
   `core.loadExtScript()` *before* the `globalThis.bootstrap.mainRuntime`
   lookup in `runtime/worker.rs`. Pushes the giant module-body work out
   of the snapshot heap. Risk: V8 parse+compile of the runtime source
   at startup may eat the savings (V8 lazy-parse caps how much), and
   the four files have a tangled static-import graph that all has to
   move together.

2. **Lazy `process.stdout`/`stderr`/`stdin` accessors that call
   `__bootstrapNodeProcess` on first read**. Lets `node:stream` /
   `node:_stream_*` / `node:tty` / `node:net` move from `esm` to
   `lazy_loaded_esm` in `ext/node/lib.rs`. For programs that don't
   actually touch process stdio (`console.log` via `core.print()`
   doesn't), this lifts a huge slice of node polyfill out of the
   snapshot heap. Risk: npm libs that read `process.stdout` at module
   body — common — pay the lazy-load cost on first call, and the
   bootstrap accessor pattern has to be done carefully so existing
   `process.stdout = newStream` assignments still work.

3. **Split the snapshot**: emit a *minimal* `CLI_SNAPSHOT_LITE.bin`
   for `deno eval`/`--unconfigured` paths (just primordials, `core`,
   a tiny console shim) alongside the current full snapshot. Pick at
   process startup. Genuinely big plumbing change in
   `cli/snapshot/build.rs` + the CLI startup path. Hard to make work
   with `--standalone` self-contained binaries.

### Useful tools left behind

- `cli/examples/startup_bench.rs` — `MainWorker` boot loop.
- `DENO_STARTUP_PHASES=1` — env-gated `eprintln!`s in
  `JsRuntime::new_inner` and `MainWorker::bootstrap` for per-phase
  timing. Cost is zero when unset.
- `DENO_SNAPSHOT_IMPORT_GRAPH=<path>` — dump JSONL of every ESM/lazy
  load during snapshot creation. Pre-existing.
- `DENO_V8_FLAGS=--profile-deserialization` — V8's own deser timer.
- `DENO_V8_FLAGS=--trace-deserialization` — every NewObject /
  Backref / ReadOnlyHeapRef during deser. ~430k lines per run.

### Workflow gotchas

- The build script v8 download fails on this VM (`librusty_v8.a`
  can't be fetched); reuse the one from a sibling worktree via
  `export RUSTY_V8_ARCHIVE=/home/orchid/orch-work/issue-219/target/debug/gn_out/obj/librusty_v8.a`.
- `sccache` is broken (`Function not implemented (os error 38)`) on
  this VM. Always set `export RUSTC_WRAPPER=` to bypass.
- `tcp::TcpSocket::listen` fails with ENOSYS on this VM (seccomp);
  some `deno_core` op_driver tests will fail locally for that reason
  alone, *not* from any code change.
- The orchestrator auto-opens a PR when you push the branch and
  spams the chat with every CI check change. Per Divy's direction:
  ignore those notifications, just keep iterating and commit
  often.

---
name: project-node-compat-triage
description: "When triaging ignored node_compat tests in denoland/deno, do not propose enabling tests whose `reason` cites a Node-internal C++ binding (`internalBinding('foo')`) — Deno intentionally does not implement those. The JSStream binding was tried (PR"
metadata: 
  node_type: memory
  type: project
  originSessionId: 7fc09524-238a-456e-97d5-c092008413bc
---

When proposing node-compat test enablement work for `denoland/deno` (via the orchid inbox), **skip tests whose `reason` cites a Node-internal C++ binding** (`internalBinding('zlib')`, `internalBinding('js_stream')`, `internalBinding('http2')`, `internalBinding('cares_wrap')`, etc.). Deno's position is that these internal C++ bindings will not be polyfilled — the tests should remain `ignore: true` indefinitely with that reason.

**Why:** The JSStream binding was added in PR #34199 (orchid issue #128, merged 2026-05-17) and subsequently reverted. The user confirmed 2026-05-18: "bindings don't exist anymore we decided to not do it and just ignore the tests." Same pattern earlier with `NODE_TLS_REJECT_UNAUTHORIZED` env var ([[project-node-compat-triage]] — Deno deliberately uses `--unsafely-ignore-certificate-errors` instead).

**How to apply:** When scanning `tests/node_compat/config.jsonc` for enablement candidates, filter out any reason mentioning `internalBinding(`. Stick to tests whose blocker is a real Deno-side gap (missing API surface, missing crypto algorithm, missing env-var handling, polyfill that's a stub, etc.) — those are tractable. Also avoid: Node-only CLI flags (`--experimental-permission`, `--abort-on-uncaught`, `node inspect`, `--build-snapshot`), V8 natives syntax (`%OptimizeFunctionOnNextCall`), and `--expose-internals` whitebox tests.

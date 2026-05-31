---
name: denoland-v8-patch-convention
description: How denoland/v8 stores floated V8 patches and how they get applied by the autoroll
metadata: 
  node_type: memory
  type: project
  originSessionId: 52cd0bfd-7196-4c01-9aca-d44fa0b74c5d
---

denoland/v8 is a meta-repo whose only artifact is a set of `patches/*.patch`
files that get floated on top of upstream chromium `v8.git` by `autoroll.ts`.

Why: README claims patches "are not functional changes to V8 code" — they're
build-system accommodations for rusty_v8. That is the stated norm, but
functional patches do happen when the upstream V8 release branch needs
something for downstream consumers; just expect maintainer pushback in
review and make the case explicitly.

How to apply:
- Patches live in `patches/`, named `NNNN-<short-subject>.patch`, in
  `git format-patch -1` output format. Names sort lexically; the autoroll
  applies them in `git am -3` order.
- Author/committer must be the contributor (e.g. Divy
  <me@littledivy.com>); don't let `git format-patch` infer the orchid
  service account.
- The cloned upstream V8 lives in `./v8/` which is `.gitignore`'d. Do
  edits there, `git format-patch -1`, copy the resulting file into
  `patches/`.

How to apply: the autoroll script does
`git checkout -b $V_VER-lkgr-denoland origin/$V_VER-lkgr` then iterates
`patches/` alphabetically and runs `git am -3 ../patches/<file>`. So any
new patch must apply cleanly against the current `lkgr` of the V8
versions listed at the top of `autoroll.ts` (currently `14.9`).

Branch naming for autoroll output: `${V8_VERSION}-lkgr-denoland`, and a
tag is pushed as `${V8_VERSION}-denoland-${COMMIT20}`. PRs against this
repo target the `autoroll` branch, not main.

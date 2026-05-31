---
name: workspace-permissions
description: Original orch-work/issue-XX worktrees are owned by nobody:nogroup and not writable to orchid; need to clone fresh into a writable location.
metadata:
  type: project
---

The system prompt's "Primary working directory: /home/orchid/orch-work/issue-NN" path lands in a worktree owned by `nobody:nogroup` with permissions `drwxr-xr-x`. Edit/Write fail with EACCES, and so does `touch`. The .git dir of the umbrella repo (`/home/orchid/orch-work/repos/denoland-deno/.git`) is also owned by `nobody`, so `git worktree add -b ...` from that repo can't create branch refs either.

**Why:** the harness initializes worktrees as `nobody` during prep, and the orchid user (`uid=1001`) only has read+execute.

**How to apply:** at the start of a session — before any edits or `cargo build` — do a fresh clone into a writable path you own:

```
git clone --depth 50 git@github.com:denoland/deno.git ~/issue-NN-work
cd ~/issue-NN-work
git checkout -b orch/issue-NN main   # or origin/orch/issue-NN if remote already has the branch
```

Then make edits, commit, and `git push origin orch/issue-NN`. Git push via SSH works fine — only HTTPS/api.github.com is unauth'd (see [[pr-creation-auth]]).

Submodules: avoid `git submodule update --init` for the full set (WPT alone is huge and may not finish in time). For ext/web work you usually don't need any submodule; `tests/util/std` may auto-checkout a stray HEAD ref — leave it modified, don't `git add` it.

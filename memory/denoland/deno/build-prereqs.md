---
name: build-prereqs
description: Reproducing a working `cargo build --bin deno` in a fresh session: reuse a sibling session's rust toolchain, set BINDGEN_EXTRA_CLANG_ARGS for libsqlite3-sys, and put the target dir outside the source tree if it isn't writable.
metadata:
  type: project
---

The harness ships without a usable Rust toolchain by default. `rustup toolchain install 1.95.0` repeatedly fails with `os error 2: No such file or directory` because the harness wipes `~/.rustup/downloads` between commands.

**Why:** the sandbox treats `~/.rustup` as ephemeral, so each fresh download is gone before rustup can rename it into place.

**How to apply:** point RUSTUP_HOME/CARGO_HOME at another orch session's pre-installed toolchain (issue-26-rust and issue-35-rust both have working `1.95.0-x86_64-unknown-linux-gnu` toolchains). Set:

```
export RUSTUP_HOME=/home/orchid/orch-work/issue-26-rust/rustup
export CARGO_HOME=/home/orchid/orch-work/issue-26-rust/cargo
export PATH="$CARGO_HOME/bin:$PATH"
export BINDGEN_EXTRA_CLANG_ARGS="-I/usr/lib/gcc/x86_64-linux-gnu/13/include"
```

The `BINDGEN_EXTRA_CLANG_ARGS` line is required: libsqlite3-sys's build script calls bindgen on `sqlite3.h`, which transitively includes `stdarg.h`. The host has no `clang` resource directory but does have gcc-13's header at that path, and pointing bindgen there fixes the bindgen error you'd otherwise see (`'stdarg.h' file not found`).

If the source tree itself isn't writable (see [[workspace-permissions]]), also set `CARGO_TARGET_DIR=/home/orchid/issue-NN-target` so cargo can write its `target/` somewhere it owns.

Expect a clean debug build to take roughly 25-40 minutes on this machine when other sessions are contending for the shared cargo registry lock; if multiple cargo processes are sleeping with no rustc workers, they're deadlocked on `.package-cache` — kill the others, you'll regain progress.

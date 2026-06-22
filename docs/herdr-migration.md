# herdr migration — tmux → herdr

orchid uses tmux as a remotely-driven PTY host: the central daemon SSHes into
each VM and runs `tmux <subcommand>` bash strings to spawn panes, paste issue
bodies, capture output, stream to the dashboard, and tear down.

This doc covers the tmux→herdr swap via a **tmux-CLI shim** (`/usr/local/bin/tmux`)
that translates orchid's existing tmux calls into herdr CLI/socket calls. The
shim lets orchid's Go code run unchanged for 13 of the 18 tmux subcommands.
The remaining gaps need small orchid Go patches — documented below with exact
file:line references.

## What the shim handles (no orchid changes needed)

| tmux subcommand | shim behaviour |
|---|---|
| `start-server` | no-op (herdr server runs as systemd unit) |
| `setenv -g K V` | stored in `/tmp/herdr-global-env`, injected via `--env` at `workspace create` |
| `new-session -d -c -s CMD` | `herdr workspace create --cwd --label --no-focus` + `herdr pane run <root> CMD` |
| `kill-session -t` | `herdr workspace close <id>` (resolved by label) |
| `has-session -t` | `herdr workspace list` filtered by label; exit 0/1 |
| `ls` | `herdr workspace list` formatted as tmux-like output |
| `capture-pane -p -t` | `herdr pane read <pane> --source recent` |
| `send-keys -t KEY` | `herdr pane send-keys <pane> <mapped-key>` (C-m→enter, Escape→esc) |
| `load-buffer -b NAME -` | stdin → `/tmp/herdr-buf-NAME` |
| `paste-buffer -b -t -d` | `herdr pane send-text <pane> <contents>` + rm buffer file |
| `delete-buffer -b` | `rm /tmp/herdr-buf-NAME` |
| `rename-session -t OLD NEW` | `herdr workspace rename <id> NEW` |
| `list-panes -t -F #{pane_pid}` | `herdr pane process-info <pane>` → pids one per line |
| `attach-session -t` | `herdr workspace focus <id>` then `exec herdr` |

Label resolution: orchid names sessions `<agent>-<issue>` (e.g. `claude-184`).
herdr workspace IDs are auto-assigned (`w1`, `w2`, ...). The shim resolves by
matching the workspace **label** against orchid's session name via
`herdr workspace list | jq`.

## What needed orchid Go patches (now applied)

### 1. pipe-pane streaming (patched)

**File:** `internal/orch/relay_agent.go:355-498`

`runCapture` previously streamed pane output via `tmux pipe-pane -t "$S" "cat >
$F"` into a FIFO for event-driven delta streaming. herdr has no pipe-pane
equivalent, so the shim no-ops pipe-pane calls — which would leave the FIFO
without a writer and hang the relay capture.

**Fix (applied):** replaced the pipe-pane + FIFO script with a 5Hz polling
loop that emits `tmux capture-pane -p -e` + NUL sentinel repeatedly. Each
snapshot is sent as a full repaint frame ('F'). The Go reading logic was
simplified from "initial full frame + delta loop" to a unified frame-reading
loop. The resync goroutine is preserved for sub-200ms late-viewer repaints.
The FIFO cleanup in the defer was removed (no FIFO). The frontend already
handled 'F' frames with clear-and-repaint (`Pane.tsx:134-141`), so no frontend
changes were needed. Trade-off: loses delta efficiency (5Hz full snapshots
instead of event-driven deltas), but matches the SSE fallback's approach.

### 2. absolute resize-window (partial — no-op in shim)

**Files:**
- `internal/orch/http_api.go:680` (`/api/pane/resize`)
- `internal/orch/http_api.go:716` (`/api/pane/stream` initial resize)
- `internal/orch/relay_agent.go:377` (`runCapture` initial resize)

orchid sets absolute pane dimensions with `tmux resize-window -t <s> -x <cols>
-y <rows>`. herdr's `pane resize` is directional + ratio (`--direction
left|right|up|down --amount FLOAT`), not absolute cols/rows. The shim no-ops
resize calls.

**Impact:** the dashboard pane viewer doesn't resize to match the browser
viewport. Panes keep their natural size. Functional but not ideal.

**Fix (optional, future):** compute a ratio from the current layout. Call
`herdr pane layout --pane <id>` (returns `area` with cols/rows), then:
```
ratio_x = target_cols / current_cols
herdr pane resize --pane <id> --direction right --amount $ratio_x
```
This is approximate (herdr clamps to tab bounds). Alternatively, accept the
no-op for now — the pane viewer still works, just at a fixed size.

### 3. capture-pane -e with escapes (works via shim fallback)

**Files:**
- `internal/orch/http_api.go:722` (SSE stream loop)
- `internal/orch/relay_agent.go:409,478` (relay polling loop + resync)

orchid uses `tmux capture-pane -p -e` to include ANSI escape sequences (colors,
cursor) in pane snapshots. herdr's `pane read` only documents `--ansi` for
`--source visible` (current rendered screen), not for `--source recent`
(scrollback). The shim falls back to `herdr pane read --source visible --ansi`
when `-e` is requested.

**Impact:** the SSE and relay paths get the visible screen with escapes (not
full scrollback). Colors work for the current viewport. This is acceptable —
the SSE path already dropped scrollback per the comment at `http_api.go:719-
720`, and the relay polling fix (#1) uses the same `--source visible --ansi`
approach.

**Fix:** none needed. If full scrollback with escapes is needed later, request
herdr add `--ansi` support for the `recent` source.

## What the shim fully handles (documented for clarity)

### setenv -g (sccache)

**File:** `internal/orch/orch.go:694-702`

orchid sets `tmux setenv -g RUSTC_WRAPPER sccache` and `tmux setenv -g
SCCACHE_DIR <dir>` before spawning sessions, so every later `new-session`
inherits sccache. The shim stores these in `/tmp/herdr-global-env` and injects
them via `--env KEY=VALUE` flags at `herdr workspace create`. No orchid change
needed. The env file is per-host and persists across shim invocations (until
reboot); `tmuxStart` re-sets it every spawn.

### attach-session

**File:** `internal/orch/cli.go:848-856`

`orch run` does `syscall.Exec(tmux, ["tmux","attach-session","-t",reply.Tmux],
env)`. The shim resolves the label, focuses the workspace via `herdr workspace
focus`, then `exec herdr`. The user lands in the herdr TUI with the workspace
focused. `Ctrl+b q` detaches (same keybinding as tmux). No orchid change
needed. `exec.LookPath("tmux")` finds the shim at `/usr/local/bin/tmux`.

## Rolling cutover procedure (prod → herdr)

Per-VM, zero downtime:

1. **Drain** a VM: let its current issues finish, or move them to other VMs
   via the dashboard. Confirm `tmux ls` (real tmux) shows no active orchid
   sessions on that VM.

2. **Install herdr + shim** on that VM:
   ```bash
   ssh orchid@<vm> "bash <(curl -fsSL https://orchid.littledivy.com/install.sh)"
   ```
   The installer:
   - installs herdr to `/usr/local/bin/herdr`
   - preserves real tmux as `/usr/local/bin/tmux.real`
   - drops the shim at `/usr/local/bin/tmux`
   - installs `opencode`, `claude`, `codex` integrations
   - starts `herdr-server.service`

3. **Verify** the herdr server is up:
   ```bash
   ssh orchid@<vm> "sudo systemctl status herdr-server"
   ssh orchid@<vm> "herdr workspace list"
   ```

4. **Let the scheduler respawn** new jobs against herdr. orchid's next
   `tick.go` spawn cycle will SSH in, run `tmux new-session` (→ shim →
   `herdr workspace create`), and the job appears in the herdr sidebar with
   semantic agent state.

5. **Repeat** for each VM.

### state.db compatibility

The `Job.Tmux` field (`internal/orch/orch.go:199`) has JSON tag `json:"tmux"`.
The shim does not rename this field — orchid's `state.db` deserializes
unchanged. The field still holds the session label (e.g. `claude-184`), which
the shim resolves to a herdr workspace at runtime. No migration needed.

## Files changed in this repo

- `cfrelaytun/relay/public/herdr-tmux-shim.sh` — the shim (new)
- `cfrelaytun/relay/public/install.sh` — herdr install + shim + integrations + systemd unit
- `.claude/skills/setup-orchid/SKILL.md` — tmux → herdr references

## orchid Go files patched for herdr parity

| File | Line(s) | Change | Status |
|---|---|---|---|
| `internal/orch/relay_agent.go` | 355-498 | pipe-pane + FIFO → 5Hz polling loop | **patched** |
| `internal/orch/http_api.go` | 680, 716 | absolute resize | no-op in shim (acceptable) |
| `internal/orch/relay_agent.go` | 377 | absolute resize | no-op in shim (acceptable) |
| `internal/orch/http_api.go` | 722 | capture -e scrollback | works via shim `--source visible --ansi` fallback |

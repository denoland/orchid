#!/usr/bin/env bash
# herdr-tmux-shim — translates the tmux subcommands orchid emits into herdr
# CLI/socket calls. Installed as `tmux` on the orchid user's PATH so orchid's
# existing Go code (orch.go, tick.go, http_api.go, relay_agent.go, cli.go)
# runs unchanged against a herdr server instead of a tmux server.
#
# Session-name semantics: orchid names sessions "<agent>-<issue>" (e.g.
# claude-184). herdr workspace IDs are auto-assigned (w1, w2, ...) and labels
# are display-only. The shim resolves orchid's session name to a herdr
# workspace by matching the workspace label, then resolves the workspace's
# root pane for pane-level operations.
#
# Covered subcommands (13/18 clean, 5 best-effort — see docs/herdr-migration.md):
#   start-server              → no-op (herdr auto-starts via systemd)
#   setenv -g K V             → stored in env file, injected at new-session
#   new-session -d -c -s CMD  → workspace create + pane run
#   kill-session -t           → workspace close
#   has-session -t            → workspace list filter, exit 0/1
#   ls                        → workspace list
#   capture-pane -p [-e] -t   → pane read --source recent
#   send-keys -t KEY          → pane send-keys (C-m→enter, Escape→esc)
#   load-buffer -b NAME -     → stdin to /tmp/herdr-buf-NAME
#   paste-buffer -b -t -d     → pane send-text from buffer file
#   delete-buffer -b          → rm buffer file
#   rename-session -t OLD NEW → workspace rename
#   list-panes -t -F #{pid}   → pane process-info, pids one per line
#   pipe-pane -t              → no-op (stop) / best-effort (start)
#   resize-window -t -x -y    → best-effort (herdr is ratio-based)
#   attach-session -t         → exec herdr (focus workspace first)
#
# Requires: herdr on PATH, jq on PATH.

set -uo pipefail

HERDR="${HERDR_BIN:-herdr}"
BUF_DIR="/tmp"
ENV_FILE="/tmp/herdr-global-env"

die() { printf 'tmux-shim: %s\n' "$*" >&2; exit 1; }
command -v "$HERDR" >/dev/null || die "herdr not found on PATH"
command -v jq >/dev/null || die "jq not found on PATH"

# ─── helpers ──────────────────────────────────────────────────────────────

# resolve_label <label> → workspace_id (stdout), empty + exit 1 if not found
resolve_label() {
  local label="$1" wid
  wid=$("$HERDR" workspace list 2>/dev/null | jq -r \
    '(.result.workspaces // .workspaces // .panes // .) | if type=="array" then .[] else . end
     | select(.label == $lbl) | .workspace_id // empty' \
    --arg lbl "$label" 2>/dev/null | head -1)
  [ -n "$wid" ] && { echo "$wid"; return 0; }
  return 1
}

# resolve_pane <label> → pane_id of the workspace's root pane
resolve_pane() {
  local label="$1" wid pid
  wid=$(resolve_label "$label") || return 1
  pid=$("$HERDR" pane list --workspace "$wid" 2>/dev/null | jq -r \
    '(.result.panes // .panes // .) | if type=="array" then .[] else . end
     | .pane_id // empty' 2>/dev/null | head -1)
  [ -n "$pid" ] && { echo "$pid"; return 0; }
  return 1
}

# map a tmux key name to herdr key-combo syntax
map_key() {
  case "$1" in
    C-m|Enter|Return) echo enter ;;
    Escape)           echo esc ;;
    C-c)              echo ctrl+c ;;
    C-z)              echo ctrl+z ;;
    Tab)              echo tab ;;
    BSpace)           echo backspace ;;
    Up|Down|Left|Right) echo "$(echo "$1" | tr '[:upper:]' '[:lower:]')" ;;
    *)                echo "$1" ;;
  esac
}

# ─── subcommand dispatch ──────────────────────────────────────────────────

cmd="${1:-}"
[ $# -gt 0 ] && shift

case "$cmd" in

  # tmux start-server 2>/dev/null || true
  start-server)
    # herdr server is managed by systemd; no-op.
    exit 0
    ;;

  # tmux setenv -g RUSTC_WRAPPER sccache
  # tmux setenv -g SCCACHE_DIR <dir>
  setenv)
    global=0
    while [ $# -gt 0 ]; do
      case "$1" in
        -g) global=1; shift ;;
        *)  break ;;
      esac
    done
    [ $global -eq 1 ] || exit 0
    key="${1:-}"; val="${2:-}"
    [ -n "$key" ] || die "setenv: missing key"
    # append/replace in env file
    touch "$ENV_FILE"
    grep -v "^${key}=" "$ENV_FILE" 2>/dev/null > "$ENV_FILE.tmp" || true
    printf '%s=%s\n' "$key" "$val" >> "$ENV_FILE.tmp"
    mv "$ENV_FILE.tmp" "$ENV_FILE"
    exit 0
    ;;

  # tmux new-session -d -c "$WORKDIR" -s "$SESSION" "$LAUNCH"
  new-session)
    detached=0; cwd=""; session=""; launch=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -d) detached=1; shift ;;
        -c) cwd="$2"; shift 2 ;;
        -s) session="$2"; shift 2 ;;
        -x|-y) shift 2 ;;  # ignore geometry
        --) shift; launch="$*"; break ;;
        -*) shift ;;
        *)  launch="$*"; break ;;
      esac
    done
    [ -n "$session" ] || die "new-session: missing -s"

    # kill existing workspace with same label (idempotent, like tmux)
    resolve_label "$session" >/dev/null 2>&1 && {
      local_wid=$(resolve_label "$session")
      "$HERDR" workspace close "$local_wid" >/dev/null 2>&1 || true
    }

    # build --env flags from the global env file
    env_args=()
    if [ -f "$ENV_FILE" ]; then
      while IFS='=' read -r ek ev; do
        [ -n "$ek" ] && env_args+=(--env "${ek}=${ev}")
      done < "$ENV_FILE"
    fi

    # create workspace (detached = --no-focus)
    focus_flag="--no-focus"
    create_out=$("$HERDR" workspace create \
      ${cwd:+--cwd "$cwd"} \
      --label "$session" \
      "${env_args[@]}" \
      "$focus_flag" 2>&1) || { echo "$create_out" >&2; exit 1; }

    # extract workspace_id from create output, fall back to listing
    wid=$(echo "$create_out" | jq -r \
      '.workspace_id // .workspace.workspace_id // .result.workspace_id // .result.workspace.workspace_id // .result.root_pane.workspace_id // empty' 2>/dev/null)
    [ -n "$wid" ] || wid=$(resolve_label "$session") || die "new-session: could not resolve workspace_id for $session"

    # resolve root pane. The pane is spawned asynchronously by the herdr server,
    # so a list immediately after create can race (returns no pane on slower
    # hosts) — retry briefly before giving up. Accept both `.result.panes` and
    # top-level `.panes` shapes, and fall back to the create output's root_pane.
    pid=""
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      pid=$("$HERDR" pane list --workspace "$wid" 2>/dev/null | jq -r \
        '(.result.panes // .panes // .) | if type=="array" then .[] else . end | .pane_id // empty' 2>/dev/null | head -1)
      [ -n "$pid" ] && break
      sleep 0.3
    done
    [ -n "$pid" ] || pid=$(echo "$create_out" | jq -r '.result.root_pane.pane_id // .root_pane.pane_id // empty' 2>/dev/null)
    [ -n "$pid" ] || die "new-session: no root pane in workspace $wid"

    # run the launch command in the root pane (pane run = text + Enter)
    if [ -n "$launch" ]; then
      "$HERDR" pane run "$pid" "$launch" >/dev/null 2>&1 || true
    fi
    exit 0
    ;;

  # tmux kill-session -t "$SESSION"
  kill-session)
    target=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -t) target="$2"; shift 2 ;;
        *)  shift ;;
      esac
    done
    [ -n "$target" ] || exit 0
    wid=$(resolve_label "$target") || exit 0
    "$HERDR" workspace close "$wid" >/dev/null 2>&1 || true
    exit 0
    ;;

  # tmux has-session -t <session>
  has-session)
    target=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -t) target="$2"; shift 2 ;;
        *)  shift ;;
      esac
    done
    [ -n "$target" ] || exit 1
    resolve_label "$target" >/dev/null 2>&1 || exit 1
    exit 0
    ;;

  # tmux ls  (VM health probe — orchid just checks exit code + that it ran)
  ls|list-sessions)
    "$HERDR" workspace list 2>/dev/null | jq -r \
      '(.result.workspaces // .workspaces // .) | if type=="array" then .[] else . end
       | "\(.label // .workspace_id): 1 windows (created)"' 2>/dev/null || true
    exit 0
    ;;

  # tmux capture-pane -p [-e] -t <session>
  capture-pane)
    print=0; escapes=0; target=""; lines=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -p) print=1; shift ;;
        -e) escapes=1; shift ;;
        -t) target="$2"; shift 2 ;;
        -S|-E) shift 2 ;;  # ignore start/end line offsets
        -L) lines="$2"; shift 2 ;;  # not standard but just in case
        *)  shift ;;
      esac
    done
    [ -n "$target" ] || exit 0
    pid=$(resolve_pane "$target") || exit 0
    # herdr pane read --source recent gives scrollback with wrapping.
    # --ansi is only documented for --source visible; use it if escapes requested.
    if [ $escapes -eq 1 ]; then
      "$HERDR" pane read "$pid" --source visible --ansi 2>/dev/null \
        || "$HERDR" pane read "$pid" --source recent 2>/dev/null \
        || true
    else
      "$HERDR" pane read "$pid" --source recent 2>/dev/null || true
    fi
    exit 0
    ;;

  # tmux send-keys -t <session> <key>
  send-keys)
    target=""; keys=()
    while [ $# -gt 0 ]; do
      case "$1" in
        -t) target="$2"; shift 2 ;;
        -l|-R) shift ;;  # literal/repeat flags
        *)  keys+=("$1"); shift ;;
      esac
    done
    [ -n "$target" ] || exit 0
    pid=$(resolve_pane "$target") || exit 0
    mapped=()
    for k in "${keys[@]}"; do
      mapped+=("$(map_key "$k")")
    done
    "$HERDR" pane send-keys "$pid" "${mapped[@]}" 2>/dev/null || true
    exit 0
    ;;

  # tmux load-buffer -b <name> -   (reads issue body from stdin)
  load-buffer)
    bufname=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -b) bufname="$2"; shift 2 ;;
        -w|-t) shift 2 ;;
        --) shift; break ;;
        *)  shift ;;
      esac
    done
    [ -n "$bufname" ] || bufname="default"
    cat > "$BUF_DIR/herdr-buf-$bufname"
    exit 0
    ;;

  # tmux paste-buffer [-p] -b <name> -t <session> -d
  paste-buffer)
    bufname=""; target=""; del=0; bracketed=0
    while [ $# -gt 0 ]; do
      case "$1" in
        -b) bufname="$2"; shift 2 ;;
        -t) target="$2"; shift 2 ;;
        -d) del=1; shift ;;
        -p) bracketed=1; shift ;;
        *)  shift ;;
      esac
    done
    [ -n "$bufname" ] || bufname="default"
    [ -n "$target" ] || exit 0
    pid=$(resolve_pane "$target") || exit 0
    bufpath="$BUF_DIR/herdr-buf-$bufname"
    [ -f "$bufpath" ] || exit 0
    # send-text takes the content as a single arg; fine for issue bodies (<ARG_MAX)
    "$HERDR" pane send-text "$pid" "$(cat "$bufpath")" 2>/dev/null || true
    [ $del -eq 1 ] && rm -f "$bufpath"
    exit 0
    ;;

  # tmux delete-buffer -b <name>
  delete-buffer)
    bufname=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -b) bufname="$2"; shift 2 ;;
        *)  shift ;;
      esac
    done
    [ -n "$bufname" ] && rm -f "$BUF_DIR/herdr-buf-$bufname"
    exit 0
    ;;

  # tmux rename-session -t <old> <new>
  rename-session)
    target=""; newname=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -t) target="$2"; shift 2 ;;
        *)  newname="$1"; shift ;;
      esac
    done
    [ -n "$target" ] && [ -n "$newname" ] || exit 0
    wid=$(resolve_label "$target") || exit 0
    "$HERDR" workspace rename "$wid" "$newname" >/dev/null 2>&1 || true
    exit 0
    ;;

  # tmux list-panes -t <session> -F '#{pane_pid}'
  list-panes)
    target=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -t) target="$2"; shift 2 ;;
        -F) shift 2 ;;  # ignore format string
        *)  shift ;;
      esac
    done
    [ -n "$target" ] || exit 0
    pid=$(resolve_pane "$target") || exit 0
    # pane process-info returns shell pid + foreground process group + processes.
    # Output pids one per line to match tmux's -F '#{pane_pid}' format.
    "$HERDR" pane process-info --pane "$pid" 2>/dev/null | jq -r \
      '(.result.process_info // .result // .) as $p
       | [$p.shell_pid // empty, ($p.foreground_processes // [] | .[].pid // empty),
          $p.foreground_pgid // empty]
       | map(select(. != null and . != ""))
       | unique | .[]' 2>/dev/null || true
    exit 0
    ;;

  # tmux pipe-pane -t <session>  (stop piping → no-op)
  # tmux pipe-pane -t <session> "cat > $F"  (start piping → best-effort)
  pipe-pane)
    target=""; pipe_cmd=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -t) target="$2"; shift 2 ;;
        -o) shift ;;
        *)  pipe_cmd="$*"; break ;;
      esac
    done
    # If no pipe command, this is a "stop piping" call → no-op.
    [ -z "$pipe_cmd" ] && exit 0
    # herdr has no pipe-pane equivalent. The relay streaming path
    # (relay_agent.go runCapture) needs an orchid Go change to use polling
    # instead of pipe-pane. The shim logs a warning and exits 0 so the
    # surrounding bash script doesn't abort.
    printf 'tmux-shim: pipe-pane start is unsupported by herdr — relay streaming needs orchid patch (see docs/herdr-migration.md)\n' >&2
    exit 0
    ;;

  # tmux resize-window -t <session> -x <cols> -y <rows>
  resize-window|resize-pane)
    target=""; cols=""; rows=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -t) target="$2"; shift 2 ;;
        -x) cols="$2"; shift 2 ;;
        -y) rows="$2"; shift 2 ;;
        *)  shift ;;
      esac
    done
    # herdr's pane resize is directional + ratio, not absolute cols/rows.
    # Best-effort: no-op for now. The dashboard pane viewer works with the
    # pane's natural size. Full absolute resize needs an orchid Go change
    # to compute ratios from pane.layout. See docs/herdr-migration.md.
    exit 0
    ;;

  # tmux attach-session -t <session>
  attach-session)
    target=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -t) target="$2"; shift 2 ;;
        *)  shift ;;
      esac
    done
    if [ -n "$target" ]; then
      # focus the workspace by label so the user lands in the right pane
      wid=$(resolve_label "$target" 2>/dev/null) || true
      [ -n "$wid" ] && "$HERDR" workspace focus "$wid" >/dev/null 2>&1 || true
    fi
    exec "$HERDR"
    ;;

  # tmux display-message, refresh-client, etc. — swallow silently
  display-message|display|refresh-client|set|set-option|set-window-option|bind-key|unbind-key|source-file)
    exit 0
    ;;

  # unknown subcommand — pass through to real tmux if available, else no-op
  *)
    if command -v tmux.real >/dev/null 2>&1; then
      exec tmux.real "$cmd" "$@"
    fi
    printf 'tmux-shim: unhandled subcommand "%s" — no-op\n' "$cmd" >&2
    exit 0
    ;;
esac

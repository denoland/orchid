#!/usr/bin/env bash
# Orchid installer.
#
# Runs as your normal user. sudo is invoked only for the steps that
# genuinely need it:
#   - package install (apt-get / dnf) for system deps
#   - `loginctl enable-linger` so the user-level service survives logout
#   - /etc/sysctl.d/99-orchid.conf for the AppArmor unprivileged-userns
#     knob (claude needs unpriv userns to sandbox sub-processes)
#
# Everything else — Go toolchain, source clone, build, binary, swarm.hcl,
# systemd unit — lives under your $HOME.
#
# Usage:
#   curl -fsSL https://orchid.littledivy.com/install.sh | bash
#
# Modes:
#   default       Install + run the orchestrator daemon (central).
#   WORKER=1      Worker-only install. Skips swarm.hcl + service.
#                 After it finishes, run:
#                   orch join vm <central-url> <invite-token>
#                 The central orch generates a dedicated SSH key, drops
#                 the pubkey into ~/.ssh/authorized_keys here, and adds
#                 a vm "<name>" {} block to its swarm.hcl.
#
# Env overrides:
#   INSTALL_DIR    $HOME/.orch
#   BIN_DIR        $HOME/.local/bin
#   SRC_DIR        $INSTALL_DIR/src
#   GO_VERSION     1.25.0 (only downloaded if system go is absent/older)
#   ORCHID_REPO    denoland/orchid (source repo to build from)
#   INBOX_REPO     prompted          (e.g. denoland/orchid; central only)
#   HTTP_SECRET    auto              (32-hex random; gates the dashboard)
#   CAPTURE_TOKEN  auto              (32-hex random; gates /api/drafts)
#   SKIP_FETCH=1   reuse $SRC_DIR/.git without running `gh repo clone`
#
# Idempotent: re-running pulls latest, rebuilds, reloads the user service.

if [ -z "${BASH_VERSION:-}" ]; then
  echo "error: install.sh requires bash. Re-run with:" >&2
  echo "  curl -fsSL https://orchid.littledivy.com/install.sh | bash" >&2
  exit 1
fi

set -euo pipefail

INSTALL_DIR=${INSTALL_DIR:-$HOME/.orch}
BIN_DIR=${BIN_DIR:-$HOME/.local/bin}
SRC_DIR=${SRC_DIR:-$INSTALL_DIR/src}
# modernc.org/sqlite v1.50+ (used for state.db) needs go 1.25.
GO_VERSION=${GO_VERSION:-1.25.0}
WORKER=${WORKER:-0}
ORCHID_REPO=${ORCHID_REPO:-denoland/orchid}

say()  { printf "\033[1;35m▶\033[0m %s\n" "$*"; }
note() { printf "  %s\n" "$*"; }
die()  { printf "\033[1;31m✗\033[0m %s\n" "$*" >&2; exit 1; }

# Detect privilege + init system. The default path is a user-owned
# install on a systemd box; running as root in a container (no sudo,
# no systemd) is supported too — common on exe.dev / fly machines /
# bare docker — by falling back to a nohup-managed launch script.
IS_ROOT=0
[ "$(id -u)" -eq 0 ] && IS_ROOT=1
HAS_SYSTEMD=0
# Probing the binary isn't enough — containers ship the systemctl CLI
# even when PID 1 is some other init. /run/systemd/system only exists
# when systemd is actually running as the system manager.
[ -d /run/systemd/system ] && HAS_SYSTEMD=1

if [ "$IS_ROOT" -eq 0 ] && ! command -v sudo >/dev/null; then
  die "sudo not found — install sudo, or run as root inside a container"
fi

# Root-mode gating. Two reasons running as root is risky:
#   1. claude refuses --dangerously-skip-permissions when its tmux
#      session runs as root → central can never spawn work locally.
#   2. The per-user systemd manager dies on shell exit without
#      `loginctl enable-linger`, and lingering root is a footgun.
#
# Central mode (default): always refuse root. The daemon hosts the
#   local VM, which would spawn claude tmux as root → broken.
# Worker mode (WORKER=1): allow root only on a container host (no
#   systemd). The "worker" is then a remote box central SSHes into;
#   central uses whatever user --user=... pointed at, so a root-only
#   container can be made to work for testing.
if [ "$IS_ROOT" -eq 1 ]; then
  if [ "${WORKER:-0}" != "1" ]; then
    die "refusing to install central as root — create a non-root user (e.g. \`adduser orchid && usermod -aG sudo orchid\`), then re-run as that user. (claude refuses --dangerously-skip-permissions when running as root, so the local VM would never spawn a session.)"
  fi
  if [ "$HAS_SYSTEMD" -eq 1 ]; then
    die "refusing to install worker as root on a systemd host — create a regular user and re-run as that user."
  fi
fi

# as_root runs the given command with root privilege. When already
# root, it execs directly; otherwise it goes through sudo.
as_root() {
  if [ "$IS_ROOT" -eq 1 ]; then
    "$@"
  else
    sudo "$@"
  fi
}

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; esac
if [ "$OS" != linux ]; then
  cat >&2 <<EOF
✗ unsupported OS: $OS

The orch daemon is Linux-only. On macOS, sign up at
https://orchid.littledivy.com instead and use \`orch join\` to attach
this Mac as a worker — the central daemon stays in the cloud.

If you want to self-host the daemon, run install.sh inside a Linux
container or VM (Docker, OrbStack, Lima, etc).
EOF
  exit 1
fi

# Pick a package manager + install prereqs. apt and dnf both ship `gh`
# from their own repos on recent distros; for older ones the install
# may need https://cli.github.com/packages instructions instead.
say "installing prerequisites${IS_ROOT:+ (as root)}"
if command -v apt-get >/dev/null; then
  as_root apt-get update -qq
  as_root env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    git tmux openssl openssh-client openssh-server curl jq ca-certificates >/dev/null
elif command -v dnf >/dev/null; then
  as_root dnf install -y -q git tmux openssl openssh-clients openssh-server curl jq >/dev/null
else
  die "no apt-get or dnf — install git tmux openssl openssh curl jq manually"
fi

# Always install gh from GitHub's own apt/dnf repo: the version shipped
# by older distros (e.g. Ubuntu 22.04 → gh 2.4) is missing flags this
# script uses for non-interactive auth setup. Already-current installs
# are a no-op.
GH_OK=0
if command -v gh >/dev/null; then
  GH_VER=$(gh --version 2>/dev/null | awk 'NR==1{print $3}' | cut -d. -f1)
  [ "${GH_VER:-0}" -ge 2 ] && gh auth setup-git --help >/dev/null 2>&1 && GH_OK=1
fi
if [ "$GH_OK" -ne 1 ]; then
  if command -v apt-get >/dev/null; then
    as_root env DEBIAN_FRONTEND=noninteractive apt-get -y -qq purge gh >/dev/null 2>&1 || true
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | \
      as_root dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg status=none
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | \
      as_root tee /etc/apt/sources.list.d/github-cli.list >/dev/null
    as_root apt-get update -qq
    as_root env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq gh >/dev/null
  elif command -v dnf >/dev/null; then
    as_root dnf install -y -q 'dnf-command(config-manager)' >/dev/null
    as_root dnf config-manager --add-repo https://cli.github.com/packages/rpm/gh-cli.repo >/dev/null
    as_root dnf install -y -q gh >/dev/null
  fi
fi

# gh handles its own credential helper; orch shells out to `gh` for every
# GitHub call so reusing the operator's existing login means no PAT to
# stash on disk and no token to rotate.
say "checking gh authentication"
if ! gh auth status -h github.com >/dev/null 2>&1; then
  cat >&2 <<'EOF'
✗ gh CLI is not authenticated to github.com.

Run:
  gh auth login --hostname github.com --git-protocol https --web

Pick scopes: repo, read:org, workflow.
Then re-run this installer. The orch daemon reuses the same gh auth
(no GH_TOKEN needed) from ~/.config/gh/hosts.yml.
EOF
  exit 1
fi

# Wire gh's credential helper into git so the later `git fetch` (on the
# update path) reuses the same auth that `gh repo clone` did. Idempotent.
gh auth setup-git -h github.com >/dev/null

# Go: prefer system install when present and recent enough; otherwise
# drop a private copy under $HOME/.local/go. No sudo, no /usr/local.
GO_BIN=$(command -v go || true)
GO_OK=0
if [ -n "$GO_BIN" ]; then
  GO_VER=$("$GO_BIN" env GOVERSION 2>/dev/null | sed 's/go//' | head -c4)
  # modernc.org/sqlite (state.db driver) requires go 1.25+.
  [ -n "$GO_VER" ] && [ "$GO_VER" \> "1.24" ] && GO_OK=1
fi
if [ "$GO_OK" -ne 1 ]; then
  say "installing go $GO_VERSION into \$HOME/.local/go"
  mkdir -p "$HOME/.local"
  rm -rf "$HOME/.local/go"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.${OS}-${ARCH}.tar.gz" \
    | tar -xz -C "$HOME/.local"
  GO_BIN="$HOME/.local/go/bin/go"
fi
export PATH="$HOME/.local/go/bin:$BIN_DIR:$PATH"

mkdir -p "$BIN_DIR" "$INSTALL_DIR"

say "fetching orchid source into $SRC_DIR"
if [ "${SKIP_FETCH:-0}" = "1" ]; then
  [ -d "$SRC_DIR/.git" ] || die "SKIP_FETCH=1 but $SRC_DIR has no .git — clone there first"
  note "using existing $SRC_DIR (SKIP_FETCH=1)"
elif [ -d "$SRC_DIR/.git" ]; then
  git -C "$SRC_DIR" fetch --quiet origin
  git -C "$SRC_DIR" reset --hard --quiet origin/main
else
  rm -rf "$SRC_DIR"
  mkdir -p "$(dirname "$SRC_DIR")"
  # `gh repo clone` goes through the gh credential flow (handles SSO
  # bouncing, fine-grained PATs, device-flow tokens) — strictly better
  # than the historical https://x-access-token:$TOK@github.com URL,
  # which dies for any auth method that needs interaction.
  gh repo clone "$ORCHID_REPO" "$SRC_DIR" -- --quiet --depth 1
fi

say "building dashboard SPA"
# The orch binary //go:embed internal/orch/embed-dist for the self-hosted
# dashboard. Relay-served deploys serve the SPA from CF's ASSETS binding,
# so for those a placeholder is enough; here we always build the SPA so
# `http://host:8000/` actually loads in a browser.
EMBED_DIR="$SRC_DIR/internal/orch/embed-dist"
mkdir -p "$EMBED_DIR"
[ -e "$EMBED_DIR/.placeholder" ] || echo "served via relay" > "$EMBED_DIR/.placeholder"
if [ -d "$SRC_DIR/www" ] && [ -f "$SRC_DIR/www/package.json" ]; then
  # Vite needs Node 20+. Ubuntu 22.04 ships node 12 in the default repo,
  # so check the version and fall back to NodeSource for a current LTS.
  NODE_OK=0
  if command -v node >/dev/null; then
    NODE_MAJOR=$(node -e 'process.stdout.write(String(process.versions.node.split(".")[0]))' 2>/dev/null || echo 0)
    [ "${NODE_MAJOR:-0}" -ge 20 ] && NODE_OK=1
  fi
  if [ "$NODE_OK" -ne 1 ]; then
    say "installing nodejs 20 (needed to build the dashboard)"
    if command -v apt-get >/dev/null; then
      # Strip any old packaged libnode/nodejs first — the NodeSource .deb
      # ships overlapping /usr/include/node files and dpkg refuses to
      # overwrite a distro-managed file from another package.
      as_root env DEBIAN_FRONTEND=noninteractive apt-get -y -qq purge \
        nodejs libnode-dev libnode72 'node-*' >/dev/null 2>&1 || true
      if [ "$IS_ROOT" -eq 1 ]; then
        curl -fsSL https://deb.nodesource.com/setup_20.x | bash - >/dev/null
      else
        curl -fsSL https://deb.nodesource.com/setup_20.x | sudo -E bash - >/dev/null
      fi
      as_root env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nodejs >/dev/null
    elif command -v dnf >/dev/null; then
      if [ "$IS_ROOT" -eq 1 ]; then
        curl -fsSL https://rpm.nodesource.com/setup_20.x | bash - >/dev/null
      else
        curl -fsSL https://rpm.nodesource.com/setup_20.x | sudo -E bash - >/dev/null
      fi
      as_root dnf install -y -q nodejs >/dev/null
    fi
  fi
  ( cd "$SRC_DIR/www" && npm install --silent --no-audit --no-fund && npm run build --silent >/dev/null )
fi

say "building orch binary"
( cd "$SRC_DIR" && CGO_ENABLED=0 "$GO_BIN" build -o "$BIN_DIR/orch.new" ./cmd/orch )
mv "$BIN_DIR/orch.new" "$BIN_DIR/orch"
chmod +x "$BIN_DIR/orch"

# claude clones work repos over SSH on each session spawn — without the
# host key trusted, the very first git clone dies.
say "trusting github.com SSH host keys"
mkdir -p "$HOME/.ssh"
touch "$HOME/.ssh/known_hosts"
chmod 600 "$HOME/.ssh/known_hosts"
if ! ssh-keygen -F github.com -f "$HOME/.ssh/known_hosts" >/dev/null 2>&1; then
  ssh-keyscan -t rsa,ed25519 github.com 2>/dev/null >> "$HOME/.ssh/known_hosts"
fi

if [ "$WORKER" = "1" ]; then
  # Worker hosts don't run the orchestrator daemon — central drives them
  # over SSH. Make sure sshd is up so `orch join vm` can hand the
  # central host a working SSH endpoint.
  if [ "$HAS_SYSTEMD" -eq 1 ]; then
    as_root systemctl enable --now ssh 2>/dev/null || as_root systemctl enable --now sshd 2>/dev/null || true
  else
    # Container path: start sshd directly. exe.dev / docker images ship
    # the daemon but no service manager.
    as_root mkdir -p /run/sshd
    pgrep -x sshd >/dev/null || as_root /usr/sbin/sshd -D >/dev/null 2>&1 &
  fi
  cat <<EOF

\033[1;32m✓ orchid worker prerequisites installed\033[0m

next, on this machine:
  $BIN_DIR/orch join vm <central-url> <invite-token>

  Get <invite-token> from the central host (its dashboard's Settings →
  Add VM, or read orchestrator.http_secret from \$INSTALL_DIR/swarm.hcl
  there). Central generates a dedicated SSH key for itself, pushes the
  public half to ~/.ssh/authorized_keys here, and appends a
  vm "<name>" {} block to its swarm.hcl.

If $BIN_DIR isn't on your PATH, add to ~/.bashrc / ~/.zshrc:
  export PATH="$BIN_DIR:\$PATH"
EOF
  exit 0
fi

# ─── central daemon mode ───

if [ -z "${INBOX_REPO:-}" ]; then
  # We're typically running under `curl | bash`, so stdin is the script
  # body, not the terminal. Read the prompt from /dev/tty when one is
  # attached; otherwise default silently.
  if [ -e /dev/tty ]; then
    read -rp "Inbox repo [denoland/orchid]: " INBOX_REPO < /dev/tty || true
  fi
  INBOX_REPO=${INBOX_REPO:-denoland/orchid}
fi

HTTP_SECRET=${HTTP_SECRET:-$(openssl rand -hex 16)}
CAPTURE_TOKEN=${CAPTURE_TOKEN:-$(openssl rand -hex 16)}

# Bot login = GitHub account orch commits/PRs as. Defaults to the logged-in
# gh user. Orchid refuses to start without one set.
BOT_LOGIN=${BOT_LOGIN:-$(gh api user --jq .login 2>/dev/null || echo "")}
if [ -z "$BOT_LOGIN" ]; then
  die "could not detect a GitHub login via gh. Set BOT_LOGIN=<your-gh-user> and re-run."
fi

mkdir -p "$INSTALL_DIR/captures" "$INSTALL_DIR/vm-keys" "$INSTALL_DIR/orch-work"
chmod 700 "$INSTALL_DIR/vm-keys"

if [ ! -f "$INSTALL_DIR/swarm.hcl" ]; then
  say "writing $INSTALL_DIR/swarm.hcl"
  cat > "$INSTALL_DIR/swarm.hcl" <<EOF
github {
  inbox_repo = "$INBOX_REPO"
}

orchestrator {
  bot_login     = "$BOT_LOGIN"
  poll_interval = "30s"
  state_db      = "$INSTALL_DIR/state.db"
  branch_prefix = "orch/"
  workdir_root  = "$INSTALL_DIR/orch-work"
  http_addr     = ":8000"
  http_secret   = "$HTTP_SECRET"

  capture {
    auth_token = "$CAPTURE_TOKEN"
    assets_dir = "$INSTALL_DIR/captures"
  }
}

# Local VM. The orchestrator daemon and the spawned claude tmux
# sessions both run as $USER — claude refuses
# --dangerously-skip-permissions as root, so a user-owned install
# Just Works without a separate service user.
vm "local" {
  host     = "localhost"
  user     = "$USER"
  capacity = 4
}

bootstrap_prompt = ""
EOF
else
  note "swarm.hcl exists — leaving as-is"
fi

# env file: relay endpoint, populated by `orch join`. Daemon picks up
# GitHub auth from ~/.config/gh/hosts.yml, so no GH_TOKEN to leak.
[ -f "$INSTALL_DIR/env" ] || cat > "$INSTALL_DIR/env" <<EOF
RELAY_URL=
RELAY_TOKEN=
EOF
chmod 600 "$INSTALL_DIR/env"

# Disable AppArmor's restriction on unprivileged user namespaces so
# claude can build its own sandbox. Best-effort: containers have
# read-only /proc, so swallow failures.
if [ -f /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]; then
  echo "kernel.apparmor_restrict_unprivileged_userns=0" | as_root tee /etc/sysctl.d/99-orchid.conf >/dev/null 2>&1 || true
  as_root sysctl -p /etc/sysctl.d/99-orchid.conf >/dev/null 2>&1 || true
fi

LAUNCH_CMD="$BIN_DIR/orch -config $INSTALL_DIR/swarm.hcl"

if [ "$HAS_SYSTEMD" -eq 1 ]; then
  # Standard path: per-user systemd service + linger so it survives logout.
  if [ "$IS_ROOT" -eq 0 ]; then
    as_root loginctl enable-linger "$USER" >/dev/null
  fi
  export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"

  UNIT_DIR="$HOME/.config/systemd/user"
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/orchid.service" <<EOF
[Unit]
Description=Orchid swarm orchestrator (user)
After=network.target

[Service]
Type=simple
# RELAY_URL / RELAY_TOKEN come from $INSTALL_DIR/env after the operator
# runs \`orch join <url> <token>\`. Empty values turn the relay agent
# into a no-op so the daemon still runs locally pre-join.
ExecStart=$LAUNCH_CMD -relay=\${RELAY_URL} -relay-token=\${RELAY_TOKEN}
EnvironmentFile=$INSTALL_DIR/env
WorkingDirectory=$INSTALL_DIR
Restart=always
RestartSec=5
StandardOutput=append:$INSTALL_DIR/orch.log
StandardError=append:$INSTALL_DIR/orch.log

[Install]
WantedBy=default.target
EOF

  systemctl --user daemon-reload
  systemctl --user enable --now orchid

  sleep 2
  systemctl --user is-active --quiet orchid || die "orchid failed to start. check: journalctl --user -u orchid -n 50"
  LOG_HINT="journalctl --user -u orchid -f   (or $INSTALL_DIR/orch.log)"
  STOP_HINT="systemctl --user stop orchid"
else
  # Container fallback: no systemd, manage with nohup + a pidfile. The
  # daemon respawn loop is more anemic than systemd's, but tmux sessions
  # outlive the orch process so this is fine for development swarms.
  cat > "$INSTALL_DIR/run.sh" <<EOF
#!/usr/bin/env bash
# Boot orchid in the background. Re-run to restart.
set -e
PIDFILE="$INSTALL_DIR/orch.pid"
if [ -f "\$PIDFILE" ] && kill -0 "\$(cat "\$PIDFILE")" 2>/dev/null; then
  kill "\$(cat "\$PIDFILE")"
  sleep 1
fi
# shellcheck disable=SC1091
set -a; . "$INSTALL_DIR/env"; set +a
cd "$INSTALL_DIR"
nohup $LAUNCH_CMD -relay="\${RELAY_URL:-}" -relay-token="\${RELAY_TOKEN:-}" \\
  >> "$INSTALL_DIR/orch.log" 2>&1 &
echo \$! > "\$PIDFILE"
echo "orchid: started pid=\$(cat "\$PIDFILE"). tail $INSTALL_DIR/orch.log"
EOF
  chmod +x "$INSTALL_DIR/run.sh"
  "$INSTALL_DIR/run.sh"
  sleep 2
  PID=$(cat "$INSTALL_DIR/orch.pid" 2>/dev/null || echo "")
  if [ -z "$PID" ] || ! kill -0 "$PID" 2>/dev/null; then
    die "orchid failed to start. check: tail -50 $INSTALL_DIR/orch.log"
  fi
  LOG_HINT="tail -F $INSTALL_DIR/orch.log"
  STOP_HINT="kill \$(cat $INSTALL_DIR/orch.pid)   (or re-run $INSTALL_DIR/run.sh)"
fi

IP=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

\033[1;32m✓ orchid is running\033[0m

  dashboard : http://${IP:-localhost}:8000/?token=$HTTP_SECRET
  capture   : http://${IP:-localhost}:8000/api/drafts
                 header X-Capture-Token: $CAPTURE_TOKEN
  state     : $INSTALL_DIR/state.db
  log       : $LOG_HINT
  config    : $INSTALL_DIR/swarm.hcl
  stop      : $STOP_HINT

If $BIN_DIR isn't on your PATH, add to ~/.bashrc / ~/.zshrc:
  export PATH="$BIN_DIR:\$PATH"

next:
  - open the dashboard URL above
  - file an issue in $INBOX_REPO with a target label → orchid spawns a session
  - add a worker VM: on that VM run install.sh with WORKER=1, then
      orch join vm http://${IP:-this-host}:8000 $HTTP_SECRET
EOF

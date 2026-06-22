#!/usr/bin/env bash
# Orchid installer. Downloads a prebuilt binary, drops a system user,
# writes a systemd unit, starts it.
#
# Usage:
#   curl -fsSL https://orchid.littledivy.com/install.sh | bash
#
# Modes:
#   default       Central daemon. Polls inbox repo + serves the dashboard.
#   WORKER=1      Worker-only host. Skips swarm.hcl/service; just makes
#                 sure sshd is up so central can drive this box.
#                 After it finishes:
#                   sudo -u orchid orch join vm <central-url> <token>
#
# Env overrides:
#   ORCHID_VERSION   release tag to install (default: latest)
#   INBOX_REPO       inbox repo (prompted; central only)
#   HTTP_SECRET      dashboard token (default: random 32 hex)
#
# Idempotent: re-running upgrades the binary in place + restarts.

if [ -z "${BASH_VERSION:-}" ]; then
  echo "error: install.sh requires bash. Re-run with: curl -fsSL ... | bash" >&2
  exit 1
fi
set -euo pipefail

ORCHID_VERSION=${ORCHID_VERSION:-latest}
ORCHID_USER=orchid
ORCHID_HOME=/var/lib/orchid
ORCHID_ETC=/etc/orchid
ORCHID_BIN=/usr/local/bin/orch
ORCHID_UNIT=/etc/systemd/system/orchid.service
WORKER=${WORKER:-0}

say()  { printf "\033[1;35m▶\033[0m %s\n" "$*"; }
die()  { printf "\033[1;31m✗\033[0m %s\n" "$*" >&2; exit 1; }

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; *) die "unsupported arch: $ARCH" ;; esac
[ "$OS" = linux ] || die "orchid daemon is Linux-only. To attach a Mac as a worker, install on a Linux box first and run \`orch join vm\` from your Mac."
[ -d /run/systemd/system ] || die "systemd is required (run on a real Linux VM, not a docker container without systemd)."

if [ "$(id -u)" -ne 0 ]; then
  command -v sudo >/dev/null || die "need root or sudo"
  SUDO=sudo
else
  SUDO=""
fi

say "checking prerequisites"
missing=()
for cmd in git ssh ssh-keyscan openssl curl jq gh; do
  command -v "$cmd" >/dev/null || missing+=("$cmd")
done
if [ ${#missing[@]} -gt 0 ]; then
  cat >&2 <<EOF
✗ missing required commands: ${missing[*]}

orchid needs these installed and on PATH before this script runs:
  git ssh ssh-keyscan openssl curl jq
  gh      — https://cli.github.com (must be 2.x+, auth'd as your bot account)

Install them via your package manager, then re-run this script.
EOF
  exit 1
fi

say "creating orchid system user"
id "$ORCHID_USER" >/dev/null 2>&1 || \
  $SUDO useradd --system --create-home --home-dir "$ORCHID_HOME" --shell /bin/bash "$ORCHID_USER"

# Fetch the release tarball through gh so a private orchid repo Just
# Works — gh reuses the caller's existing auth. Run as the invoking
# user (not via $SUDO) so we pick up their gh config in $HOME.
say "downloading orch ($ORCHID_VERSION, linux-$ARCH)"
if ! gh auth status -h github.com >/dev/null 2>&1; then
  die "gh is not authenticated. Run \`gh auth login --hostname github.com --git-protocol https --web\` then re-run."
fi
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT
TAG=""
[ "$ORCHID_VERSION" != "latest" ] && TAG="$ORCHID_VERSION"
gh release download $TAG \
  --repo denoland/orchid \
  --pattern "orch-linux-${ARCH}.tar.gz" \
  --dir "$TMPDIR"
tar -xzf "$TMPDIR/orch-linux-${ARCH}.tar.gz" -C "$TMPDIR"
$SUDO install -m 0755 "$TMPDIR/orch" "$ORCHID_BIN"

# ─── herdr (replaces tmux as the pane host) ───
# orchid drives panes by SSHing in and running `tmux <subcommand>`. We install
# herdr plus a tmux-CLI shim at /usr/local/bin/tmux that translates those calls
# into herdr CLI/socket calls. This lets orchid's Go code run unchanged.
# Real tmux (if present) is preserved as tmux.real for the shim's fallback path.

say "installing herdr"
if ! command -v herdr >/dev/null 2>&1; then
  curl -fsSL https://herdr.dev/install.sh | $SUDO sh -s -- --yes 2>/dev/null \
    || curl -fsSL https://herdr.dev/install.sh | sh -s -- --yes
fi
# herdr installer may put the binary in ~/.local/bin or /usr/local/bin;
# ensure it's reachable system-wide.
HERDR_BIN_PATH=$(command -v herdr 2>/dev/null || true)
if [ -z "$HERDR_BIN_PATH" ]; then
  # try common install locations
  for p in /usr/local/bin/herdr "$HOME/.local/bin/herdr" "$ORCHID_HOME/.local/bin/herdr"; do
    [ -x "$p" ] && HERDR_BIN_PATH="$p" && break
  done
fi
[ -n "$HERDR_BIN_PATH" ] || die "herdr installation failed — install manually from https://herdr.dev and re-run"
# symlink to /usr/local/bin so the orchid user (and the shim) can find it
[ -x /usr/local/bin/herdr ] || $SUDO ln -sf "$HERDR_BIN_PATH" /usr/local/bin/herdr

say "installing tmux→herdr shim"
# preserve real tmux if present
if [ -x /usr/local/bin/tmux ] && [ ! -L /usr/local/bin/tmux ]; then
  $SUDO mv /usr/local/bin/tmux /usr/local/bin/tmux.real
fi
$SUDO curl -fsSL https://orchid.littledivy.com/herdr-tmux-shim.sh -o /usr/local/bin/tmux
$SUDO chmod 0755 /usr/local/bin/tmux

say "installing herdr agent integrations"
# integrations give the herdr sidebar semantic agent state (blocked/working/done)
# instead of relying on process-name detection alone.
for agent in opencode claude codex; do
  $SUDO -u "$ORCHID_USER" /usr/local/bin/herdr integration install "$agent" >/dev/null 2>&1 || true
done

say "writing herdr-server systemd unit"
HERDR_UNIT=/etc/systemd/system/herdr-server.service
$SUDO tee "$HERDR_UNIT" >/dev/null <<EOF
[Unit]
Description=Herdr terminal multiplexer server
After=network.target

[Service]
Type=simple
User=$ORCHID_USER
Group=$ORCHID_USER
ExecStart=/usr/local/bin/herdr server
Restart=always
RestartSec=3
Environment=HOME=$ORCHID_HOME

[Install]
WantedBy=multi-user.target
EOF
$SUDO systemctl daemon-reload
$SUDO systemctl enable --now herdr-server
sleep 1
$SUDO systemctl is-active --quiet herdr-server || die "herdr-server failed to start. check: journalctl -u herdr-server -n 50"

if [ "$WORKER" = "1" ]; then
  $SUDO systemctl enable --now ssh 2>/dev/null || $SUDO systemctl enable --now sshd 2>/dev/null || true
  cat <<EOF

\033[1;32m✓ orchid worker prerequisites installed (herdr + tmux shim active)\033[0m

  herdr server : sudo systemctl status herdr-server
  pane host    : /usr/local/bin/tmux (shim → herdr)
  integrations : opencode, claude, codex

next, from this host:
  sudo -u $ORCHID_USER $ORCHID_BIN join vm <central-url> <invite-token>

EOF
  exit 0
fi

# ─── central daemon mode ───

if [ -z "${INBOX_REPO:-}" ]; then
  if [ -e /dev/tty ]; then
    read -rp "Inbox repo [denoland/orchid]: " INBOX_REPO < /dev/tty || true
  fi
  INBOX_REPO=${INBOX_REPO:-denoland/orchid}
fi

HTTP_SECRET=${HTTP_SECRET:-$(openssl rand -hex 16)}

$SUDO mkdir -p "$ORCHID_ETC" "$ORCHID_HOME/vm-keys" "$ORCHID_HOME/orch-work"
$SUDO chown -R "$ORCHID_USER:$ORCHID_USER" "$ORCHID_HOME"
# /etc/orchid needs to be writable by the orchid daemon — both the
# dashboard's Settings save and `orch join vm` rewrite swarm.hcl in
# place. Without this, /api/config PUT fails with EACCES.
$SUDO chown -R "$ORCHID_USER:$ORCHID_USER" "$ORCHID_ETC"
$SUDO chmod 700 "$ORCHID_HOME/vm-keys"

if [ ! -f "$ORCHID_ETC/swarm.hcl" ]; then
  say "writing $ORCHID_ETC/swarm.hcl"
  $SUDO tee "$ORCHID_ETC/swarm.hcl" >/dev/null <<EOF
github {
  inbox_repo = "$INBOX_REPO"
}

orchestrator {
  poll_interval = "30s"
  state_db      = "$ORCHID_HOME/state.db"
  branch_prefix = "orch/"
  workdir_root  = "$ORCHID_HOME/orch-work"
  http_addr     = ":8000"
  http_secret   = "$HTTP_SECRET"
}

vm "local" {
  host     = "localhost"
  user     = "$ORCHID_USER"
  capacity = 4
}

bootstrap_prompt = ""
EOF
else
  say "swarm.hcl exists — leaving as-is"
fi

# env file for RELAY_URL/RELAY_TOKEN, populated by \`orch join\`.
[ -f "$ORCHID_ETC/env" ] || $SUDO tee "$ORCHID_ETC/env" >/dev/null <<EOF
RELAY_URL=
RELAY_TOKEN=
EOF
$SUDO chmod 600 "$ORCHID_ETC/env"
$SUDO chown "$ORCHID_USER:$ORCHID_USER" "$ORCHID_ETC/env"

# AppArmor knob for claude's unprivileged-userns sandbox. Best-effort.
if [ -f /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]; then
  echo "kernel.apparmor_restrict_unprivileged_userns=0" | \
    $SUDO tee /etc/sysctl.d/99-orchid.conf >/dev/null
  $SUDO sysctl -p /etc/sysctl.d/99-orchid.conf >/dev/null 2>&1 || true
fi

say "writing $ORCHID_UNIT"
$SUDO tee "$ORCHID_UNIT" >/dev/null <<EOF
[Unit]
Description=Orchid swarm orchestrator
After=network.target

[Service]
Type=simple
User=$ORCHID_USER
Group=$ORCHID_USER
EnvironmentFile=$ORCHID_ETC/env
WorkingDirectory=$ORCHID_HOME
ExecStart=$ORCHID_BIN -config $ORCHID_ETC/swarm.hcl -relay=\${RELAY_URL} -relay-token=\${RELAY_TOKEN}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

$SUDO systemctl daemon-reload
$SUDO systemctl enable --now orchid

sleep 2
$SUDO systemctl is-active --quiet orchid || die "orchid failed to start. check: journalctl -u orchid -n 50"

IP=$(hostname -I 2>/dev/null | awk '{print $1}')
cat <<EOF

\033[1;32m✓ orchid is running\033[0m

  dashboard : http://${IP:-localhost}:8000/?token=$HTTP_SECRET
  config    : $ORCHID_ETC/swarm.hcl
  state     : $ORCHID_HOME/state.db
  pane host : herdr (tmux shim at /usr/local/bin/tmux)
  log       : journalctl -u orchid -f
  herdr log : journalctl -u herdr-server -f
  stop      : sudo systemctl stop orchid

next:
  - open the dashboard URL above
  - click Connect GitHub (Settings → Integrations) to enable session spawning
  - file an issue in $INBOX_REPO with a target label → orchid spawns a session
  - add a worker VM: on that VM run install.sh with WORKER=1, then
      sudo -u $ORCHID_USER orch join vm http://${IP:-this-host}:8000 $HTTP_SECRET
EOF

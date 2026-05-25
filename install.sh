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
#   GO_VERSION     1.23.4 (only downloaded if system go is absent/older)
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
GO_VERSION=${GO_VERSION:-1.23.4}
WORKER=${WORKER:-0}
ORCHID_REPO=${ORCHID_REPO:-denoland/orchid}

say()  { printf "\033[1;35m▶\033[0m %s\n" "$*"; }
note() { printf "  %s\n" "$*"; }
die()  { printf "\033[1;31m✗\033[0m %s\n" "$*" >&2; exit 1; }

# Refuse to keep running as root — the script is designed around a
# user-owned install. Root-mode installs are the legacy path; nothing
# below assumes / wants UID 0.
if [ "$(id -u)" -eq 0 ]; then
  die "run as your normal user, not root. sudo is invoked internally only where needed."
fi

if ! command -v sudo >/dev/null; then
  die "sudo not found — install sudo (or run inside a shell with it on PATH)"
fi

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; esac
[ "$OS" = linux ] || die "unsupported OS: $OS (orchid needs systemd, Linux only)"
command -v systemctl >/dev/null || die "systemd required"

say "installing prerequisites (will prompt for sudo)"
if command -v apt-get >/dev/null; then
  sudo apt-get update -qq
  sudo apt-get install -y -qq git tmux openssl openssh-client openssh-server curl gh jq >/dev/null
elif command -v dnf >/dev/null; then
  sudo dnf install -y -q git tmux openssl openssh-clients openssh-server curl gh jq >/dev/null
else
  die "no apt-get or dnf — install git tmux openssl openssh curl gh jq manually"
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
  [ -n "$GO_VER" ] && [ "$GO_VER" \> "1.22" ] && GO_OK=1
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

say "building orch binary"
# orch.go //go:embed www/dist for the self-hosted dashboard. Relay-served
# deploys serve the SPA from CF's ASSETS binding, so the embedded copy
# is unused there — a placeholder is enough to satisfy //go:embed.
mkdir -p "$SRC_DIR/www/dist"
[ -e "$SRC_DIR/www/dist/.placeholder" ] || echo "served via relay" > "$SRC_DIR/www/dist/.placeholder"
( cd "$SRC_DIR" && CGO_ENABLED=0 "$GO_BIN" build -o "$BIN_DIR/orch.new" . )
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
  # over SSH. Just make sure sshd is up so `orch join vm` can hand the
  # central host a working SSH endpoint.
  sudo systemctl enable --now ssh 2>/dev/null || sudo systemctl enable --now sshd 2>/dev/null || true
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

mkdir -p "$INSTALL_DIR/captures" "$INSTALL_DIR/vm-keys" "$INSTALL_DIR/orch-work"
chmod 700 "$INSTALL_DIR/vm-keys"

if [ ! -f "$INSTALL_DIR/swarm.hcl" ]; then
  say "writing $INSTALL_DIR/swarm.hcl"
  cat > "$INSTALL_DIR/swarm.hcl" <<EOF
github {
  inbox_repo = "$INBOX_REPO"
}

orchestrator {
  poll_interval = "30s"
  state_file    = "$INSTALL_DIR/state.json"
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
# claude can build its own sandbox. Persists to /etc/sysctl.d.
if [ -f /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]; then
  echo "kernel.apparmor_restrict_unprivileged_userns=0" | sudo tee /etc/sysctl.d/99-orchid.conf >/dev/null
  sudo sysctl -p /etc/sysctl.d/99-orchid.conf >/dev/null 2>&1 || true
fi

# Enable user-service lingering so the daemon keeps running after the
# install shell exits. Requires root; everything else below is per-user.
sudo loginctl enable-linger "$USER" >/dev/null

# Make sure systemctl --user can reach the user manager from this shell.
# Linger guarantees the manager is up; XDG_RUNTIME_DIR points us at it.
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
ExecStart=$BIN_DIR/orch -config $INSTALL_DIR/swarm.hcl -relay=\${RELAY_URL} -relay-token=\${RELAY_TOKEN}
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
if systemctl --user is-active --quiet orchid; then
  IP=$(hostname -I 2>/dev/null | awk '{print $1}')
  cat <<EOF

\033[1;32m✓ orchid is running\033[0m

  dashboard : http://${IP:-localhost}:8000/?token=$HTTP_SECRET
  capture   : http://${IP:-localhost}:8000/api/drafts
                 header X-Capture-Token: $CAPTURE_TOKEN
  state     : $INSTALL_DIR/state.json
  log       : journalctl --user -u orchid -f   (or $INSTALL_DIR/orch.log)
  config    : $INSTALL_DIR/swarm.hcl

If $BIN_DIR isn't on your PATH, add to ~/.bashrc / ~/.zshrc:
  export PATH="$BIN_DIR:\$PATH"

next:
  - open the dashboard URL above
  - file an issue in $INBOX_REPO with a target label → orchid spawns a session
  - add a worker VM: on that VM run install.sh with WORKER=1, then
      orch join vm http://${IP:-this-host}:8000 $HTTP_SECRET
EOF
else
  die "orchid failed to start. check: journalctl --user -u orchid -n 50"
fi

#!/usr/bin/env bash
# Orchid installer. Run on the target machine as root:
#
#     curl -fsSL https://raw.githubusercontent.com/denoland/orchid/main/install.sh | bash
#
# Two modes:
#   default        Install + run the orchestrator daemon (central).
#                  Reads `gh auth status` for the daemon's GitHub creds.
#   WORKER=1       Worker-only install: deps + binary + service user, but no
#                  swarm.hcl and no orchid.service. After it finishes, run
#                    orch join vm <central-url> <invite-token>
#                  on this machine — the central orch will provision a
#                  dedicated SSH key for itself and add a `vm` block.
#
# Env overrides:
#   INSTALL_DIR    /root/orch
#   SERVICE_USER   orchid          (created if missing, runs the worker tmux sessions)
#   BOT_LOGIN      orchidbot       (GitHub login the bot commits/PRs as; central only)
#   INBOX_REPO     (prompted)      (e.g. denoland/orchid; central only)
#   ORCHID_REPO    denoland/orchid (source repo to build orch from)
#
# Idempotent: re-running pulls latest, rebuilds, restarts.
set -euo pipefail

INSTALL_DIR=${INSTALL_DIR:-/root/orch}
SERVICE_USER=${SERVICE_USER:-orchid}
BOT_LOGIN=${BOT_LOGIN:-orchidbot}
SRC_DIR=${SRC_DIR:-/tmp/orchid-src}
GO_VERSION=${GO_VERSION:-1.23.4}
WORKER=${WORKER:-0}

say() { printf "\033[1;35m▶\033[0m %s\n" "$*"; }
die() { printf "\033[1;31m✗\033[0m %s\n" "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "must run as root (try: sudo bash <(curl -fsSL …))"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; esac
[ "$OS" = linux ] || die "unsupported OS: $OS (orchid needs systemd, Linux only)"
command -v systemctl >/dev/null || die "systemd required"

say "installing prerequisites"
if command -v apt-get >/dev/null; then
  apt-get update -qq
  apt-get install -y -qq git tmux openssl openssh-client openssh-server curl gh sudo jq >/dev/null
elif command -v dnf >/dev/null; then
  dnf install -y -q git tmux openssl openssh-clients openssh-server curl gh sudo jq >/dev/null
else
  die "no apt-get or dnf — install git tmux openssl openssh curl gh sudo jq manually"
fi

# Require the operator to authenticate gh themselves rather than us
# stashing a long-lived PAT in /root/orch/env. The daemon shells out to
# `gh` for every API call, which reads ~/.config/gh/hosts.yml; this
# means whatever auth the operator already trusts gets reused as-is.
say "checking gh authentication"
if ! gh auth status -h github.com >/dev/null 2>&1; then
  cat >&2 <<'EOF'
✗ gh CLI is not authenticated to github.com.

Run:
  gh auth login --hostname github.com --git-protocol https --web

Pick scopes: repo, read:org, workflow.
Then re-run this installer. The orch daemon will pick up the same auth
(no GH_TOKEN needed) from /root/.config/gh/hosts.yml.
EOF
  exit 1
fi

if ! command -v go >/dev/null || [ "$(go env GOVERSION 2>/dev/null | sed 's/go//' | head -c4)" \< "1.23" ]; then
  say "installing go $GO_VERSION"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.${OS}-${ARCH}.tar.gz" \
    | tar -xz -C /usr/local
  export PATH=/usr/local/go/bin:$PATH
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
fi

say "fetching orchid source"
# gh auth token honors whatever method the operator used (browser, PAT,
# device flow), so the clone re-uses their existing credentials instead
# of needing a separate GH_TOKEN env.
CLONE_TOKEN=$(gh auth token 2>/dev/null || true)
[ -n "$CLONE_TOKEN" ] || die "gh auth token returned empty — re-run 'gh auth login'"
CLONE_URL="https://x-access-token:${CLONE_TOKEN}@github.com/${ORCHID_REPO:-denoland/orchid}.git"
if [ -d "$SRC_DIR/.git" ]; then
  git -C "$SRC_DIR" remote set-url origin "$CLONE_URL"
  git -C "$SRC_DIR" fetch --quiet origin
  git -C "$SRC_DIR" reset --hard --quiet origin/main
  # Strip the token back out so it doesn't sit in the working copy's
  # .git/config.
  git -C "$SRC_DIR" remote set-url origin "https://github.com/${ORCHID_REPO:-denoland/orchid}.git"
else
  rm -rf "$SRC_DIR"
  git clone --quiet --depth 1 "$CLONE_URL" "$SRC_DIR"
  git -C "$SRC_DIR" remote set-url origin "https://github.com/${ORCHID_REPO:-denoland/orchid}.git"
fi

say "building orch binary"
# orch.go embeds www/dist for the self-hosted dashboard. We don't ship a
# bundled build with the clone (it's gitignored), so seed a placeholder
# file the //go:embed directive can pick up. Relay-served deploys serve
# the SPA from the worker's ASSETS binding, so the embedded copy is
# unused in that path.
mkdir -p "$SRC_DIR/www/dist"
[ -e "$SRC_DIR/www/dist/.placeholder" ] || echo "served via relay" > "$SRC_DIR/www/dist/.placeholder"
( cd "$SRC_DIR" && CGO_ENABLED=0 go build -o /tmp/orch.new . )

say "preparing service user $SERVICE_USER"
id -u "$SERVICE_USER" >/dev/null 2>&1 || useradd -m -s /bin/bash "$SERVICE_USER"
loginctl enable-linger "$SERVICE_USER" >/dev/null 2>&1 || true
runuser -u "$SERVICE_USER" -- mkdir -p "/home/$SERVICE_USER/orch-work"

# Worker sessions clone work repos over SSH — without github.com's host
# key trusted, every spawn dies at the first `git clone`. Trust the
# key for both root (orch's identity) and the service user (claude's).
say "trusting github.com SSH host keys"
for u in root "$SERVICE_USER"; do
  home=$(getent passwd "$u" | cut -d: -f6)
  [ -n "$home" ] || continue
  runuser -u "$u" -- mkdir -p "$home/.ssh"
  runuser -u "$u" -- bash -c "touch '$home/.ssh/known_hosts' && chmod 600 '$home/.ssh/known_hosts'"
  if ! runuser -u "$u" -- ssh-keygen -F github.com -f "$home/.ssh/known_hosts" >/dev/null 2>&1; then
    ssh-keyscan -t rsa,ed25519 github.com 2>/dev/null >> "$home/.ssh/known_hosts"
    chown "$u:" "$home/.ssh/known_hosts"
  fi
done

say "writing $INSTALL_DIR"
mkdir -p "$INSTALL_DIR" "$INSTALL_DIR/captures" "$INSTALL_DIR/vm-keys"
chmod 700 "$INSTALL_DIR/vm-keys"
mv /tmp/orch.new "$INSTALL_DIR/orch"
chmod +x "$INSTALL_DIR/orch"
# Expose the binary on PATH so `orch join …` works from any shell after
# install. Required on both central and worker hosts.
ln -sf "$INSTALL_DIR/orch" /usr/local/bin/orch

if [ "$WORKER" = "1" ]; then
  # Worker hosts don't run the orchestrator daemon — central drives them
  # over SSH. We just need the binary, the service user, and ssh ready.
  # Make sure sshd is up so `orch join vm` (run next) can hand central
  # an SSH key that actually authenticates.
  systemctl enable --now ssh 2>/dev/null || systemctl enable --now sshd 2>/dev/null || true
  cat <<EOF

\033[1;32m✓ orchid worker prerequisites installed\033[0m

next, on this machine:
  orch join vm <central-url> <invite-token>

  Get <invite-token> from the central host (its dashboard's Settings →
  Add VM, or grab the orchestrator.http_secret from $INSTALL_DIR/swarm.hcl
  there). Central will generate a dedicated SSH key, push the public
  half to /home/$SERVICE_USER/.ssh/authorized_keys here, and add a
  vm "<name>" {} block to its swarm.hcl.
EOF
  exit 0
fi

if [ -z "${INBOX_REPO:-}" ]; then
  read -rp "Inbox repo [denoland/orchid]: " INBOX_REPO
  INBOX_REPO=${INBOX_REPO:-denoland/orchid}
fi

HTTP_SECRET=${HTTP_SECRET:-$(openssl rand -hex 16)}
CAPTURE_TOKEN=${CAPTURE_TOKEN:-$(openssl rand -hex 16)}

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
  workdir_root  = "/home/$SERVICE_USER/orch-work"
  http_addr     = ":8000"
  http_secret   = "$HTTP_SECRET"
  bot_login     = "$BOT_LOGIN"

  capture {
    auth_token = "$CAPTURE_TOKEN"
    assets_dir = "$INSTALL_DIR/captures"
  }
}

# Local VM. Orchid runs claude sessions as the $SERVICE_USER user
# in tmux. Capacity = number of concurrent issues this box handles.
vm "local" {
  host     = "localhost"
  user     = "$SERVICE_USER"
  capacity = 4
  bot_login = "$BOT_LOGIN"
}

bootstrap_prompt = ""
EOF
else
  say "swarm.hcl exists — leaving as-is"
fi

# env file holds only relay state now. The daemon picks up GitHub
# credentials via ~/.config/gh/hosts.yml (populated by `gh auth login`),
# so no GH_TOKEN to leak / rotate / sync.
cat > "$INSTALL_DIR/env" <<EOF
RELAY_URL=
RELAY_TOKEN=
EOF
chmod 600 "$INSTALL_DIR/env"

cat > /etc/systemd/system/orchid.service <<EOF
[Unit]
Description=Orchid swarm orchestrator
After=network.target

[Service]
Type=simple
# RELAY_URL / RELAY_TOKEN are picked up from EnvironmentFile after the
# operator runs \`orch join <url> <token>\`. Empty values turn the relay
# agent into a no-op so the daemon still runs locally pre-join.
ExecStart=$INSTALL_DIR/orch -config $INSTALL_DIR/swarm.hcl -relay=\${RELAY_URL} -relay-token=\${RELAY_TOKEN}
EnvironmentFile=$INSTALL_DIR/env
WorkingDirectory=$INSTALL_DIR
Restart=always
RestartSec=5
StandardOutput=append:$INSTALL_DIR/orch.log
StandardError=append:$INSTALL_DIR/orch.log

[Install]
WantedBy=multi-user.target
EOF

# Userns + apparmor knob so the orchid user can run sandboxed sub-processes.
if [ -d /proc/sys/kernel/apparmor_restrict_unprivileged_userns ] 2>/dev/null \
   || [ -f /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]; then
  echo "kernel.apparmor_restrict_unprivileged_userns=0" > /etc/sysctl.d/99-orchid.conf
  sysctl -p /etc/sysctl.d/99-orchid.conf >/dev/null 2>&1 || true
fi

systemctl daemon-reload
systemctl enable --now orchid

sleep 2
if systemctl is-active --quiet orchid; then
  IP=$(hostname -I 2>/dev/null | awk '{print $1}')
  cat <<EOF

\033[1;32m✓ orchid is running\033[0m

  dashboard : http://${IP:-localhost}:8000/?token=$HTTP_SECRET
  capture   : http://${IP:-localhost}:8000/api/drafts
                 header X-Capture-Token: $CAPTURE_TOKEN
  state     : $INSTALL_DIR/state.json
  log       : journalctl -u orchid -f   (or $INSTALL_DIR/orch.log)
  config    : $INSTALL_DIR/swarm.hcl

next:
  - open the dashboard URL above
  - file an issue in $INBOX_REPO with a target label → orchid spawns a session
  - to add a worker VM: on that VM run install.sh with WORKER=1, then
      orch join vm http://${IP:-this-host}:8000 $HTTP_SECRET
EOF
else
  die "orchid failed to start. check: journalctl -u orchid -n 50"
fi

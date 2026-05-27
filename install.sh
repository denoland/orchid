#!/usr/bin/env bash
# orch installer — builds www, builds orch, installs binary into a
# directory on PATH. Works on macOS + Linux without sudo for the build
# itself (only the final mv into /usr/local/bin may need sudo).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
cd "$here"

have() { command -v "$1" >/dev/null 2>&1; }

have go    || { echo "install.sh: 'go' not on PATH (need Go 1.25+). brew install go / apt install golang-go" >&2; exit 1; }
have node  || { echo "install.sh: 'node' not on PATH. brew install node" >&2; exit 1; }
have npm   || { echo "install.sh: 'npm' not on PATH." >&2; exit 1; }

echo "==> building www (vite)"
( cd www && npm install --no-audit --no-fund --silent && npm run build )

echo "==> building orch binary"
go build -o "$here/orch" ./cmd/orch

# Pick install dir in this order: $ORCH_INSTALL_DIR, /usr/local/bin (if
# writable or sudo available), ~/.local/bin (created if missing).
dest="${ORCH_INSTALL_DIR:-}"
if [ -z "$dest" ]; then
    if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
        dest=/usr/local/bin
    elif [ -d /usr/local/bin ] && have sudo; then
        dest=/usr/local/bin
    else
        dest="$HOME/.local/bin"
        mkdir -p "$dest"
    fi
fi

bin="$dest/orch"
if [ -w "$dest" ]; then
    mv -f "$here/orch" "$bin"
else
    sudo mv -f "$here/orch" "$bin"
fi
chmod +x "$bin"

echo
echo "installed: $bin"
case ":$PATH:" in
    *":$dest:"*) ;;
    *)
        echo "warning: $dest is not on PATH. Add to your shell rc:"
        echo "    export PATH=\"$dest:\$PATH\""
        ;;
esac
echo
echo "next: orch join vm <central-url> <token> --user=<ssh-user> --hostname=<reachable-host>"

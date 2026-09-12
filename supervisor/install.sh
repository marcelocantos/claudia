#!/bin/sh
# Render supervisor/claudia.ini into supervisor.d and make supervisord
# the owner of the host broker socket. Exactly one parent may own that
# socket, so evict brew services / owner launchd / any leftover serve
# before starting (same takeover as bullseye/mnemo supervisor/install.sh).
set -e

REPO="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
CONF_DIR="${SUPERVISOR_CONF_DIR:-/opt/homebrew/etc/supervisor.d}"
DEST="$CONF_DIR/claudia.ini"
TEMPLATE="$REPO/supervisor/claudia.ini"

if [ -z "${HOME:-}" ]; then
  HOME="$(eval echo ~"$(id -un)")"
  export HOME
fi

mkdir -p "$CONF_DIR"
mkdir -p "$HOME/.local/var/log"
chmod +x "$REPO/supervisor/run-claudia.sh" "$REPO/supervisor/install.sh"

# Keep a host CLAUDIA_BIN pin across re-renders (this machine runs the
# remint-capable tree build until the next Cellar ships it).
if [ -z "${CLAUDIA_BIN:-}" ] && [ -f "$DEST" ]; then
  CLAUDIA_BIN="$(sed -n 's/.*CLAUDIA_BIN="\([^"]*\)".*/\1/p' "$DEST" | head -n 1)"
fi

rm -f "$DEST"
sed "s|@REPO@|$REPO|g" "$TEMPLATE" >"$DEST"
if [ -n "${CLAUDIA_BIN:-}" ]; then
  tmp="$(mktemp)"
  sed "s|LANG=\"en_US.UTF-8\"|LANG=\"en_US.UTF-8\",CLAUDIA_BIN=\"$CLAUDIA_BIN\"|" "$DEST" >"$tmp"
  mv "$tmp" "$DEST"
  echo "pinned CLAUDIA_BIN=$CLAUDIA_BIN"
fi
echo "rendered $DEST (from $TEMPLATE)"

if [ "${SUPERVISOR_SKIP_CTL:-}" = 1 ]; then
  exit 0
fi

if ! command -v supervisorctl >/dev/null 2>&1; then
  echo "supervisorctl not on PATH — ini written; start Homebrew supervisor to load it" >&2
  exit 1
fi

if [ "${SUPERVISOR_NO_TAKEOVER:-}" = 1 ]; then
  echo "claudia: rendered, not started (SUPERVISOR_NO_TAKEOVER=1)."
  echo "claudia: free the broker socket before supervisord reloads, or the program goes FATAL."
  exit 0
fi

if command -v brew >/dev/null 2>&1; then
  brew services stop claudia >/dev/null 2>&1 || true
fi
if command -v launchctl >/dev/null 2>&1; then
  uid="$(id -u)"
  launchctl bootout "gui/$uid/sh.brew.claudia" 2>/dev/null || true
  launchctl bootout "gui/$uid/homebrew.mxcl.claudia" 2>/dev/null || true
  launchctl bootout "gui/$uid/com.marcelocantos.claudia-broker" 2>/dev/null || true
fi
rm -f "$HOME/Library/LaunchAgents/com.marcelocantos.claudia-broker.plist"

# Leftover `claudia broker serve` after brew/launchd unload. Do not
# match this installer or supervisorctl.
leftover="$(pgrep -f '[c]laudia broker serve' || true)"
if [ -n "$leftover" ]; then
  echo "claudia: stopping leftover broker serve: $leftover"
  # shellcheck disable=SC2086
  kill $leftover 2>/dev/null || true
  i=0
  while [ "$i" -lt 20 ]; do
    leftover="$(pgrep -f '[c]laudia broker serve' || true)"
    [ -z "$leftover" ] && break
    sleep 1
    i=$((i + 1))
  done
  leftover="$(pgrep -f '[c]laudia broker serve' || true)"
  if [ -n "$leftover" ]; then
    # shellcheck disable=SC2086
    kill -9 $leftover 2>/dev/null || true
  fi
fi

supervisorctl reread
supervisorctl update
supervisorctl restart claudia 2>/dev/null || supervisorctl start claudia

echo "claudia installed at $DEST"
supervisorctl status claudia

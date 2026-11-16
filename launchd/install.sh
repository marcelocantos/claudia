#!/bin/sh
# Install the claudia LaunchAgent: render launchd/com.marcelocantos.claudia.plist into
# ~/Library/LaunchAgents and (re)load it in the user's GUI domain. launchd
# runs the program directly, so macOS privacy prompts (TCC) name it rather
# than a supervisor. If an old supervisord program of the same name exists,
# this stops it and removes its config first.
#
#   LAUNCHD_SKIP_LOAD=1  render only; do not touch launchd or supervisord
set -eu

NAME=claudia
LABEL=com.marcelocantos.claudia
REPO="$(CDPATH='' cd "$(dirname "$0")/.." && pwd)"
: "${HOME:?HOME is not set}"
AGENTS="$HOME/Library/LaunchAgents"
DEST="$AGENTS/$LABEL.plist"
DOMAIN="gui/$(id -u)"

mkdir -p "$AGENTS" "$HOME/.local/var/log"
tmp="$(mktemp "$AGENTS/.$LABEL.XXXXXX")"
trap 'rm -f "$tmp"' EXIT HUP INT TERM
sed -e "s|@REPO@|$REPO|g" -e "s|@HOME@|$HOME|g" -e "s|@USER@|$(id -un)|g" \
  "$REPO/launchd/$LABEL.plist" > "$tmp"
# Preserve the explicit dev/bisect pin, never an earlier render's pin.
if [ -n "${CLAUDIA_BIN:-}" ]; then
  plutil -insert EnvironmentVariables.CLAUDIA_BIN -string "$CLAUDIA_BIN" "$tmp"
fi
plutil -lint "$tmp" >/dev/null
chmod 644 "$tmp"
mv "$tmp" "$DEST"
echo "rendered $DEST"

if [ "${LAUNCHD_SKIP_LOAD:-}" = 1 ]; then
  exit 0
fi

old=/opt/homebrew/etc/supervisor.d/$NAME.ini
if [ -f "$old" ]; then
  if command -v supervisorctl >/dev/null 2>&1; then
    supervisorctl stop "$NAME" || true
  fi
  rm -f "$old"
  if command -v supervisorctl >/dev/null 2>&1; then
    supervisorctl update >/dev/null || true
  fi
  echo "removed supervisord program $NAME"
fi

# Retire the older Homebrew and broker-install agents sharing this socket.
if command -v brew >/dev/null 2>&1; then
  brew services stop claudia >/dev/null 2>&1 || true
fi
for old_label in sh.brew.claudia homebrew.mxcl.claudia com.marcelocantos.claudia-broker; do
  launchctl bootout "$DOMAIN/$old_label" 2>/dev/null || true
done
rm -f "$AGENTS/com.marcelocantos.claudia-broker.plist"

launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
# bootstrap can briefly fail while the old instance is still being torn down.
n=0
until launchctl bootstrap "$DOMAIN" "$DEST" 2>/dev/null; do
  n=$((n + 1))
  if [ "$n" -ge 10 ]; then
    launchctl bootstrap "$DOMAIN" "$DEST"
  fi
  sleep 1
done
launchctl print "$DOMAIN/$LABEL" | grep -E '^	(state|pid) = '

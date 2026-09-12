#!/bin/sh
# Start `claudia broker serve` for supervisord. Homebrew Cellar only
# on the shared socket: a PATH lookup can resolve ~/go/bin and
# serve an unreleased tree build to every consumer. CLAUDIA_BIN is the
# explicit pin for a bisect; otherwise prefer brew --prefix/opt.
set -e

if [ -z "${HOME:-}" ]; then
  HOME="$(eval echo ~"$(id -un)")"
  export HOME
fi
export USER="${USER:-$(id -un)}"
export PATH="/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:${HOME}/.cargo/bin:${HOME}/.local/bin:${HOME}/.py/bin:${HOME}/go/bin:${HOME}/.grok/bin:/usr/bin:/bin:/usr/sbin:/sbin"
export TERM="${TERM:-xterm-256color}"
export LANG="${LANG:-en_US.UTF-8}"

if [ -n "${CLAUDIA_BIN:-}" ]; then
  BIN="$CLAUDIA_BIN"
  if [ ! -x "$BIN" ]; then
    echo "claudia: CLAUDIA_BIN=$BIN is not executable" >&2
    exit 1
  fi
else
  BIN=""
  if command -v brew >/dev/null 2>&1; then
    BIN="$(brew --prefix)/opt/claudia/bin/claudia"
  fi
  if [ -z "$BIN" ] || [ ! -x "$BIN" ]; then
    echo "claudia: no Homebrew install at \$brew --prefix/opt/claudia." >&2
    echo "claudia: brew install marcelocantos/tap/claudia" >&2
    exit 1
  fi
fi

if [ "${1:-}" = "--print-bin" ]; then
  echo "$BIN"
  exit 0
fi

echo "claudia: running $BIN broker serve" >&2
exec "$BIN" broker serve

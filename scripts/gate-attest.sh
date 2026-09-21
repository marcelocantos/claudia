#!/bin/sh
# Record that `make gate` passed on the tree it measured.
#
# `make gate` calls this as its last step. It writes one small file under
# .git/ — per clone, never tracked, never dirtying the tree — naming the tree
# it just verified. scripts/hooks/pre-push reads it and refuses to push a
# commit whose tree it does not name. The gate stays the oracle; the hook
# only checks that the oracle ran on what is being pushed.
#
# The key is the tree hash, not the commit: a commit amended without a
# content change is still the verified tree.
set -eu
cd "$(git rev-parse --show-toplevel)"

git_dir=$(git rev-parse --git-dir)
tree=$(git rev-parse HEAD^{tree})
commit=$(git rev-parse HEAD)
dirty=$(git status --porcelain --untracked-files=no | wc -l | tr -d ' ')
at=$(date -u +%Y-%m-%dT%H:%M:%SZ)

cat > "$git_dir/gate-attestation" <<EOF
tree=$tree
commit=$commit
dirty=$dirty
at=$at
EOF

if [ "$dirty" != "0" ]; then
    echo "gate attestation: recorded for tree $tree with $dirty uncommitted change(s) — commit, then run make gate again before pushing" >&2
else
    echo "✓ gate attestation: tree $tree"
fi

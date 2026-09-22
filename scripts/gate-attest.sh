#!/bin/sh
# Record that `make gate` passed on the tree it measured.
#
# `make gate` calls this twice: `begin` as its first step and `end` as its
# last. `begin` notes the tree the gate is about to measure; `end` refuses
# to attest if HEAD's tree is no longer that one — a commit made during the
# 25-minute run would otherwise be attested without having been measured
# (the first version of this script read HEAD at the end and did exactly
# that, 2026-09-22). On success `end` writes one small file under .git/ —
# per clone, never tracked, never dirtying the tree — naming the tree the
# gate verified. scripts/hooks/pre-push reads it and refuses to push a tip
# commit whose tree it does not name. The gate stays the oracle; the hook
# only checks that the oracle ran on what is being pushed.
#
# The key is the tree hash, not the commit: a commit amended without a
# content change is still the verified tree.
set -eu
cd "$(git rev-parse --show-toplevel)"

git_dir=$(git rev-parse --git-dir)
pending="$git_dir/gate-attestation.pending"
final="$git_dir/gate-attestation"

tree=$(git rev-parse HEAD^{tree})
commit=$(git rev-parse HEAD)
dirty=$(git status --porcelain --untracked-files=no | wc -l | tr -d ' ')

case "${1:-end}" in
begin)
    printf 'tree=%s\ncommit=%s\ndirty=%s\n' "$tree" "$commit" "$dirty" > "$pending"
    ;;
end)
    if [ ! -f "$pending" ]; then
        echo "gate attestation: no record of the tree this gate began on — run make gate, not its steps by hand" >&2
        exit 1
    fi
    began=$(sed -n 's/^tree=//p' "$pending")
    began_dirty=$(sed -n 's/^dirty=//p' "$pending")
    rm -f "$pending"
    if [ "$began" != "$tree" ]; then
        echo "gate attestation: HEAD's tree changed during the gate ($began -> $tree); nothing attested — run make gate again" >&2
        exit 1
    fi
    if [ "$began_dirty" != "0" ] || [ "$dirty" != "0" ]; then
        # Record it anyway so the hook can say why it refuses.
        printf 'tree=%s\ncommit=%s\ndirty=%s\nat=%s\n' "$tree" "$commit" "$dirty" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$final"
        echo "gate attestation: recorded for tree $tree with uncommitted changes — commit, then run make gate again before pushing" >&2
        exit 0
    fi
    printf 'tree=%s\ncommit=%s\ndirty=0\nat=%s\n' "$tree" "$commit" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$final"
    echo "✓ gate attestation: tree $tree"
    ;;
*)
    echo "usage: gate-attest.sh begin|end" >&2
    exit 2
    ;;
esac

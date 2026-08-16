# Standing invariants checked by bullseye_convergence.
# Exit 0 = all green; non-zero = at least one violation.
bullseye:
	@go vet ./... && echo "✓ vet"
	@# Never pipe this: `go test ... | tail` reports tail's status, not
	@# go test's, so a failing suite prints "✓ tests" and exits 0. That
	@# trap produced a false green here before.
	@go test -race -count=1 ./... && echo "✓ tests"
	@scripts/check-stability-surface.py >/dev/null && echo "✓ stability surface"
	@$(MAKE) --no-print-directory verify-mutation-evidence >/dev/null && \
	 echo "✓ mutation evidence"
	@test -z "$$(git status --porcelain)" && echo "✓ clean" || \
	 (echo "✗ dirty tree"; git status --short; exit 1)

# T29 oracle: STABILITY.md must enumerate exactly the public surface of the
# release tag named in its "Snapshot as of:" line — no item missing, no item
# claimed that the tag does not have, every row assessed, and the stated
# per-package counts matching. The surface is derived from `go doc -all` over a
# clean worktree of that tag, never from the document, so the check cannot pass
# by agreeing with itself. Needs the tag present locally (CI: fetch-depth 0).
.PHONY: verify-stability
verify-stability:
	@scripts/check-stability-surface.py

# T32 oracle: the mutation evidence a target's acceptance quotes is re-derived
# rather than trusted. Each entry in mutation-evidence.json names a target, a
# mutation as an anchored source edit, and the tests that must go RED under it —
# and the check applies the mutation in a throwaway worktree and runs them. A
# commit that edits those tests' call sites until the mutation stops biting
# fails here, which is the failure no suite can see: 8c5e04a disarmed T28's
# evidence with every suite green.
#
# Both halves run. --prove-teeth is what keeps this check from becoming the next
# unwired oracle: it applies the decay each entry declares (8c5e04a's landed=true
# for T28) and requires it to be invisible to the suites AND to make this check
# go red. A check that cannot be shown to fail is not evidence either.
# CI runs this in .github/workflows/test.yml.
.PHONY: verify-mutation-evidence
verify-mutation-evidence:
	@scripts/check-mutation-evidence.py
	@scripts/check-mutation-evidence.py --prove-teeth

# Model-check the broker lifecycle spec (T2.0/T2.8 oracle). The correct config
# must be green AND every fault-injection mutant must be caught — a spec that
# stays green on known-broken code is toothless. Requires Java + tla2tools.jar
# (see scripts/tlc.sh). CI runs this in .github/workflows/specs.yml.
.PHONY: verify-specs
verify-specs:
	@scripts/tlc.sh AgentLifecycle.tla AgentLifecycle.cfg >/dev/null && \
	 echo "✓ correct spec: no invariant violated"
	@if scripts/tlc.sh AgentLifecycle.tla AgentLifecycle_mutant_reap.cfg >/dev/null 2>&1; then \
	 echo "✗ mutant reap-while-held survived — Inv_NoHeldReap is toothless"; exit 1; \
	 else echo "✓ mutant reap-while-held caught by Inv_NoHeldReap"; fi
	@if scripts/tlc.sh AgentLifecycle.tla AgentLifecycle_mutant_steal.cfg >/dev/null 2>&1; then \
	 echo "✗ mutant steal-grant survived — Inv_NoDoubleOwnership is toothless"; exit 1; \
	 else echo "✓ mutant steal-grant caught by Inv_NoDoubleOwnership"; fi
	@if scripts/tlc.sh AgentLifecycle.tla AgentLifecycle_mutant_stale_handle.cfg >/dev/null 2>&1; then \
	 echo "✗ mutant stale-handle survived — Inv_NoSendAfterReap is toothless"; exit 1; \
	 else echo "✓ mutant stale-handle caught by Inv_NoSendAfterReap"; fi

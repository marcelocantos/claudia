# Standing invariants checked by bullseye_convergence.
# Exit 0 = all green; non-zero = at least one violation.
bullseye:
	@go vet ./... && echo "✓ vet"
	@# Never pipe this: `go test ... | tail` reports tail's status, not
	@# go test's, so a failing suite prints "✓ tests" and exits 0. That
	@# trap produced a false green here before.
	@go test -race -count=1 ./... && echo "✓ tests"
	@scripts/check-stability-surface.py >/dev/null && echo "✓ stability surface"
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

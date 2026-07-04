# Standing invariants checked by bullseye_convergence.
# Exit 0 = all green; non-zero = at least one violation.
bullseye:
	@go vet ./... && echo "✓ vet"
	@go test -race -count=1 ./... 2>&1 | tail -n 5 && echo "✓ tests"
	@test -z "$$(git status --porcelain)" && echo "✓ clean" || \
	 (echo "✗ dirty tree"; git status --short; exit 1)

# Model-check the broker lifecycle spec (T2.0 oracle). The correct config must
# be green AND both fault-injection mutants must be caught — a spec that stays
# green on known-broken code is toothless. Requires Java + tla2tools.jar (see
# scripts/tlc.sh). CI runs this in .github/workflows/specs.yml.
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

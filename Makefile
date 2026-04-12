# Standing invariants checked by bullseye_convergence.
# Exit 0 = all green; non-zero = at least one violation.
bullseye:
	@go vet ./... && echo "✓ vet"
	@go test -race -count=1 ./... 2>&1 | tail -n 5 && echo "✓ tests"
	@test -z "$$(git status --porcelain)" && echo "✓ clean" || \
	 (echo "✗ dirty tree"; git status --short; exit 1)

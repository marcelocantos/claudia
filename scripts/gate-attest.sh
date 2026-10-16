#!/bin/sh
# One process owns the snapshot, lock, checks and atomic attestation.
set -eu
exec env GOWORK=off GOFLAGS=-mod=readonly go run ./cmd/ownergate "$@"

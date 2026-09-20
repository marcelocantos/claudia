// Package gowalk shares the go command's own rule for which directories a
// `./...` pattern skips, so that the repo's hand-rolled tree walks read
// exactly the files the toolchain compiles.
//
// The two disagreeing is a shared-gate hazard (T99). An agent's scratch .go
// file under a visible directory is compiled by `go vet ./...` and read by
// every walk in the suite, which is how one seat's private experiment turned
// another seat's gate RED for reasons that were nobody's defect:
//
//	vet: scratchpad/agent-head.go:62:11: undefined: Provider
//
// In-repo scratch therefore lives under `_scratchpad/`, which the toolchain
// ignores, and the walks below agree with the toolchain about what that means.
// `.gitignore` is not part of this: the go command does not read it.
package gowalk

import "strings"

// IgnoredDir reports whether the go command skips a directory with this base
// name when matching a `./...` pattern: names beginning with "." or "_", and
// "testdata". The walk root itself ("." or "..") is never ignored.
//
// Walks over the repo's .go files should skip these directories, so a file the
// toolchain never compiles is a file the suite never reads.
func IgnoredDir(name string) bool {
	if name == "." || name == ".." {
		return false
	}
	return name == "testdata" ||
		strings.HasPrefix(name, ".") ||
		strings.HasPrefix(name, "_")
}

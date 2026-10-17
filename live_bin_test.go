// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"os/exec"
)

// liveCodexBin locates a Codex CLI for a live test's skip decision.
// resolveCodexBin refuses before any lookup.
func liveCodexBin() (string, error) {
	return resolveCodexBinFrom(os.Getenv, exec.LookPath, os.Stat, codexBinCandidates())
}

// liveGrokBin locates a Grok CLI for a live test's skip decision.
// resolveGrokBin refuses before any lookup.
func liveGrokBin() (string, error) {
	return resolveGrokBinFrom(os.Getenv, exec.LookPath, os.Stat, grokBinCandidates())
}

// liveCursorBin locates a Cursor agent CLI for a live test's skip decision.
// resolveCursorBin refuses before any lookup.
func liveCursorBin() (string, error) {
	return resolveCursorBinFrom(os.Getenv, exec.LookPath, os.Stat, cursorBinCandidates())
}

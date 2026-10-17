// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// t108ready used to measure a cold Claude Code seat in tmux (🎯T108).
// That transport is gone. The command refuses before it looks up claude
// or tmux, and before it spawns a window.
package main

import (
	"fmt"
	"os"

	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

func main() {
	if err := run(); err != nil {
		die("%v", err)
	}
}

// run refuses. It does not parse a sample count, look up claude, or
// call SpawnWindow.
func run() error {
	return fmt.Errorf("%w: t108ready", tmuxagent.ErrLegacyCLISeatTransport)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

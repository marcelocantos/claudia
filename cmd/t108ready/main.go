// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// t108ready used to measure a cold Claude Code seat in tmux (🎯T108).
// That transport is deleted. The command refuses before it looks up
// claude or tmux.
package main

import (
	"fmt"
	"os"

	"github.com/marcelocantos/claudia"
)

func main() {
	if err := run(); err != nil {
		die("%v", err)
	}
}

func run() error {
	return fmt.Errorf("%w: t108ready", claudia.ErrLegacyCLISeatTransport)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

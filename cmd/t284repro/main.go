// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// t284repro used to measure tmux readiness against a live Claude Code seat.
// That transport is deleted.
package main

import (
	"fmt"
	"os"

	"github.com/marcelocantos/claudia"
)

func main() {
	fmt.Fprintf(os.Stderr, "%v\n", fmt.Errorf("%w: t284repro", claudia.ErrLegacyCLISeatTransport))
	os.Exit(1)
}

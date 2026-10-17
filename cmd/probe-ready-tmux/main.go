// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// probe-ready-tmux used to spawn Claude Code in a tmux window and poll
// the pane for readiness. That transport is deleted.
package main

import (
	"fmt"
	"os"

	"github.com/marcelocantos/claudia"
)

func main() {
	fmt.Fprintf(os.Stderr, "%v\n", fmt.Errorf("%w: probe-ready-tmux", claudia.ErrLegacyCLISeatTransport))
	os.Exit(1)
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// t28size used to sweep paste sizes against a live Claude Code pane.
// That transport is deleted. The command refuses, and the paste helpers
// the hermetic test calls refuse before they create a file or run tmux.
package main

import (
	"fmt"
	"os"

	"github.com/marcelocantos/claudia"
)

func main() {
	fmt.Fprintf(os.Stderr, "%v\n", fmt.Errorf("%w: t28size", claudia.ErrLegacyCLISeatTransport))
	os.Exit(1)
}

func sendKey(windowID, key string) error {
	return fmt.Errorf("%w: send-key", claudia.ErrLegacyCLISeatTransport)
}

func pasteBuffer(windowID, msg string) error {
	return fmt.Errorf("%w: paste", claudia.ErrLegacyCLISeatTransport)
}

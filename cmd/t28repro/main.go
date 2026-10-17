// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// t28repro used to capture an unsubmitted composer on a tmux pane.
// That transport is deleted.
package main

import (
	"fmt"
	"os"

	"github.com/marcelocantos/claudia"
)

func main() {
	fmt.Fprintf(os.Stderr, "%v\n", fmt.Errorf("%w: t28repro", claudia.ErrLegacyCLISeatTransport))
	os.Exit(1)
}

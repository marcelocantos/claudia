// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// t30send used to drive a large paste through the tmux send path.
// That transport is deleted.
package main

import (
	"fmt"
	"os"

	"github.com/marcelocantos/claudia"
)

func main() {
	fmt.Fprintf(os.Stderr, "%v\n", fmt.Errorf("%w: t30send", claudia.ErrLegacyCLISeatTransport))
	os.Exit(1)
}

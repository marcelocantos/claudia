// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import "github.com/marcelocantos/claudia/internal/taskerror"

// ErrTaskCLIRemoved is returned before dispatch for the retired CLI Task transport.
// Subscription seats remain supported; no replacement Task engine is provided.
var ErrTaskCLIRemoved = taskerror.CLIRemoved

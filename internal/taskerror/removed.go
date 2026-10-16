// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package taskerror shares the removed-transport identity between compatibility APIs.
package taskerror

import "errors"

var CLIRemoved = errors.New("claudia: Task CLI transport removed; no OMP Task replacement is available (T192; T190)")

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package claudia

import "os"

// openConsumerIntentFileNoFollow opens path for reading. Windows has no
// POSIX FIFO concept at an ordinary filesystem path (named pipes live
// under \\.\pipe\ and are never what a consumer state dir points at), so
// the hang this guards against on unix cannot occur here; a plain Open
// is used and the caller's post-open Stat().Mode().IsRegular() check
// still refuses anything that is not a regular file (🎯T233.2 follow-up).
func openConsumerIntentFileNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}

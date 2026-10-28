// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package claudia

import (
	"os"
	"syscall"
)

// openConsumerIntentFileNoFollow opens path for reading with O_NOFOLLOW (a
// symlink at path fails the open with ELOOP, so it is never silently
// traversed) and O_NONBLOCK (opening a FIFO with no writer attached
// returns immediately instead of blocking the calling goroutine
// indefinitely — plain os.Open on such a FIFO would hang forever). Any
// other node type (directory, device, regular file) opens as normal; the
// caller still re-checks the resulting fd's own Stat().Mode() before
// trusting it as a regular file, since O_NOFOLLOW/O_NONBLOCK alone do not
// certify that (🎯T233.2 follow-up).
func openConsumerIntentFileNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

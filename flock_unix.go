// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package claudia

import (
	"os"
	"syscall"
)

// flockExclusive acquires an advisory exclusive lock on the given file,
// blocking until the lock is granted.
func flockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// flockUnlock releases the advisory lock acquired by flockExclusive.
func flockUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

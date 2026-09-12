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

// flockTryExclusive acquires an exclusive lock without blocking.
// ok is false when another process holds the lock.
func flockTryExclusive(f *os.File) (ok bool, err error) {
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
		return false, nil
	}
	return false, err
}

// flockUnlock releases the advisory lock acquired by flockExclusive.
func flockUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

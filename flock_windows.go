// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package claudia

import (
	"os"

	"golang.org/x/sys/windows"
)

// flockExclusive acquires an advisory exclusive lock on the given file using
// the Win32 LockFileEx API. The lock is released when the file handle is
// closed or flockUnlock is called.
func flockExclusive(f *os.File) error {
	// Lock the entire conceptual byte range. maxUint32 for low and high
	// parts of the offset gives us coverage for any realistic file size
	// without needing to Stat first.
	var ol windows.Overlapped
	const maxUint32 = ^uint32(0)
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		maxUint32,
		maxUint32,
		&ol,
	)
}

// flockUnlock releases the advisory lock acquired by flockExclusive.
func flockUnlock(f *os.File) error {
	var ol windows.Overlapped
	const maxUint32 = ^uint32(0)
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		maxUint32,
		maxUint32,
		&ol,
	)
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// readBoundedStateFile reads a consumer-state JSON file (fleet/intent.json,
// fleet/policy-hold.json) up to maxBytes, refusing anything that is not a
// plain regular file. present reports whether a readable file was found at
// all: false for a missing file, a non-regular node (symlink, FIFO,
// device, directory), or anything over the size bound. Callers enforcing a
// hold (🎯T233.2) MUST fail closed when present is false.
//
// openStateFileNoFollow (unix: O_NOFOLLOW|O_NONBLOCK) is the one syscall
// that both decides and acts: O_NOFOLLOW fails the open with ELOOP on a
// symlink instead of traversing it, and O_NONBLOCK means opening a FIFO
// with no writer attached returns immediately rather than blocking this
// goroutine forever — there is no separate Lstat-then-Open TOCTOU window
// to close here because there is no separate Lstat; the single open call
// is both the check and the use. The trust boundary this still rests on
// is the consumer state dir being private to its writer and this broker,
// not an attacker-writable path; that is a deployment property, not
// something this function can enforce.
func readBoundedStateFile(path string, maxBytes int64) (raw []byte, present bool, err error) {
	f, err := openStateFileNoFollow(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	// The fd's own Stat is what gets size- and type-checked, not the
	// path's: whatever the open actually attached to is what gets read.
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s: refusing non-regular file (mode %v)", path, info.Mode())
	}
	if info.Size() > maxBytes {
		return nil, false, fmt.Errorf("%s: %d bytes exceeds %d byte limit", path, info.Size(), maxBytes)
	}
	// Bounded read regardless of what Stat claimed: a file that grows
	// between stat and read (e.g. a writer appending in place rather than
	// atomically renaming) is cut off at the limit rather than read
	// without bound.
	raw, err = io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, false, fmt.Errorf("%s: exceeds %d byte limit while reading", path, maxBytes)
	}
	return raw, true, nil
}

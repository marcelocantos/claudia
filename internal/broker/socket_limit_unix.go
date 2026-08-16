// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package broker

import "syscall"

// maxSocketPathLen is the kernel's sun_path capacity, taken from the platform's
// own socket address struct rather than written down: it is 104 on the BSDs and
// macOS and 108 on Linux, and a hardcoded guess would either reject paths that
// work or admit paths that fail the bind. The expression is a compile-time
// constant — len of an array-typed expression with no function call in it.
const maxSocketPathLen = len(syscall.RawSockaddrUnix{}.Path)

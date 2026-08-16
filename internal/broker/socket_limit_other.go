// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package broker

// maxSocketPathLen mirrors the AF_UNIX sun_path capacity on platforms whose
// syscall package does not expose the address struct. Windows implements AF_UNIX
// with the same 108-byte field Linux uses. This is the permissive of the two
// limits, so a path that passes here can still fail the bind — which is why
// Listen reports the bind error rather than assuming this check was sufficient.
const maxSocketPathLen = 108

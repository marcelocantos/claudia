// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import "os"

// ResolveArgs is retained so a caller can prove Run performs no binary
// or credential discovery. The CLI resolver and the subscription auth
// preflight are deleted. Nil is valid. Prefer a struct over functional
// options.
type ResolveArgs struct {
	// BinPath used to force a codex binary path. Run ignores it.
	BinPath string
	// AuthPath used to override ~/.codex/auth.json. Run ignores it.
	AuthPath string
	// Getenv used to override os.Getenv. Run ignores it.
	Getenv func(string) string
	// LookPath used to override exec.LookPath. Run ignores it.
	LookPath func(string) (string, error)
	// Stat used to override os.Stat. Run ignores it.
	Stat func(string) (os.FileInfo, error)
	// SkipAuthPreflight used to skip subscription auth. Run ignores it.
	SkipAuthPreflight bool
}

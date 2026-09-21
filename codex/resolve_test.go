// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	"errors"
	"os"
	"testing"
)

// TestResolveBinSkipsCmuxShimOnPATH: cmux puts a "codex" shim on PATH that
// prints "codex not found in PATH" and exits 127. A plain PATH lookup finds
// it first; resolution must reach the real binary in a known install dir
// (🎯T126). Restoring PATH-first resolution turns this red.
func TestResolveBinSkipsCmuxShimOnPATH(t *testing.T) {
	const shim = "/tmp/cmux-cli-shims/codex"
	const real = "/Applications/ChatGPT.app/Contents/Resources/codex"
	got, err := resolveBin(&ResolveArgs{
		Getenv:   func(string) string { return "" },
		LookPath: func(string) (string, error) { return shim, nil },
		Stat: func(p string) (os.FileInfo, error) {
			if p == real {
				return nil, nil
			}
			return nil, errors.New("absent")
		},
	})
	if err != nil || got != real {
		t.Fatalf("resolveBin = %q, %v; want %q", got, err, real)
	}
}

// TestResolveBinRefusesShimAsOnlyCandidate: with nothing but the shim
// available the resolver reports no binary, so the live test skips with a
// reason instead of failing on exit 127.
func TestResolveBinRefusesShimAsOnlyCandidate(t *testing.T) {
	_, err := resolveBin(&ResolveArgs{
		Getenv:   func(string) string { return "" },
		LookPath: func(string) (string, error) { return "/tmp/cmux-cli-shims/codex", nil },
		Stat:     func(string) (os.FileInfo, error) { return nil, errors.New("absent") },
	})
	if err == nil {
		t.Fatal("shim accepted as the codex binary")
	}
}

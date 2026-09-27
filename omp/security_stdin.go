// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// ExecSecurityStdin runs security -q -i with the Keychain command on stdin.
// It never puts that input in argv and does not echo it in errors (🎯T131).
func ExecSecurityStdin(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if name != "security" || len(args) != 2 || args[0] != "-q" || args[1] != "-i" {
		return nil, fmt.Errorf("omp: refusing unexpected Keychain stdin command")
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.Env = ScrubEnv(os.Environ())
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("omp: keychain ACL interaction required: %w", ctx.Err())
		}
		return nil, fmt.Errorf("omp: Keychain stdin write failed: %w", err)
	}
	return nil, nil
}

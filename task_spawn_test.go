// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// writeFakeCLI writes an executable that prints fixturePath to stdout and
// exits with exitCode. It ignores argv so production arg construction does
// not need to match a real CLI. Used to exercise production RunTask spawn
// without network or a real coding agent.
func writeFakeCLI(t *testing.T, fixturePath string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("hermetic fake CLI spawn tests use a POSIX shell script")
	}
	absFixture, err := filepath.Abs(fixturePath)
	if err != nil {
		t.Fatalf("Abs(%s): %v", fixturePath, err)
	}
	if _, err := os.Stat(absFixture); err != nil {
		t.Fatalf("fixture %s: %v", absFixture, err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-agent-cli")
	// #!/bin/sh — portable on macOS CI (bash 3.2) and Linux.
	script := "#!/bin/sh\n" +
		"cat \"" + absFixture + "\"\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile fake CLI: %v", err)
	}
	return bin
}

func drainTaskEvents(t *testing.T, events <-chan TaskEvent) []TaskEvent {
	t.Helper()
	var got []TaskEvent
	for ev := range events {
		got = append(got, ev)
	}
	return got
}

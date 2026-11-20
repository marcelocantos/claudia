// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// TestStopEndsTheSeatsToolProcess: Agent.Stop (which Registry.Stop calls
// for every stop that is not a removal) on a seat whose turn is running a
// Bash tool. Once Stop has returned nobody is attached to the seat; the
// tool's process must not keep running.
func TestStopEndsTheSeatsToolProcess(t *testing.T) {
	ctx, dir := startAbortSidecar(t)
	a, err := StartDirect(Config{Name: "hunt-stop", Provider: Provider(omp.Anthropic), Model: "claude-sonnet-5",
		WorkDir: dir, TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "child.pid")
	if err := a.Send("BASH_CANCEL " + marker); err != nil {
		t.Fatal(err)
	}
	var pid int
	waitFor(t, "Bash tool child running", func() bool {
		raw, err := os.ReadFile(marker)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && pid > 1
	})
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	a.Stop()
	deadline := time.Now().Add(time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("Stop returned and nothing is attached to the seat, but its Bash tool (pid %d) is still running", pid)
	}
}

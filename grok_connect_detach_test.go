// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDetachedServeSurvivesSpawnerExit is the 🎯T40 process-durability
// oracle at the OS layer: a Setsid-detached long-lived child must still
// be alive after the process that started it has exited (without
// waiting/killing). Uses `sleep` so the suite stays hermetic (no Grok).
func TestDetachedServeSurvivesSpawnerExit(t *testing.T) {
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not on PATH")
	}
	// Helper child: detach sleep, print PID, exit 0 without Wait/Kill.
	helper := filepath.Join(t.TempDir(), "spawn_detach")
	src := `package main
import (
  "fmt"
  "os"
  "os/exec"
  "syscall"
)
func main() {
  cmd := exec.Command(os.Args[1], "30")
  cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
  if err := cmd.Start(); err != nil { panic(err) }
  fmt.Println(cmd.Process.Pid)
  _ = cmd.Process.Release()
  os.Exit(0)
}
`
	if err := os.WriteFile(helper+".go", []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// Build helper
	out := filepath.Join(t.TempDir(), "helper")
	build := exec.Command("go", "build", "-o", out, helper+".go")
	build.Env = os.Environ()
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v\n%s", err, b)
	}
	run := exec.Command(out, sleepBin)
	stdout, err := run.Output()
	if err != nil {
		t.Fatalf("run helper: %v", err)
	}
	pidStr := strings.TrimSpace(string(stdout))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		t.Fatalf("pid %q: %v", pidStr, err)
	}
	// Spawner is gone; sleep must still be alive.
	time.Sleep(100 * time.Millisecond)
	if !processAlive(pid) {
		t.Fatalf("detached pid %d died after spawner exit", pid)
	}
	// Cleanup
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

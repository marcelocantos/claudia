// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

// isolateTmux points CLAUDIA_TMUX_SOCKET at a short temp path and
// tears the server down. t.TempDir() is too long for sun_path.
func isolateTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sockDir, err := os.MkdirTemp("", "t34")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s.sock")
	t.Setenv("CLAUDIA_TMUX_SOCKET", sock)
	if err := tmuxagent.EnsureServer(); err != nil {
		t.Skipf("cannot start an isolated tmux server: %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run() })
}

func stubClaude(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 3600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_BIN", bin)
}

func sessionWindows(t *testing.T, sessionID string) []string {
	t.Helper()
	ids, err := tmuxagent.WindowsForSession(sessionID)
	if err != nil {
		t.Fatalf("WindowsForSession: %v", err)
	}
	return ids
}

// TestStopKillsWindowWithoutHandle is the 🎯T34 proof that Stop is not
// a silent no-op after a restart: the Registry never launched the
// agent, so r.procs is empty, and the window still dies.
func TestStopKillsWindowWithoutHandle(t *testing.T) {
	isolateTmux(t)

	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	windowID, err := tmuxagent.SpawnWindow(t.TempDir(), tmuxagent.SessionWindowName(sid), "cat", nil)
	if err != nil {
		t.Fatalf("SpawnWindow: %v", err)
	}
	if err := tmuxagent.SetWindowOption(windowID, "claudia-session-id", sid); err != nil {
		t.Fatalf("SetWindowOption: %v", err)
	}

	path := filepath.Join(t.TempDir(), "agents.json")
	r, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(AgentDef{
		Name:      "ghost",
		WorkDir:   t.TempDir(),
		SessionID: sid,
		AutoStart: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Fresh registry: simulates a consumer that loaded agents.json
	// and never called Launch.
	r2, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Get("ghost") != nil {
		t.Fatal("fresh registry already holds a handle")
	}
	r2.Stop("ghost")

	if tmuxagent.IsWindowAlive(windowID) {
		t.Fatal("Stop on a handle-less Registry left the window alive")
	}
	if left := sessionWindows(t, sid); len(left) != 0 {
		t.Fatalf("leftover windows after Stop: %v", left)
	}
}

// TestLaunchRecreatesRatherThanAdopts: a leftover window for the
// session is killed, then a new process is spawned. Count stays 1.
func TestLaunchRecreatesRatherThanAdopts(t *testing.T) {
	isolateTmux(t)
	stubClaude(t)

	sid := "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	old, err := tmuxagent.SpawnWindow(t.TempDir(), tmuxagent.SessionWindowName(sid), "cat", nil)
	if err != nil {
		t.Fatalf("SpawnWindow leftover: %v", err)
	}
	if err := tmuxagent.SetWindowOption(old, "claudia-session-id", sid); err != nil {
		t.Fatal(err)
	}

	r, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(AgentDef{
		Name:      "worker",
		WorkDir:   t.TempDir(),
		SessionID: sid,
		AutoStart: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Launch("worker"); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { r.Stop("worker") })

	if tmuxagent.IsWindowAlive(old) {
		t.Fatal("Launch adopted the leftover window instead of reaping it")
	}
	if got := sessionWindows(t, sid); len(got) != 1 {
		t.Fatalf("windows for session after Launch: %v, want exactly 1", got)
	}
}

// TestRestartStartAllStopAllLeavesZero is the 🎯T34 CI oracle:
// crash the consumer (drop the Registry), rematerialise from disk,
// then StopAll. Nothing survives.
func TestRestartStartAllStopAllLeavesZero(t *testing.T) {
	isolateTmux(t)
	stubClaude(t)

	path := filepath.Join(t.TempDir(), "agents.json")
	r, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}

	sids := []string{
		"11111111-2222-3333-4444-555555555555",
		"66666666-7777-8888-9999-aaaaaaaaaaaa",
	}
	for i, sid := range sids {
		name := []string{"one", "two"}[i]
		if err := r.Register(AgentDef{
			Name:      name,
			WorkDir:   t.TempDir(),
			SessionID: sid,
			AutoStart: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	r.StartAll()
	// Crash: drop r without StopAll. Windows stay up.

	r2, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	r2.StartAll()
	for _, sid := range sids {
		if got := sessionWindows(t, sid); len(got) != 1 {
			t.Fatalf("after rematerialise, session %s has %v, want 1", sid, got)
		}
	}
	r2.StopAll()

	for _, sid := range sids {
		if got := sessionWindows(t, sid); len(got) != 0 {
			t.Fatalf("after StopAll, session %s still has %v", sid, got)
		}
	}
}

// TestAdoptReusesWindow: upgrade boot. Drop the Registry, Adopt, same
// window, count stays 1.
func TestAdoptReusesWindow(t *testing.T) {
	isolateTmux(t)
	stubClaude(t)

	path := filepath.Join(t.TempDir(), "agents.json")
	r, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	sid := "cccccccc-dddd-eeee-ffff-000000000001"
	if err := r.Register(AgentDef{
		Name:      "keep",
		WorkDir:   t.TempDir(),
		SessionID: sid,
		AutoStart: true,
	}); err != nil {
		t.Fatal(err)
	}
	first, err := r.Launch("keep")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	old := first.WindowID()
	if old == "" {
		t.Fatal("Launch did not record a window id")
	}

	r2, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r2.Adopt("keep")
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	t.Cleanup(func() { r2.Stop("keep") })
	if second.WindowID() != old {
		t.Fatalf("Adopt window %q, want leftover %q", second.WindowID(), old)
	}
	if got := sessionWindows(t, sid); len(got) != 1 || got[0] != old {
		t.Fatalf("windows after Adopt: %v, want [%s]", got, old)
	}
}

func TestAdoptFailsWhenGone(t *testing.T) {
	isolateTmux(t)
	r, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(AgentDef{
		Name:      "gone",
		WorkDir:   t.TempDir(),
		SessionID: "dddddddd-eeee-ffff-0000-111111111111",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = r.Adopt("gone")
	if !errors.Is(err, ErrNoSessionWindow) {
		t.Fatalf("Adopt missing window: %v", err)
	}
}

func TestAdoptOrLaunchFallsBackToLaunch(t *testing.T) {
	isolateTmux(t)
	stubClaude(t)
	r, err := NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	sid := "eeeeeeee-ffff-0000-1111-222222222222"
	if err := r.Register(AgentDef{
		Name:      "fresh",
		WorkDir:   t.TempDir(),
		SessionID: sid,
		AutoStart: true,
	}); err != nil {
		t.Fatal(err)
	}
	a, err := r.AdoptOrLaunch("fresh")
	if err != nil {
		t.Fatalf("AdoptOrLaunch: %v", err)
	}
	t.Cleanup(func() { r.Stop("fresh") })
	if a.WindowID() == "" {
		t.Fatal("fallback Launch produced no window")
	}
	if got := sessionWindows(t, sid); len(got) != 1 {
		t.Fatalf("windows: %v", got)
	}
}

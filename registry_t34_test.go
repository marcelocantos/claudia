// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// TestStopKillsWindowWithoutHandle: Stop on a handle-less seat does not
// start tmux to reap a CLI window.
func TestStopKillsWindowWithoutHandle(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "t34")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s.sock")
	t.Setenv("CLAUDIA_TMUX_SOCKET", sock)

	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
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
	r2, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Get("ghost") != nil {
		t.Fatal("fresh registry already holds a handle")
	}
	r2.Stop("ghost")
	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Fatalf("Stop created a tmux socket: %v", statErr)
	}
}

// TestLaunchDoesNotReapLeftovers: a seat Launch does not exec a CLI and
// does not create a tmux socket. The stub claude touches a marker if a
// regression starts it.
func TestLaunchDoesNotReapLeftovers(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "t34")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s.sock")
	t.Setenv("CLAUDIA_TMUX_SOCKET", sock)
	marker := filepath.Join(sockDir, "ran")
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_BIN", bin)

	sid := "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
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
	if _, err := r.Launch("worker"); err == nil || !strings.Contains(err.Error(), "never uses the real sidecar socket") {
		t.Fatalf("Launch = %v, want the sidecar test refusal", err)
	}
	t.Cleanup(func() { r.Stop("worker") })
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("Launch executed claude: %v", statErr)
	}
	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Fatalf("Launch created a tmux socket: %v", statErr)
	}
}

// TestStopAllKillsWhatThisProcessStarted: StartAll of a fleet seat does
// not exec a CLI. StopAll still leaves no window. The stub claude is on
// PATH so a regression back to tmux spawn fails this before StopAll.
func TestStopAllKillsWhatThisProcessStarted(t *testing.T) {
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
	for _, sid := range sids {
		if got := sessionWindows(t, sid); len(got) != 0 {
			t.Fatalf("StartAll spawned %v for %s", got, sid)
		}
	}
	r.StopAll()

	for _, sid := range sids {
		if got := sessionWindows(t, sid); len(got) != 0 {
			t.Fatalf("after StopAll, session %s still has %v", sid, got)
		}
	}
}

// TestAdoptReusesWindow: upgrade boot must not attach a CLI pane. Launch
// and Adopt both refuse without a sidecar, and neither creates a window.
// The stub claude is on PATH so a regression back to tmux spawn fails.
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
	if _, err := r.Launch("keep"); err == nil || !strings.Contains(err.Error(), "never uses the real sidecar socket") {
		t.Fatalf("Launch = %v, want the sidecar test refusal", err)
	}

	r2, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r2.Adopt("keep")
	if !errors.Is(err, ErrNoSessionWindow) {
		t.Fatalf("Adopt = %v, want ErrNoSessionWindow", err)
	}
	t.Cleanup(func() { r2.Stop("keep") })
	if got := sessionWindows(t, sid); len(got) != 0 {
		t.Fatalf("windows after Adopt: %v, want none", got)
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
	if _, err := r.AdoptOrLaunch("fresh"); err == nil || !strings.Contains(err.Error(), "never uses the real sidecar socket") {
		t.Fatalf("AdoptOrLaunch = %v, want the sidecar test refusal", err)
	}
	t.Cleanup(func() { r.Stop("fresh") })
	if got := sessionWindows(t, sid); len(got) != 0 {
		t.Fatalf("windows: %v, want none", got)
	}
}

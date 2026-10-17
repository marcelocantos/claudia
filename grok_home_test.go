// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExclusiveGrokHomeRejectsMissingInvalidAndCollidingState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, sid := range []string{"", "..", "../escape", "missing"} {
		if _, err := exclusiveGrokHomeForStart(sid, true); err == nil {
			t.Errorf("required resume accepted %q", sid)
		}
	}
	one, err := exclusiveGrokHomeForStart("one", false)
	if err != nil {
		t.Fatal(err)
	}
	two, err := exclusiveGrokHomeForStart("two", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishExclusiveGrokHome(one, "actual"); err != nil {
		t.Fatal(err)
	}
	if err := publishExclusiveGrokHome(one, "actual"); err != nil {
		t.Fatal(err)
	}
	if err := publishExclusiveGrokHome(two, "actual"); err == nil {
		t.Fatal("overwrote another conversation home")
	}
	bad := exclusiveGrokHomeDir("not-a-directory")
	if err := os.WriteFile(bad, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := exclusiveGrokHomeForStart("not-a-directory", true); err == nil {
		t.Fatal("accepted non-directory home")
	}
	if err := os.Symlink(filepath.Join(one, "absent"), exclusiveGrokHomeDir("dangling")); err != nil {
		t.Fatal(err)
	}
	if _, err := exclusiveGrokHomeForStart("dangling", true); err == nil {
		t.Fatal("accepted dangling required home")
	}
	if err := publishExclusiveGrokHome(one, "dangling"); err == nil {
		t.Fatal("overwrote dangling mapping")
	}
}

func TestExclusiveGrokHomeRefreshKeepsConversationAndExcludesUserMCP(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	user := t.TempDir()
	t.Setenv("HOME", user)
	if err := os.Mkdir(filepath.Join(user, ".grok"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, ".grok", "auth.json"), []byte(`{"fake":"auth"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, ".grok", "config.toml"), []byte("user-mcp-secret-marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	home, err := exclusiveGrokHomeForStart("session", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "session-data"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := exclusiveGrokHomeForStart("session", true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil || strings.Contains(string(b), "user-mcp-secret-marker") || strings.Count(string(b), "mcps = false") != 2 {
		t.Fatalf("isolation config: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(home, "session-data")); err != nil || string(b) != "keep" {
		t.Fatalf("refresh lost session: %q %v", b, err)
	}
}

func TestExclusiveGrokHomeRejectsRedirectionWithoutChangingVictim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	victim := t.TempDir()
	victimFile := filepath.Join(victim, "config.toml")
	if err := os.WriteFile(victimFile, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	home, err := exclusiveGrokHomeForStart("safe", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, exclusiveGrokHomeDir("escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := exclusiveGrokHomeForStart("escape", true); err == nil {
		t.Fatal("accepted external home alias")
	}
	config := filepath.Join(home, "config.toml")
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victimFile, config); err != nil {
		t.Fatal(err)
	}
	if _, err := exclusiveGrokHomeForStart("safe", true); err == nil {
		t.Fatal("accepted redirected configuration")
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(victimFile, config); err != nil {
		t.Fatal(err)
	}
	if _, err := exclusiveGrokHomeForStart("safe", true); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victimFile, filepath.Join(home, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := exclusiveGrokHomeForStart("safe", true); err == nil {
		t.Fatal("accepted redirected auth without a user auth source")
	}
	if b, err := os.ReadFile(victimFile); err != nil || string(b) != "untouched" {
		t.Fatalf("modified victim: %q %v", b, err)
	}
}

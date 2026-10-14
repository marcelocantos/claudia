// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"testing"
)

// Registry.Remove used to reap a Claude seat's tmux windows, and asking
// tmux about them started a claudia-anchor server every time: the source
// of hundreds of orphaned anchor servers. Seats run on the sidecar now;
// removing one must not run tmux at all.
func TestRegistryRemoveStartsNoTmux(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "tmux-ran")
	script := "#!/bin/sh\necho \"$@\" >> " + marker + "\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	r, err := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Provider{"", ProviderClaude, Provider("anthropic")} {
		name := "rm-" + string(p)
		if err := r.Register(AgentDef{Name: name, WorkDir: t.TempDir(), SessionID: "sid-" + name, Provider: p}); err != nil {
			t.Fatal(err)
		}
		if err := r.Remove(name); err != nil {
			t.Fatalf("Remove(%s): %v", name, err)
		}
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("Registry.Remove ran tmux: %s", b)
	}
}

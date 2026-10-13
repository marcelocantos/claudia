// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// Every sidecar seat maps sandbox_policy onto the sidecar's tools, not just
// Codex. Refusing it for claude stranded ge-po on resume after 🎯T178.
func TestOMPSidecarSeatsMapSandboxPolicy(t *testing.T) {
	repo := t109Repo(t)
	gitDir := filepath.Join(repo, ".git")
	for _, p := range []Provider{Provider(omp.Anthropic), ProviderClaude, ProviderCursor, ProviderGrok, Provider(omp.XAIOAuth)} {
		t.Run(string(p), func(t *testing.T) {
			if sb, err := ompSandboxFor(Config{Provider: p, WorkDir: repo}); err != nil || sb != nil {
				t.Fatalf("no settings = %+v, %v; want unconfined", sb, err)
			}
			sb, err := ompSandboxFor(Config{Provider: p, WorkDir: repo, SandboxMode: "read-only"})
			if err != nil || sb == nil || sb.Mode != "read-only" {
				t.Fatalf("read-only = %+v, %v", sb, err)
			}
			sb, err = ompSandboxFor(Config{Provider: p, WorkDir: repo, SandboxMode: "workspace-write"})
			if err != nil || sb == nil || sb.Mode != "workspace-write" || !samePaths(sb.Protected, []string{gitDir}) {
				t.Fatalf("workspace-write = %+v, %v", sb, err)
			}
			sb, err = ompSandboxFor(Config{Provider: p, WorkDir: repo, SandboxMode: "workspace-write",
				SandboxWritableRoots: []string{"/tmp/gates"}, SandboxNetworkAccess: true, SandboxGitWrite: true})
			if err != nil || sb == nil || !samePaths(sb.WritableRoots, []string{"/tmp/gates", bullseyeLedgerRoot(), gitDir}) || len(sb.Protected) != 0 {
				t.Fatalf("ge-po shape = %+v, %v", sb, err)
			}
		})
	}
}

// ge-po's grant: an anthropic seat with workspace-write, writable roots,
// network and git write. Launch (resume) and adopt both bring it up on the
// sidecar with the policy, instead of refusing it.
func TestOMPAnthropicSeatWithSandboxPolicyResumes(t *testing.T) {
	for _, how := range []string{"launch", "adopt-or-launch"} {
		t.Run(how, func(t *testing.T) {
			f := startOMPFixture(t)
			repo := t109Repo(t)
			r, err := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Register(AgentDef{
				Name: "ge-po", WorkDir: repo, SessionID: "sid-gepo", Provider: Provider(omp.Anthropic), Model: "claude-sonnet-5",
				SandboxMode: "workspace-write", SandboxWritableRoots: []string{"/tmp/gates"},
				SandboxNetworkAccess: true, SandboxGitWrite: true,
			}); err != nil {
				t.Fatal(err)
			}
			var agent *Agent
			if how == "launch" {
				agent, err = r.Launch("ge-po")
			} else {
				agent, err = r.AdoptOrLaunch("ge-po")
			}
			if err != nil {
				t.Fatalf("%s: %v", how, err)
			}
			defer agent.Stop()
			load := f.nextLoad(t)
			if load.Provider != omp.Anthropic || load.SessionID != "sid-gepo" || load.Sandbox == nil ||
				load.Sandbox.Mode != "workspace-write" ||
				!samePaths(load.Sandbox.WritableRoots, []string{"/tmp/gates", bullseyeLedgerRoot(), filepath.Join(repo, ".git")}) {
				t.Fatalf("load = provider %q session %q sandbox %+v", load.Provider, load.SessionID, load.Sandbox)
			}
		})
	}
}

// Workspace-write sidecar seats may write the bullseye ledger, and nothing
// broader; read-only seats get no writable root at all.
func TestOMPWorkspaceWriteAddsBullseyeLedgerRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".local", "share", "bullseye")
	if got := bullseyeLedgerRoot(); got != want {
		t.Fatalf("bullseyeLedgerRoot = %q, want %q", got, want)
	}
	dir := t.TempDir()
	for _, p := range []Provider{Provider(omp.Anthropic), ProviderCodex, ProviderCursor, ProviderGrok} {
		t.Run(string(p), func(t *testing.T) {
			sb, err := ompSandboxFor(Config{Provider: p, WorkDir: dir, SandboxMode: "workspace-write"})
			if err != nil || sb == nil || len(sb.WritableRoots) != 1 || sb.WritableRoots[0] != want {
				t.Fatalf("workspace-write = %+v, %v; want only %s", sb, err, want)
			}
			sb, err = ompSandboxFor(Config{Provider: p, WorkDir: dir, SandboxMode: "workspace-write", SandboxWritableRoots: []string{want}})
			if err != nil || len(sb.WritableRoots) != 1 {
				t.Fatalf("explicit root duplicated: %+v, %v", sb, err)
			}
			sb, err = ompSandboxFor(Config{Provider: p, WorkDir: dir, SandboxMode: "read-only"})
			if err != nil || sb == nil || sb.Mode != "read-only" || len(sb.WritableRoots) != 0 {
				t.Fatalf("read-only = %+v, %v; want no writable roots", sb, err)
			}
		})
	}
	if sb, _ := ompSandboxFor(Config{Provider: ProviderCodex, WorkDir: dir}); sb == nil || len(sb.WritableRoots) != 0 {
		t.Fatalf("bare codex (read-only default) = %+v", sb)
	}
}

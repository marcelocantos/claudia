// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// 🎯T178: Codex sandbox settings map onto the sidecar's tool policy.
func TestOMPCodexSandboxMapping(t *testing.T) {
	repo := t109Repo(t)
	gitDir := filepath.Join(repo, ".git")
	plain := t.TempDir()
	bl := bullseyeLedgerRoot()
	for _, tc := range []struct {
		name string
		cfg  Config
		want *omp.Sandbox
	}{
		{"bare codex default is read-only", Config{Provider: ProviderCodex, WorkDir: repo}, &omp.Sandbox{Mode: "read-only"}},
		{"subscription id without settings is untouched", Config{Provider: Provider(omp.OpenAICodex), WorkDir: repo}, nil},
		{"read-only", Config{Provider: ProviderCodex, WorkDir: repo, SandboxMode: "read-only"}, &omp.Sandbox{Mode: "read-only"}},
		{"workspace-write keeps git protected", Config{Provider: ProviderCodex, WorkDir: repo, SandboxMode: "workspace-write"},
			&omp.Sandbox{Mode: "workspace-write", WritableRoots: []string{bl}, Protected: []string{gitDir}}},
		{"git write adds the git dir", Config{Provider: ProviderCodex, WorkDir: repo, SandboxMode: "workspace-write", SandboxGitWrite: true, ApprovalPolicy: "never"},
			&omp.Sandbox{Mode: "workspace-write", WritableRoots: []string{bl, gitDir}}},
		{"writable roots carried", Config{Provider: Provider(omp.OpenAICodex), WorkDir: plain, SandboxMode: "workspace-write", SandboxWritableRoots: []string{"/tmp/x"}},
			&omp.Sandbox{Mode: "workspace-write", WritableRoots: []string{"/tmp/x", bl}}},
		{"full access is unconfined", Config{Provider: ProviderCodex, WorkDir: repo, SandboxMode: "danger-full-access"}, nil},
		{"not codex", Config{Provider: ProviderGrok, WorkDir: repo}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ompSandboxFor(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if got == nil {
				return
			}
			if got.Mode != tc.want.Mode || !slices.Equal(got.WritableRoots, tc.want.WritableRoots) || !samePaths(got.Protected, tc.want.Protected) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func samePaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ra, _ := filepath.EvalSymlinks(a[i])
		rb, _ := filepath.EvalSymlinks(b[i])
		if ra != rb {
			return false
		}
	}
	return true
}

// Settings the sidecar cannot honour are refused, named, before any dial.
func TestOMPCodexUnsupportedSettingsFailClosed(t *testing.T) {
	t.Setenv(omp.SocketEnv, filepath.Join(t.TempDir(), "missing.sock"))
	for _, provider := range []Provider{ProviderCodex, Provider(omp.OpenAICodex)} {
		for _, tc := range []struct {
			name, field string
			set         func(*Config)
		}{
			{"approval on-request", "ApprovalPolicy", func(c *Config) { c.ApprovalPolicy = "on-request" }},
			{"unknown mode", "SandboxMode", func(c *Config) { c.SandboxMode = "bogus" }},
			{"read-only with git write", "SandboxGitWrite", func(c *Config) { c.SandboxMode = "read-only"; c.SandboxGitWrite = true }},
		} {
			t.Run(string(provider)+"/"+tc.name, func(t *testing.T) {
				cfg := Config{Provider: provider, Model: "gpt-6-sol", WorkDir: t.TempDir()}
				tc.set(&cfg)
				if _, err := StartDirectContext(t.Context(), cfg); !errors.Is(err, ErrOMPCodexSandboxUnsupported) {
					t.Fatalf("StartDirect accepted: %v", err)
				}
				_, err := (ompAgentBackend{}).StartAgent(agentStartRequest{Context: t.Context(), Config: cfg})
				if !errors.Is(err, ErrOMPCodexSandboxUnsupported) || !strings.Contains(err.Error(), tc.field) {
					t.Fatalf("backend error = %v, want named refusal of %s", err, tc.field)
				}
			})
		}
	}
}

// The policy reaches the sidecar on load, the seat works, and a reload
// sends it again.
func TestOMPCodexLoadCarriesSandbox(t *testing.T) {
	f := startOMPFixture(t)
	repo := t109Repo(t)
	agent, err := StartDirect(Config{
		Name: "codex-ws", Provider: ProviderCodex, WorkDir: repo, Model: "gpt-6-sol",
		SandboxMode: "workspace-write", SandboxGitWrite: true, ApprovalPolicy: "never", TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	load := f.nextLoad(t)
	if load.Provider != omp.OpenAICodex || load.Sandbox == nil || load.Sandbox.Mode != "workspace-write" ||
		!samePaths(load.Sandbox.WritableRoots, []string{bullseyeLedgerRoot(), filepath.Join(repo, ".git")}) || len(load.Sandbox.Protected) != 0 {
		t.Fatalf("load = provider %q sandbox %+v", load.Provider, load.Sandbox)
	}
	if err := agent.Send("hello"); err != nil {
		t.Fatal(err)
	}
	got, err := agent.WaitForResponse(context.Background())
	if err != nil || got != "fixture-ok" {
		t.Fatalf("WaitForResponse = %q, %v", got, err)
	}
	ctrl, ok := agent.tmuxCtrl.(*ompControl)
	if !ok {
		t.Fatalf("control is %T, want *ompControl", agent.tmuxCtrl)
	}
	if re := ctrl.loadMessage(); re.Sandbox == nil || re.Sandbox.Mode != "workspace-write" {
		t.Fatalf("reload sandbox = %+v", re.Sandbox)
	}
}

// A sidecar that does not confirm the policy is not trusted with the seat.
func TestOMPSandboxNotHonouredRefuses(t *testing.T) {
	f := startOMPFixture(t)
	f.noSandboxEcho.Store(true)
	_, err := StartDirect(Config{Name: "codex-ro", Provider: ProviderCodex, WorkDir: t.TempDir(), Model: "gpt-6-sol", TermLogPath: "-"})
	if !errors.Is(err, ErrOMPSandboxNotHonoured) {
		t.Fatalf("Start = %v, want ErrOMPSandboxNotHonoured", err)
	}
}

// A Codex Session seat runs on the sidecar (🎯T178), not `codex app-server`.
func TestStartCodexSessionUsesSidecar(t *testing.T) {
	f := startOMPFixture(t)
	t.Setenv("CODEX_BIN", "/nonexistent/codex-must-not-run")
	agent, err := Start(Config{Provider: ProviderCodex, WorkDir: t.TempDir(), Model: "gpt-6-sol", TermLogPath: "-"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	if agent.SessionID() == "" {
		t.Fatal("empty SessionID after Codex Start")
	}
	if load := f.nextLoad(t); load.Provider != omp.OpenAICodex {
		t.Fatalf("load provider = %q", load.Provider)
	}
}

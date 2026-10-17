// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/internal/tmuxagent"
	"github.com/marcelocantos/claudia/omp"
)

// TestT1055SessionDispatchIsSidecar drives the backend Start and Registry.Launch
// select. Fleet ids stay on the sidecar. The CLI fallback refuses.
func TestT1055SessionDispatchIsSidecar(t *testing.T) {
	ids := []Provider{
		"", ProviderClaude, ProviderCodex, ProviderGrok, ProviderCursor,
		Provider(omp.Anthropic), Provider(omp.OpenAICodex), Provider(omp.XAIOAuth),
	}
	for _, id := range ids {
		backend := agentBackendFor(Config{Provider: id})
		if _, ok := backend.(ompAgentBackend); !ok {
			t.Errorf("agentBackendFor(%q) = %T, want omp", id, backend)
		}
	}
	for _, id := range []Provider{"", ProviderClaude, ProviderCodex, ProviderGrok, ProviderCursor} {
		backend := agentBackendForProvider(id)
		got, err := backend.StartAgent(agentStartRequest{Context: context.Background(), Config: Config{Provider: id, WorkDir: t.TempDir()}})
		if got != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
			t.Errorf("fallback %q = (%v, %v), want legacy refusal", id, got, err)
		}
	}
}

// TestT1055RegistryAdoptClaudeConnectDoesNotUseCLI drives Registry.Adopt,
// the reattach path, for a claude row that still carries a CLI connect URL.
func TestT1055RegistryAdoptClaudeConnectDoesNotUseCLI(t *testing.T) {
	t.Setenv(broker.NoBrokerEnv, "1")
	dir := t.TempDir()
	reg, err := NewRegistry(filepath.Join(dir, "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.StopAll)
	reg.SetDirect(true)
	if err := reg.Register(AgentDef{
		Name: "seat", Provider: ProviderClaude, SessionID: "sid-legacy",
		WorkDir: dir, ConnectURL: "http://127.0.0.1:9",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = reg.Adopt("seat")
	if err == nil || !strings.Contains(err.Error(), "legacy CLI configuration") {
		t.Fatalf("adopt = %v, want the legacy CLI configuration refusal", err)
	}
}

// TestT1055CLIBackendsDoNotSpawn drives the concrete session backends.
// Product dispatch never selects them. A direct call must still refuse
// before any process exists.
func TestT1055CLIBackendsDoNotSpawn(t *testing.T) {
	backends := []struct {
		name string
		b    agentBackend
	}{
		{"claude", claudeAgentBackend{}},
		{"codex", codexAgentBackend{}},
		{"grok", grokAgentBackend{}},
		{"cursor", cursorAgentBackend{}},
		{"adopt", adoptClaudeBackend{}},
	}
	for _, tc := range backends {
		got, err := tc.b.StartAgent(agentStartRequest{
			Context:   context.Background(),
			SessionID: "sid",
			Config:    Config{WorkDir: t.TempDir()},
		})
		if got != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
			t.Errorf("%s StartAgent = (%v, %v), want legacy refusal", tc.name, got, err)
		}
	}
}

// TestT1055CursorOpeningWatchReportsStuck is the in-memory alarm for
// `watch := false` on the opening prompt. A client that can be woken and
// never is reports ErrCursorPromptStuck. The CLI seat tests that used to
// carry this alarm started a process.
func TestT1055CursorOpeningWatchReportsStuck(t *testing.T) {
	shortenCursorSilenceBound(t, 30*time.Millisecond)
	c := &cursorACPClient{
		sessionID: "s1",
		peerWoke:  make(chan struct{}, 1),
		stdin:     discardWrite{},
		onEvent:   func(Event) {},
	}
	err := c.Prompt("the opening brief")
	if !errors.Is(err, ErrCursorPromptStuck) {
		t.Fatalf("Prompt = %v, want ErrCursorPromptStuck", err)
	}
}

// TestT1055CompiledSpawnPathsDoNotExec drives the spawn entry points that
// remained after the concrete StartAgent bodies were removed. Each one
// returns the legacy refusal, and the marker binary is not executed.
func TestT1055CompiledSpawnPathsDoNotExec(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "t1055s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s.sock")
	t.Setenv("CLAUDIA_TMUX_SOCKET", sock)
	t.Setenv(broker.NoBrokerEnv, "1")

	marker := filepath.Join(sockDir, "ran")
	bin := filepath.Join(sockDir, "bin")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_BIN", bin)
	work := t.TempDir()

	id, err := tmuxagent.SpawnWindow(work, "w", bin, nil)
	if id != "" || !errors.Is(err, tmuxagent.ErrLegacyCLISeatTransport) {
		t.Fatalf("SpawnWindow = (%q, %v), want legacy refusal", id, err)
	}
	if _, err := Acquire(context.Background(), Config{WorkDir: work}); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("Acquire = %v, want legacy refusal", err)
	}
	if _, err := AcquireDirect(context.Background(), Config{WorkDir: work}); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("AcquireDirect = %v, want legacy refusal", err)
	}
	client, err := startCodexAppServer(bin, work, "", "", false, "", codexSandboxTuning{}, nil, nil, nil)
	if client != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
		if client != nil {
			client.Close()
		}
		t.Fatalf("startCodexAppServer = (%v, %v), want legacy refusal", client, err)
	}
	if _, err := startGrokACP(bin, work, "", "", false, nil, nil, nil, nil); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("startGrokACP = %v, want legacy refusal", err)
	}
	if _, err := startGrokACPConnect(bin, work, "", "", false, nil, Config{}, nil, nil, nil); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("startGrokACPConnect = %v, want legacy refusal", err)
	}
	if _, err := spawnDetachedGrokServe(bin, "", nil); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("spawnDetachedGrokServe = %v, want legacy refusal", err)
	}
	if c, err := startCursorACP(t.Context(), bin, work, "", "", false, nil, nil, nil, nil); c != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
		if c != nil {
			c.Close()
		}
		t.Fatalf("startCursorACP = (%v, %v), want legacy refusal", c, err)
	}

	reg, err := NewRegistry(filepath.Join(work, "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{
		Name:      "ghost",
		WorkDir:   work,
		SessionID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	}); err != nil {
		t.Fatal(err)
	}
	reg.Stop("ghost")

	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("a spawn path executed the binary: %v", statErr)
	}
	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Fatalf("a spawn path created a tmux socket: %v", statErr)
	}
}

// recordingBackend records the term-log path the start path handed it.
// StartAgent succeeding is how an empty or disabled log is distinguished
// from a real path, which must refuse before this is called.
type recordingBackend struct {
	called bool
	term   string
}

func (b *recordingBackend) Capabilities() providerCapabilities { return providerCapabilities{} }

func (b *recordingBackend) StartAgent(req agentStartRequest) (*agentStart, error) {
	b.called = true
	b.term = req.TermLogPath
	return &agentStart{}, nil
}

// TestT1055LegacySurfacesDoNotExec drives Attach, the CLI term-log writer,
// Rewind, and the tmux entry points that still compiled after the spawn
// refusals. A fake tmux on PATH would record a call; none of these may
// make one, create a socket, or change a transcript.
func TestT1055LegacySurfacesDoNotExec(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "t1055s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s.sock")
	t.Setenv("CLAUDIA_TMUX_SOCKET", sock)

	marker := filepath.Join(sockDir, "tmux-ran")
	binDir := filepath.Join(sockDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmux := filepath.Join(binDir, "tmux")
	if err := os.WriteFile(tmux, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	attached := &Agent{provider: ProviderClaude, tmuxWindowID: "@9", ops: claudeAgentOps()}
	if got := attached.AttachCommand(); got != "" {
		t.Fatalf("AttachCommand = %q, want empty", got)
	}

	realTerm := filepath.Join(sockDir, "seat.term")
	refusing := &recordingBackend{}
	if _, err := startWithBackend(Config{WorkDir: t.TempDir(), SessionID: "sid-term", TermLogPath: realTerm}, refusing); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("real TermLogPath = %v, want legacy refusal", err)
	}
	if refusing.called {
		t.Fatal("real TermLogPath reached the backend")
	}
	if _, statErr := os.Stat(realTerm); !os.IsNotExist(statErr) {
		t.Fatalf("term log was created: %v", statErr)
	}

	for _, disabled := range []string{"", "-"} {
		work := t.TempDir()
		rec := &recordingBackend{}
		agent, err := startWithBackend(Config{WorkDir: work, SessionID: "sid-off", TermLogPath: disabled}, rec)
		if err != nil {
			t.Fatalf("TermLogPath %q: %v", disabled, err)
		}
		agent.Stop()
		if !rec.called || rec.term != "" {
			t.Fatalf("TermLogPath %q handed the backend %q", disabled, rec.term)
		}
		resolved, err := filepath.EvalSymlinks(work)
		if err != nil {
			t.Fatal(err)
		}
		defaultTerm := filepath.Join(termLogDir(resolved), "sid-off.term")
		if _, statErr := os.Stat(defaultTerm); !os.IsNotExist(statErr) {
			t.Fatalf("TermLogPath %q created %s: %v", disabled, defaultTerm, statErr)
		}
	}

	transcript := filepath.Join(sockDir, "seat.jsonl")
	const body = "turn\nCHARLIE\n"
	if err := os.WriteFile(transcript, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	seat := &Agent{provider: ProviderClaude, sessionID: "sid", jsonlPath: transcript}
	rewound, err := seat.Rewind(1, Config{Provider: ProviderClaude, WorkDir: sockDir})
	if rewound != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("Agent.Rewind = (%v, %v), want legacy refusal", rewound, err)
	}
	if got, readErr := os.ReadFile(transcript); readErr != nil || string(got) != body {
		t.Fatalf("Agent.Rewind changed the transcript: %q %v", got, readErr)
	}

	reg, err := NewRegistry(filepath.Join(sockDir, "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{Name: "seat", WorkDir: sockDir, SessionID: "sid", TermLogPath: "-"}); err != nil {
		t.Fatal(err)
	}
	next, res, err := reg.Rewind(context.Background(), "seat", 1)
	if next != nil || res != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("Registry.Rewind = (%v, %+v, %v), want legacy refusal", next, res, err)
	}
	if got, readErr := os.ReadFile(transcript); readErr != nil || string(got) != body {
		t.Fatalf("Registry.Rewind changed the transcript: %q %v", got, readErr)
	}

	if err := tmuxagent.EnsureServer(); !errors.Is(err, tmuxagent.ErrLegacyCLISeatTransport) {
		t.Fatalf("EnsureServer = %v, want legacy refusal", err)
	}
	if err := tmuxagent.KillWindow("@1"); !errors.Is(err, tmuxagent.ErrLegacyCLISeatTransport) {
		t.Fatalf("KillWindow = %v, want legacy refusal", err)
	}
	if windows, err := tmuxagent.ListWindows(); windows != nil || !errors.Is(err, tmuxagent.ErrLegacyCLISeatTransport) {
		t.Fatalf("ListWindows = (%v, %v), want legacy refusal", windows, err)
	}
	if err := tmuxagent.SendKeys("@1", "hi"); !errors.Is(err, tmuxagent.ErrLegacyCLISeatTransport) {
		t.Fatalf("SendKeys = %v, want legacy refusal", err)
	}
	if agent, err := acquireLocked(context.Background(), Config{}, sockDir, "", "w", "spawn"); agent != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("acquireLocked = (%v, %v), want legacy refusal", agent, err)
	}

	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("a legacy surface executed tmux: %v", statErr)
	}
	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Fatalf("a legacy surface created a tmux socket: %v", statErr)
	}
}

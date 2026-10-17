// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/internal/tmuxagent"
	"github.com/marcelocantos/claudia/omp"
)

// TestT1055FleetSpawnUsesOMPOnly is the fleet spawn for 🎯T1055: one direct
// Registry.Launch per currently-used provider, against the real sidecar.
// Subscription CLIs and tmux are first on PATH. A launch that succeeded
// has no native window, and none of those binaries run.
//
// Attach, a terminal-log file, and Rewind have no sidecar equivalent.
// 🎯T189 says seat removal does not wait for them; those calls refuse.
// Task mode has no OMP replacement (🎯T192). Bedrock and Ollama stay API
// paths and are not fleet seat providers.
func TestT1055FleetSpawnUsesOMPOnly(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "t1055f")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	marker := filepath.Join(root, "cli-ran")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ntouch '" + marker + "'\n"
	for _, name := range []string{"claude", "codex", "grok", "agent", "cursor-agent", "tmux", "security"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	socket := filepath.Join(root, "omp.sock")
	cmd := exec.Command(bun, "sidecar/server.ts", socket)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + root,
		"CLAUDIA_OMP_SPOOL_DIR=" + filepath.Join(root, "spool"),
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	ctx := t.Context()
	for !omp.Listening(ctx, socket) {
		if err := ctx.Err(); err != nil {
			t.Fatalf("sidecar: %v", err)
		}
	}

	t.Setenv("HOME", root)
	t.Setenv(omp.SocketEnv, socket)
	t.Setenv("CLAUDIA_OMP_SPOOL_DIR", filepath.Join(root, "spool"))
	t.Setenv("CLAUDIA_OMP_NO_REFRESH", "1")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(broker.NoBrokerEnv, "1")
	t.Setenv(tmuxSocketEnv, filepath.Join(root, "tmux.sock"))

	omp.ResetKeychainShot()
	t.Cleanup(omp.ResetKeychainShot)
	item := omp.Item{Records: map[string]omp.Record{}}
	for _, p := range []string{omp.Anthropic, omp.OpenAICodex, omp.XAIOAuth, omp.Cursor} {
		item.Records[p] = omp.Record{AccessToken: "fixture", RefreshToken: "fixture", Expiry: time.Now().Add(time.Hour)}
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if err := omp.Open(ctx, omp.Store{Run: func(context.Context, string, ...string) ([]byte, error) {
		return raw, nil
	}}); err != nil {
		t.Fatal(err)
	}

	providers := []Provider{
		"", ProviderClaude, Provider(omp.Anthropic),
		ProviderCodex, Provider(omp.OpenAICodex),
		ProviderGrok, Provider(omp.XAIOAuth),
		ProviderCursor,
	}
	for i, p := range providers {
		name := fmt.Sprintf("fleet-%d", i)
		reg, err := NewRegistry(filepath.Join(root, name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		reg.SetDirect(true)
		if err := reg.Register(AgentDef{
			Name: name, SessionID: "fleet", Provider: p, WorkDir: root, TermLogPath: "-",
		}); err != nil {
			t.Fatal(err)
		}
		a, err := reg.LaunchContext(ctx, name)
		if err != nil {
			t.Fatalf("provider %q: %v", p, err)
		}
		if a.WindowID() != "" || a.AttachCommand() != "" {
			t.Fatalf("provider %q launched a native transport: window %q attach %q", p, a.WindowID(), a.AttachCommand())
		}
		want := p
		if p == "" {
			want = ProviderClaude
		}
		if a.Provider() != want {
			t.Fatalf("provider %q reported %q", p, a.Provider())
		}
		reg.Stop(name)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("fleet spawn executed a CLI: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, "tmux.sock")); !os.IsNotExist(statErr) {
		t.Fatalf("fleet spawn created a tmux socket: %v", statErr)
	}
}

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

	if err := checkTmux(); !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("checkTmux = %v, want legacy refusal", err)
	}

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

	if frame, err := tmuxagent.CapturePane("@1"); frame != nil || !errors.Is(err, tmuxagent.ErrLegacyCLISeatTransport) {
		t.Fatalf("CapturePane = (%q, %v), want legacy refusal", frame, err)
	}
	if err := tmuxagent.SendEscape("@1"); !errors.Is(err, tmuxagent.ErrLegacyCLISeatTransport) {
		t.Fatalf("SendEscape = %v, want legacy refusal", err)
	}
	if err := tmuxagent.SetWindowOption("@1", "claudia-held", "0"); !errors.Is(err, tmuxagent.ErrLegacyCLISeatTransport) {
		t.Fatalf("SetWindowOption = %v, want legacy refusal", err)
	}
	if ctrl, err := tmuxagent.DialControl("@1"); ctrl != nil || !errors.Is(err, tmuxagent.ErrLegacyCLISeatTransport) {
		t.Fatalf("DialControl = (%v, %v), want legacy refusal", ctrl, err)
	}
	if err := tmuxagent.ResizeWindow("@1", 80, 24); !errors.Is(err, tmuxagent.ErrLegacyCLISeatTransport) {
		t.Fatalf("ResizeWindow = %v, want legacy refusal", err)
	}

	adopted, err := adoptWindow(context.Background(), Config{}, sockDir, "@7", "sid")
	if adopted != nil || !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("adoptWindow = (%v, %v), want legacy refusal", adopted, err)
	}
	if _, err := adoptWindow(context.Background(), Config{}, sockDir, "@7", ""); err == nil || !strings.Contains(err.Error(), poolSessionOption) {
		t.Fatalf("empty session adopt = %v, want %s", err, poolSessionOption)
	}

	pooled, err := buildPoolAgent(Config{TermLogPath: "-"}, sockDir, "@7", "sid", true)
	if pooled != nil || !errors.Is(err, ErrLegacyCLISeatTransport) || !strings.Contains(err.Error(), "pool") {
		t.Fatalf("buildPoolAgent = (%v, %v), want pool refusal", pooled, err)
	}
	pooled, err = buildPoolAgent(Config{TermLogPath: realTerm}, sockDir, "@7", "sid", false)
	if pooled != nil || !errors.Is(err, ErrLegacyCLISeatTransport) || !strings.Contains(err.Error(), "term log") {
		t.Fatalf("buildPoolAgent term log = (%v, %v), want term-log refusal", pooled, err)
	}
	if _, statErr := os.Stat(realTerm); !os.IsNotExist(statErr) {
		t.Fatalf("buildPoolAgent created a term log: %v", statErr)
	}

	stopped := false
	local := &Agent{tmuxWindowID: "@9", ops: agentOps{stop: func(*Agent) { stopped = true }}}
	for _, disposition := range []string{"return", "drop", "keep_alive_for:30", "nope"} {
		if err := local.Release(disposition); !errors.Is(err, ErrLegacyCLISeatTransport) {
			t.Fatalf("Release %q = %v, want legacy refusal", disposition, err)
		}
	}
	if stopped {
		t.Fatal("local Release called Stop")
	}
	released := false
	brokered := &Agent{ops: agentOps{release: func(_ *Agent, disposition string) error {
		released = disposition == "return"
		return nil
	}}}
	if err := brokered.Release("return"); err != nil || !released {
		t.Fatalf("broker Release = %v released=%v", err, released)
	}

	flying := &Agent{provider: ProviderClaude, tmuxWindowID: "@9", alive: true, ops: claudeAgentOps()}
	if flying.PromptInFlight() {
		t.Fatal("PromptInFlight reported a turn without a pane")
	}
	if err := flying.Interrupt(); !errors.Is(err, tmuxagent.ErrLegacyCLISeatTransport) {
		t.Fatalf("Interrupt = %v, want legacy refusal", err)
	}
	if err := flying.Resize(80, 24); !errors.Is(err, tmuxagent.ErrLegacyCLISeatTransport) {
		t.Fatalf("Resize = %v, want legacy refusal", err)
	}

	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("a legacy surface executed tmux: %v", statErr)
	}
	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Fatalf("a legacy surface created a tmux socket: %v", statErr)
	}
}

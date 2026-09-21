// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

func TestCodexAppServerTurnSteerRequest(t *testing.T) {
	t.Parallel()
	req := codexAppServerTurnSteer(7, "thr_1", "turn_1", "use Go")
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"method":"turn/steer","id":7,"params":{"threadId":"thr_1","expectedTurnId":"turn_1","input":[{"type":"text","text":"use Go"}]}}`
	if string(b) != want {
		t.Errorf("turn/steer = %s\nwant %s", b, want)
	}
}

// The probe answers from the schema dump the CLI itself produced: the
// steer file present means turn/steer is on the wire, absent or a
// failed dump means it is not. A file that exists but is empty is not
// a listing.
func TestCodexAppServerSupportsSteerFromSchemaDump(t *testing.T) {
	t.Parallel()
	writes := func(body string) func(string, string) error {
		return func(_ string, outDir string) error {
			path := filepath.Join(outDir, codexSteerSchemaFile)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			return os.WriteFile(path, []byte(body), 0o644)
		}
	}
	if !codexAppServerSupportsSteerFrom("codex", writes(`{"title":"TurnSteerParams"}`)) {
		t.Error("steer schema present → want supported")
	}
	if codexAppServerSupportsSteerFrom("codex", writes("")) {
		t.Error("empty steer schema → want unsupported")
	}
	if codexAppServerSupportsSteerFrom("codex", func(string, string) error { return nil }) {
		t.Error("no steer schema → want unsupported")
	}
	if codexAppServerSupportsSteerFrom("codex", func(string, string) error { return errors.New("exit 2") }) {
		t.Error("failed dump → want unsupported")
	}
	// The cached probe is keyed by binary path, so two installs do not
	// share an answer.
	codexSteerProbeCache.Store("/fake/with-steer", true)
	codexSteerProbeCache.Store("/fake/without-steer", false)
	if !codexAppServerSupportsSteer("/fake/with-steer") || codexAppServerSupportsSteer("/fake/without-steer") {
		t.Error("probe cache is not keyed by binary path")
	}
}

func TestCodexTurnCapsFollowInstalledCLI(t *testing.T) {
	t.Parallel()
	with := codexTurnCaps(true)
	if !with.CanSteer || with.SteerPolicy != SteerFinishSlice || !with.CanInterrupt {
		t.Errorf("with turn/steer = %+v", with)
	}
	without := codexTurnCaps(false)
	if without.CanSteer || without.SteerPolicy != SteerQueueUntilIdle || !without.CanInterrupt {
		t.Errorf("without turn/steer = %+v", without)
	}
}

func waitForTurnPhase(t *testing.T, agent *Agent, want TurnPhase) {
	t.Helper()
	backstop := wallclockguard.UntilTestTimeout(t)
	for backstop.Err() == nil {
		if agent.TurnPhase() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("TurnPhase never became %s (now %s)", want, agent.TurnPhase())
}

// A Codex CLI whose schema lists turn/steer: Start wires the mechanism,
// TurnCaps says so, and Steer against an open turn speaks turn/steer
// with the active turn id as the precondition.
func TestHermeticCodexSteerFoldsIntoOpenTurn(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	t.Setenv("FAKE_CODEX_STEER", "1")
	t.Setenv("FAKE_CODEX_HOLD_TURN", "1")
	steerLog := filepath.Join(t.TempDir(), "steer.log")
	t.Setenv("FAKE_CODEX_STEER_LOG", steerLog)
	writeFakeCodexSubscriptionAuth(t)

	agent, err := Start(Config{Provider: ProviderCodex, WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	caps := agent.TurnCaps()
	if !caps.CanSteer || caps.SteerPolicy != SteerFinishSlice || !caps.CanInterrupt {
		t.Fatalf("TurnCaps = %+v, want Codex with turn/steer", caps)
	}
	if agent.TurnPhase() != TurnIdle {
		t.Fatalf("TurnPhase before any turn = %s", agent.TurnPhase())
	}
	if _, err := agent.Steer("too early"); !errors.Is(err, ErrTurnIdle) {
		t.Fatalf("Steer while idle err = %v, want ErrTurnIdle", err)
	}

	waitForEventSubscribers(t, agent, 0)
	if err := agent.Send("start"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitForTurnPhase(t, agent, TurnInTurn)

	out, err := agent.SendMode("actually, use Go", DeliverySteer)
	if err != nil {
		t.Fatalf("SendMode(steer): %v", err)
	}
	if out.Mechanism != MechanismCodexTurnSteer || out.PhaseBefore != TurnInTurn || out.SupersededTurnID != "" {
		t.Errorf("outcome = %+v", out)
	}
	waitForTurnPhase(t, agent, TurnIdle)

	logged, err := os.ReadFile(steerLog)
	if err != nil {
		t.Fatalf("steer log: %v", err)
	}
	if !strings.Contains(string(logged), `"expectedTurnId":"turn_success"`) || !strings.Contains(string(logged), `"text":"actually, use Go"`) {
		t.Errorf("turn/steer params = %s", logged)
	}
}

// A Codex CLI whose schema has no turn/steer: the steer claim is
// withdrawn at Start and Steer refuses with the typed error rather than
// sending a method the server would not know.
func TestHermeticCodexWithoutSteerRefusesHonestly(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	t.Setenv("FAKE_CODEX_HOLD_TURN", "1")
	writeFakeCodexSubscriptionAuth(t)

	agent, err := Start(Config{Provider: ProviderCodex, WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	caps := agent.TurnCaps()
	if caps.CanSteer || caps.SteerPolicy != SteerQueueUntilIdle || !caps.CanInterrupt {
		t.Fatalf("TurnCaps = %+v, want steer withdrawn", caps)
	}
	waitForEventSubscribers(t, agent, 0)
	if err := agent.Send("start"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitForTurnPhase(t, agent, TurnInTurn)
	out, err := agent.Steer("nudge")
	if !errors.Is(err, ErrSteerUnsupported) || out.Mechanism != MechanismSteerUnsupported {
		t.Fatalf("Steer = %+v / %v, want ErrSteerUnsupported", out, err)
	}
	// The interrupt path still works, and SendMode(interrupt) is the
	// honest fallback: hard-stop, then the new text starts a turn.
	t.Setenv("FAKE_CODEX_HOLD_TURN", "0")
	out, err = agent.SendMode("stop; do this instead", DeliveryInterrupt)
	if err != nil {
		t.Fatalf("SendMode(interrupt): %v", err)
	}
	if out.Mechanism != MechanismInterruptThenSubmit {
		t.Errorf("outcome = %+v", out)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

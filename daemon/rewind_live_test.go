// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia"
)

// TestRewindLive is 🎯T75.8's live gate. After the live skips, Rewind
// refuses before it stops a seat or truncates a transcript. It does not
// start claude.
func TestRewindLive(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test spends API credit)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	// The CLI resume that used to prove a truncated transcript is gone.
	// Rewind refuses before it stops a seat or writes the file.
	reg, err := claudia.NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := reg.Register(claudia.AgentDef{Name: "rewind-live", WorkDir: work, SessionID: "sid-live", TermLogPath: "-"}); err != nil {
		t.Fatal(err)
	}
	path := claudia.SessionJSONLPath("sid-live", work)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const transcript = "ALPHA\nBRAVO\n"
	if err := os.WriteFile(path, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	got, res, err := reg.Rewind(context.Background(), "rewind-live", 1)
	if got != nil || res != nil || !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("Rewind = (%v, %+v, %v), want legacy refusal", got, res, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != transcript {
		t.Fatalf("rewind mutated the transcript:\n%s", b)
	}
}

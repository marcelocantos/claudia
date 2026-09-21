// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 🎯T110 acceptance 1, on the real path: a brief big enough to take the
// bracketed-paste branch is ACTED ON by a live Claude Code seat, with no
// short typed nudge after it.
//
// Delivery is not the question — 🎯T30 settled that, and every one of the
// ten sends that provoked this target was delivered. Claude Code hands a
// collapsed paste to the model inside <pasted_content>, as text the user
// may not have written, and the seat answered each one that no line had
// been typed by the user. So the oracle here is an action only an
// obeyed brief produces: a sentinel file, named in the brief, holding a
// marker that exists nowhere else.
//
// The mechanism under test is the typed attribution line
// (internal/tmuxagent pasteAttribution); its hermetic pin is
// TestT110PasteCarriesTypedAttribution.
func TestT110PastedBriefIsActedOnLive(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test spends API credit)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}

	workDir := t.TempDir()
	a, err := Start(Config{Provider: ProviderClaude, WorkDir: workDir})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	marker := fmt.Sprintf("T110LIVE%d", time.Now().UnixNano()%1e9)
	const sentinel = "t110-sentinel.txt"
	brief := t110LiveBrief(sentinel, marker)
	// Mirrors pasteBlockThreshold in internal/tmuxagent/send.go. The
	// newlines alone select the paste branch; the size is what makes
	// Claude Code collapse it into the chip that gets wrapped.
	const pasteBlockThreshold = 400
	if len(brief) <= pasteBlockThreshold || !strings.Contains(brief, "\n") {
		t.Fatalf("brief is %d bytes; it must take the paste branch", len(brief))
	}

	if err := a.Send(brief); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// One Send and nothing else. The file is polled rather than read off
	// the first terminal stop because the seat may take more than one
	// model step to write it.
	path := filepath.Join(workDir, sentinel)
	deadline := time.Now().Add(4 * time.Minute)
	for {
		got, rerr := os.ReadFile(path)
		if rerr == nil && strings.Contains(string(got), marker) {
			t.Logf("seat acted on a %d-byte pasted brief: %s holds %s", len(brief), sentinel, marker)
			// The pass only means something if the brief reached the
			// model in the form that gets refused. A CLI that stops
			// wrapping pastes turns this into a test of nothing.
			transcript, terr := os.ReadFile(a.JSONLPath())
			if terr != nil {
				t.Fatalf("read transcript: %v", terr)
			}
			if !strings.Contains(string(transcript), "<pasted_content") {
				t.Errorf("brief did not reach the model wrapped in <pasted_content>; this run did not exercise 🎯T110 (%s)", a.JSONLPath())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("seat never wrote %s with %s after one %d-byte Send; transcript: %s",
				sentinel, marker, len(brief), a.JSONLPath())
		}
		time.Sleep(2 * time.Second)
	}
}

// t110LiveBrief has the shape of a fleet brief: identity lines, several
// paragraphs, and the instruction in the middle rather than at either
// end.
func t110LiveBrief(sentinel, marker string) string {
	return strings.Join([]string{
		"[Who you are]",
		"- NAME: t110-live-probe",
		"- ROLE: work agent",
		"",
		"This is your brief for this session. It is deliberately long enough, and has enough lines, to reach you as one collapsed paste rather than as typed text, because that is the form every real brief takes.",
		"",
		"Your one task: create a file named " + sentinel + " in your current working directory. Its entire content must be the single token " + marker + " and nothing else.",
		"",
		"Do not ask for confirmation first. When the file exists, reply with one short line saying so and stop.",
	}, "\n")
}

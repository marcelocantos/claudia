// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureLines is a synthetic transcript with three genuine user turns
// (ALPHA, turn2, CHARLIE). Between turn2 and turn3 sits a tool_use assistant
// message and its tool_result — which the transcript records with role "user"
// but which must NOT be counted as a turn.
var fixtureLines = []string{
	`{"type":"summary","summary":"prior session"}`,
	`{"type":"user","message":{"role":"user","content":"turn1 ALPHA"}}`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}}`,
	`{"type":"user","message":{"role":"user","content":"turn2 run a tool"}}`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash"}],"stop_reason":"tool_use"}}`,
	`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"done"}]}}`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"finished"}],"stop_reason":"end_turn"}}`,
	`{"type":"user","message":{"role":"user","content":"turn3 CHARLIE"}}`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}}`,
}

func writeTranscript(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRewindJSONLMetamorphic is the core oracle: after a rewind, the truncated
// transcript must be a byte-exact prefix of the original, and prefix + removed
// tail must reconstitute the original exactly. This is the property claude
// --resume relies on to replay only the surviving turns.
func TestRewindJSONLMetamorphic(t *testing.T) {
	path := writeTranscript(t, fixtureLines)
	orig := mustRead(t, path)

	res, err := rewindJSONL(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	trunc := mustRead(t, path)

	if !bytes.Equal(trunc, orig[:len(trunc)]) {
		t.Fatal("truncated transcript is not a byte-exact prefix of the original")
	}
	if res.BytesRemoved != int64(len(orig)-len(trunc)) {
		t.Fatalf("BytesRemoved=%d, want %d", res.BytesRemoved, len(orig)-len(trunc))
	}
	// The backup holds the full pre-rewind transcript.
	if backup := mustRead(t, res.BackupPath); !bytes.Equal(backup, orig) {
		t.Fatal("backup is not the full pre-rewind transcript")
	}
	// prefix + removed tail == original.
	if !bytes.Equal(append(append([]byte{}, trunc...), orig[len(trunc):]...), orig) {
		t.Fatal("prefix + removed tail does not reconstitute the original")
	}
}

func TestRewindJSONLTurnBoundaries(t *testing.T) {
	// rewind(1) drops only turn3 (CHARLIE); turns 1 and 2 survive.
	path := writeTranscript(t, fixtureLines)
	res, err := rewindJSONL(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	got := string(mustRead(t, path))
	if strings.Contains(got, "CHARLIE") {
		t.Error("rewind(1) must drop turn3 CHARLIE")
	}
	if !strings.Contains(got, "ALPHA") || !strings.Contains(got, "turn2") {
		t.Error("rewind(1) must keep turns 1 and 2")
	}
	if res.TurnsRemoved != 1 {
		t.Errorf("TurnsRemoved=%d, want 1", res.TurnsRemoved)
	}
}

// TestRewindJSONLSkipsToolResults guards the mid-tool-use hazard: rewind(2)
// must drop turns 2 and 3 and land cleanly at turn1's boundary. If the
// tool_result line (role "user") were miscounted as a turn, rewind(2) would cut
// inside turn2's tool exchange and leave a dangling tool_use.
func TestRewindJSONLSkipsToolResults(t *testing.T) {
	path := writeTranscript(t, fixtureLines)
	if _, err := rewindJSONL(path, 2); err != nil {
		t.Fatal(err)
	}
	got := string(mustRead(t, path))
	if strings.Contains(got, "turn2") || strings.Contains(got, "CHARLIE") || strings.Contains(got, "tool_result") {
		t.Errorf("rewind(2) must drop turns 2 and 3 (incl. the tool exchange); got:\n%s", got)
	}
	if !strings.Contains(got, "ALPHA") {
		t.Error("rewind(2) must keep turn1 ALPHA")
	}
	// Only the summary + turn1 user + turn1 assistant survive.
	if lines := strings.Count(strings.TrimRight(got, "\n"), "\n") + 1; lines != 3 {
		t.Errorf("expected 3 surviving lines, got %d:\n%s", lines, got)
	}
}

func TestRewindJSONLErrors(t *testing.T) {
	path := writeTranscript(t, fixtureLines)
	if _, err := rewindJSONL(path, 0); err == nil {
		t.Error("n=0 must error")
	}
	if _, err := rewindJSONL(path, 4); err == nil {
		t.Error("n greater than the 3 available turns must error")
	}
	// A failed rewind must not have altered the transcript.
	if got := mustRead(t, path); string(got) != strings.Join(fixtureLines, "\n")+"\n" {
		t.Error("errored rewind must leave the transcript untouched")
	}
}

func TestUnrewindRestores(t *testing.T) {
	path := writeTranscript(t, fixtureLines)
	orig := mustRead(t, path)
	if _, err := rewindJSONL(path, 2); err != nil {
		t.Fatal(err)
	}
	if err := Unrewind(path); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, path); !bytes.Equal(got, orig) {
		t.Fatal("Unrewind must restore the exact original transcript")
	}
	if _, err := os.Stat(path + rewindBackupSuffix); !os.IsNotExist(err) {
		t.Error("Unrewind must remove the backup sidecar")
	}
}

// TestRewindSessionLive is the end-to-end oracle: build a real three-codeword
// transcript, RewindSession back two turns, resume, and confirm the model
// recalls only the surviving turn — proving claude --resume honours the
// truncation. Gated on CLAUDIA_LIVE (spends API credit).
func TestRewindSessionLive(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test spends API credit)")
	}
	if _, err := resolveClaudeBin(); err != nil {
		t.Skip("claude binary not found")
	}

	run := func(task *Task, prompt string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		ch, err := task.Run(ctx, prompt)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		var text, result strings.Builder
		for ev := range ch {
			switch ev.Type {
			case TaskEventText:
				text.WriteString(ev.Content)
			case TaskEventResult:
				result.WriteString(ev.Content)
			case TaskEventError:
				t.Fatalf("task error: %s", ev.ErrorMsg)
			}
		}
		if result.Len() > 0 {
			return result.String()
		}
		return text.String()
	}

	workDir := t.TempDir()
	task := NewTask(TaskConfig{ID: "rw", WorkDir: workDir})
	run(task, "Remember this codeword: ALPHA. Reply with only: ok")
	cid := task.ClaudeID()
	if cid == "" {
		t.Fatal("no session id after first run")
	}
	run(task, "Remember this codeword too: BRAVO. Reply with only: ok")
	run(task, "Remember this codeword too: CHARLIE. Reply with only: ok")

	resolved, _ := filepath.EvalSymlinks(workDir)
	before := mustRead(t, SessionJSONLPath(cid, resolved))

	res, err := RewindSession(cid, workDir, 2) // drop BRAVO + CHARLIE, keep ALPHA
	if err != nil {
		t.Fatalf("RewindSession: %v", err)
	}
	after := mustRead(t, res.JSONLPath)
	if !bytes.Equal(after, before[:len(after)]) {
		t.Fatal("rewound transcript is not a byte-exact prefix of the original")
	}

	task2 := NewTask(TaskConfig{ID: "rw2", WorkDir: workDir, ClaudeID: cid})
	resp := strings.ToUpper(run(task2, "List every codeword I have asked you to remember, comma-separated. If none, reply NONE."))
	t.Logf("post-rewind recall: %q", resp)
	if !strings.Contains(resp, "ALPHA") {
		t.Error("resume lost the surviving turn (ALPHA)")
	}
	if strings.Contains(resp, "BRAVO") || strings.Contains(resp, "CHARLIE") {
		t.Error("resume resurfaced rewound turns (BRAVO/CHARLIE) — truncation not honoured")
	}
}

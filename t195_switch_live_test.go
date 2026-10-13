// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// 🎯T195 live: a running sidecar seat moves from the anthropic plan to the
// openai-codex plan in place, and the new model answers from the
// conversation the old one had, tool calls included. One secret is said in a
// prompt; the other exists only in a file the seat reads with a tool before
// the switch, so recalling it means the destination read the converted
// tool-call history. The session id must not change. It spends a few turns
// on both plans.
func TestT195LiveSidecarSwitchKeepsConversation(t *testing.T) {
	if os.Getenv("CLAUDIA_OMP_LIVE") == "" {
		t.Skip("CLAUDIA_OMP_LIVE not set (this test spends anthropic and openai-codex plan capacity)")
	}
	if !BrokerAvailable() {
		t.Skip("no claudia daemon reachable (start one with `claudia broker serve`; run with CLAUDIA_NO_BROKER=0)")
	}
	name := "t195-switch-live-" + newRunID()
	work := t.TempDir()
	const fileSecret = "OCHREPELICAN27"
	if err := os.WriteFile(filepath.Join(work, "secret.txt"), []byte(fileSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Start(Config{Name: name, Provider: Provider(omp.Anthropic), WorkDir: work})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(a.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := a.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	ask := func(prompt string) string {
		t.Helper()
		if err := a.Send(prompt); err != nil {
			t.Fatalf("Send: %v", err)
		}
		turn, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		text, err := a.WaitForResponse(turn)
		if err != nil {
			t.Fatalf("WaitForResponse: %v", err)
		}
		return text
	}
	const codeword = "MAGENTAWALRUS61"
	ask("Remember this codeword for later: " + codeword + ". Reply with exactly: STORED")
	read := ask("Use your Read or Bash tool to read the file secret.txt in your working directory. Reply with its contents only.")
	if !strings.Contains(strings.ToUpper(read), fileSecret) {
		t.Fatalf("anthropic seat did not read the file with a tool: %q", read)
	}
	session := a.SessionID()

	model, err := standardSessionModel(ctx, ProviderCodex)
	if err != nil {
		t.Fatalf("standard codex model: %v", err)
	}
	if err := a.SwitchModel(Provider(omp.OpenAICodex), model); err != nil {
		t.Fatalf("SwitchModel: %v", err)
	}
	if a.Provider() != Provider(omp.OpenAICodex) || a.Model() != model {
		t.Fatalf("seat is on %s/%s, want openai-codex/%s", a.Provider(), a.Model(), model)
	}
	if a.SessionID() != session {
		t.Fatalf("switch replaced the session: %s → %s", session, a.SessionID())
	}
	got := ask("What is the codeword I asked you to remember? Reply with the codeword only.")
	if !strings.Contains(strings.ToUpper(got), codeword) {
		t.Fatalf("openai-codex/%s did not recall the codeword from the anthropic turns: %q", model, got)
	}
	// Gone now: only the conversation can answer.
	if err := os.Remove(filepath.Join(work, "secret.txt")); err != nil {
		t.Fatal(err)
	}
	got = ask("Without using any tool, what did secret.txt contain when you read it earlier? Reply with the contents only.")
	if !strings.Contains(strings.ToUpper(got), fileSecret) {
		t.Fatalf("openai-codex/%s did not recall the earlier tool result: %q", model, got)
	}
	t.Logf("switched %s anthropic → openai-codex/%s on session %s; recalled the codeword and the tool result %q", name, model, session, got)
}

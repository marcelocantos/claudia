// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestBrokerReclaimLiveBackends is 🎯T2.11's live gate: through the
// installed daemon, a consumer grants a real seat, sends a turn, dies
// without releasing, and a second consumer reclaims the seat by name and
// receives the turn's answer. Gated like every live test (spends plan
// capacity) and additionally on a reachable daemon: run with
// CLAUDIA_NO_BROKER=0 so the suite's TestMain opt-out does not apply.
func TestBrokerReclaimLiveBackends(t *testing.T) {
	if !BrokerAvailable() {
		t.Skip("no claudia daemon reachable (start one with `claudia broker serve`; run with CLAUDIA_NO_BROKER=0)")
	}
	cases := []struct {
		gate     string
		provider Provider
	}{
		{"CLAUDIA_LIVE", ProviderClaude},
		{"CLAUDIA_GROK_LIVE", ProviderGrok},
		{"CLAUDIA_CODEX_LIVE", ProviderCodex},
		{"CLAUDIA_CURSOR_LIVE", ProviderCursor},
	}
	for _, tc := range cases {
		t.Run(string(tc.provider), func(t *testing.T) {
			if os.Getenv(tc.gate) == "" {
				t.Skipf("%s not set (this test spends plan capacity)", tc.gate)
			}
			name := "reclaim-live-" + string(tc.provider) + "-" + newRunID()
			// The daemon keeps this seat's terminal log (default path) so a
			// failure here can show what the provider printed.
			cfg := Config{Name: name, Provider: tc.provider, WorkDir: t.TempDir()}
			first, err := Start(cfg)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			stopped := false
			t.Cleanup(func() {
				if !stopped {
					first.Stop()
				}
			})
			if err := first.WaitReady(t.Context()); err != nil {
				t.Fatalf("WaitReady: %v", err)
			}
			if err := first.Send("Reply with exactly: pong"); err != nil {
				t.Fatalf("Send: %v\nterminal log %s:\n%s", err, first.TermLogPath(), termLogTail(first.TermLogPath()))
			}
			sid := first.SessionID()
			// The consumer dies mid-turn: drop the socket without a release.
			first.mcpCleanup()
			deadline := time.Now().Add(5 * time.Second)
			for first.Alive() && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if first.Alive() {
				t.Fatal("dead consumer's handle still reports alive")
			}

			cfg.SessionID = sid
			second, err := Start(cfg)
			if err != nil {
				t.Fatalf("reclaim Start: %v", err)
			}
			stopped = true
			t.Cleanup(second.Stop)
			if second.SessionID() != sid {
				t.Fatalf("reclaim changed the session: %s → %s", sid, second.SessionID())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			text, err := second.WaitForResponse(ctx)
			if err != nil {
				t.Fatalf("WaitForResponse after reclaim: %v", err)
			}
			if !strings.Contains(strings.ToLower(text), "pong") {
				t.Fatalf("reclaimed turn answer = %q, want pong", text)
			}
			t.Logf("reclaimed %s seat %s; answer %q", tc.provider, sid, text)
		})
	}
}

// termLogTail returns the last few KB of a terminal log with escapes stripped.
func termLogTail(path string) string {
	if path == "" {
		return "(no terminal log)"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "(unreadable: " + err.Error() + ")"
	}
	if len(raw) > 4000 {
		raw = raw[len(raw)-4000:]
	}
	var b strings.Builder
	esc := false
	for _, r := range string(raw) {
		switch {
		case r == 0x1b:
			esc = true
		case esc && ((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')):
			esc = false
		case esc:
		case r == '\r':
			b.WriteByte('\n')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

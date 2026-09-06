// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestExclusiveGrokHomeSurvivesAgentStopAndReturnedID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("GROK_BIN", writeFakeGrokACP(t))
	t.Setenv("FAKE_ACP_REJECT_LOAD", "1")
	cfg := Config{Provider: ProviderGrok, WorkDir: t.TempDir(), SessionID: "requested-id", MCPExclusive: true, TermLogPath: "-"}
	agent, err := Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Stop()
	sid := agent.SessionID()
	if sid == cfg.SessionID {
		t.Fatal("fixture did not replace requested ID")
	}
	home := exclusiveGrokHomeDir(sid)
	marker := filepath.Join(home, "conversation-evidence")
	if err := os.WriteFile(marker, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	agent.Stop()
	if b, err := os.ReadFile(marker); err != nil || string(b) != "retained" {
		t.Fatalf("Stop lost provider storage: %q %v", b, err)
	}
	t.Setenv("FAKE_ACP_REJECT_LOAD", "")
	cfg.SessionID, cfg.RequireResume = sid, true
	next, err := Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Stop()
	if next.SessionID() != sid {
		t.Fatalf("resume changed session: %s", next.SessionID())
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "retained" {
		t.Fatalf("restart lost provider storage: %q %v", b, err)
	}
}

func TestExclusiveGrokHomeRejectsMissingInvalidAndCollidingState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, sid := range []string{"", "..", "../escape", "missing"} {
		if _, err := exclusiveGrokHomeForStart(sid, true); err == nil {
			t.Errorf("required resume accepted %q", sid)
		}
	}
	one, err := exclusiveGrokHomeForStart("one", false)
	if err != nil {
		t.Fatal(err)
	}
	two, err := exclusiveGrokHomeForStart("two", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishExclusiveGrokHome(one, "actual"); err != nil {
		t.Fatal(err)
	}
	if err := publishExclusiveGrokHome(one, "actual"); err != nil {
		t.Fatal(err)
	}
	if err := publishExclusiveGrokHome(two, "actual"); err == nil {
		t.Fatal("overwrote another conversation home")
	}
	bad := exclusiveGrokHomeDir("not-a-directory")
	if err := os.WriteFile(bad, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := exclusiveGrokHomeForStart("not-a-directory", true); err == nil {
		t.Fatal("accepted non-directory home")
	}
	if err := os.Symlink(filepath.Join(one, "absent"), exclusiveGrokHomeDir("dangling")); err != nil {
		t.Fatal(err)
	}
	if _, err := exclusiveGrokHomeForStart("dangling", true); err == nil {
		t.Fatal("accepted dangling required home")
	}
	if err := publishExclusiveGrokHome(one, "dangling"); err == nil {
		t.Fatal("overwrote dangling mapping")
	}
}

func TestExclusiveGrokHomeRefreshKeepsConversationAndExcludesUserMCP(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	user := t.TempDir()
	t.Setenv("HOME", user)
	if err := os.Mkdir(filepath.Join(user, ".grok"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, ".grok", "auth.json"), []byte(`{"fake":"auth"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, ".grok", "config.toml"), []byte("user-mcp-secret-marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	home, err := exclusiveGrokHomeForStart("session", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "session-data"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := exclusiveGrokHomeForStart("session", true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil || strings.Contains(string(b), "user-mcp-secret-marker") || strings.Count(string(b), "mcps = false") != 2 {
		t.Fatalf("isolation config: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(home, "session-data")); err != nil || string(b) != "keep" {
		t.Fatalf("refresh lost session: %q %v", b, err)
	}
}

// Real provider persistence is the oracle: the successor is never told the
// retained fact. Run both stdio and detached serve, through ordinary Start/Stop.
func TestExclusiveGrokSessionResumeLive(t *testing.T) {
	if os.Getenv("CLAUDIA_GROK_LIVE") == "" {
		t.Skip("CLAUDIA_GROK_LIVE not set")
	}
	for _, connect := range []bool{false, true} {
		t.Run(fmt.Sprint(connect), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv(EnvGrokConnect, "0")
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			cfg := Config{Provider: ProviderGrok, WorkDir: t.TempDir(), MCPExclusive: true, GrokConnect: connect, TermLogPath: "-"}
			agent, err := Start(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer agent.Stop()
			direct := func(a *Agent, prompt, expected string) {
				t.Helper()
				if err := a.WaitReady(ctx); err != nil {
					t.Fatal(err)
				}
				// Subscribe before Send; replay without a live prompt identity does
				// not count. Grok sends append fragments, including empty terminals.
				const eventBuffer = 256
				events := make(chan Event, eventBuffer)
				var overflow atomic.Bool
				sub := a.SubscribeEvents(func(ev Event) {
					select {
					case events <- ev:
					default:
						overflow.Store(true)
					}
				})
				defer a.UnsubscribeEvents(sub)
				if err := a.Send(prompt); err != nil {
					t.Fatal(err)
				}
				var text strings.Builder
				turn := ""
				for {
					select {
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case ev := <-events:
						if overflow.Load() {
							t.Fatal("provider event observation overflowed")
						}
						if ev.SessionID != a.SessionID() || ev.TurnID == "" {
							continue
						}
						if turn == "" {
							turn = ev.TurnID
						}
						if ev.TurnID != turn {
							t.Fatal("multiple provider turns in one direct")
						}
						if ev.Type == "assistant" {
							text.WriteString(ev.Text)
						}
						if ev.IsTerminalStop() {
							if strings.TrimSpace(text.String()) != expected {
								t.Fatalf("reply=%q, want=%q", text.String(), expected)
							}
							return
						}
					}
				}
			}
			secret, ack := "remember-"+uuid.NewString(), "stored-"+uuid.NewString()
			direct(agent, "Remember this fact for my next question: "+secret+". Reply with exactly: "+ack, ack)
			sid := agent.SessionID()
			original, err := filepath.EvalSymlinks(exclusiveGrokHomeDir(sid))
			if err != nil {
				t.Fatal(err)
			}
			agent.Stop()
			if _, err := os.Stat(original); err != nil {
				t.Fatalf("Stop removed provider home: %v", err)
			}
			cfg.SessionID, cfg.RequireResume = sid, true
			next, err := Start(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Stop()
			if next.SessionID() != sid {
				t.Fatalf("changed session: %s", next.SessionID())
			}
			challenge := "now-" + uuid.NewString()
			direct(next, "What fact did I ask you to remember? Reply with exactly two words: that fact, then "+challenge+". No other text.", secret+" "+challenge)
		})
	}
}

func TestExclusiveGrokHomeRejectsRedirectionWithoutChangingVictim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	victim := t.TempDir()
	victimFile := filepath.Join(victim, "config.toml")
	if err := os.WriteFile(victimFile, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	home, err := exclusiveGrokHomeForStart("safe", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, exclusiveGrokHomeDir("escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := exclusiveGrokHomeForStart("escape", true); err == nil {
		t.Fatal("accepted external home alias")
	}
	config := filepath.Join(home, "config.toml")
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victimFile, config); err != nil {
		t.Fatal(err)
	}
	if _, err := exclusiveGrokHomeForStart("safe", true); err == nil {
		t.Fatal("accepted redirected configuration")
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(victimFile, config); err != nil {
		t.Fatal(err)
	}
	if _, err := exclusiveGrokHomeForStart("safe", true); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victimFile, filepath.Join(home, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := exclusiveGrokHomeForStart("safe", true); err == nil {
		t.Fatal("accepted redirected auth without a user auth source")
	}
	if b, err := os.ReadFile(victimFile); err != nil || string(b) != "untouched" {
		t.Fatalf("modified victim: %q %v", b, err)
	}
}

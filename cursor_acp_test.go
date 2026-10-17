// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"time"

	"github.com/google/uuid"
	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

func TestCursorACPCloseKillsAfterReadLoopClosed(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	c := &cursorACPClient{cmd: cmd, ownsProcess: true, closed: true}
	c.Close()

	backstop := wallclockguard.UntilTestTimeout(t)
	for backstop.Err() == nil {
		out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "pid=").Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Close left pid %d alive after readLoop already marked closed", pid)
}

func writeFakeCursorACP(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake Cursor ACP uses a POSIX shell wrapper")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for fake ACP server")
	}
	py, err := filepath.Abs("testdata/cursor/acp/fake_acp.py")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	script := "#!/bin/sh\n" +
		"# Ignore agent/acp flags; speak ACP on stdio.\n" +
		"exec python3 \"" + py + "\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestIsCursorResumeDeniedSeesDaemonWrappedError(t *testing.T) {
	wrapped := fmt.Errorf("broker protocol: agent_failed: acp session/load sid: Invalid params (%s)", ErrCursorResumeDenied)
	if !IsCursorResumeDenied(wrapped) {
		t.Fatal("daemon-wrapped resume denial not recognized")
	}
	if IsCursorResumeDenied(fmt.Errorf("acp session/load interrupted")) {
		t.Fatal("non-denial error matched")
	}
}

func TestCursorAgentBackendCapabilities(t *testing.T) {
	caps := ompAgentBackend{}.Capabilities()
	if caps.Task || !caps.Session || !caps.Resume {
		t.Fatalf("capabilities = %+v, want Session+Resume only", caps)
	}
}

func TestCursorRewindFailsWithCapabilityError(t *testing.T) {
	agent := &Agent{provider: ProviderCursor}
	_, err := agent.Rewind(1, Config{Provider: ProviderCursor})
	if err == nil {
		t.Fatal("Cursor Rewind returned nil error")
	}
	var capErr *CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("error = %T %v, want CapabilityError", err, err)
	}
	if capErr.Provider != ProviderCursor || capErr.Capability != CapabilityRewind {
		t.Errorf("CapabilityError = %+v", capErr)
	}
}

func TestCursorSessionLiveSmoke(t *testing.T) {
	if os.Getenv("CLAUDIA_CURSOR_LIVE") == "" {
		t.Skip("CLAUDIA_CURSOR_LIVE not set (this test spends API credit)")
	}
	if _, err := exec.LookPath("cursor-agent"); err != nil {
		t.Skipf("cursor agent binary not found: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	text, err := Run(ctx, "Reply with exactly: pong", Config{
		Provider:    ProviderCursor,
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(strings.ToLower(text), "pong") {
		t.Fatalf("response %q, want pong", text)
	}
}

// A provider that remains alive but withholds a startup response reproduces
// the development hang. Cancellation must finish cleanup, not abandon Start.

func TestCursorStartupCancellationAtEveryStage(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	bin := filepath.Join(t.TempDir(), "cursor")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"initialize", "authenticate", "session/load", "session/new"} {
		t.Run(method, func(t *testing.T) {
			c, err := startCursorACP(t.Context(), bin, t.TempDir(), "", "saved-conversation", false, nil, nil, nil, nil)
			if c != nil {
				c.Close()
				t.Fatal("returned a client")
			}
			if !errors.Is(err, ErrLegacyCLISeatTransport) {
				t.Fatalf("startCursorACP(%s) = %v, want legacy refusal", method, err)
			}
		})
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("startCursorACP executed the binary: %v", err)
	}
}

type cursorBlockedWriter struct {
	io.WriteCloser
	entered chan struct{}
}

func (w *cursorBlockedWriter) Write(p []byte) (int, error) {
	close(w.entered)
	return w.WriteCloser.Write(p)
}

func TestCursorRequestCancellationInterruptsBlockedWrite(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	writer := &cursorBlockedWriter{WriteCloser: w, entered: make(chan struct{})}
	c := &cursorACPClient{stdin: writer, pending: make(map[int64]chan acpRPCMessage)}
	defer c.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.requestContext(ctx, "initialize", nil); done <- err }()
	select {
	case <-writer.entered:
	case <-wallclockguard.UntilTestTimeout(t).Done():
		t.Fatal("request never reached blocked write")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want cancellation, got %v", err)
		}
	case <-wallclockguard.UntilTestTimeout(t).Done():
		t.Fatal("cancellation waited behind pipe write lock")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) != 0 {
		t.Fatalf("pending requests leaked: %d", len(c.pending))
	}
}

func TestCursorSuccessfulStartupDisarmsCancellation(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	bin := filepath.Join(t.TempDir(), "cursor")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := startCursorACP(t.Context(), bin, t.TempDir(), "", "saved-conversation", true, nil, nil, nil, nil)
	if c != nil {
		c.Close()
		t.Fatal("returned a client")
	}
	if !errors.Is(err, ErrLegacyCLISeatTransport) {
		t.Fatalf("startCursorACP = %v, want legacy refusal", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("startCursorACP executed the binary: %v", statErr)
	}
}

// eventDeadline is a context whose deadline is an event rather than a
// duration: it reports context.DeadlineExceeded once fired is closed, and
// nothing before. A test that needs a deadline to expire in a particular
// state fires it from that state instead of guessing how long reaching the
// state will take.
type eventDeadline struct {
	context.Context
	fired chan struct{}
}

func (d *eventDeadline) Done() <-chan struct{} { return d.fired }

func (d *eventDeadline) Err() error {
	select {
	case <-d.fired:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func TestCursorStartupDeadlineDoesNotPoisonResume(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	bin := filepath.Join(t.TempDir(), "cursor")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := startCursorACP(t.Context(), bin, t.TempDir(), "", "saved-conversation", true, nil, nil, nil, nil)
	if c != nil {
		c.Close()
		t.Fatal("returned a client")
	}
	if !errors.Is(err, ErrLegacyCLISeatTransport) || IsCursorResumeDenied(err) {
		t.Fatalf("startCursorACP = %v, want legacy refusal", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("startCursorACP executed the binary: %v", statErr)
	}
}

func TestCursorSavedSessionResumeLive(t *testing.T) {
	if os.Getenv("CLAUDIA_CURSOR_LIVE") == "" {
		t.Skip("CLAUDIA_CURSOR_LIVE not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	cfg := Config{Provider: ProviderCursor, WorkDir: t.TempDir(), MCPExclusive: true, TermLogPath: "-"}
	a, err := Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	direct := func(a *Agent, prompt, expected string) {
		t.Helper()
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
					t.Fatal("provider observation overflowed")
				}
				if ev.SessionID != a.SessionID() || ev.TurnID == "" {
					continue
				}
				if turn == "" {
					turn = ev.TurnID
				}
				if ev.TurnID != turn {
					t.Fatal("multiple turns observed for one request")
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
	direct(a, "Remember this fact for my next question: "+secret+". Reply with exactly: "+ack, ack)
	sid := a.SessionID()
	a.Stop()
	cfg.SessionID, cfg.RequireResume = sid, true
	next, err := Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Stop()
	if next.SessionID() != sid {
		t.Fatalf("session changed to %s", next.SessionID())
	}
	challenge := "now-" + uuid.NewString()
	direct(next, "What fact did I ask you to remember? Reply with exactly two words: that fact, then "+challenge+". No other text.", secret+" "+challenge)
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
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

func TestHermeticCursorSessionStartSendWait(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)

	workDir := t.TempDir()
	agent, err := Start(Config{
		Provider:    ProviderCursor,
		WorkDir:     workDir,
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	if agent.SessionID() == "" {
		t.Fatal("empty session id")
	}
	if !agent.Alive() {
		t.Fatal("agent not alive")
	}
	if agent.PID() <= 0 {
		t.Fatal("cursor ACP start must record the child PID (🎯T541.1)")
	}

	ctx := t.Context()
	type outcome struct {
		text string
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		text, err := agent.WaitForResponse(ctx)
		ch <- outcome{text, err}
	}()
	runtime.Gosched()

	if err := agent.Send("Reply with exactly: pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case <-ctx.Done():
		t.Fatal("timeout waiting for response")
	case out := <-ch:
		if out.err != nil {
			t.Fatalf("WaitForResponse: %v", out.err)
		}
		if !strings.Contains(out.text, "pong") {
			t.Fatalf("response %q, want pong", out.text)
		}
	}
}

func TestHermeticCursorSessionRunHelper(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)

	ctx := t.Context()
	text, err := Run(ctx, "Reply with exactly: pong", Config{
		Provider:    ProviderCursor,
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(text, "pong") {
		t.Fatalf("Run text %q, want pong", text)
	}
}

func TestHermeticCursorSessionLoad(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)

	agent, err := Start(Config{
		Provider:    ProviderCursor,
		WorkDir:     t.TempDir(),
		SessionID:   "sess-resume-me",
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	if agent.SessionID() != "sess-resume-me" {
		t.Fatalf("SessionID = %q, want sess-resume-me", agent.SessionID())
	}
}

func TestHermeticCursorLoadSurvivesMultiMegabyteJSONLine(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_HUGE_LOAD", "1")

	agent, err := Start(Config{
		Provider:      ProviderCursor,
		WorkDir:       t.TempDir(),
		SessionID:     "sess-huge-replay",
		RequireResume: true,
		TermLogPath:   "-",
	})
	if err != nil {
		t.Fatalf("session/load with a >1MiB JSON-RPC line: %v", err)
	}
	defer agent.Stop()
	if agent.SessionID() != "sess-huge-replay" {
		t.Fatalf("session=%q", agent.SessionID())
	}
}

func TestHermeticCursorLoadFailsClosedWhenRequireResume(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_REJECT_LOAD", "1")

	agent, err := Start(Config{
		Provider:      ProviderCursor,
		WorkDir:       t.TempDir(),
		SessionID:     "sess-exists",
		RequireResume: true,
		TermLogPath:   "-",
	})
	if err == nil {
		agent.Stop()
		t.Fatal("Start must fail closed when load fails for an existing conversation")
	}
	if !strings.Contains(err.Error(), "refusing to mint a replacement session") {
		t.Fatalf("error %q lacks the fail-closed explanation", err)
	}
	if !IsCursorResumeDenied(err) {
		t.Fatalf("error %v is not ErrCursorResumeDenied", err)
	}
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

func TestHermeticCursorLoadFailsClosedWhenStoreExists(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_REJECT_LOAD", "1")
	home := t.TempDir()
	t.Setenv("HOME", home)
	sid := "sess-has-store"
	dir := filepath.Join(home, ".cursor", "acp-sessions", sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "store.db"), []byte("sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}

	agent, err := Start(Config{
		Provider:    ProviderCursor,
		WorkDir:     t.TempDir(),
		SessionID:   sid,
		TermLogPath: "-",
	})
	if err == nil {
		agent.Stop()
		t.Fatal("Start must not session/new when store.db already exists")
	}
	if !strings.Contains(err.Error(), "refusing to mint a replacement session") {
		t.Fatalf("error %q", err)
	}
	if !IsCursorResumeDenied(err) {
		t.Fatalf("error %v is not ErrCursorResumeDenied", err)
	}
}

func TestHermeticCursorLoadFallsThroughForMintedID(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_REJECT_LOAD", "1")

	agent, err := Start(Config{
		Provider:    ProviderCursor,
		WorkDir:     t.TempDir(),
		SessionID:   "sess-never-materialized",
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start should mint a new session for an unmaterialized id: %v", err)
	}
	defer agent.Stop()
	if agent.SessionID() == "" {
		t.Fatal("empty session id after session/new fallback")
	}
	if agent.SessionID() == "sess-never-materialized" {
		t.Fatal("fake rejected load but id unchanged — fallback did not run")
	}
}

func TestHermeticCursorHyphenPermissionOptionID(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_CURSOR_PERMISSION", "1")

	agent, err := Start(Config{
		Provider:    ProviderCursor,
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	ctx := t.Context()
	type outcome struct {
		text string
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		text, err := agent.WaitForResponse(ctx)
		ch <- outcome{text, err}
	}()
	runtime.Gosched()

	if err := agent.Send("run a tool then reply pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case <-ctx.Done():
		t.Fatal("timeout waiting for response (permission round-trip stuck?)")
	case out := <-ch:
		if out.err != nil {
			t.Fatalf("WaitForResponse: %v", out.err)
		}
		if !strings.Contains(out.text, "pong") {
			t.Fatalf("response %q, want pong after hyphen permission grant", out.text)
		}
	}
}

func TestHermeticCursorAskQuestionDoesNotStall(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_CURSOR_ASK", "1")

	text, err := Run(t.Context(), "Reply with exactly: pong", Config{
		Provider:    ProviderCursor,
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(text, "pong") {
		t.Fatalf("Run text %q, want pong after skipped ask_question", text)
	}
}

func TestCursorAgentBackendCapabilities(t *testing.T) {
	caps := cursorAgentBackend{}.Capabilities()
	if caps.Task || !caps.Session || !caps.Resume {
		t.Fatalf("capabilities = %+v, want Session+Resume only", caps)
	}
}

func TestCursorTaskBackendCapabilities(t *testing.T) {
	caps := cursorTaskBackend{}.Capabilities()
	if !caps.Task || !caps.Resume || caps.Session {
		t.Fatalf("capabilities = %+v, want Task+Resume", caps)
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
	if _, err := resolveCursorBin(); err != nil {
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
	for _, method := range []string{"initialize", "authenticate", "session/load", "session/new"} {
		t.Run(method, func(t *testing.T) {
			bin := writeFakeCursorACP(t)
			logPath := filepath.Join(t.TempDir(), "requests.jsonl")
			t.Setenv("FAKE_ACP_REQUEST_LOG", logPath)
			t.Setenv("FAKE_ACP_WITHHOLD", method)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sid := "saved-conversation"
			if method == "session/new" {
				sid = ""
			}
			done := make(chan error, 1)
			go func() {
				c, err := startCursorACP(ctx, bin, t.TempDir(), "", sid, false, nil, nil, nil, nil)
				if c != nil {
					c.Close()
				}
				done <- err
			}()
			var pid int
			deadline := wallclockguard.UntilTestTimeout(t)
			// 🎯T97 exemption: a poll interval. A tick only re-reads state; the
			// wait's one failure is the UntilTestTimeout case, not this clock.
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
		wait:
			for {
				select {
				case <-deadline.Done():
					t.Fatal("provider did not reach ", method)
				case err := <-done:
					t.Fatalf("startup returned before cancellation: %v", err)
				case <-tick.C:
					b, _ := os.ReadFile(logPath)
					for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
						var req struct {
							PID    int
							Method string
						}
						if json.Unmarshal([]byte(line), &req) == nil && req.Method == method {
							pid = req.PID
							break wait
						}
					}
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("want cancellation, got %v", err)
				}
				if IsCursorResumeDenied(err) {
					t.Fatalf("cancellation poisoned resume refusal: %v", err)
				}
			case <-wallclockguard.UntilTestTimeout(t).Done():
				t.Fatal("startup cancellation did not finish")
			}
			if processAlive(pid) {
				t.Fatalf("canceled startup left owned process %d alive", pid)
			}
			b, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if sid != "" && strings.Contains(string(b), `"session/new"`) {
				t.Fatalf("cancellation reminted saved conversation: %s", b)
			}
		})
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
	bin := writeFakeCursorACP(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := make(chan Event, 8)
	c, err := startCursorACP(ctx, bin, t.TempDir(), "", "saved-conversation", true, nil, nil, func(ev Event) { events <- ev }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cancel()
	if c.SessionID() != "saved-conversation" {
		t.Fatalf("changed session: %s", c.SessionID())
	}
	if err := c.Prompt("pong"); err != nil {
		t.Fatalf("parent cancellation killed handed-off session: %v", err)
	}
	deadline := wallclockguard.UntilTestTimeout(t)
	var reply strings.Builder
	for {
		select {
		case <-deadline.Done():
			t.Fatal("handed-off session did not complete a fresh reply after cancellation")
		case ev := <-events:
			if ev.SessionID != "saved-conversation" || ev.TurnID == "" {
				continue
			}
			if ev.Type == "assistant" {
				reply.WriteString(ev.Text)
			}
			if ev.IsTerminalStop() {
				if reply.String() != "pong" {
					t.Fatalf("fresh reply=%q", reply.String())
				}
				return
			}
		}
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
	bin := writeFakeCursorACP(t)
	t.Setenv("FAKE_ACP_WITHHOLD", "session/load")
	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	t.Setenv("FAKE_ACP_REQUEST_LOG", logPath)
	// The deadline has to expire while the peer is withholding
	// session/load, because that is the state under test. A 1s
	// context.WithTimeout used to stand in for it, and on a loaded host it
	// could expire before the fake had even logged the request — failing
	// the last assertion below for a reason that was the host's (🎯T97).
	// So the deadline is an event: it fires once the log shows the
	// withheld request, and not before.
	ctx := &eventDeadline{Context: t.Context(), fired: make(chan struct{})}
	go func() {
		for {
			b, _ := os.ReadFile(logPath)
			if strings.Contains(string(b), `"session/load"`) {
				close(ctx.fired)
				return
			}
			if t.Context().Err() != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	c, err := startCursorACP(ctx, bin, t.TempDir(), "", "saved-conversation", true, nil, nil, nil, nil)
	if c != nil {
		c.Close()
		t.Fatal("deadline returned a client")
	}
	if !errors.Is(err, context.DeadlineExceeded) || IsCursorResumeDenied(err) {
		t.Fatalf("deadline classified as %v", err)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"session/load"`) || strings.Contains(string(b), `"session/new"`) {
		t.Fatalf("did not test saved-session timeout: %s", b)
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

// TestHermeticCursorSessionMultiChunkReply is the hermetic twin of the
// live TestCursorSessionLiveSmoke failure (🎯T79): the fake peer streams
// "pong" as the two deltas a real agent emits, and WaitForResponse must
// hand back the word, not "p\nong". The single-chunk fixture above
// cannot see this — it never splits a reply.
func TestHermeticCursorSessionMultiChunkReply(t *testing.T) {
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)
	t.Setenv("FAKE_ACP_CHUNKS", "p|ong")

	agent, err := Start(Config{
		Provider:    ProviderCursor,
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	ctx := t.Context()
	type outcome struct {
		text string
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		text, err := agent.WaitForResponse(ctx)
		ch <- outcome{text, err}
	}()
	runtime.Gosched()

	if err := agent.Send("Reply with exactly: pong"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case <-ctx.Done():
		t.Fatal("timeout waiting for response")
	case out := <-ch:
		if out.err != nil {
			t.Fatalf("WaitForResponse: %v", out.err)
		}
		if out.text != "pong" {
			t.Fatalf("response %q, want pong (streamed deltas must concatenate, not join on newlines)", out.text)
		}
	}
}

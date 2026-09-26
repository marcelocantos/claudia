// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func TestGrantHelpMentionsSeatFlags(t *testing.T) {
	errOut := captureStderr(t, func() error {
		if err := grantCmd([]string{"-h"}); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	for _, want := range []string{
		"--provider", "--pick", "--workdir", "--purpose", "--parent", "--send", "--mode", "--wait", "--release",
	} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("grant -h missing %q:\n%s", want, errOut)
		}
	}
}

func TestSendAndEventsHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"send", "-h"}, "--mode"},
		{[]string{"interrupt", "-h"}, "usage: claudia broker interrupt"},
		{[]string{"events", "-h"}, "--wait"},
	} {
		errOut := captureStderr(t, func() error {
			if code := run(append([]string{"broker"}, tc.args...)); code != 0 {
				t.Fatalf("%v: exit %d", tc.args, code)
			}
			return nil
		})
		if !strings.Contains(errOut, tc.want) {
			t.Fatalf("%v missing %q:\n%s", tc.args, tc.want, errOut)
		}
	}
}

func TestBrokerSubcommandHelp(t *testing.T) {
	errOut := captureStderr(t, func() error {
		if code := run([]string{"broker", "-h"}); code != 0 {
			t.Fatalf("exit %d", code)
		}
		return nil
	})
	if !strings.Contains(errOut, "grant|send|interrupt|events") {
		t.Fatalf("broker -h missing seat verbs:\n%s", errOut)
	}
}

func TestGrantRejectsUnknownPurposeBeforeDial(t *testing.T) {
	t.Setenv(broker.SocketPathEnv, filepath.Join(t.TempDir(), "missing.sock"))
	err := grantCmd([]string{
		"--name", "pimp-smoke-1", "--provider", "grok", "--purpose", "pimp",
	})
	if err == nil || !strings.Contains(err.Error(), "purpose") {
		t.Fatalf("error = %v, want a purpose refusal", err)
	}
	if strings.Contains(err.Error(), "no daemon") {
		t.Fatalf("rejected purpose dialed anyway: %v", err)
	}
}

func TestQueueWaitRefusedBeforeDial(t *testing.T) {
	t.Setenv(broker.SocketPathEnv, filepath.Join(t.TempDir(), "missing.sock"))
	err := sendCmd([]string{"--mode", "queue", "--wait", "--text", "later", "pimp-smoke-1"})
	if err == nil || !strings.Contains(err.Error(), "queue") {
		t.Fatalf("error = %v, want a queue refusal", err)
	}
	if strings.Contains(err.Error(), "no daemon") {
		t.Fatalf("refused --wait dialed anyway: %v", err)
	}
}

func TestTurnFoldMatchesWaitForResponse(t *testing.T) {
	var blocks turnFold
	blocks.add(claudia.Event{Type: "assistant", Text: "one", StopReason: "end_turn"})
	blocks.add(claudia.Event{Type: "assistant", Text: "two", StopReason: "end_turn"})
	if blocks.text() != "one\ntwo" || !blocks.terminal {
		t.Fatalf("blocks = %q terminal=%v", blocks.text(), blocks.terminal)
	}

	var deltas turnFold
	deltas.add(claudia.Event{Type: "assistant", Text: "p", PreviewUpdate: claudia.PreviewUpdateAppend})
	deltas.add(claudia.Event{Type: "assistant", Text: "ong", PreviewUpdate: claudia.PreviewUpdateAppend, StopReason: "end_turn"})
	if deltas.text() != "pong" || !deltas.terminal {
		t.Fatalf("deltas = %q terminal=%v", deltas.text(), deltas.terminal)
	}

	var tool turnFold
	if tool.add(claudia.Event{Type: "assistant", Text: "calling", StopReason: "tool_use"}) {
		t.Fatal("tool_use restarted the settle")
	}
	if tool.terminal {
		t.Fatal("tool_use is not a finished turn")
	}

	var failed turnFold
	failed.add(claudia.Event{Type: "assistant", Text: "model_not_found", IsError: true, StopReason: "end_turn"})
	if failed.err == nil || !strings.Contains(failed.err.Error(), "model_not_found") {
		t.Fatalf("error event = %v", failed.err)
	}
}

func TestGrantSendWaitReleaseOnWire(t *testing.T) {
	shortenSettle(t)
	fake := startSeatFake(t)
	// A terminal event replayed on grant is the previous turn. --wait after
	// --send must not return it.
	fake.beforeGrant = []claudia.Event{
		{Type: "assistant", Text: "stale", StopReason: "end_turn"},
	}
	fake.afterSend = []claudia.Event{
		{Type: "assistant", Text: "one", StopReason: "end_turn"},
		{Type: "assistant", Text: "two", StopReason: "end_turn"},
	}
	dir := t.TempDir()
	out := captureStdout(t, func() error {
		return grantCmd([]string{
			"--name", "pimp-smoke-1",
			"--provider", "grok",
			"--workdir", dir,
			"--parent", "pimp",
			"--purpose", "work",
			"--send", "ping",
			"--wait",
			"--release", "stop",
			"--timeout", "5s",
			"--json",
		})
	})
	var got struct {
		Grant struct {
			Name      string `json:"name"`
			Provider  string `json:"provider"`
			SessionID string `json:"session_id"`
		} `json:"grant"`
		Sent struct {
			Mode string `json:"mode"`
		} `json:"sent"`
		Text    string `json:"text"`
		Release string `json:"release"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout %q: %v", out, err)
	}
	if got.Grant.Name != "pimp-smoke-1" || got.Grant.Provider != "grok" || got.Grant.SessionID == "" {
		t.Fatalf("grant = %+v", got.Grant)
	}
	if got.Sent.Mode != string(broker.SendModeSubmit) {
		t.Fatalf("sent mode = %q", got.Sent.Mode)
	}
	if got.Text != "one\ntwo" {
		t.Fatalf("text = %q", got.Text)
	}
	if got.Release != string(broker.DispositionStop) {
		t.Fatalf("release = %q", got.Release)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.grant == nil || fake.send == nil || fake.release == nil {
		t.Fatalf("ops = %v grant=%v send=%v release=%v", fake.ops, fake.grant != nil, fake.send != nil, fake.release != nil)
	}
	def, err := claudia.DecodeGrantDefinition(fake.grant.Def)
	if err != nil {
		t.Fatal(err)
	}
	if def.Name != "pimp-smoke-1" || def.Provider != claudia.ProviderGrok || def.Parent != "pimp" || def.Purpose != claudia.PurposeWork || def.WorkDir != dir {
		t.Fatalf("def = %+v", def.AgentDef)
	}
	if fake.send.Text != "ping" || fake.send.Mode != broker.SendModeSubmit {
		t.Fatalf("send = %+v", fake.send)
	}
	if fake.release.Disposition != broker.DispositionStop || fake.release.Name != "pimp-smoke-1" {
		t.Fatalf("release = %+v", fake.release)
	}
	if strings.Join(fake.ops, ",") != "grant,send,release" {
		t.Fatalf("ops = %v", fake.ops)
	}
}

func TestGrantPrintsHumanLine(t *testing.T) {
	fake := startSeatFake(t)
	out := captureStdout(t, func() error {
		return grantCmd([]string{
			"--name", "pimp-smoke-2", "--provider", "cursor", "--workdir", t.TempDir(),
			"--parent", "pimp", "--timeout", "5s",
		})
	})
	if !strings.Contains(out, "granted pimp-smoke-2 provider=cursor session=") {
		t.Fatalf("stdout = %q", out)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.grant == nil || fake.grant.Adopt || fake.grant.Fallback {
		t.Fatalf("fresh grant = %+v", fake.grant)
	}
}

func TestSendReclaimsListedGrantAndSteers(t *testing.T) {
	shortenSettle(t)
	fake := startSeatFake(t)
	fake.listed = []broker.GrantStatus{{
		Name: "pimp-smoke-3", Provider: "grok", Model: "grok-4", SessionID: "sid-kept",
		WorkDir: "/work", Purpose: "work", Parent: "pimp",
	}}
	fake.afterSend = []claudia.Event{
		{Type: "assistant", Text: "steered", StopReason: "end_turn"},
	}
	out := captureStdout(t, func() error {
		return sendCmd([]string{
			"--mode", "steer", "--wait", "--timeout", "5s", "pimp-smoke-3", "consider", "this",
		})
	})
	if strings.TrimSpace(out) != "steered" {
		t.Fatalf("stdout = %q", out)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.grant == nil || !fake.grant.Adopt || !fake.grant.Fallback {
		t.Fatalf("reclaim grant = %+v", fake.grant)
	}
	def, err := claudia.DecodeGrantDefinition(fake.grant.Def)
	if err != nil {
		t.Fatal(err)
	}
	if def.SessionID != "sid-kept" || def.Provider != claudia.ProviderGrok || def.Parent != "pimp" || def.WorkDir != "/work" || def.Model != "grok-4" {
		t.Fatalf("reclaim def = %+v", def.AgentDef)
	}
	if fake.send == nil || fake.send.Mode != broker.SendModeSteer || fake.send.Text != "consider this" {
		t.Fatalf("send = %+v", fake.send)
	}
	want := []string{"grants", "grant", "send"}
	if strings.Join(fake.ops, ",") != strings.Join(want, ",") {
		t.Fatalf("ops = %v, want %v", fake.ops, want)
	}
}

func TestSendUnknownGrant(t *testing.T) {
	startSeatFake(t)
	err := sendCmd([]string{"--timeout", "5s", "--text", "hi", "missing"})
	if err == nil || !strings.Contains(err.Error(), "not one the daemon holds") {
		t.Fatalf("error = %v", err)
	}
}

func TestInterruptReclaimsThenCancels(t *testing.T) {
	fake := startSeatFake(t)
	fake.listed = []broker.GrantStatus{{
		Name: "pimp-smoke-4", Provider: "claude", SessionID: "sid-4", WorkDir: "/w",
	}}
	out := captureStdout(t, func() error {
		return interruptCmd([]string{"--timeout", "5s", "pimp-smoke-4"})
	})
	if strings.TrimSpace(out) != "interrupted pimp-smoke-4" {
		t.Fatalf("stdout = %q", out)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.interrupted != "pimp-smoke-4" {
		t.Fatalf("interrupted = %q ops=%v", fake.interrupted, fake.ops)
	}
	if strings.Join(fake.ops, ",") != "grants,grant,interrupt" {
		t.Fatalf("ops = %v", fake.ops)
	}
}

func TestEventsWaitFoldsReplay(t *testing.T) {
	shortenSettle(t)
	fake := startSeatFake(t)
	fake.listed = []broker.GrantStatus{{
		Name: "pimp-smoke-5", Provider: "grok", SessionID: "sid-5", WorkDir: "/w", Parent: "pimp",
	}}
	fake.beforeGrant = []claudia.Event{
		{Type: "assistant", Text: "from-replay", StopReason: "end_turn"},
	}
	out := captureStdout(t, func() error {
		return eventsCmd([]string{"--wait", "--timeout", "5s", "pimp-smoke-5"})
	})
	if strings.TrimSpace(out) != "from-replay" {
		t.Fatalf("stdout = %q", out)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if strings.Join(fake.ops, ",") != "grants,grant" {
		t.Fatalf("ops = %v", fake.ops)
	}
}

func TestEventsStreamsReplay(t *testing.T) {
	fake := startSeatFake(t)
	fake.listed = []broker.GrantStatus{{
		Name: "pimp-smoke-6", Provider: "grok", SessionID: "sid-6", WorkDir: "/w",
	}}
	fake.beforeGrant = []claudia.Event{
		{Type: "assistant", Text: "live-line", StopReason: "end_turn"},
	}
	out, err := captureFDErr(t, &os.Stdout, func() error {
		return eventsCmd([]string{"--timeout", "1s", "pimp-smoke-6"})
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v", err)
	}
	ev, jerr := claudia.DecodeEventWire(json.RawMessage(strings.TrimSpace(out)))
	if jerr != nil {
		t.Fatalf("stdout %q: %v", out, jerr)
	}
	if ev.Text != "live-line" || ev.StopReason != "end_turn" {
		t.Fatalf("event = %+v", ev)
	}
}

func shortenSettle(t *testing.T) {
	t.Helper()
	prev := turnSettle
	turnSettle = 40 * time.Millisecond
	t.Cleanup(func() { turnSettle = prev })
}

type seatFake struct {
	mu          sync.Mutex
	listed      []broker.GrantStatus
	grant       *broker.GrantRequest
	send        *broker.SendRequest
	release     *broker.ReleaseRequest
	interrupted string
	ops         []string
	afterSend   []claudia.Event
	beforeGrant []claudia.Event
}

func (f *seatFake) HandleRequest(c *broker.ClientConn, req *broker.Request) bool {
	f.mu.Lock()
	f.ops = append(f.ops, string(req.Type))
	switch req.Type {
	case broker.TypeGrants:
		listed := append([]broker.GrantStatus(nil), f.listed...)
		f.mu.Unlock()
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeGrantsResult, Grants: &broker.GrantsResponse{Grants: listed}})
		return true
	case broker.TypeGrant:
		cp := *req.Grant
		cp.Def = append(json.RawMessage(nil), req.Grant.Def...)
		f.grant = &cp
		before := append([]claudia.Event(nil), f.beforeGrant...)
		f.mu.Unlock()
		def, _ := claudia.DecodeGrantDefinition(cp.Def)
		sid := def.SessionID
		if sid == "" {
			sid = "sid-minted"
		}
		if err := writeEvents(c, cp.Name, before); err != nil {
			return true
		}
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeGranted, Granted: &broker.GrantResponse{
			Name: cp.Name, SessionID: sid, Provider: broker.Provider(def.Provider), Reclaimed: sid != "sid-minted",
		}})
		return true
	case broker.TypeSend:
		cp := *req.Send
		f.send = &cp
		after := append([]claudia.Event(nil), f.afterSend...)
		f.mu.Unlock()
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeSent, Sent: &broker.SentResponse{
			Name: cp.Name, Mode: cp.Mode, Mechanism: "test",
		}})
		_ = writeEvents(c, cp.Name, after)
		return true
	case broker.TypeInterrupt:
		f.interrupted = req.Interrupt.Name
		f.mu.Unlock()
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeInterrupted, Interrupted: &broker.NamedResponse{Name: req.Interrupt.Name}})
		return true
	case broker.TypeRelease:
		cp := *req.Release
		f.release = &cp
		f.mu.Unlock()
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeReleased, Released: &broker.ReleaseResponse{Name: cp.Name, Disposition: cp.Disposition}})
		return true
	case broker.TypeGoalVerdict:
		f.mu.Unlock()
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeGoalVerdictNoted, GoalVerdictNoted: &broker.NamedResponse{Name: req.GoalVerdict.Name}})
		return true
	default:
		f.mu.Unlock()
		return false
	}
}

func (f *seatFake) ConnClosed(*broker.ClientConn) {}

func writeEvents(c *broker.ClientConn, name string, evs []claudia.Event) error {
	for _, ev := range evs {
		raw, err := claudia.EncodeEventWire(ev)
		if err != nil {
			_ = c.Fail("", err)
			return err
		}
		if err := c.Reply(&broker.Response{Type: broker.TypeAgentEvent, AgentEvent: &broker.AgentEventMessage{Name: name, Event: raw}}); err != nil {
			return err
		}
	}
	return nil
}

func startSeatFake(t *testing.T) *seatFake {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "seat")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "b.sock")
	ln, err := broker.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	fake := &seatFake{}
	srv, err := broker.Serve(&broker.ServeArgs{Listener: ln, Handler: fake})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")
	return fake
}

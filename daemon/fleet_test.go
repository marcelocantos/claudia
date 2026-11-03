// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// The fleet path end to end (🎯T247), hermetically: the real daemon, wire,
// client, MCP host and Agent, with stub provider processes. A live run on real
// models is the target's separate oracle.

func fleetPickForTest(*claudia.FleetSeatSpec) (claudia.Provider, string, error) {
	return claudia.ProviderClaude, "test-model", nil
}

func (f *fixture) fleetOptions() Options {
	opts := f.options(nil)
	opts.DisableResume = true
	opts.fleetPick = fleetPickForTest
	return opts
}

func openTestFleet(t *testing.T, f *fixture) *claudia.Fleet {
	t.Helper()
	fleet, err := claudia.OpenFleet(wallclockguard.UntilTestTimeout(t), "home", nil)
	if err != nil {
		t.Fatal(err)
	}
	return fleet
}

func startTestAgent(t *testing.T, fleet *claudia.Fleet, name string) {
	t.Helper()
	err := fleet.Start(wallclockguard.UntilTestTimeout(t), &claudia.FleetStartArgs{Name: name, Instructions: "BRIEF", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
}

// answerTurn ends the seat's open turn the way a provider does.
func answerTurn(t *testing.T, f *fixture, grant, text string) {
	t.Helper()
	proc := f.d.reg.Get(grant)
	if proc == nil {
		t.Fatalf("no proc for %s", grant)
	}
	proc.PublishEvent(claudia.Event{Type: "assistant", Text: text, StopReason: "end_turn"})
}

// nextOwnerMessage reads events after *seq until one is addressed to the owner.
func nextOwnerMessage(t *testing.T, fleet *claudia.Fleet, seq *uint64) *claudia.FleetMessage {
	t.Helper()
	for {
		events, err := fleet.Read(wallclockguard.UntilTestTimeout(t), *seq)
		if err != nil {
			t.Fatal(err)
		}
		for i := range events {
			*seq = events[i].Seq
			if m := events[i].Message; m != nil && m.To == claudia.FleetOwner {
				return m
			}
		}
	}
}

// callSeatTool makes a tool call to the URL a seat was given.
func callSeatTool(t *testing.T, url, name string, args map[string]any) (string, bool) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s: HTTP %d: %v", name, resp.StatusCode, err)
	}
	if len(out.Result.Content) == 0 {
		t.Fatalf("%s: no content", name)
	}
	return out.Result.Content[0].Text, out.Result.IsError
}

func seatFleetURL(t *testing.T, s *seat) string {
	t.Helper()
	cfg := s.start().Config
	if !cfg.MCPExclusive || len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Name != claudia.FleetMCPServerName {
		t.Fatalf("fleet seat's MCP config: exclusive=%v servers=%+v", cfg.MCPExclusive, cfg.MCPServers)
	}
	return cfg.MCPServers[0].URL
}

func TestFleetSeatsCoordinateThroughTheBrokerWithNoConsumer(t *testing.T) {
	f := newFixture(t)
	f.bootWith(t, f.fleetOptions())
	fleet := openTestFleet(t, f)
	startTestAgent(t, fleet, "lead")
	lead := f.seat(0)
	url := seatFleetURL(t, lead)

	if text, isErr := callSeatTool(t, url, "start", map[string]any{"name": "helper", "instructions": "HELPER-BRIEF"}); isErr {
		t.Fatalf("start: %s", text)
	}
	waitFor(t, "helper's seat", func() bool { return f.seatCount() == 2 })
	helper := f.seat(1)
	if text, isErr := callSeatTool(t, url, "send", map[string]any{"to": "helper", "text": "do the job"}); isErr {
		t.Fatalf("send: %s", text)
	}
	waitFor(t, "helper's turn", func() bool { return len(helper.sent()) == 1 })
	if got := helper.sent()[0]; !strings.Contains(got, "HELPER-BRIEF") || !strings.Contains(got, "do the job") {
		t.Fatalf("helper got %q", got)
	}
	answerTurn(t, f, "fleet-home--helper", "job done")
	waitFor(t, "lead to hear", func() bool { return len(lead.sent()) == 1 })
	if got := lead.sent()[0]; !strings.Contains(got, "job done") {
		t.Fatalf("lead got %q", got)
	}
	answerTurn(t, f, "fleet-home--lead", "all finished")

	var seq uint64
	m := nextOwnerMessage(t, fleet, &seq)
	if m.Text != "all finished" || m.From != "lead" {
		t.Fatalf("owner got %+v", m)
	}
	st, err := fleet.Status(wallclockguard.UntilTestTimeout(t))
	if err != nil || len(st.Agents) != 2 || st.Agents[0].Model == "" {
		t.Fatalf("status %+v %v", st, err)
	}
	var grants []string
	for _, g := range f.d.grantList() {
		grants = append(grants, g.Name)
	}
	if got := strings.Join(grants, ","); !strings.Contains(got, "fleet-home--helper") || !strings.Contains(got, "fleet-home--lead") {
		t.Fatalf("grants do not show fleet seats: %s", got)
	}
}

func TestFleetStopEndsTheSeatAndTellsTheRequester(t *testing.T) {
	f := newFixture(t)
	f.bootWith(t, f.fleetOptions())
	fleet := openTestFleet(t, f)
	startTestAgent(t, fleet, "worker")
	worker := f.seat(0)
	ctx := wallclockguard.UntilTestTimeout(t)
	if _, err := fleet.Send(ctx, "worker", "long job", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn", func() bool { return len(worker.sent()) == 1 && worker.inFlight.Load() })
	if err := fleet.StopTree(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	if m := nextOwnerMessage(t, fleet, &seq); !strings.Contains(m.Text, "agent worker is gone") {
		t.Fatalf("owner got %+v", m)
	}
	if interrupts, _, _ := worker.counts(); interrupts == 0 {
		t.Fatal("the turn in progress was not interrupted")
	}
	if f.d.reg.Def("fleet-home--worker") != nil || f.d.reg.Get("fleet-home--worker") != nil {
		t.Fatal("seat left registered")
	}
	st, err := fleet.Status(ctx)
	if err != nil || len(st.Agents) != 0 {
		t.Fatalf("status %+v %v", st, err)
	}
}

func TestFleetSurvivesABrokerRestartMidTurn(t *testing.T) {
	f := newFixture(t)
	opts := f.fleetOptions()
	f.bootWith(t, opts)
	fleet := openTestFleet(t, f)
	startTestAgent(t, fleet, "worker")
	first := f.seat(0)
	oldURL := seatFleetURL(t, first)
	ctx := wallclockguard.UntilTestTimeout(t)
	id, err := fleet.Send(ctx, "worker", "unfinished job", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn", func() bool { return len(first.sent()) == 1 })
	before, err := fleet.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.d.Close(); err != nil {
		t.Fatal(err)
	}
	f.bootWith(t, opts)

	waitFor(t, "seat to come back", func() bool { return f.seatCount() == 2 })
	second := f.seat(1)
	waitFor(t, "the interrupted turn to be delivered again", func() bool { return len(second.sent()) == 1 })
	if got := second.sent()[0]; !strings.Contains(got, "unfinished job") || !strings.Contains(got, "Claudia restarted") {
		t.Fatalf("redelivery: %q", got)
	}
	if nudged := second.sent()[0]; strings.Contains(nudged, "restart-nudge") {
		t.Fatalf("a fleet seat got the broker's restart nudge: %q", nudged)
	}
	// The client carries on by name and sequence number, across the restart.
	answerTurn(t, f, "fleet-home--worker", "finished after all")
	seq := before.LastSeq
	var m *claudia.FleetMessage
	for m == nil || m.InReplyTo != id {
		m = nextOwnerMessage(t, fleet, &seq)
	}
	if m.Text != "finished after all" {
		t.Fatalf("owner got %+v", m)
	}
	// The seat's tools still work at its new address, though the listener moved.
	newURL := seatFleetURL(t, second)
	if newURL == oldURL {
		t.Log("the listener happened to keep its port")
	}
	if text, isErr := callSeatTool(t, newURL, "list", nil); isErr || !strings.Contains(text, `"worker"`) {
		t.Fatalf("list after restart: %q", text)
	}
}

func TestSeatsOutsideAFleetGetNoFleetTools(t *testing.T) {
	f := newFixture(t)
	f.bootWith(t, f.fleetOptions())
	a, err := claudia.Start(claudia.Config{Name: "plain", Provider: claudia.ProviderCursor, WorkDir: t.TempDir(), SessionID: "sid-plain", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Detach()
	for _, s := range f.seat(0).start().Config.MCPServers {
		if s.Name == claudia.FleetMCPServerName {
			t.Fatalf("an ordinary seat was given %+v", s)
		}
	}
	if st, err := f.d.fleet.Status("home"); err == nil {
		t.Fatalf("a fleet exists that nobody opened: %+v", st)
	}
}

func TestFleetBoundaryFailuresAreReported(t *testing.T) {
	f := newFixture(t)
	opts := f.fleetOptions()
	opts.fleetPick = func(*claudia.FleetSeatSpec) (claudia.Provider, string, error) {
		return "", "", context.DeadlineExceeded
	}
	f.bootWith(t, opts)
	fleet := openTestFleet(t, f)
	ctx := wallclockguard.UntilTestTimeout(t)
	err := fleet.Start(ctx, &claudia.FleetStartArgs{Name: "a", Instructions: "x", WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "no model satisfies") {
		t.Fatalf("start with no model: %v", err)
	}
	if err := fleet.Stop(ctx, "ghost"); err == nil {
		t.Fatal("stopped an agent that does not exist")
	}
	if _, err := claudia.OpenFleet(ctx, "Bad Name", nil); err == nil {
		t.Fatal("opened a badly named fleet")
	}
}

func TestDamagedFleetStateDisablesFleetsAndNotTheBroker(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.state, fleetDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fleet-state.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.bootWith(t, f.fleetOptions())
	ctx := wallclockguard.UntilTestTimeout(t)
	if _, err := claudia.OpenFleet(ctx, "home", nil); err == nil || !strings.Contains(err.Error(), "damaged state") {
		t.Fatalf("open on damaged state: %v", err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "{broken" {
		t.Fatalf("state rewritten: %q", raw)
	}
	a, err := claudia.Start(claudia.Config{Name: "plain", Provider: claudia.ProviderCursor, WorkDir: t.TempDir(), SessionID: "sid-plain", TermLogPath: "-"})
	if err != nil {
		t.Fatalf("an ordinary seat could not start: %v", err)
	}
	_ = a.Detach()
}

// Review finding 9: nobody claims a fleet seat, so its events must not pile up.
func TestFleetSeatEventsDoNotAccumulateInTheBroker(t *testing.T) {
	f := newFixture(t)
	f.bootWith(t, f.fleetOptions())
	fleet := openTestFleet(t, f)
	startTestAgent(t, fleet, "worker")
	proc := f.d.reg.Get("fleet-home--worker")
	for range 50 {
		proc.PublishEvent(claudia.Event{Type: "assistant", Text: "chatter"})
	}
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	if g := f.d.grants["fleet-home--worker"]; g == nil || len(g.ring) != 0 {
		t.Fatalf("grant %+v retained events", g)
	}
}

// Review finding 10: a client cannot take over or stop a fleet seat.
func TestClientsCannotGrantOrReleaseAFleetSeat(t *testing.T) {
	f := newFixture(t)
	f.bootWith(t, f.fleetOptions())
	fleet := openTestFleet(t, f)
	startTestAgent(t, fleet, "worker")
	before := f.d.reg.Def("fleet-home--worker")
	_, err := claudia.Start(claudia.Config{Name: "fleet-home--worker", Provider: claudia.ProviderCursor, WorkDir: t.TempDir(), SessionID: "sid-thief", TermLogPath: "-"})
	if err == nil || !strings.Contains(err.Error(), "fleet seat") {
		t.Fatalf("grant of a fleet seat: %v", err)
	}
	if after := f.d.reg.Def("fleet-home--worker"); after.SessionID != before.SessionID || len(after.MCPServers) != 1 || after.AutoStart {
		t.Fatalf("definition changed: %+v", after)
	}
}

// Review finding 6: names cannot collide across fleets, and an ordinary seat is
// never cleared as if it were a stale fleet seat.
func TestFleetGrantNamesAreUnambiguous(t *testing.T) {
	f := newFixture(t)
	f.bootWith(t, f.fleetOptions())
	ctx := wallclockguard.UntilTestTimeout(t)
	one, err := claudia.OpenFleet(ctx, "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	two, err := claudia.OpenFleet(ctx, "a-b", nil)
	if err != nil {
		t.Fatal(err)
	}
	startTestAgent(t, one, "b-c")
	startTestAgent(t, two, "c")
	if f.seatCount() != 2 || f.d.reg.Def(fleetGrantName("a", "b-c")) == nil || f.d.reg.Def(fleetGrantName("a-b", "c")) == nil {
		t.Fatal("two agents shared a seat")
	}
	if !isFleetGrantName(fleetGrantName("a", "b")) || isFleetGrantName("fleet-ordinary") || isFleetGrantName("worker--x") ||
		isFleetGrantName("fleet-X--Y") || isFleetGrantName("fleet---x") {
		t.Fatal("namespace check is wrong")
	}
}

// Review finding 12: a crash can leave a seat with no agent behind it.
func TestOrphanFleetSeatIsRemovedAtBoot(t *testing.T) {
	f := newFixture(t)
	opts := f.fleetOptions()
	f.bootWith(t, opts)
	fleet := openTestFleet(t, f)
	startTestAgent(t, fleet, "kept")
	orphan := fleetGrantName("home", "orphan")
	if err := f.d.reg.Register(claudia.AgentDef{Name: orphan, Provider: claudia.ProviderCursor, WorkDir: t.TempDir(), SessionID: "sid-orphan",
		MCPServers: []claudia.MCPServer{{Name: claudia.FleetMCPServerName, Type: "http", URL: "http://127.0.0.1:1/upstream/fleet/tok"}}}); err != nil {
		t.Fatal(err)
	}
	// Re-review finding 3: ordinary seats whose names look like fleet seats stay.
	for _, name := range []string{"ordinary", "fleet-ops--bot", "fleet-X--Y", "fleet---x"} {
		if err := f.d.reg.Register(claudia.AgentDef{Name: name, Provider: claudia.ProviderCursor, WorkDir: t.TempDir(), SessionID: "sid-" + name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.d.Close(); err != nil {
		t.Fatal(err)
	}
	f.bootWith(t, opts)
	if f.d.reg.Def(orphan) != nil {
		t.Fatal("orphan survived boot")
	}
	for _, name := range []string{"ordinary", "fleet-ops--bot", "fleet-X--Y", "fleet---x", fleetGrantName("home", "kept")} {
		if f.d.reg.Def(name) == nil {
			t.Fatalf("removed %s, which should have stayed", name)
		}
	}
}

// Review finding 2, through the real wire.
func TestFleetBacklogLargerThanAFrameReachesAClient(t *testing.T) {
	f := newFixture(t)
	f.bootWith(t, f.fleetOptions())
	fleet := openTestFleet(t, f)
	startTestAgent(t, fleet, "worker")
	ctx := wallclockguard.UntilTestTimeout(t)
	const messages = 12
	text := strings.Repeat("x", 100<<10)
	for range messages {
		if _, err := fleet.Send(ctx, "worker", text, 0); err != nil {
			t.Fatal(err)
		}
	}
	st, err := fleet.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seq uint64
	seen := 0
	for seq < st.LastSeq {
		events, err := fleet.Read(ctx, seq)
		if err != nil {
			t.Fatal(err)
		}
		for i := range events {
			seq = events[i].Seq
			if events[i].Message != nil {
				seen++
			}
		}
	}
	if seen != messages {
		t.Fatalf("saw %d of %d messages", seen, messages)
	}
}

func TestSeatThatDiesInATurnIsBroughtBack(t *testing.T) {
	f := newFixture(t)
	f.bootWith(t, f.fleetOptions())
	fleet := openTestFleet(t, f)
	startTestAgent(t, fleet, "worker")
	first := f.seat(0)
	ctx := wallclockguard.UntilTestTimeout(t)
	if _, err := fleet.Send(ctx, "worker", "the job", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn", func() bool { return len(first.sent()) == 1 })
	first.exit()
	waitFor(t, "a new seat", func() bool { return f.seatCount() >= 2 })
	waitFor(t, "the job to go round again", func() bool {
		sends := f.seat(f.seatCount() - 1).sent()
		return len(sends) == 1 && strings.Contains(sends[0], "the job")
	})
}

// 🎯T248 through the real wire: checks, scheduled messages and budgets.
func TestFleetChecksTimersAndBudgetsOverTheWire(t *testing.T) {
	f := newFixture(t)
	f.bootWith(t, f.fleetOptions())
	fleet := openTestFleet(t, f)
	startTestAgent(t, fleet, "worker")
	ctx := wallclockguard.UntilTestTimeout(t)

	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "master"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"}, {"commit", "-q", "--allow-empty", "-m", "start"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := fleet.RegisterCheck(ctx, &claudia.FleetCheckArgs{Name: "answer", Repo: repo, Script: "echo 42\n"}); err != nil {
		t.Fatal(err)
	}
	id, err := fleet.RunCheck(ctx, "answer", "")
	if err != nil {
		t.Fatal(err)
	}
	var seq uint64
	var result *claudia.FleetCheckResult
	for result == nil {
		events, err := fleet.Read(ctx, seq)
		if err != nil {
			t.Fatal(err)
		}
		for i := range events {
			seq = events[i].Seq
			if events[i].Kind == claudia.FleetEventCheckResult {
				result = events[i].Check
			}
		}
	}
	if result.RunID != id || result.Score == nil || *result.Score != 42 {
		t.Fatalf("result %+v", result)
	}

	timer, err := fleet.Schedule(ctx, &claudia.FleetScheduleArgs{To: "worker", Kind: claudia.FleetNotice, Text: "tick", Every: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := fleet.SetBudget(ctx, "worker", claudia.FleetBudget{Turns: 5}); err != nil {
		t.Fatal(err)
	}
	st, err := fleet.Status(ctx)
	if err != nil || len(st.Timers) != 1 || st.Timers[0].ID != timer || len(st.Budgets) != 1 || st.Budgets[0].Limits.Turns != 5 || len(st.Checks) != 1 {
		t.Fatalf("status %+v %v", st, err)
	}
	if err := fleet.Cancel(ctx, timer); err != nil {
		t.Fatal(err)
	}
	if st, _ := fleet.Status(ctx); len(st.Timers) != 0 {
		t.Fatalf("timer survived cancel: %+v", st.Timers)
	}
}

// Review finding 5: an empty tool set crosses the wire as no tools.
func TestFleetEmptyToolSetSurvivesTheWire(t *testing.T) {
	f := newFixture(t)
	f.bootWith(t, f.fleetOptions())
	fleet := openTestFleet(t, f)
	ctx := wallclockguard.UntilTestTimeout(t)
	if err := fleet.Start(ctx, &claudia.FleetStartArgs{Name: "mute", Instructions: "x", WorkDir: t.TempDir(), Tools: []string{}}); err != nil {
		t.Fatal(err)
	}
	st, err := fleet.Status(ctx)
	if err != nil || len(st.Agents) != 1 || len(st.Agents[0].Tools) != 0 {
		t.Fatalf("status %+v %v", st, err)
	}
}

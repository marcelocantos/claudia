// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// Clause oracles the first daemon suite did not name (2026-09-12 vcheck
// BLOCKs on 🎯T2.9, 🎯T2.11, 🎯T2.14, 🎯T3), and the 🎯T75 pass-throughs.
// Each test is one acceptance clause, named for it.

// goalSettle outlasts claudia's Goal settle timer (250ms), after which a
// continuation that was going to be sent has been.
const goalSettle = 400 * time.Millisecond

// TestResumeAdoptsWithoutNudgeAndEmitsTailEvents (🎯T2.14 clauses 2, 3, 4):
// a seat whose process survived is adopted and not nudged; a seat that did
// not is launched and nudged; the tail carries resume/nudge per seat with
// the outcome in Detail; a seat that cannot be resumed reports resume_failed
// with the reason.
func TestResumeAdoptsWithoutNudgeAndEmitsTailEvents(t *testing.T) {
	f := newFixture(t)
	// "still-there" is adoptable: the adopt seam hands back a live stub.
	adoptable := newSeat()
	f.adoptHook = func(cfg claudia.Config) (*claudia.Agent, error) {
		if cfg.Name == "still-there" {
			return claudia.StartStub(context.Background(), cfg, adoptable.ops())
		}
		return nil, claudia.ErrNoSessionWindow
	}
	f.startHook = func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
		if cfg.Name == "broken" {
			return nil, os.ErrPermission
		}
		return f.startSeat(ctx, cfg)
	}
	writeGrantsTable(t, f.state, []claudia.AgentDef{
		{Name: "still-there", WorkDir: t.TempDir(), SessionID: "sid-a", AutoStart: true},
		{Name: "gone", WorkDir: t.TempDir(), SessionID: "sid-b", AutoStart: true},
		{Name: "broken", WorkDir: t.TempDir(), SessionID: "sid-c", AutoStart: true},
	})
	gate := make(chan struct{})
	opts := f.options(nil)
	opts.resumeGate = gate
	f.bootWith(t, opts)
	tail := tailEvents(t, f.sock)
	close(gate)

	waitFor(t, "resume events", func() bool { return len(kinds(tail(), "")) >= 4 })
	evs := tail()
	if got := kinds(evs, "still-there"); strings.Join(got, ",") != "resume/adopted" {
		t.Fatalf("adopted seat tail = %v, want resume/adopted only (no nudge)", got)
	}
	if got := kinds(evs, "gone"); strings.Join(got, ",") != "resume/launched,nudge/" {
		t.Fatalf("launched seat tail = %v", got)
	}
	if got := kinds(evs, "broken"); len(got) != 1 || !strings.HasPrefix(got[0], "resume_failed/") || !strings.Contains(got[0], "permission") {
		t.Fatalf("broken seat tail = %v", got)
	}
	if sends := adoptable.sent(); len(sends) != 0 {
		t.Fatalf("adopted seat was nudged: sends=%v", sends)
	}
	if sends := f.seat(0).sent(); len(sends) != 1 || sends[0] != "restart-nudge" {
		t.Fatalf("launched seat sends = %v", sends)
	}
	// `claudia broker grants` (the grants request) shows resumed seats as
	// alive and unowned.
	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.WriteRequest(&broker.Request{ID: "g", Type: broker.TypeGrants, Grants: &broker.GrantsRequest{}})
	resp, err := c.ReadResponse()
	if err != nil || resp.Grants == nil {
		t.Fatalf("grants: %v %v", resp, err)
	}
	seen := map[string]broker.GrantStatus{}
	for _, g := range resp.Grants.Grants {
		seen[g.Name] = g
	}
	for _, name := range []string{"still-there", "gone"} {
		g, ok := seen[name]
		if !ok || !g.Alive || g.Owned {
			t.Fatalf("grants shows %s as %+v, want alive and unowned", name, g)
		}
	}
}

// TestResumeNudgeTextAndDisable (🎯T2.14 clause 3): the empty option sends
// DefaultRestartNudge; "-" sends nothing.
func TestResumeNudgeTextAndDisable(t *testing.T) {
	for _, tc := range []struct {
		name, nudge string
		wantSends   int
		wantText    string
	}{
		{"default", "", 1, "The host restarted at"},
		{"disabled", "-", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			writeGrantsTable(t, f.state, []claudia.AgentDef{{Name: "seat", WorkDir: t.TempDir(), SessionID: "sid", AutoStart: true}})
			opts := f.options(nil)
			opts.RestartNudge = tc.nudge
			f.bootWith(t, opts)
			waitFor(t, "seat launched", func() bool { return f.seat(0) != nil })
			<-f.d.resumeDone
			sends := f.seat(0).sent()
			if len(sends) != tc.wantSends {
				t.Fatalf("sends = %v, want %d", sends, tc.wantSends)
			}
			if tc.wantText != "" && !strings.Contains(sends[0], tc.wantText) {
				t.Fatalf("nudge text = %q", sends[0])
			}
		})
	}
}

// TestResumeConcurrencyIsBounded (🎯T2.14 clause 2): with ResumeConcurrency
// 2 and four seats, no more than two provider starts are in flight at once.
func TestResumeConcurrencyIsBounded(t *testing.T) {
	f := newFixture(t)
	var inflight, peak atomic.Int32
	release := make(chan struct{})
	f.startHook = func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inflight.Add(-1)
		return f.startSeat(ctx, cfg)
	}
	var defs []claudia.AgentDef
	for _, n := range []string{"a", "b", "c", "d"} {
		defs = append(defs, claudia.AgentDef{Name: n, WorkDir: t.TempDir(), SessionID: "sid-" + n, AutoStart: true})
	}
	writeGrantsTable(t, f.state, defs)
	opts := f.options(nil)
	opts.RestartNudge = "-"
	opts.ResumeConcurrency = 2
	f.bootWith(t, opts)
	waitFor(t, "two starts in flight", func() bool { return inflight.Load() == 2 })
	time.Sleep(50 * time.Millisecond)
	if got := inflight.Load(); got != 2 {
		t.Fatalf("in flight = %d with bound 2", got)
	}
	close(release)
	<-f.d.resumeDone
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak concurrency = %d, want 2", p)
	}
}

// TestGrantsPersistAcrossDaemonRestart (🎯T2.14 clause 1): the daemon writes
// a granted seat to grants.json marked live, a stop-release removes it, and
// a new daemon over the same state dir lists what the old one held.
func TestGrantsPersistAcrossDaemonRestart(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	a, err := claudia.Start(claudia.Config{Name: "kept", WorkDir: t.TempDir(), SessionID: "sid-k", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := claudia.Start(claudia.Config{Name: "dropped", WorkDir: t.TempDir(), SessionID: "sid-d", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	table := readGrantsTable(t, f.state)
	if !table["kept"].AutoStart || !table["dropped"].AutoStart {
		t.Fatalf("granted seats not marked live on disk: %+v", table)
	}
	b.Stop()
	waitFor(t, "dropped removed on disk", func() bool { _, ok := readGrantsTable(t, f.state)["dropped"]; return !ok })
	if err := a.Detach(); err != nil { // the consumer goes; the seat is still the daemon's
		t.Fatal(err)
	}
	if err := f.d.Close(); err != nil {
		t.Fatal(err)
	}
	opts := f.options(nil)
	opts.DisableResume = true
	d2, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d2.Close() })
	defs := d2.reg.List()
	if len(defs) != 1 || defs[0].Name != "kept" || defs[0].SessionID != "sid-k" || !defs[0].AutoStart {
		t.Fatalf("new daemon's table = %+v, want only the kept seat, live", defs)
	}
}

// TestKillMidTurnReclaimPerProvider (🎯T2.11 clauses 1–3): for every Session
// provider, a consumer grants, sends, and leaves while the turn is in
// flight; a second consumer reclaims by name, sees the same prompt-in-flight
// state the daemon holds, and WaitForResponse returns the turn's answer. No
// second provider process is started.
func TestKillMidTurnReclaimPerProvider(t *testing.T) {
	for _, provider := range []claudia.Provider{claudia.ProviderClaude, claudia.ProviderGrok, claudia.ProviderCodex, claudia.ProviderCursor} {
		t.Run(string(provider), func(t *testing.T) {
			f := newFixture(t)
			f.boot(t, nil)
			name := "seat-" + string(provider)
			cfg := claudia.Config{Name: name, Provider: provider, WorkDir: t.TempDir(), SessionID: "sid-" + string(provider), TermLogPath: "-"}
			if provider == claudia.ProviderCodex {
				cfg.SessionID = "" // Codex ids are minted by the provider
			}
			first, err := claudia.Start(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Send("do the thing"); err != nil {
				t.Fatal(err)
			}
			s := f.seat(0)
			s.inFlight.Store(true)
			if !first.PromptInFlight() {
				t.Fatal("handle does not see the daemon's in-flight state")
			}
			sid := first.SessionID()
			if err := first.Detach(); err != nil { // consumer leaves mid-turn
				t.Fatal(err)
			}
			waitFor(t, "detached", func() bool { return !f.owned(name) })
			proc := f.d.reg.Get(name)
			if proc == nil || !proc.Alive() {
				t.Fatal("seat did not survive the consumer")
			}
			// The provider keeps working while unowned.
			proc.PublishEvent(claudia.Event{Type: "progress", ProgressType: "tool_use", ToolTitle: "Bash"})

			cfg.SessionID = sid
			second, err := claudia.Start(cfg)
			if err != nil {
				t.Fatalf("reclaim: %v", err)
			}
			t.Cleanup(second.Stop)
			if second.SessionID() != sid || f.seat(1) != nil {
				t.Fatalf("reclaim minted a new seat: sid=%s seats=%d", second.SessionID(), f.seatCount())
			}
			if !second.PromptInFlight() {
				t.Fatal("reclaimed handle lost the in-flight state")
			}
			// The turn completes after the reclaim; the answer reaches the
			// new consumer's WaitForResponse.
			go func() {
				time.Sleep(20 * time.Millisecond)
				s.inFlight.Store(false)
				proc.PublishEvent(claudia.Event{Type: "assistant", Text: "the answer", StopReason: "end_turn"})
			}()
			text, err := second.WaitForResponse(wallclockguard.UntilTestTimeout(t))
			if err != nil || text != "the answer" {
				t.Fatalf("WaitForResponse after reclaim = %q, %v", text, err)
			}
			if sends := s.sent(); len(sends) != 1 || sends[0] != "do the thing" {
				t.Fatalf("provider saw sends %v", sends)
			}
		})
	}
}

// TestReclaimReplayReachesFirstSubscriber (🎯T2.11 clause 1): history
// retained while unowned is delivered to the reclaiming consumer's first
// subscriber, not lost in the gap between Start returning and
// SubscribeEvents.
func TestReclaimReplayReachesFirstSubscriber(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	first, err := claudia.Start(claudia.Config{Name: "hist", WorkDir: t.TempDir(), SessionID: "sid-h", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Detach(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "detached", func() bool { return !f.owned("hist") })
	proc := f.d.reg.Get("hist")
	proc.PublishEvent(claudia.Event{Type: "assistant", Text: "said while unowned", StopReason: "end_turn"})
	second, err := claudia.Start(claudia.Config{Name: "hist", WorkDir: t.TempDir(), SessionID: "sid-h", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Stop)
	got := collectEvents(second)
	waitFor(t, "replayed history", func() bool { return len(got()) == 1 })
	if got()[0].Text != "said while unowned" {
		t.Fatalf("replayed = %+v", got())
	}
}

// TestHandleNeverRebinds (🎯T3, 🎯T2.12 residue): a daemon-held handle whose
// seat reports a rate limit publishes stuck and does not switch model or
// provider on its own.
func TestHandleNeverRebinds(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	a, err := claudia.Start(claudia.Config{Name: "pinned", WorkDir: t.TempDir(), SessionID: "sid-p", Model: "opus", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	got := collectEvents(a)
	proc := f.d.reg.Get("pinned")
	proc.PublishEvent(claudia.Event{Type: "assistant", IsError: true, Text: "429 rate limit exceeded"})
	waitFor(t, "stuck on the handle", func() bool {
		for _, ev := range got() {
			if ev.ProgressType == claudia.ProgressStuck {
				return true
			}
		}
		return false
	})
	if err := a.Send("next"); err != nil {
		t.Fatal(err)
	}
	for _, ev := range got() {
		if ev.ProgressType == claudia.ProgressModelSwitch {
			t.Fatalf("handle rebound on its own: %+v", ev)
		}
	}
	if a.Model() != "opus" || a.Provider() != claudia.ProviderClaude {
		t.Fatalf("handle drifted to %s/%s", a.Provider(), a.Model())
	}
	stuckCount := 0
	for _, ev := range got() {
		if ev.ProgressType == claudia.ProgressStuck {
			stuckCount++
		}
	}
	if stuckCount != 1 {
		t.Fatalf("stuck published %d times, want once (daemon does not forward its own synthesis)", stuckCount)
	}
}

// TestUsageUpdateOnTail (🎯T2.9 clause 5): every refresh emits usage_update
// per provider on the tail.
func TestUsageUpdateOnTail(t *testing.T) {
	pct := 50.0
	usage := []claudia.PlanUsage{
		{Provider: claudia.ProviderClaude, Status: claudia.PlanUsageAvailable, Windows: []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &pct}}},
		{Provider: claudia.ProviderGrok, Status: claudia.PlanUsageUnavailable, Reason: "opt-in"},
	}
	f := newFixture(t)
	f.boot(t, usage)
	tail := tailEvents(t, f.sock)
	if _, err := claudia.LoadPlanUsage(context.Background(), &claudia.PlanUsageCacheArgs{Refresh: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "usage_update events", func() bool {
		n := 0
		for _, ev := range tail() {
			if ev.Kind == broker.EventUsageUpdate {
				n++
			}
		}
		return n >= 2
	})
	seen := map[string]bool{}
	for _, ev := range tail() {
		if ev.Kind == broker.EventUsageUpdate {
			seen[ev.Detail] = true
		}
	}
	if !seen["claude"] || !seen["grok"] {
		t.Fatalf("usage_update details = %v", seen)
	}
}

// TestResolveIdenticalAcrossDaemonAndCache (🎯T2.9 clause 3): the same
// snapshot yields the same ModelPick whether Resolve reads it from the
// daemon over the socket or from the filesystem cache with no daemon.
func TestResolveIdenticalAcrossDaemonAndCache(t *testing.T) {
	low, high := 12.0, 80.0
	snapshot := []claudia.PlanUsage{
		{Provider: claudia.ProviderClaude, Status: claudia.PlanUsageAvailable, Windows: []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &low}}},
		{Provider: claudia.ProviderGrok, Status: claudia.PlanUsageAvailable, Windows: []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &high}}},
		{Provider: claudia.ProviderCodex, Status: claudia.PlanUsageUnavailable, Reason: "signed out"},
	}
	pred := claudia.ModelPredicates{Mode: claudia.CapabilityTask, PreferPlan: true, PreferProvider: claudia.ProviderClaude}

	// Daemon branch: the socket answers with the daemon's snapshot.
	f := newFixture(t)
	f.boot(t, snapshot)
	waitFor(t, "daemon fetched", func() bool { return f.fetchCount() == 1 })
	viaDaemon, err := claudia.LoadPlanUsage(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadPlanUsage via daemon: %v", err)
	}
	pickDaemon, err := claudia.Resolve(context.Background(), pred)
	if err != nil {
		t.Fatalf("Resolve via daemon: %v", err)
	}
	if f.fetchCount() != 1 {
		t.Fatalf("daemon branch reached the vendor fetch %d times", f.fetchCount())
	}

	// Cache branch: no daemon (CLAUDIA_NO_BROKER=1), a fresh cache dir,
	// the same snapshot from the T61 fetch seam.
	t.Setenv(broker.NoBrokerEnv, "1")
	cacheFetches := 0
	cache := &claudia.PlanUsageCacheArgs{Dir: t.TempDir(), Fetch: func(context.Context) ([]claudia.PlanUsage, error) {
		cacheFetches++
		return snapshot, nil
	}}
	viaCache, err := claudia.LoadPlanUsage(context.Background(), cache)
	if err != nil {
		t.Fatalf("LoadPlanUsage via cache: %v", err)
	}
	pred.Cache = cache
	pickCache, err := claudia.Resolve(context.Background(), pred)
	if err != nil {
		t.Fatalf("Resolve via cache: %v", err)
	}
	if cacheFetches != 1 {
		t.Fatalf("cache branch fetched %d times", cacheFetches)
	}

	normalize := func(us []claudia.PlanUsage) []claudia.PlanUsage {
		out := make([]claudia.PlanUsage, len(us))
		for i, u := range us {
			u.FetchedAt = time.Time{}
			out[i] = u
		}
		return out
	}
	if !reflect.DeepEqual(normalize(viaDaemon), normalize(viaCache)) {
		t.Fatalf("usage differs by branch\n daemon: %+v\n cache:  %+v", viaDaemon, viaCache)
	}
	if !reflect.DeepEqual(pickDaemon, pickCache) {
		t.Fatalf("ModelPick differs by branch\n daemon: %+v\n cache:  %+v", pickDaemon, pickCache)
	}
	if pickDaemon.Provider == "" || pickDaemon.Model == "" {
		t.Fatalf("empty pick on both branches: %+v", pickDaemon)
	}
}

// TestT70UnownedStreamSurvivesBounceRing: more than the old 256-event bound
// arrives while unowned; reclaim is not Lagged and WaitForResponse sees the
// turn's answer (🎯T70).
func TestT70UnownedStreamSurvivesBounceRing(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	first, err := claudia.Start(claudia.Config{Name: "stream", WorkDir: t.TempDir(), SessionID: "sid-stream", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	s := f.seat(0)
	if err := first.Send("do the thing"); err != nil {
		t.Fatal(err)
	}
	s.inFlight.Store(true)
	if err := first.Detach(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "detached", func() bool { return !f.owned("stream") })
	proc := f.d.reg.Get("stream")
	if proc == nil || !proc.Alive() {
		t.Fatal("seat died when the consumer left")
	}
	const n = 300
	for i := 0; i < n-1; i++ {
		proc.PublishEvent(claudia.Event{Type: "progress", ProgressType: "tool_use", ToolTitle: fmt.Sprintf("t%d", i)})
	}
	s.inFlight.Store(false)
	proc.PublishEvent(claudia.Event{Type: "assistant", Text: "the answer", StopReason: "end_turn"})

	f.d.mu.Lock()
	g := f.d.grants["stream"]
	ring, lagged := len(g.ring), g.lag
	f.d.mu.Unlock()
	if ring != n || lagged {
		t.Fatalf("unowned ring=%d lagged=%v, want %d unlagged (old cap was 256)", ring, lagged, n)
	}

	second, err := claudia.Start(claudia.Config{Name: "stream", WorkDir: t.TempDir(), SessionID: "sid-stream", TermLogPath: "-"})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	t.Cleanup(second.Stop)
	text, err := second.WaitForResponse(wallclockguard.UntilTestTimeout(t))
	if err != nil || text != "the answer" {
		t.Fatalf("WaitForResponse after reclaim = %q, %v", text, err)
	}
	if f.seat(1) != nil {
		t.Fatal("reclaim started a second provider process")
	}
}

// TestForwardsSeatGone (🎯T75.6): the daemon's liveness watch is its
// Registry's. A granted seat whose process dies reaches the owner as
// agent_gone (its handle stops reporting alive) and the tail as gone, once.
func TestForwardsSeatGone(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	tail := tailEvents(t, f.sock)
	a, err := claudia.Start(claudia.Config{Name: "seat-g", WorkDir: t.TempDir(), SessionID: "sid-g", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	f.seat(0).exit()

	for range 3 {
		f.clock.Advance(claudia.DefaultSeatWatchInterval)
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, "handle not alive", func() bool { return !a.Alive() })
	waitFor(t, "gone on tail", func() bool { return len(kinds(tail(), "seat-g")) >= 2 })
	var gone int
	for _, k := range kinds(tail(), "seat-g") {
		if strings.HasPrefix(k, string(broker.EventGone)+"/") {
			gone++
		}
	}
	if gone != 1 {
		t.Fatalf("tail for seat-g = %v, want one gone", kinds(tail(), "seat-g"))
	}
}

// TestDefaultStateDirHoldsModelIntel: a daemon built with an empty StateDir,
// as `claudia broker serve` builds it, keeps model intel in the resolved
// state directory. It used to join the empty option, writing ./model-intel
// into whatever directory the daemon ran from and resolving against that
// rather than the store consumers read.
func TestDefaultStateDirHoldsModelIntel(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cbi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv(broker.StateHomeEnv, filepath.Join(dir, "xdg"))
	t.Setenv("CLAUDIA_AA_API_KEY", "")
	cwd := t.TempDir()
	t.Chdir(cwd)

	d, err := New(Options{
		SocketPath:     filepath.Join(dir, "b.sock"),
		DisableResume:  true,
		DisableMCPHost: true,
		UsageFetch:     func(context.Context) ([]claudia.PlanUsage, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	want, err := claudia.DefaultModelIntelDir()
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "intel run recorded", func() bool {
		_, err := os.Stat(filepath.Join(want, "ingest_runs.jsonl"))
		return err == nil
	})
	if _, err := os.Stat(filepath.Join(cwd, claudia.ModelIntelDirName)); err == nil {
		t.Fatal("daemon wrote model intel relative to its working directory")
	}
}

// TestCarriesTaskRawLog (🎯T75.10): a raw-log func on a Task run through the
// daemon sees every line the direct path sees for the same stub, in order; a
// run without one does not ask the daemon for lines.
func TestCarriesTaskRawLog(t *testing.T) {
	lines := []string{`{"type":"system","subtype":"init"}`, `{"type":"assistant","n":1}`, `{"type":"result"}`}
	var mu sync.Mutex
	var asked []bool
	stubRun := func(_ context.Context, run claudia.StubTaskRun) (<-chan claudia.TaskEvent, error) {
		mu.Lock()
		asked = append(asked, run.RawLog != nil)
		mu.Unlock()
		if run.RawLog != nil {
			for _, line := range lines {
				run.RawLog([]byte(line))
			}
		}
		ch := make(chan claudia.TaskEvent, 2)
		ch <- claudia.TaskEvent{Type: claudia.TaskEventInit, SessionID: "raw-sid"}
		ch <- claudia.TaskEvent{Type: claudia.TaskEventResult, Content: "done"}
		close(ch)
		return ch, nil
	}
	collect := func(task *claudia.Task) func() []string {
		var mu sync.Mutex
		var got []string
		task.SetRawLog(func(line []byte) {
			mu.Lock()
			got = append(got, string(line))
			mu.Unlock()
		})
		return func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), got...)
		}
	}
	drain := func(task *claudia.Task) {
		t.Helper()
		ch, err := task.Run(context.Background(), "go")
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
		}
	}

	direct := claudia.NewStubTask(claudia.TaskConfig{ID: "direct", WorkDir: t.TempDir()}, &claudia.StubTaskOps{Run: stubRun})
	directLines := collect(direct)
	drain(direct)

	f := newFixture(t)
	f.boot(t, nil)
	prev := daemonNewTask
	daemonNewTask = func(cfg claudia.TaskConfig) *claudia.Task {
		return claudia.NewStubTask(cfg, &claudia.StubTaskOps{Run: stubRun})
	}
	t.Cleanup(func() { daemonNewTask = prev })

	brokered := claudia.NewTask(claudia.TaskConfig{ID: "brokered", WorkDir: t.TempDir()})
	brokeredLines := collect(brokered)
	drain(brokered)
	if got, want := brokeredLines(), directLines(); !reflect.DeepEqual(got, want) || len(want) != len(lines) {
		t.Fatalf("raw lines through the daemon = %q, direct = %q", got, want)
	}

	plain := claudia.NewTask(claudia.TaskConfig{ID: "plain", WorkDir: t.TempDir()})
	drain(plain)
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(asked, []bool{true, true, false}) {
		t.Fatalf("raw-log funcs handed to the stub = %v, want direct, brokered, then none for the plain run", asked)
	}
}

// TestRewindsHeldSeat (🎯T75.8): Rewind on a daemon-held seat is the
// daemon's Registry.Rewind. The handle is kept and re-pointed, the
// transcript loses one turn, the daemon holds the relaunched process, and
// events from it reach the owner.
func TestRewindsHeldSeat(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	a, err := claudia.Start(claudia.Config{Name: "seat-rw", WorkDir: t.TempDir(), SessionID: "sid-rw", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	old := f.d.reg.Get("seat-rw")
	if old == nil {
		t.Fatal("daemon holds no proc")
	}
	transcript := old.JSONLPath()
	writeTranscript(t, transcript)
	events := collectEvents(a)

	got, err := a.Rewind(1, claudia.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got != a {
		t.Fatal("a daemon-held rewind must keep the handle")
	}
	next := f.d.reg.Get("seat-rw")
	if next == nil || next == old {
		t.Fatalf("daemon registry holds %p, old %p", next, old)
	}
	b, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("CHARLIE")) || !bytes.Contains(b, []byte("turn2")) {
		t.Fatalf("transcript not rewound:\n%s", b)
	}
	if a.SessionID() != "sid-rw" || !a.Alive() {
		t.Fatalf("handle after rewind: session=%q alive=%v", a.SessionID(), a.Alive())
	}
	next.PublishEvent(claudia.Event{Type: "assistant", Text: "after rewind"})
	waitFor(t, "event from relaunched process", func() bool {
		for _, ev := range events() {
			if ev.Text == "after rewind" {
				return true
			}
		}
		return false
	})
}

// TestHonoursOwnerGoalCompleteCheck (🎯T75.9): the daemon's Goal loop asks
// the owning handle's GoalCompleteCheck, in both directions, and falls back
// to ParseGoalStatus when the handle has none.
func TestHonoursOwnerGoalCompleteCheck(t *testing.T) {
	for _, tc := range []struct {
		name      string
		check     func(goal, text string) bool
		text      string
		wantSends int
		wantOpen  bool
	}{
		{"check says complete", func(string, string) bool { return true }, "workers finished", 1, false},
		{"check says not complete", func(string, string) bool { return false }, "workers finished", 2, true},
		{"no check falls back to status", nil, "workers finished", 2, true},
		{"status line still wins", func(string, string) bool { return false }, "done\n" + claudia.GoalStatusComplete, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.boot(t, nil)
			var mu sync.Mutex
			var asked []string
			cfg := claudia.Config{Name: "goal-seat", WorkDir: t.TempDir(), SessionID: "sid-goal", TermLogPath: "-", Goal: "ship T75"}
			if tc.check != nil {
				cfg.GoalCompleteCheck = func(goal, text string) bool {
					mu.Lock()
					asked = append(asked, goal+"|"+text)
					mu.Unlock()
					return tc.check(goal, text)
				}
			}
			a, err := claudia.Start(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.Stop)
			collectEvents(a) // a subscribed consumer, so pushes are not held for replay
			if err := a.Send("go"); err != nil {
				t.Fatal(err)
			}
			proc, s := f.d.reg.Get("goal-seat"), f.seat(0)
			s.inFlight.Store(false)
			proc.PublishEvent(claudia.Event{Type: "assistant", Text: tc.text, StopReason: "end_turn"})
			if tc.wantOpen {
				waitFor(t, "continuation", func() bool { return len(s.sent()) >= tc.wantSends })
			} else {
				waitFor(t, "goal closed", func() bool { return !proc.GoalActive() })
			}
			time.Sleep(goalSettle) // nothing further arrives

			if got := len(s.sent()); got != tc.wantSends {
				t.Fatalf("provider sends = %d (%q), want %d", got, s.sent(), tc.wantSends)
			}
			if proc.GoalActive() != tc.wantOpen {
				t.Fatalf("daemon-side goal active = %v, want %v", proc.GoalActive(), tc.wantOpen)
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.check != nil && tc.text == "workers finished" && (len(asked) != 1 || asked[0] != "ship T75|workers finished") {
				t.Fatalf("owner check asked %q, want once with the goal and the turn text", asked)
			}
		})
	}
}

// TestRewindOnHeldSeatRefusesLikeDirectAndReturnsResult (🎯T75.8): capability
// refusal is the same on a daemon-held seat as on one started in-process,
// and a consumer Registry's Rewind of a daemon-held seat returns what the
// daemon removed, keeps the handle, and leaves the seat persisted on the
// daemon.
func TestRewindOnHeldSeatRefusesLikeDirectAndReturnsResult(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)

	// Refusal: Grok has no rewind, held or direct.
	held, err := claudia.Start(claudia.Config{Name: "grok-seat", Provider: claudia.ProviderGrok, WorkDir: t.TempDir(), SessionID: "sid-grok", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(held.Stop)
	if !held.DaemonHeld() {
		t.Fatal("grok seat is not daemon-held")
	}
	direct, err := claudia.StartStub(context.Background(), claudia.Config{Provider: claudia.ProviderGrok, WorkDir: t.TempDir(), SessionID: "sid-grok-direct", TermLogPath: "-"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(direct.Stop)
	_, heldErr := held.Rewind(1, claudia.Config{})
	_, directErr := direct.Rewind(1, claudia.Config{})
	if heldErr == nil || directErr == nil || heldErr.Error() != directErr.Error() {
		t.Fatalf("rewind refusal held = %v, direct = %v; want the same capability error", heldErr, directErr)
	}
	var capErr *claudia.CapabilityError
	if !errors.As(heldErr, &capErr) {
		t.Fatalf("held refusal is %T, want *claudia.CapabilityError", heldErr)
	}

	// Result through a consumer Registry.
	reg, err := claudia.NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(claudia.AgentDef{Name: "rw-seat", WorkDir: t.TempDir(), SessionID: "sid-rwr", TermLogPath: "-"}); err != nil {
		t.Fatal(err)
	}
	proc, err := reg.Launch("rw-seat")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Stop("rw-seat") })
	writeTranscript(t, f.d.reg.Get("rw-seat").JSONLPath())
	got, res, err := reg.Rewind(context.Background(), "rw-seat", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != proc || res == nil || res.TurnsRemoved != 1 || res.BackupPath == "" || res.BytesRemoved <= 0 {
		t.Fatalf("Registry.Rewind via daemon = handle %p (was %p), result %+v", got, proc, res)
	}
	if def := readGrantsTable(t, f.state)["rw-seat"]; def.SessionID != "sid-rwr" || !def.AutoStart {
		t.Fatalf("daemon's persisted seat after rewind = %+v", def)
	}
}

// TestGoalCheckWithNoOwnerFallsBackToStatus (🎯T75.9): a seat whose
// consumer has gone has no owner to ask, so its Goal loop runs on
// ParseGoalStatus alone — continuing without a status line and closing on
// one.
func TestGoalCheckWithNoOwnerFallsBackToStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		text      string
		wantSends int
		wantOpen  bool
	}{
		{"no status continues", "workers finished", 2, true},
		{"status closes", "done\n" + claudia.GoalStatusComplete, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.boot(t, nil)
			var asked atomic.Int32
			a, err := claudia.Start(claudia.Config{Name: "orphan-goal", WorkDir: t.TempDir(), SessionID: "sid-og", TermLogPath: "-", Goal: "ship T75",
				GoalCompleteCheck: func(string, string) bool { asked.Add(1); return true }})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Send("go"); err != nil {
				t.Fatal(err)
			}
			if err := a.Detach(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "detached", func() bool { return !f.owned("orphan-goal") })
			proc, s := f.d.reg.Get("orphan-goal"), f.seat(0)
			s.inFlight.Store(false)
			proc.PublishEvent(claudia.Event{Type: "assistant", Text: tc.text, StopReason: "end_turn"})
			if tc.wantOpen {
				waitFor(t, "continuation", func() bool { return len(s.sent()) >= tc.wantSends })
			} else {
				waitFor(t, "goal closed", func() bool { return !proc.GoalActive() })
			}
			time.Sleep(goalSettle)
			if got := len(s.sent()); got != tc.wantSends {
				t.Fatalf("provider sends = %d (%q), want %d", got, s.sent(), tc.wantSends)
			}
			if proc.GoalActive() != tc.wantOpen {
				t.Fatalf("goal active = %v, want %v", proc.GoalActive(), tc.wantOpen)
			}
			if n := asked.Load(); n != 0 {
				t.Fatalf("a departed consumer's check was asked %d time(s)", n)
			}
		})
	}
}

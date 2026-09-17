// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// Clause oracles the first daemon suite did not name (2026-09-12 vcheck
// BLOCKs on 🎯T2.9, 🎯T2.11, 🎯T2.14, 🎯T3). Each test is one acceptance
// clause, named for it.

// tailEvents subscribes a raw tail connection to the daemon and returns a
// snapshot function. It is subscribed before the observed action so the
// first event cannot race the subscription (the tailing ack is written
// after the subscription is live).
func tailEvents(t *testing.T, sock string) func() []broker.EventMessage {
	t.Helper()
	c, err := broker.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.WriteRequest(&broker.Request{ID: "tail", Type: broker.TypeTail, Tail: &broker.TailRequest{}}); err != nil {
		t.Fatal(err)
	}
	ack, err := c.ReadResponse()
	if err != nil || ack.Type != broker.TypeTailing {
		t.Fatalf("tail ack = %v, %v", ack, err)
	}
	var mu sync.Mutex
	var got []broker.EventMessage
	go func() {
		for {
			resp, err := c.ReadResponse()
			if err != nil {
				return
			}
			if resp.Type == broker.TypeEvent {
				mu.Lock()
				got = append(got, *resp.Event)
				mu.Unlock()
			}
		}
	}()
	return func() []broker.EventMessage {
		mu.Lock()
		defer mu.Unlock()
		return append([]broker.EventMessage(nil), got...)
	}
}

func kinds(evs []broker.EventMessage, name string) []string {
	var out []string
	for _, ev := range evs {
		if name == "" || ev.Name == name {
			out = append(out, string(ev.Kind)+"/"+ev.Detail)
		}
	}
	return out
}

func writeGrantsTable(t *testing.T, state string, defs []AgentDef) {
	t.Helper()
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(defs)
	if err := os.WriteFile(filepath.Join(state, grantsFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readGrantsTable(t *testing.T, state string) map[string]AgentDef {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(state, grantsFile))
	if err != nil {
		t.Fatal(err)
	}
	var defs []AgentDef
	if err := json.Unmarshal(raw, &defs); err != nil {
		t.Fatal(err)
	}
	out := map[string]AgentDef{}
	for _, d := range defs {
		out[d.Name] = d
	}
	return out
}

// TestBrokerDaemonResumeAdoptsWithoutNudgeAndEmitsTailEvents (🎯T2.14
// clauses 2, 3, 4): a seat whose process survived is adopted and not
// nudged; a seat that did not is launched and nudged; the tail carries
// resume/nudge per seat with the outcome in Detail; a seat that cannot be
// resumed reports resume_failed with the reason.
func TestBrokerDaemonResumeAdoptsWithoutNudgeAndEmitsTailEvents(t *testing.T) {
	f := startDaemon(t, true, nil)
	// "still-there" is adoptable: the adopt seam hands back a live fake.
	adoptable := &fakeAgentBackend{name: "fake-claude"}
	registryAdopt = func(cfg Config) (*Agent, error) {
		if cfg.Name == "still-there" {
			return startWithBackendContext(context.Background(), cfg, adoptable)
		}
		return nil, ErrNoSessionWindow
	}
	prevStart := registryStartDirect
	registryStartDirect = func(ctx context.Context, cfg Config) (*Agent, error) {
		if cfg.Name == "broken" {
			return nil, os.ErrPermission
		}
		return prevStart(ctx, cfg)
	}
	writeGrantsTable(t, f.state, []AgentDef{
		{Name: "still-there", WorkDir: t.TempDir(), SessionID: "sid-a", AutoStart: true},
		{Name: "gone", WorkDir: t.TempDir(), SessionID: "sid-b", AutoStart: true},
		{Name: "broken", WorkDir: t.TempDir(), SessionID: "sid-c", AutoStart: true},
	})
	gate := make(chan struct{})
	d, err := NewBrokerDaemon(BrokerDaemonOptions{
		SocketPath: f.sock, StateDir: f.state, RestartNudge: "restart-nudge", clock: f.clock, resumeGate: gate,
		UsageFetch: func(context.Context) ([]PlanUsage, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	f.d = d
	tail := tailEvents(t, f.sock)
	close(gate)

	waitFor(t, "resume events", func() bool {
		ks := kinds(tail(), "")
		return len(ks) >= 4
	})
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
	adoptable.mu.Lock()
	adoptedSends := len(adoptable.sends)
	adoptable.mu.Unlock()
	if adoptedSends != 0 {
		t.Fatalf("adopted seat was nudged: sends=%v", adoptable.sends)
	}
	launched := f.backend(0)
	launched.mu.Lock()
	sends := append([]string(nil), launched.sends...)
	launched.mu.Unlock()
	if len(sends) != 1 || sends[0] != "restart-nudge" {
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

// TestBrokerDaemonResumeNudgeTextAndDisable (🎯T2.14 clause 3): the empty
// option sends DefaultRestartNudge; "-" sends nothing.
func TestBrokerDaemonResumeNudgeTextAndDisable(t *testing.T) {
	for _, tc := range []struct {
		name, nudge string
		wantSends   int
		wantText    string
	}{
		{"default", "", 1, "The host restarted at"},
		{"disabled", "-", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startDaemon(t, true, nil)
			writeGrantsTable(t, f.state, []AgentDef{{Name: "seat", WorkDir: t.TempDir(), SessionID: "sid", AutoStart: true}})
			d, err := NewBrokerDaemon(BrokerDaemonOptions{
				SocketPath: f.sock, StateDir: f.state, RestartNudge: tc.nudge, clock: f.clock,
				UsageFetch: func(context.Context) ([]PlanUsage, error) { return nil, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			waitFor(t, "seat launched", func() bool { return f.backend(0) != nil })
			<-d.resumeDone
			b := f.backend(0)
			b.mu.Lock()
			sends := append([]string(nil), b.sends...)
			b.mu.Unlock()
			if len(sends) != tc.wantSends {
				t.Fatalf("sends = %v, want %d", sends, tc.wantSends)
			}
			if tc.wantText != "" && !strings.Contains(sends[0], tc.wantText) {
				t.Fatalf("nudge text = %q", sends[0])
			}
		})
	}
}

// TestBrokerDaemonResumeConcurrencyIsBounded (🎯T2.14 clause 2): with
// ResumeConcurrency 2 and four seats, no more than two provider starts are
// in flight at once.
func TestBrokerDaemonResumeConcurrencyIsBounded(t *testing.T) {
	f := startDaemon(t, true, nil)
	var inflight, peak atomic.Int32
	release := make(chan struct{})
	prev := registryStartDirect
	registryStartDirect = func(ctx context.Context, cfg Config) (*Agent, error) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inflight.Add(-1)
		return prev(ctx, cfg)
	}
	var defs []AgentDef
	for _, n := range []string{"a", "b", "c", "d"} {
		defs = append(defs, AgentDef{Name: n, WorkDir: t.TempDir(), SessionID: "sid-" + n, AutoStart: true})
	}
	writeGrantsTable(t, f.state, defs)
	d, err := NewBrokerDaemon(BrokerDaemonOptions{
		SocketPath: f.sock, StateDir: f.state, RestartNudge: "-", ResumeConcurrency: 2, clock: f.clock,
		UsageFetch: func(context.Context) ([]PlanUsage, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	waitFor(t, "two starts in flight", func() bool { return inflight.Load() == 2 })
	time.Sleep(50 * time.Millisecond)
	if got := inflight.Load(); got != 2 {
		t.Fatalf("in flight = %d with bound 2", got)
	}
	close(release)
	<-d.resumeDone
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak concurrency = %d, want 2", p)
	}
}

// TestBrokerDaemonGrantsPersistAcrossDaemonRestart (🎯T2.14 clause 1): the
// daemon writes a granted seat to grants.json marked live, a stop-release
// removes it, and a new daemon over the same state dir lists what the old
// one held.
func TestBrokerDaemonGrantsPersistAcrossDaemonRestart(t *testing.T) {
	f := startDaemon(t, false, nil)
	f.boot(t, false, nil)
	a, err := Start(Config{Name: "kept", WorkDir: t.TempDir(), SessionID: "sid-k", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Start(Config{Name: "dropped", WorkDir: t.TempDir(), SessionID: "sid-d", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	table := readGrantsTable(t, f.state)
	if !table["kept"].AutoStart || !table["dropped"].AutoStart {
		t.Fatalf("granted seats not marked live on disk: %+v", table)
	}
	b.Stop()
	waitFor(t, "dropped removed on disk", func() bool { _, ok := readGrantsTable(t, f.state)["dropped"]; return !ok })
	a.mcpCleanup() // consumer dies; the seat is still the daemon's
	if err := f.d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := NewBrokerDaemon(BrokerDaemonOptions{
		SocketPath: f.sock, StateDir: f.state, DisableResume: true, clock: f.clock,
		UsageFetch: func(context.Context) ([]PlanUsage, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d2.Close() })
	defs := d2.reg.List()
	if len(defs) != 1 || defs[0].Name != "kept" || defs[0].SessionID != "sid-k" || !defs[0].AutoStart {
		t.Fatalf("new daemon's table = %+v, want only the kept seat, live", defs)
	}
}

// TestBrokerDaemonKillMidTurnReclaimPerProvider (🎯T2.11 clauses 1–3): for
// every Session provider, a consumer grants, sends, and dies while the
// turn is in flight; a second consumer reclaims by name, sees the same
// prompt-in-flight state the daemon holds, and WaitForResponse returns the
// turn's answer. No second provider process is started.
func TestBrokerDaemonKillMidTurnReclaimPerProvider(t *testing.T) {
	for _, provider := range []Provider{ProviderClaude, ProviderGrok, ProviderCodex, ProviderCursor} {
		t.Run(string(provider), func(t *testing.T) {
			f := startDaemon(t, false, nil)
			f.boot(t, false, nil)
			name := "seat-" + string(provider)
			cfg := Config{Name: name, Provider: provider, WorkDir: t.TempDir(), SessionID: "sid-" + string(provider), TermLogPath: "-"}
			if provider == ProviderCodex {
				cfg.SessionID = "" // Codex ids are minted by the provider
			}
			first, err := Start(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Send("do the thing"); err != nil {
				t.Fatal(err)
			}
			backend := f.backend(0)
			backend.inFlight.Store(true)
			if !first.PromptInFlight() {
				t.Fatal("handle does not see the daemon's in-flight state")
			}
			sid := first.SessionID()
			first.mcpCleanup() // consumer dies mid-turn
			waitFor(t, "detached", func() bool {
				f.d.mu.Lock()
				defer f.d.mu.Unlock()
				g := f.d.grants[name]
				return g != nil && g.owner == nil
			})
			proc := f.d.reg.Get(name)
			if proc == nil || !proc.Alive() {
				t.Fatal("seat did not survive the consumer")
			}
			// The provider keeps working while unowned.
			proc.PublishEvent(Event{Type: "progress", ProgressType: "tool_use", ToolTitle: "Bash"})

			cfg.SessionID = sid
			second, err := Start(cfg)
			if err != nil {
				t.Fatalf("reclaim: %v", err)
			}
			t.Cleanup(second.Stop)
			if second.SessionID() != sid || f.backend(1) != nil {
				t.Fatalf("reclaim minted a new seat: sid=%s backends=%d", second.SessionID(), len(f.backends))
			}
			if !second.PromptInFlight() {
				t.Fatal("reclaimed handle lost the in-flight state")
			}
			// The turn completes after the reclaim; the answer reaches the
			// new consumer's WaitForResponse.
			go func() {
				time.Sleep(20 * time.Millisecond)
				backend.inFlight.Store(false)
				proc.PublishEvent(Event{Type: "assistant", Text: "the answer", StopReason: "end_turn"})
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			text, err := second.WaitForResponse(ctx)
			if err != nil || text != "the answer" {
				t.Fatalf("WaitForResponse after reclaim = %q, %v", text, err)
			}
			backend.mu.Lock()
			sends := append([]string(nil), backend.sends...)
			backend.mu.Unlock()
			if len(sends) != 1 || sends[0] != "do the thing" {
				t.Fatalf("provider saw sends %v", sends)
			}
		})
	}
}

// TestBrokerDaemonReclaimReplayReachesFirstSubscriber (🎯T2.11 clause 1):
// history retained while unowned is delivered to the reclaiming consumer's
// first subscriber, not lost in the gap between Start returning and
// SubscribeEvents.
func TestBrokerDaemonReclaimReplayReachesFirstSubscriber(t *testing.T) {
	f := startDaemon(t, false, nil)
	f.boot(t, false, nil)
	first, err := Start(Config{Name: "hist", WorkDir: t.TempDir(), SessionID: "sid-h", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	first.mcpCleanup()
	waitFor(t, "detached", func() bool {
		f.d.mu.Lock()
		defer f.d.mu.Unlock()
		return f.d.grants["hist"].owner == nil
	})
	proc := f.d.reg.Get("hist")
	proc.PublishEvent(Event{Type: "assistant", Text: "said while unowned", StopReason: "end_turn"})
	second, err := Start(Config{Name: "hist", WorkDir: t.TempDir(), SessionID: "sid-h", TermLogPath: "-"})
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

// TestBrokerHandleNeverRebinds (🎯T3, 🎯T2.12 residue): a broker-held handle
// whose seat reports a rate limit publishes stuck and does not switch
// model or provider; the library never auto-rebinds.
func TestBrokerHandleNeverRebinds(t *testing.T) {
	f := startDaemon(t, false, nil)
	f.boot(t, false, nil)
	a, err := Start(Config{Name: "pinned", WorkDir: t.TempDir(), SessionID: "sid-p", Model: "opus", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	got := collectEvents(a)
	proc := f.d.reg.Get("pinned")
	proc.PublishEvent(Event{Type: "assistant", IsError: true, Text: "429 rate limit exceeded"})
	waitFor(t, "stuck on the handle", func() bool {
		for _, ev := range got() {
			if ev.ProgressType == ProgressStuck {
				return true
			}
		}
		return false
	})
	if err := a.Send("next"); err != nil {
		t.Fatal(err)
	}
	for _, ev := range got() {
		if ev.ProgressType == ProgressModelSwitch {
			t.Fatalf("handle rebound on its own: %+v", ev)
		}
	}
	if a.Model() != "opus" || a.Provider() != ProviderClaude {
		t.Fatalf("handle drifted to %s/%s", a.Provider(), a.Model())
	}
	stuckCount := 0
	for _, ev := range got() {
		if ev.ProgressType == ProgressStuck {
			stuckCount++
		}
	}
	if stuckCount != 1 {
		t.Fatalf("stuck published %d times, want once (daemon does not forward its own synthesis)", stuckCount)
	}
}

// TestBrokerDaemonUsageUpdateOnTail (🎯T2.9 clause 5): every refresh emits
// usage_update per provider on the tail.
func TestBrokerDaemonUsageUpdateOnTail(t *testing.T) {
	pct := 50.0
	usage := []PlanUsage{
		{Provider: ProviderClaude, Status: PlanUsageAvailable, Windows: []PlanWindow{{Name: PlanWindowWeekly, RemainingPercent: &pct}}},
		{Provider: ProviderGrok, Status: PlanUsageUnavailable, Reason: "opt-in"},
	}
	f := startDaemon(t, false, usage)
	f.boot(t, false, usage)
	tail := tailEvents(t, f.sock)
	if _, _, err := brokerUsage(context.Background(), true); err != nil {
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
	snapshot := []PlanUsage{
		{Provider: ProviderClaude, Status: PlanUsageAvailable, Windows: []PlanWindow{{Name: PlanWindowWeekly, RemainingPercent: &low}}},
		{Provider: ProviderGrok, Status: PlanUsageAvailable, Windows: []PlanWindow{{Name: PlanWindowWeekly, RemainingPercent: &high}}},
		{Provider: ProviderCodex, Status: PlanUsageUnavailable, Reason: "signed out"},
	}
	pred := ModelPredicates{Mode: CapabilityTask, PreferPlan: true, PreferProvider: ProviderClaude}

	// Daemon branch: the socket answers with the daemon's snapshot.
	f := startDaemon(t, false, snapshot)
	f.boot(t, false, snapshot)
	waitFor(t, "daemon fetched", func() bool { return f.fetchCount() == 1 })
	viaDaemon, err := LoadPlanUsage(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadPlanUsage via daemon: %v", err)
	}
	pickDaemon, err := Resolve(context.Background(), pred)
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
	cache := &PlanUsageCacheArgs{Dir: t.TempDir(), Fetch: func(context.Context) ([]PlanUsage, error) {
		cacheFetches++
		return snapshot, nil
	}}
	viaCache, err := LoadPlanUsage(context.Background(), cache)
	if err != nil {
		t.Fatalf("LoadPlanUsage via cache: %v", err)
	}
	pred.Cache = cache
	pickCache, err := Resolve(context.Background(), pred)
	if err != nil {
		t.Fatalf("Resolve via cache: %v", err)
	}
	if cacheFetches != 1 {
		t.Fatalf("cache branch fetched %d times", cacheFetches)
	}

	normalize := func(us []PlanUsage) []PlanUsage {
		out := make([]PlanUsage, len(us))
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

// TestT70UnownedStreamSurvivesBounceRing: more than the old 256-event
// bound arrives while unowned; reclaim is not Lagged and WaitForResponse
// sees the turn's answer (🎯T70).
func TestT70UnownedStreamSurvivesBounceRing(t *testing.T) {
	f := startDaemon(t, false, nil)
	f.boot(t, false, nil)
	first, err := Start(Config{Name: "stream", WorkDir: t.TempDir(), SessionID: "sid-stream", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	backend := f.backend(0)
	if err := first.Send("do the thing"); err != nil {
		t.Fatal(err)
	}
	backend.inFlight.Store(true)
	first.mcpCleanup()
	waitFor(t, "detached", func() bool {
		f.d.mu.Lock()
		defer f.d.mu.Unlock()
		g := f.d.grants["stream"]
		return g != nil && g.owner == nil
	})
	proc := f.d.reg.Get("stream")
	if proc == nil || !proc.Alive() {
		t.Fatal("seat died when the consumer left")
	}
	const n = 300
	for i := 0; i < n-1; i++ {
		proc.PublishEvent(Event{Type: "progress", ProgressType: "tool_use", ToolTitle: fmt.Sprintf("t%d", i)})
	}
	backend.inFlight.Store(false)
	proc.PublishEvent(Event{Type: "assistant", Text: "the answer", StopReason: "end_turn"})

	f.d.mu.Lock()
	g := f.d.grants["stream"]
	ring, lagged := len(g.ring), g.lag
	f.d.mu.Unlock()
	if ring != n || lagged {
		t.Fatalf("unowned ring=%d lagged=%v, want %d unlagged (old cap was 256)", ring, lagged, n)
	}

	second, err := Start(Config{Name: "stream", WorkDir: t.TempDir(), SessionID: "sid-stream", TermLogPath: "-"})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	t.Cleanup(second.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	text, err := second.WaitForResponse(ctx)
	if err != nil || text != "the answer" {
		t.Fatalf("WaitForResponse after reclaim = %q, %v", text, err)
	}
	if f.backend(1) != nil {
		t.Fatal("reclaim started a second provider process")
	}
}

// TestBrokerDaemonForwardsSeatGone (🎯T75.6): the daemon's liveness watch
// is its Registry's. A granted seat whose process dies reaches the owner as
// agent_gone (its handle stops reporting alive) and the tail as gone, once.
func TestBrokerDaemonForwardsSeatGone(t *testing.T) {
	f := startDaemon(t, false, nil)
	f.boot(t, false, nil)
	tail := tailEvents(t, f.sock)
	a, err := Start(Config{Name: "seat-g", WorkDir: t.TempDir(), SessionID: "sid-g", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	proc := f.d.reg.Get("seat-g")
	if proc == nil {
		t.Fatal("daemon holds no proc")
	}
	proc.mu.Lock()
	proc.alive = false
	proc.mu.Unlock()

	for range 3 {
		f.clock.Advance(seatWatchInterval)
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

// TestBrokerDaemonDefaultStateDirHoldsModelIntel: a daemon built with an
// empty StateDir, as `claudia broker serve` builds it, keeps model intel in
// the resolved state directory. It used to join the empty option, writing
// ./model-intel into whatever directory the daemon ran from and resolving
// against that rather than the store consumers read.
func TestBrokerDaemonDefaultStateDirHoldsModelIntel(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cbi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv(broker.StateHomeEnv, filepath.Join(dir, "xdg"))
	t.Setenv(modelIntelEnvAAKey, "")
	cwd := t.TempDir()
	t.Chdir(cwd)

	d, err := NewBrokerDaemon(BrokerDaemonOptions{
		SocketPath:     filepath.Join(dir, "b.sock"),
		DisableResume:  true,
		DisableMCPHost: true,
		UsageFetch:     func(context.Context) ([]PlanUsage, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	want, err := DefaultModelIntelDir()
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "intel run recorded", func() bool {
		_, err := os.Stat(filepath.Join(want, modelIntelRunsFile))
		return err == nil
	})
	if _, err := os.Stat(filepath.Join(cwd, modelIntelDirName)); err == nil {
		t.Fatal("daemon wrote model intel relative to its working directory")
	}
}

// TestBrokerDaemonCarriesTaskRawLog (🎯T75.10): a raw-log func on a Task
// run through the daemon sees every line the direct path sees for the same
// backend, in order; a run without one does not ask the daemon for lines.
func TestBrokerDaemonCarriesTaskRawLog(t *testing.T) {
	lines := []string{`{"type":"system","subtype":"init"}`, `{"type":"assistant","n":1}`, `{"type":"result"}`}
	newFake := func() *fakeTaskBackend {
		return &fakeTaskBackend{name: "fake-claude", rawLines: lines, events: []TaskEvent{
			{Type: TaskEventInit, SessionID: "raw-sid"},
			{Type: TaskEventResult, Content: "done"},
		}}
	}
	collect := func(task *Task) func() []string {
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
	drain := func(task *Task) {
		t.Helper()
		ch, err := task.Run(context.Background(), "go")
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
		}
	}

	direct := newTaskWithBackend(TaskConfig{ID: "direct", WorkDir: t.TempDir()}, newFake())
	direct.direct = true
	directLines := collect(direct)
	drain(direct)

	f := startDaemon(t, false, nil)
	f.boot(t, false, nil)
	var fakes []*fakeTaskBackend
	prev := daemonNewTask
	daemonNewTask = func(cfg TaskConfig) *Task {
		fake := newFake()
		fakes = append(fakes, fake)
		tk := newTaskWithBackend(cfg, fake)
		tk.direct = true
		return tk
	}
	t.Cleanup(func() { daemonNewTask = prev })

	brokered := NewTask(TaskConfig{ID: "brokered", WorkDir: t.TempDir()})
	brokeredLines := collect(brokered)
	drain(brokered)
	if got, want := brokeredLines(), directLines(); !reflect.DeepEqual(got, want) || len(want) != len(lines) {
		t.Fatalf("raw lines through the daemon = %q, direct = %q", got, want)
	}

	plain := NewTask(TaskConfig{ID: "plain", WorkDir: t.TempDir()})
	drain(plain)
	if len(fakes) != 2 {
		t.Fatalf("daemon ran %d tasks, want 2", len(fakes))
	}
	if fakes[1].request(t).RawLog != nil {
		t.Fatal("a run without a raw-log func asked the daemon for raw lines")
	}
}

// TestBrokerDaemonRewindsHeldSeat (🎯T75.8): Rewind on a daemon-held seat
// is the daemon's Registry.Rewind. The handle is kept and re-pointed, the
// transcript loses one turn, the daemon holds the relaunched process, and
// events from it reach the owner.
func TestBrokerDaemonRewindsHeldSeat(t *testing.T) {
	f := startDaemon(t, false, nil)
	f.boot(t, false, nil)
	a, err := Start(Config{Name: "seat-rw", WorkDir: t.TempDir(), SessionID: "sid-rw", TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	old := f.d.reg.Get("seat-rw")
	if old == nil {
		t.Fatal("daemon holds no proc")
	}
	transcript := old.JSONLPath()
	writeSeatTranscript(t, transcript)
	events := collectEvents(a)

	got, err := a.Rewind(1, Config{})
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
	if b := mustRead(t, transcript); bytes.Contains(b, []byte("CHARLIE")) || !bytes.Contains(b, []byte("turn2")) {
		t.Fatalf("transcript not rewound:\n%s", b)
	}
	if a.SessionID() != "sid-rw" || !a.Alive() {
		t.Fatalf("handle after rewind: session=%q alive=%v", a.SessionID(), a.Alive())
	}
	next.PublishEvent(Event{Type: "assistant", Text: "after rewind"})
	waitFor(t, "event from relaunched process", func() bool {
		for _, ev := range events() {
			if ev.Text == "after rewind" {
				return true
			}
		}
		return false
	})
}

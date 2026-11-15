// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

func usageFixture(t *testing.T) (*UsagePage, string, string) {
	return usageProducerFixture(t, "sidecar/testdata/usage-producer.ts")
}

func usageProducerFixture(t *testing.T, script string, args ...string) (*UsagePage, string, string) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("hermetic sidecar test requires Bun: ", err)
	}
	root, err := os.MkdirTemp("", "cl-usage-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	cmd := exec.Command(bun, append([]string{script, root}, args...)...)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("actual producer: %v: %s", err, raw)
	}
	var page UsagePage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("producer page: %v: %s", err, raw)
	}
	if err := page.Validate("seat", "generation", 0, UsagePageLimit); err != nil {
		t.Fatal(err)
	}
	return &page, root, bun
}

func TestUsageCursorProtocolProducerAndBrokerReplay(t *testing.T) {
	page, _, _ := usageProducerFixture(t, "sidecar/testdata/provider-usage-producer.ts", "cursor")
	if len(page.Facts) != 2 {
		t.Fatalf("missing Cursor Run observation: %+v", page.Facts)
	}
	fact := page.Facts[1]
	if fact.State != "completed" || fact.Counters != (UsageCounters{}) || fact.ProtocolTokenDeltaTotal == nil || *fact.ProtocolTokenDeltaTotal != 7 || fact.ContextTokens == nil || *fact.ContextTokens != 23 || fact.Coverage != "provider_attempt" || fact.Provider != "cursor" {
		t.Fatalf("Cursor observation conflated with consumption: %+v", fact)
	}
	_, a, _, peer, events := receiptPipe(t)
	a.mu.Lock()
	a.sessionID, a.startCfg.Name = "generation", "seat"
	a.mu.Unlock()
	b := &Agent{sessionID: "generation", startCfg: Config{Name: "seat"}}
	for _, fact := range page.Facts {
		replyReceipt(t, peer, omp.Event{Type: "usage", Seat: "seat", UsageFact: &fact})
		ev := <-events
		if ev.Type != "usage" || !reflect.DeepEqual(ev.UsageFact, &fact) {
			t.Fatalf("socket changed Cursor fact: %+v", ev)
		}
		raw, err := EncodeEventWire(ev)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeEventWire(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded.UsageFact, &fact) {
			t.Fatal("broker changed Cursor fact")
		}
		b.publishEvent(decoded)
		b.publishEvent(decoded)
	}
	want := Usage{}
	if a.Usage() != want || b.Usage() != want {
		t.Fatalf("Cursor protocol observations entered consumption: %v %v", a.Usage(), b.Usage())
	}
}

func TestUsageCodexPhysicalProducerAndBrokerReplay(t *testing.T) {
	page, _, _ := usageProducerFixture(t, "sidecar/testdata/provider-usage-producer.ts", "codex")
	if len(page.Facts) != 4 {
		t.Fatalf("missing Codex physical retry: %+v", page.Facts)
	}
	first, second := page.Facts[1], page.Facts[3]
	if first.State != "failed" || first.Counters != (UsageCounters{}) || second.State != "completed" || first.InvocationID != second.InvocationID || first.ProviderAttemptID == second.ProviderAttemptID || second.Coverage != "provider_attempt" || second.Provider != "openai-codex" {
		t.Fatalf("Codex attempts conflated: %+v", page.Facts)
	}
	_, a, _, peer, events := receiptPipe(t)
	a.mu.Lock()
	a.sessionID, a.startCfg.Name = "generation", "seat"
	a.mu.Unlock()
	b := &Agent{sessionID: "generation", startCfg: Config{Name: "seat"}}
	for _, fact := range page.Facts {
		replyReceipt(t, peer, omp.Event{Type: "usage", Seat: "seat", UsageFact: &fact})
		ev := <-events
		if ev.Type != "usage" || !reflect.DeepEqual(ev.UsageFact, &fact) {
			t.Fatalf("socket changed Codex fact: %+v", ev)
		}
		raw, err := EncodeEventWire(ev)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeEventWire(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded.UsageFact, &fact) {
			t.Fatal("broker changed Codex fact")
		}
		b.publishEvent(decoded)
		b.publishEvent(decoded)
	}
	want := Usage{InputTokens: 21, OutputTokens: 4, CacheReadInputTokens: 7}
	if a.Usage() != want || b.Usage() != want {
		t.Fatalf("Codex aggregate counted twice: %v %v", a.Usage(), b.Usage())
	}
}

func TestUsagePhysicalProducerSocketAndBrokerPreserveRetries(t *testing.T) {
	page, _, _ := usageProducerFixture(t, "sidecar/testdata/provider-usage-producer.ts")
	if len(page.Facts) != 4 {
		t.Fatalf("not two physical requests: %+v", page)
	}
	first, second := page.Facts[1], page.Facts[3]
	if first.Coverage != "provider_attempt" || first.InvocationID == "" || first.InvocationID != second.InvocationID || first.ProviderAttemptID == second.ProviderAttemptID || first.CallID != first.ProviderAttemptID || first.Requests[0].AttemptID != "attempt-A" {
		t.Fatalf("physical identity lost: %+v", page.Facts)
	}
	if first.Counters.TotalInputTokens == nil || *first.Counters.TotalInputTokens != 10 || second.Counters.TotalInputTokens == nil || *second.Counters.TotalInputTokens != 28 || second.Counters.CacheCreationInputTokens == nil || *second.Counters.CacheCreationInputTokens != 0 {
		t.Fatalf("raw presence/total lost: %+v", page.Facts)
	}
	_, a, _, peer, events := receiptPipe(t)
	a.mu.Lock()
	a.sessionID, a.startCfg.Name = "generation", "seat"
	a.mu.Unlock()
	b := &Agent{sessionID: "generation", startCfg: Config{Name: "seat"}}
	for _, fact := range page.Facts {
		replyReceipt(t, peer, omp.Event{Type: "usage", Seat: "seat", UsageFact: &fact})
		ev := <-events
		if ev.Type != "usage" || !reflect.DeepEqual(ev.UsageFact, &fact) {
			t.Fatalf("socket lost physical evidence: %+v", ev)
		}
		raw, err := EncodeEventWire(ev)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeEventWire(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded.UsageFact, &fact) {
			t.Fatal("broker changed physical evidence")
		}
		b.publishEvent(decoded)
		b.publishEvent(decoded)
	}
	want := Usage{InputTokens: 31, OutputTokens: 5, CacheReadInputTokens: 7}
	if a.Usage() != want || b.Usage() != want {
		t.Fatalf("aggregate or replay double count: direct=%+v broker=%+v", a.Usage(), b.Usage())
	}
	changed := second
	changed.Seq++
	changed.InvocationID = "different-invocation"
	b.publishEvent(Event{Type: "usage", SessionID: "generation", UsageFact: &changed})
	if b.UsageError() == nil || b.Usage() != want {
		t.Fatal("identity mutation accepted")
	}
}

func TestUsageActualProducerSocketPumpAndBrokerReplay(t *testing.T) {
	page, root, bun := usageFixture(t)
	if len(page.Facts) != 2 {
		t.Fatalf("not one actual invocation: %+v", page)
	}
	_, a, _, peer, events := receiptPipe(t)
	a.mu.Lock()
	a.sessionID = "generation"
	a.startCfg.Name = "seat"
	a.mu.Unlock()
	b := &Agent{sessionID: "generation", startCfg: Config{Name: "seat"}}
	for _, fact := range page.Facts {
		replyReceipt(t, peer, omp.Event{Type: "usage", Seat: "seat", UsageFact: &fact})
		ev := <-events
		if ev.Type != "usage" || ev.UsageFact == nil {
			t.Fatalf("pump lost fact: %+v", ev)
		}
		raw, err := EncodeEventWire(ev)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeEventWire(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ev.UsageFact, decoded.UsageFact) {
			t.Fatal("broker changed usage identity/availability")
		}
		b.publishEvent(decoded)
		b.publishEvent(decoded)
	}
	want := Usage{InputTokens: 11, OutputTokens: 3, CacheReadInputTokens: 5}
	if a.Usage() != want || b.Usage() != want {
		t.Fatalf("delta double count: direct=%+v broker=%+v", a.Usage(), b.Usage())
	}
	if page.Facts[1].Counters.CacheCreationInputTokens != nil {
		t.Fatal("synthetic zero became measured")
	}
	// Reopen the durable history through the actual server, without loading a
	// provider seat or reading credentials. No model is available to this RPC.
	socket := filepath.Join(root, "omp.sock")
	cmd := exec.Command(bun, "sidecar/server.ts", socket)
	cmd.Env = append(os.Environ(), "CLAUDIA_OMP_SPOOL_DIR="+filepath.Join(root, "spool"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	t.Setenv(omp.SocketEnv, socket)
	ctx := t.Context()
	for !omp.Listening(ctx, socket) {
		time.Sleep(10 * time.Millisecond)
	}
	reopened, err := QueryUsageDirect(ctx, UsageQuery{Seat: "seat", SessionID: "generation", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reopened.Facts, page.Facts[:1]) || !reopened.More {
		t.Fatalf("restart changed fact: %+v", reopened)
	}
	before, err := os.ReadFile(filepath.Join(root, "omp-seats", "seat", "generation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	b.ops.usagePage = func(ctx context.Context, _ *Agent, after uint64, limit int) (*UsagePage, error) {
		return QueryUsageDirect(ctx, UsageQuery{Seat: "seat", SessionID: "generation", After: after, Limit: limit})
	}
	if err := b.RefreshUsage(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.RefreshUsage(ctx); err != nil {
		t.Fatal(err)
	}
	if b.Usage() != want {
		t.Fatalf("replay summed snapshots: %+v", b.Usage())
	}
	after, _ := os.ReadFile(filepath.Join(root, "omp-seats", "seat", "generation.jsonl"))
	if string(before) != string(after) {
		t.Fatal("read query modified history")
	}
	if _, err := QueryUsageDirect(ctx, UsageQuery{Seat: "seat", SessionID: "missing"}); err == nil {
		t.Fatal("absent session became measured zero")
	}
}

func TestUsageConflictsRemainErrorsAndDoNotAdvanceReplay(t *testing.T) {
	page, _, _ := usageFixture(t)
	a := &Agent{sessionID: "generation", startCfg: Config{Name: "seat"}}
	a.publishEvent(Event{Type: "usage", UsageFact: &page.Facts[1]})
	bad := page.Facts[1]
	n := int64(99)
	bad.Counters.InputTokens = &n
	a.ops.usagePage = func(context.Context, *Agent, uint64, int) (*UsagePage, error) {
		return &UsagePage{Seat: "seat", SessionID: "generation", Facts: []UsageFact{page.Facts[0], bad}, Next: 2}, nil
	}
	if err := a.RefreshUsage(context.Background()); err == nil {
		t.Fatal("conflicting replay reported success")
	}
	if a.usageCursor != 0 || a.UsageError() == nil || a.Usage().InputTokens != 11 {
		t.Fatalf("conflict changed accounting: cursor=%d usage=%+v", a.usageCursor, a.Usage())
	}
	foreign := page.Facts[1]
	foreign.Seat = "another"
	b := &Agent{sessionID: "generation", startCfg: Config{Name: "seat"}}
	b.publishEvent(Event{Type: "usage", UsageFact: &foreign})
	if b.UsageError() == nil || b.Usage() != (Usage{}) {
		t.Fatal("foreign seat counted")
	}
	c := &Agent{sessionID: "generation", startCfg: Config{Name: "seat"}}
	c.ops.usagePage = func(context.Context, *Agent, uint64, int) (*UsagePage, error) {
		return &UsagePage{Seat: "foreign", SessionID: "generation", Facts: []UsageFact{}}, nil
	}
	if err := c.RefreshUsage(context.Background()); err == nil {
		t.Fatal("foreign empty page accepted")
	}
}

func TestUsageProjectionRejectsForeignIdentityInOlderReplay(t *testing.T) {
	start := UsageFact{Seq: 1, CallID: "physical", ProviderAttemptID: "physical", InvocationID: "invocation", Seat: "seat", SessionID: "generation", Kind: "model", State: "started", Provider: "cursor", Model: "fixture", Requests: []UsageRequestRef{{RequestID: "logical", AttemptID: "delivery"}}, StartedAt: "2026-10-16T01:00:00Z", CounterSource: "provider_reported", Coverage: "provider_attempt"}
	final := start
	final.Seq, final.State, final.FinishedAt = 2, "completed", "2026-10-16T01:00:01Z"
	a := &Agent{sessionID: "generation", startCfg: Config{Name: "seat"}}
	a.publishEvent(Event{Type: "usage", UsageFact: &final})
	start.InvocationID = "foreign-invocation"
	a.publishEvent(Event{Type: "usage", UsageFact: &start})
	if a.UsageError() == nil {
		t.Fatal("older replay bypassed immutable usage identity validation")
	}
}

// This producer restores a real seat and triggers automatic native compaction
// before its ordinary model request. Its only fetch implementation is a frozen
// in-memory wire fixture; this test does not authenticate or call a provider.
func TestUsageNativeCompactionProducerSocketAndBrokerReplay(t *testing.T) {
	for _, backend := range []string{"openai", "codex"} {
		t.Run(backend, func(t *testing.T) {
			bun, err := exec.LookPath("bun")
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			cmd := exec.Command(bun, "sidecar/testdata/native-usage-producer.ts", root, "success", backend)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + root}
			raw, err := cmd.Output()
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					t.Fatalf("actual native producer: %v: %s", err, exit.Stderr)
				}
				t.Fatal(err)
			}
			var page UsagePage
			if err := json.Unmarshal(raw, &page); err != nil {
				t.Fatal(err)
			}
			if err := page.Validate("seat", "generation", 0, UsagePageLimit); err != nil {
				t.Fatal(err)
			}
			if len(page.Facts) != 4 {
				t.Fatalf("native plus model physical attempts: %+v", page.Facts)
			}
			compaction, model := page.Facts[1], page.Facts[3]
			if compaction.Kind != "compaction" || compaction.State != "completed" || compaction.Counters.TotalInputTokens == nil || *compaction.Counters.TotalInputTokens != 100 || model.Kind != "model" || compaction.InvocationID == model.InvocationID {
				t.Fatalf("compaction identity or raw usage lost: %+v", page.Facts)
			}
			_, a, _, peer, events := receiptPipe(t)
			a.mu.Lock()
			a.sessionID, a.startCfg.Name = "generation", "seat"
			a.mu.Unlock()
			b := &Agent{sessionID: "generation", startCfg: Config{Name: "seat"}}
			for _, fact := range page.Facts {
				replyReceipt(t, peer, omp.Event{Type: "usage", Seat: "seat", UsageFact: &fact})
				// The suite timeout is the backstop; host scheduling does not decide this assertion.
				ev := <-events
				if ev.Type != "usage" || !reflect.DeepEqual(ev.UsageFact, &fact) {
					t.Fatalf("socket changed native fact: %+v", ev)
				}
				raw, err := EncodeEventWire(ev)
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := DecodeEventWire(raw)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(decoded.UsageFact, &fact) {
					t.Fatal("broker changed native fact")
				}
				b.publishEvent(decoded)
				b.publishEvent(decoded)
			}
			// Raw inclusive inputs 100 + 28 include caches 40 + 7 already.
			// The legacy accumulator remains disjoint and replay does not add twice.
			want := Usage{InputTokens: 81, OutputTokens: 11, CacheReadInputTokens: 47}
			if a.Usage() != want || b.Usage() != want {
				t.Fatalf("native consumption double-counted: %v %v", a.Usage(), b.Usage())
			}
		})
	}
}

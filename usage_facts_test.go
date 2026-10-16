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

func usageProducerFixture(t *testing.T, script string) (*UsagePage, string, string) {
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
	cmd := exec.Command(bun, script, root)
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
		select {
		case ev := <-events:
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
		case <-time.After(5 * time.Second):
			t.Fatal("missing physical observation")
		}
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
		select {
		case ev := <-events:
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
		case <-time.After(5 * time.Second):
			t.Fatal("pump lost usage event")
		}
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
	cmd.Env = append(os.Environ(), "JEVONS_SPOOL_DIR="+filepath.Join(root, "spool"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	t.Setenv(omp.SocketEnv, socket)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var reopened *UsagePage
	var err error
	for {
		reopened, err = QueryUsageDirect(ctx, UsageQuery{Seat: "seat", SessionID: "generation", Limit: 1})
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(err)
		case <-time.After(10 * time.Millisecond):
		}
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

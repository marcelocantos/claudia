// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

func TestRegistryCompactRefusesNonPositiveBound(t *testing.T) {
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Compact(context.Background(), "missing", 0); err == nil {
		t.Fatal("want error for bound 0")
	}
}

func TestAgentCompactMapsTurnInFlight(t *testing.T) {
	var flying atomic.Bool
	flying.Store(true)
	a := &Agent{
		ready:    make(chan struct{}),
		provider: ProviderCursor,
		startCfg: Config{OMP: true},
		ops: agentOps{
			promptInFlight: func(*Agent) bool { return flying.Load() },
			compact: func(*Agent, context.Context, int) (SeatCompact, error) {
				t.Fatal("compact must not run")
				return SeatCompact{}, nil
			},
		},
	}
	close(a.ready)
	_, err := a.Compact(context.Background(), 180_000)
	if !errors.Is(err, ErrTurnInFlight) {
		t.Fatalf("Compact = %v, want ErrTurnInFlight", err)
	}
}

func TestAgentCompactReturnsObservedSizes(t *testing.T) {
	a := &Agent{
		ready:    make(chan struct{}),
		provider: ProviderCursor,
		startCfg: Config{Provider: ProviderCursor, OMP: true},
		ops: agentOps{
			promptInFlight: func(*Agent) bool { return false },
			compact: func(_ *Agent, _ context.Context, bound int) (SeatCompact, error) {
				if bound != 180_000 {
					t.Fatalf("bound = %d", bound)
				}
				return SeatCompact{TokensBefore: 200_000, TokensAfter: 120_000, Committed: true}, nil
			},
		},
	}
	close(a.ready)
	out, err := a.Compact(context.Background(), 180_000)
	if err != nil {
		t.Fatal(err)
	}
	if out.TokensBefore != 200_000 || out.TokensAfter != 120_000 || !out.Committed {
		t.Fatalf("out = %+v", out)
	}
}

func TestAgentCompactMissingOpReportsCompactCapability(t *testing.T) {
	a := &Agent{
		ready:    make(chan struct{}),
		provider: ProviderCursor,
		startCfg: Config{Provider: ProviderCursor, OMP: true},
		ops: agentOps{
			promptInFlight: func(*Agent) bool { return false },
		},
	}
	close(a.ready)
	_, err := a.Compact(context.Background(), 180_000)
	var ce *CapabilityError
	if !errors.As(err, &ce) || ce.Capability != CapabilityCompact {
		t.Fatalf("Compact = %v, want *CapabilityError compact", err)
	}
}

func TestOMPCompactNoEligibleFoldEndToEnd(t *testing.T) {
	f := startFakeSidecar(t, []string{`{"type":"turn_end","text":"ok"}`})
	f.compactReason = "no_eligible_fold"
	ctx := context.Background()
	agent, err := startDirectContext(ctx, Config{
		Provider: Provider(omp.Anthropic), Name: "jv-compact-fold", Model: "claude-sonnet-4-20250514",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Stop()
	if err := agent.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = agent.Compact(ctx, 180_000)
	if !errors.Is(err, ErrCompactNoEligibleFold) {
		t.Fatalf("Compact = %v, want ErrCompactNoEligibleFold", err)
	}
}

func TestT244CompactDuringPromptReturnsTurnInFlight(t *testing.T) {
	hold := make(chan struct{})
	f := startFakeSidecar(t, []string{`{"type":"turn_end","text":"ok"}`})
	f.promptHold = hold
	ctx := context.Background()
	agent, err := startDirectContext(ctx, Config{
		Provider: Provider(omp.Anthropic), Name: "jv-compact-race", Model: "claude-sonnet-4-20250514",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Stop()
	if err := agent.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	var promptErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		promptErr = agent.Send("hold open")
	}()
	waitForPromptInFlight(t, &f.promptInFlight)
	_, compactErr := agent.Compact(ctx, 180_000)
	close(hold)
	wg.Wait()
	if !errors.Is(compactErr, ErrTurnInFlight) {
		t.Fatalf("Compact during prompt = %v, want ErrTurnInFlight", compactErr)
	}
	if promptErr != nil {
		t.Fatalf("Send = %v", promptErr)
	}
}

func TestT244CompactContextCancelSettlesWithoutCommit(t *testing.T) {
	f := startFakeSidecar(t, []string{`{"type":"turn_end","text":"ok"}`})
	f.setCompactBehavior(false, 2*time.Second)
	ctx := context.Background()
	agent, err := startDirectContext(ctx, Config{
		Provider: Provider(omp.Anthropic), Name: "jv-compact-cancel", Model: "claude-sonnet-4-20250514",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Stop()
	if err := agent.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	compactCtx, compactCancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	var compactErr error
	go func() {
		defer wg.Done()
		_, compactErr = agent.Compact(compactCtx, 180_000)
	}()
	time.Sleep(200 * time.Millisecond)
	compactCancel()
	wg.Wait()
	if !errors.Is(compactErr, ErrCompactCanceled) {
		t.Fatalf("Compact = %v, want ErrCompactCanceled", compactErr)
	}
	if err := agent.Send("ok after cancel"); err != nil {
		t.Fatalf("Send after settled cancel = %v", err)
	}
}

func waitForPromptInFlight(t *testing.T, flying *atomic.Bool) {
	t.Helper()
	for !flying.Load() {
		select {
		case <-t.Context().Done():
			t.Fatal("prompt never entered in-flight state")
		default:
			runtime.Gosched()
		}
	}
}




func TestT244CompactCancelSendsCompactAbort(t *testing.T) {
	f := startFakeSidecar(t, []string{`{"type":"turn_end","text":"ok"}`})
	f.setCompactBehavior(false, 30*time.Second)
	ctx := context.Background()
	agent, err := startDirectContext(ctx, Config{
		Provider: Provider(omp.Anthropic), Name: "jv-compact-abort-wire", Model: "claude-sonnet-4-20250514",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Stop()
	if err := agent.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	compactCtx, compactCancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	var compactErr error
	go func() {
		defer wg.Done()
		_, compactErr = agent.Compact(compactCtx, 180_000)
	}()
	time.Sleep(300 * time.Millisecond)
	compactCancel()
	wg.Wait()
	if !errors.Is(compactErr, ErrCompactCanceled) {
		t.Fatalf("Compact = %v, want ErrCompactCanceled", compactErr)
	}
	f.compactAbortsMu.Lock()
	n := len(f.compactAborts)
	f.compactAbortsMu.Unlock()
	if n != 1 {
		t.Fatalf("compact_abort sends = %d, want 1", n)
	}
}

func TestT244CompactUncertainBlocksSendUntilRecovery(t *testing.T) {
	f := startFakeSidecar(t, []string{`{"type":"turn_end","text":"ok"}`})
	f.setCompactBehavior(true, 0)
	ctx := context.Background()
	agent, err := startDirectContext(ctx, Config{
		Provider: Provider(omp.Anthropic), Name: "jv-compact-uncertain", Model: "claude-sonnet-4-20250514",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Stop()
	if err := agent.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	compactCtx, compactCancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	var compactErr error
	go func() {
		defer wg.Done()
		_, compactErr = agent.Compact(compactCtx, 180_000)
	}()
	time.Sleep(200 * time.Millisecond)
	compactCancel()
	wg.Wait()
	if !errors.Is(compactErr, ErrCompactUncertain) {
		t.Fatalf("Compact = %v, want ErrCompactUncertain", compactErr)
	}
	if err := agent.Send("blocked"); !errors.Is(err, ErrCompactUncertain) {
		t.Fatalf("Send while uncertain = %v, want ErrCompactUncertain", err)
	}
	f.setCompactBehavior(false, 0)
	if _, err := agent.Compact(ctx, 180_000); err != nil {
		t.Fatalf("recovery Compact = %v", err)
	}
	if err := agent.Send("recovered"); err != nil {
		t.Fatalf("Send after recovery = %v", err)
	}
}

func TestT244HeldNextPromptRefusedWhileCompactInFlight(t *testing.T) {
	f := startFakeSidecar(t, []string{`{"type":"turn_end","text":"ok"}`})
	f.setCompactBehavior(false, 400*time.Millisecond)
	ctx := context.Background()
	agent, err := startDirectContext(ctx, Config{
		Provider: Provider(omp.Anthropic), Name: "jv-compact-hold-send", Model: "claude-sonnet-4-20250514",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Stop()
	if err := agent.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	var compactErr error
	go func() {
		defer wg.Done()
		_, compactErr = agent.Compact(ctx, 180_000)
	}()
	time.Sleep(50 * time.Millisecond)
	if err := agent.Send(strings.Repeat("x", 8000)); !errors.Is(err, ErrTurnInFlight) {
		t.Fatalf("Send during compact = %v, want ErrTurnInFlight", err)
	}
	wg.Wait()
	if compactErr != nil {
		t.Fatalf("Compact = %v", compactErr)
	}
	if err := agent.Send("after compact"); err != nil {
		t.Fatalf("Send after compact = %v", err)
	}
}

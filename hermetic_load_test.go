// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// 🎯T31 remaining work: a synthetic fleet load, and RED-without-fix
// evidence that the pre-fix helper fails the stall the new helper waits out.
//
// waitHermeticTaskReady is gone (hermetic_wait_test.go). These tests do not
// bring it back as a verdict; they keep its algorithm only so the stall
// contrast can fail it.

// legacyHermeticReadyDeadline is the 5s wall clock waitHermeticTaskReady
// used to carry. Widening it is not the fix (🎯T17 / 🎯T31).
const legacyHermeticReadyDeadline = 5 * time.Second

// hermeticReadyStall is past that deadline. The pre-fix helper must RED
// here; the signal-driven helper must wait the fake out.
const hermeticReadyStall = legacyHermeticReadyDeadline + time.Second

const (
	syntheticFleetAgents     = 16
	syntheticRaceWorkerFloor = 8
)

// waitHermeticTaskReadyLegacy is the pre-🎯T31 helper, returning the error
// it used to t.Fatal so the stall contrast can show the RED without failing
// the suite. Algorithm is unchanged: poll ClaudeID until a wall-clock
// deadline.
func waitHermeticTaskReadyLegacy(task *Task, deadline time.Duration) error {
	until := time.Now().Add(deadline)
	for time.Now().Before(until) {
		if task.ClaudeID() != "" {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return errors.New("hermetic task never became ready (no TaskEventInit / ClaudeID)")
}

type delayedInitTaskBackend struct {
	delay     time.Duration
	sessionID string
}

func (b delayedInitTaskBackend) Capabilities() providerCapabilities {
	return providerCapabilities{Task: true}
}

func (b delayedInitTaskBackend) RunTask(ctx context.Context, _ taskRunRequest) (*taskRun, error) {
	ch := make(chan TaskEvent, 2)
	go func() {
		defer close(ch)
		timer := time.NewTimer(b.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return
		}
		select {
		case ch <- TaskEvent{Type: TaskEventInit, SessionID: b.sessionID}:
		case <-ctx.Done():
			return
		}
		select {
		case ch <- TaskEvent{Type: TaskEventResult, Content: "done"}:
		case <-ctx.Done():
		}
	}()
	return &taskRun{events: ch}, nil
}

// holdAfterInitTaskBackend emits init then holds until Cancel or ctx
// cancel — the in-process stand-in for a live agent mid-turn.
type holdAfterInitTaskBackend struct{}

func (holdAfterInitTaskBackend) Capabilities() providerCapabilities {
	return providerCapabilities{Task: true}
}

func (holdAfterInitTaskBackend) RunTask(ctx context.Context, _ taskRunRequest) (*taskRun, error) {
	ch := make(chan TaskEvent, 1)
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(ch)
		select {
		case ch <- TaskEvent{Type: TaskEventInit, SessionID: "fleet-sess"}:
		case <-ctx.Done():
			return
		}
		select {
		case <-done:
		case <-ctx.Done():
		}
	}()
	return &taskRun{
		events: ch,
		interrupt: func() error {
			once.Do(func() { close(done) })
			return nil
		},
	}, nil
}

type delayedInitArgs struct {
	Delay     time.Duration
	SessionID string
}

func startDelayedInitTask(t *testing.T, args *delayedInitArgs) (*Task, *hermeticTaskWatch) {
	t.Helper()
	if args == nil {
		args = &delayedInitArgs{}
	}
	delay := args.Delay
	if delay <= 0 {
		delay = hermeticReadyStall
	}
	sessionID := args.SessionID
	if sessionID == "" {
		sessionID = "stall-sess"
	}
	task := newTaskWithBackend(TaskConfig{
		ID:      "stall-task",
		WorkDir: t.TempDir(),
	}, delayedInitTaskBackend{delay: delay, sessionID: sessionID})
	events, err := task.Run(t.Context(), "stall")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return task, watchHermeticTask(events)
}

// syntheticLoadArgs configures the fleet-load generator. Zero fields take
// the defaults a live fleet actually applies: a dozen-plus concurrent
// agents plus a second -race-suite's worth of instrumented workers.
type syntheticLoadArgs struct {
	Agents  int
	Workers int
}

// startSyntheticLoad applies that load for the rest of the test. The
// agents are real Task.Run pumps holding after init; the workers write a
// mutex-protected map and allocate, which is what a second `go test -race`
// suite spends its time doing. Both stop in Cleanup.
func startSyntheticLoad(t *testing.T, args *syntheticLoadArgs) {
	t.Helper()
	if args == nil {
		args = &syntheticLoadArgs{}
	}
	agents := args.Agents
	if agents <= 0 {
		agents = syntheticFleetAgents
	}
	workers := args.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0) * 4
		if workers < syntheticRaceWorkerFloor {
			workers = syntheticRaceWorkerFloor
		}
	}

	ctx := t.Context()
	work := t.TempDir()
	watches := make([]*hermeticTaskWatch, 0, agents)
	for i := 0; i < agents; i++ {
		task := newTaskWithBackend(TaskConfig{
			ID:      fmt.Sprintf("synthetic-fleet-%d", i),
			WorkDir: work,
		}, holdAfterInitTaskBackend{})
		events, err := task.Run(ctx, "fleet-load")
		if err != nil {
			t.Fatalf("synthetic fleet agent %d: %v", i, err)
		}
		watches = append(watches, watchHermeticTask(events))
	}
	for _, w := range watches {
		w.waitReady(t)
	}

	wctx, stopWorkers := context.WithCancel(context.Background())
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		shared = make(map[int]int, 64)
	)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			var sink uint64
			for {
				select {
				case <-wctx.Done():
					return
				default:
					for j := 0; j < 64; j++ {
						mu.Lock()
						shared[j] = j
						sink += uint64(shared[j])
						mu.Unlock()
						_ = make([]byte, 256)
					}
					runtime.Gosched()
				}
			}
		}()
	}
	t.Cleanup(func() {
		stopWorkers()
		wg.Wait()
	})
	t.Logf("synthetic load: %d agents + %d race-suite workers", agents, workers)
}

// TestHermeticTaskReadyStallContrast is the RED-without-fix evidence.
// The same fake is delayed past the old 5s deadline: the pre-fix helper
// fails that stall, and watchHermeticTask.waitReady waits it out.
func TestHermeticTaskReadyStallContrast(t *testing.T) {
	task, watch := startDelayedInitTask(t, &delayedInitArgs{
		Delay:     hermeticReadyStall,
		SessionID: "stall-sess",
	})

	err := waitHermeticTaskReadyLegacy(task, legacyHermeticReadyDeadline)
	if err == nil {
		t.Fatal("pre-fix helper passed a stall past its 5s deadline; the clock is not what failed")
	}
	if task.ClaudeID() != "" {
		t.Fatal("ClaudeID set inside the old deadline; the fake was not delayed past 5s")
	}

	watch.waitReady(t)
	if task.ClaudeID() != "stall-sess" {
		t.Fatalf("ClaudeID = %q after signal wait, want stall-sess", task.ClaudeID())
	}
}

// TestHermeticOraclesSurviveFleetLoad runs the readiness-sensitive
// oracles while a synthetic fleet (16 holding Task.Run agents) and a
// second -race-suite analog (instrumented alloc/map workers) are already
// on the machine. A quiet-machine pass is not this test.
func TestHermeticOraclesSurviveFleetLoad(t *testing.T) {
	startSyntheticLoad(t, nil)

	t.Run("signal-ready-under-load", func(t *testing.T) {
		task := newTaskWithBackend(TaskConfig{
			ID:      "load-ready",
			WorkDir: t.TempDir(),
		}, holdAfterInitTaskBackend{})
		events, err := task.Run(t.Context(), "hold")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		watch := watchHermeticTask(events)
		watch.waitReady(t)
		if task.ClaudeID() == "" {
			t.Fatal("signal helper returned without ClaudeID")
		}
		if err := task.Cancel(); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		watch.waitDrained()
	})

	t.Run("spawn-drain-under-load", func(t *testing.T) {
		bin := writeFakeCLI(t, "testdata/claude/exec/success.jsonl", 0)
		t.Setenv("CLAUDE_BIN", bin)
		task := NewTask(TaskConfig{
			ID:       "load-spawn",
			Provider: ProviderClaude,
			WorkDir:  t.TempDir(),
		})
		events, err := task.Run(t.Context(), "summarize")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		got := drainTaskEvents(t, events)
		var sawInit, sawResult bool
		for _, ev := range got {
			switch ev.Type {
			case TaskEventInit:
				sawInit = true
			case TaskEventResult:
				sawResult = true
			case TaskEventError:
				t.Errorf("unexpected error event: %s", ev.ErrorMsg)
			}
		}
		if !sawInit || !sawResult {
			t.Fatalf("events incomplete under load: init=%v result=%v got=%#v", sawInit, sawResult, got)
		}
	})

	t.Run("cancel-slow-fake-under-load", func(t *testing.T) {
		bin := writeSlowFakeCLI(t, "testdata/claude/exec/success.jsonl", 30*time.Second)
		t.Setenv("CLAUDE_BIN", bin)
		task := NewTask(TaskConfig{
			ID:       "load-cancel",
			Provider: ProviderClaude,
			WorkDir:  t.TempDir(),
		})
		events, err := task.Run(t.Context(), "hang please")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		watch := watchHermeticTask(events)
		watch.waitReady(t)
		if err := task.Cancel(); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		watch.waitDrained()
	})
}

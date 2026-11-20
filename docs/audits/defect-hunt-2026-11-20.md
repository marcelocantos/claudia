# Defect hunt, 2026-11-20

A read-only hunt for defects the existing tests do not catch, at a6fdf67 (master). Every defect below has a test that fails against that commit because of it; each test is inline, and the shared helpers it needs are in the appendix. Nothing in the product was changed.

Severity: **Critical** is one of the owner's worst outcomes: an agent turn silently lost or run twice, a backend process left running with nobody attached, failover that picks an exhausted provider or loops, or a seat reported healthy while its process is dead. **High** is a hang of a seat or daemon path, persisted-state corruption, a guard or policy bypass, or a crash. **Medium** is a wrong status, a misleading error, a race with a bounded effect, or a bounded leak. **Low** is cosmetic or needs an unusual trigger.

To run a Go repro, put the file at the path shown in a checkout, add the helpers listed for that file in the appendix, and run `CLAUDIA_NO_BROKER=1 CLAUDIA_OMP_NO_REFRESH=1 go test -race -count=1 -run '^TestName$' ./<pkg>` (H14 and H15 need `CLAUDIA_OMP_NO_REFRESH` unset). To run a sidecar repro, put the file in `sidecar/` and run `bun test <file>` with a private HOME, XDG_* and TMPDIR. All the sidecar repros use the repo's existing `sidecar/testdata` fixtures.

## Summary

| ID | Defect (desired state) | Severity | Where |
|----|------------------------|----------|-------|
| C1 | A reclaim replays every event that was queued for an owner that died, in order | Critical | `daemon/daemon.go:1326-1349` |
| C2 | A reclaimer that dies during replay never takes the seat's unowned history with it | Critical | `daemon/daemon.go:1005-1006` |
| C3 | WaitForResponse after Interrupt and Send never returns the aborted turn's end as the new turn's answer | Critical | `omp_agent.go:565-566` |
| C4 | Dropping a busy sidecar seat never runs its queued prompt, and drop followed by reload never runs a prompt twice or corrupts the session store | Critical | `sidecar/server.ts:471-477` |
| C5 | A prompt refused during compaction never stays durably accepted | Critical | `sidecar/seat.ts:905` |
| C6 | A steer that reaches an idle seat is run or refused, never acknowledged and parked | Critical | `sidecar/seat.ts:940-954` |
| C7 | A sidecar that exits on lifeline EOF leaves none of its tool processes running | Critical | `sidecar/server.ts:84-92` |
| C8 | Stopping a sidecar seat through the Registry leaves no turn or tool process running | Critical | `omp_agent.go:589-591` |
| C9 | A cancelled brokered StartContext never leaves a seat running with no owner | Critical | `broker_client.go:262-267` |
| C10 | One consumer that stops reading never stops the daemon from reporting other seats' deaths | Critical | `daemon/daemon.go:2274-2276` |
| C11 | Fleet status never reports an agent whose seat process has died as idle | Critical | `fleet.go:1386-1394` |
| H1 | Resolve honours an exclusion or preference named by the sidecar runtime id | High | `resolve.go:146-148` |
| H2 | An unreadable grants.json is never opened as an empty registry and then overwritten | High | `registry.go:236-245` |
| H3 | A seat released with stop stays released when the save fails and the daemon restarts | High | `registry.go:810-817` |
| H4 | A plain grant never rewrites a live seat's provider in grants.json | High | `daemon/daemon.go:917` |
| H5 | An operator CLI command never erases a seat's persisted policy | High | `cmd/claudia/broker_seat.go:546-591` |
| H6 | A broker-held seat keeps the consumer's Config.ProtectedPaths | High | `broker_wire.go:213-255` |
| H7 | A sidecar seat start honours cancellation after the load is sent | High | `omp_agent.go:385-403` |
| H8 | The shared-clone git guards see the subcommand after `git -c key=value` | High | `sidecar/gitdestructguard.ts:24-30` |
| H9 | Host lines reach the seat with their UTF-8 intact, whatever the socket chunking | High | `sidecar/server.ts:236` |
| H10 | A tail subscriber that stops reading never blocks the daemon | High | `internal/broker/server.go:524-530` |
| H11 | A reclaim delivers the unowned history before any live event | High | `daemon/daemon.go:1007-1027` |
| H12 | Detaching a slow consumer and its same-connection re-claim keep event order | High | `daemon/daemon.go:1275-1281` |
| H13 | Two connections granting the same name at once never both get it | High | `daemon/daemon.go:880-893` |
| H14 | A refusal refresh never hands one plan's token to another plan | High | `omp_agent.go:1464-1466` |
| H15 | Concurrent launches refresh an expired plan once | High | `omp_agent.go:1328-1341` |
| H16 | A plan renewal never reverts another plan's renewed record | High | `omp/cred.go:414-423` |
| H17 | Stopping or adopting a cursor seat never signals an unrelated process | High | `cursor_reap.go:80-82` |
| H18 | A wait retried after a timeout never loses the text the timed-out wait collected | High | `turn_latch.go:141-151` |
| H19 | Cancelling a brokered Task run mid-stream never crashes the consumer | High | `broker_task.go:66` |
| H20 | One malformed sidecar line never kills the handle while the seat keeps running | High | `omp/sidecar.go:347-356` |
| H21 | Registry.Descendants never recurses forever on a Parent cycle | High | `registry.go:1001-1016` |
| H22 | A stdio MCP server that stops reading stdin never blocks every caller and the host's Close | High | `mcp_host_stdio.go:216-238` |
| M1 | A stale ephemeral TTL timer never reaps the seat that replaced it | Medium | `daemon/daemon.go:731-776` |
| M2 | A registry change whose save failed never stays in memory to make the retry a no-op | Medium | `registry.go:407-425` |
| M3 | A provider switch that reports success leaves the definition naming the provider the seat is on | Medium | `registry_switch.go:76-80` |
| M4 | A wait that starts after a context-overflow refusal still returns ErrContextOverflow | Medium | `turn_latch.go:113-122` |
| M5 | A context-overflow refusal is never classified as a rate limit | Medium | `stuck.go:46` |
| M6 | A transient policy hold lifted before boot resume never keeps the seat stood down | Medium | `daemon/consumer_intent.go:24-43` |
| M7 | The broker never reports a committed compaction as failed | Medium | `daemon/daemon.go:1638-1663` |
| M8 | A bound compaction ends promptly when its sidecar dies | Medium | `omp_agent.go:1209-1221` |
| M9 | The host plan-usage floor admits one request at a time | Medium | `plan_usage.go:349-356` |
| M10 | A seat policy that permits no destination never migrates the seat | Medium | `seat_policy.go:37` |
| M11 | A seat stopped by a control intent stays stopped at the next daemon boot | Medium | `controlplane/reconcile.go:143-174` |
| M12 | Stopping a sidecar seat from inside its own event callback never deadlocks | Medium | `omp_agent.go:790-794` |
| M13 | Killing a stdio MCP server stops a server started through a wrapper | Medium | `mcp_host_stdio.go:261` |
| M14 | A stopped sidecar seat never reports Alive | Medium | `omp_agent.go:790-794` |
| M15 | The daemon reads a grant's owner and process under its lock | Medium | `daemon/daemon.go:1566-1580` |
| M16 | A plan renewal reads a seat's provider under the seat's lock | Medium | `omp_agent.go:1397` |
| M17 | SessionID, JSONLPath and WindowID never race a broker Rewind repoint | Medium | `broker_agent.go:482-487` |
| M18 | Escalation never pins stopped agents in the global ladders map | Medium | `escalation.go:49` |
| L1 | Plan-usage pacing never stalls beyond its floor after the wall clock steps back | Low | `plan_usage_throttle.go:139-160` |
| L2 | A broker client call honours its context while writing the request | Low | `broker_client.go:217-230` |
| L3 | The registry copies ContextPins in and out | Low | `registry_lifecycle.go:93-109` |
| L4 | The plan-usage recorder never turns a body read error into a complete body | Low | `plan_usage_record.go:69-74` |
| L5 | RegisterChain never writes outside the chains directory | Low | `chain.go:25-26` |
| L6 | Removing a seat leaves no chain file behind | Low | `agent.go:922` |

57 confirmed defects: 11 critical, 22 high, 18 medium, 6 low. 47 unconfirmed suspicions are listed after them.

## Critical

### C1. A reclaim replays every event that was queued for an owner that died, in order

**Severity: Critical.** Events, including a turn's final answer, vanish while the reclaim reports `Lagged=false`, so the consumer cannot know the turn's output is incomplete.

**Mechanism.** When an owner's connection closes, `detachLocked` closes the pump channel while it still holds queued events. `runPump` keeps ranging, its next write fails, and it retains only the event it was writing before returning; everything still buffered in the closed channel (the queue holds up to `brokerPumpSize` = 100,000) is discarded. The retained event is also appended after events the forwarder put in the ring since the detach, so what survives replays out of order.

**Where.** daemon/daemon.go:1326-1349 (`runPump`), daemon/daemon.go:703-713 (`detachLocked`)

**Reproduction.**

`daemon/zz_hunt_pump_tail_test.go` (package and helpers in the appendix):

```go
// A consumer whose connection dies while its pump still holds queued events.
// detachLocked closes the pump channel; runPump's failed write retains only
// the event it was writing and returns, so every event still buffered in the
// closed channel is discarded. The reclaim then reports Lagged=false.
func TestHuntReclaimReplaysEveryQueuedEvent(t *testing.T) {
	const name = "replay-many"
	const queued = 50
	f := newFixture(t)
	gate, release := heldPump(t)
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost, opts.pumpGate = true, true, gate
	f.bootWith(t, opts)
	t.Cleanup(release)

	old := rawSeat(t, f.sock, name)
	proc := f.d.reg.Get(name)
	for i := 0; i < queued; i++ {
		proc.PublishEvent(claudia.Event{Type: "progress", ProgressType: "tool_use", ToolTitle: fmt.Sprintf("t%d", i)})
	}
	f.d.mu.Lock()
	pump := f.d.grants[name].pump
	f.d.mu.Unlock()
	waitFor(t, "pump to hold one event in flight", func() bool { return len(pump) == queued-1 })
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "disconnect to detach the owner", func() bool { return f.unowned(name) })
	release()
	waitFor(t, "failed write to retain its event", func() bool {
		f.d.mu.Lock()
		defer f.d.mu.Unlock()
		return len(f.d.grants[name].ring) != 0
	})

	replacement, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: *f.d.reg.Def(name)})
	if err != nil {
		t.Fatal(err)
	}
	got := rawCall(t, replacement, &broker.Request{ID: "reclaim", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: name, Def: raw, Adopt: true, Fallback: true}})
	if got.Type != broker.TypeGranted {
		t.Fatalf("reclaim failed: %+v", got.Error)
	}
	if got.Granted.Replayed < queued && !got.Granted.Lagged {
		t.Fatalf("%d events were queued for the dead consumer; reclaim replayed %d and reported lagged=%v",
			queued, got.Granted.Replayed, got.Granted.Lagged)
	}
}
```

`daemon/zz_hunt_pump_test.go` (package and helpers in the appendix):

```go
// TestHuntOwnerLossReplayKeepsOrderAndTail extends the prior lead
// TestAuditReclaimDoesNotLoseUndeliveredPumpTail: when the owner's
// connection goes, the pump's in-flight event is appended to the ring after
// events the seat produced since, and the events still queued behind it are
// dropped without marking the replay lagged.
func TestHuntOwnerLossReplayKeepsOrderAndTail(t *testing.T) {
	const name = "lost"
	f := newFixture(t)
	gate, release := heldPump(t)
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost, opts.pumpGate = true, true, gate
	f.bootWith(t, opts)
	t.Cleanup(release)

	old := rawSeat(t, f.sock, name)
	proc := f.d.reg.Get(name)
	f.d.mu.Lock()
	pump := f.d.grants[name].pump
	f.d.mu.Unlock()
	huntPublish(proc, "e0")
	waitFor(t, "pump to dequeue e0", func() bool { return len(pump) == 0 })
	huntPublish(proc, "e1") // queued behind the in-flight write
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "owner loss to detach", func() bool { return f.unowned(name) })
	huntPublish(proc, "e2") // produced while unowned: ring
	release()
	waitFor(t, "failed write to be retained", func() bool {
		f.d.mu.Lock()
		defer f.d.mu.Unlock()
		return len(f.d.grants[name].ring) >= 2
	})
	time.Sleep(200 * time.Millisecond)

	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: *f.d.reg.Def(name)})
	if err != nil {
		t.Fatal(err)
	}
	resp := rawCallCollect(t, c, &broker.Request{ID: "reclaim", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: name, Def: raw, Adopt: true, Fallback: true}})
	if order := huntEventsOnly(resp.titles); order != "e0,e1,e2" {
		t.Fatalf("reclaim replayed %s (replayed=%d lagged=%v), want e0,e1,e2",
			order, resp.granted.Replayed, resp.granted.Lagged)
	}
}
```

**Evidence.**

- `zz_hunt_pump_tail_test.go:60: 50 events were queued for the dead consumer; reclaim replayed 1 and reported lagged=false`
- `zz_hunt_pump_test.go:148: reclaim replayed e2,e0 (replayed=2 lagged=false), want e0,e1,e2`

**Smallest fix.** On a failed write in `runPump`, take `d.mu`, drain what is left in `pump`, and put `[raw] + rest` at the front of `g.ring` (applying the ring cap and setting `g.lag` if it trims). The prior attempt's `TestAuditReclaimDoesNotLoseUndeliveredPumpTail` is the two-event form of the first test.

### C2. A reclaimer that dies during replay never takes the seat's unowned history with it

**Severity: Critical.** A second crash in a restart loop silently empties the history; the next reclaim gets `Replayed=0, Lagged=false`.

**Mechanism.** `handleGrant` empties `g.ring` under the lock and then writes the saved slice to the new owner, ignoring write errors. If that connection is already gone, the history is written into a dead socket and is not kept anywhere.

**Where.** daemon/daemon.go:1005-1006 and 1025-1027

**Reproduction.**

`daemon/zz_hunt_replay_crash_test.go` (package and helpers in the appendix):

```go
// TestHuntReclaimerCrashMidReplayKeepsHistory: handleGrant empties the
// reclaim ring before writing it to the new owner and ignores write errors.
// A consumer that dies while its reclaim is being replayed (a second crash
// in a restart loop) takes the seat's unowned history with it: the next
// reclaim is told Replayed=0, Lagged=false, as if nothing had happened.
func TestHuntReclaimerCrashMidReplayKeepsHistory(t *testing.T) {
	const name = "history"
	f := newFixture(t)
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost = true, true
	f.bootWith(t, opts)

	old := rawSeat(t, f.sock, name)
	proc := f.d.reg.Get(name)
	_ = old.Close()
	waitFor(t, "owner gone", func() bool { return f.unowned(name) })
	const n = 50
	for i := 0; i < n; i++ {
		huntPublish(proc, fmt.Sprintf("e%02d", i))
	}
	raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: *f.d.reg.Def(name)})
	if err != nil {
		t.Fatal(err)
	}
	reclaim := &broker.Request{ID: "reclaim", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: name, Def: raw, Adopt: true, Fallback: true}}

	// The first reclaimer sends its grant and dies before reading anything.
	crashed, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := crashed.WriteRequest(reclaim); err != nil {
		t.Fatal(err)
	}
	_ = crashed.Close()
	waitFor(t, "crashed reclaimer's grant to be handled and dropped", func() bool {
		f.d.mu.Lock()
		defer f.d.mu.Unlock()
		g := f.d.grants[name]
		return g != nil && g.owner == nil && len(g.ring) != n
	})

	c := dialRaw(t, f.sock)
	got := rawCallCollect(t, c, reclaim)
	if len(got.titles) != n && !got.granted.Lagged {
		t.Fatalf("second reclaim replayed %d of %d events, lagged=%v: history lost silently",
			len(got.titles), n, got.granted.Lagged)
	}
}
```

**Evidence.**

- `zz_hunt_replay_crash_test.go:57: second reclaim replayed 0 of 50 events, lagged=false: history lost silently`

**Smallest fix.** Clear only the replayed prefix of the ring, and only after the `Granted` reply has been written. On a write error, put the unreplayed suffix back at the front of the ring.

### C3. WaitForResponse after Interrupt and Send never returns the aborted turn's end as the new turn's answer

**Severity: Critical.** The new turn's wait returns `"", nil` (an empty answer reported as success) and the real answer goes to whoever waits next. The escalation ladder's interrupt rung produces this on every escalated message.

**Mechanism.** The sidecar interrupt op returns as soon as the abort is written (omp_agent.go:565-566). The sidecar then sends the aborted turn's own `turn_end`, which the pump publishes as an ordinary terminal event. A plain Send arms the latch before writing (agent.go:1526), and neither `WaitForResponse`'s subscriber nor the latch checks which turn an event belongs to, so the old turn's end satisfies the new wait. Existing tests miss it because `startFakeSidecar` ignores `OpAbort` and the real-sidecar abort test waits for the aborted turn's final event before its second send.

**Where.** omp_agent.go:565-566; agent.go:1815-1847; turn_latch.go:99-136; escalation.go:167

**Reproduction.**

`zz_hunt_interrupt_send_test.go` (package and helpers in the appendix):

```go
// The real sidecar answers an abort with the open turn's own turn_end
// (stop_reason "aborted", no text), and only then starts the next queued
// prompt (sidecar/server.ts op "abort"). Interrupt returns as soon as the
// abort is written, so a Send right behind it arms the next turn before the
// aborted turn's end comes back. Here the fake emits that turn_end when it
// reads the second prompt: the wire order is the real one (A's end before
// anything of B), and B's model call then takes 600ms, as a real one does.
func TestHuntInterruptThenSendReturnsTheNewTurnsAnswer(t *testing.T) {
	var turnA string
	startHuntSidecar(t, func(h *huntSidecar, msg omp.Message) {
		switch msg.Op {
		case omp.OpPrompt:
			if turnA == "" {
				turnA = msg.TurnID
				h.write(t, map[string]any{"seat": msg.Seat, "type": "text", "text": "working on A", "turn_id": turnA})
				return
			}
			turnB := msg.TurnID
			h.write(t, map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": turnA, "stop_reason": "aborted"})
			go func() {
				time.Sleep(600 * time.Millisecond)
				h.write(t, map[string]any{"seat": msg.Seat, "type": "text", "text": "answer B", "turn_id": turnB})
				h.write(t, map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": turnB})
			}()
		case omp.OpAbort:
			// Acknowledged by the turn_end above (emitted when B arrives).
		}
	})
	a, err := StartDirectContext(t.Context(), Config{
		Name: "hunt", Provider: ProviderClaude, Model: "fixture-model",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()

	sawA := make(chan struct{}, 1)
	a.SubscribeEvents(func(ev Event) {
		if ev.Type == "assistant" && ev.Text == "working on A" {
			select {
			case sawA <- struct{}{}:
			default:
			}
		}
	})
	if err := a.Send("question A"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sawA:
	case <-time.After(10 * time.Second):
		t.Fatal("turn A never started")
	}
	if err := a.Interrupt(); err != nil {
		t.Fatal(err)
	}
	if err := a.Send("question B"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got, err := a.WaitForResponse(ctx)
	if err != nil || got != "answer B" {
		t.Fatalf("WaitForResponse after Interrupt+Send(B) = %q, %v; want %q (the aborted turn A's end was taken as B's answer)", got, err, "answer B")
	}
}
```

`zz_hunt_escalate_test.go` (package and helpers in the appendix):

```go
// SendEscalating on a busy seat: the message is queued behind turn A, and
// when it has not been absorbed by the rung's deadline the ladder interrupts
// A. The sidecar answers the abort with A's own turn_end (stop_reason
// "aborted") and then drains the queue as a new turn B (server.ts op
// "abort"; seat.ts "The ordinary interrupt path delivers queues"). The host's
// wait for its escalated message must return B's answer, not A's empty end.
// No timing race here: the waiter is subscribed long before the rung fires.
func TestHuntEscalatedMessageWaitGetsItsOwnAnswer(t *testing.T) {
	var turnA, queuedB string
	startHuntSidecar(t, func(h *huntSidecar, msg omp.Message) {
		switch msg.Op {
		case omp.OpPrompt:
			if turnA == "" {
				turnA = msg.TurnID
				h.write(t, map[string]any{"seat": msg.Seat, "type": "accepted", "turn_id": turnA})
				h.write(t, map[string]any{"seat": msg.Seat, "type": "text", "text": "A is running a long tool", "turn_id": turnA})
				return
			}
			// Mid-turn: queued behind A, acknowledged at once (seat.ts emits
			// this accepted with no turn_id).
			queuedB = msg.TurnID
			h.write(t, map[string]any{"seat": msg.Seat, "type": "accepted"})
		case omp.OpAbort:
			h.write(t, map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": turnA, "stop_reason": "aborted"})
			turnB := queuedB
			go func() {
				h.write(t, map[string]any{"seat": msg.Seat, "type": "accepted", "turn_id": turnB})
				time.Sleep(600 * time.Millisecond)
				h.write(t, map[string]any{"seat": msg.Seat, "type": "text", "text": "answer B", "turn_id": turnB})
				h.write(t, map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": turnB})
			}()
		}
	})
	a, err := StartDirectContext(t.Context(), Config{
		Name: "hunt-escalate", Provider: ProviderClaude, Model: "fixture-model",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	sawA := make(chan struct{}, 1)
	a.SubscribeEvents(func(ev Event) {
		if ev.Text == "A is running a long tool" {
			select {
			case sawA <- struct{}{}:
			default:
			}
		}
	})
	if err := a.Send("question A"); err != nil {
		t.Fatal(err)
	}
	<-sawA
	if a.TurnPhase() != TurnInTurn {
		t.Fatal("fixture: seat should be busy")
	}
	out, err := a.SendEscalating("urgent B", Escalation{
		{Mode: DeliverySubmit},
		{Mode: DeliveryInterrupt, After: 200 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("SendEscalating: %+v %v", out, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got, err := a.WaitForResponse(ctx)
	if err != nil || got != "answer B" {
		next, nextErr := a.WaitForResponse(ctx)
		t.Fatalf("wait for escalated message = %q, %v; want %q. The next wait returned %q, %v", got, err, "answer B", next, nextErr)
	}
}
```

`zz_hunt_interrupt_send_real_test.go` (package and helpers in the appendix):

```go
// Same as TestHuntInterruptThenSendReturnsTheNewTurnsAnswer, but the fake
// answers the abort exactly as sidecar/server.ts does: at once, with the
// aborted turn's turn_end. Nothing is held back; the only race is the real
// one between Interrupt's return and the abort's echo. Run with -count to
// see how often the host loses it. The second wait shows where B's answer
// went: to whoever waits next.
func TestHuntInterruptThenSendRealOrdering(t *testing.T) {
	var turnA string
	startHuntSidecar(t, func(h *huntSidecar, msg omp.Message) {
		switch msg.Op {
		case omp.OpPrompt:
			if turnA == "" {
				turnA = msg.TurnID
				h.write(t, map[string]any{"seat": msg.Seat, "type": "text", "text": "working on A", "turn_id": turnA})
				return
			}
			turnB := msg.TurnID
			go func() {
				time.Sleep(600 * time.Millisecond)
				h.write(t, map[string]any{"seat": msg.Seat, "type": "text", "text": "answer B", "turn_id": turnB})
				h.write(t, map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": turnB})
			}()
		case omp.OpAbort:
			h.write(t, map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": turnA, "stop_reason": "aborted"})
		}
	})
	a, err := StartDirectContext(t.Context(), Config{
		Name: "hunt", Provider: ProviderClaude, Model: "fixture-model",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	sawA := make(chan struct{}, 1)
	a.SubscribeEvents(func(ev Event) {
		if ev.Type == "assistant" && ev.Text == "working on A" {
			select {
			case sawA <- struct{}{}:
			default:
			}
		}
	})
	if err := a.Send("question A"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sawA:
	case <-time.After(10 * time.Second):
		t.Fatal("turn A never started")
	}
	if err := a.Interrupt(); err != nil {
		t.Fatal(err)
	}
	if err := a.Send("question B"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got, err := a.WaitForResponse(ctx)
	if err != nil || got != "answer B" {
		next, nextErr := a.WaitForResponse(ctx)
		t.Fatalf("WaitForResponse for B = %q, %v; want %q. The next wait (no new Send) returned %q, %v",
			got, err, "answer B", next, nextErr)
	}
}
```

**Evidence.**

- `zz_hunt_interrupt_send_test.go:75: WaitForResponse after Interrupt+Send(B) = "", <nil>; want "answer B" (the aborted turn A's end was taken as B's answer)`
- `zz_hunt_escalate_test.go:80: wait for escalated message = "", <nil>; want "answer B". The next wait returned "answer B", <nil>`
- The third test answers the abort immediately, as server.ts does; it is timing-dependent and failed 4 of 20 runs in the reader's measurement (pueue 212). It was not rerun for this report.

**Smallest fix.** Record `OpenTurnID()` when a turn is armed and when a wait subscribes, and ignore a terminal event whose `Reason` is aborted and whose turn id is that recorded turn. Also make the sidecar interrupt op wait, with a bound, for its `turn_end`/`aborted` echo before returning.

### C4. Dropping a busy sidecar seat never runs its queued prompt, and drop followed by reload never runs a prompt twice or corrupts the session store

**Severity: Critical.** Three worst outcomes at once: a queued turn runs on a seat the host can no longer abort, the same prompt runs twice, and the store then refuses to load after a restart.

**Mechanism.** The `drop` handler calls `agent.abort()`, not `stopTurn()`, so the queue survives; `abortTurn`'s interrupt-deliver continuation then runs the queued prompt on an agent already deleted from `seats` (abort answers `seat is not loaded`). `replaceStaleOMPSeat` sends drop and load back to back; the new SeatAgent restores while the old one is still unwinding, both append to one JSONL file, and the second `started` after `completed` makes the request journal throw on the next load.

**Where.** sidecar/server.ts:471-477 (drop handler); sidecar/seat.ts:852-893 (`abortTurn`); omp_agent.go `replaceStaleOMPSeat`

**Reproduction.**

`sidecar/zz-hunt-drop-queue.test.ts`, whole file:

```ts
// Defect hunt 2026-11-20 (fork sidecar): a dropped seat must not go on to run
// prompts that were queued behind its open turn.
import { afterEach, expect, setDefaultTimeout, test } from "bun:test";
import { join } from "node:path";
import { FIXTURE_CLEANUP_MS, launchAbortFixture } from "./testdata/abort-fixture";

setDefaultTimeout(90_000);
const fixtures: ReturnType<typeof launchAbortFixture>[] = [];
afterEach(async () => {
  for (const f of fixtures.splice(0)) await f.close();
}, FIXTURE_CLEANUP_MS * 2);

test("drop of a busy seat does not start its queued prompt", async () => {
  const f = launchAbortFixture({ entrypoint: join(import.meta.dir, "testdata/abort-provider.ts") });
  fixtures.push(f);
  const host = await f.ready;
  const seat = "drop-queued";
  const session = "s1";
  host.send({ op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: f.dir, session_id: session });
  expect((await host.next("ready", (e) => e.type === "ready" || e.type === "error")).type).toBe("ready");
  const prompt = (request: string, turn: string, text: string) =>
    ({ op: "prompt", seat, session_id: session, request_id: request, rpc_id: `rpc-${request}`, attempt_id: `att-${request}`, turn_id: turn, text });
  host.send(prompt("r1", "T1", "HOLD the first turn open"));
  await host.next("r1 started", (e) => e.type === "receipt" && e.request_id === "r1" && e.receipt?.state === "started");
  host.send(prompt("r2", "T2", "queued behind the held turn"));
  await host.next("r2 accepted", (e) => e.type === "accepted" && e.request_id === "r2");

  // The host releases the seat (Ops.discard / replaceStaleOMPSeat send this).
  host.send({ op: "drop", seat, session_id: session });
  await host.next("dropped", (e) => e.type === "dropped" && e.seat === seat);
  const droppedAt = host.events.findIndex((e) => e.type === "dropped");
  await Bun.sleep(3000);

  const after = host.events.slice(droppedAt + 1).filter((e) => e.seat === seat && e.type !== "turn");
  // Nothing may run on a seat the host no longer holds: no model reply, no
  // new turn, no started receipt.
  expect(after.map((e) => [e.type, e.turn_id, e.request_id, e.text ?? e.receipt?.state])).toEqual([]);

  // And the host can no longer reach it to stop it.
  host.send({ op: "abort", seat });
  const refused = await host.next("abort refused", (e) => e.type === "seat_error" && e.seat === seat);
  expect(refused.text).toBe("seat is not loaded");
});

// replaceStaleOMPSeat (omp_agent.go) unloads a seat with drop and loads it
// again at once. The new seat must be the only writer of the session store.
test("drop and reload leaves one writer on the session store", async () => {
  const f = launchAbortFixture({ entrypoint: join(import.meta.dir, "testdata/abort-provider.ts") });
  fixtures.push(f);
  const host = await f.ready;
  const seat = "drop-reload";
  const session = "s1";
  const load = { op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: f.dir, session_id: session };
  host.send(load);
  expect((await host.next("ready", (e) => e.type === "ready" || e.type === "error")).type).toBe("ready");
  const prompt = (request: string, turn: string, text: string) =>
    ({ op: "prompt", seat, session_id: session, request_id: request, rpc_id: `rpc-${request}`, attempt_id: `att-${request}`, turn_id: turn, text });
  host.send(prompt("r1", "T1", "HOLD the first turn open"));
  await host.next("r1 started", (e) => e.type === "receipt" && e.request_id === "r1" && e.receipt?.state === "started");
  host.send(prompt("r2", "T2", "queued behind the held turn"));
  await host.next("r2 accepted", (e) => e.type === "accepted" && e.request_id === "r2");
  host.send({ op: "drop", seat }, load);
  await host.next("reloaded", (e) => e.type === "ready" && e.how === "launched" && host.events.some((d) => d.type === "dropped"));
  await Bun.sleep(3000);
  // The live seat's conversation, from the seat itself.
  host.send({ op: "abort", seat });
  const live = await host.next("snapshot", (e) => e.type === "aborted" && e.seat === seat);
  const liveUsers = (live.snapshot.messages as { role: string; content: unknown }[])
    .filter((m) => m.role === "user").map((m) => JSON.stringify(m.content));
  const { readFileSync } = await import("node:fs");
  const disk = readFileSync(join(f.dir, "omp-seats", seat, `${session}.jsonl`), "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l));
  const diskUsers = disk.filter((e) => e.type === "message" && e.message.role === "user").map((e) => JSON.stringify(e.message.content));
  expect(diskUsers).toEqual(liveUsers);
});

// The queued prompt is an accepted obligation in the store when the reload
// reads it, so the new seat queues it too: it must still run only once, and
// the store the two seats shared must still load after a restart.
test("drop and reload runs a queued prompt once", async () => {
  const { existsSync, readFileSync, mkdirSync, writeFileSync } = await import("node:fs");
  const f = launchAbortFixture({ entrypoint: join(import.meta.dir, "testdata/abort-provider.ts") });
  fixtures.push(f);
  const host = await f.ready;
  const seat = "drop-twice";
  const session = "s1";
  const load = { op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: f.dir, session_id: session };
  host.send(load);
  expect((await host.next("ready", (e) => e.type === "ready" || e.type === "error")).type).toBe("ready");
  const prompt = (request: string, turn: string, text: string) =>
    ({ op: "prompt", seat, session_id: session, request_id: request, rpc_id: `rpc-${request}`, attempt_id: `att-${request}`, turn_id: turn, text });
  const marker = join(f.dir, "bash.pid");
  host.send(prompt("r1", "T1", `BASH_CANCEL ${marker}`)); // the turn stays open in Bash
  while (!existsSync(marker)) await Bun.sleep(50);
  host.send(prompt("r2", "T2", "Reply with exactly: ONCE-ONLY"));
  await host.next("r2 accepted", (e) => e.type === "accepted" && e.request_id === "r2");
  host.send({ op: "drop", seat }, load);
  await host.next("reloaded", (e) => e.type === "ready" && e.how === "launched" && host.events.some((d) => d.type === "dropped"));
  await Bun.sleep(3000);
  host.send(prompt("r3", "T3", "Reply with exactly: AFTER-RELOAD"));
  await host.next("r3 answered", (e) => e.type === "text" && String(e.text).includes("AFTER-RELOAD"), 20_000).catch(() => undefined);
  await Bun.sleep(1000);
  const answers = host.events.filter((e) => e.type === "text" && String(e.text).includes("ONCE-ONLY"))
    .map((e) => [e.turn_id, e.request_id, e.text]);
  const store = readFileSync(join(f.dir, "omp-seats", seat, `${session}.jsonl`), "utf8");

  // Restart: a fresh sidecar loads the same store.
  const g = launchAbortFixture({ entrypoint: join(import.meta.dir, "testdata/abort-provider.ts") });
  fixtures.push(g);
  mkdirSync(join(g.dir, "omp-seats", seat), { recursive: true });
  writeFileSync(join(g.dir, "omp-seats", seat, `${session}.jsonl`), store);
  const h2 = await g.ready;
  h2.send({ ...load, cwd: g.dir });
  const reload = await h2.next("restart load", (e) => e.type === "ready" || e.type === "error");
  expect({ answers, restartLoad: reload.type === "ready" ? "ready" : `error: ${reload.text}` })
    .toEqual({ answers: [["T2", "r2", "ONCE-ONLY"]], restartLoad: "ready" });
});
```

**Evidence.**

- `drop of a busy seat: after `dropped` the seat emitted ["accepted","T2"], ["receipt","T2","r2","started"], ["absorbed","T2","r2",...], ["text","T2","r2","reply: queued behind the held turn"], ["turn_end","T2","r2"] (expected none)`
- `drop and reload: the on-disk store held a user message the live seat does not have (`"queued behind the held turn"`)`
- `-   "restartLoad": "ready",  +   "restartLoad": "error: request journal: corrupt request transition",`

**Smallest fix.** In the drop handler call `loaded.agent.stopTurn()` instead of `abort()`, and await the old turn's unwinding (a SeatAgent `close()` that awaits `turnDone`/idle and then makes `runTurn` and `History.persist` refuse) before deleting the seat and writing `dropped`.

### C5. A prompt refused during compaction never stays durably accepted

**Severity: Critical.** The host is told the turn failed, yet the prompt runs after the next sidecar restart; a same-id retry is acknowledged and then never runs.

**Mechanism.** `prompt()` persists the receipt with `receipts.accept` before `refuseDuringBoundCompact()` throws `turn_in_flight`; the message is not queued in memory but the durable record says `accepted`. A retry takes the `!accepted.fresh` branch, emits `accepted` and returns. After a restart `receipts.pending()` replays the refused text. `steer()` has the right order. `ompControl.switchModel` does not hold prompts, so a switch-time fold reaches this from a normal host.

**Where.** sidecar/seat.ts:905 (accept) versus seat.ts:937 (refuse)

**Reproduction.**

`sidecar/zz-hunt-refused-prompt.test.ts`, whole file:

```ts
// Defect hunt 2026-11-20 (fork sidecar): a prompt refused while the seat is
// compacting must not leave a durable "accepted" obligation behind.
import { afterEach, describe, expect, setDefaultTimeout, test } from "bun:test";
import { mkdirSync, writeFileSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { FIXTURE_CLEANUP_MS, Host, launchAbortFixture, type Ev } from "./testdata/abort-fixture";

setDefaultTimeout(90_000);
const chunkText = "the quick brown fox jumps over the lazy dog. ".repeat(1200);
const seat = "refused-prompt";
const session = "s-heavy";

function heavyStoreLines(): string[] {
  const lines = [JSON.stringify({ type: "claudia_seat", v: 1, seat, session })];
  for (let i = 0; i < 16; i++) {
    lines.push(JSON.stringify({ type: "message", id: `u${i}`, parentId: i === 0 ? null : `a${i - 1}`,
      timestamp: new Date(Date.UTC(2026, 9, 31, 0, 0, i)).toISOString(),
      message: { role: "user", content: `${i}: ${chunkText}`, timestamp: i * 2 } }));
    lines.push(JSON.stringify({ type: "message", id: `a${i}`, parentId: `u${i}`,
      timestamp: new Date(Date.UTC(2026, 9, 31, 0, 0, i, 500)).toISOString(),
      message: { role: "assistant", content: [{ type: "text", text: "ok" }], api: "anthropic-messages",
        provider: "anthropic", model: "claude-sonnet-5", timestamp: i * 2 + 1,
        usage: { input: 12_000, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 12_001,
          cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } }, stopReason: "stop" } }));
  }
  return lines;
}

function storePath(root: string) { return join(root, "omp-seats", seat, `${session}.jsonl`); }
function writeStore(root: string, body: string) {
  mkdirSync(join(root, "omp-seats", seat), { recursive: true });
  writeFileSync(storePath(root), body);
}

function prompt(request: string, turn: string, text: string) {
  return { op: "prompt", seat, session_id: session, request_id: request, rpc_id: `rpc-${request}-${turn}`, attempt_id: `att-${request}`, turn_id: turn, text };
}

const fixtures: ReturnType<typeof launchAbortFixture>[] = [];
function launch() {
  const f = launchAbortFixture({
    entrypoint: join(import.meta.dir, "testdata/t244-interleave-provider.ts"),
    readyMarker: "t244-interleave-fixture-ready",
  });
  fixtures.push(f);
  return f;
}
afterEach(async () => {
  for (const f of fixtures.splice(0)) await f.close();
}, FIXTURE_CLEANUP_MS * 4);

async function load(host: Host) {
  host.send({ op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: process.env.TMPDIR ?? "/tmp", session_id: session });
  const ready = await host.next("ready", (e) => e.type === "ready" || e.type === "error");
  expect(ready.type).toBe("ready");
}

// Refuse a prompt (request r-slip) while a bound compaction is held, then
// cancel the compaction so the seat is idle again.
async function refuseDuringCompaction(host: Host) {
  host.send({ op: "compact", seat, session_id: session, request_id: "c-hold", bound_tokens: 180_000 });
  await host.next("compacting", (e: Ev) => e.type === "compacting");
  host.send(prompt("r-slip", "T1", "refused owner message"));
  const err = await host.next("prompt refused", (e) => e.type === "error" && e.request_id === "r-slip");
  expect(err.reason).toBe("turn_in_flight");
  host.send({ op: "compact_abort", seat, request_id: "c-hold" });
  await host.next("compact settled", (e) => e.type === "error" && e.request_id === "c-hold");
}

describe("prompt refused during compaction (defect hunt)", () => {
  test("the refusal leaves no accepted receipt behind", async () => {
    const f = launch();
    writeStore(f.dir, heavyStoreLines().join("\n") + "\n");
    const host = await f.ready;
    await load(host);
    await refuseDuringCompaction(host);
    host.send({ op: "receipt", seat, session_id: session, request_id: "r-slip", rpc_id: "q-slip" });
    const r = await host.next("receipt", (e) => e.type === "receipt" && e.rpc_id === "q-slip");
    // The host was told the prompt failed; the journal must not say it is
    // still an obligation waiting to run.
    expect(r.receipt.state).not.toBe("accepted");
  });

  test("an ack retry of the refused request runs it", async () => {
    const f = launch();
    writeStore(f.dir, heavyStoreLines().join("\n") + "\n");
    const host = await f.ready;
    await load(host);
    await refuseDuringCompaction(host);
    // Same request id (the delivery retry the receipt journal documents).
    host.send(prompt("r-slip", "T2", "refused owner message"));
    const accepted = await host.next("accepted", (e) => e.type === "accepted" && e.request_id === "r-slip");
    expect(accepted.receipt.state).toBe("accepted");
    const ended = await host.next("turn end", (e) => e.type === "turn_end" || (e.type === "error" && e.request_id !== "c-hold" && e.request_id !== "r-slip"), 10_000)
      .catch((err) => ({ type: "timeout", text: String(err) } as Ev));
    expect(ended.type).toBe("turn_end");
  });

  test("a sidecar restart does not run the refused prompt", async () => {
    const f1 = launch();
    writeStore(f1.dir, heavyStoreLines().join("\n") + "\n");
    const h1 = await f1.ready;
    await load(h1);
    await refuseDuringCompaction(h1);
    const durable = readFileSync(storePath(f1.dir), "utf8");
    await f1.close();
    fixtures.splice(fixtures.indexOf(f1), 1);

    const f2 = launch();
    writeStore(f2.dir, durable);
    const h2 = await f2.ready;
    await load(h2);
    h2.send(prompt("r-new", "T3", "a fresh owner message"));
    await h2.next("turn end", (e) => e.type === "turn_end" && e.turn_id === "T3", 15_000).catch(() => undefined);
    // Anything the model was asked after the restart.
    const absorbed = h2.events.filter((e) => (e.type === "absorbed" || e.type === "text") && String(e.text).includes("refused owner"));
    expect(absorbed.map((e) => [e.type, e.text, e.request_id])).toEqual([]);
  });

  // The same refusal reached through the path the Go host does not guard:
  // ompControl.switchModel sends a load with a request id and does not hold
  // prompts (blockIfCompactBusy covers only bound compact). A switch to a
  // smaller window folds first; a prompt that lands during the fold is
  // refused but stays durably accepted.
  test("a prompt refused during a switch-time fold leaves no accepted receipt", async () => {
    const f = launch();
    writeStore(f.dir, heavyStoreLines().join("\n") + "\n");
    const host = await f.ready;
    await load(host);
    host.send({ op: "load", seat, provider: "anthropic", model: "claude-haiku-4-5", token: "fixture", cwd: f.dir, session_id: session, request_id: "switch-1" });
    await host.next("compacting", (e: Ev) => e.type === "compacting");
    host.send(prompt("r-slip", "T1", "refused owner message"));
    const err = await host.next("prompt refused", (e) => e.type === "error" && e.request_id === "r-slip");
    expect(err.reason).toBe("turn_in_flight");
    host.send({ op: "abort", seat });
    const sw = await host.next("switch settled", (e) => (e.type === "error" || e.type === "ready") && e.request_id === "switch-1");
    expect(sw.type).toBe("error");
    host.send({ op: "receipt", seat, session_id: session, request_id: "r-slip", rpc_id: "q-slip" });
    const r = await host.next("receipt", (e) => e.type === "receipt" && e.rpc_id === "q-slip");
    expect(r.receipt.state).not.toBe("accepted");
  });
});
```

**Evidence.**

- `the refusal leaves no accepted receipt behind: Expected: not "accepted"`
- `an ack retry of the refused request runs it: Expected: "turn_end" Received: "timeout"`
- `a sidecar restart does not run the refused prompt: received ["absorbed", "refused owner message", "r-slip"], ["text", "reply: refused owner message", "r-slip"]`
- `a prompt refused during a switch-time fold leaves no accepted receipt: Expected: not "accepted"`

**Smallest fix.** Call `refuseDuringBoundCompact()` before `receipts.accept` in `prompt()`, as `steer()` does.

### C6. A steer that reaches an idle seat is run or refused, never acknowledged and parked

**Severity: Critical.** An owner message is acknowledged and never answered. After any reattach the host believes an idle seat is mid-turn, so every steer-mode delivery to it parks.

**Mechanism.** `steer()` only enqueues the message and emits `accepted`; nothing starts a turn on an idle seat. On the Go side any reattach ready (`how` adopted or switched) sets in-flight true even when the seat is idle (the T202 bias, omp_agent.go:504-506), so `TurnPhase()` reads in-turn and `SendMode(DeliverySteer)` takes the steer path; the host also ignores the sidecar's `aborted` (no turn open) reply, so nothing clears the bias. A steer sent just after `turn_end`, before the pump reads it, hits the same path without a reattach.

**Where.** sidecar/seat.ts:940-954; omp_agent.go:504-506 and 944-947; delivery.go:150-161

**Reproduction.**

`sidecar/zz-hunt-idle-steer.test.ts`, whole file:

```ts
// Defect hunt 2026-11-20 (fork sidecar): a steer that reaches a seat whose turn
// has just ended (the host chose steer while it still saw the turn open) must
// either run or be refused, never be acknowledged and parked.
import { afterEach, expect, setDefaultTimeout, test } from "bun:test";
import { join } from "node:path";
import { FIXTURE_CLEANUP_MS, launchAbortFixture } from "./testdata/abort-fixture";

setDefaultTimeout(90_000);
const fixtures: ReturnType<typeof launchAbortFixture>[] = [];
afterEach(async () => {
  for (const f of fixtures.splice(0)) await f.close();
}, FIXTURE_CLEANUP_MS * 2);

test("a steer on an idle seat is answered or refused", async () => {
  const f = launchAbortFixture({ entrypoint: join(import.meta.dir, "testdata/abort-provider.ts") });
  fixtures.push(f);
  const host = await f.ready;
  const seat = "idle-steer";
  const session = "s1";
  host.send({ op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: f.dir, session_id: session });
  expect((await host.next("ready", (e) => e.type === "ready" || e.type === "error")).type).toBe("ready");
  host.send({ op: "prompt", seat, session_id: session, request_id: "r1", rpc_id: "rpc-r1", attempt_id: "a1", turn_id: "T1", text: "Reply with exactly: FIRST" });
  await host.next("T1 end", (e) => e.type === "turn_end" && e.turn_id === "T1", 20_000);

  // The owner's next message, sent as a steer because the host had not yet
  // read T1's end.
  host.send({ op: "steer", seat, session_id: session, request_id: "r2", rpc_id: "rpc-r2", attempt_id: "a2", turn_id: "T2", text: "Reply with exactly: SECOND" });
  const ack = await host.next("steer ack", (e) => (e.type === "accepted" || e.type === "error") && e.request_id === "r2");
  const outcome = ack.type === "error"
    ? "refused"
    : await host.next("steer answered", (e) => e.type === "text" && String(e.text).includes("SECOND"), 10_000).then(() => "answered", () => "parked");
  host.send({ op: "receipt", seat, session_id: session, request_id: "r2", rpc_id: "q-r2" });
  const receipt = await host.next("receipt", (e) => e.type === "receipt" && e.rpc_id === "q-r2");
  expect({ ack: ack.type, ackTurn: ack.turn_id ?? null, outcome, receipt: receipt.receipt.state })
    .toEqual({ ack: "accepted", ackTurn: null, outcome: expect.stringMatching(/answered|refused/), receipt: expect.anything() });
});
```

`zz_hunt_real_sidecar_test.go` (package and helpers in the appendix):

```go
// TestHuntRealSidecarSteerAfterReattachIsAnswered runs the unchanged
// production sidecar (mock provider, startAbortSidecar). A seat answers a
// turn and goes idle; its handle is stopped (the sidecar keeps the seat) and
// the same name and session is started again, which reattaches it. An owner
// message delivered with DeliverySteer must be answered.
func TestHuntRealSidecarSteerAfterReattachIsAnswered(t *testing.T) {
	ctx, dir := startAbortSidecar(t)
	cfg := Config{Name: "hunt-steer", Provider: Provider(omp.Anthropic), Model: "claude-sonnet-5",
		WorkDir: dir, TermLogPath: "-", SessionID: uuid.NewString()}
	first, err := StartDirect(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Send("Reply with exactly: one"); err != nil {
		t.Fatal(err)
	}
	if got, err := first.WaitForResponse(ctx); err != nil || !strings.Contains(got, "one") {
		t.Fatalf("first turn: %q %v", got, err)
	}
	first.Stop()

	second, err := StartDirect(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Stop()
	finals := make(chan Event, 4)
	second.SubscribeEvents(func(ev Event) {
		if ev.Type == "assistant" && ev.Final {
			finals <- ev
		}
	})
	phase := second.TurnPhase()
	out, err := second.SendMode("Reply with exactly: two", DeliverySteer)
	if err != nil {
		t.Fatalf("SendMode: %v", err)
	}
	select {
	case ev := <-finals:
		if !strings.Contains(ev.Text, "two") {
			t.Fatalf("answer = %q", ev.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("idle reattached seat (phase %s, PromptInFlight %v) took the owner message as a steer "+
			"(outcome mechanism %q) and never answered it", phase, second.PromptInFlight(), out.Mechanism)
	}
}
```

`zz_hunt_steer_idle_test.go` (package and helpers in the appendix):

```go
// TestHuntSteerToIdleReattachedSeatIsAnswered: a seat the sidecar already
// holds idle is reattached (a Launch of a name whose seat is still loaded,
// which the sidecar answers "ready how=adopted" with no turn_id because
// seat.ts openTurnID() is undefined when idle). The host then delivers an
// owner message with DeliverySteer. The message must be answered.
//
// The scripted sidecar answers exactly as server.ts/seat.ts do:
//   - load of a loaded seat: ready, how "adopted", turn_id omitted when idle
//     (server.ts handle(), load branch; seat.ts openTurnID).
//   - steer with no turn open: enqueue, emit {"type":"accepted"} and start
//     nothing (seat.ts steer: enqueue(message); sink.emit accepted).
//   - prompt on an idle seat: accepted, text, turn_end under the prompt's turn.
func TestHuntSteerToIdleReattachedSeatIsAnswered(t *testing.T) {
	var mu sync.Mutex
	var queued []string
	startHuntSidecar(t, func(h *huntConn, msg omp.Message) {
		switch msg.Op {
		case omp.OpLoad, omp.OpAdopt:
			h.write(map[string]any{"seat": msg.Seat, "type": "ready", "how": "adopted",
				"provider": msg.Provider, "model": msg.Model, "bound_tools": []string{}})
		case omp.OpSteer:
			mu.Lock()
			queued = append(queued, msg.Text)
			mu.Unlock()
			h.write(map[string]any{"seat": msg.Seat, "type": "accepted"})
		case omp.OpPrompt:
			h.write(map[string]any{"seat": msg.Seat, "type": "accepted", "turn_id": msg.TurnID})
			h.write(map[string]any{"seat": msg.Seat, "type": "text", "turn_id": msg.TurnID, "text": "answer"})
			h.write(map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": msg.TurnID})
		}
	})
	a, err := StartDirect(Config{Name: "seat", Provider: ProviderClaude, Model: "fixture-model",
		WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()

	answered := make(chan Event, 1)
	a.SubscribeEvents(func(ev Event) {
		if ev.IsTerminalStop() {
			select {
			case answered <- ev:
			default:
			}
		}
	})
	phase := a.TurnPhase()
	out, err := a.SendMode("owner message", DeliverySteer)
	if err != nil {
		t.Fatalf("SendMode: %v", err)
	}
	select {
	case <-answered:
	case <-time.After(3 * time.Second):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("owner message never answered: idle seat reattached reads phase=%s, delivery outcome mechanism %q, "+
			"sidecar holds it in its steering queue %q with no turn to take it; PromptInFlight=%v",
			phase, out.Mechanism, queued, a.PromptInFlight())
	}
}
```

**Evidence.**

- `bun: -   "outcome": StringMatching /answered|refused/,  +   "outcome": "parked",  +   "receipt": "accepted",`
- `zz_hunt_real_sidecar_test.go:59: idle reattached seat (phase in_turn, PromptInFlight true) took the owner message as a steer (outcome mechanism "") and never answered it`
- `zz_hunt_steer_idle_test.go:68: owner message never answered: idle seat reattached reads phase=in_turn, delivery outcome mechanism "", sidecar holds it in its steering queue ["owner message"] with no turn to take it; PromptInFlight=true`

**Smallest fix.** In `steer()`, when no turn is open and nothing is streaming, start a turn as `prompt`'s queued branch does (`runTurn("", meta, () => agent.continue())`). On the Go side, clear in-flight on the sidecar's `aborted` reply. Separately, a sidecar steer returns an empty `DeliveryOutcome` (omp_agent.go:558-564), so `Mechanism` is "".

### C7. A sidecar that exits on lifeline EOF leaves none of its tool processes running

**Severity: Critical.** A Bash command, build or server keeps running with nobody attached, and its timeout died with the sidecar.

**Mechanism.** `runCommand` spawns every tool child `detached: true`, in its own process group. The `--lifeline=stdin` handler only finishes receipts and calls `process.exit(0)`; no process group is killed.

**Where.** sidecar/server.ts:84-92; sidecar/process.ts:72

**Reproduction.**

`sidecar/zz-hunt-lifeline-orphan.test.ts`, whole file:

```ts
// Defect hunt 2026-11-20 (fork sidecar): when the sidecar exits because its
// lifeline closed (🎯T166), the shell commands its seats were running must not
// keep running with nobody attached.
import { afterEach, expect, setDefaultTimeout, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { FIXTURE_CLEANUP_MS, launchAbortFixture } from "./testdata/abort-fixture";

setDefaultTimeout(90_000);
const fixtures: ReturnType<typeof launchAbortFixture>[] = [];
const leftovers: number[] = [];
afterEach(async () => {
  for (const pid of leftovers.splice(0)) { try { process.kill(pid, "SIGKILL"); } catch { /* gone */ } }
  for (const f of fixtures.splice(0)) await f.close();
}, FIXTURE_CLEANUP_MS * 2);

function alive(pid: number): boolean {
  try { process.kill(pid, 0); return true; } catch { return false; }
}

test("a lifeline exit does not orphan a running Bash command", async () => {
  const f = launchAbortFixture({ entrypoint: join(import.meta.dir, "testdata/abort-provider.ts"), args: ["--lifeline=stdin"] });
  fixtures.push(f);
  const host = await f.ready;
  const seat = "lifeline";
  host.send({ op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: f.dir, session_id: "s1" });
  expect((await host.next("ready", (e) => e.type === "ready" || e.type === "error")).type).toBe("ready");
  const marker = join(f.dir, "sleep.pid");
  // The fixture model answers this with Bash: `sleep 60 & echo $! > marker; wait`.
  host.send({ op: "prompt", seat, session_id: "s1", turn_id: "T1", text: `BASH_CANCEL ${marker}` });
  const deadline = Date.now() + 30_000;
  while (!existsSync(marker) || !readFileSync(marker, "utf8").trim()) {
    if (Date.now() > deadline) throw new Error("Bash never started");
    await Bun.sleep(50);
  }
  const sleepPid = Number(readFileSync(marker, "utf8").trim());
  leftovers.push(sleepPid);
  expect(alive(sleepPid)).toBe(true);

  // The process that started the sidecar goes away: its lifeline closes.
  f.child.stdin.end();
  const status = await f.exited;
  expect(status).toBe("exit=0 signal=none");
  await Bun.sleep(500);
  // The sidecar is gone; its seat's command must be gone too.
  expect({ sidecar: status, sleepAlive: alive(sleepPid) }).toEqual({ sidecar: "exit=0 signal=none", sleepAlive: false });
});
```

**Evidence.**

- `-   "sleepAlive": false,  +   "sleepAlive": true,`

**Smallest fix.** Track spawned group leaders in `runCommand` (add on spawn, remove on close), export a `killAllGroups()` that sends SIGKILL to each `-pid`, and call it from the lifeline handler and the SIGTERM/SIGINT handlers before exiting.

### C8. Stopping a sidecar seat through the Registry leaves no turn or tool process running

**Severity: Critical.** The provider turn and its Bash children keep running after `Stop` returns, with nobody attached; the control-plane stop intent then publishes `stopped`.

**Mechanism.** `Agent.Stop` reaches `shutdownOMPSeat`, which only cancels the seat context and closes the connection. On close the sidecar fails pending host tool calls and nothing else (server.ts:183-189 says so on purpose). The daemon's Release Stop compensates with `Quiesce` (daemon.go:1420-1424), but `Registry.Stop` / `stopHeld`, used by controlplane reconcile (stop intent and generation change), the fleet and `StopAll`, does not.

**Where.** omp_agent.go:589-591 and 1914-1929; registry.go:805; sidecar/server.ts:183-189

**Reproduction.**

`zz_hunt_real_sidecar_test.go` (package and helpers in the appendix):

```go
// TestHuntRealSidecarStopEndsTheSeatsTools: Agent.Stop (which Registry.Stop
// calls for every stop that is not a removal, registry.go stopHeld) on a
// seat whose turn is running a Bash tool. Once Stop has returned nobody is
// attached to the seat; the tool's process must not keep running.
func TestHuntRealSidecarStopEndsTheSeatsTools(t *testing.T) {
	ctx, dir := startAbortSidecar(t)
	a, err := StartDirect(Config{Name: "hunt-stop", Provider: Provider(omp.Anthropic), Model: "claude-sonnet-5",
		WorkDir: dir, TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "child.pid")
	if err := a.Send("BASH_CANCEL " + marker); err != nil {
		t.Fatal(err)
	}
	var pid int
	waitFor(t, "Bash tool child running", func() bool {
		raw, err := os.ReadFile(marker)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && pid > 1
	})
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	a.Stop()
	time.Sleep(3 * time.Second)
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("Stop returned 3s ago and nothing is attached to the seat, but its Bash tool (pid %d) is still running", pid)
	}
}
```

**Evidence.**

- `zz_hunt_real_sidecar_test.go:95: Stop returned 3s ago and nothing is attached to the seat, but its Bash tool (pid 29329) is still running`

**Smallest fix.** Have the sidecar seat's stop op send `OpAbort{StopTurn: true}` and wait for the settle, as the daemon's Release Stop does, before closing; a Remove after Stop must still send `OpDrop`.

### C9. A cancelled brokered StartContext never leaves a seat running with no owner

**Severity: Critical.** A backend seat keeps running with nobody attached; it is AutoStart and stamped held, so every later daemon boot resumes it and nudges it.

**Mechanism.** `brokerClient.call` returns `ctx.Err()` without telling the daemon, and the library closes the connection. The daemon handles the grant synchronously on that connection's goroutine, finishes the launch, binds the seat to the dead connection and ignores the failed reply write; `ConnClosed` then treats it as an ordinary detach and keeps the seat.

**Where.** broker_client.go:262-267; broker_agent.go:147-155; daemon/daemon.go:964-1047 and 662

**Reproduction.**

`daemon/zz_hunt_cancel_test.go` (package and helpers in the appendix):

```go
// TestHuntCancelledStartLeavesNoSeatRunning: StartContext documents a
// cancelable startup. Through the daemon, a start the caller gave up on
// still launches the provider, binds it to the (now closed) connection, and
// is then kept running unowned as if a consumer had detached it. Nobody
// holds the handle, and a non-plumbing seat has no TTL, so the process runs
// until an operator finds it.
func TestHuntCancelledStartLeavesNoSeatRunning(t *testing.T) {
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.startHook = func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
		close(entered)
		<-release // a slow provider launch
		return f.startSeat(ctx, cfg)
	}
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost = true, true
	f.bootWith(t, opts)

	const name = "abandoned"
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		a, err := claudia.StartContext(ctx, claudia.Config{Name: name, WorkDir: t.TempDir(),
			SessionID: "sid-abandoned", TermLogPath: "-"})
		if a != nil {
			a.Stop()
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("daemon never began launching")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("StartContext = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("StartContext did not return after cancel")
	}
	close(release)
	waitFor(t, "daemon to finish the launch and lose the owner", func() bool {
		f.d.mu.Lock()
		defer f.d.mu.Unlock()
		g := f.d.grants[name]
		return g != nil && g.proc != nil && g.owner == nil
	})
	time.Sleep(500 * time.Millisecond)
	if proc := f.d.reg.Get(name); proc != nil && proc.Alive() {
		_, _, stops := f.seat(0).counts()
		t.Fatalf("cancelled StartContext left seat %q running with no owner (provider stops=%d, ephemeral TTL n/a)", name, stops)
	}
}
```

**Evidence.**

- `zz_hunt_cancel_test.go:65: cancelled StartContext left seat "abandoned" running with no owner (provider stops=0, ephemeral TTL n/a)`

**Smallest fix.** In `handleGrant`, when the `Granted` reply write fails and the seat was not a reclaim, detach it and `d.reg.Remove(name)`.

### C10. One consumer that stops reading never stops the daemon from reporting other seats' deaths

**Severity: Critical.** Other consumers' handles keep reporting `Alive()` after their seats' processes have died.

**Mechanism.** The Registry runs one `watchSeats` goroutine for all seats. On `SeatGone` the daemon's `onSeatEvent` writes `agent_gone` to the dead seat's owner synchronously; if that owner has stopped reading, its pump already holds the connection's write lock inside a blocked `Write`, so `Reply` waits forever and no later seat death is reported to anyone. A broker handle's `Alive()` is cleared only by `agent_gone` or connection loss.

**Where.** daemon/daemon.go:2274-2276, called from registry_seats.go:209

**Reproduction.**

`daemon/zz_hunt_stall_test.go` (package and helpers in the appendix):

```go
// TestHuntStalledOwnerDoesNotBlindSeatWatch: the Registry's liveness watch
// is one goroutine for every seat, and the daemon writes agent_gone to the
// dead seat's owner from inside it. An owner whose socket is full (a paused
// or wedged consumer) must not stop every other owner from learning that
// its seat's process died: their handles would read Alive() forever.
func TestHuntStalledOwnerDoesNotBlindSeatWatch(t *testing.T) {
	f := newFixture(t)
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost = true, true
	f.bootWith(t, opts)

	stalled := rawSeat(t, f.sock, "a-stalled") // seat 0; this owner never reads again
	reader := rawSeat(t, f.sock, "b-reader")   // seat 1
	_ = stalled

	// The stalled owner's seat streams enough to fill its socket: its pump
	// goroutine is now blocked in Write holding that connection's lock.
	proc := f.d.reg.Get("a-stalled")
	pad := strings.Repeat("y", 8192)
	for i := 0; i < 400; i++ {
		proc.PublishEvent(claudia.Event{Type: "progress", ProgressType: "tool_use", ToolTitle: fmt.Sprintf("%d-%s", i, pad)})
	}
	time.Sleep(time.Second)

	// Both seats' processes die.
	f.seat(0).exit()
	f.seat(1).exit()
	got := make(chan error, 1)
	go func() {
		_, err := huntAwait(reader, "", broker.TypeAgentGone, 15*time.Second)
		got <- err
	}()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-got:
			if err != nil {
				t.Fatalf("reader read: %v", err)
			}
			return
		default:
		}
		f.clock.Advance(claudia.DefaultSeatWatchInterval)
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("b-reader's owner never told its seat died; seat watch stuck: %s",
		excerpt(huntStacks(), "onSeatEvent"))
}
```

**Evidence.**

- `zz_hunt_stall_test.go:148: b-reader's owner never told its seat died; seat watch stuck: (stack: onSeatEvent daemon.go:2275 -> (*Conn).writeLine transport.go:273, sync.Mutex.Lock)`

**Smallest fix.** Send `agent_gone` off the watch goroutine (a goroutine counted in `d.wg`, as `notifyDetached` does), or give the write a deadline.

### C11. Fleet status never reports an agent whose seat process has died as idle

**Severity: Critical.** This is the seat-healthy-while-dead outcome: a dead idle agent shows `idle` with its model, and timers and expectations stay queued against it until some later Send fails.

**Mechanism.** `StatusFor` derives the state only from whether `k.running[key]` has an entry and never asks `rt.seat.Alive()`. The run loop sleeps on `rt.wake.Wait()` while the inbox is empty with no liveness check, and fleet grants have no owner, so the daemon's SeatGone handler tells nobody in the kernel.

**Where.** fleet.go:1386-1394; fleet.go:871-873; daemon/daemon.go:2265-2277

**Reproduction.**

`zz_hunt_fleet_test.go` (package and helpers in the appendix):

```go
// An idle fleet agent whose seat process has died must not be reported as a
// live, idle agent.
func TestHuntFleetStatusNeverReportsADeadSeatAsIdle(t *testing.T) {
	seat := newFakeFleetSeat(fleetAnswer("done"))
	clock := NewManualClock(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
	r := &fleetRig{t: t, dir: t.TempDir(), seats: map[string]*fakeFleetSeat{"a": seat}, clock: clock}
	r.open()
	if err := r.k.Open("f", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.k.Close() })
	r.start("a", FleetOwner)
	r.send(FleetOwner, "a", "first job")
	var seq uint64
	if m := r.ownerMessage(&seq); m.Text != "done" {
		t.Fatalf("fixture: %+v", m)
	}

	seat.dead.Store(true) // the seat's process exits between turns
	clock.Advance(time.Hour)

	st, err := r.k.Status("f")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 1 {
		t.Fatalf("status %+v", st)
	}
	row := st.Agents[0]
	if row.State == FleetStateIdle || row.State == FleetStateWorking {
		t.Fatalf("seat process dead for an hour, status reports %q (model %q, seat starts so far %d): nothing will notice until a message is sent",
			row.State, row.Model, r.specCount())
	}
}
```

**Evidence.**

- `zz_hunt_fleet_test.go:41: seat process dead for an hour, status reports "idle" (model "fake/model", seat starts so far 1): nothing will notice until a message is sent`

**Smallest fix.** Report a non-live state in `StatusFor` when `rt != nil && !rt.seat.Alive()`, and have the idle wait poll `Alive()` on `k.clock`, treating a death there like a mid-turn death (drop `running`, then revive).

## High

### H1. Resolve honours an exclusion or preference named by the sidecar runtime id

**Severity: High.** A failover walk that excludes the seat's current provider by its runtime id (`anthropic`) gets the same provider back, so the seat loops on the exhausted plan. Critical whenever a host passes `AgentDef.Provider`, which carries runtime ids.

**Mechanism.** Seats carry runtime ids (`SubscriptionSeatProvider` gives `anthropic`, `xai-oauth`, `openai-codex`). `Resolve` matches `ExcludeProviders`, `PreferProvider` and `TieBreak` against catalog product ids without `PlanProvider()`, while `OwnerOverride`, `ResolveSeatPlacement` and `SetSeatPlanPolicy` do normalise. A brokered `TypeResolve` passes the caller's values through unchanged.

**Where.** resolve.go:146-148, 411-421, 438-449, 671-673, 762-772

**Reproduction.**

`zz_hunt_plan_test.go` (package and helpers in the appendix):

```go
// A seat's AgentDef.Provider is the sidecar runtime id after
// SubscriptionSeatProvider ("anthropic", "xai-oauth", "openai-codex").
// A host walking the ladder away from a seat's current provider names it the
// way the seat names it. Resolve must not hand the same provider back.
func TestHuntResolveExcludesProviderNamedByRuntimeID(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	// Claude is the natural leader: under-spent ("under") against Grok's "ok".
	usage := []PlanUsage{huntWeekly(ProviderClaude, 10, now), huntWeekly(ProviderGrok, 40, now)}
	pred := ModelPredicates{
		Mode: CapabilitySession, Quality: ModelQualityStandard, PreferPlan: true,
		Now: now, Usage: usage, Rand: func(int) int { return 0 },
	}

	pred.ExcludeProviders = []Provider{ProviderClaude, ProviderCodex, ProviderCursor}
	got, err := Resolve(context.Background(), pred)
	if err != nil || got.Provider != ProviderGrok {
		t.Fatalf("fixture: product-id exclusion should land on grok, got %+v (%v)", got, err)
	}

	runtimeID := SubscriptionSeatProvider(ProviderClaude) // "anthropic"
	pred.ExcludeProviders = []Provider{runtimeID, ProviderCodex, ProviderCursor}
	got, err = Resolve(context.Background(), pred)
	if err != nil {
		t.Fatal(err)
	}
	if PlanProvider(got.Provider) == PlanProvider(runtimeID) {
		t.Fatalf("Resolve excluded %q but picked %s/%s: the ladder walk lands back on the provider it is leaving",
			runtimeID, got.Provider, got.Model)
	}
}

func TestHuntResolvePrefersProviderNamedByRuntimeID(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	usage := []PlanUsage{huntWeekly(ProviderClaude, 10, now), huntWeekly(ProviderGrok, 40, now)}
	pred := ModelPredicates{
		Mode: CapabilitySession, Quality: ModelQualityStandard, PreferPlan: true,
		Now: now, Usage: usage, Rand: func(int) int { return 0 },
		ExcludeProviders: []Provider{ProviderCodex, ProviderCursor},
	}
	pred.PreferProvider = ProviderGrok
	got, err := Resolve(context.Background(), pred)
	if err != nil || got.Provider != ProviderGrok {
		t.Fatalf("fixture: product-id preference should land on grok, got %+v (%v)", got, err)
	}
	pred.PreferProvider = SubscriptionSeatProvider(ProviderGrok) // "xai-oauth"
	got, err = Resolve(context.Background(), pred)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != ProviderGrok {
		t.Fatalf("PreferProvider %q (a token-eligible dest) was ignored; picked %s", pred.PreferProvider, got.Provider)
	}
}
```

**Evidence.**

- `zz_hunt_plan_test.go:47: Resolve excluded "anthropic" but picked claude/claude-sonnet-5: the ladder walk lands back on the provider it is leaving`
- `zz_hunt_plan_test.go:71: PreferProvider "xai-oauth" (a token-eligible dest) was ignored; picked claude`

**Smallest fix.** In `normalizePredicates`, map `ExcludeProviders`, `TieBreak` and `PreferProvider` through `PlanProvider()`; that covers the catalog and intel paths.

### H2. An unreadable grants.json is never opened as an empty registry and then overwritten

**Severity: High.** One transient read error (permissions, EIO) followed by any registration erases every seat definition on disk.

**Mechanism.** `NewRegistry` acts only on `err == nil` from `os.ReadFile`; every other error, not just a missing file, yields an empty registry, and the next `save` renames a file with only the new entry over the real one. `loadSeatClaims` (daemon/claims.go:79-85) gets this right.

**Where.** registry.go:236-245

**Reproduction.**

`zz_lead_registry_load_test.go` (package and helpers in the appendix):

```go
func TestAuditRegistryRefusesUnreadableSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grants.json")
	old := AgentDef{Name: "existing", SessionID: "old-id", AutoStart: true}
	original, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := original.Register(old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); !errors.Is(err, os.ErrPermission) {
		t.Skipf("test requires unreadable file under a writable directory, read returned %v", err)
	}

	reopened, err := NewRegistry(path)
	if errors.Is(err, os.ErrPermission) {
		return
	}
	if err != nil {
		t.Fatalf("want an error wrapping permission denied, got %v", err)
	}
	if err := reopened.Register(AgentDef{Name: "later", SessionID: "new-id"}); err != nil {
		t.Fatalf("unreadable registry opened successfully, subsequent write returned %v", err)
	}
	latest, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("unreadable registry opened as empty, then accepted a save: existing=%v later=%v",
		latest.Def("existing") != nil, latest.Def("later") != nil)
}
```

**Evidence.**

- `zz_lead_registry_load_test.go:41: unreadable registry opened as empty, then accepted a save: existing=false later=true`

**Smallest fix.** `if err != nil && !errors.Is(err, os.ErrNotExist) { return nil, fmt.Errorf("read %s: %w", path, err) }`.

### H3. A seat released with stop stays released when the save fails and the daemon restarts

**Severity: High.** The consumer is told `released`, and the next boot resumes the seat unowned with nobody attached.

**Mechanism.** `stopHeld` deletes the definition from memory and then saves with no rollback; `handleRelease` only logs the error and replies `released`. grants.json still holds the seat with AutoStart, and `ResumeAll` brings it back. Same root cause as M2, worse outcome.

**Where.** registry.go:810-817; daemon/daemon.go:1486-1491

**Reproduction.**

`daemon/zz_hunt_grant_test.go` (package and helpers in the appendix):

```go
// Registry.stopHeld deletes the definition from memory and then saves; a
// failed save is returned, and handleRelease only logs it and answers
// released. grants.json still holds the seat with AutoStart, so the next
// boot resumes the seat the consumer stopped, unowned. The write failure is
// modelled by a read-only state directory (disk full behaves the same).
func TestHuntReleasedSeatStaysReleasedAfterRestart(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	opts := f.options(nil)
	opts.DisableMCPHost, opts.RestartNudge = true, "-"
	f.bootWith(t, opts)
	<-f.d.resumeDone
	c := rawSeat(t, f.sock, "gone")
	if err := os.Chmod(f.state, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.state, 0o700) })
	rel := rawCall(t, c, &broker.Request{ID: "release", Type: broker.TypeRelease,
		Release: &broker.ReleaseRequest{Name: "gone", Disposition: broker.DispositionStop}})
	if err := os.Chmod(f.state, 0o700); err != nil {
		t.Fatal(err)
	}
	if rel.Type != broker.TypeReleased {
		return // the release reported its failure: acceptable
	}
	if err := f.d.Close(); err != nil {
		t.Fatal(err)
	}
	f.bootWith(t, opts)
	<-f.d.resumeDone
	if proc := f.d.reg.Get("gone"); proc != nil {
		t.Fatalf("release answered %s, but the next boot resumed the seat (alive=%v); starts=%v",
			rel.Type, proc.Alive(), starts())
	}
}
```

**Evidence.**

- `zz_hunt_grant_test.go:155: release answered released, but the next boot resumed the seat (alive=true); starts=[gone gone]`

**Smallest fix.** Restore `r.agents[name]` in `stopHeld` when the save fails, as `recordSwitch` and `SetSeatPlanPolicy` already do, and reply `CodeReleaseFailed` from `handleRelease` when the removal errors.

### H4. A plain grant never rewrites a live seat's provider in grants.json

**Severity: High.** The stored definition is silently wrong, and the next broker boot resumes the seat on the stale provider: the exhausted-plan outcome when the live provider was a migration target.

**Mechanism.** `handleGrant` reconciles the provider only when `req.Grant.Adopt` is set. A non-adopt grant (`claudia.Start`, `Registry.Launch`) takes the else-branch, which merges only connect and materialized fields, and `Register(def)` persists the caller's provider while `LaunchContext` returns the live seat on the old one.

**Where.** daemon/daemon.go:917, 949, 959, 973; registry.go:531

**Reproduction.**

`daemon/zz_hunt_grant_test.go` (package and helpers in the appendix):

```go
// TestT238AdoptRefusesStaleProviderInsteadOfSuccessfulOldBackend pins the
// adopt path: a grant naming a provider other than the live seat's is
// refused and the definition is left alone. A plain (non-adopt) grant of
// the same live seat — what claudia.Start or Registry.Launch sends after a
// consumer restart — takes the else-branch of handleGrant's merge, which
// only reconciles connect fields, then Register overwrites the provider in
// grants.json while LaunchContext hands back the seat still running on the
// old one. The registry and the running process now disagree, and the
// next boot resumes the seat on the definition's provider.
func TestHuntPlainGrantKeepsLiveSeatProvider(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	first := rawSeat(t, f.sock, "moved") // live on the default (Claude) provider
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "old owner detached", func() bool {
		f.d.mu.Lock()
		defer f.d.mu.Unlock()
		g := f.d.grants["moved"]
		return g != nil && g.owner == nil
	})
	live := f.d.reg.Get("moved")
	if live == nil || !live.Alive() {
		t.Fatal("fixture: seat should still be running unowned")
	}
	liveProvider := live.Provider()

	contender, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Close()
	def, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: "moved", Provider: claudia.ProviderGrok, WorkDir: t.TempDir(), SessionID: "sid-moved", TermLogPath: "-",
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := rawCall(t, contender, &broker.Request{ID: "plain", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: "moved", Def: def}})
	if got.Type == broker.TypeError {
		return // refused like the adopt path: acceptable
	}
	reopened, err := claudia.NewRegistry(filepath.Join(f.state, grantsFile))
	if err != nil {
		t.Fatal(err)
	}
	persisted := reopened.Def("moved").Provider
	if f.seatCount() != 1 {
		t.Fatalf("plain grant started a second seat: count=%d", f.seatCount())
	}
	if persisted != liveProvider || f.d.reg.Def("moved").Provider != liveProvider {
		t.Fatalf("granted %q on the live %q seat, but grants.json now records provider %q (in memory %q)",
			got.Granted.Provider, liveProvider, persisted, f.d.reg.Def("moved").Provider)
	}
}

// The same plain grant, followed by a broker restart: the seat comes back
// on the provider the stale grant wrote, not the one it was running on.
// When the live provider is the destination of a plan migration and the
// stale one is the exhausted source, this is the restart that puts the
// seat back on the exhausted plan.
func TestHuntPlainGrantRestartResumesStaleProvider(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	opts := f.options(nil)
	opts.DisableMCPHost, opts.RestartNudge = true, "-"
	f.bootWith(t, opts)
	<-f.d.resumeDone
	first := rawSeat(t, f.sock, "moved")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "old owner detached", func() bool {
		f.d.mu.Lock()
		defer f.d.mu.Unlock()
		g := f.d.grants["moved"]
		return g != nil && g.owner == nil
	})
	liveProvider := f.d.reg.Get("moved").Provider()
	contender, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	def, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: "moved", Provider: claudia.ProviderGrok, WorkDir: t.TempDir(), SessionID: "sid-moved", TermLogPath: "-",
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := rawCall(t, contender, &broker.Request{ID: "plain", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: "moved", Def: def}})
	_ = contender.Close()
	if got.Type != broker.TypeGranted {
		t.Skipf("plain grant refused (%+v): the defect is fixed", got.Error)
	}
	if err := f.d.Close(); err != nil {
		t.Fatal(err)
	}
	f.bootWith(t, opts)
	<-f.d.resumeDone
	resumed := f.d.reg.Get("moved")
	if resumed == nil {
		t.Fatalf("seat not resumed after restart; starts=%v", starts())
	}
	if resumed.Provider() != liveProvider {
		t.Fatalf("seat ran on %q and was granted as %q; after a broker restart it resumed on %q",
			liveProvider, got.Granted.Provider, resumed.Provider())
	}
}
```

**Evidence.**

- `zz_hunt_grant_test.go:65: granted "claude" on the live "claude" seat, but grants.json now records provider "grok" (in memory "grok")`
- `zz_hunt_grant_test.go:119: seat ran on "claude" and was granted as "claude"; after a broker restart it resumed on "grok"`

**Smallest fix.** Drop `req.Grant.Adopt &&` at daemon.go:917 so every grant of an existing definition goes through the same reconciliation; a live seat on another provider then gets `CodeProviderMismatch`.

### H5. An operator CLI command never erases a seat's persisted policy

**Severity: High.** `claudia broker send|interrupt|events NAME` silently rewrites grants.json; the next boot resumes the seat without its sandbox, protected paths, role or provider policy.

**Mechanism.** The CLI reclaims with `reclaimListed`'s definition, which carries only name, provider, model, session, workdir, purpose and parent. `handleGrant` merges back only connect and materialized fields, and `registerLocked` replaces the whole definition.

**Where.** cmd/claudia/broker_seat.go:546-591; daemon/daemon.go:916-962

**Reproduction.**

`daemon/zz_hunt_cli_reclaim_test.go` (package and helpers in the appendix):

```go
// TestHuntCLIReclaimKeepsRegisteredPolicy: `claudia broker send|interrupt|
// events NAME` reclaims a running seat with a definition rebuilt from the
// grants list (cmd/claudia/broker_seat.go reclaimListed), which carries only
// name/provider/model/session/workdir/purpose/parent. handleGrant registers
// that definition whole, so the seat's persisted tool and sandbox policy is
// replaced by nothing, and the next daemon restart resumes the seat from it.
func TestHuntCLIReclaimKeepsRegisteredPolicy(t *testing.T) {
	const name = "guarded"
	f := newFixture(t)
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost = true, true
	f.bootWith(t, opts)

	work := t.TempDir()
	full, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: name, WorkDir: work, SessionID: "sid-guarded", TermLogPath: "-",
		Provider: claudia.ProviderClaude, Role: "reviewer", SandboxMode: "read-only",
		ProtectedPaths:   []claudia.ProtectedPathRule{{Pattern: "secrets/**", Reason: "fixture"}},
		AllowedProviders: []claudia.Provider{claudia.ProviderClaude},
	}})
	if err != nil {
		t.Fatal(err)
	}
	owner := dialRaw(t, f.sock)
	if got := rawCall(t, owner, &broker.Request{ID: "grant", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: name, Def: full}}); got.Type != broker.TypeGranted {
		t.Fatalf("grant: %+v", got.Error)
	}
	_ = owner.Close() // the consumer restarts; the seat keeps running
	waitFor(t, "owner gone", func() bool { return f.unowned(name) })

	// What reclaimListed sends, verbatim.
	op := dialRaw(t, f.sock)
	list := rawCall(t, op, &broker.Request{ID: "grants", Type: broker.TypeGrants, Grants: &broker.GrantsRequest{}})
	var st *broker.GrantStatus
	for i := range list.Grants.Grants {
		if list.Grants.Grants[i].Name == name {
			st = &list.Grants.Grants[i]
		}
	}
	raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: st.Name, Provider: claudia.Provider(st.Provider), Model: st.Model, SessionID: st.SessionID,
		WorkDir: st.WorkDir, Purpose: st.Purpose, Parent: st.Parent,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rawCall(t, op, &broker.Request{ID: "reclaim", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: name, Def: raw, Adopt: true, Fallback: true}}); got.Type != broker.TypeGranted {
		t.Fatalf("reclaim: %+v", got.Error)
	}

	persisted, err := claudia.NewRegistry(filepath.Join(f.state, grantsFile))
	if err != nil {
		t.Fatal(err)
	}
	def := persisted.Def(name)
	if def.SandboxMode != "read-only" || len(def.ProtectedPaths) != 1 || def.Role != "reviewer" ||
		!slices.Equal(def.AllowedProviders, []claudia.Provider{claudia.ProviderClaude}) {
		t.Fatalf("an operator reclaim rewrote the seat's persisted policy: sandbox=%q protected=%v role=%q allowed=%v",
			def.SandboxMode, def.ProtectedPaths, def.Role, def.AllowedProviders)
	}
}
```

**Evidence.**

- `zz_hunt_cli_reclaim_test.go:71: an operator reclaim rewrote the seat's persisted policy: sandbox="" protected=[] role="" allowed=[]`

**Smallest fix.** For an adopt of an existing registration with the same session, start from the existing definition and overlay only identity fields; or give the CLI a reclaim-by-name request that carries no definition.

### H6. A broker-held seat keeps the consumer's Config.ProtectedPaths

**Severity: High.** A security policy (the T209 host edit rules) is dropped silently on the supported path.

**Mechanism.** `configToGrantDef` copies most `Config` fields onto the grant definition but not `ProtectedPaths`. `TestBrokerWireMirrorsAreComplete` only checks that a same-named `AgentDef` field exists, so it passes.

**Where.** broker_wire.go:213-255

**Reproduction.**

`zz_hunt_grantdef_test.go` (package and helpers in the appendix):

```go
// TestHuntGrantDefinitionCarriesEveryConfigField: TestBrokerWireMirrorsAreComplete
// checks that each Config field has a same-named AgentDef field, which is a
// property of the types. What reaches the daemon is what configToGrantDef
// copies. Fill every such Config field with a non-zero value and see which
// arrive on the grant.
func TestHuntGrantDefinitionCarriesEveryConfigField(t *testing.T) {
	defType := reflect.TypeFor[AgentDef]()
	var cfg Config
	cv := reflect.ValueOf(&cfg).Elem()
	var shared []string
	for i := 0; i < cv.NumField(); i++ {
		f := cv.Type().Field(i)
		if _, ok := defType.FieldByName(f.Name); !ok || !f.IsExported() {
			continue
		}
		fv := cv.Field(i)
		switch fv.Kind() {
		case reflect.String:
			fv.SetString("x")
		case reflect.Bool:
			fv.SetBool(true)
		case reflect.Int, reflect.Int64:
			fv.SetInt(7)
		case reflect.Slice:
			fv.Set(reflect.MakeSlice(fv.Type(), 1, 1))
		case reflect.Ptr:
			fv.Set(reflect.New(fv.Type().Elem()))
		case reflect.Map:
			m := reflect.MakeMap(fv.Type())
			m.SetMapIndex(reflect.New(fv.Type().Key()).Elem(), reflect.New(fv.Type().Elem()).Elem())
			fv.Set(m)
		default:
			t.Logf("skipping %s (%s)", f.Name, fv.Kind())
			continue
		}
		shared = append(shared, f.Name)
	}
	g := configToGrantDef("seat", cfg, nil)
	dv := reflect.ValueOf(g.AgentDef)
	var dropped []string
	for _, name := range shared {
		if name == "Name" {
			continue
		}
		if dv.FieldByName(name).IsZero() {
			dropped = append(dropped, name)
		}
	}
	if len(dropped) > 0 {
		t.Fatalf("Config fields the daemon never receives on a grant: %v", dropped)
	}
}
```

`daemon/zz_hunt_protected_test.go` (package and helpers in the appendix):

```go
// TestHuntBrokerSeatKeepsProtectedPaths: a consumer that starts a seat with
// Config.ProtectedPaths (🎯T209 host edit rules for the sidecar's file tools)
// through the daemon must get a seat started with those rules.
func TestHuntBrokerSeatKeepsProtectedPaths(t *testing.T) {
	f := newFixture(t)
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost = true, true
	f.bootWith(t, opts)

	rules := []claudia.ProtectedPathRule{{Pattern: "secrets/**", Reason: "fixture"}}
	a, err := claudia.Start(claudia.Config{Name: "guarded-paths", WorkDir: t.TempDir(),
		SessionID: "sid-guarded-paths", TermLogPath: "-", ProtectedPaths: rules})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	if !a.DaemonHeld() {
		t.Fatal("fixture: seat did not go through the daemon")
	}
	got := f.seat(0).start().Config.ProtectedPaths
	if len(got) != 1 || got[0] != rules[0] {
		t.Fatalf("daemon started the seat with ProtectedPaths=%v; the consumer asked for %v", got, rules)
	}
	if def := f.d.reg.Def("guarded-paths"); len(def.ProtectedPaths) != 1 {
		t.Fatalf("daemon registered ProtectedPaths=%v", def.ProtectedPaths)
	}
}
```

**Evidence.**

- `zz_hunt_grantdef_test.go:57: Config fields the daemon never receives on a grant: [ProtectedPaths]`
- `zz_hunt_protected_test.go:30: daemon started the seat with ProtectedPaths=[]; the consumer asked for [{secrets/** fixture}]`

**Smallest fix.** Add `ProtectedPaths: slices.Clone(cfg.ProtectedPaths)` to `configToGrantDef`, and make the mirror test behavioural like the first repro.

### H7. A sidecar seat start honours cancellation after the load is sent

**Severity: High.** A sidecar that accepts `load` and never answers wedges the start forever; `grantStartTimeout` does not bound it, and `Registry.Stop`/`Remove` of that name join the stuck launch and hang too.

**Mechanism.** After `conn.Send(load)`, `StartAgent` calls `conn.Recv()` with no deadline (only the AdoptOnly path sets one) and nothing ties the request context to the connection; `replaceStaleOMPSeat`'s reload has the same shape. `stopLifecycle` cancels the launch's context and then waits on the lifecycle gate with `context.Background()`, so it waits as long as the launch does.

**Where.** omp_agent.go:385-403; registry_lifecycle.go:66-71

**Reproduction.**

`zz_lead_omp_start_test.go` (package and helpers in the appendix):

```go
func TestAuditDirectStartCancellationClosesHandshake(t *testing.T) {
	stubOMPPlans(t)
	dir, err := os.MkdirTemp("/tmp", "claudia-audit-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	t.Setenv(omp.SocketEnv, socket)
	received := make(chan error, 1)
	closePeer := make(chan struct{})
	defer close(closePeer)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			received <- err
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				received <- err
				return
			}
			var req omp.Message
			if err := json.Unmarshal(line, &req); err != nil {
				received <- err
				return
			}
			if req.Op == omp.OpLoad {
				received <- nil
				<-closePeer
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	workDir := t.TempDir()
	go func() {
		a, err := StartDirectContext(ctx, Config{
			Name: "audit", Provider: ProviderClaude, Model: "fixture-model",
			WorkDir: workDir, TermLogPath: "-",
		})
		if a != nil {
			a.Stop()
		}
		result <- err
	}()
	select {
	case err := <-received:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-result:
		t.Fatalf("start returned before load: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("fake never received load")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("start returned %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("start is still blocked after cancellation with load received")
	}
}
```

**Evidence.**

- `zz_lead_omp_start_test.go:92: start is still blocked after cancellation with load received`

**Smallest fix.** Wrap the handshake in `stop := context.AfterFunc(req.Context, func() { conn.Close() }); defer stop()`, and map the resulting read error to `req.Context.Err()`.

### H8. The shared-clone git guards see the subcommand after `git -c key=value`

**Severity: High.** `git -c color.ui=never stash` or `git -c core.pager=cat checkout other` bypasses both gitdestructguard (T240) and branchguard (T164), the T102 class of destroyed uncommitted work.

**Mechanism.** Both lexers skip words that start with `-` and take the next word as the subcommand. `-c` takes a separate operand, so `color.ui=never` becomes the subcommand; the same applies to `--git-dir`, `--work-tree`, `--namespace`, `--super-prefix` and `--config-env`. Branchguard handles `-C` only.

**Where.** sidecar/gitdestructguard.ts:24-30; sidecar/branchguard.ts:68-78

**Reproduction.**

`sidecar/zz-hunt-git-c-bypass.test.ts`, whole file:

```ts
// Defect hunt 2026-11-20 (fork sidecar): the shared-clone git guards (🎯T164,
// 🎯T240) must see through git's own global options, `-c key=value` above all.
import { afterEach, expect, test } from "bun:test";
import { execFileSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { detectBranchCheckout } from "./branchguard.ts";
import { destructiveGitCommands } from "./gitdestructguard.ts";
import { runShell } from "./execution.ts";

const dirs: string[] = [];
afterEach(() => { for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true }); });

function sharedClone(): string {
  const dir = mkdtempSync(join(tmpdir(), "gitc-"));
  dirs.push(dir);
  const git = (...args: string[]) => execFileSync("git", ["-C", dir, "-c", "user.email=t@example.invalid", "-c", "user.name=t", ...args], { stdio: "pipe" });
  git("init", "-q", "-b", "master");
  writeFileSync(join(dir, "work.txt"), "committed\n");
  git("add", "work.txt");
  git("commit", "-q", "-m", "init");
  git("branch", "other");
  // Another seat's uncommitted work in the shared clone.
  writeFileSync(join(dir, "work.txt"), "another seat's uncommitted edit\n");
  return dir;
}

test("the lexers see a git subcommand after -c key=value", () => {
  expect(destructiveGitCommands("git stash").map((h) => h.operation)).toEqual(["stash"]);
  expect(destructiveGitCommands("git -c color.ui=never stash").map((h) => h.operation)).toEqual(["stash"]);
  expect(destructiveGitCommands("git -c core.pager=cat reset --hard").map((h) => h.operation)).toEqual(["reset"]);
  expect(detectBranchCheckout("git -c core.pager=cat checkout -b other")?.ref).toBe("other");
});

test("Bash refuses `git -c … stash` in the shared clone as it refuses `git stash`", async () => {
  const dir = sharedClone();
  const run = (command: string) => runShell(() => dir, () => undefined, { command, timeoutMs: 20_000, maxBuffer: 1 << 20 });
  await expect(run("git stash")).rejects.toThrow(/gitdestructguard/);
  expect(readFileSync(join(dir, "work.txt"), "utf8")).toBe("another seat's uncommitted edit\n");
  let outcome = "refused";
  try { await run("git -c color.ui=never stash"); outcome = "ran"; } catch (err) { outcome = `refused: ${String(err).slice(0, 60)}`; }
  expect({ outcome, workTxt: readFileSync(join(dir, "work.txt"), "utf8") })
    .toEqual({ outcome: expect.stringMatching(/^refused/), workTxt: "another seat's uncommitted edit\n" });
});

test("Bash refuses `git -c … checkout other` in the shared clone", async () => {
  const dir = sharedClone();
  execFileSync("git", ["-C", dir, "stash", "-q"]); // a clean tree so checkout can move HEAD
  const run = (command: string) => runShell(() => dir, () => undefined, { command, timeoutMs: 20_000, maxBuffer: 1 << 20 });
  await expect(run("git checkout other")).rejects.toThrow(/branchguard/);
  let outcome = "refused";
  try { await run("git -c core.pager=cat checkout other"); outcome = "ran"; } catch { outcome = "refused"; }
  const head = execFileSync("git", ["-C", dir, "symbolic-ref", "--short", "HEAD"]).toString().trim();
  expect({ outcome, head }).toEqual({ outcome: "refused", head: "master" });
  expect(existsSync(dir)).toBe(true);
});
```

**Evidence.**

- `destructiveGitCommands("git -c color.ui=never stash"): - [ "stash" ]  + []`
- `-   "outcome": StringMatching /^refused/,  +   "outcome": "ran",  (work.txt lost another seat's edit)`
- `-   "head": "master",  -   "outcome": "refused",  +   "head": "other",  +   "outcome": "ran",`

**Smallest fix.** In both option loops, treat the git global options that take a separate operand (`-c`, `-C`, `--git-dir`, `--work-tree`, `--namespace`, `--super-prefix`, `--config-env`) as consuming the next word.

### H9. Host lines reach the seat with their UTF-8 intact, whatever the socket chunking

**Severity: High.** Large non-ASCII prompts and tool results are silently corrupted with U+FFFD on their way into the model's context and the durable store; JSON still parses, so nothing notices.

**Mechanism.** The socket `data` handler decodes each chunk separately with `chunk.toString("utf8")`, so a multi-byte character split across two reads becomes replacement characters.

**Where.** sidecar/server.ts:236

**Reproduction.**

`sidecar/zz-hunt-utf8-split.test.ts`, whole file:

```ts
// Defect hunt 2026-11-20 (fork sidecar): a host line whose bytes arrive in two
// socket reads split inside a multi-byte UTF-8 character must reach the seat
// unchanged.
import { afterEach, expect, setDefaultTimeout, test } from "bun:test";
import { join } from "node:path";
import { FIXTURE_CLEANUP_MS, launchAbortFixture } from "./testdata/abort-fixture";

setDefaultTimeout(90_000);
const fixtures: ReturnType<typeof launchAbortFixture>[] = [];
afterEach(async () => {
  for (const f of fixtures.splice(0)) await f.close();
}, FIXTURE_CLEANUP_MS * 2);

test("a prompt split mid-character across reads keeps its text", async () => {
  const f = launchAbortFixture({ entrypoint: join(import.meta.dir, "testdata/abort-provider.ts") });
  fixtures.push(f);
  const host = await f.ready;
  const seat = "utf8";
  host.send({ op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: f.dir });
  expect((await host.next("ready", (e) => e.type === "ready" || e.type === "error")).type).toBe("ready");

  const text = "café — naïve 日本語";
  const line = Buffer.from(JSON.stringify({ op: "prompt", seat, turn_id: "T1", text }) + "\n", "utf8");
  // Split inside the two-byte "é" (0xC3 0xA9).
  const at = line.indexOf(Buffer.from("é", "utf8")) + 1;
  const socket = (host as unknown as { socket: import("node:net").Socket }).socket;
  socket.write(line.subarray(0, at));
  await Bun.sleep(300); // two separate reads on the sidecar side
  socket.write(line.subarray(at));

  const reply = await host.next("reply", (e) => e.type === "text" && e.turn_id === "T1", 20_000);
  expect(reply.text).toBe(`reply: ${text}`);
});

// No artificial split: one write of a large prompt, as omp.Conn.Send does it.
test("a large non-ASCII prompt in one write keeps its text", async () => {
  const f = launchAbortFixture({ entrypoint: join(import.meta.dir, "testdata/abort-provider.ts") });
  fixtures.push(f);
  const host = await f.ready;
  const seat = "utf8-large1"; // odd-length line prefix: characters start at odd byte offsets
  host.send({ op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: f.dir });
  expect((await host.next("ready", (e) => e.type === "ready" || e.type === "error")).type).toBe("ready");
  // 80 KB of two-byte characters in a non-repeating order (the mock's
  // stream-loop detector refuses a repeated cycle).
  const alphabet = "àáâãäåæçèéêëìíîïñòóôõöùúûüýÀÁÂÃÄÅÆÇÈÉÊËÌÍÎÏÑÒÓÔÕÖÙÚÛÜÝ";
  let x = 12345;
  let text = "";
  for (let i = 0; i < 40_001; i++) { x = (x * 1103515245 + 12345) % 2147483648; text += alphabet[x % alphabet.length]; }
  host.send({ op: "prompt", seat, turn_id: "T1", text });
  const end = await host.next("end", (e) => (e.type === "text" || e.type === "turn_end" || e.type === "error") && e.turn_id === "T1", 30_000);
  if (end.type !== "text") throw new Error(`no reply: ${JSON.stringify({ ...end, snapshot: undefined })}`);
  const got = String(end.text);
  expect({ length: got.length, replacementChars: (got.match(/�/g) ?? []).length })
    .toEqual({ length: `reply: ${text}`.length, replacementChars: 0 });
});
```

**Evidence.**

- `Expected: "reply: café — naïve 日本語"  Received: "reply: caf\ufffd\ufffd — naïve 日本語"`
- `-   "replacementChars": 0,  +   "replacementChars": 35,`

**Smallest fix.** `socket.setEncoding("utf8")` at the top of the connection handler (a StringDecoder underneath) and append `chunk` directly.

### H10. A tail subscriber that stops reading never blocks the daemon

**Severity: High.** Every daemon path that emits a lifecycle event hangs, and a consumer's Send on an unrelated seat goes unanswered.

**Mechanism.** When a tail subscriber's 64-event queue is full, `Server.emit` writes the lag error to it synchronously, but the tail's writer goroutine already holds the connection lock inside a blocked `Write`. `emit` is called from `handleGrant`, `handleRelease`, `ConnClosed`, the orphan timers, `onSeatEvent` and the usage update, several of them on a connection's own request goroutine.

**Where.** internal/broker/server.go:524-530 (with server.go:503 and transport.go:272-275)

**Reproduction.**

`internal/broker/zz_hunt_tail_test.go` (package and helpers in the appendix):

```go
// TestHuntLaggedTailNeverBlocksEmit: a tail subscriber that stops reading
// must be dropped, not waited on. emit is called from every daemon path that
// publishes a lifecycle event (grant, release, detach, resume, usage update),
// so a blocking emit stalls those paths for as long as one tail client sleeps.
func TestHuntLaggedTailNeverBlocksEmit(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "hbt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ln, err := Listen(filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Serve(&ServeArgs{Listener: ln})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c, err := Dial(filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteRequest(&Request{ID: "tail", Type: TypeTail, Tail: &TailRequest{}}); err != nil {
		t.Fatal(err)
	}
	if ack, err := c.ReadResponse(); err != nil || ack.Type != TypeTailing {
		t.Fatalf("tail ack = %v, %v", ack, err)
	}
	// From here on the tail client never reads: a stopped `claudia broker
	// tail`, or a consumer process that is paused or wedged.

	detail := strings.Repeat("x", 4096)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Paced, as lifecycle events are: the tail's writer drains its
		// channel into the socket until the kernel buffer is full and the
		// write blocks; only then does the channel back up and lag.
		for i := 0; i < 3000; i++ {
			srv.Emit(EventMessage{Kind: EventGrant, Name: "seat", Detail: detail})
			if i%8 == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = c.Close() // unblock the stuck write so the goroutine and Close can finish
		<-done
		t.Fatal("Emit blocked for 30s behind a tail subscriber that stopped reading")
	}
	_ = c.Close()
}
```

`daemon/zz_hunt_stall_test.go` (package and helpers in the appendix):

```go
// TestHuntStalledTailDoesNotStallSeatOwner: one `broker tail` client that
// stops reading must not stop the daemon from answering an unrelated
// consumer's requests on its own seat.
func TestHuntStalledTailDoesNotStallSeatOwner(t *testing.T) {
	f := newFixture(t)
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost = true, true
	f.bootWith(t, opts)

	tail, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := tail.WriteRequest(&broker.Request{ID: "tail", Type: broker.TypeTail, Tail: &broker.TailRequest{}}); err != nil {
		t.Fatal(err)
	}
	if ack, err := tail.ReadResponse(); err != nil || ack.Type != broker.TypeTailing {
		t.Fatalf("tail ack = %v, %v", ack, err)
	}
	// The tail client now stops reading. Closing it releases everything.
	t.Cleanup(func() { _ = tail.Close() })

	// A burst of lifecycle events (a boot resume, a usage refresh), paced
	// so the tail's socket fills before its 64-event queue does.
	detail := strings.Repeat("x", 4096)
	go func() {
		for i := 0; i < 3000; i++ {
			f.d.emit(broker.EventMessage{Kind: broker.EventUsageUpdate, Detail: detail})
			if i%8 == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()
	time.Sleep(3 * time.Second)

	// An unrelated consumer takes a seat and sends a turn.
	c := rawSeat(t, f.sock, "victim")
	if err := c.WriteRequest(&broker.Request{ID: "send", Type: broker.TypeSend,
		Send: &broker.SendRequest{Name: "victim", Text: "hello"}}); err != nil {
		t.Fatal(err)
	}
	resp, err := huntAwait(c, "send", "", 10*time.Second)
	if err != nil {
		stacks := huntStacks()
		i := strings.Index(stacks, "broker.(*Server).emit")
		t.Fatalf("send on an unrelated seat not answered within 10s (%v); daemon stuck in tail emit: %v\n%s",
			err, i >= 0, excerpt(stacks, "handleGrant"))
	}
	if resp.Type != broker.TypeSent {
		t.Fatalf("send answered %s: %+v", resp.Type, resp.Error)
	}
}
```

**Evidence.**

- `zz_hunt_tail_test.go:63: Emit blocked for 30s behind a tail subscriber that stopped reading`
- `zz_hunt_stall_test.go:79: send on an unrelated seat not answered within 10s (read unix ->/tmp/.../b.sock: i/o timeout); daemon stuck in tail emit: true`

**Smallest fix.** Give the lag-path write a deadline (`SetDeadline(ioDeadline(shutdownWriteBound))` before `replyErr`), or close the subscriber and let `writeTail` say goodbye.

### H11. A reclaim delivers the unowned history before any live event

**Severity: High.** The consumer sees a turn's events reordered, against the documented "history before live traffic".

**Mechanism.** `handleGrant` creates and starts the new owner's `runPump` before it writes the ring, so live events published during the replay contend for the same connection lock and land inside the history.

**Where.** daemon/daemon.go:1007-1027

**Reproduction.**

`daemon/zz_hunt_order_test.go` (package and helpers in the appendix):

```go
// TestHuntReclaimReplaysHistoryBeforeLiveEvents: handleGrant says "Replay
// what the seat said while unowned, then confirm the grant so the consumer
// sees history before live traffic". The new owner's pump is started before
// that replay, so a live event published while the ring is being written
// reaches the consumer in the middle of (or ahead of) the history.
func TestHuntReclaimReplaysHistoryBeforeLiveEvents(t *testing.T) {
	const name = "order"
	f := newFixture(t)
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost = true, true
	f.bootWith(t, opts)

	old := rawSeat(t, f.sock, name)
	proc := f.d.reg.Get(name)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "disconnect to detach the owner", func() bool { return f.unowned(name) })

	// A jevonsd-length bounce: the seat keeps streaming while unowned.
	pad := strings.Repeat("p", 200)
	const history = 3000
	for i := 0; i < history; i++ {
		proc.PublishEvent(claudia.Event{Type: "progress", ProgressType: "tool_use", ToolTitle: fmt.Sprintf("h%04d-%s", i, pad)})
	}

	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: *f.d.reg.Def(name)})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteRequest(&broker.Request{ID: "reclaim", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: name, Def: raw, Adopt: true, Fallback: true}}); err != nil {
		t.Fatal(err)
	}
	// The consumer has not started reading yet. As soon as the daemon has
	// handed it the grant, the seat says something new.
	waitFor(t, "reclaim to take ownership", func() bool { return f.owned(name) })
	proc.PublishEvent(claudia.Event{Type: "progress", ProgressType: "tool_use", ToolTitle: "LIVE"})

	titles := huntReadTitles(t, c, "reclaim")
	live, last := -1, -1
	for i, title := range titles {
		if title == "LIVE" {
			live = i
		}
		if strings.HasPrefix(title, "h") {
			last = i
		}
	}
	if live == -1 {
		return // LIVE arrived after the grant reply: correct order.
	}
	if live < last {
		t.Fatalf("live event delivered at position %d, before history event at %d (of %d history events); grant reply at %d",
			live, last, history, len(titles)-1)
	}
}
```

**Evidence.**

- `zz_hunt_order_test.go:95: live event delivered at position 62, before history event at 3000 (of 3000 history events); grant reply at 3001`

**Smallest fix.** Start `go d.runPump(...)` only after the replay loop and the `Granted` reply; events arriving meanwhile queue in the pump channel.

### H12. Detaching a slow consumer and its same-connection re-claim keep event order

**Severity: High.** After a T125 slow-consumer detach and the library's automatic re-claim, the consumer sees newer events before older ones.

**Mechanism.** The forwarder's detach closes the pump, but the old `runPump` keeps draining its buffered events to the same live connection, and those writes land after the re-claim's replay of newer ring events.

**Where.** daemon/daemon.go:1275-1281 and 1326-1331

**Reproduction.**

`daemon/zz_hunt_pump_test.go` (package and helpers in the appendix):

```go
// TestHuntSlowConsumerReclaimKeepsOrder: the daemon detaches an owner whose
// queue filled and tells it so; the library answers by re-claiming on the
// same connection (🎯T124/🎯T125). The detached pump still holds events it
// dequeued before the detach and keeps writing them after the re-claim's
// replay, so the consumer sees the seat's events out of order.
func TestHuntSlowConsumerReclaimKeepsOrder(t *testing.T) {
	const name = "slow"
	f := newFixture(t)
	gate, release := heldPump(t)
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost, opts.pumpSize, opts.pumpGate = true, true, 2, gate
	f.bootWith(t, opts)
	t.Cleanup(release)

	c := rawSeat(t, f.sock, name)
	proc := f.d.reg.Get(name)
	f.d.mu.Lock()
	pump := f.d.grants[name].pump
	f.d.mu.Unlock()
	huntPublish(proc, "e0")
	waitFor(t, "pump to dequeue e0", func() bool { return len(pump) == 0 })
	huntPublish(proc, "e1")
	huntPublish(proc, "e2") // queue full
	huntPublish(proc, "e3") // overflow: detach, e3 retained
	waitFor(t, "slow consumer detached", func() bool { return f.unowned(name) })
	huntPublish(proc, "e4") // retained

	raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: *f.d.reg.Def(name)})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteRequest(&broker.Request{ID: "reclaim", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: name, Def: raw, Adopt: true, Fallback: true}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "re-claim to take the grant", func() bool { return f.owned(name) })
	time.Sleep(200 * time.Millisecond) // let the replay reach the socket
	release()

	got := huntCollect(t, c, []string{"e0", "e1", "e2", "e3", "e4"})
	if order := huntEventsOnly(got); order != "e0,e1,e2,e3,e4" {
		t.Fatalf("consumer saw %s (full sequence %v), want e0,e1,e2,e3,e4", order, got)
	}
}
```

**Evidence.**

- `zz_hunt_pump_test.go:97: consumer saw e3,e4,e0,e1,e2 (full sequence [e3 e4 <granted> e0 e1 e2]), want e0,e1,e2,e3,e4`

**Smallest fix.** Before each write, have `runPump` check under `d.mu` that `g.pump == pump && g.owner == c`; if not, put the remaining events back at the front of the ring (the same helper C1 needs).

### H13. Two connections granting the same name at once never both get it

**Severity: High.** The first consumer holds a `Granted` reply but silently stops receiving events; its waits hang and its re-claims fail with `grant_held`.

**Mechanism.** The ownership check runs before the launch and ownership is taken after it, so both requests pass the check. The second to bind calls `detachLocked` on the first without `notifyDetached`.

**Where.** daemon/daemon.go:880-893 and 998-1000

**Reproduction.**

`daemon/zz_hunt_two_owner_test.go` (package and helpers in the appendix):

```go
// TestHuntConcurrentGrantHasOneOwner: two connections grant the same new
// name while the seat is still starting. handleGrant checks ownership
// before the launch and takes it after, so both pass the check; the second
// to finish silently detaches the first (no agent_detached), and both
// consumers hold a Granted reply for one seat.
func TestHuntConcurrentGrantHasOneOwner(t *testing.T) {
	const name = "contended"
	f := newFixture(t)
	release := make(chan struct{})
	var once sync.Once
	f.startHook = func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
		<-release
		return f.startSeat(ctx, cfg)
	}
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost = true, true
	f.bootWith(t, opts)
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: name, WorkDir: t.TempDir(), SessionID: "sid-contended", TermLogPath: "-"}})
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		who  string
		resp *broker.Response
		err  error
		c    *broker.Conn
	}
	answers := make(chan answer, 2)
	for _, who := range []string{"first", "second"} {
		c := dialRaw(t, f.sock)
		if err := c.WriteRequest(&broker.Request{ID: who, Type: broker.TypeGrant,
			Grant: &broker.GrantRequest{Name: name, Def: raw}}); err != nil {
			t.Fatal(err)
		}
		go func() {
			resp, err := huntAwait(c, who, "", 20*time.Second)
			answers <- answer{who, resp, err, c}
		}()
	}
	time.Sleep(500 * time.Millisecond) // both requests are inside handleGrant
	once.Do(func() { close(release) })

	var granted []answer
	for range 2 {
		a := <-answers
		if a.err != nil {
			t.Fatalf("%s: %v", a.who, a.err)
		}
		if a.resp.Type == broker.TypeGranted {
			granted = append(granted, a)
		}
	}
	if len(granted) < 2 {
		return // one was refused: one owner, as intended
	}
	// Both were granted. The one that is not the owner must at least be told.
	f.d.mu.Lock()
	ownerID := f.d.grants[name].owner.ID
	f.d.mu.Unlock()
	for _, a := range granted {
		if resp, err := huntAwait(a.c, "", broker.TypeAgentDetached, 2*time.Second); err == nil && resp != nil {
			return // the displaced consumer learned it lost the seat
		}
	}
	t.Fatalf("both connections were granted %q; owner is conn %d and the other was displaced without agent_detached", name, ownerID)
}
```

**Evidence.**

- `zz_hunt_two_owner_test.go:80: both connections were granted "contended"; owner is conn 1 and the other was displaced without agent_detached`

**Smallest fix.** At bind time, if `g.owner != nil && g.owner != c`, refuse with `grant_held` instead of detaching; at minimum, `notifyDetached` the displaced owner.

### H14. A refusal refresh never hands one plan's token to another plan

**Severity: High.** Every seat on the failover plan runs on the wrong plan's token, and the refreshed plan's sidecar token stays revoked.

**Mechanism.** `recoverFailedToken` refreshes the plan it read from `c.provider` at the start, which takes seconds. Afterwards `refreshRejectedToken` re-reads `c.provider` for `reloadOMPSeats` and `c.reload(token)`; if the seat failed over in between, it sends `load{provider: openai-codex, token: <fresh anthropic token>}`, and the sidecar's `setPlanToken` installs it for the whole codex plan.

**Where.** omp_agent.go:1464-1466 (with 1088 and 1091), 1355-1375

**Reproduction.**

`zz_hunt_cross_plan_token_test.go` (package and helpers in the appendix):

```go
// TestHuntRefusalRefreshNeverHandsOnePlansTokenToAnother: a seat's turn is
// refused on its plan's token (anthropic), so the seat refreshes the plan
// (recoverFailedToken -> refreshRejectedToken, a provider round trip that
// takes seconds). While the refresh is out, the seat fails over to another
// plan (switchModel to openai-codex). When the refresh comes back, the
// renewed anthropic token must reach anthropic's seats, and the codex plan's
// token in the sidecar must stay codex's.
//
// The scripted sidecar keeps one token per plan the way server.ts does:
// setPlanToken(msg.provider, msg.token) on every load that names both
// (switchIfAsked), and on OpToken.
func TestHuntRefusalRefreshNeverHandsOnePlansTokenToAnother(t *testing.T) {
	var mu sync.Mutex
	planTokens := map[string]string{}
	seatProvider := ""
	startHuntSidecar(t, func(h *huntConn, msg omp.Message) {
		mu.Lock()
		defer mu.Unlock()
		switch msg.Op {
		case omp.OpToken:
			planTokens[msg.Provider] = msg.Token
		case omp.OpLoad:
			if msg.Provider != "" && msg.Token != "" {
				planTokens[msg.Provider] = msg.Token
			}
			how := "launched"
			if seatProvider != "" {
				how = "adopted"
				if msg.Provider != "" && msg.Provider != seatProvider {
					how = "switched"
				}
			}
			if msg.Provider != "" {
				seatProvider = msg.Provider
			}
			h.write(map[string]any{"seat": msg.Seat, "type": "ready", "how": how, "provider": seatProvider,
				"model": msg.Model, "request_id": msg.RequestID, "restored": false})
		}
	})
	// stubOMPPlans (inside startHuntSidecar) stores anthropic=plan-token and
	// openai-codex=codex-token. The anthropic refresh waits for release.
	release := make(chan struct{})
	entered := make(chan struct{})
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	ompLogin = omp.Login{
		Script: "auth.ts",
		Run: func(context.Context, string, ...string) ([]byte, error) {
			close(entered)
			<-release
			return []byte(`{"refresh_token":"r-new","access_token":"fresh-anthropic","expiry":"` + exp + `"}`), nil
		},
	}
	t.Cleanup(func() { ompLogin = omp.Login{} })

	a, err := StartDirect(Config{Name: "seat", Provider: ProviderClaude, Model: "fixture-model",
		WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	ctrl := a.tmuxCtrl.(*ompControl)

	refreshed := make(chan struct{})
	go func() {
		defer close(refreshed)
		ctrl.recoverFailedToken(tokenFingerprint("plan-token")) // the provider refused plan-token
	}()
	<-entered
	if err := ctrl.switchModel(ProviderCodex, "fixture-codex-model", SwitchOptions{}); err != nil {
		t.Fatalf("failover switch: %v", err)
	}
	close(release)
	<-refreshed
	time.Sleep(200 * time.Millisecond) // let the last frames land

	mu.Lock()
	defer mu.Unlock()
	if planTokens[omp.OpenAICodex] != "codex-token" {
		t.Fatalf("after the anthropic refresh the sidecar's openai-codex plan token is %q (want %q); anthropic plan token %q",
			planTokens[omp.OpenAICodex], "codex-token", planTokens[omp.Anthropic])
	}
}
```

**Evidence.**

- `zz_hunt_cross_plan_token_test.go:90: after the anthropic refresh the sidecar's openai-codex plan token is "fresh-anthropic" (want "codex-token"); anthropic plan token "plan-token"`
- Run without `CLAUDIA_OMP_NO_REFRESH` (the login runner is a stub; nothing reaches the network). With that variable set the refresh is refused and the test waits forever at line 79.

**Smallest fix.** Capture the provider once in `recoverFailedToken` and pass it through; distribute the refreshed token with `OpToken{Provider: provider}`, never with a load message built from the seat's current provider.

### H15. Concurrent launches refresh an expired plan once

**Severity: High.** The second refresh presents a spent refresh token and gets `invalid_grant` (the T158 failure); that launch fails and the plan can end up needing an owner sign-in.

**Mechanism.** `ensureOMPToken` -> `Store.Ensure` -> `login.Refresh` takes no `ompRefreshLock`, while the renewal loop and the refusal paths do.

**Where.** omp_agent.go:1328-1341; omp/cred.go:469-479

**Reproduction.**

`zz_hunt_launch_refresh_test.go` (package and helpers in the appendix):

```go
// TestHuntConcurrentLaunchesRefreshAnExpiredPlanOnce: two seats launch on a
// plan whose access token has lapsed (the broker slept past the renewal
// loop's margin, or a renewal failed and is in backoff). Each launch calls
// ensureOMPToken (omp_agent.go StartAgent). A refresh spends the refresh
// token it presents, so the plan must be refreshed once; the second launch
// must take the first one's token (the 🎯T158 rule the refusal path keeps
// with ompRefreshLock).
func TestHuntConcurrentLaunchesRefreshAnExpiredPlanOnce(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	blob := `{"records":{"anthropic":{"refresh_token":"r1","access_token":"stale","expiry":"` + past + `"}}}`
	ompKeychain = func(context.Context, string, ...string) ([]byte, error) { return []byte(blob), nil }
	var mu sync.Mutex
	var presented []string
	ompLogin = omp.Login{
		Script: "auth.ts",
		Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			var existing omp.Record
			_ = json.Unmarshal([]byte(args[len(args)-1]), &existing)
			mu.Lock()
			presented = append(presented, existing.RefreshToken)
			mu.Unlock()
			time.Sleep(200 * time.Millisecond) // the provider round trip
			return []byte(`{"refresh_token":"r2","access_token":"fresh","expiry":"` + future + `"}`), nil
		},
	}
	omp.ResetKeychainShot()
	t.Cleanup(func() { ompKeychain = nil; ompLogin = omp.Login{}; omp.ResetKeychainShot() })
	if err := OpenOMPPlans(context.Background()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for _, seat := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ensureOMPToken(context.Background(), omp.Anthropic, "launch "+seat); err != nil {
				t.Errorf("launch %s: %v", seat, err)
			}
		}()
	}
	wg.Wait()
	if len(presented) != 1 {
		t.Fatalf("plan refreshed %d times by two concurrent launches, presenting refresh tokens %q", len(presented), presented)
	}
}
```

**Evidence.**

- `zz_hunt_launch_refresh_test.go:57: plan refreshed 2 times by two concurrent launches, presenting refresh tokens ["r1" "r1"]`
- Run without `CLAUDIA_OMP_NO_REFRESH`, as for H14.

**Smallest fix.** Take `ompRefreshLock(provider)` in `ensureOMPToken` and re-check the access token under it before refreshing.

### H16. A plan renewal never reverts another plan's renewed record

**Severity: High.** A reverted record holds a spent refresh token; that plan's next refresh fails with `invalid_grant`.

**Mechanism.** `Store.Put` loads a clone of the shared item, sets one record and saves the whole item, releasing `shot.mu` between Load and Save. Per-provider refresh locks let different plans renew concurrently, so one plan's save overwrites the other's.

**Where.** omp/cred.go:414-423

**Reproduction.**

`omp/zz_hunt_put_test.go` (package and helpers in the appendix):

```go
// TestHuntConcurrentPlanRenewalsKeepBothRecords: two plans renew at once (a
// refused codex seat's refresh while the expiry loop renews anthropic; each
// holds only its own provider's refresh lock). Each renewal ends in
// Store.Put, which loads the shared item, sets one record and saves the whole
// item back. Neither renewal may lose the other's new record: a refresh spends
// the refresh token it replaces, so a reverted record holds a spent token.
func TestHuntConcurrentPlanRenewalsKeepBothRecords(t *testing.T) {
	resetKeychainShot()
	t.Cleanup(resetKeychainShot)
	s := Store{
		BrokerPath: "/usr/local/bin/claudia", DataPath: filepath.Join(t.TempDir(), "plan.enc"),
		Run: func(context.Context, string, ...string) ([]byte, error) { return []byte(`{"records":{}}`), nil },
	}
	ctx := context.Background()
	if err := Open(ctx, s); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	rec := func(p string, i int) Record {
		return Record{RefreshToken: p + "-r" + strconv.Itoa(i), AccessToken: p + "-a" + strconv.Itoa(i), Expiry: exp}
	}
	const n = 20000
	var stop atomic.Bool
	var reverted atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // anthropic renews continuously until codex is done
		defer wg.Done()
		for i := 1; !stop.Load(); i++ {
			if err := s.Put(ctx, Anthropic, rec(Anthropic, i)); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer stop.Store(true)
		for i := 1; i <= n; i++ {
			if err := s.Put(ctx, OpenAICodex, rec(OpenAICodex, i)); err != nil {
				t.Error(err)
				return
			}
			item, _ := s.Load(ctx)
			if got := item.Records[OpenAICodex].RefreshToken; got != rec(OpenAICodex, i).RefreshToken {
				reverted.Add(1)
			}
		}
	}()
	wg.Wait()
	item, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	final := item.Records[OpenAICodex].RefreshToken
	if reverted.Load() > 0 || final != rec(OpenAICodex, n).RefreshToken {
		t.Fatalf("codex record reverted %d times by a concurrent anthropic Put; final refresh token %q, want %q",
			reverted.Load(), final, rec(OpenAICodex, n).RefreshToken)
	}
}
```

**Evidence.**

- `zz_hunt_put_test.go:69: codex record reverted 258 times by a concurrent anthropic Put; final refresh token "openai-codex-r20000", want "openai-codex-r20000"`

**Smallest fix.** Make `Put` mutate `shot.item.Records[provider]` and persist under one `shot.mu` hold (a `Store.Update(provider, rec)`), with no Load/Save pair.

### H17. Stopping or adopting a cursor seat never signals an unrelated process

**Severity: High.** SIGKILL goes to whatever same-user process now has a stale pid. Reachable from grants.json entries left by the retired cursor CLI transport, or from a grant definition whose `connect_pid` the daemon does not validate.

**Mechanism.** `ReapCursorACPLeftovers` kills the persisted `ConnectPID` unconditionally, with no check that the pid is a cursor-agent. It runs on `Registry.Stop`/`Remove` of a cursor definition with no live handle and on cursor adopt failure.

**Where.** cursor_reap.go:80-82 and 173-189; registry.go:591, 596, 808-809

**Reproduction.**

`zz_hunt_cursor_reap_test.go` (package and helpers in the appendix):

```go
// TestHuntStopNeverKillsAnUnrelatedProcess: a cursor seat's definition still
// carries the ConnectPID its CLI-era process had. That process is long gone
// and the pid now belongs to something else (here, a sleep this test owns).
// Stopping the seat, which has no live handle, must not signal that pid.
func TestHuntStopNeverKillsAnUnrelatedProcess(t *testing.T) {
	victim := exec.Command("sleep", "30")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- victim.Wait() }()
	t.Cleanup(func() { _ = victim.Process.Kill() })

	reg, err := NewRegistry(filepath.Join(t.TempDir(), "grants.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(AgentDef{Name: "cur", Provider: ProviderCursor, SessionID: "sess-1",
		ConnectPID: victim.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	reg.Stop("cur")
	select {
	case err := <-exited:
		t.Fatalf("Stop of a seat with no live handle killed pid %d, an unrelated process: %v", victim.Process.Pid, err)
	case <-time.After(1 * time.Second):
	}
}
```

**Evidence.**

- `zz_hunt_cursor_reap_test.go:34: Stop of a seat with no live handle killed pid 28543, an unrelated process: signal: killed`

**Smallest fix.** Delete the `extraPID` kill (the transport that set it is gone); at minimum verify the pid's command line before signalling.

### H18. A wait retried after a timeout never loses the text the timed-out wait collected

**Severity: High.** A retried wait returns the tail of the turn with a nil error, so the caller accepts a truncated answer as complete.

**Mechanism.** `subscribeEventsTakingTurn` takes and disarms the latch when a wait subscribes. A wait that ends on its context or the silence bound throws away what it collected, events published between waits reach nobody, and the next wait returns only what arrives after it subscribes.

**Where.** turn_latch.go:141-151 and 168

**Reproduction.**

`zz_hunt_rewait_test.go` (package and helpers in the appendix):

```go
// A caller waits with a deadline, the deadline passes mid-turn, and the
// caller waits again for the same turn (no new Send). The first wait took
// the turn latch at subscription and disarmed it, so the text it collected
// and the text published between the two waits is gone: the second wait
// returns only the tail, with a nil error, as if it were the whole reply.
func TestHuntRewaitAfterTimeoutReturnsWholeTurn(t *testing.T) {
	step1, step2 := make(chan struct{}), make(chan struct{})
	startHuntSidecar(t, func(h *huntSidecar, msg omp.Message) {
		if msg.Op != omp.OpPrompt {
			return
		}
		turn := msg.TurnID
		h.write(t, map[string]any{"seat": msg.Seat, "type": "text", "text": "one ", "turn_id": turn})
		go func() {
			<-step1
			h.write(t, map[string]any{"seat": msg.Seat, "type": "text", "text": "two ", "turn_id": turn})
			<-step2
			h.write(t, map[string]any{"seat": msg.Seat, "type": "text", "text": "three", "turn_id": turn})
			h.write(t, map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": turn})
		}()
	})
	a, err := StartDirectContext(t.Context(), Config{
		Name: "hunt-rewait", Provider: ProviderClaude, Model: "fixture-model",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	sawOne := make(chan struct{}, 1)
	sawTwo := make(chan struct{}, 1)
	a.SubscribeEvents(func(ev Event) {
		switch ev.Text {
		case "one ":
			sawOne <- struct{}{}
		case "two ":
			sawTwo <- struct{}{}
		}
	})
	if err := a.Send("count to three"); err != nil {
		t.Fatal(err)
	}
	<-sawOne
	short, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	_, err = a.WaitForResponse(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fixture: first wait = %v, want deadline", err)
	}
	close(step1)
	<-sawTwo
	long, cancel2 := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel2()
	go func() { time.Sleep(100 * time.Millisecond); close(step2) }()
	got, err := a.WaitForResponse(long)
	if err != nil || got != "one two three" {
		t.Fatalf("re-wait for the same turn = %q, %v; want %q", got, err, "one two three")
	}
}
```

**Evidence.**

- `zz_hunt_rewait_test.go:68: re-wait for the same turn = "three", <nil>; want "one two three"`

**Smallest fix.** Disarm the latch only when a wait returns a result; on a context or abandoned exit, put the collected text back and keep the latch armed unless a newer `beginTurn` has happened.

### H19. Cancelling a brokered Task run mid-stream never crashes the consumer

**Severity: High.** The consumer process dies with `panic: send on closed channel`.

**Mechanism.** The run goroutine calls `client.Close()` and then `close(events)`, but `Close` does not wait for `readLoop`, which keeps handing already-buffered frames to the push callback; its `select` then sends on the closed channel.

**Where.** broker_task.go:66; broker_client.go:128

**Reproduction.**

`zz_hunt_broker_task_cancel_test.go` (package and helpers in the appendix):

```go
// A brokered Task run is cancelled while the daemon is still streaming
// task_event frames. The run goroutine in brokerTaskBackend.RunTask closes
// the client and then the events channel, but brokerClient.Close does not
// join readLoop: frames already in the bufio buffer are still handed to the
// push callback, whose select may pick `events <- ev` on the now-closed
// channel. That is a "send on closed channel" panic in the consumer process.
func TestHuntBrokerTaskCancelMidStreamDoesNotPanic(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "hunt-bt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "b.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	raw, err := EncodeTaskEventWire(TaskEvent{Type: TaskEventText, Content: "chunk"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func(nc net.Conn) {
				defer nc.Close()
				c := broker.NewConn(nc)
				req, err := c.ReadRequest()
				if err != nil {
					return
				}
				_ = c.WriteResponse(&broker.Response{ID: req.ID, Type: broker.TypeTaskStarted,
					TaskStarted: &broker.TaskStartedResponse{RunID: "r1"}})
				for i := 0; i < 20000; i++ {
					if c.WriteResponse(&broker.Response{Type: broker.TypeTaskEvent,
						TaskEvent: &broker.TaskEventMessage{RunID: "r1", Event: raw}}) != nil {
						return
					}
				}
			}(nc)
		}
	}()

	work := t.TempDir()
	for i := 0; i < 200; i++ {
		bc, err := broker.Dial(sock)
		if err != nil {
			t.Fatal(err)
		}
		client := &brokerClient{conn: bc, pending: map[string]chan *broker.Response{}, done: make(chan struct{})}
		go client.readLoop()
		bb := &brokerTaskBackend{cfg: TaskConfig{Provider: ProviderBedrock}, client: client}
		ctx, cancel := context.WithCancel(t.Context())
		run, err := bb.RunTask(ctx, taskRunRequest{WorkDir: work, Prompt: "x"})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		<-run.events // the stream is flowing
		time.Sleep(time.Millisecond)
		cancel() // consumer gives up mid-stream
		for range run.events {
		}
		<-client.done
	}
}
```

**Evidence.**

- `panic: send on closed channel ... (*brokerTaskBackend).RunTask.func1 broker_task.go:66 ... (*brokerClient).readLoop broker_client.go:128 (reproduced in pueue 286; the panic ends the test binary)`

**Smallest fix.** Close `events` only after `<-b.client.done` (readLoop has exited), or guard the push sends with a done flag under a mutex.

### H20. One malformed sidecar line never kills the handle while the seat keeps running

**Severity: High.** The handle reports the agent dead mid-turn while the sidecar seat keeps writing its turn into a socket nobody reads, a backend running with nobody attached.

**Mechanism.** `omp.Conn.Recv` returns any JSON decode error and `pump` returns on any Recv error. The handle is marked dead, but the pump never closes the connection.

**Where.** omp/sidecar.go:347-356; omp_agent.go:798-800

**Reproduction.**

`zz_hunt_malformed_line_test.go` (package and helpers in the appendix):

```go
// One sidecar line that does not decode into omp.Event (here a compaction
// progress line whose context_before is not an integer) makes Conn.Recv
// return an error, and ompControl.pump returns on any Recv error. The handle
// goes dead mid-turn, but the pump never closes the sidecar connection, so
// the sidecar's seat keeps running its turn into a socket nobody reads.
func TestHuntMalformedSidecarLineDoesNotOrphanTheSeat(t *testing.T) {
	h := startHuntSidecar(t, func(h *huntSidecar, msg omp.Message) {
		if msg.Op != omp.OpPrompt {
			return
		}
		h.write(t, map[string]any{"seat": msg.Seat, "type": "text", "text": "partial ", "turn_id": msg.TurnID})
		h.write(t, map[string]any{"seat": msg.Seat, "type": "compacted", "text": "folded", "context_before": 1.5})
		go func() {
			time.Sleep(300 * time.Millisecond)
			h.write(t, map[string]any{"seat": msg.Seat, "type": "text", "text": "answer", "turn_id": msg.TurnID})
			h.write(t, map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": msg.TurnID})
		}()
	})
	a, err := StartDirectContext(t.Context(), Config{
		Name: "hunt-malformed", Provider: ProviderClaude, Model: "fixture-model",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	if err := a.Send("hello"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got, err := a.WaitForResponse(ctx)
	if errors.Is(err, ErrAgentGone) {
		open := true
		select {
		case <-h.eof:
			open = false
		case <-time.After(2 * time.Second):
		}
		t.Fatalf("one malformed sidecar line killed the handle mid-turn (wait: %v; Alive=%v); sidecar connection still open 2s later: %v",
			err, a.Alive(), open)
	}
	if err != nil || got != "partial answer" {
		t.Fatalf("wait = %q, %v", got, err)
	}
}
```

**Evidence.**

- `zz_hunt_malformed_line_test.go:51: one malformed sidecar line killed the handle mid-turn (wait: ... agent died before the turn ended ...); sidecar connection still open 2s later: true`

**Smallest fix.** Log and skip an undecodable line (the framing is intact), and close `c.conn` on every exit from the pump.

### H21. Registry.Descendants never recurses forever on a Parent cycle

**Severity: High.** A stack overflow kills the host process, and because the cycle is persisted the crash repeats on every call; Jevons calls Descendants on its kill and reap paths.

**Mechanism.** `Parent` is never validated (Register, EnsureAgentWithParent, broker grant definitions). `IsAncestor` guards against cycles with a seen set, but `Descendants`' recursive walk has none.

**Where.** registry.go:1001-1016

**Reproduction.**

`zz_hunt_registry_test.go` (package and helpers in the appendix):

```go
// Registry.IsAncestor breaks Parent cycles with a visit set; Descendants
// walks the same links with no guard. Parent is caller-supplied (Register,
// EnsureAgentWithParent, a broker grant's def) and never validated, so one
// cycle in grants.json makes every Descendants call recurse until the
// runtime kills the process. The walk runs in a child process so the
// fatal stack overflow is observed rather than taking the suite down.
func TestHuntDescendantsSurvivesParentCycle(t *testing.T) {
	if os.Getenv("HUNT_DESCENDANTS_CHILD") == "1" {
		debug.SetMaxStack(64 << 20) // fail in well under a second
		reg, err := NewRegistry(filepath.Join(t.TempDir(), "grants.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, def := range []AgentDef{
			{Name: "a", SessionID: "sa", Parent: "b"},
			{Name: "b", SessionID: "sb", Parent: "a"},
		} {
			if err := reg.Register(def); err != nil {
				t.Fatal(err)
			}
		}
		if !reg.IsAncestor("a", "b") {
			t.Fatal("fixture: a should be b's parent")
		}
		t.Logf("descendants of a: %v", reg.Descendants("a"))
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHuntDescendantsSurvivesParentCycle$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "HUNT_DESCENDANTS_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		var head []string
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "runtime:") || strings.HasPrefix(line, "fatal error") ||
				strings.Contains(line, "claudia.(*Registry).Descendants") {
				head = append(head, line)
				if len(head) == 5 {
					break
				}
			}
		}
		t.Fatalf("Descendants on a two-seat Parent cycle killed the process (%v):\n%s", err, strings.Join(head, "\n"))
	}
}
```

**Evidence.**

- `zz_hunt_registry_test.go:55: Descendants on a two-seat Parent cycle killed the process (exit status 2): runtime: goroutine stack exceeds 67108864-byte limit ... fatal error: stack overflow`

**Smallest fix.** Keep a `seen` set in `walk`, seeded with the root, and skip names already visited.

### H22. A stdio MCP server that stops reading stdin never blocks every caller and the host's Close

**Severity: High.** Every seat's requests to that server block past their deadlines, the T934 wedge restart can never run, and `MCPHost.Close` (daemon shutdown) hangs while holding the host lock that every other MCP route needs.

**Mechanism.** `send` writes the request frame to the child's stdin while holding `b.mu`, with no deadline. Once the pipe is full (one large tools/call, or a few hundred timed-out requests to a wedged server), the write blocks forever; every other request, `restartWedged` and `close()` wait on `b.mu`. `MCPHost.Close` calls `b.close()` while holding `h.mu`, which `ServeHTTP` also takes.

**Where.** mcp_host_stdio.go:216-238 (`send`), 101-105 (`close`), 426-449 (`restartWedged`); mcp_host.go:170-176

**Reproduction.**

`zz_hunt_mcp_stdio_test.go` (package and helpers in the appendix):

```go
// A stdio MCP server that has stopped reading its stdin. One request whose
// frame does not fit in the pipe blocks inside send while it holds b.mu;
// every other seat's request to the same server then waits on the mutex
// instead of failing at its own deadline, and close() (also b.mu) can
// never kill the process.
func TestHuntMCPStdioBlockedWriteHoldsEveryCaller(t *testing.T) {
	marker := fmt.Sprintf("hunt-mcp-nonreader-%d", time.Now().UnixNano())
	b := newMCPStdioBackend(MCPServer{Name: "nonreader", Command: "/bin/sh",
		Args: []string{"-c", "sleep 600; true # " + marker}})
	t.Cleanup(func() { _ = exec.Command("pkill", "-f", marker).Run() })

	big := strings.Repeat("x", 256<<10) // larger than any pipe buffer
	go func() {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "t", "arguments": map[string]any{"text": big}}})
		var msg map[string]json.RawMessage
		_ = json.Unmarshal(raw, &msg)
		_, _ = b.roundTrip(context.Background(), raw, msg)
	}()
	time.Sleep(time.Second) // let the big write fill the pipe and block

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	small := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var msg map[string]json.RawMessage
	_ = json.Unmarshal(small, &msg)
	done := make(chan struct{})
	go func() { _, _ = b.roundTrip(ctx, small, msg); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		closed := make(chan struct{})
		go func() { b.close(); close(closed) }()
		select {
		case <-closed:
			t.Fatal("a request with a 500ms deadline is still blocked after 5s: the backend lock is held by a stdin write that will never complete")
		case <-time.After(2 * time.Second):
			t.Fatal("a request with a 500ms deadline is still blocked after 5s, and close() cannot take the lock to kill the process")
		}
	}
}
```

**Evidence.**

- `zz_hunt_mcp_stdio_test.go:52: a request with a 500ms deadline is still blocked after 5s, and close() cannot take the lock to kill the process`

**Smallest fix.** Do the stdin write outside `b.mu` through a per-process writer goroutine fed by a bounded channel (a full channel fails the request at its deadline), so `close` and `restartWedged` can always take the lock and kill the process.

## Medium

### M1. A stale ephemeral TTL timer never reaps the seat that replaced it

**Severity: Medium.** A plumbing seat (`pimp-smoke-*`, `pimp-handoff-*`) that was stopped and granted again is reaped part-way through its own TTL, losing its conversation before the promised reclaim window ends. Jevons grants are not affected.

**Mechanism.** `orphanGen` lives on each `brokerGrant`. Release Stop deletes the struct, and the replacement grant is a fresh struct whose counter climbs back through the same values (grant takes it to 1, detach to 2). The old timer's `(name, gen)` then matches the replacement and `stopEphemeralOrphan` stops it.

**Where.** daemon/daemon.go:731-776 (arm and fire); daemon/daemon.go:987-990 (fresh struct); daemon/daemon.go:1479 (delete)

**Reproduction.**

`daemon/zz_lead_orphan_test.go` (package and helpers in the appendix):

```go
func TestAuditOrphanTimerCannotReapReplacement(t *testing.T) {
	f := newFixture(t)
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost = true, true
	f.bootWith(t, opts)
	const name = "pimp-smoke-audit-orphan"
	decisions := make(chan bool, 2)
	f.d.orphanDecision = func(seat string, stopped bool) {
		if seat == name {
			decisions <- stopped
		}
	}
	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	work := t.TempDir()
	grant := func(id string) {
		t.Helper()
		raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
			Name: name, Parent: claudia.ParentPimp, WorkDir: work, SessionID: id, TermLogPath: "-",
		}})
		if err != nil {
			t.Fatal(err)
		}
		got := rawCall(t, c, &broker.Request{ID: id, Type: broker.TypeGrant,
			Grant: &broker.GrantRequest{Name: name, Def: raw}})
		if got.Type != broker.TypeGranted {
			t.Fatalf("grant %q: %+v", id, got.Error)
		}
	}
	release := func(disposition broker.Disposition) {
		t.Helper()
		got := rawCall(t, c, &broker.Request{ID: "release", Type: broker.TypeRelease,
			Release: &broker.ReleaseRequest{Name: name, Disposition: disposition}})
		if got.Type != broker.TypeReleased {
			t.Fatalf("release %s: %+v", disposition, got.Error)
		}
	}

	grant("first")
	release(broker.DispositionDetach)
	release(broker.DispositionStop)
	f.clock.Advance(claudia.EphemeralGrantTTL / 2)
	grant("replacement")
	release(broker.DispositionDetach)
	f.clock.Advance(claudia.EphemeralGrantTTL / 2)
	select {
	case stopped := <-decisions:
		if stopped || f.d.reg.Def(name) == nil {
			t.Fatal("first grant's TTL stopped its replacement halfway through the replacement's TTL")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first orphan timer did not fire")
	}
}
```

**Evidence.**

- `zz_lead_orphan_test.go:62: first grant's TTL stopped its replacement halfway through the replacement's TTL`

**Smallest fix.** Capture the `*brokerGrant` in the timer goroutine and require `d.grants[name] == g` as well as the generation, or draw generations from one daemon-wide counter.

### M2. A registry change whose save failed never stays in memory to make the retry a no-op

**Severity: Medium.** The caller sees an error, retries, and is told success while nothing reached disk. For `MarkMaterialized` that drops the RequireResume protection at the next restart, which can mint a replacement session; for `EnsureAgent` a restart mints a new SessionID.

**Mechanism.** `registerLocked` stores the new definition and `MarkMaterialized` sets the flag before `save`, and neither rolls back on failure. The retry then finds the change already present (`EnsureAgentWithParent` returns the existing definition; `MarkMaterialized` returns nil because the flag is set) and never saves. `recordSwitch` and `SetSeatPlanPolicy` do roll back, which shows the intent.

**Where.** registry.go:407-425 (`registerLocked`), 723-736 (`MarkMaterialized`), 953-959 (`EnsureAgentWithParent`)

**Reproduction.**

`zz_lead_registry_retry_test.go` (package and helpers in the appendix):

```go
func TestAuditRegistryRetriesActuallyPersist(t *testing.T) {
	for _, name := range []string{"ensure", "materialize"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			path := filepath.Join(dir, "grants.json")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			reg, err := NewRegistry(path)
			if err != nil {
				t.Fatal(err)
			}

			change := func() error {
				_, err := reg.EnsureAgent("worker", dir, "", true)
				return err
			}
			if name == "materialize" {
				if err := change(); err != nil {
					t.Fatal(err)
				}
				change = func() error { return reg.MarkMaterialized("worker") }
			}

			if err := os.Rename(dir, dir+".offline"); err != nil {
				t.Fatal(err)
			}
			if err := change(); err == nil {
				t.Fatal("first change should fail while the state directory is absent")
			}
			if err := os.Rename(dir+".offline", dir); err != nil {
				t.Fatal(err)
			}
			if err := change(); err != nil {
				t.Fatalf("retry failed: %v", err)
			}

			reopened, err := NewRegistry(path)
			if err != nil {
				t.Fatal(err)
			}
			saved := reopened.Def("worker")
			if saved == nil || (name == "materialize" && !saved.Materialized) {
				t.Fatalf("retry reported success, but reloaded definition is %+v", saved)
			}
		})
	}
}
```

**Evidence.**

- `zz_lead_registry_retry_test.go:52: retry reported success, but reloaded definition is <nil>   (ensure)`
- `zz_lead_registry_retry_test.go:52: retry reported success, but reloaded definition is &{Name:worker ... Materialized:false ...}   (materialize)`

**Smallest fix.** Restore the previous map entry (or flag) when `save` fails in `registerLocked` and `MarkMaterialized`, as `recordSwitch` does.

### M3. A provider switch that reports success leaves the definition naming the provider the seat is on

**Severity: Medium.** After a transient save failure a retry reports success while memory and disk still name the old provider; the next launch or broker restart moves the seat back, usually onto the exhausted plan it left.

**Mechanism.** `recordSwitch` reverts `r.agents[name]` when `save` fails, but the live seat stays switched. The retry hits the early return for a seat already on the requested provider and model, returns `Changed:false` with no error, and never records.

**Where.** registry_switch.go:76-80; registry.go:708-711

**Reproduction.**

`zz_hunt_switch_test.go` (package and helpers in the appendix):

```go
// A live seat that switched provider in place must end with a definition that
// names the provider it is actually on, once the switch is reported done —
// including when the first attempt to record it failed and the caller retried.
// Otherwise the next launch (a broker restart) silently moves the seat back.
func TestHuntRegistrySwitchRetryAfterFailedRecordPersists(t *testing.T) {
	startFakeSidecar(t, nil)
	reg, path := switchRegistry(t)
	if err := reg.Register(AgentDef{Name: "w", Provider: Provider(omp.Anthropic), Model: "claude-opus", WorkDir: t.TempDir(), SessionID: "s-1"}); err != nil {
		t.Fatal(err)
	}
	proc, err := reg.Launch("w")
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Stop("w")
	if err := proc.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil { // the state directory is briefly unwritable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, first := reg.SwitchModel(context.Background(), "w", ProviderCodex, "gpt-5.5", false)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Fatal("fixture: the first switch should fail to record while the directory is read-only")
	}
	retry, err := reg.SwitchModel(context.Background(), "w", ProviderCodex, "gpt-5.5", false)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	reread, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	disk, mem := reread.Def("w"), reg.Def("w")
	if disk.Provider != Provider(omp.OpenAICodex) || mem.Provider != Provider(omp.OpenAICodex) {
		t.Fatalf("first attempt: %v\nretry reported success %+v, live seat runs %s/%s, but the definition says %s/%s in memory and %s/%s on disk: the next launch puts the seat back",
			first, retry, proc.Provider(), proc.Model(), mem.Provider, mem.Model, disk.Provider, disk.Model)
	}
}
```

**Evidence.**

- `zz_hunt_switch_test.go:55: first attempt: switched to openai-codex/gpt-5.5, but recording the seat failed: persist switched agent "w": open .../.agents-...: permission denied ... retry reported success {Live:true Changed:false ...}, live seat runs openai-codex/gpt-5.5, but the definition says anthropic/claude-opus in memory and anthropic/claude-opus on disk`

**Smallest fix.** In the early-return branch, when the definition's provider or model differs from the live seat's, return `r.recordSwitch(name, proc)`.

### M4. A wait that starts after a context-overflow refusal still returns ErrContextOverflow

**Severity: Medium.** A caller that compacts on `errors.Is(err, ErrContextOverflow)` gets a plain error instead, so the seat keeps failing every turn. Overflow refusals come back fast, so a late wait is the common case after a blocking Send.

**Mechanism.** A wait that subscribes after the turn ended is seeded from the latch, and `recordTurnEventLocked` builds the error with `errors.New(msg)`. The live subscriber in `WaitForResponse` wraps `ErrContextOverflow` when `ev.Reason == ReasonContextOverflow`; the comment on the latch says the two must agree.

**Where.** turn_latch.go:113-122 versus agent.go:1825-1826

**Reproduction.**

`zz_lead_turn_overflow_test.go` (package and helpers in the appendix):

```go
func TestAuditLateWaitPreservesContextOverflow(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	startFakeSidecar(t, []string{
		"{\"type\":\"turn_end\",\"error\":\"input does not fit\",\"reason\":\"context_overflow\"}",
	})
	a, err := StartDirectContext(t.Context(), Config{
		Name: "audit", Provider: ProviderClaude, Model: "fixture-model",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	published := make(chan Event, 1)
	a.SubscribeEvents(func(ev Event) {
		if ev.IsError {
			published <- ev
		}
	})
	if err := a.Send("hello"); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-published:
		if ev.Reason != ReasonContextOverflow {
			t.Fatalf("fixture did not publish overflow: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal event missing")
	}
	_, err = a.WaitForResponse(t.Context())
	if !errors.Is(err, ErrContextOverflow) {
		t.Fatalf("late wait lost ErrContextOverflow: %T %v", err, err)
	}
}
```

**Evidence.**

- `zz_lead_turn_overflow_test.go:41: late wait lost ErrContextOverflow: *errors.errorString provider refused the turn: input does not fit`

**Smallest fix.** In `recordTurnEventLocked`, wrap `ErrContextOverflow` when `ev.Reason == ReasonContextOverflow`, exactly as the live subscriber does.

### M5. A context-overflow refusal is never classified as a rate limit

**Severity: Medium.** Consumers making failover decisions see a false rate limit, and the plan-usage cache is invalidated for nothing.

**Mechanism.** `classifyStuckEvent` matches the bare substring `429`, so `prompt is too long: 204291 tokens > 200000 maximum` (and a request id such as `7f3a4290`) is published as a `rate_limit` stuck event, which also triggers `invalidatePlanUsage()`.

**Where.** stuck.go:46; agent.go:1643-1651

**Reproduction.**

`zz_hunt_stuck_overflow_test.go` (package and helpers in the appendix):

```go
// Anthropic's context-window refusal names the token count:
// "prompt is too long: 204291 tokens > 200000 maximum". classifyStuckEvent
// matches the bare substring "429" anywhere in the error text, so a count
// that happens to contain those digits makes a context overflow a
// rate_limit stuck event (and invalidates every plan-usage snapshot).
func TestHuntContextOverflowIsNotClassifiedRateLimit(t *testing.T) {
	const refusal = "prompt is too long: 204291 tokens > 200000 maximum"
	startHuntSidecar(t, func(h *huntSidecar, msg omp.Message) {
		if msg.Op == omp.OpPrompt {
			h.write(t, map[string]any{"seat": msg.Seat, "type": "turn_end", "turn_id": msg.TurnID,
				"error": refusal, "reason": "context_overflow"})
		}
	})
	a, err := StartDirectContext(t.Context(), Config{
		Name: "hunt-overflow", Provider: ProviderClaude, Model: "fixture-model",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	stuck := make(chan Event, 4)
	a.SubscribeEvents(func(ev Event) {
		if ev.ProgressType == ProgressStuck {
			stuck <- ev
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.WaitForResponse(ctx); done <- err }()
	time.Sleep(50 * time.Millisecond)
	if err := a.Send("hello"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrContextOverflow) {
		t.Fatalf("fixture: wait = %v, want ErrContextOverflow", err)
	}
	select {
	case ev := <-stuck:
		t.Fatalf("context overflow %q was published as a %q stuck event", refusal, ev.StuckClass)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestHuntClassifyStuckDigits(t *testing.T) {
	for _, text := range []string{
		"provider refused the turn: prompt is too long: 204291 tokens > 200000 maximum",
		"provider refused the turn: request 7f3a4290 failed: overloaded_error",
	} {
		if class, _ := classifyStuckEvent(Event{IsError: true, Text: text}); class == StuckClassRateLimit {
			t.Errorf("classifyStuckEvent(%q) = rate_limit; the text contains no rate limit, only the digits 429", text)
		}
	}
}
```

**Evidence.**

- `zz_hunt_stuck_overflow_test.go:52: context overflow "prompt is too long: 204291 tokens > 200000 maximum" was published as a "rate_limit" stuck event`
- `zz_hunt_stuck_overflow_test.go:63: classifyStuckEvent("provider refused the turn: request 7f3a4290 failed: overloaded_error") = rate_limit; the text contains no rate limit, only the digits 429`

**Smallest fix.** Skip classification when `ev.Reason == ReasonContextOverflow`, and match 429 only as a whole token or a status field.

### M6. A transient policy hold lifted before boot resume never keeps the seat stood down

**Severity: Medium.** An AutoStart seat that happened to be policy-held when the daemon was constructed is not resumed on this boot or any later one until a consumer grants it, contrary to the comment that only the fresh read gates a launch.

**Mechanism.** `mergeConsumerIntentStandDown` runs at construction and records every held name, transient policy holds included, as `StoodDown` in claims.json. The resume `Select` re-reads both files fresh, but then calls `resumeOrder`, which skips any name with a non-zero `StoodDown`; only a consumer grant clears it.

**Where.** daemon/consumer_intent.go:24-43; daemon/claims.go:138-152 and 186-189

**Reproduction.**

`daemon/zz_lead_hold_test.go` (package and helpers in the appendix):

```go
func TestAuditBootResumeObservesLiftedTransientHold(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	root := t.TempDir()
	const name = "audit-worker"
	writePolicyHold(t, root, map[string]string{name: claudia.PolicyHoldReasonFrontierConsumeDisabled})
	writeGrantsTable(t, f.state, autoStart(t, name, "always-eligible"))

	gate := make(chan struct{})
	opts := f.options(nil)
	opts.ConsumerStateDir = root
	opts.DisableMCPHost, opts.RestartNudge, opts.ResumeConcurrency = true, "-", 1
	opts.resumeGate = gate
	f.bootWith(t, opts)

	writePolicyHold(t, root, map[string]string{})
	close(gate)
	<-f.d.resumeDone
	if got := starts(); !slices.Equal(got, []string{"always-eligible", name}) {
		t.Fatalf("start order %v after transient hold was lifted, want both seats", got)
	}
}
```

**Evidence.**

- `zz_lead_hold_test.go:29: start order [always-eligible] after transient hold was lifted, want both seats`

**Smallest fix.** Do not persist stand-downs from the policy-hold file at construction (keep that early record to `fleet/intent.json`), or let the fresh snapshot in `Select` clear `StoodDown` for names neither file holds any more.

### M7. The broker never reports a committed compaction as failed

**Severity: Medium.** A caller told `context canceled` may compact again and fold away more of the conversation than intended.

**Mechanism.** The worker goroutine sends the result and then runs its deferred `cancel()`. The reply goroutine selects on the result channel and `ctx.Done()`; if it is scheduled after both are ready it picks either, and replies `agent_failed: context canceled` for a committed result. At the 15-minute deadline it also replies before the library's cancel settlement (`CompactSettleTimeout`) has decided whether the fold committed.

**Where.** daemon/daemon.go:1638-1663

**Reproduction.**

`daemon/zz_lead_compact_test.go` (package and helpers in the appendix):

```go
func TestAuditBrokerCompactAcknowledgesCommittedResult(t *testing.T) {
	f := newFixture(t)
	commits := make(chan struct{}, 1)
	f.startHook = func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
		return claudia.StartStub(ctx, cfg, &claudia.StubAgentOps{
			Compact: func(context.Context, int) (claudia.SeatCompact, error) {
				commits <- struct{}{}
				return claudia.SeatCompact{TokensBefore: 60, TokensAfter: 40, Committed: true}, nil
			},
		})
	}
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost = true, true
	f.bootWith(t, opts)
	c := rawSeat(t, f.sock, "compact")
	for i := 0; i < 128; i++ {
		got := rawCall(t, c, &broker.Request{ID: "compact", Type: broker.TypeCompact,
			Compact: &broker.CompactRequest{Name: "compact", BoundTokens: 50}})
		select {
		case <-commits:
		default:
			t.Fatal("fake compact was never called")
		}
		if got.Type != broker.TypeCompactResult || got.Compact == nil || !got.Compact.Committed {
			t.Fatalf("attempt %d: backend committed, broker replied %s (%+v)", i, got.Type, got.Error)
		}
	}
}
```

**Evidence.**

- `zz_lead_compact_test.go:35: attempt 4: backend committed, broker replied error (&{Code:agent_failed Message:context canceled ...})`
- The fixture compacts instantly; with a real sidecar the reply goroutine is usually parked before the result arrives, so the race window is narrower than this test makes it.

**Smallest fix.** Use one goroutine: run `proc.Compact(ctx, ...)` and reply with whatever it returns, so the library's own settlement decides the outcome.

### M8. A bound compaction ends promptly when its sidecar dies

**Severity: Medium.** `Registry.Compact` stalls for the full deadline plus 30 seconds and then reports a false `ErrCompactUncertain`, which latches the seat as uncertain.

**Mechanism.** The first wait in `compact` selects only on the reply and the context, not on `c.done`. After the deadline it aborts on a closed connection and waits `CompactSettleTimeout` more before latching `compactUncertain`.

**Where.** omp_agent.go:1209-1221 and 1229-1253

**Reproduction.**

`zz_hunt_compact_dead_test.go` (package and helpers in the appendix):

```go
// TestHuntCompactEndsWhenSidecarDies: the sidecar dies (its connection
// closes) while a bound compaction is waiting for its answer. Compact must
// end when the connection is gone and say the seat is gone, not wait out the
// caller's whole deadline and then CompactSettleTimeout more on a closed
// connection before calling the outcome "uncertain".
func TestHuntCompactEndsWhenSidecarDies(t *testing.T) {
	startHuntSidecar(t, func(h *huntConn, msg omp.Message) {
		switch msg.Op {
		case omp.OpLoad:
			h.write(map[string]any{"seat": msg.Seat, "type": "ready", "how": "launched",
				"provider": msg.Provider, "model": msg.Model, "restored": false})
		case omp.OpCompact:
			_ = h.c.Close() // the sidecar process went away mid-compaction
		}
	})
	a, err := StartDirect(Config{Name: "seat", Provider: ProviderClaude, Model: "fixture-model",
		WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		_, err := a.Compact(ctx, 1000)
		done <- result{err, time.Since(start)}
	}()
	select {
	case r := <-done:
		if errors.Is(r.err, ErrCompactUncertain) {
			t.Fatalf("compact on a dead sidecar reported %v after %s", r.err, r.elapsed)
		}
		if r.elapsed > 2*time.Second {
			t.Fatalf("compact took %s to notice the sidecar was gone: %v", r.elapsed, r.err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("compact still blocked 45s after the sidecar closed")
	}
	if a.Alive() {
		t.Log("note: seat still reports alive")
	}
}
```

**Evidence.**

- `zz_hunt_compact_dead_test.go:49: compact on a dead sidecar reported omp: compact: compact: outcome uncertain after 33.057082917s`

**Smallest fix.** Add `case <-c.done:` to both selects, returning a seat-gone error and leaving `compactUncertain` unset.

### M9. The host plan-usage floor admits one request at a time

**Severity: Medium.** Concurrent callers (the daemon's poller, a CLI refresh, a consumer) all reach the vendor before the floor records anything: the burst shape that put Anthropic's usage endpoint into a multi-hour refusal (T85), after which placement runs without readings.

**Mechanism.** `CheckPlanThrottle` and `RecordPlanAttempt` are separate critical sections, and the attempt is recorded only after the response. Every caller that checks before the first response passes.

**Where.** plan_usage.go:349-356 and 370-378; plan_usage_throttle.go:125-206

**Reproduction.**

`zz_lead_plan_floor_test.go` (package and helpers in the appendix):

```go
func TestAuditSharedPlanFloorAdmitsOneConcurrentAttempt(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{}, 2)
		release := make(chan struct{})
		client := &http.Client{Transport: auditPlanTransport(func(r *http.Request) (*http.Response, error) {
			entered <- struct{}{}
			<-release
			return &http.Response{
				StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(auditUsage)),
			}, nil
		})}
		read := func() {
			_, _ = QueryPlanUsage(context.Background(), &PlanUsageArgs{
				Provider: ProviderClaude, ClaudeAccessToken: "fixture", ClaudeUsageURL: "https://fixture.invalid",
				HTTPClient: client, ThrottleDir: dir, Now: time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC),
			})
		}
		go read()
		<-entered
		go read()
		synctest.Wait()
		admitted := 1 + len(entered)
		close(release)
		if admitted != 1 {
			t.Fatalf("shared floor admitted %d attempts before either response", admitted)
		}
	})
}
```

**Evidence.**

- `zz_lead_plan_floor_test.go:46: shared floor admitted 2 attempts before either response`

**Smallest fix.** Reserve the attempt in the same critical section as the check (write `LastAttempt = now` before the request), and record the outcome afterwards.

### M10. A seat policy that permits no destination never migrates the seat

**Severity: Medium.** Placement moves a seat to a provider the host forbade. The `AllowNone` flag works; the documented empty-slice form does not.

**Mechanism.** `SeatPolicy.Allowed()` returns `append([]Provider(nil), AllowedProviders...)`, which is nil for an empty slice, and `ResolveSeatPlacementForSeat` treats nil as unrestricted. `allowed_providers` is also `omitempty`, so after a reopen even `restricted` reads false.

**Where.** seat_policy.go:37 and 56-64; seat_placement.go:70-73

**Reproduction.**

`zz_hunt_plan_test.go` (package and helpers in the appendix):

```go
// SeatPolicy: an allow-list that is present but empty permits no destination
// (SeatPlacementArgs: "a non-nil empty slice permits none"; Registry
// SetSeatPlanPolicy: "An empty, non-nil allowed slice deliberately permits no
// destination"). A seat whose host permits no destination must park, not move.
func TestHuntSeatPolicyEmptyAllowListPermitsNoDestination(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	args := func() *SeatPlacementArgs {
		return &SeatPlacementArgs{
			CurrentProvider: ProviderClaude, Now: now,
			// Claude's weekly window is spent, so the seat must leave or park.
			Usage: []PlanUsage{huntWeekly(ProviderClaude, 100, now), huntWeekly(ProviderGrok, 40, now)},
		}
	}
	direct := args()
	direct.AllowedProviders = []Provider{}
	if got, err := ResolveSeatPlacement(context.Background(), direct); err != nil || got.Action != SeatPark {
		t.Fatalf("fixture: an empty allow-list passed directly parks; got %+v (%v)", got, err)
	}
	for _, tc := range []struct {
		name   string
		policy SeatPolicy
	}{
		{"allow-none flag", SeatPolicy{AllowNone: true}},
		{"empty allow-list", SeatPolicy{AllowedProviders: []Provider{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st, err := OpenSeatPolicyStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Put("seat", tc.policy); err != nil {
				t.Fatal(err)
			}
			for _, phase := range []string{"stored", "reopened"} {
				if phase == "reopened" {
					if st, err = OpenSeatPolicyStore(dir); err != nil {
						t.Fatal(err)
					}
				}
				_, restricted := st.Get("seat").Allowed()
				got, err := ResolveSeatPlacementForSeat(context.Background(), "seat", st, args())
				if err != nil {
					t.Fatal(err)
				}
				if got.Action != SeatPark {
					t.Errorf("%s: policy permits no destination (Allowed restricted=%v) but placement says %s -> %s (%s)",
						phase, restricted, got.Action, got.Pick.Provider, got.Reason)
				}
			}
		})
	}
}
```

**Evidence.**

- `zz_hunt_plan_test.go:121: stored: policy permits no destination (Allowed restricted=true) but placement says migrate -> grok (...)`
- `zz_hunt_plan_test.go:121: reopened: policy permits no destination (Allowed restricted=false) but placement says migrate -> grok (...)`

**Smallest fix.** Return `slices.Clone(p.AllowedProviders)` (non-nil for an empty slice) from `Allowed()`, and set `AllowNone` in `Update` and `ImportLegacyFile` when `AllowedProviders` is non-nil and empty.

### M11. A seat stopped by a control intent stays stopped at the next daemon boot

**Severity: Medium.** A seat the consumer stopped is relaunched at the next restart. Latent today: Jevons publishes no desired-seat rows yet, so production has no reconcile input.

**Mechanism.** `reconcileSeat` never reads `prev.State`; every granted, desired seat that is not alive is launched. The stop intent's outcome row exists, so the intent is not reapplied. The existing `stop is accepted once` test runs a second pass but checks only the outcome reason, not that the seat stayed down.

**Where.** controlplane/reconcile.go:143-174; controlplane/t1074_9_test.go:276-281

**Reproduction.**

`controlplane/zz_lead_control_stop_test.go` (package and helpers in the appendix):

```go
func TestAuditReconcileKeepsStoppedGenerationDown(t *testing.T) {
	reg, lc := testRegistry(t)
	db := openFileDB(t)
	publishSeat(t, db, "audit", 1, 1, true)
	if err := PublishControlIntent(db, ControlIntent{
		OperationID: "stop-1", Seat: "audit", Generation: 1, Kind: "stop",
	}); err != nil {
		t.Fatal(err)
	}
	if err := Reconcile(t.Context(), db, reg); err != nil {
		t.Fatal(err)
	}
	before := mustStatus(t, db, "audit")
	if before.State != statusStopped || alive(reg, "audit") {
		t.Fatalf("fixture stop failed: %+v", before)
	}
	if err := Reconcile(t.Context(), db, reg); err != nil {
		t.Fatal(err)
	}
	after := mustStatus(t, db, "audit")
	if after.State != statusStopped || alive(reg, "audit") {
		t.Fatalf("same generation + revision: %s -> %s, starts=%d, stop=%+v",
			before.State, after.State, lc.starts(), mustOutcome(t, db, "stop-1"))
	}
}
```

**Evidence.**

- `zz_lead_control_stop_test.go:26: same generation + revision: stopped -> running, starts=2, stop={OperationID:stop-1 Seat:audit Generation:1 Outcome:accepted Reason:stopped}`

**Smallest fix.** In `reconcileSeat`, when `prev.State == statusStopped` and the generation is unchanged, publish the status unchanged and do not launch.

### M12. Stopping a sidecar seat from inside its own event callback never deadlocks

**Severity: Medium.** A consumer that stops a seat in reaction to an event (an auth failure, an exhausted plan) hangs that goroutine forever, and the seat is never removed from `ompSeats`. No internal subscriber does this; the API does not forbid it.

**Mechanism.** The pump goroutine calls subscribers synchronously. `Stop` reaches `shutdownOMPSeat`, which waits on `c.done`, closed only when the pump returns, so a Stop issued from the pump's own callback waits for itself.

**Where.** omp_agent.go:790-794 (pump) and 1914-1926 (`shutdownOMPSeat`, `waitPumpStopped`)

**Reproduction.**

`zz_lead_omp_callback_stop_test.go` (package and helpers in the appendix):

```go
func TestAuditOMPSubscriberCanStopItsAgent(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp")
	startFakeSidecar(t, []string{"{\"type\":\"text\",\"text\":\"hello\"}"})
	a, err := StartDirectContext(t.Context(), Config{
		Name: "audit", Provider: ProviderClaude, Model: "fixture-model",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctrl := a.tmuxCtrl.(*ompControl)
	t.Cleanup(func() {
		ctrl.closePumpDone() // release the self-join if the assertion fails
		a.Stop()
	})
	entered, stopped := make(chan struct{}), make(chan struct{})
	a.SubscribeEvents(func(ev Event) {
		if ev.Type == "assistant" && ev.Text == "hello" {
			close(entered)
			a.Stop()
			close(stopped)
		}
	})
	if err := a.Send("hello"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("fake answer never reached its subscriber")
	}
	select {
	case <-stopped:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop is blocked inside its own delivery callback")
	}
}
```

**Evidence.**

- `zz_lead_omp_callback_stop_test.go:42: Stop is blocked inside its own delivery callback`

**Smallest fix.** Mark the pump's delivery (an atomic set around subscriber calls) and, when Stop runs during it, finish the join on a separate goroutine; or document on `SubscribeEvents` that callbacks must not call Stop.

### M13. Killing a stdio MCP server stops a server started through a wrapper

**Severity: Medium.** A wedged server behind `npx`, `uvx` or a shell script survives the T934 restart and host shutdown as an orphan nobody stops; each restart can leak another.

**Mechanism.** The backend starts the command with no process group, and `kill()` sends SIGKILL to the direct child only. A grandchild that is not reading stdin (the wedged case the restart exists for) never sees EOF and keeps running, reparented to launchd. `fleet_check_unix.go` already uses `Setpgid` and `kill(-pid)` for the same problem.

**Where.** mcp_host_stdio.go:261 (spawn) and 323-331 (`kill`)

**Reproduction.**

`zz_hunt_mcp_stdio_test.go` (package and helpers in the appendix):

```go
// kill() signals only the direct child. A server started through a wrapper
// (npx, uvx, a shell script) that is wedged and not reading stdin survives
// close/restartWedged as an orphan nobody will ever reap or stop.
func TestHuntMCPStdioKillReachesGrandchild(t *testing.T) {
	marker := fmt.Sprintf("hunt-mcp-wrapper-%d", time.Now().UnixNano())
	b := newMCPStdioBackend(MCPServer{Name: "wrapped", Command: "/bin/sh",
		Args: []string{"-c", "sleep 600; true # " + marker}})
	t.Cleanup(b.close)

	note := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	var msg map[string]json.RawMessage
	_ = json.Unmarshal(note, &msg)
	if _, err := b.roundTrip(context.Background(), note, msg); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	shPID := b.proc.cmd.Process.Pid
	b.mu.Unlock()
	var child int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && child == 0; time.Sleep(50 * time.Millisecond) {
		out, _ := exec.Command("pgrep", "-P", strconv.Itoa(shPID)).Output()
		child, _ = strconv.Atoi(strings.TrimSpace(string(out)))
	}
	if child == 0 {
		t.Fatal("wrapper never started its server")
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })

	b.close()
	time.Sleep(time.Second)
	if err := syscall.Kill(child, 0); err == nil {
		t.Fatalf("server process %d (child of killed wrapper %d) is still running after close()", child, shPID)
	}
}
```

**Evidence.**

- `zz_hunt_mcp_stdio_test.go:88: server process 27352 (child of killed wrapper 27347) is still running after close()`

**Smallest fix.** Set `SysProcAttr{Setpgid: true}` in `ensureLocked` and kill `-pid` in `kill()`, as `setCheckProcessGroup` does.

### M14. A stopped sidecar seat never reports Alive

**Severity: Medium.** A health check right after `Stop` gets a stale true.

**Mechanism.** The pump's defers close `done`, which `Stop` waits on, before `bytes`, and `markDead` runs only in the goroutine ranging over `bytes`. `Stop` therefore returns while `alive` is still true.

**Where.** omp_agent.go:790-794; agent.go:926-937

**Reproduction.**

`zz_hunt_stop_alive_test.go` (package and helpers in the appendix):

```go
// TestHuntStoppedSeatNeverReportsAlive: once Stop has returned, the seat must
// not report Alive. The sidecar seat's death is published by a goroutine that
// ranges over ompControl.bytes, which the pump closes only after the done
// channel Stop waits on.
func TestHuntStoppedSeatNeverReportsAlive(t *testing.T) {
	const rounds = 60
	aliveAfterStop := 0
	var longest time.Duration
	for i := 0; i < rounds; i++ {
		startFakeSidecar(t, []string{`{"type":"turn_end"}`})
		a, err := StartDirect(Config{Name: "seat", Provider: ProviderClaude, Model: "fixture-model",
			WorkDir: t.TempDir(), TermLogPath: "-"})
		if err != nil {
			t.Fatal(err)
		}
		a.Stop()
		stopped := time.Now()
		if a.Alive() {
			aliveAfterStop++
			for a.Alive() && time.Since(stopped) < 2*time.Second {
				time.Sleep(50 * time.Microsecond)
			}
			if d := time.Since(stopped); d > longest {
				longest = d
			}
		}
	}
	if aliveAfterStop > 0 {
		t.Fatalf("Alive() was true right after Stop() returned in %d of %d rounds (longest stale window %s)",
			aliveAfterStop, rounds, longest)
	}
}
```

`zz_hunt_alive_after_stop_test.go` (package and helpers in the appendix):

```go
// Stop on a sidecar seat closes the connection and joins the event pump,
// but alive is cleared later, by the goroutine draining Control.Bytes in
// startWithBackendContext. A caller that checks Alive right after Stop
// returns can be told the stopped seat is alive.
func TestHuntAliveIsFalseOnceStopReturns(t *testing.T) {
	startHuntSidecar(t, func(*huntSidecar, omp.Message) {})
	a, err := StartDirectContext(t.Context(), Config{
		Name: "hunt-alive", Provider: ProviderClaude, Model: "fixture-model",
		WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Stop()
	if a.Alive() {
		t.Fatal("Alive() reported true after Stop returned")
	}
	if err := a.Send("after stop"); err == nil {
		t.Fatal("Send after Stop returned nil")
	}
}
```

**Evidence.**

- `zz_hunt_stop_alive_test.go:36: Alive() was true right after Stop() returned in 57 of 60 rounds (longest stale window 122.292µs)`
- `zz_hunt_alive_after_stop_test.go:24: Alive() reported true after Stop returned`

**Smallest fix.** Call `a.markDead()` in `Agent.Stop` after `ops.stop` returns.

### M15. The daemon reads a grant's owner and process under its lock

**Severity: Medium.** A data race; the ownership check for Send, Interrupt and the other seat operations can pass on a stale owner.

**Mechanism.** `seatFor` takes `d.mu` to look up the grant and then reads `g.owner` and `g.proc` after unlocking, while `detachLocked` writes them under the lock.

**Where.** daemon/daemon.go:1566-1580

**Reproduction.**

`daemon/zz_hunt_race_test.go` (package and helpers in the appendix):

```go
// TestHuntSeatForOwnerReadIsSynchronised: seatFor reads g.owner and g.proc
// after releasing d.mu, while handleRelease (an operator's force detach),
// ConnClosed, the forwarder and handleGrant write them under d.mu. Run with
// -race: the detector reports the unsynchronised read.
func TestHuntSeatForOwnerReadIsSynchronised(t *testing.T) {
	const name = "racy"
	f := newFixture(t)
	opts := f.options(nil)
	opts.DisableResume, opts.DisableMCPHost = true, true
	f.bootWith(t, opts)

	owner := rawSeat(t, f.sock, name)
	raw, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: *f.d.reg.Def(name)})
	if err != nil {
		t.Fatal(err)
	}
	op := dialRaw(t, f.sock)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // operator: force-detach over and over
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = op.WriteRequest(&broker.Request{ID: "d", Type: broker.TypeRelease,
				Release: &broker.ReleaseRequest{Name: name, Disposition: broker.DispositionDetach, Force: true}})
			_, _ = huntAwait(op, "d", "", 5*time.Second)
		}
	}()
	// owner: info + re-claim, as opCall does on not_owner
	for i := 0; i < 200; i++ {
		_ = owner.WriteRequest(&broker.Request{ID: "i", Type: broker.TypeInterrupt, Interrupt: &broker.NamedRequest{Name: name}})
		_, _ = huntAwait(owner, "i", "", 5*time.Second)
		_ = owner.WriteRequest(&broker.Request{ID: "g", Type: broker.TypeGrant,
			Grant: &broker.GrantRequest{Name: name, Def: raw, Adopt: true, Fallback: true}})
		_, _ = huntAwait(owner, "g", "", 5*time.Second)
	}
	close(stop)
	wg.Wait()
}
```

**Evidence.**

- `WARNING: DATA RACE  write at daemon.go:707 (detachLocked, via handleRelease) / previous read at daemon.go:1575 (seatFor, via handleAgentOp)`

**Smallest fix.** Copy `g.owner` and `g.proc` while holding `d.mu`.

### M16. A plan renewal reads a seat's provider under the seat's lock

**Severity: Medium.** A data race on a string header; a torn read can match the wrong plan.

**Mechanism.** `reloadOMPSeats` reads `c.provider` without a lock while `switchModel` writes it under `c.mu`; `recoverFailedToken` and `refreshRejectedToken` also read it unlocked.

**Where.** omp_agent.go:1397 (read) versus 1309 (write); also 1088, 1091, 1464

**Reproduction.**

`zz_hunt_provider_race_test.go` (package and helpers in the appendix):

```go
// TestHuntPlanReloadDoesNotRaceSeatSwitch: a plan renewal (reloadOMPSeats,
// run by RunOMPPlanRenewal, RecoverOMPPlan and a refused seat's refresh)
// walks every live seat while a failover moves one of them to another
// provider (switchModel). Both touch ompControl.provider; run with -race.
func TestHuntPlanReloadDoesNotRaceSeatSwitch(t *testing.T) {
	startFakeSidecar(t, []string{`{"type":"turn_end"}`})
	a, err := StartDirect(Config{Name: "seat", Provider: ProviderClaude, Model: "fixture-model",
		WorkDir: t.TempDir(), TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	ctrl := a.tmuxCtrl.(*ompControl)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Lands after the switch below has written the new provider; the
		// sleep orders nothing for the race detector, which is the point.
		time.Sleep(500 * time.Millisecond)
		reloadOMPSeats(context.Background(), "anthropic")
	}()
	if err := ctrl.switchModel(ProviderCodex, "fixture-codex-model", SwitchOptions{}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	wg.Wait()
}
```

**Evidence.**

- `WARNING: DATA RACE  Read ... reloadOMPSeats.func1() omp_agent.go:1397 / Previous write ... (*ompControl).switchModel() omp_agent.go:1309`

**Smallest fix.** Read through `c.currentProvider()` everywhere.

### M17. SessionID, JSONLPath and WindowID never race a broker Rewind repoint

**Severity: Medium.** A data race on fields every caller reads.

**Mechanism.** `brokerAgentBackend.repoint` writes these fields under `a.mu`; the getters read them with no lock.

**Where.** broker_agent.go:482-487; agent.go:1251, 1255, 1263

**Reproduction.**

`zz_hunt_repoint_race_test.go` (package and helpers in the appendix):

```go
// brokerAgentBackend.repoint (the broker Rewind path) rewrites the handle's
// sessionID, jsonlPath and tmuxWindowID under a.mu, but SessionID,
// JSONLPath and WindowID read them with no lock. Any reader running while a
// Rewind completes (publishEvent's stuck path and turnWaitError both call
// a.SessionID()) races the write.
func TestHuntRepointDoesNotRaceSessionID(t *testing.T) {
	b := &brokerAgentBackend{}
	a := &Agent{alive: true}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			b.repoint(a, seatWhere{provider: ProviderClaude, sessionID: "s" + string(rune('a'+i%26)), jsonlPath: "/x", windowID: "@1"})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = a.SessionID()
			_ = a.JSONLPath()
		}
	}()
	wg.Wait()
}
```

**Evidence.**

- `WARNING: DATA RACE  Read ... (*Agent).SessionID() agent.go:1251 / Previous write ... repoint() broker_agent.go:482`

**Smallest fix.** Read the three fields under `a.mu`, or store them atomically.

### M18. Escalation never pins stopped agents in the global ladders map

**Severity: Medium.** Memory grows with seat churn; each entry holds a dead Agent and up to 128 KiB of terminal buffer.

**Mechanism.** `supersedeLadder` stores `*Agent -> chan` in a package-level map and only the multi-rung `climb` goroutine deletes it. A one-rung ladder, or a ladder whose first rung fails, returns and leaves the entry.

**Where.** escalation.go:49, 115, 116, 131

**Reproduction.**

`zz_hunt_ladder_leak_test.go` (package and helpers in the appendix):

```go
// supersedeLadder stores a cancel channel keyed by *Agent in the
// package-global ladders map. Only the multi-rung climb goroutine deletes
// it. A one-rung ladder on a busy seat, or a multi-rung ladder whose first
// rung fails, returns without deleting it, so every such Agent stays
// reachable from a global for the life of the process.
func TestHuntEscalationLadderEntryIsReleased(t *testing.T) {
	before := huntLadderEntries()
	for i := 0; i < 3; i++ {
		a, err := StartStub(t.Context(), Config{TermLogPath: "-"}, &StubAgentOps{
			TurnPhase: func() TurnPhase { return TurnInTurn },
			Steer:     func(string) (DeliveryOutcome, error) { return DeliveryOutcome{}, nil },
			Send:      func(string) error { return errors.New("seat refused") },
		})
		if err != nil {
			t.Fatal(err)
		}
		// One rung: steer the busy seat.
		if _, err := a.SendEscalating("one rung", Escalation{{Mode: DeliverySteer}}); err != nil {
			t.Fatal(err)
		}
		a.Stop()
		b, err := StartStub(t.Context(), Config{TermLogPath: "-"}, &StubAgentOps{
			TurnPhase: func() TurnPhase { return TurnInTurn },
			Send:      func(string) error { return errors.New("seat refused") },
		})
		if err != nil {
			t.Fatal(err)
		}
		// Two rungs, first rung (submit) fails.
		_, _ = b.SendEscalating("two rungs", Escalation{{Mode: DeliverySubmit}, {Mode: DeliveryInterrupt, After: time.Hour}})
		b.Stop()
	}
	if got := huntLadderEntries() - before; got != 0 {
		t.Fatalf("%d stopped agents are still pinned by the global escalation ladder map", got)
	}
}
```

**Evidence.**

- `zz_hunt_ladder_leak_test.go:48: 6 stopped agents are still pinned by the global escalation ladder map`

**Smallest fix.** `defer ladders.CompareAndDelete(a, cancel)` on every synchronous return path.

## Low

### L1. Plan-usage pacing never stalls beyond its floor after the wall clock steps back

**Severity: Low.** The trigger is rare (a manual or NTP step), but for its length no provider is re-polled, and the last reading carries a FetchedAt in the future that placement treats as fresh.

**Mechanism.** The host throttle file and the broker control database store `LastAttempt` and `NextAttemptAt` as wall-clock times compared with `now.Before(...)`, and nothing clamps a stored time that lies in the future. `stalePlanReading` treats a negative age as fresh.

**Where.** plan_usage_throttle.go:139-160; controlplane/plan_usage.go:88; plan_usage_observation.go:69-75 and 102; seat_placement.go:213

**Reproduction.**

`zz_hunt_plan_test.go` (package and helpers in the appendix):

```go
// The host request floor must never withhold a provider for longer than the
// floor itself, measured from the time the caller is asking at.
func TestHuntPlanThrottleNeverWithholdsBeyondFloorAfterClockStepsBack(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	RecordPlanAttempt(dir, ProviderClaude, t0, 200)
	now := t0.Add(-2 * time.Hour) // the wall clock is stepped back two hours
	d := CheckPlanThrottle(dir, ProviderClaude, now, false)
	if !d.Allowed && d.RetryAt.Sub(now) > PlanThrottleMinInterval {
		t.Fatalf("withheld for %s after a clock step, floor is %s: %s", d.RetryAt.Sub(now), PlanThrottleMinInterval, d.Reason)
	}
}
```

`controlplane/zz_hunt_plan_clock_test.go` (package and helpers in the appendix):

```go
// After the wall clock is stepped back, the broker's durable poll schedule must
// not suspend polling for longer than the floor, and the reading routing reads
// must not be presented as newer than the clock.
func TestHuntBrokerPlanPollingSurvivesClockStepBack(t *testing.T) {
	asBroker(t)
	path, socket := brokerDirs(t)
	publishOwned(t, path, socket)
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	clock := claudia.NewManualClock(t0)
	calls := 0
	fetch := func(_ context.Context, provider claudia.Provider) (claudia.PlanUsage, error) {
		if provider != claudia.ProviderClaude {
			return claudia.PlanUsage{Status: claudia.PlanUsageUnavailable, Reason: "unsupported fixture"}, nil
		}
		calls++
		remaining, used := 50.0, 50.0
		reset := clock.Now().Add(3 * 24 * time.Hour)
		return claudia.PlanUsage{Status: claudia.PlanUsageAvailable, HTTPStatus: 200, Windows: []claudia.PlanWindow{{
			Name: claudia.PlanWindowWeekly, RemainingPercent: &remaining, UsedPercent: &used, ResetsAt: &reset, LimitWindow: 7 * 24 * time.Hour}}}, nil
	}
	if _, err := claudia.PollBrokerPlanUsage(t.Context(), clock, fetch); err != nil {
		t.Fatal(err)
	}
	clock.Advance(-2 * time.Hour)                       // NTP/manual step back
	clock.Advance(2 * claudia.PlanThrottleMinInterval) // two floors of real time pass
	if _, err := claudia.PollBrokerPlanUsage(t.Context(), clock, fetch); err != nil {
		t.Fatal(err)
	}
	snap := claudia.ReadBrokerPlanUsage()
	if calls < 2 {
		t.Errorf("polling suspended: %d fetch after %s of elapsed time (floor %s); snapshot FetchedAt %s is %s ahead of the clock",
			calls, 2*claudia.PlanThrottleMinInterval, claudia.PlanThrottleMinInterval,
			snap.FetchedAt.Format(time.RFC3339), snap.FetchedAt.Sub(clock.Now()))
	}
}
```

`controlplane/zz_hunt_plan_test.go` (package and helpers in the appendix):

```go
// ReservePlanUsage refuses a poll while now is before the stored
// NextAttemptAt, and the reservation is durable on purpose (a restart must
// not reset pacing). Nothing bounds NextAttemptAt against now: the poller
// never schedules further ahead than PlanThrottleMaxPenalty, so a stored
// value further away than that can only be a wall-clock step. After the
// clock steps back a day, every provider stays unpolled for that day, and
// the last reading (FetchedAt now in the future) is served as current.
func TestHuntPlanPacingSurvivesBackwardClockStep(t *testing.T) {
	asBroker(t)
	path, socket := brokerDirs(t)
	publishOwned(t, path, socket)
	start := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	calls := 0
	fetch := func(_ context.Context, provider claudia.Provider) (claudia.PlanUsage, error) {
		if provider != claudia.ProviderClaude {
			return claudia.PlanUsage{Status: claudia.PlanUsageUnavailable, Reason: "unsupported fixture"}, nil
		}
		calls++
		remaining, used := 75.0, 25.0
		reset := start.Add(3 * time.Hour)
		return claudia.PlanUsage{Status: claudia.PlanUsageAvailable, HTTPStatus: 200, Windows: []claudia.PlanWindow{{
			Name: claudia.PlanWindowSession, RemainingPercent: &remaining, UsedPercent: &used, ResetsAt: &reset, LimitWindow: 5 * time.Hour,
		}}}, nil
	}
	if _, err := claudia.PollBrokerPlanUsage(t.Context(), claudia.NewManualClock(start), fetch); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("fixture: first poll made %d requests", calls)
	}
	// The wall clock steps back a day (an NTP correction of a clock that had
	// run ahead), then runs for longer than any interval the poller sets.
	stepped := claudia.NewManualClock(start.Add(-24 * time.Hour))
	stepped.Advance(claudia.PlanThrottleMaxPenalty + time.Minute)
	snap, err := claudia.PollBrokerPlanUsage(t.Context(), stepped, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		var fetched time.Time
		for _, u := range snap {
			if u.Provider == claudia.ProviderClaude {
				fetched = u.FetchedAt
			}
		}
		t.Fatalf("%s after a 24h backward clock step no provider was polled (requests=%d); the served Claude reading is stamped %s, %s in the future",
			claudia.PlanThrottleMaxPenalty+time.Minute, calls, fetched.Format(time.RFC3339), fetched.Sub(stepped.Now()))
	}
}
```

**Evidence.**

- `zz_hunt_plan_test.go:138: withheld for 2h5m0s after a clock step, floor is 5m0s: last claude usage request was -2h0m0s ago; ...`
- `zz_hunt_plan_clock_test.go:44: polling suspended: 1 fetch after 10m0s of elapsed time (floor 5m0s); snapshot FetchedAt 2026-11-20T12:00:00Z is 1h50m0s ahead of the clock`
- `zz_hunt_plan_test.go:56: 2h1m0s after a 24h backward clock step no provider was polled (requests=1); the served Claude reading is stamped 2026-10-25T00:00:00Z, 21h59m0s in the future`

**Smallest fix.** Treat a stored attempt time after `now` as `now`, cap `NextAttemptAt` at `now + PlanThrottleMaxPenalty` in `CheckPlanThrottle` and `ReservePlanUsage`, and count a FetchedAt beyond a small tolerance in the future as stale.

### L2. A broker client call honours its context while writing the request

**Severity: Low.** A call can outlive its deadline when the daemon is not reading that connection, which happens whenever the daemon is inside a long synchronous handler on it (a 90-second grant, a release settle, a usage fetch) and the payload exceeds the socket buffer.

**Mechanism.** `brokerClient.call` registers the waiter and calls `conn.WriteRequest` with no deadline and no link to `ctx`; the context is consulted only after the write returns.

**Where.** broker_client.go:217-230; internal/broker/server.go:307-327 (requests are handled on the read goroutine)

**Reproduction.**

`zz_lead_broker_write_test.go` (package and helpers in the appendix):

```go
func TestAuditBrokerCallCancelsAnInProgressWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sender, receiver := net.Pipe()
		defer sender.Close()
		defer receiver.Close()
		client := &brokerClient{
			conn: broker.NewConn(sender), pending: map[string]chan *broker.Response{},
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := client.call(ctx, &broker.Request{Type: broker.TypeGrants, Grants: &broker.GrantsRequest{}})
			result <- err
		}()
		synctest.Wait() // WriteRequest is now blocked in the peer's pipe.
		cancel()
		synctest.Wait()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v, want context.Canceled", err)
			}
		default:
			receiver.Close()
			t.Fatalf("call returned %v only after the peer closed", <-result)
		}
	})
}
```

**Evidence.**

- `zz_lead_broker_write_test.go:38: call returned broker: write 33-byte message: io: read/write on closed pipe only after the peer closed`

**Smallest fix.** Set a write deadline from `ctx` (`SetWriteDeadline`) around the write, or close the connection from a `context.AfterFunc` when the context ends mid-write.

### L3. The registry copies ContextPins in and out

**Severity: Low.** A caller that edits its input slice or a snapshot from `Def` changes the registry, and the edit is persisted by any unrelated save.

**Mechanism.** `cloneAgentDef` clones every slice field except `ContextPins`, and `registryConfig` passes it through uncloned.

**Where.** registry_lifecycle.go:93-109 and 137

**Reproduction.**

`zz_lead_registry_copy_test.go` (package and helpers in the appendix):

```go
func TestAuditRegistryContextPinsAreCopied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grants.json")
	reg, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	pins := []string{"original"}
	if err := reg.Register(AgentDef{Name: "worker", SessionID: "sid", ContextPins: pins}); err != nil {
		t.Fatal(err)
	}
	pins[0] = "caller mutated input"
	if got := reg.Def("worker").ContextPins[0]; got != "original" {
		t.Errorf("mutating the registration input changed the registry: %q", got)
	}
	snapshot := reg.Def("worker")
	snapshot.ContextPins[0] = "caller mutated snapshot"
	if _, err := reg.EnsureAgent("unrelated", "", "", false); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Def("worker").ContextPins[0]; got != "original" {
		t.Fatalf("unrelated save persisted a caller's snapshot edit: %q", got)
	}
}
```

**Evidence.**

- `zz_lead_registry_copy_test.go:20: mutating the registration input changed the registry: "caller mutated input"`
- `zz_lead_registry_copy_test.go:32: unrelated save persisted a caller's snapshot edit: "caller mutated snapshot"`

**Smallest fix.** Add `def.ContextPins = slices.Clone(def.ContextPins)` to `cloneAgentDef`.

### L4. The plan-usage recorder never turns a body read error into a complete body

**Severity: Low.** The fetcher never sees the read error, and the original response body is never closed. A truncated JSON object cannot parse, so the visible effect is a misleading reason rather than a wrong reading.

**Mechanism.** When reading through the limit fails, `planRecordingTransport.RoundTrip` replaces the body with `NopCloser(head)` and returns success, dropping both the error and the original `Close`.

**Where.** plan_usage_record.go:69-74

**Reproduction.**

`zz_lead_plan_read_error_test.go` (package and helpers in the appendix):

```go
func TestAuditPlanReadRetainsFailureAndClosesBody(t *testing.T) {
	b := &auditInterruptedBody{}
	result, err := QueryPlanUsage(t.Context(), &PlanUsageArgs{
		Provider: ProviderClaude, ClaudeAccessToken: "fixture", ClaudeUsageURL: "https://fixture.invalid",
		HTTPClient: &http.Client{Transport: auditResponseTransport{body: b}},
		Now:        time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC),
	})
	if !b.closed || (err == nil && result.Status == PlanUsageAvailable) {
		t.Fatalf("interrupted read status=%s error=%v original body closed=%t", result.Status, err, b.closed)
	}
}
```

**Evidence.**

- `zz_lead_plan_read_error_test.go:37: interrupted read status=available error=<nil> original body closed=false`

**Smallest fix.** On a read error, close the original body and return the error from `RoundTrip` (or splice an error-returning reader after `head`).

### L5. RegisterChain never writes outside the chains directory

**Severity: Low.** A session id containing `../` writes a file elsewhere; the only caller is the same user on a 0600 socket.

**Mechanism.** `chainPath` joins the raw chain id, every Start registers its session id as a chain, and a broker grant's SessionID is not validated.

**Where.** chain.go:25-26 and 42; agent.go:922

**Reproduction.**

`zz_hunt_registry_test.go` (package and helpers in the appendix):

```go
// RegisterChain builds its file name from the chain id with no check, and
// every Start registers the seat's session id as a chain. A session id is
// caller-supplied (a broker grant's def carries it verbatim), so a "../" id
// writes outside the chains directory. exclusiveCodexHomeKey refuses the
// same input for the same reason.
func TestHuntRegisterChainStaysInChainsDir(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if err := RegisterChain("../../escaped", "sid"); err != nil {
		return // refused: the desired behaviour
	}
	escaped := filepath.Join(state, "escaped.chain")
	if _, err := os.Stat(escaped); err == nil {
		t.Fatalf("RegisterChain wrote %s, outside the chains directory %s", escaped, chainsDir())
	}
}
```

**Evidence.**

- `zz_hunt_registry_test.go:72: RegisterChain wrote .../001/escaped.chain, outside the chains directory .../claudia/chains`

**Smallest fix.** Reject chain ids that are empty, `.`, `..`, or contain a path separator.

### L6. Removing a seat leaves no chain file behind

**Severity: Low.** Unbounded growth of tiny write-only files (nothing reads them: `LookupChain` has no caller in claudia or Jevons).

**Mechanism.** Every Start appends `<session>.chain` and nothing deletes one.

**Where.** agent.go:922; chain.go:36-59

**Reproduction.**

`zz_hunt_registry_test.go` (package and helpers in the appendix):

```go
// Every Start registers its session as a chain file under the state
// directory, and nothing in the module (or in jevons) ever reads chains back
// (LookupChain has no caller) or deletes one. Removing the seat leaves its
// chain file behind, so the directory grows by one file per seat start for
// the life of the host.
func TestHuntRemovedSeatLeavesNoChainFile(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "grants.json"))
	if err != nil {
		t.Fatal(err)
	}
	reg.SetDirect(true)
	reg.SetLaunchers(&RegistryLaunchers{Start: func(ctx context.Context, cfg Config) (*Agent, error) {
		return StartStub(ctx, cfg, &StubAgentOps{})
	}})
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("w%d", i)
		if err := reg.Register(AgentDef{Name: name, WorkDir: t.TempDir(), SessionID: "sid-" + name, TermLogPath: "-"}); err != nil {
			t.Fatal(err)
		}
		if _, err := reg.Launch(name); err != nil {
			t.Fatal(err)
		}
		if err := reg.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(chainsDir())
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("three seats started and removed; %d chain files remain in %s: %v", len(entries), chainsDir(), names)
	}
}
```

**Evidence.**

- `zz_hunt_registry_test.go:110: three seats started and removed; 3 chain files remain in .../claudia/chains: [sid-w0.chain sid-w1.chain sid-w2.chain]`

**Smallest fix.** Stop calling `RegisterChain` from Start until a reader exists, or delete the file in Remove.

## Unconfirmed suspicions

Each was reasoned from the code, and in some cases partly tested, but none has a test that fails because of it.

1. `broker_client.go:262-267; broker_agent.go:61-64`. A brokered Send whose client-side deadline expires after the write is still delivered by the daemon, so a caller that retries runs the turn twice. The comment at broker_agent.go:61-64 acknowledges the timeout. Reasoned only; C9 is the same shape for grants and is confirmed.
2. `daemon/daemon.go:1479-1490 and 769-771`. Release Stop and the orphan reap delete the grant and then remove the registration outside the lock; a grant of the same name arriving in that window could bind to a seat being removed. Not tested: the window needs a hook.
3. `daemon/daemon.go:969-993`. Events published between the registry launch and `bindSeatLocked` reach no subscriber and no ring. It matters if the sidecar seat emits events during start or reattach of an in-flight turn. Not tested.
4. `daemon/daemon.go:1209`. `askOwnerGoalCheck` writes to the owner synchronously from the seat's goal loop: C10's mechanism, confined to one seat. Not tested.
5. `daemon/daemon.go:572, 585, 587`. `handleFleet`, `handleReauth` and `handleRecoveryDetail` goroutines are not counted in `d.wg`, so `Close` can return while they still use a closed kernel or MCP host. Not tested.
6. `daemon/daemon.go:1826-1829`. The task-run relay stops draining the task channel after a write error; whether the producer then blocks depends on it honouring cancellation. Not checked; tasks are mostly removed.
7. `broker_client.go:101-114; internal/broker/wire.go`. Strict response decoding: a newer daemon sending an unknown field or push type fails an older client's whole connection. May be deliberate given the version field.
8. `daemon/fleet.go:202`. `startFleetSeat` subscribes to the process on every start and never unsubscribes; a small leak when the same process is re-adopted. Not tested.
9. `daemon/daemon.go:327 versus 339`. `ReconcileControlFile` launches desired seats before `StartSidecar`'s `StopUnowned` runs, so those seats can land on a leftover sidecar that is killed moments later. It also runs with `context.Background()`, so a wedged load (H7) blocks `New` before the socket listens. Latent: no desired rows exist in production.
10. `controlplane/reconcile.go:164`. `reg.Def(seat).SessionID` dereferences nil if the definition is removed concurrently; unreachable while reconcile runs only at boot.
11. `controlplane/replicate.go:248`. `PublishReceipt` upserts with no ordering check; `CancelStartedReceipts` on sidecar exit turns started into cancelled, which a consumer may read as permission to replay while the sidecar resumes the request. Needs sidecar semantics to confirm.
12. `controlplane/observe.go:198`. In `consumerLinkSlot.begin`, a connection whose peer pid reads as 0 evicts a live consumer. Not tested.
13. `registry_switch.go:129-144`. The stopped-seat `SwitchModel` path ignores `r.lifecycle`: a Launch already in flight starts on the old model while the definition records the new one and the call reports `Changed=true, Live=false`. Not tested.
14. `registry.go:698`. A crash between the sidecar switch and `recordSwitch` leaves the old provider in the definition, as in M3; what adopt then does with the mismatch was not traced.
15. `registry.go:264; registry.go:279; daemon/claims.go:237`. grants.json is written 0644 although MCPServers Env and Headers can carry credentials (it relies on the state directory being 0700), and no rename is followed by a directory fsync, so a power loss can lose an update. Not tested.
16. `registry.go:806-809`. Remove with no live handle never sends `OpDrop` to the sidecar. The sidecar's lifeline ties it to its broker, so the broker deployment looks unexposed. Not tested.
17. `plan_usage_throttle.go:114-118 and 249-253`. Both the throttle file and readings.json are written through a fixed `<path>.tmp` name; two processes writing at once can rename one another's half-written temp file into place, and a corrupt throttle file reads as empty (the floor resets). Reasoned only.
18. `omp_agent.go:558-561; sidecar/server.ts errorEvent`. A sidecar-refused unidentified steer becomes `seat_error` while `ops.steer` returns nil, so the caller is told it was delivered. Needs a seat in a refusing state (`compact_uncertain` or `execution_uncertain`). Not tested.
19. `sidecar/loadswitch.ts; omp_agent.go `loadMessage``. A token reload sent as a load after the sidecar committed a switch, but before the host processed its ready, switches the seat back to the old (possibly exhausted) provider while the host records the new one. Reasoned only; `reloadOMPSeats` normally uses `OpToken`.
20. `omp_agent.go:1303-1314`. A switch that times out after 15 minutes leaves host and sidecar disagreeing; the late `ready` is ignored by the pump. Not tested.
21. `omp/ensure.go:259-271 and 344-362`. `StopUnowned` signals the pid in the pid file, and `StopSidecar` does so without the lock, with no identity check. Tiny windows; not tested.
22. `omp/cred.go Flush/writeKey`. `shot.mu` is held across `security -q -i`; a Keychain ACL prompt on first-key creation would block every `Load`, which is every launch. Not tested (Keychain is out of bounds).
23. `omp_agent.go:1328-1341`. The launch path never sets `NoLogin`, so an unattended broker launch on a plan with no refresh token starts an interactive sign-in. Tests refuse interactive logins, so it could not be exercised.
24. `turn_latch.go:99-136; sidecar/seat.ts:627`. The latch is not turn-scoped for unidentified sends, so late deltas of an aborted turn could join the next answer (related to C3). Not tested.
25. `omp_agent.go:959 versus 1007; daemon/fleet.go fleetSeat.Send`. `setOpenTurn(false)` runs before the `turn_end` is published, so `awaitIdle` (DeliveryInterrupt) and the fleet's Send can read idle and arm the next turn before the aborted end is recorded: C3 through a microsecond window. Not reproduced.
26. `sidecar/server.ts dispatch.catch`. If `run()` throws after an abort, the sidecar could emit an `error` with the same turn id after `turn_end(aborted)`, giving one turn two terminal events. Needs the real agent library; not tested.
27. `goal.go:160 versus 209`. `maybeContinueGoal` sets `goalTimer = nil` without stopping a timer `noteGoalEvent` re-armed concurrently, so a backend without `promptInFlight` could send two continuations. Not reproduced.
28. `omp_agent.go turn caps`. The sidecar seat claims `BusyOnSecondSubmit: reject`, but the sidecar queues a mid-turn prompt. No failure shown.
29. `agent.go:2096`. `SubscribeTerminal` channels are never closed when the agent dies, so a consumer ranging over one blocks forever. The daemon unsubscribes on release; no hang shown.
30. `sidecar/server.ts:227-231`. `write()` swallows `JSON.stringify` exceptions, so an unserialisable snapshot would drop a `turn_end` silently. Not tested.
31. `omp/health.go:60-61; omp/cause.go OneLine`. Truncation at a byte offset can split UTF-8 (cosmetic). `SetOMPAgentPause` has no callers and returns nil when there are no seats.
32. `sidecar/cursorexec.ts:107-121, 164, 170`. Cursor shell commands run without a `signal`, so an aborted, stopped or dropped Cursor turn leaves its command running in the workspace for up to 600 s, concurrent with the next turn. The mock API cannot emit Cursor exec frames; reading only.
33. `sidecar/seat.ts restore after drop+reload`. The reloaded seat restores a store whose tail is the cancelled user message (the old agent's `closeAbortedTail` lands after the reload); in one run its next turn re-sent the held prompt and hung. Same root cause as C4; not isolated.
34. `sidecar/maintain.ts:309, 319; seat.ts:689`. A receipt appended during a deferred-persist switch fold reaches disk, and a later memory-only rollback leaves disk and memory diverged. Reasoned only.
35. `sidecar/server.ts:144, 407, 528 (`setPlanToken`)`. Any load from any connection replaces the plan token for every seat on that plan with no ordering, so a host holding a stale token regresses the whole plan (the T165 churn class). Needs two hosts on one sidecar; not tested.
36. `sidecar/protected.ts:36-46`. Protected-path rules match the model-supplied relative string; an absolute path, `./a/../`, or a symlink made by Bash bypasses directory-qualified patterns. Bash can write those paths anyway, so this is advisory. Not tested.
37. `sidecar/store.ts:125-132; maintain.ts:294-304`. After an `appendFileSync` throw `#persisted` does not advance, so the next persist re-appends; a torn mid-file line makes `load()` throw. The disk-full class; not tested.
38. `agent.go:1651; plan_usage_monitor.go:187; controlplane ReservePlanUsage`. A seat's quota stop invalidates plan usage, but the broker's poll is refused until `NextAttemptAt` (up to 5 minutes), so `pick=remaining` and `Resolve` keep choosing the provider whose seat just hit its limit. Comments say no path bypasses the schedule on purpose. Not tested.
39. `stuck.go:40`. Any blob containing `auth` without `rate` (for example `xai-oauth` or `authorization`) suppresses the quota and rate-limit classes. Sidecar events do not appear to set `Raw`, so reachability is unclear. Not tested.
40. `plan_usage_grok.go:93-98`. Every Grok period is labelled weekly with a fixed 7-day window whatever `currentPeriod.type` says; a period ending more than 7 days out reads as 0 elapsed and anything at or above 46% used classifies hot. No payload evidence.
41. `plan_usage_codex.go:76-77`. `rate_limit.limit_reached` and `allowed` are parsed but unused; if the vendor sets them below 100% used, seats keep being admitted. No payload evidence.
42. `seat_placement.go:118`. `ResolveSeatPlacement` takes no seat model, so a seat on a spent per-model window (T86) is always told Stay. An API gap rather than a broken branch.
43. `fleet.go:1521-1535`. `eventLocked` advances seq and caches the event even when the append fails; a partial line glued to the next makes `openLogLocked` refuse the whole log after a restart, and reused seq numbers let a reader miss events. A partial write could not be injected on macOS.
44. `plan_cache.go:149-210`. Waiters never steal a lease whose holder still has the flock; the holder's heartbeat keeps the lease fresh while its fetch hangs on `http.DefaultClient`, which has no timeout, so a caller with no deadline waits forever. Not tested.
45. `fleet.go:781 versus 990-995`. Stop right after a mid-turn death says "still starting", because `run()` drops `k.running` before `revive` sets `k.reviving`. Minor; not tested.
46. `plan_usage_observation.go:110-113`. `PollBrokerPlanUsage` commits a failed observation when the daemon's context is cancelled mid-fetch, so a known-exhausted provider reads as unpublished (not a veto) until the next poll after restart. Not tested.
47. `mcp_host.go:170-176 with mcp_host_stdio.go:252-290`. A request that looked up a stdio backend just before `MCPHost.Close` can reach `ensureLocked` afterwards and start a new server process nobody will close. The window is narrow; not tested.

## Coverage

**Method.** The parent session mapped the repository and verified the previous attempt's 14 leads. Six parallel readers then each took one subsystem, read it line by line and wrote repro tests in their own scratch copies (`git archive` copies of a6fdf67, removed once this report was written; every repro is inline here). The parent re-ran every repro reported here and read the code behind each claim. The evidence lines quoted above come from those parent runs (Pueue tasks 203, 242, 259, 276, 280, 281, 284, 285, 286, 290, 291 and 292, since cleared from the shared queue), with one exception: the timing-dependent third C3 test, whose failure rate is cited from the reader's own measurement. Go tests ran with `-race`, `CLAUDIA_NO_BROKER=1` and the repo's `internal/testhome` isolation. Sidecar tests ran under bun with a private HOME, XDG and TMPDIR, against the repo's own mock-provider fixtures. Nothing contacted a provider, the installed daemon, or the Keychain.

**Disclosure.** One reader ran `ls ~/.local/state/claudia/chains | wc -l` once, to size L6. That read live-state metadata (a count of 21048 files), which the brief forbids. Nothing else under the real home was read or written. Some early runs used `-p 2`/`-p 3` and `GOMAXPROCS=4`, before the brief's two-worker rule was added; every later run used `-p 1 GOMAXPROCS=2`.

**Read in full.**
- Daemon and broker: `daemon/` (daemon.go, fleet.go, claims.go, consumer_intent.go, generations.go, receipt.go, usage_facts.go, legacy_grants.go, auth_recovery_detail.go); `internal/broker/` server.go, transport.go, backpressure.go, socket.go and clock.go; broker_agent.go, broker_client.go, broker_task.go, broker_mode.go, broker_error.go, broker_seats.go and ephemeral_seat.go.
- Sidecar seat (Go): omp_agent.go, omp_pause.go, omp_renew.go, omp_refusal.go, omp_recovery_detail.go, omp_receipt.go and mcp_invocation.go; all of `omp/` except usage.go and cause.go; cursor_reap.go, detach_unix.go, process_alive_unix.go and fake_sidecar_test.go.
- Turn loop: agent.go, turn_wait.go, turn_latch.go, turn_text.go, turn_caps.go, delivery.go, event.go, request_correlation.go, goal.go, compact.go, escalation.go, stuck.go, rewind.go, context_remint.go and stub_agent.go.
- Plan and fleet: plan_usage*.go, plan_band.go, plan_cache.go and plan_fleet.go; fleet.go, fleet_budget.go, fleet_timer.go, fleet_expect.go, fleet_check.go, fleet_log.go, fleet_tools.go and fleet_mcp.go; seat_placement.go, seat_policy.go, switch.go, switch_options.go, registry_switch.go, resolve.go, provider.go, provider_cause.go, usage_facts.go, catalog.go, omp/usage_transition.go and controlplane/plan_usage.go.
- Registry and control plane: registry.go, registry_lifecycle.go, registry_seats.go, registry_rewind.go, legacy_grants.go, generations.go, chain.go, receipt.go, ledger_guard.go, flock_unix.go, consumer_state_file*.go, consumer_intent.go, policy_hold.go, claims_read.go, control_observe.go, control_reconcile.go and mcp_exclusive.go; every file in `controlplane/`.
- MCP: mcp_host.go, mcp_host_stdio.go and mcp_proxy.go (ServeHTTP and auth paths).
- TypeScript sidecar: server.ts, seat.ts, turn.ts, store.ts, journal.ts, receipts.ts, maintain.ts, loadswitch.ts, authretry.ts, hostcalls.ts, seat_command.ts, bound_compact_inflight.ts, singleton.ts, plantokens.ts, transcript-end.ts, readguard.ts, killguard.ts, process.ts, coding.ts, execution.ts, protected.ts, spool.ts, usage.ts, generations.ts, cursorexec.ts, gitdestructguard.ts, branchguard.ts, truncate.ts and auth.ts.

**Read in part.**
- task.go: lines 140-700. The CLI Task transport is removed, so the stream parsers are unreachable.
- internal/broker: wire.go and wire_grants.go, the envelope, grant and event bodies only.
- cmd/claudia/broker_seat.go: `driveSeat`, `reclaimListed` and flag handling.
- cmd/claudia/main.go: the file writers only.
- sidecar/usage-reader.ts: lines 1-80.
- omp/usage.go and omp/cause.go.
- judge.go: the retry loop only.
- mcp.go and mcp_oauth.go: the proxy's use of them only.

**Not reached.**
- The session transports that now only return `ErrLegacyCLISeatTransport` (cursor_acp.go, grok_acp.go, grok_acp_connect.go, grok_spawn.go, codex_session.go, codex_app_server.go, acp_*.go, cursor_task.go). They are dead code on the shipped path.
- bedrock.go, ollama.go, the `codex/` package, grok/realtime.go, tui_preview.go and the model_intel*.go files.
- fleet_client.go, capability.go and cursor_models.go.
- The remaining cmd/ programs.
- The existing sidecar `*.test.ts` files, apart from the fixtures the repros reuse.

## Appendix: the rest of each Go repro file

Each block is what a repro file holds besides the test functions shown above: the package clause, imports and helpers. Imports are as they were in the full file, so a file that holds only some of its tests may need `goimports`. Helpers that a test calls but that do not appear here are existing repo test fixtures (for example `newFixture`, `rawSeat`, `startFakeSidecar`, `startAbortSidecar`, `testRegistry`). The sidecar-seat and turn-loop repros each define their own `startHuntSidecar` with a different signature (they ran in separate scratch copies), so install one set at a time or rename one.

### `controlplane/zz_hunt_plan_clock_test.go`

```go
// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
)
```

### `controlplane/zz_hunt_plan_test.go`

```go
package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
)
```

### `controlplane/zz_lead_control_stop_test.go`

```go
package controlplane

import "testing"
```

### `daemon/zz_hunt_cancel_test.go`

```go
package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
)
```

### `daemon/zz_hunt_cli_reclaim_test.go`

```go
package daemon

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)
```

### `daemon/zz_hunt_grant_test.go`

```go
package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)
```

### `daemon/zz_hunt_order_test.go`

```go
package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// readTitles reads frames from c until the response with id arrives, and
// returns the ToolTitle of every agent_event seen before it, in order.
func huntReadTitles(t *testing.T, c *broker.Conn, id string) []string {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	var titles []string
	for {
		resp, err := c.ReadResponse()
		if err != nil {
			t.Fatalf("read: %v (titles so far %d)", err, len(titles))
		}
		switch {
		case resp.ID == id:
			return append(titles, "<granted>")
		case resp.Type == broker.TypeAgentEvent:
			ev, err := claudia.DecodeEventWire(resp.AgentEvent.Event)
			if err != nil {
				t.Fatal(err)
			}
			titles = append(titles, ev.ToolTitle)
		}
	}
}
```

### `daemon/zz_hunt_protected_test.go`

```go
package daemon

import (
	"testing"

	"github.com/marcelocantos/claudia"
)
```

### `daemon/zz_hunt_pump_tail_test.go`

```go
package daemon

import (
	"fmt"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)
```

### `daemon/zz_hunt_pump_test.go`

```go
package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func huntPublish(proc *claudia.Agent, title string) {
	proc.PublishEvent(claudia.Event{Type: "progress", ProgressType: "tool_use", ToolTitle: title})
}

// huntCollect reads agent_event titles from c until every title in want has
// been seen or 10s pass; the grant reply is recorded as "<granted>".
func huntCollect(t *testing.T, c *broker.Conn, want []string) []string {
	t.Helper()
	need := map[string]bool{}
	for _, w := range want {
		need[w] = true
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	var got []string
	for len(need) > 0 {
		resp, err := c.ReadResponse()
		if err != nil {
			t.Fatalf("read after %v: %v (still missing %v)", got, err, need)
		}
		switch {
		case resp.Type == broker.TypeGranted:
			got = append(got, "<granted>")
		case resp.Type == broker.TypeAgentEvent:
			ev, err := claudia.DecodeEventWire(resp.AgentEvent.Event)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, ev.ToolTitle)
			delete(need, ev.ToolTitle)
		}
	}
	return got
}

func huntEventsOnly(seq []string) string {
	var out []string
	for _, s := range seq {
		if strings.HasPrefix(s, "e") {
			out = append(out, s)
		}
	}
	return strings.Join(out, ",")
}

type huntGrant struct {
	titles  []string
	granted *broker.GrantResponse
}

func rawCallCollect(t *testing.T, c *broker.Conn, req *broker.Request) huntGrant {
	t.Helper()
	if err := c.WriteRequest(req); err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	var out huntGrant
	for {
		resp, err := c.ReadResponse()
		if err != nil {
			t.Fatal(err)
		}
		if resp.ID == req.ID {
			if resp.Granted == nil {
				t.Fatalf("grant answered %s: %+v", resp.Type, resp.Error)
			}
			out.granted = resp.Granted
			return out
		}
		if resp.Type == broker.TypeAgentEvent {
			ev, err := claudia.DecodeEventWire(resp.AgentEvent.Event)
			if err != nil {
				t.Fatal(err)
			}
			out.titles = append(out.titles, ev.ToolTitle)
		}
	}
}
```

### `daemon/zz_hunt_race_test.go`

```go
package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)
```

### `daemon/zz_hunt_replay_crash_test.go`

```go
package daemon

import (
	"fmt"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)
```

### `daemon/zz_hunt_stall_test.go`

```go
package daemon

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func huntStacks() string {
	buf := make([]byte, 1<<22)
	return string(buf[:runtime.Stack(buf, true)])
}

// huntAwait reads c until a response with id (or, with id "", a push of
// type want) arrives, or d passes.
func huntAwait(c *broker.Conn, id string, want broker.MessageType, d time.Duration) (*broker.Response, error) {
	_ = c.SetDeadline(time.Now().Add(d))
	for {
		resp, err := c.ReadResponse()
		if err != nil {
			return nil, err
		}
		if (id != "" && resp.ID == id) || (id == "" && resp.Type == want) {
			return resp, nil
		}
	}
}

func excerpt(s, needle string) string {
	i := strings.Index(s, needle)
	if i < 0 {
		return "(no " + needle + " frame)"
	}
	start := strings.LastIndex(s[:i], "\n\ngoroutine ")
	if start < 0 {
		start = 0
	}
	end := strings.Index(s[i:], "\n\n")
	if end < 0 {
		end = len(s) - i
	}
	return s[start : i+end]
}
```

### `daemon/zz_hunt_two_owner_test.go`

```go
package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)
```

### `daemon/zz_lead_compact_test.go`

```go
package daemon

import (
	"context"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)
```

### `daemon/zz_lead_hold_test.go`

```go
package daemon

import (
	"slices"
	"testing"

	"github.com/marcelocantos/claudia"
)
```

### `daemon/zz_lead_orphan_test.go`

```go
package daemon

import (
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)
```

### `internal/broker/zz_hunt_tail_test.go`

```go
package broker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)
```

### `omp/zz_hunt_put_test.go`

```go
package omp

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)
```

### `zz_hunt_alive_after_stop_test.go`

```go
package claudia

import (
	"testing"

	"github.com/marcelocantos/claudia/omp"
)
```

### `zz_hunt_broker_task_cancel_test.go`

```go
package claudia

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)
```

### `zz_hunt_compact_dead_test.go`

```go
package claudia

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)
```

### `zz_hunt_cross_plan_token_test.go`

```go
package claudia

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)
```

### `zz_hunt_cursor_reap_test.go`

```go
package claudia

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)
```

### `zz_hunt_escalate_test.go`

```go
package claudia

import (
	"context"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)
```

### `zz_hunt_fleet_test.go`

```go
// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"testing"
	"time"
)
```

### `zz_hunt_grantdef_test.go`

```go
package claudia

import (
	"reflect"
	"testing"
)
```

### `zz_hunt_interrupt_send_real_test.go`

```go
package claudia

import (
	"context"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)
```

### `zz_hunt_interrupt_send_test.go`

```go
package claudia

import (
	"context"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)
```

### `zz_hunt_ladder_leak_test.go`

```go
package claudia

import (
	"errors"
	"testing"
	"time"
)

func huntLadderEntries() int {
	n := 0
	ladders.Range(func(any, any) bool { n++; return true })
	return n
}
```

### `zz_hunt_launch_refresh_test.go`

```go
package claudia

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)
```

### `zz_hunt_malformed_line_test.go`

```go
package claudia

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)
```

### `zz_hunt_mcp_stdio_test.go`

```go
package claudia

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)
```

### `zz_hunt_plan_test.go`

```go
// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"testing"
	"time"
)

// huntWeekly is a plan reading with one weekly window, half the week gone.
func huntWeekly(p Provider, used float64, now time.Time) PlanUsage {
	resets := now.Add(3*24*time.Hour + 12*time.Hour)
	return PlanUsage{Provider: p, Status: PlanUsageAvailable, FetchedAt: now,
		Windows: []PlanWindow{{Name: PlanWindowWeekly,
			UsedPercent: floatPtr(used), RemainingPercent: floatPtr(100 - used),
			ResetsAt: &resets, LimitWindow: defaultWeeklyWindow}}}
}
```

### `zz_hunt_provider_race_test.go`

```go
package claudia

import (
	"context"
	"sync"
	"testing"
	"time"
)
```

### `zz_hunt_real_sidecar_test.go`

```go
package claudia

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marcelocantos/claudia/omp"
)
```

### `zz_hunt_registry_test.go`

```go
package claudia

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)
```

### `zz_hunt_repoint_race_test.go`

```go
package claudia

import (
	"sync"
	"testing"
)
```

### `zz_hunt_rewait_test.go`

```go
package claudia

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)
```

### `zz_hunt_steer_idle_test.go`

```go
package claudia

import (
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)
```

### `zz_hunt_stop_alive_test.go`

```go
package claudia

import (
	"testing"
	"time"
)
```

### `zz_hunt_stuck_overflow_test.go`

```go
package claudia

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)
```

### `zz_hunt_switch_test.go`

```go
// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)
```

### `zz_lead_broker_write_test.go`

```go
package claudia

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"

	"github.com/marcelocantos/claudia/internal/broker"
)
```

### `zz_lead_omp_callback_stop_test.go`

```go
package claudia

import (
	"testing"
	"time"
)
```

### `zz_lead_omp_start_test.go`

```go
package claudia

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)
```

### `zz_lead_plan_floor_test.go`

```go
package claudia

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type auditPlanTransport func(*http.Request) (*http.Response, error)

func (f auditPlanTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

const auditUsage = "{\"five_hour\":{\"utilization\":15,\"resets_at\":\"2026-11-20T05:00:00Z\"}}"
```

### `zz_lead_plan_read_error_test.go`

```go
package claudia

import (
	"io"
	"net/http"
	"testing"
	"time"
)

type auditInterruptedBody struct {
	closed bool
}

func (b *auditInterruptedBody) Read(p []byte) (int, error) {
	return copy(p, "{\"five_hour\":{\"utilization\":15,\"resets_at\":\"2026-11-20T05:00:00Z\"}}"), io.ErrUnexpectedEOF
}

func (b *auditInterruptedBody) Close() error {
	b.closed = true
	return nil
}

type auditResponseTransport struct{ body io.ReadCloser }

func (f auditResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: f.body}, nil
}
```

### `zz_lead_registry_copy_test.go`

```go
package claudia

import (
	"path/filepath"
	"testing"
)
```

### `zz_lead_registry_load_test.go`

```go
package claudia

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)
```

### `zz_lead_registry_retry_test.go`

```go
package claudia

import (
	"os"
	"path/filepath"
	"testing"
)
```

### `zz_lead_turn_overflow_test.go`

```go
package claudia

import (
	"errors"
	"testing"
	"time"
)
```

### `zz_hunt_helpers_test.go` (shared helpers for the sidecar-seat repros)

```go
package claudia

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// huntConn is one host connection to a scripted sidecar. write serialises
// lines the way the real server's per-socket write does.
type huntConn struct {
	c  net.Conn
	mu sync.Mutex
}

func (h *huntConn) write(ev map[string]any) {
	b, _ := json.Marshal(ev)
	h.mu.Lock()
	defer h.mu.Unlock()
	_, _ = h.c.Write(append(b, '\n'))
}

// startHuntSidecar serves every connection on a short unix socket, hands each
// decoded host message to handle, points direct starts at it and stubs the
// plan Keychain. It returns the socket path.
func startHuntSidecar(t *testing.T, handle func(h *huntConn, msg omp.Message)) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hunt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			h := &huntConn{c: c}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					var msg omp.Message
					if json.Unmarshal(line, &msg) != nil {
						continue
					}
					handle(h, msg)
				}
			}()
		}
	}()
	t.Setenv(omp.SocketEnv, socket)
	stubOMPPlans(t)
	return socket
}
```

### `zz_hunt_helpers_test.go` (shared helpers for the turn-loop repros)

```go
package claudia

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// huntSidecar is a scripted one-connection sidecar fake. Every message the
// host sends after the load reaches onMsg, which answers through write.
type huntSidecar struct {
	mu  sync.Mutex
	c   net.Conn
	in  []omp.Message
	inC chan omp.Message
	// eof closes when the host side of the connection is closed.
	eof chan struct{}
}

func (h *huntSidecar) write(t *testing.T, v map[string]any) {
	b, err := json.Marshal(v)
	if err != nil {
		t.Errorf("marshal: %v", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, _ = h.c.Write(append(b, '\n'))
}

func startHuntSidecar(t *testing.T, onMsg func(h *huntSidecar, msg omp.Message)) *huntSidecar {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hunt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	h := &huntSidecar{inC: make(chan omp.Message, 64), eof: make(chan struct{})}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		h.mu.Lock()
		h.c = c
		h.mu.Unlock()
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				close(h.eof)
				return
			}
			var msg omp.Message
			_ = json.Unmarshal(line, &msg)
			h.mu.Lock()
			h.in = append(h.in, msg)
			h.mu.Unlock()
			select {
			case h.inC <- msg:
			default:
			}
			if msg.Op == omp.OpLoad && msg.RequestID == "" {
				h.write(t, map[string]any{"type": "ready", "provider": msg.Provider, "how": "launched", "restored": false})
				continue
			}
			if msg.Op == omp.OpToken {
				continue
			}
			onMsg(h, msg)
		}
	}()
	t.Setenv(omp.SocketEnv, socket)
	stubOMPPlans(t)
	return h
}
```

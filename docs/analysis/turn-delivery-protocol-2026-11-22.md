# Turn and delivery protocol across death, compaction, drops and restarts

Deep analysis, 2026-11-22. Branch `filler/analysis-2026-11-22`, cut from
`filler/fix-hunt-findings-2` at `c33e60e` (both reviewed hunt-fix groups,
not yet on `master`). No product code or existing test was changed. Models,
configs, scripts, reproductions and their logs are under
[`turn-delivery-protocol-2026-11-22/`](turn-delivery-protocol-2026-11-22/).

Question: is the turn and delivery protocol correct across process death,
consumer death, compaction, drops and restarts? Three properties were
checked with TLC, and each is answered here:

1. **Every event is delivered to a live consumer exactly once or reported
   lost.** No. Six ways an event is lost without a report reaching the
   application, and one way a live consumer receives the same history twice.
   §3.1, §4.
2. **No prompt is accepted during compaction and then dropped.** Yes, to the
   model's bound, including interrupts, drops and sidecar crashes during
   compaction. But compaction opens a window for the opposite fault: an input
   whose turn is stopped or interrupted during its pre-turn compaction runs
   anyway. And Release Stop leaves the inputs it discards durably accepted.
   §3.2, §4.
3. **No child outlives its owner.** Not in two cases: a background job whose
   command already returned, and every tool process when the sidecar is
   SIGKILLed. Lifeline EOF and SIGTERM now reap what is still in a tracked
   group. §3.3.

🎯T1404's acknowledgement design closes the consumer-death half of the gap,
and per-event sequence numbers alone make every loss detectable. Neither
covers a daemon restart without a durable ring. §5.

## 1. What the system is, from the code

Five kinds of process take part. Arrows are sockets or pipes; every wire is
newline-delimited JSON.

```
consumer (Jevons; library: broker_agent.go, broker_client.go)
   │  unix socket, one connection per grant (internal/broker)
   ▼
daemon  `claudia broker serve` (daemon/daemon.go): Registry of seats,
   │    one brokerGrant per seat (owner, pump, ring, draining)
   │  unix socket, one connection per seat (omp/sidecar.go, ompControl)
   ▼
sidecar  one Bun process (sidecar/server.ts), started with a stdin lifeline
   │    one SeatAgent per seat (seat.ts) on pi-agent-core; durable store
   │    omp-seats/<seat>/<session>.jsonl: messages, compactions, request facts
   ▼
tool process groups (process.ts runCommand: detached, own session)
```

**Event path.** The sidecar seat emits (`server.ts:239`, which also appends a
usage spool row) to the seat's connection. `ompControl.pump`
(`omp_agent.go:802`) maps each line to an `Event` and calls
`Agent.publishEvent` on the daemon-side Agent, whose one subscriber is the
daemon's `forwarder` (`daemon.go:1357`). The forwarder encodes the event and,
under `d.mu`, puts it on the owner's `grantPump` queue (capacity 100,000) or,
with no owner, on the grant's reclaim `ring` (capacity 100,000; overflow
drops the oldest and sets `lag`). A full queue detaches the owner and tells it
(`agent_detached`). The pump goroutine (`runPump`, `:1488`) writes one frame
at a time; a successful `write()` counts as delivery. On the consumer,
`brokerClient.readLoop` hands id-less frames to the grant's `pushQueue`;
`drain` waits for the handle to exist and for the first subscriber (at most
`replayGrace`, 500 ms), then calls `Agent.publishEvent` on the consumer's
Agent, which calls the application's subscribers or, with none, drops it.

**Ownership and reclaim.** A grant has at most one owning connection. A
consumer that dies leaves its seat running, unowned: `ConnClosed` detaches
(`daemon.go:677`), moving the pump's unsent events onto the ring. A grant by
name (`handleGrant`, `:870`) makes a new pump before the replay so live
events queue behind it, writes the ring, then `Granted{Replayed, Lagged}`,
and only then clears the ring and starts the pump (`:1151`). A writer
detached mid-write becomes `g.draining`; the same connection's next grant
waits for it, another connection takes its events. Rings, pumps and grants
are memory only.

**Turns and prompts.** The host mints a turn id per prompt
(`promptMessage`, `omp_agent.go:722`) and sends identified prompts
(`sendIdentified`, `omp_receipt.go:57`). The seat runs one turn at a time
(`runTurn`, `seat.ts:838`); a prompt arriving mid-turn is accepted and queued
and the running turn answers it (`absorbed`). An identified prompt is durably
accepted (`receipts.accept`) before it can run; the pre-model gate marks it
`started` (`:558`); the turn's end settles started rows `completed`,
`failed` or `cancelled`. On load, `started` becomes `unknown` and `accepted`
rows go back on the queue, run only when the next prompt arrives
(`seat.ts:526-545`).

**Compaction.** Three kinds. Bound compaction (the host's `compact` op,
`seat.ts:1018`) runs only between turns and refuses prompts while it runs
(`refuseDuringBoundCompact`, `:827`). Maintenance compaction runs inside a
turn (pre-turn `:665`, mid-turn `:429`, post-turn `:754`); prompts arriving
then are queued onto the turn. A switch-time fold runs inside a `load` that
changes model. `maint.compacting` is true only while `Maintenance.run` holds
its abort controller (`maintain.ts:639`).

**Kill paths.** Release Stop (`handleRelease`, `daemon.go:1627`):
`Agent.Quiesce` sends `{abort, stop_turn}` (`omp_agent.go:575`), waits up to
5 s for the turn to settle, deletes the grant and `RemoveKeepingBackend`s the
seat (the sidecar seat stays loaded). Remove adds `drop`
(`server.ts:523`): `close()` stops the turn, awaits its unwind and seals the
store. `Agent.Stop` sends the stop abort only when a turn is open
(`quiesceForStop`, `omp_agent.go:1930`). The sidecar on lifeline EOF cancels
started receipts, SIGKILLs every tracked tool group and exits
(`server.ts:108`); SIGTERM and SIGINT also kill the groups first
(`:91-101`); SIGKILL runs nothing.

## 2. Invariants

Status key: **violated** (with the reproduction), **model** (holds in the TLC
model to the stated bound), **argument** (holds by reading), **unknown**.

| # | Invariant | Maintained at | How it breaks | Status |
|---|-----------|---------------|---------------|--------|
| I1 | No consumer incarnation's application receives an event twice. | `detachLocked` committed/draining split (`daemon.go:744`); success clears `writing` (`:1522`); ring cleared after the reply (`:1151`). | A pump overflow while the grant's replay is blocked leaves the replay on the ring; the re-claim it asks for replays it again (D3). | violated: TLC `Delivery-D1`, Go repro |
| I2 | Events reach the application in publish order. | Put-back goes in front of the ring (`:1434`); the pump exists before the replay (`:1103`). | Not found; current-mode checks combine order with loss in I3, and every I3 trace was a loss. | argument |
| I3 | An event a live consumer did not receive is still in daemon custody, or the application is told before it receives a later event (or by the end). | Lagged on ring overflow (`:1458`) and replay skips (`:1118`). | Bytes in a dead consumer's socket (🎯T1404); daemon restart (rings are memory); live oversize drop (`:1532`); Lagged never reaches the application; on a re-claim the lagged reply arrives after the replayed events. | violated: TLC `Delivery-D2..D6`, Go repros |
| I4 | A grant reply's Lagged reaches the application. | `StartAgent` logs it (`broker_agent.go:163`); `reclaim` discards the reply (`:242`). | Always. | violated: Go repro, code |
| I5 | A reclaim's replay reaches the subscriber the consumer attaches. | `drain` waits for the first subscriber for `replayGrace` (`broker_agent.go:98`, `:564`). | A subscriber attached more than 500 ms after Start gets none of it; `publishEvent` drops events with no subscriber (`agent.go:1605`). | violated: Go repro |
| I6 | A prompt refused during bound compaction or a switch fold leaves no durable accepted receipt (C5). | Refusal before `receipts.accept` (`seat.ts:930`, `:969`). | Only with the pre-fix order (mutant). | model: `PromptProtocol-P1`, 3 requests, 4 sends; the mutant fails (P0) |
| I7 | A prompt accepted while compaction runs is not dropped: it stays durable, and while its seat is loaded it is queued, running, or done. | Mid-turn compaction queues onto the turn; drain loops (`:733`, `:758`); `pending()` on load. | Not found, with interrupts, drops and SIGKILL/lifeline crashes during compaction. | model: `P1`, `P1b`, 3 requests, 4 sends |
| I8 | An input the host was told had ended (its turn ended aborted, or its own Release Stop discarded it) never starts later. | `stopTurn` empties the queues (`:907`); drop seals the store. | An abort or stop during the turn's pre-turn compaction re-queues the input and the unwind runs it (D1). Release Stop leaves discarded inputs `accepted`; the next sidecar process runs them (D2). | violated: Bun repros, TLC `P2`, `P2b`, `P3` |
| I9 | An acknowledged identified prompt is never stranded: accepted, on an idle loaded seat, with nothing that will run it. | The not-fresh path runs a non-empty queue (`:932-937`). | A same-id retry after Release Stop is acknowledged and never runs (D2). A steer that reaches an idle seat parks (hunt C6). A provider refusal leaves queued inputs waiting for a later prompt (by design, `:954`). | violated: Bun repro, TLC `P4`, `P6`, `P7` |
| I10 | A dropped seat starts no input (C4). | `closed` checked in `runTurn` and the unwind (`:839`, `:881`). | Not found. | model: `P1`, `P1b` |
| I11 | No input starts twice across turns, drops and restarts. | `started` → `unknown` on load; `start` refuses `unknown` (`receipts.ts:140`). | Not found. | model: `P1`, `P1b`, `P5` |
| I12 | No tool process outlives the sidecar that started it. | `killAllGroups` on lifeline EOF, SIGTERM, SIGINT (`server.ts:83-124`). | SIGKILL runs nothing; a group whose leader exited with redirected stdio is forgotten (`process.ts:119`) and its background job survives. | violated: TLC `Ownership-crash`, `-retired`, Bun repro |
| I13 | No tool process outlives `Agent.Stop`, Release Stop or drop of its seat. | The stop abort signals the running command's group (`process.ts:84`). | The same forgotten groups survive; a seat whose Go side believes it idle sends no abort (`omp_agent.go:1931`). | violated (same mechanism as I12) |
| I14 | The sidecar does not outlive the daemon. | Lifeline EOF handler. | Only transiently, until the EOF is handled; eventually always. | model: `Ownership-strict` (transient), `-baseline` (liveness) |
| I15 | A displaced owner is told. | — | `handleGrant` displaces silently (`daemon.go:1062`), hunt H13. | unknown (not re-checked) |

## 3. Models and what TLC found

All runs: TLC 2.19, two workers, seed 1, through Pueue. Each config's first
line declares the expected outcome and `run_tlc.sh` fails on a mismatch; the
final runs matched on every config. Traces, reduced to one line per step,
are in `results/`.

### 3.1 Delivery (`Delivery.tla`)

One seat, one logical consumer whose process may die and be replaced
(incarnations), slow consumers, detach on a full queue, re-claim, daemon
crash. It models `grantPump` exactly: `queue`, `writing` and the writer's
local copy, `started`, `committed`, `closed`, `stolen`, `draining`; the three
lock sections of `handleGrant`; the replay and the granted reply as separate
writes; the kernel socket buffer (bytes a `write()` accepted); the consumer's
push queue before the first reply; `ConnClosed` only after `handleGrant`
returns. Bounds: N ≤ 3 events, pump capacity 1, ring 1–3, socket 1 frame,
2 incarnations, 3–4 grants, at most one daemon crash. `Mode` selects the
current code or one of the two 🎯T1404 designs (§5).

| Config | Checks | Result | What the trace is |
|--------|--------|--------|-------------------|
| D1 current, one consumer, ring 3 | all | **NoDupIncarnation violated**, 19 steps | Event 1 is on the ring; the grant replays it into the buffer and the reply blocks; events 2 and 3 overflow the unstarted pump; detach appends the queue to the uncleared ring; the reply goes out, the commit does not clear (pump changed); `agent_detached` arrives; the re-claim replays 1 again. |
| D2 current, consumer death | all | **InOrderOrReported violated** | Event 1 written into the dead consumer's buffer; the next owner's grant is clean; it receives 2 over the hole. 🎯T1404. |
| D3 current, daemon crash | all | **EndComplete violated** | Events 1–2 on the ring; the daemon dies; the new daemon's grant replays nothing with Lagged=false. |
| D4 current, event 1 oversize | all | **InOrderOrReported violated** | The attached writer drops the frame (`daemon.go:1532`); event 2 arrives with nothing said. |
| D5 current, ring 1, Lagged as coded | all | **InOrderOrReported violated** | The ring overflows, the reply says Lagged, the application is not told. |
| D6 current, ring 1, Lagged counted as told | all | **InOrderOrReported violated** | Same, on a re-claim: the replayed events are delivered as they arrive, before the lagged reply. |
| S1 seq, durable cursor, crash, oversize | all | pass, 416,993 states | |
| S2 seq, volatile cursor | all | **InOrderOrReported violated** | A fresh incarnation cannot judge what its predecessor missed. |
| A1 ack, durable cursor, oversize | all | pass, 181,948 states | |
| A2 ack, volatile cursor | all | **NoDupLogical violated** | Delivered to incarnation 1, unacknowledged at its death, replayed to 2. |
| A3 ack, daemon crash | all | pass, 16,441 states | The loss is reported (epoch), not prevented. |

### 3.2 Prompt admission (`PromptProtocol.tla`)

One sidecar seat: `prompt`, `steer`, the turn taking its root input and its
queue at the pre-model gate, maintenance compaction inside a turn, bound
compaction between turns, interrupt and its unwind, Release Stop, drop with
its closing window, SIGKILL and lifeline crashes, restart, load from the
journal. The host's view of each request is what the wire told it. A step is
one JavaScript handler up to its first real I/O await; the state between a
prompt and its first model call with no compaction is excluded (`Settled`),
since only microtasks separate them. Bounds: 2–3 request ids, 3–4 sends.

| Config | Checks | Result | What the trace is |
|--------|--------|--------|-------------------|
| P0 C5 mutant (accept before refusing) | I6, I7, I10, I11 | **RefusedNotDurable violated**, 4 steps | The known C5 bug; shows the model sees it. |
| P1 compaction, drop, crash | I6, I7, I10, I11 | pass, 54,673 states | |
| P1b as P1 plus interrupts | I6, I7, I10, I11 | pass, 102,593 states | |
| P2 interrupt during pre-turn compaction | I8 | **EndedStaysEnded violated**, 6 steps | prompt(1); pre-turn compaction; abort; the catch re-queues 1; the unwind starts a turn; 1 starts. |
| P2b Release Stop during pre-turn compaction | I8 | **EndedStaysEnded violated**, 5 steps | Same with `stop_turn`. |
| P3 Release Stop, no re-queue, restart | I8 | **EndedStaysEnded violated**, 9 steps | The stopped input stays `accepted`; crash, restart, load re-queue it; the next prompt runs it. |
| P4 Release Stop, then a same-id retry | I9 | **NotStranded violated**, 6 steps | The retry is acknowledged `accepted`; the queue is empty, so nothing runs it. |
| P5 the proposed fix, everything enabled | all | pass, 115,139 states | |
| P6 steer on an idle seat | I9 | **NotStranded violated**, 2 steps | Hunt C6, still present. |
| P7 provider refusal with a queued input | I9 | **NotStranded violated**, 5 steps | By design (`seat.ts:954`). |

### 3.3 Ownership (`Ownership.tla`, from the Codex partials)

Kept unchanged after review (§8): broker, sidecar epochs, one seat, two tool
groups each with a leader and a descendant that stays in the group, leader
exit with and without forgetting the group, lifeline, SIGTERM, SIGKILL, seat
stop, restart. Kill delivery is atomic, so the model is optimistic about
signal races. Bounds: 2 groups, 2 sidecar epochs. Pueue 1537:

| Config | Result | What the trace is |
|--------|--------|-------------------|
| baseline (leaders stay, no SIGKILL; liveness included) | pass, 204 states | Lifeline and SIGTERM reap every tracked group; the EOF is eventually handled. |
| crash (SIGKILL allowed) | **NoToolOutlivesSidecar violated**, 3 steps | Spawn, SIGKILL; the group outlives the sidecar. |
| retain (leaders exit, groups kept) | pass, 348 states | Keeping the group until it is empty would be enough. |
| retired (leaders exit, groups forgotten: the code) | **NoToolOutlivesSidecar violated**, 6 steps | Spawn, fork a background child, the leader exits and is forgotten, the sidecar exits; the child lives. |
| strict (sidecar must never outlive the broker) | **StrictNoChildOutlivesOwner violated**, 3 steps | Between the broker's death and the EOF being handled; transient by design. |

"No child outlives its owner" therefore holds for the lifeline and SIGTERM
paths over everything still in a tracked group, and fails for a SIGKILLed
sidecar and for a background job whose command already returned.

## 4. Confirmed defects

Most severe first. Titles are the desired state. Config and test names in
`turn-delivery-protocol-2026-11-22/` predate this numbering; each defect
names its own files.

### D1. An input whose turn is stopped or interrupted during its pre-turn compaction does not run afterwards

**Severity: high.** Release Stop is how a plan-policy park stops a seat (🎯T239);
here it runs the very input it stopped, on the plan it parked, under a turn
id the host never sent. The host was told the turn ended `aborted`.
Pre-turn compaction runs whenever a long conversation is near its window,
which is the normal state of a busy fleet seat.

**Mechanism.** An idle seat's prompt opens a turn with the prompt as its own
input (`seat.ts:963-964`). `executeTurn` runs the pre-turn compaction first
(`:665-667`); the input is accepted but not started. The stop arrives:
`abortTurn` cancels started receipts (none), sets `cancelled`, aborts the
compaction, closes the digest and schedules the unwind (`:862-904`);
`stopTurn` empties the queues (`:911`). The aborted compaction then throws
into `executeTurn`'s catch, which, because `run()` was never invoked, puts
the input back on the queue (`:768-774`). The unwind finds a queue and runs
it as an `interrupt-deliver` turn with a fresh turn id (`:878-896`). The
input runs and its receipt ends `completed`.

**Reproduction.** `repro/analysis-precompact-stop.test.ts` with the fixture
`repro/analysis-precompact-provider.ts` (real `server.ts`, mock model, 240k
window, first compaction held), Pueue 1528, log
`repro/pueue-1528-precompact-stop.log`. Both variants fail:

```
ANALYSIS-P2 {"stopTurn":true,"endedStop":"aborted","ran":{"turn":"4c6d85aa-…","request":"r1"},"receipt":"completed",…}
ANALYSIS-P2 {"stopTurn":false,"endedStop":"aborted","ran":{"turn":"23629150-…","request":"r1"},"receipt":"completed",…}
```

Model: `PromptProtocol-P2`, `-P2b` (traces in `results/`).

**Smallest fix.** In the catch, re-queue only when the turn was not
cancelled (`if (!runInvoked && accepted && !cancelled)`), and settle that
input `cancelled`. The journal has no `accepted → cancelled` transition
(`#validateTransition`, `receipts.ts:230`), so that transition is part of the fix (see D2).
`PromptProtocol-P5` checks the combined fix.

### D2. An input Release Stop discards is settled cancelled, so neither a retry nor a restart runs it

**Severity: high.** The host released the seat and treats its waiting work
as discarded. The next sidecar process (every daemon restart starts one) runs
it, possibly long after and out of context. A same-id retry, which
`ErrReceiptUncertain` tells hosts to make, is acknowledged and never runs.

**Mechanism.** `stopTurn` empties the agent queues (`seat.ts:911`).
`cancelOpenReceipts` settles only `started` rows (`:552-557`), and
`receipts.finish` skips anything not started (`receipts.ts:168`). The
discarded prompt stays `accepted` in the store. A same-id retry takes the
not-fresh branch, which runs only a non-empty queue (`seat.ts:932-937`). On
the next load `pending()` re-queues every `accepted` row (`receipts.ts:123`,
`seat.ts:529-545`) and the next prompt runs it first.

**Reproduction.** `repro/analysis-stop-discard.test.ts`, Pueue 1492, log
`repro/pueue-1492-stop-discard.log`:

```
ANALYSIS-P1 {"acceptedBeforeStop":"accepted","ranAfterStop":false,"receiptAfterStop":"accepted",
             "retryAck":["accepted","accepted"],"ranOnRetry":false,"ranAfterRestart":true,"startedAfterRestart":true}
```

Model: `PromptProtocol-P3` (restart) and `-P4` (retry).

**Smallest fix.** Allow `accepted → cancelled` in the journal, and have
`stopTurn` settle the identified messages it removes (they are on the agent
queues and in `heldSteering`/`heldFollowUps`; each carries `claudiaRequest`).
A drop that the host follows with a load (`replaceStaleOMPSeat`) wants the
opposite (🎯T1407: the queued prompt runs once on the reloaded seat), so the
settle belongs to `stopTurn` called for Release Stop, not to `close()`.

### D3. One connection receives each event once even when its pump overflows during the grant's replay

**Severity: medium.** A whole ring (up to 100,000 events) delivered twice to
one live consumer. It needs more events than the pump holds (100,000 in
production) to arrive while one replay is blocked on a slow reader; 🎯T125
saw a 1,024-event pump overflow during a transcript replay, so the shape is
real but rare at the current capacity.

**Mechanism.** The grant's pump is created unstarted before the replay
(`daemon.go:1103`). The forwarder detaches a grant whose queue is full
whether or not the pump has started (`:1385`), and `takePumpLocked`
appends the queue to a ring that still holds the replay (`:1422`). The reply
is still written on the same connection, and the commit clears the ring only
when `g.pump` is still this grant's pump (`:1152`), which it no longer is.
The `agent_detached` notice makes the consumer re-claim, and the re-claim
replays the history it has just received.

**Reproduction.** TLC `Delivery-D1`, 19-step trace in
`results/Delivery-D1-current-reclaim-dup.trace.txt`. Go:
`TestAnalysisPumpOverflowDuringReplayReplaysTwice` in
`repro/daemon_analysis_test.go.txt` (pending Pueue 1535).

**Smallest fix.** Count the replay: when the reply was written but the pump
changed, drop the replayed prefix from the front of the ring at the commit
(it is the same slice prefix). Or do not detach for an unstarted pump's
overflow; the grant is already about to replay.

### D4. A consumer that dies with events unread gets them replayed, or the next grant reports Lagged (🎯T1404)

**Severity: high** (already filed as 🎯T1404). Confirmed on this tip end to
end: three events written into the socket of a consumer that read none, the
next owner's grant says `Replayed=0, Lagged=false`. Go:
`TestAnalysisUnreadEventsOfADeadConsumerAreReportedClean` (pending Pueue
1535). Model: `Delivery-D2`. §5 is the design.

### D5. A daemon restart reports the events it lost

**Severity: medium-high.** A daemon restart is routine (launchd, upgrades).
Every ring and pump queue is memory; the new daemon's grant says
`Lagged=false`. The consumer learns the broker restarted
(`ExitCauseBrokerRestarted` / `ExitCauseBrokerLost`, `broker_agent.go:580`)
but not that its seat's history has a hole. Model `Delivery-D3`. The cheap
fix is an epoch (the daemon's boot id) in `Granted`, compared by the library
with the last epoch it saw (`Delivery-A3`, `-S1`). Preventing the loss needs
a durable ring.

### D6. A lagged grant reaches the application

**Severity: medium.** Every daemon-side loss report ends in a log line:
`StartAgent` logs Lagged (`broker_agent.go:163`); `reclaim`, the path a full
queue triggers, discards the whole reply (`:242`), so a lagged re-claim is not
even logged. Nothing in `Event`, `Agent` or an error carries it. On a
re-claim the replayed events are delivered before the reply is read, so even
a library that read Lagged would report it after the events (`Delivery-D6`).
Go: `TestAnalysisLaggedGrantReachesTheApplication` (pending Pueue 1535).
**Fix:** publish one `system` event naming the gap from both `StartAgent` and
`reclaim`; with sequence numbers (§5) the gap is known per event.

### D7. A reclaim's replay reaches a subscriber attached after Start

**Severity: medium-low.** `drain` waits for the first subscriber for 500 ms
(`broker_agent.go:98`, `:564`) and then delivers the replay to nobody;
`publishEvent` drops events with no subscriber (`agent.go:1605`). On this
host at load 200 a 500 ms gap between Start returning and the next statement
is plausible. Go: `TestAnalysisLateSubscriberGetsTheReplay` (pending Pueue
1535). **Fix:** hold the replay until the first subscriber with a count
bound rather than a clock, or take the subscriber in `Config`.

### D8. An oversize live event is reported

**Severity: low.** An attached writer drops a frame over `MaxLineLen` with a
log line (`daemon.go:1532`); the same event in a replay marks the grant Lagged
(`:1118`). `EncodeEventWire` bounds `Raw` and `Text` only
(`boundEventForWire`, `broker_bound.go:153`), so it takes an unbounded field such as `ToolTitle`
over 1 MiB. Model `Delivery-D4`; Go `TestAnalysisLiveOversizeEventIsReported`
(pending Pueue 1535). **Fix:** set a per-grant "dropped" flag the next frame
or grant carries, or bound every string field in `boundEventForWire`.

### D9. A tool process does not outlive its sidecar or its seat's stop

**Severity: medium** (processes left running with nobody attached; this host
pays in load). Two mechanisms, both already named as residue on the branch:
a group is forgotten when its leader's stdio closes (`process.ts:119`), so
`sleep 60 > /dev/null 2>&1 &` from a finished command survives
`killAllGroups`, Release Stop and drop; and a SIGKILLed sidecar runs no
handler while every tool group is its own session. Model `Ownership-retired`,
`-crash`; the Codex reproduction rerun (pending Pueue 1538). **Fix:** keep a
group until `kill(-pgid, 0)` says it is empty, not until its leader closes;
for SIGKILL, record live pgids beside the socket and have the next sidecar
reap them after checking each leader's start time.

## 5. 🎯T1404: the acknowledgement design

Modelled as two designs in `Delivery.tla` (`Mode = "seq"`, `"ack"`):

* **seq**: every `agent_event` carries a per-seat sequence number; `Granted`
  carries the daemon's epoch and the high-water sequence at the replay
  snapshot. The library reports a gap when a sequence skips, when the
  high-water mark is above its cursor and the replay does not cover it, and
  when the epoch changes. No acknowledgement.
* **ack**: seq, plus a cumulative acknowledgement of what the library has
  delivered. The daemon keeps every event written to the owner until the owner
  acknowledges it; a detach merges the unacknowledged events back into the
  ring by sequence; a grant carries the consumer's cursor and the replay
  starts after it.

| Question | seq | ack |
|----------|-----|-----|
| Is every loss reported before a later event, or by the end? | Yes, for a consumer with a baseline (S1 pass), including oversize drops, the lagged-re-claim ordering and a daemon restart (epoch). | Yes (A1, A3 pass). |
| Does a consumer that dies with events unread get them? | No: reported, not recovered. | Yes (A1). This is 🎯T1404's first acceptance line. |
| Does one consumer receive an event twice? | No: the library drops a sequence at or below its cursor (also removes D3). | No per incarnation (A1, A2). Across a consumer restart only if the cursor is durable and stored with the application's effect (A2 fails with a volatile cursor). |
| Does a daemon restart lose events? | Yes, reported. | Yes, reported. Recovering them needs a durable ring or a replayable sidecar spool. |

So acknowledgement closes the consumer-death half of 🎯T1404 and its
duplicate line, and sequence numbers alone give the reporting half for much
less. Neither closes the daemon-restart loss (D5), and exactly-once across
consumer restarts is the application's to finish: the library can only offer
the cursor.

**Wire cost.** Per `agent_event`, `"seq":N` is 10–16 bytes on a frame of a
few hundred, roughly 2–5%. `Granted` gains an epoch (a 16-byte boot id) and a
high-water sequence; `GrantRequest` gains `resume_after`. Acknowledgements are
one small request per batch, `{"type":"ack","ack":{"name":"…","seq":N}}`,
about 60 bytes; coalesced every 256 events or 250 ms they are under 1% of
event bytes and need no reply. Memory: the daemon holds written,
unacknowledged events per owned seat, so the window needs a bound (the ring's
100,000, overflow marking a gap). Compatibility: a consumer opts in on the
grant request; the daemon retains only for consumers that acknowledge, and
old consumers keep today's behaviour. Sequence numbers alone are the same
per-frame cost with no request traffic and no retention.

## 6. Design findings that are not defects

**F1. The request journal cannot record "discarded before it started".**
`receipts.ts` allows `accepted → started → terminal` and nothing else
(`#validateTransition`, `:230`), and `finish` skips rows that are not started
(`:168`). Every path that removes an input before the pre-model gate (Release
Stop, an abort during pre-turn compaction, a drop for removal, a turn ended by
a refusal) therefore leaves an `accepted` row that the next load re-queues.
D1 and D2 are two faces of this. *Change:* add `accepted → cancelled` (and
`accepted → failed` for the refusal case), and settle at each removal point.
*Cost:* one transition rule, a settle call in `stopTurn`, the `executeTurn`
catch and the refusal drain; old stores stay valid because the rule only
widens.

**F2. Every loss report ends in a log.** Lagged (`broker_agent.go:163`), a
discarded re-claim reply (`:242`), skipped oversize frames
(`brokerClient.noteDropped`, whose own comment says the count reaches no
consumer) and the live oversize drop (`daemon.go:1479`) are all log lines.
The application has no way to know its history has a hole. *Change:* one
`system` event (for example `ProgressType: "history_gap"`, with the reason
and, once sequence numbers exist, the range), published by the library
before the next event. *Cost:* a new event type; consumers that ignore it
behave as today.

**F3. Replay delivery is decided by a wall clock.** The 500 ms
`replayGrace` (D7) is a guess about how soon a consumer subscribes, on a
host whose own notes record multi-second scheduling delays at high load.
*Change:* buffer the replay until the first subscriber, bounded by count,
or let `Config` carry the subscriber so it exists before the grant. *Cost:*
small; the bound already exists as the ring capacity.

**F4. Lifecycle messages bypass the pump.** `agent_gone` (`daemon.go:2486`)
and goal checks are written with `owner.Reply` directly, so they can overtake
events still queued for the same seat, and the seat-watch goroutine that
writes `agent_gone` blocks on a consumer that is not reading (hunt C10).
*Change:* send them through the grant's pump as ordered frames. *Cost:* a
frame type in the pump; ordering becomes a property instead of an accident.

**F5. One blocked write stalls a whole connection.** `Conn.writeLine` holds
the connection mutex across `net.Write` with no deadline
(`transport.go`, `writeLine`). A consumer that stops reading blocks every
writer to it: grant replies, goal checks, notices, every seat it owns. The
detach-on-overflow rule only fires after 100,000 queued events. *Change:* a
write deadline that detaches the consumer, as the overflow does. *Cost:* one
deadline and a test; a consumer that pauses longer than the deadline is
detached and re-claims, which it already handles.

**F6. Release Stop and drop share `stopTurn`, but want different things
for the queue.** Drop before a reload wants the queued inputs to survive
(🎯T1407's acceptance); Release Stop wants them gone. Today both clear
memory and leave the journal, which suits neither exactly (D2). *Change:* a
`stopTurn({settle})` flag, settle on Release Stop and on removal, keep on the
replacement drop. *Cost:* one parameter; the Go side already distinguishes
the two (`quiesce` vs `discard`).

**F7. A provider refusal strands queued inputs.** The drain loops stop on a
refusal (`seat.ts:733`, `:758`) and the comment at `:954` says the next
prompt runs them. The host was told each was accepted and its waiter waits
for a turn that will not come until someone else sends (`PromptProtocol-P7`).
*Change:* settle them `failed` with the turn's cause, or run them (they will
likely be refused the same way, which at least answers them). *Cost:* small.

**F8. Tests that pin behaviour rather than check it.** The branch's own
review already notes that `TestSlowConsumerReclaimKeepsEventOrder` passes on
the pre-H12 code. The kill tests in `daemon/pump_kill_test.go` are the better
pattern: `assertUnreadReplayed` takes its expectation from what the dead
consumer actually read, not from the daemon's own counters. The D3 and D4
reproductions here use the same oracle.

## 7. Unconfirmed suspicions

1. **A stolen in-flight event reaches two consumers.** When another
   connection takes a draining writer's events (`daemon.go:1065-1080`) it
   clears `pump.writing`, but the writer goroutine still writes its local
   `raw` (`:1497`, `:1514`). If the wedged consumer is alive and later reads,
   it receives an event the new owner was also replayed. Needs two live
   consumers of one seat; not modelled (one logical consumer) and not run.
2. **`agent_gone` can arrive before the seat's last events** (F4). The
   consumer marks the handle dead and keeps delivering the queued events;
   whether any consumer misreads that order was not checked.
3. **Release Stop drops the stopped turn's last events.** `handleRelease`
   detaches the live owner and deletes the grant (`daemon.go:1683-1689`);
   events still queued in its pump go onto the ring of a deleted grant. The
   owner asked to stop and does not wait for them, so this may be harmless;
   not reproduced.
4. **Whether the daemon catches D1's re-run turn.** After the stop,
   `handleRelease` polls `PromptInFlight` every 10 ms for up to 5 s, and
   `Agent.Stop` sends a second stop only if `inflight` is set. The re-run
   turn's `accepted` arrives right behind the aborted `turn_end`, so the
   daemon either waits out 5 s and discards the seat, or proceeds and closes
   the connection while the turn runs on. Either way the stopped input
   reaches the provider at least for its compaction. Only a live run decides
   which; not run (brief: no live gate).
5. **The window between a prompt and its first model call.** The prompt
   model excludes stops in that state (`Settled`), on the reading that
   nothing awaits real I/O between `agent.prompt` and the pre-model gate. If
   pi-agent-core does await I/O there, an input stopped in that window stays
   `accepted` (the `P3`/`P4` traces before the guard showed it). Not checked
   in pi-agent-core's source.
6. **`seatFor` reads `g.owner` and `g.proc` after releasing `d.mu`**
   (`daemon.go:1775-1789`): a data race against a concurrent detach; benign
   in effect (a stale ownership answer), not run under `-race`.
7. **The eight-pass drain bound** (`seat.ts:733`, `:758`): a turn that keeps
   receiving prompts faster than it answers them ends with inputs queued,
   stranded like F7. Not modelled (the model drains without a bound).
8. **Hunt C6 (steer on an idle seat parks) is still present.** Shown by the
   model (`PromptProtocol-P6`) and the code (`seat.ts:966-980` opens no
   turn); not rerun on the real seat here.
9. **D1's re-run answer may be handed to the next wait.** The re-run turn
   produces a `started` receipt for the stopped request, which re-arms the
   host's turn latch on a turn id the host never minted
   (`turn_latch.go`, `recordTurnEventLocked`); `retireEndedTurnFor` keeps a
   turn that is still running, so the next send's wait may take the stopped
   request's answer. The C3 class; not run.

## 8. Relation to the 2026-11-21 Codex partials

The Codex run left 24 files under
`analysis-partials-2026-11-21/claudia-filler_analysis-2026-11-21/docs/analysis/turn-delivery-model/`.
All were read.

* **Kept, copied unchanged:** `Ownership.tla` and its five configs. The
  abstraction matches `process.ts` and `server.ts`: groups are tracked at
  spawn, forgotten when the leader closes, killed on lifeline and SIGTERM,
  and not on SIGKILL. The only additions are the `EXPECT` line on each
  config and a provenance comment. `Ownership-repro.ts` is kept as
  `repro/analysis-ownership-repro.ts` with its import path changed; it
  exercises the real `runCommand` and `killAllGroups`.
* **Redone: `Delivery.tla`.** It modelled an abstract log, wire and inbox in
  which a write retires the event and a detach only flips a flag. With no
  pump, no draining writer, no grant replay and no commit step it could not
  reach D3, the lagged re-claim ordering, the replay grace or the
  granted-reply boundary. Its conclusions (socket loss, daemon-restart loss,
  silent oversize drop, ack without dedup duplicates) are all confirmed by
  the new model (`D2`, `D3`, `D4`, `A2`), and its ack design is close to the
  `ack` mode here.
* **Redone: `PromptProtocol.tla`.** It had no Release Stop, no interrupt, no
  pre-turn compaction inside a turn and no host view of a request, and it
  treated re-queueing every accepted input on reload as the goal, so it could
  not find D1 or D2. Its invariants correspond to I6, I7 and I10 here.
* **Redone: `_repro/delivery_test.go`.** Its two tests called daemon
  internals directly (`detachLocked`, `forwarder`). The equivalents here go
  through a real socket and `handleGrant`
  (`TestAnalysisUnreadEventsOfADeadConsumerAreReportedClean`,
  `TestAnalysisLiveOversizeEventIsReported`).
* **Replaced:** `run_delivery.py` by `run_tlc.sh`, which covers all three
  models and fails on any outcome that differs from the config's claim.

## 9. Coverage

**Read in full:** `daemon/daemon.go` except `New` (`:289-480`) and the task,
pick and resolve handlers (`:1957-2260`); `internal/broker/server.go`,
`transport.go`; `broker_client.go`, `broker_agent.go`, `broker_bound.go`;
`turn_latch.go`; `omp/sidecar.go`; `omp_receipt.go`; `sidecar/server.ts`,
`seat.ts`, `receipts.ts`, `process.ts`; `sidecar/maintain.ts` from
`Maintenance` on (`:586-1013`); the daemon test fixture
(`daemon/fixture_test.go`, `pump_test.go`, `pump_kill_test.go`) and the
sidecar fixtures used by the reproductions; all 24 Codex partial files;
AGENTS.md, `~/.claude/go.md`, `~/.claude/tlaplus.md`, `~/.claude/bash.md`;
the claudia sections of the 2026-11-20 hunt digest; 🎯T1400–🎯T1412 in this
branch's `bullseye.yaml`.

**Read in part:** `omp_agent.go` (`StartAgent`, the pump, compaction, switch,
stop and shutdown, `:322-1341`, `:1900-1975`); `agent.go` (subscribe,
publish, interrupt, quiesce, stop); `registry.go` (stop and remove);
`mcp_invocation.go` (`sendContext`); `sidecar/killguard.ts` (first 80
lines); `broker_wire.go` (the event codec); `internal/broker/wire_grants.go`
(the grant reply fields).

**Not reached:** `turn_wait.go` and `delivery.go` (WaitForResponse and the
escalation ladder, which suspicion 9 touches); `registry_seats.go`
(`ResumeAll` and the seat watch) beyond the lines cited; the usage spool
(`omp/spool.go`, `sidecar/spool.ts`); `sidecar/turn.ts`, `hostcalls.ts`,
`maintain.ts`'s `History`; `mcp_host_stdio.go` (M13's group kill, taken from
🎯T1412 as written); every non-sidecar provider backend (Codex app-server,
Grok and Cursor ACP), whose processes the ownership model does not cover;
the fleet kernel.

**Not run:** the live gate (brief), the repository's own suites (not needed
for these claims), pi-agent-core's internals (suspicion 5).

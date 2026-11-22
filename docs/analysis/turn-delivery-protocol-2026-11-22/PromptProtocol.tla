--------------------------- MODULE PromptProtocol ---------------------------
\* Copyright 2026 Marcelo Cantos
\* SPDX-License-Identifier: Apache-2.0
\*
\* Prompt admission on one sidecar seat: sidecar/seat.ts (prompt, steer,
\* abortTurn, stopTurn, close, compactAtBound, executeTurn), receipts.ts (the
\* durable request journal) and server.ts (drop, load). Written 2026-11-22
\* against c33e60e. Every prompt is identified (has a request id), as the
\* Go host sends them (omp_receipt.go sendIdentified).
\*
\* JavaScript runs one handler at a time, so a whole prompt() up to its first
\* await is one step here. Turn progress (the model taking a queued input,
\* a turn ending, compaction starting and ending) is a separate step, since
\* each waits on the provider.
\*
\* The host's view of each request is what the wire told it: accepted (an
\* "accepted" event), refused (an error for that request), stopped (the host
\* itself released the seat with Release Stop while the request was waiting),
\* done (a terminal receipt).
EXTENDS Naturals, Sequences, FiniteSets

CONSTANTS
  Req,               \* request ids
  MaxSends,          \* prompt/steer messages the host sends in total
  GuardBeforeAccept, \* TRUE: c33e60e refuses during bound compaction before accept (C5 fix)
  Fix,               \* TRUE: the proposed fix. An input whose turn ends before it
                     \* starts, or that a stop discards, is settled cancelled.
  RequeueAbortedRoot,\* TRUE: c33e60e, executeTurn's catch re-queues it (seat.ts:768)
  AllowAbort, AllowStop, AllowDrop, AllowCrash, AllowRefusal, AllowSteer

VARIABLES
  proc,      \* the sidecar process: "up" | "down"
  seat,      \* "none" | "loaded" | "closing" (drop has set closed and awaits the unwind)
  turn,      \* [on, root (0 = continue on the queue), rootTaken]
  queue,     \* the agent's follow-up/steering queues (memory only)
  deliver,   \* abortTurn's unwind will run the queue (interrupt-deliver)
  compact,   \* "none" | "bound" (between turns, the compact op) | "mid" (inside a turn)
  j,         \* durable receipt state per request
  host,      \* the host's view per request
  sends,
  starts,    \* how many times each request started
  dc,        \* requests the host was told were accepted while compaction ran
  fresh,     \* the seat was just loaded and has not had a prompt since
  flag       \* first contract a step broke

vars == <<proc, seat, turn, queue, deliver, compact, j, host, sends, starts, dc, fresh, flag>>

Range(s) == {s[i] : i \in DOMAIN s}
Remove(s, r) == SelectSeq(s, LAMBDA x: x # r)
SortedOf(S) == [i \in 1..Cardinality(S) |-> CHOOSE x \in S : Cardinality({y \in S : y < x}) = i - 1]
Terminal == {"completed", "cancelled", "failed"}
NoTurn == [on |-> FALSE, root |-> 0, rootTaken |-> FALSE]

Init ==
  /\ proc = "up" /\ seat = "loaded" /\ turn = NoTurn /\ queue = <<>> /\ deliver = FALSE
  /\ compact = "none" /\ j = [r \in Req |-> "none"] /\ host = [r \in Req |-> "none"]
  /\ sends = 0 /\ starts = [r \in Req |-> 0] /\ dc = {} /\ fresh = FALSE
  /\ flag = [refusedDurable |-> FALSE, ranAfterEnd |-> FALSE, ranAfterDrop |-> FALSE,
             twice |-> FALSE]

\* prompt's busy test (seat.ts:946): turnDone || retrying || isStreaming || hasQueuedMessages.
\* A closing seat is still unwinding its stopped turn, so turnDone is set.
Busy == turn.on \/ deliver \/ queue # <<>> \/ seat = "closing"
Loaded == proc = "up" /\ seat \in {"loaded", "closing"}

\* A turn whose input has not reached the model and that is not compacting
\* is between microtasks: runTurn calls agent.prompt synchronously when no
\* pre-turn pass is due, and nothing awaits real I/O before the pre-model
\* gate. No socket message is handled in that state.
Settled == ~(turn.on /\ ~turn.rootTaken /\ turn.root # 0 /\ compact = "none")

\* The host is told about r (a terminal receipt reads as done).
Tell(r, v) == [host EXCEPT ![r] = IF j'[r] \in Terminal \cup {"unknown"} THEN "done" ELSE v]

\* seat.prompt (seat.ts:924-965) and server.ts:564.
Prompt(r) ==
  /\ Settled /\ sends < MaxSends /\ host[r] # "done"
  /\ sends' = sends + 1
  /\ fresh' = FALSE
  /\ IF ~Loaded
     THEN \* "seat is not loaded"
          /\ host' = [host EXCEPT ![r] = "refused"]
          /\ UNCHANGED <<turn, queue, deliver, j, dc, flag>>
     ELSE IF compact = "bound" /\ ~Busy
     THEN \* refuseDuringBoundCompact (:830): turn_in_flight
          /\ j' = IF GuardBeforeAccept \/ j[r] # "none" THEN j ELSE [j EXCEPT ![r] = "accepted"]
          /\ host' = [host EXCEPT ![r] = "refused"]
          /\ flag' = [flag EXCEPT !.refusedDurable = @ \/ j'[r] = "accepted"]
          /\ UNCHANGED <<turn, queue, deliver, dc>>
     ELSE IF j[r] # "none"
     THEN \* not fresh: the ack names the existing receipt (:932-937)
          /\ UNCHANGED j
          /\ host' = Tell(r, "accepted")
          /\ IF j[r] = "accepted" /\ ~turn.on /\ ~deliver /\ seat = "loaded" /\ queue # <<>>
             THEN /\ turn' = [on |-> TRUE, root |-> 0, rootTaken |-> TRUE]
             ELSE UNCHANGED turn
          /\ dc' = IF compact # "none" THEN dc \cup {r} ELSE dc
          /\ UNCHANGED <<queue, deliver, flag>>
     ELSE IF Busy
     THEN \* queued behind the running turn (:946-961)
          /\ j' = [j EXCEPT ![r] = "accepted"]
          /\ queue' = Append(queue, r)
          /\ host' = [host EXCEPT ![r] = "accepted"]
          /\ dc' = IF compact # "none" THEN dc \cup {r} ELSE dc
          \* a turn opens on the queue when nothing is running (:957)
          /\ turn' = IF ~turn.on /\ ~deliver /\ seat = "loaded"
                     THEN [on |-> TRUE, root |-> 0, rootTaken |-> TRUE] ELSE turn
          /\ UNCHANGED <<deliver, flag>>
     ELSE \* idle: runTurn with r as its own input (:963-964)
          /\ j' = [j EXCEPT ![r] = "accepted"]
          /\ turn' = [on |-> TRUE, root |-> r, rootTaken |-> FALSE]
          /\ host' = [host EXCEPT ![r] = "accepted"]
          /\ dc' = IF compact # "none" THEN dc \cup {r} ELSE dc
          /\ UNCHANGED <<queue, deliver, flag>>
  /\ UNCHANGED <<proc, seat, compact, starts>>

\* seat.steer (seat.ts:966-980): accepted and queued, and it opens no turn.
Steer(r) ==
  /\ AllowSteer /\ Settled /\ sends < MaxSends /\ host[r] = "none" /\ Loaded
  /\ ~(compact = "bound" /\ ~Busy)
  /\ sends' = sends + 1 /\ fresh' = FALSE
  /\ j' = [j EXCEPT ![r] = "accepted"]
  /\ queue' = Append(queue, r)
  /\ host' = [host EXCEPT ![r] = "accepted"]
  /\ dc' = IF compact # "none" THEN dc \cup {r} ELSE dc
  /\ UNCHANGED <<proc, seat, turn, deliver, compact, starts, flag>>

\* The engine reaches the pre-model gate with an input: receipts.start
\* (seat.ts:558-576). The root first, then queued inputs in order.
Take ==
  /\ Loaded /\ turn.on /\ compact = "none"
  /\ LET x == IF ~turn.rootTaken THEN turn.root ELSE Head(queue) IN
     /\ (~turn.rootTaken \/ queue # <<>>)
     /\ j[x] = "accepted"
     /\ j' = [j EXCEPT ![x] = "started"]
     /\ starts' = [starts EXCEPT ![x] = @ + 1]
     /\ turn' = [turn EXCEPT !.rootTaken = TRUE]
     /\ queue' = IF ~turn.rootTaken THEN queue ELSE Tail(queue)
     /\ flag' = [flag EXCEPT !.ranAfterEnd = @ \/ host[x] \in {"stopped", "done"},
                             !.ranAfterDrop = @ \/ seat = "closing",
                             !.twice = @ \/ starts[x] >= 1]
  /\ UNCHANGED <<proc, seat, deliver, compact, host, sends, dc, fresh>>

\* Maintenance inside a turn (pre_turn, mid_turn, post_turn passes).
MidCompact ==
  /\ Loaded /\ turn.on
  /\ compact' = IF compact = "none" THEN "mid" ELSE IF compact = "mid" THEN "none" ELSE compact
  /\ compact # "bound"
  /\ UNCHANGED <<proc, seat, turn, queue, deliver, j, host, sends, starts, dc, fresh, flag>>

\* executeTurn ends normally: the drain loop has emptied the queue
\* (seat.ts:733-761), the started inputs complete.
TurnEnd ==
  /\ Loaded /\ turn.on /\ turn.rootTaken /\ queue = <<>> /\ compact = "none"
  /\ j' = [r \in Req |-> IF j[r] = "started" THEN "completed" ELSE j[r]]
  /\ host' = [r \in Req |-> IF j[r] = "started" THEN "done" ELSE host[r]]
  /\ turn' = NoTurn
  /\ UNCHANGED <<proc, seat, queue, deliver, compact, sends, starts, dc, fresh, flag>>

\* A provider refusal ends the turn and leaves queued inputs queued
\* ("A terminal refusal leaves accepted inputs queued", seat.ts:954).
TurnRefused ==
  /\ AllowRefusal /\ Loaded /\ turn.on /\ turn.rootTaken /\ compact = "none"
  /\ j' = [r \in Req |-> IF j[r] = "started" THEN "failed" ELSE j[r]]
  /\ host' = [r \in Req |-> IF j[r] = "started" THEN "done" ELSE host[r]]
  /\ turn' = NoTurn
  /\ UNCHANGED <<proc, seat, queue, deliver, compact, sends, starts, dc, fresh, flag>>

CancelStarted == [r \in Req |-> IF j[r] = "started" THEN "cancelled" ELSE j[r]]

\* Interrupt: abortTurn (seat.ts:862-905). The queue survives and the unwind
\* runs it as its own turn.
Abort ==
  /\ AllowAbort /\ Settled /\ Loaded /\ seat = "loaded" /\ turn.on
  /\ LET r0 == turn.root
         unstarted == ~turn.rootTaken /\ r0 # 0
     IN /\ j' = IF Fix /\ unstarted THEN [CancelStarted EXCEPT ![r0] = "cancelled"] ELSE CancelStarted
        \* The host hears the turn end aborted, so its wait for r0 is over.
        /\ host' = [r \in Req |-> IF j[r] = "started" \/ (unstarted /\ r = r0) THEN "done" ELSE host[r]]
        \* An input that had not reached the model stays accepted. During a
        \* pre-turn compaction executeTurn's catch puts it back on the queue
        \* (seat.ts:768-774); otherwise it is in the conversation, answered
        \* by closeAbortedTail, and only its receipt is left.
        /\ queue' = IF unstarted /\ compact = "mid" /\ RequeueAbortedRoot /\ ~Fix THEN Append(queue, r0) ELSE queue
  /\ turn' = NoTurn /\ deliver' = TRUE
  /\ compact' = IF compact = "mid" THEN "none" ELSE compact
  /\ UNCHANGED <<proc, seat, sends, starts, dc, fresh, flag>>

Unwind ==
  /\ deliver /\ Loaded
  /\ deliver' = FALSE
  /\ turn' = IF queue # <<>> /\ seat = "loaded" /\ ~turn.on
             THEN [on |-> TRUE, root |-> 0, rootTaken |-> TRUE] ELSE turn
  /\ UNCHANGED <<proc, seat, queue, compact, j, host, sends, starts, dc, fresh, flag>>

\* Release Stop: the daemon's Quiesce sends {abort, stop_turn} (omp_agent.go:575)
\* and seat.stopTurn empties the queues (seat.ts:907-915). The host has
\* released the seat; what it had waiting is, to it, discarded.
Stop ==
  /\ AllowStop /\ Settled /\ Loaded /\ seat = "loaded"
  /\ LET r0 == turn.root
         unstarted == turn.on /\ ~turn.rootTaken /\ r0 # 0
         discarded == Range(queue) \cup (IF unstarted THEN {r0} ELSE {})
         cancelled == CancelStarted
     IN /\ j' = IF Fix
                THEN [r \in Req |-> IF r \in discarded /\ cancelled[r] = "accepted" THEN "cancelled" ELSE cancelled[r]]
                ELSE cancelled
        /\ host' = [r \in Req |-> IF j[r] = "started" THEN "done"
                                  ELSE IF r \in discarded THEN (IF Fix THEN "done" ELSE "stopped")
                                  ELSE host[r]]
        \* stopTurn empties the queues at once; executeTurn's catch runs later
        \* and puts an input stopped during pre-turn compaction back.
        /\ queue' = IF unstarted /\ compact = "mid" /\ RequeueAbortedRoot /\ ~Fix THEN <<r0>> ELSE <<>>
  /\ turn' = NoTurn /\ deliver' = TRUE
  /\ compact' = IF compact = "mid" THEN "none" ELSE compact
  /\ UNCHANGED <<proc, seat, sends, starts, dc, fresh, flag>>

\* The compact op between turns (server.ts:537; seat.ts:1018 refuses a busy seat).
BoundBegin ==
  /\ Loaded /\ seat = "loaded" /\ ~Busy /\ compact = "none"
  /\ compact' = "bound"
  /\ UNCHANGED <<proc, seat, turn, queue, deliver, j, host, sends, starts, dc, fresh, flag>>
BoundEnd ==
  /\ compact = "bound" /\ compact' = "none"
  /\ UNCHANGED <<proc, seat, turn, queue, deliver, j, host, sends, starts, dc, fresh, flag>>

\* drop (server.ts:523): close() sets closed, stopTurn, awaits the unwind,
\* then seals the store. With no turn open there is no window.
DropBegin ==
  /\ AllowDrop /\ Settled /\ Loaded /\ seat = "loaded"
  /\ j' = IF Fix /\ turn.on /\ ~turn.rootTaken /\ turn.root # 0
          THEN [CancelStarted EXCEPT ![turn.root] = "cancelled"] ELSE CancelStarted
  /\ host' = [r \in Req |-> IF j[r] = "started" \/ (turn.on /\ ~turn.rootTaken /\ r = turn.root) THEN "done" ELSE host[r]]
  /\ queue' = <<>> /\ deliver' = FALSE
  /\ compact' = IF compact = "mid" THEN "none" ELSE compact
  /\ IF turn.on THEN seat' = "closing" ELSE seat' = "none"
  /\ turn' = NoTurn
  /\ UNCHANGED <<proc, sends, starts, dc, fresh, flag>>
DropSeal ==
  /\ seat = "closing" /\ proc = "up"
  /\ seat' = "none" /\ queue' = <<>> /\ deliver' = FALSE
  /\ UNCHANGED <<proc, turn, compact, j, host, sends, starts, dc, fresh, flag>>

\* The sidecar dies. On a lifeline EOF finishStartedOnExit cancels started
\* receipts; a SIGKILL leaves them started, and the next load reads them as
\* unknown (receipts.ts constructor).
Crash ==
  /\ AllowCrash /\ proc = "up"
  /\ \E graceful \in BOOLEAN :
       /\ j' = IF graceful THEN CancelStarted ELSE j
       /\ host' = IF graceful THEN [r \in Req |-> IF j[r] = "started" THEN "done" ELSE host[r]] ELSE host
  /\ proc' = "down" /\ seat' = "none" /\ turn' = NoTurn /\ queue' = <<>> /\ deliver' = FALSE
  /\ compact' = "none"
  /\ UNCHANGED <<sends, starts, dc, fresh, flag>>
Restart ==
  /\ proc = "down" /\ proc' = "up"
  /\ UNCHANGED <<seat, turn, queue, deliver, compact, j, host, sends, starts, dc, fresh, flag>>

\* load of the session (server.ts:455-498): receipts.pending() goes back on
\* the queue in acceptance order (approximated by id) and nothing runs until
\* the next prompt (seat.ts:526-545). Started rows are now unknown.
Load ==
  /\ proc = "up" /\ seat = "none"
  /\ seat' = "loaded"
  /\ j' = [r \in Req |-> IF j[r] = "started" THEN "unknown" ELSE j[r]]
  /\ queue' = SortedOf({r \in Req : j[r] = "accepted"})
  /\ fresh' = TRUE
  /\ UNCHANGED <<proc, turn, deliver, compact, host, sends, starts, dc, flag>>

Next ==
  \/ \E r \in Req : Prompt(r) \/ Steer(r)
  \/ Take \/ MidCompact \/ TurnEnd \/ TurnRefused \/ Abort \/ Unwind \/ Stop
  \/ BoundBegin \/ BoundEnd \/ DropBegin \/ DropSeal \/ Crash \/ Restart \/ Load

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
\* C5: a request the host was told was refused is not durably accepted.
RefusedNotDurable == ~flag.refusedDurable
\* C4: a dropped seat starts no input.
NoRunAfterDrop == ~flag.ranAfterDrop
\* No input starts twice, across turns, drops and restarts.
AtMostOnce == ~flag.twice
\* An input the host was told had ended (its turn aborted, or discarded by
\* its own Release Stop) never starts later.
EndedStaysEnded == ~flag.ranAfterEnd

\* r is waiting where nothing will run it: the host holds an accepted, the
\* journal says accepted, the seat is loaded and idle with no unwind owed,
\* and the seat was not just loaded (a restart nudge would be the next prompt).
Stranded(r) ==
  /\ host[r] = "accepted" /\ j[r] = "accepted"
  /\ proc = "up" /\ seat = "loaded" /\ ~turn.on /\ ~deliver /\ compact = "none" /\ ~fresh
NotStranded == \A r \in Req : ~Stranded(r)

\* The question's property: a prompt accepted while compaction ran is not
\* dropped. It stays durable, and while its seat is loaded it is on the
\* seat's queue, is the input of the running turn, or has run.
CompactionAcceptedKept ==
  \A r \in dc :
    host[r] = "accepted" =>
      /\ j[r] # "none"
      /\ (j[r] = "accepted" /\ proc = "up" /\ seat = "loaded") =>
           (r \in Range(queue) \/ (turn.on /\ turn.root = r /\ ~turn.rootTaken))

TypeOK ==
  /\ proc \in {"up", "down"} /\ seat \in {"none", "loaded", "closing"}
  /\ compact \in {"none", "bound", "mid"}
  /\ j \in [Req -> {"none", "accepted", "started", "completed", "cancelled", "failed", "unknown"}]
  /\ host \in [Req -> {"none", "accepted", "refused", "stopped", "done"}]
  /\ Len(queue) <= Cardinality(Req)
=============================================================================

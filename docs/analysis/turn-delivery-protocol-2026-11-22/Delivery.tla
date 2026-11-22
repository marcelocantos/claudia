------------------------------ MODULE Delivery ------------------------------
\* Copyright 2026 Marcelo Cantos
\* SPDX-License-Identifier: Apache-2.0
\*
\* Event delivery for one daemon seat, from the seat's forwarder to the
\* consumer application, across consumer death, broker crash, slow-consumer
\* detach, re-claim and replay. Written 2026-11-22 against c33e60e
\* (filler/fix-hunt-findings-2). Each action names the code it abstracts.
\*
\* One logical consumer (a Jevons, say) whose process may die and be replaced
\* by the next incarnation, 1..Incs, one alive at a time. Incarnation c talks
\* over connection c; after a broker crash it reconnects on a fresh one, which
\* keeps the number (the old daemon's state is gone, so nothing confuses the
\* two). Events 1..N are the seat's Event stream in publish order.
\*
\* Mode "current" is the code as written. "seq" and "ack" are the two designs
\* 🎯T1404 leaves open (report section 5):
\*   "seq": each agent_event carries a per-seat sequence number and a grant
\*          reply carries the daemon's boot epoch; the consumer library
\*          reports a gap or an epoch change. No acknowledgement.
\*   "ack": "seq" plus a cumulative acknowledgement. The daemon keeps every
\*          event written to the owner until the owner acknowledges it, a
\*          detach puts the unacknowledged events back (merged by seq), and a
\*          grant names the consumer's cursor so the replay starts after it.
\* DurableCursor: the consumer's applied cursor survives its own death (an
\* application that stores the cursor with what it did with the event).
\* LagReachesApp: whether a grant reply's Lagged reaches the application.
\* c33e60e logs it in StartAgent (broker_agent.go:163) and drops it on a
\* re-claim (broker_agent.go:242), so the faithful value is FALSE.
EXTENDS Naturals, Sequences, FiniteSets, TLC

CONSTANTS N, QCap, RingCap, SockCap, Incs, MaxGrants, BrokerCrashes,
          Oversize, Mode, DurableCursor, LagReachesApp

ASSUME Mode \in {"current", "seq", "ack"}
ASSUME Oversize \subseteq 1..N

Conns == 1..Incs
Events == 1..N
Range(s) == {s[i] : i \in DOMAIN s}

VARIABLES
  pub,      \* events published so far
  d,        \* the brokerGrant: owner, pump, draining, ring, lag (+ ack state)
  pumps,    \* grantPump records by id
  npumps,   \* pumps created so far (ids are never reused)
  gr,       \* handleGrant progress, per connection
  sock,     \* frames in the kernel buffer, daemon -> consumer, per connection
  notice,   \* notifyDetached goroutine not yet run, per connection
  alive,    \* consumer process alive, per incarnation
  born,
  open,     \* the daemon has not yet run ConnClosed for this connection
  cl,       \* consumer library state (brokerAgentBackend), per incarnation
  applied,  \* events the application received, per incarnation
  cursor,   \* highest event id applied, per incarnation
  knows,    \* the incarnation has a baseline to detect a gap from
  reported, \* events the application was told may be lost
  up, crashes, grants,
  epochNew, \* the daemon restarted since a consumer last read a grant reply
  bad       \* which delivery contract a step broke (checked as invariants)

vars == <<pub, d, pumps, npumps, gr, sock, notice, alive, born, open, cl,
          applied, cursor, knows, reported, up, crashes, grants, epochNew, bad>>

-----------------------------------------------------------------------------
\* Records

DInit == [owner |-> 0, pump |-> 0, draining |-> 0, ring |-> <<>>, lag |-> FALSE,
          unacked |-> <<>>, acked |-> 0]
\* st: none | unstarted | run | taken | committed | done
\* w is pump.writing (shared, cleared by a thief); raw is the writer's local copy.
PNone == [st |-> "none", conn |-> 0, q |-> <<>>, w |-> 0, raw |-> 0,
          closed |-> FALSE, stolen |-> FALSE]
GIdle == [ph |-> "idle", rep |-> <<>>, lagged |-> FALSE, p |-> 0, own |-> FALSE, from |-> 0, upto |-> 0]
CFresh == [granted |-> FALSE, held |-> <<>>, want |-> TRUE]
EvF(e) == [k |-> "E", e |-> e, lag |-> FALSE, upto |-> 0]
GF(l, u) == [k |-> "G", e |-> 0, lag |-> l, upto |-> u]
DF == [k |-> "D", e |-> 0, lag |-> FALSE, upto |-> 0]

-----------------------------------------------------------------------------
\* Ring operations (daemon.go retainUnowned :1451, putBackLocked :1434).
\* In "ack" mode the daemon has sequence numbers, so it merges by seq.

SortedOf(S) == [i \in 1..Cardinality(S) |->
                  CHOOSE x \in S : Cardinality({y \in S : y < x}) = i - 1]
Merge(s, t) == SortedOf(Range(s) \cup Range(t))
Trim(s) == IF Len(s) > RingCap THEN SubSeq(s, Len(s) - RingCap + 1, Len(s)) ELSE s
WithRing(dd, s) == [dd EXCEPT !.ring = Trim(s), !.lag = dd.lag \/ Len(s) > RingCap]
Retain(dd, e) == WithRing(dd, IF Mode = "ack" THEN Merge(dd.ring, <<e>>) ELSE Append(dd.ring, e))
PutBack(dd, pend) == WithRing(dd, IF Mode = "ack" THEN Merge(pend, dd.ring) ELSE pend \o dd.ring)
WSeq(rec) == IF rec.w = 0 THEN <<>> ELSE <<rec.w>>

\* detachLocked (daemon.go:744). A writer that committed its write keeps the
\* event and becomes g.draining; otherwise takePumpLocked moves the in-flight
\* event and the queue onto the ring. "ack": unacknowledged events go back too.
DetachR(dd, pp) ==
  LET p == dd.pump
      back == IF Mode = "ack" THEN dd.unacked ELSE <<>>
  IN IF p = 0 THEN [d |-> [dd EXCEPT !.owner = 0], p |-> pp]
     ELSE IF pp[p].st = "committed"
     THEN [d |-> [PutBack(dd, back) EXCEPT !.owner = 0, !.pump = 0, !.draining = p, !.unacked = <<>>],
           p |-> [pp EXCEPT ![p].closed = TRUE]]
     ELSE LET taken == back \o WSeq(pp[p]) \o pp[p].q
              d1 == IF Mode = "ack" THEN WithRing(dd, Merge(dd.ring, taken))
                    ELSE WithRing(dd, dd.ring \o taken)
          IN [d |-> [d1 EXCEPT !.owner = 0, !.pump = 0, !.unacked = <<>>],
              p |-> [pp EXCEPT ![p].closed = TRUE, ![p].stolen = TRUE, ![p].w = 0, ![p].q = <<>>]]

\* finishPumpLocked (daemon.go:1539).
FinishR(dd, pp, p) ==
  LET rec == pp[p]
      back == IF ~rec.stolen /\ rec.closed THEN WSeq(rec) \o rec.q ELSE <<>>
      d1 == IF back # <<>> THEN PutBack(dd, back) ELSE dd
      d2 == IF d1.draining = p THEN [d1 EXCEPT !.draining = 0] ELSE d1
  IN [d |-> d2, p |-> [pp EXCEPT ![p].st = "done", ![p].w = 0, ![p].q = <<>>]]

-----------------------------------------------------------------------------
\* The application receiving one event (brokerAgentBackend.deliver ->
\* Agent.publishEvent -> subscriber). st is [applied, cursor, reported, bad].

AppliedBy(ap) == UNION {ap[i] : i \in Conns}

\* Events that can still reach a consumer: the ring, a pump's queue and
\* in-flight event, the writer's own copy once it has committed to a write
\* to a live consumer (before that it re-checks stolen, daemon.go:1504; a
\* write to a dead one fails and puts nothing back once stolen), the
\* unacknowledged window, a socket, a push queue. An oversize event can
\* never be written, so it is not on its way anywhere.
InCustody ==
  (Range(d.ring) \cup Range(d.unacked)
   \cup UNION {Range(pumps[p].q) \cup ({pumps[p].w} \ {0})
               \cup (IF pumps[p].st = "committed" /\ alive[pumps[p].conn] THEN {pumps[p].raw} ELSE {})
             : p \in 1..MaxGrants}
   \cup UNION {{f.e : f \in Range(sock[c])} \ {0} : c \in Conns}
   \cup UNION {Range(cl[c].held) : c \in Conns}) \ Oversize

ApplyOne(st, c, e) ==
  IF Mode # "current" /\ e <= st.cursor[c] THEN st   \* seq dedup
  ELSE
    LET all == AppliedBy(st.applied)
        gap == IF Mode # "current" /\ st.knows[c]
               THEN {x \in (st.cursor[c] + 1)..(e - 1) : x \notin all} ELSE {}
        rep == st.reported \cup gap
        hole == {x \in 1..(e - 1) : x \notin all} \ rep
        \* an earlier event still on its way arrives after this one
        late == {x \in 1..(e - 1) : x \notin all /\ x \in InCustody}
    IN [applied |-> [st.applied EXCEPT ![c] = @ \cup {e}],
        cursor |-> [st.cursor EXCEPT ![c] = IF e > @ THEN e ELSE @],
        \* Having applied one event, the incarnation has a baseline; a fresh
        \* one with no stored cursor cannot judge what came before it.
        knows |-> [st.knows EXCEPT ![c] = TRUE],
        reported |-> rep,
        bad |-> [st.bad EXCEPT !.dupInc = @ \/ e \in st.applied[c],
                               !.dupLogical = @ \/ e \in (all \ st.applied[c]),
                               !.hole = @ \/ hole # {},
                               !.reorder = @ \/ late # {}]]

RECURSIVE ApplyAll(_, _, _)
ApplyAll(st, c, s) == IF s = <<>> THEN st ELSE ApplyAll(ApplyOne(st, c, Head(s)), c, Tail(s))

St == [applied |-> applied, cursor |-> cursor, knows |-> knows, reported |-> reported, bad |-> bad]
SetSt(st) == /\ applied' = st.applied /\ cursor' = st.cursor /\ knows' = st.knows
             /\ reported' = st.reported /\ bad' = st.bad

-----------------------------------------------------------------------------
Init ==
  /\ pub = 0 /\ d = DInit /\ pumps = [p \in 1..MaxGrants |-> PNone] /\ npumps = 0
  /\ gr = [c \in Conns |-> GIdle] /\ sock = [c \in Conns |-> <<>>]
  /\ notice = [c \in Conns |-> FALSE]
  /\ alive = [c \in Conns |-> c = 1] /\ born = [c \in Conns |-> c = 1]
  /\ open = [c \in Conns |-> c = 1]
  /\ cl = [c \in Conns |-> CFresh]
  /\ applied = [c \in Conns |-> {}] /\ cursor = [c \in Conns |-> 0]
  /\ knows = [c \in Conns |-> c = 1] /\ reported = {}
  /\ up = TRUE /\ crashes = 0 /\ grants = 0 /\ epochNew = FALSE
  /\ bad = [dupInc |-> FALSE, dupLogical |-> FALSE, hole |-> FALSE, reorder |-> FALSE]

ConsumerVars == <<alive, born, cl, applied, cursor, knows, reported, bad>>

\* forwarder (daemon.go:1357): enqueue, or detach a consumer whose queue is
\* full and tell it (notifyDetached), then retain.
Publish ==
  /\ up /\ pub < N
  /\ LET e == pub + 1
         p == d.pump
     IN /\ pub' = e
        /\ IF p # 0 /\ Len(pumps[p].q) < QCap
           THEN /\ pumps' = [pumps EXCEPT ![p].q = Append(@, e)]
                /\ UNCHANGED <<d, notice>>
           ELSE IF p # 0
           THEN LET r == DetachR(d, pumps) IN
                /\ d' = Retain(r.d, e)
                /\ pumps' = r.p
                /\ notice' = [notice EXCEPT ![d.owner] = TRUE]
           ELSE /\ d' = Retain(d, e)
                /\ UNCHANGED <<pumps, notice>>
  /\ UNCHANGED <<npumps, gr, sock, open, up, crashes, grants, epochNew, ConsumerVars>>

\* runPump (daemon.go:1488), dequeue and record the in-flight event.
Take(p) ==
  /\ up /\ pumps[p].st = "run" /\ d.pump = p /\ ~pumps[p].stolen
  /\ pumps[p].w = 0 /\ pumps[p].q # <<>>
  /\ pumps' = [pumps EXCEPT ![p].w = Head(pumps[p].q), ![p].raw = Head(pumps[p].q),
                            ![p].q = Tail(pumps[p].q), ![p].st = "taken"]
  /\ UNCHANGED <<pub, d, npumps, gr, sock, notice, open, up, crashes, grants, epochNew, ConsumerVars>>

\* runPump :1503-1509, relock; past here the write happens.
Commit(p) ==
  /\ up /\ pumps[p].st = "taken"
  /\ IF d.pump = p /\ ~pumps[p].stolen
     THEN /\ pumps' = [pumps EXCEPT ![p].st = "committed"]
          /\ UNCHANGED d
     ELSE LET r == FinishR(d, pumps, p) IN d' = r.d /\ pumps' = r.p
  /\ UNCHANGED <<pub, npumps, gr, sock, notice, open, up, crashes, grants, epochNew, ConsumerVars>>

\* runPump :1514-1531, the socket write and what the writer records after it.
\* A write succeeds once the kernel takes the bytes, whether or not the
\* consumer ever reads them (🎯T1404). It blocks while the buffer is full.
Write(p) ==
  /\ up /\ pumps[p].st = "committed"
  /\ LET c == pumps[p].conn
         e == pumps[p].raw
         dead == ~alive[c] \/ ~open[c]
         attached == d.pump = p
         p1 == [pumps EXCEPT ![p].st = "run"]
     IN \/ \* the kernel accepted the frame
           /\ ~dead /\ e \notin Oversize /\ Len(sock[c]) < SockCap
           /\ sock' = [sock EXCEPT ![c] = Append(@, EvF(e))]
           /\ IF attached
              THEN /\ pumps' = [p1 EXCEPT ![p].w = 0]
                   /\ d' = IF Mode = "ack" THEN [d EXCEPT !.unacked = Append(@, e)] ELSE d
              ELSE \* detached meanwhile: finishPumpLocked. "ack" keeps the
                   \* event, since no acknowledgement from this owner will count.
                   LET r == FinishR(d, IF Mode = "ack" THEN p1 ELSE [p1 EXCEPT ![p].w = 0], p)
                   IN d' = r.d /\ pumps' = r.p
        \/ \* ErrFrameTooLarge: an attached writer drops it (:1532), with no Lagged
           /\ ~dead /\ e \in Oversize
           /\ UNCHANGED sock
           /\ IF attached THEN pumps' = [p1 EXCEPT ![p].w = 0] /\ UNCHANGED d
              ELSE LET r == FinishR(d, p1, p) IN d' = r.d /\ pumps' = r.p
        \/ \* EPIPE: detach (:1527), which now takes the event back
           /\ dead
           /\ UNCHANGED sock
           /\ IF attached
              THEN LET r == DetachR(d, p1)
                       r2 == FinishR(r.d, r.p, p)
                   IN d' = r2.d /\ pumps' = r2.p
              ELSE LET r == FinishR(d, p1, p) IN d' = r.d /\ pumps' = r.p
  /\ UNCHANGED <<pub, npumps, gr, notice, open, up, crashes, grants, epochNew, ConsumerVars>>

\* runPump :1492, woken with nothing to do after a detach.
ExitIdle(p) ==
  /\ up /\ pumps[p].st = "run" /\ (d.pump # p \/ pumps[p].stolen)
  /\ LET r == FinishR(d, pumps, p) IN d' = r.d /\ pumps' = r.p
  /\ UNCHANGED <<pub, npumps, gr, sock, notice, open, up, crashes, grants, epochNew, ConsumerVars>>

\* The consumer library sends a grant: StartAgent, or reclaim() after an
\* agent_detached notice (broker_agent.go:231, :612).
RequestGrant(c) ==
  /\ up /\ alive[c] /\ open[c] /\ cl[c].want /\ gr[c].ph = "idle" /\ grants < MaxGrants
  /\ gr' = [gr EXCEPT ![c] = [GIdle EXCEPT !.ph = "req", !.from = cursor[c]]]
  /\ cl' = [cl EXCEPT ![c].want = FALSE]
  /\ grants' = grants + 1
  /\ UNCHANGED <<pub, d, pumps, npumps, sock, notice, open, up, crashes, epochNew,
                 alive, born, applied, cursor, knows, reported, bad>>

\* handleGrant first lock section (daemon.go:928-941). Launch follows.
GrantCheck(c) ==
  /\ up /\ gr[c].ph = "req"
  /\ IF d.owner # 0 /\ d.owner # c
     THEN /\ gr' = [gr EXCEPT ![c] = GIdle]     \* grant_held; the consumer retries
          /\ cl' = [cl EXCEPT ![c].want = TRUE]
     ELSE /\ gr' = [gr EXCEPT ![c].ph = "launched"]
          /\ UNCHANGED cl
  /\ UNCHANGED <<pub, d, pumps, npumps, sock, notice, open, up, crashes, grants, epochNew,
                 alive, born, applied, cursor, knows, reported, bad>>

\* handleGrant second lock section (daemon.go:1028-1110): displace another
\* owner (no notice: H13), take a draining writer that writes elsewhere, wait
\* for one that writes here, then copy the ring and make a new pump.
GrantBind(c) ==
  /\ up /\ gr[c].ph \in {"launched", "wait"}
  /\ gr[c].ph = "wait" => d.draining = 0
  /\ LET r1 == IF d.owner # 0 /\ d.owner # c THEN DetachR(d, pumps) ELSE [d |-> d, p |-> pumps]
         dr == r1.d.draining
         r2 == IF dr # 0 /\ r1.p[dr].conn # c
               THEN [d |-> [PutBack(r1.d, WSeq(r1.p[dr]) \o r1.p[dr].q) EXCEPT !.draining = 0],
                     p |-> [r1.p EXCEPT ![dr].stolen = TRUE, ![dr].w = 0, ![dr].q = <<>>]]
               ELSE r1
     IN IF r2.d.draining # 0
        THEN /\ d' = r2.d /\ pumps' = r2.p
             /\ gr' = [gr EXCEPT ![c].ph = "wait"]
             /\ UNCHANGED npumps
        ELSE LET own == r2.d.owner = c /\ r2.d.pump # 0
                 np == npumps + 1
                 rep == IF Mode = "ack" THEN SelectSeq(r2.d.ring, LAMBDA x: x > gr[c].from)
                        ELSE r2.d.ring
             IN /\ d' = IF own THEN r2.d ELSE [r2.d EXCEPT !.owner = c, !.pump = np]
                /\ pumps' = IF own THEN r2.p
                            ELSE [r2.p EXCEPT ![np] = [PNone EXCEPT !.st = "unstarted", !.conn = c]]
                /\ npumps' = IF own THEN npumps ELSE np
                /\ gr' = [gr EXCEPT ![c].ph = "replay", ![c].rep = rep,
                                    ![c].lagged = r2.d.lag \/ (Mode # "current" /\ epochNew),
                                    ![c].p = IF own THEN r2.d.pump ELSE np, ![c].own = own,
                                    ![c].upto = pub]
  /\ UNCHANGED <<pub, sock, notice, open, up, crashes, grants, epochNew, ConsumerVars>>

\* handleGrant replay loop (daemon.go:1115-1127).
Replay(c) ==
  /\ up /\ gr[c].ph = "replay" /\ gr[c].rep # <<>>
  /\ LET e == Head(gr[c].rep) IN
     \/ /\ ~alive[c]                       \* c.Close(); return
        /\ gr' = [gr EXCEPT ![c] = GIdle] /\ UNCHANGED sock
     \/ /\ alive[c] /\ e \in Oversize       \* skipped, marked lagged
        /\ gr' = [gr EXCEPT ![c].rep = Tail(@), ![c].lagged = TRUE] /\ UNCHANGED sock
     \/ /\ alive[c] /\ e \notin Oversize /\ Len(sock[c]) < SockCap
        /\ sock' = [sock EXCEPT ![c] = Append(@, EvF(e))]
        /\ gr' = [gr EXCEPT ![c].rep = Tail(@)]
  /\ UNCHANGED <<pub, d, pumps, npumps, notice, open, up, crashes, grants, epochNew, ConsumerVars>>

\* The granted reply (daemon.go:1128-1150).
Reply(c) ==
  /\ up /\ gr[c].ph = "replay" /\ gr[c].rep = <<>>
  /\ \/ /\ ~alive[c] /\ gr' = [gr EXCEPT ![c] = GIdle] /\ UNCHANGED sock
     \/ /\ alive[c] /\ Len(sock[c]) < SockCap
        /\ sock' = [sock EXCEPT ![c] = Append(@, GF(gr[c].lagged, gr[c].upto))]
        /\ gr' = [gr EXCEPT ![c].ph = "reply"]
  /\ UNCHANGED <<pub, d, pumps, npumps, notice, open, up, crashes, grants, epochNew, ConsumerVars>>

\* handleGrant third lock section (daemon.go:1151-1158): the replay is
\* consumed and the pump starts, unless the grant changed meanwhile.
GrantCommit(c) ==
  /\ up /\ gr[c].ph = "reply"
  /\ LET p == gr[c].p IN
     IF d.pump = p
     THEN /\ d' = [d EXCEPT !.ring = <<>>, !.lag = FALSE,
                            !.unacked = IF Mode = "ack" THEN Merge(@, SelectSeq(d.ring, LAMBDA x: x > gr[c].from)) ELSE @]
          /\ pumps' = IF gr[c].own THEN pumps ELSE [pumps EXCEPT ![p].st = "run"]
     ELSE UNCHANGED <<d, pumps>>
  /\ gr' = [gr EXCEPT ![c] = GIdle]
  /\ UNCHANGED <<pub, npumps, sock, notice, open, up, crashes, grants, epochNew, ConsumerVars>>

\* Server.handleConn sees EOF once handleGrant has returned (server.go:236),
\* then ConnClosed detaches (daemon.go:677).
ConnClosed(c) ==
  /\ up /\ open[c] /\ ~alive[c] /\ gr[c].ph = "idle"
  /\ open' = [open EXCEPT ![c] = FALSE]
  /\ IF d.owner = c
     THEN LET r == DetachR(d, pumps) IN d' = r.d /\ pumps' = r.p
     ELSE UNCHANGED <<d, pumps>>
  /\ UNCHANGED <<pub, npumps, gr, sock, notice, up, crashes, grants, epochNew, ConsumerVars>>

\* notifyDetached's goroutine (daemon.go:1409).
WriteNotice(c) ==
  /\ up /\ notice[c]
  /\ \/ /\ alive[c] /\ open[c] /\ Len(sock[c]) < SockCap
        /\ sock' = [sock EXCEPT ![c] = Append(@, DF)]
     \/ /\ ~alive[c] \/ ~open[c]
        /\ UNCHANGED sock
  /\ notice' = [notice EXCEPT ![c] = FALSE]
  /\ UNCHANGED <<pub, d, pumps, npumps, gr, open, up, crashes, grants, epochNew, ConsumerVars>>

\* The consumer reads one frame: readLoop -> pushQueue -> drain -> deliver.
\* Before the first granted reply on a connection the handle does not exist,
\* so events wait in the push queue and are delivered after it (StartAgent,
\* DetectReady closes ready). A re-claim on a live handle delivers at once.
Read(c) ==
  /\ alive[c] /\ sock[c] # <<>>
  /\ LET f == Head(sock[c]) IN
     /\ sock' = [sock EXCEPT ![c] = Tail(@)]
     /\ CASE f.k = "E" ->
               IF cl[c].granted
               THEN SetSt(ApplyOne(St, c, f.e)) /\ UNCHANGED <<cl, epochNew>>
               ELSE /\ cl' = [cl EXCEPT ![c].held = Append(@, f.e)]
                    /\ UNCHANGED <<applied, cursor, knows, reported, bad, epochNew>>
          [] f.k = "G" ->
               \* upto is the high-water mark at the replay snapshot; what
               \* follows the reply comes from the pump. "seq"/"ack" compare
               \* it with the cursor; a lagged reply covers what is missing.
               LET told == (Mode # "current" \/ LagReachesApp) /\ f.lag
                   heldS == Range(cl[c].held)
                   missing == {x \in 1..f.upto : x \notin AppliedBy(applied) /\ x \notin heldS}
                   hw == IF Mode # "current" /\ knows[c]
                         THEN {x \in missing : x > cursor[c]} ELSE {}
                   \* A fresh incarnation takes its baseline from its first
                   \* reply: the replay's first seq, or the high-water mark.
                   base == IF cl[c].held # <<>> THEN Head(cl[c].held) - 1 ELSE f.upto
                   st0 == [St EXCEPT !.reported = @ \cup hw \cup (IF told THEN missing ELSE {}),
                                     !.knows = IF Mode # "current" THEN [@ EXCEPT ![c] = TRUE] ELSE @,
                                     !.cursor = IF Mode # "current" /\ ~knows[c]
                                                THEN [@ EXCEPT ![c] = IF base > @ THEN base ELSE @] ELSE @]
               IN /\ SetSt(ApplyAll(st0, c, cl[c].held))
                  /\ cl' = [cl EXCEPT ![c].held = <<>>, ![c].granted = TRUE]
                  /\ epochNew' = FALSE
          [] f.k = "D" ->
               /\ cl' = [cl EXCEPT ![c].want = TRUE]
               /\ UNCHANGED <<applied, cursor, knows, reported, bad, epochNew>>
  /\ UNCHANGED <<pub, d, pumps, npumps, gr, notice, open, up, crashes, grants, alive, born>>

\* "ack" only: the library acknowledges what it delivered, cumulatively.
AckSend(c) ==
  /\ Mode = "ack" /\ up /\ alive[c] /\ open[c] /\ d.owner = c /\ cursor[c] > d.acked
  /\ d' = [d EXCEPT !.acked = cursor[c], !.unacked = SelectSeq(@, LAMBDA x: x > cursor[c])]
  /\ UNCHANGED <<pub, pumps, npumps, gr, sock, notice, open, up, crashes, grants, epochNew, ConsumerVars>>

\* The consumer process dies. Bytes the kernel accepted for it, and anything
\* in its push queue, die with it.
Die(c) ==
  /\ alive[c]
  /\ alive' = [alive EXCEPT ![c] = FALSE]
  /\ sock' = [sock EXCEPT ![c] = <<>>]
  /\ cl' = [cl EXCEPT ![c].held = <<>>]
  /\ UNCHANGED <<pub, d, pumps, npumps, gr, notice, open, up, crashes, grants, epochNew,
                 born, applied, cursor, knows, reported, bad>>

\* The next incarnation starts (Jevons restarts) and grants by name.
Born(c) ==
  /\ c > 1 /\ ~born[c] /\ born[c - 1] /\ ~alive[c - 1]
  /\ born' = [born EXCEPT ![c] = TRUE]
  /\ alive' = [alive EXCEPT ![c] = TRUE]
  /\ open' = [open EXCEPT ![c] = up]
  /\ cursor' = [cursor EXCEPT ![c] = IF DurableCursor THEN cursor[c - 1] ELSE 0]
  /\ knows' = [knows EXCEPT ![c] = DurableCursor]
  /\ cl' = [cl EXCEPT ![c] = CFresh]
  /\ UNCHANGED <<pub, d, pumps, npumps, gr, sock, notice, up, crashes, grants, epochNew,
                 applied, reported, bad>>

\* The daemon process dies (a crash, or launchd restarting it). Its rings,
\* pumps and grants are memory. The consumer can still read what the kernel
\* already holds for it; the seat's sidecar dies on its lifeline.
BrokerCrash ==
  /\ up /\ crashes < BrokerCrashes
  /\ up' = FALSE /\ crashes' = crashes + 1
  /\ d' = DInit /\ pumps' = [p \in 1..MaxGrants |-> IF pumps[p].st = "none" THEN PNone ELSE [PNone EXCEPT !.st = "done"]]
  /\ gr' = [c \in Conns |-> GIdle] /\ notice' = [c \in Conns |-> FALSE]
  /\ open' = [c \in Conns |-> FALSE]
  /\ epochNew' = TRUE
  /\ UNCHANGED <<pub, npumps, sock, grants, ConsumerVars>>

\* launchd starts the daemon again; resumed seats publish from where the
\* seat's own history left off.
BrokerStart ==
  /\ ~up /\ up' = TRUE
  /\ UNCHANGED <<pub, d, pumps, npumps, gr, sock, notice, open, crashes, grants, epochNew, ConsumerVars>>

\* The consumer saw EOF (drain marks the handle dead, broker_agent.go:575)
\* and grants by name on a new connection.
Reconnect(c) ==
  /\ up /\ alive[c] /\ ~open[c] /\ sock[c] = <<>>
  /\ open' = [open EXCEPT ![c] = TRUE]
  /\ cl' = [cl EXCEPT ![c] = CFresh]
  /\ UNCHANGED <<pub, d, pumps, npumps, gr, sock, notice, up, crashes, grants, epochNew,
                 alive, born, applied, cursor, knows, reported, bad>>

Next ==
  \/ Publish
  \/ \E p \in 1..MaxGrants : Take(p) \/ Commit(p) \/ Write(p) \/ ExitIdle(p)
  \/ \E c \in Conns :
       \/ RequestGrant(c) \/ GrantCheck(c) \/ GrantBind(c) \/ Replay(c) \/ Reply(c)
       \/ GrantCommit(c) \/ ConnClosed(c) \/ WriteNotice(c) \/ Read(c) \/ AckSend(c)
       \/ Die(c) \/ Born(c) \/ Reconnect(c)
  \/ BrokerCrash \/ BrokerStart

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
\* Contracts. "Exactly once to a live consumer, or reported lost" splits into:

\* No incarnation's application receives an event twice.
NoDupIncarnation == ~bad.dupInc
\* No event reaches the application of two incarnations (the logical consumer
\* sees each event once across its own restarts).
NoDupLogical == ~bad.dupLogical
\* The application never receives an event while an earlier one is missing
\* and was not reported: the history arrives in order, and a hole is
\* announced before the stream moves past it.
InOrderOrReported == ~bad.hole
\* Order alone: no event is delivered while an earlier one is still on its
\* way (a hole that is a loss is InOrderOrReported's).
NoReorder == ~bad.reorder

Quiescent ==
  /\ pub = N /\ up
  /\ \E c \in Conns : alive[c] /\ open[c] /\ d.owner = c /\ sock[c] = <<>> /\ cl[c].held = <<>>
  /\ d.ring = <<>> /\ d.draining = 0
  /\ \A p \in 1..MaxGrants : pumps[p].q = <<>> /\ pumps[p].st \in {"none", "run", "done"} /\ pumps[p].w = 0
  /\ \A c \in Conns : gr[c].ph = "idle" /\ ~notice[c]
\* Once everything has drained to a live owner, every event arrived or was
\* reported (catches losses at the tail of the stream).
EndComplete == Quiescent => \A e \in Events : e \in AppliedBy(applied) \cup reported

TypeOK ==
  /\ pub \in 0..N /\ npumps \in 0..MaxGrants /\ grants \in 0..MaxGrants
  /\ d.owner \in 0..Incs /\ d.pump \in 0..MaxGrants /\ d.draining \in 0..MaxGrants
  /\ Len(d.ring) <= RingCap
  /\ \A c \in Conns : Len(sock[c]) <= SockCap

\* State constraint for TLC: held never outgrows what a replay can carry.
Bound == \A c \in Conns : Len(cl[c].held) <= RingCap + 1
=============================================================================

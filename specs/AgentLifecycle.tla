---------------------------- MODULE AgentLifecycle ----------------------------
\* Load-bearing safety model for the claudia broker's agent lifecycle (T2.6
\* preemption + T2.3 shared pool), seeded ahead of the implementation it guards
\* (T2.8). The broker grants claude agents to consumers, preempts them under
\* pressure (SIGTSTP/SIGCONT), reclaims them on heartbeat timeout, reaps idle
\* ones, and finally reclaims their slot:
\*
\*   idle -> granted -> inuse -> paused -> (resumed) inuse -> ... -> idle
\*   idle -> reaped -> reclaimed
\*
\* Three invariants must never break:
\*
\*   Inv_NoDoubleOwnership - no agent is owned by two consumers at once.
\*   Inv_NoHeldReap        - a torn-down agent is owned by nobody.
\*   Inv_NoSendAfterReap   - nobody ever Sends to a torn-down session.
\*
\* The model deliberately separates two views of ownership that the real system
\* also keeps separately, and whose disagreement IS the bug class:
\*
\*   held[a]    - the BROKER's record of who owns agent a.
\*   handles[c] - what consumer c believes it may still Send to.
\*
\* A reaper consults `held`, never `handles` — it cannot see inside a consumer.
\* So any path that drops the broker's record without invalidating the
\* consumer's handle leaves a live handle to an agent the broker is now free to
\* reap, and the next Send lands on a dead session. That is exactly the
\* stale-handle fault injected below.
\*
\* Three faults are modelled and guarded behind CONSTANTS so the mutation
\* configs (AgentLifecycle_mutant_*.cfg) can switch each one on and confirm the
\* matching invariant catches it. An invariant green on known-broken code is
\* worthless; the mutation runs are how we measure that these have teeth
\* (oracle-first rule 12: test the oracle, not just the code).
EXTENDS FiniteSets, Naturals

CONSTANTS
    Agents,              \* set of agent ids, e.g. {a1, a2}
    Consumers,           \* set of consumer ids, e.g. {c1, c2}
    AllowReapWhileHeld,  \* fault: reap a preempted-but-still-held agent
    AllowStealGrant,     \* fault: grant an already-held agent to another consumer
    AllowStaleHandle     \* fault: reclaim without invalidating the consumer's handle

VARIABLES
    held,           \* held[a]    : set of consumers the BROKER records as owning a
    handles,        \* handles[c] : set of agents consumer c believes it may Send to
    state,          \* state[a]   : lifecycle state of a
    sentAfterReap   \* set of agents that received a Send after teardown

vars == <<held, handles, state, sentAfterReap>>

AgentStates == {"idle", "granted", "inuse", "paused", "reaped", "reclaimed"}

\* States in which the underlying claude session no longer exists.
TornDown == {"reaped", "reclaimed"}

TypeOK ==
    /\ held \in [Agents -> SUBSET Consumers]
    /\ handles \in [Consumers -> SUBSET Agents]
    /\ state \in [Agents -> AgentStates]
    /\ sentAfterReap \subseteq Agents

Init ==
    /\ held = [a \in Agents |-> {}]
    /\ handles = [c \in Consumers |-> {}]
    /\ state = [a \in Agents |-> "idle"]
    /\ sentAfterReap = {}

\* ---- Correct transitions -------------------------------------------------

\* The broker grants an idle, unheld agent to a consumer: it records ownership
\* and the consumer receives a handle.
Grant(a, c) ==
    /\ state[a] = "idle"
    /\ held[a] = {}
    /\ held' = [held EXCEPT ![a] = {c}]
    /\ handles' = [handles EXCEPT ![c] = handles[c] \cup {a}]
    /\ state' = [state EXCEPT ![a] = "granted"]
    /\ UNCHANGED sentAfterReap

\* The consumer starts using its granted agent.
Begin(a, c) ==
    /\ state[a] = "granted"
    /\ c \in held[a]
    /\ state' = [state EXCEPT ![a] = "inuse"]
    /\ UNCHANGED <<held, handles, sentAfterReap>>

\* A consumer Sends on a handle it holds. This is the observable the
\* Send-after-reap invariant is stated over: if the session has already been
\* torn down, the send is recorded as the fault it is. Nothing else changes —
\* a Send is not a lifecycle transition.
Send(a, c) ==
    /\ a \in handles[c]
    /\ IF state[a] \in TornDown
       THEN sentAfterReap' = sentAfterReap \cup {a}
       ELSE UNCHANGED sentAfterReap
    /\ UNCHANGED <<held, handles, state>>

\* The broker preempts an in-use agent (SIGTSTP). It stays held by its consumer.
Pause(a) ==
    /\ state[a] = "inuse"
    /\ state' = [state EXCEPT ![a] = "paused"]
    /\ UNCHANGED <<held, handles, sentAfterReap>>

\* The broker resumes a preempted agent (SIGCONT).
Resume(a) ==
    /\ state[a] = "paused"
    /\ state' = [state EXCEPT ![a] = "inuse"]
    /\ UNCHANGED <<held, handles, sentAfterReap>>

\* A consumer returns the agent to the pool, dropping its own handle as it goes.
Release(a, c) ==
    /\ c \in held[a]
    /\ state[a] \in {"granted", "inuse", "paused"}
    /\ held' = [held EXCEPT ![a] = held[a] \ {c}]
    /\ handles' = [handles EXCEPT ![c] = handles[c] \ {a}]
    /\ state' = [state EXCEPT ![a] = "idle"]
    /\ UNCHANGED sentAfterReap

\* The broker reclaims an agent whose consumer heartbeat lapsed (>30s absence).
\* Correctness hinges on invalidating the consumer's handle in the same step:
\* the broker is dropping its record unilaterally, so a handle left behind is
\* one nobody will ever clean up.
Reclaim(a, c) ==
    /\ c \in held[a]
    /\ held' = [held EXCEPT ![a] = held[a] \ {c}]
    /\ handles' = IF AllowStaleHandle
                  THEN handles                                     \* fault: handle survives
                  ELSE [handles EXCEPT ![c] = handles[c] \ {a}]
    /\ state' = [state EXCEPT ![a] = "idle"]
    /\ UNCHANGED sentAfterReap

\* Reaping: only idle agents the broker records as unheld are torn down. The
\* reaper cannot inspect consumer handles, so this precondition is deliberately
\* stated over `held` alone — strengthening it with `handles` would model a
\* reaper that does not exist and would hide the stale-handle fault.
Reap(a) ==
    /\ state[a] = "idle"
    /\ held[a] = {}
    /\ state' = [state EXCEPT ![a] = "reaped"]
    /\ UNCHANGED <<held, handles, sentAfterReap>>

\* The reaped agent's slot is returned to the pool. Terminal.
Free(a) ==
    /\ state[a] = "reaped"
    /\ state' = [state EXCEPT ![a] = "reclaimed"]
    /\ UNCHANGED <<held, handles, sentAfterReap>>

\* ---- Injected faults (disabled in the correct config) --------------------

\* Fault: reap a preempted agent a consumer still holds. Models a reaper racing
\* the SIGTSTP preemption path. Trips Inv_NoHeldReap.
BuggyReapWhileHeld(a) ==
    /\ AllowReapWhileHeld
    /\ state[a] = "paused"
    /\ held[a] # {}
    /\ state' = [state EXCEPT ![a] = "reaped"]
    /\ UNCHANGED <<held, handles, sentAfterReap>>

\* Fault: grant an already-held agent to a second consumer. Models a
\* heartbeat-timeout race that re-grants without first reclaiming. Trips
\* Inv_NoDoubleOwnership.
BuggyStealGrant(a, c) ==
    /\ AllowStealGrant
    /\ held[a] # {}
    /\ c \notin held[a]
    /\ held' = [held EXCEPT ![a] = held[a] \cup {c}]
    /\ handles' = [handles EXCEPT ![c] = handles[c] \cup {a}]
    /\ UNCHANGED <<state, sentAfterReap>>

\* The third fault needs no action of its own: AllowStaleHandle weakens Reclaim
\* above, leaving the consumer holding a handle the broker has forgotten. The
\* agent then reaps legitimately and the next Send lands on a dead session,
\* tripping Inv_NoSendAfterReap.

Next ==
    \/ \E a \in Agents, c \in Consumers : Grant(a, c)
    \/ \E a \in Agents, c \in Consumers : Begin(a, c)
    \/ \E a \in Agents, c \in Consumers : Send(a, c)
    \/ \E a \in Agents, c \in Consumers : Release(a, c)
    \/ \E a \in Agents, c \in Consumers : Reclaim(a, c)
    \/ \E a \in Agents : Pause(a)
    \/ \E a \in Agents : Resume(a)
    \/ \E a \in Agents : Reap(a)
    \/ \E a \in Agents : Free(a)
    \/ \E a \in Agents : BuggyReapWhileHeld(a)
    \/ \E a \in Agents, c \in Consumers : BuggyStealGrant(a, c)
    \* Terminal stutter: once every agent is reclaimed and no handle survives,
    \* no real action is enabled; this keeps TLC from reporting a spurious
    \* deadlock on that end state.
    \/ /\ \A a \in Agents : state[a] = "reclaimed"
       /\ \A c \in Consumers : handles[c] = {}
       /\ UNCHANGED vars

Spec == Init /\ [][Next]_vars

\* ---- Load-bearing safety invariants --------------------------------------

\* No agent is owned by two consumers at once.
Inv_NoDoubleOwnership == \A a \in Agents : Cardinality(held[a]) <= 1

\* A torn-down agent is owned by nobody, per the broker's own record.
Inv_NoHeldReap == \A a \in Agents : state[a] \in TornDown => held[a] = {}

\* Nobody ever Sends to a torn-down session. Stated over the observable rather
\* than over the handle set, so it fails on the act, not on a proxy for it.
Inv_NoSendAfterReap == sentAfterReap = {}
===============================================================================

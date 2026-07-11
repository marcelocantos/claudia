---------------------------- MODULE AgentLifecycle ----------------------------
\* Load-bearing safety model for the claudia broker's agent lifecycle (T2.6
\* preemption + T2.3 shared pool). The broker hands claude agents to consumers,
\* preempts them under pressure (SIGTSTP/SIGCONT), reclaims them on heartbeat
\* timeout, and reaps idle ones. Two invariants must never break:
\*
\*   Inv_NoDoubleOwnership - no agent is owned by two consumers at once.
\*   Inv_NoHeldReap        - a reaped agent is held by nobody, so no consumer
\*                           can still Send to a torn-down session.
\*
\* Two faults are modelled and guarded behind CONSTANTS so the mutation configs
\* (AgentLifecycle_mutant_*.cfg) can switch each one on and confirm the matching
\* invariant catches it. An invariant green on known-broken code is worthless;
\* the mutation runs are how we measure that these have teeth (oracle-first
\* rule 12: test the oracle, not just the code).
EXTENDS FiniteSets, Naturals

CONSTANTS
    Agents,              \* set of agent ids, e.g. {a1, a2}
    Consumers,           \* set of consumer ids, e.g. {c1, c2}
    AllowReapWhileHeld,  \* fault: reap a preempted-but-still-held agent
    AllowStealGrant      \* fault: grant an already-held agent to another consumer

VARIABLES
    held,   \* held[a] : set of consumers that believe they own a
    state   \* state[a] : "idle" | "inuse" | "paused" | "reaped"

vars == <<held, state>>

AgentStates == {"idle", "inuse", "paused", "reaped"}

TypeOK ==
    /\ held \in [Agents -> SUBSET Consumers]
    /\ state \in [Agents -> AgentStates]

Init ==
    /\ held = [a \in Agents |-> {}]
    /\ state = [a \in Agents |-> "idle"]

\* ---- Correct transitions -------------------------------------------------

\* The broker grants an idle, unheld agent to a consumer.
Acquire(a, c) ==
    /\ state[a] = "idle"
    /\ held[a] = {}
    /\ held' = [held EXCEPT ![a] = {c}]
    /\ state' = [state EXCEPT ![a] = "inuse"]

\* A consumer returns the agent to the pool.
Release(a, c) ==
    /\ c \in held[a]
    /\ state[a] \in {"inuse", "paused"}
    /\ held' = [held EXCEPT ![a] = held[a] \ {c}]
    /\ state' = [state EXCEPT ![a] = "idle"]

\* The broker preempts an in-use agent (SIGTSTP). It stays held by its consumer.
Pause(a) ==
    /\ state[a] = "inuse"
    /\ state' = [state EXCEPT ![a] = "paused"]
    /\ UNCHANGED held

\* The broker resumes a preempted agent (SIGCONT).
Resume(a) ==
    /\ state[a] = "paused"
    /\ state' = [state EXCEPT ![a] = "inuse"]
    /\ UNCHANGED held

\* The broker reclaims an agent whose consumer heartbeat lapsed (>30s absence).
Reclaim(a, c) ==
    /\ c \in held[a]
    /\ held' = [held EXCEPT ![a] = held[a] \ {c}]
    /\ state' = [state EXCEPT ![a] = "idle"]

\* Correct reaping: only idle, unheld agents become reaped (terminal).
Reap(a) ==
    /\ state[a] = "idle"
    /\ held[a] = {}
    /\ state' = [state EXCEPT ![a] = "reaped"]
    /\ UNCHANGED held

\* ---- Injected faults (disabled in the correct config) --------------------

\* Fault: reap a preempted agent a consumer still holds. Models a reaper racing
\* the SIGTSTP preemption path — the consumer keeps Sending to a session the
\* broker has already torn down. Trips Inv_NoHeldReap.
BuggyReapWhileHeld(a) ==
    /\ AllowReapWhileHeld
    /\ state[a] = "paused"
    /\ held[a] # {}
    /\ state' = [state EXCEPT ![a] = "reaped"]
    /\ UNCHANGED held

\* Fault: grant an already-held agent to a second consumer. Models a
\* heartbeat-timeout race that re-grants without first reclaiming. Trips
\* Inv_NoDoubleOwnership.
BuggyStealGrant(a, c) ==
    /\ AllowStealGrant
    /\ held[a] # {}
    /\ c \notin held[a]
    /\ held' = [held EXCEPT ![a] = held[a] \cup {c}]
    /\ UNCHANGED state

Next ==
    \/ \E a \in Agents, c \in Consumers : Acquire(a, c)
    \/ \E a \in Agents, c \in Consumers : Release(a, c)
    \/ \E a \in Agents, c \in Consumers : Reclaim(a, c)
    \/ \E a \in Agents : Pause(a)
    \/ \E a \in Agents : Resume(a)
    \/ \E a \in Agents : Reap(a)
    \/ \E a \in Agents : BuggyReapWhileHeld(a)
    \/ \E a \in Agents, c \in Consumers : BuggyStealGrant(a, c)
    \* Terminal stutter: once every agent is reaped no real action is enabled;
    \* this keeps TLC from reporting a spurious deadlock on that end state.
    \/ ((\A a \in Agents : state[a] = "reaped") /\ UNCHANGED vars)

Spec == Init /\ [][Next]_vars

\* ---- Load-bearing safety invariants --------------------------------------

Inv_NoDoubleOwnership == \A a \in Agents : Cardinality(held[a]) <= 1

Inv_NoHeldReap == \A a \in Agents : state[a] = "reaped" => held[a] = {}
===============================================================================

---------------------------- MODULE Ownership ----------------------------
EXTENDS Naturals, FiniteSets
\* Copyright 2026 Marcelo Cantos; SPDX-License-Identifier: Apache-2.0
\* Copied unchanged from the 2026-11-21 Codex partial (analysis-partials-2026-11-21,
\* docs/analysis/turn-delivery-model/Ownership.tla) after review on 2026-11-22.
\* One consumer, one broker, one sidecar incarnation at a time, one seat.
\* At most MaxGroups process groups across MaxEpochs sidecar incarnations;
\* each group has one shell leader and one descendant which stays in PGID.
\* Kill delivery/reaping is collapsed atomically (optimistic abstraction).
\* No provider, PID reuse, setsid escape, or Windows behavior is represented.
CONSTANTS MaxGroups, MaxEpochs, AllowLeaderExit, AllowSidecarCrash,
          ForgetExitedLeader
VARIABLES broker, sidecar, consumer, seat, epoch, born, groupEpoch,
          leaders, descendants, tracked, deadEpochs
vars == <<broker, sidecar, consumer, seat, epoch, born, groupEpoch,
          leaders, descendants, tracked, deadEpochs>>
Groups == 1..MaxGroups
Epochs == 1..MaxEpochs
Live == leaders \cup descendants
EpochLive(e) == {g \in Live : groupEpoch[g] = e}

Init == /\ broker = TRUE /\ sidecar = TRUE /\ consumer = TRUE
        /\ seat = TRUE /\ epoch = 1 /\ born = {}
        /\ groupEpoch = [g \in Groups |-> 1]
        /\ leaders = {} /\ descendants = {} /\ tracked = {}
        /\ deadEpochs = {}

Spawn(g) == /\ broker /\ sidecar /\ seat /\ g \notin born
            /\ born' = born \cup {g}
            /\ groupEpoch' = [groupEpoch EXCEPT ![g] = epoch]
            /\ leaders' = leaders \cup {g}
            /\ tracked' = tracked \cup {g}
            /\ UNCHANGED <<broker, sidecar, consumer, seat, epoch,
                           descendants, deadEpochs>>

Fork(g) == /\ g \in leaders /\ g \notin descendants
           /\ descendants' = descendants \cup {g}
           /\ UNCHANGED <<broker, sidecar, consumer, seat, epoch, born,
                          groupEpoch, leaders, tracked, deadEpochs>>

\* Shell exits after redirecting the descendant's standard streams.
LeaderExit(g) == /\ AllowLeaderExit /\ g \in leaders
                 /\ leaders' = leaders \ {g}
                 /\ tracked' = IF ForgetExitedLeader THEN tracked \ {g}
                                ELSE tracked
                 /\ UNCHANGED <<broker, sidecar, consumer, seat, epoch,
                                born, groupEpoch, descendants, deadEpochs>>

\* Consumer connections do not own the sidecar's OS processes.
ConsumerDies == /\ consumer /\ consumer' = FALSE
                /\ UNCHANGED <<broker, sidecar, seat, epoch, born,
                               groupEpoch, leaders, descendants, tracked, deadEpochs>>
Reclaim == /\ ~consumer /\ broker /\ sidecar /\ consumer' = TRUE
           /\ UNCHANGED <<broker, sidecar, seat, epoch, born,
                          groupEpoch, leaders, descendants, tracked, deadEpochs>>

BrokerDies == /\ broker /\ broker' = FALSE
              /\ UNCHANGED <<sidecar, consumer, seat, epoch, born,
                             groupEpoch, leaders, descendants, tracked, deadEpochs>>

\* EOF is scheduled later, not atomically with death of the broker.
Lifeline == /\ ~broker /\ sidecar /\ sidecar' = FALSE /\ seat' = FALSE
            /\ leaders' = leaders \ tracked
            /\ descendants' = descendants \ tracked /\ tracked' = {}
            /\ deadEpochs' = deadEpochs \cup {epoch}
            /\ UNCHANGED <<broker, consumer, epoch, born, groupEpoch>>

SignalSidecar == /\ broker /\ sidecar /\ sidecar' = FALSE /\ seat' = FALSE
                 /\ leaders' = leaders \ tracked
                 /\ descendants' = descendants \ tracked /\ tracked' = {}
                 /\ deadEpochs' = deadEpochs \cup {epoch}
                 /\ UNCHANGED <<broker, consumer, epoch, born, groupEpoch>>

\* SIGKILL cannot run the in-process handler or process-group cleanup.
CrashSidecar == /\ AllowSidecarCrash /\ sidecar
                /\ sidecar' = FALSE /\ seat' = FALSE /\ tracked' = {}
                /\ deadEpochs' = deadEpochs \cup {epoch}
                /\ UNCHANGED <<broker, consumer, epoch, born, groupEpoch,
                               leaders, descendants>>

StopSeat == /\ sidecar /\ seat /\ seat' = FALSE
            /\ leaders' = leaders \ tracked
            /\ descendants' = descendants \ tracked /\ tracked' = {}
            /\ UNCHANGED <<broker, sidecar, consumer, epoch, born,
                           groupEpoch, deadEpochs>>

Restart == /\ ~sidecar /\ epoch < MaxEpochs
           /\ broker' = TRUE /\ sidecar' = TRUE /\ seat' = TRUE
           /\ epoch' = epoch + 1
           /\ UNCHANGED <<consumer, born, groupEpoch, leaders, descendants,
                          tracked, deadEpochs>>

Next == \/ \E g \in Groups : Spawn(g) \/ Fork(g) \/ LeaderExit(g)
        \/ ConsumerDies \/ Reclaim \/ BrokerDies \/ Lifeline
        \/ SignalSidecar \/ CrashSidecar \/ StopSeat \/ Restart
Spec == Init /\ [][Next]_vars /\ WF_vars(Lifeline)

TypeOK == /\ broker \in BOOLEAN /\ sidecar \in BOOLEAN
          /\ consumer \in BOOLEAN /\ seat \in BOOLEAN
          /\ epoch \in Epochs /\ born \subseteq Groups
          /\ groupEpoch \in [Groups -> Epochs]
          /\ leaders \subseteq born /\ descendants \subseteq born
          /\ tracked \subseteq born /\ deadEpochs \subseteq Epochs
NoToolOutlivesSidecar == \A e \in deadEpochs : EpochLive(e) = {}
NoToolAfterStop == ~seat => EpochLive(epoch) = {}
StrictNoChildOutlivesOwner == (sidecar => broker) /\ NoToolOutlivesSidecar
EventualCleanup == \A e \in Epochs :
                      (e \in deadEpochs) ~> (EpochLive(e) = {})
BrokerEOFEventuallyHandled == (~broker /\ sidecar) ~> ~sidecar
=============================================================================

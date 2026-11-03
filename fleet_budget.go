// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// A budget limits what a subtree spends (🎯T248.2). The owner chooses the amount;
// the kernel stops new turns once it is spent and says why. A turn in progress
// finishes. An agent can put a budget on a child, which only narrows what the
// child may spend, because every budget above it still applies; nothing an agent
// does can raise one.

// FleetBudget is a limit in up to three dimensions. Zero is unlimited.
type FleetBudget struct {
	// Tokens counts input, output and cache-write tokens where the provider
	// reports them. Cache reads are not counted.
	Tokens int64 `json:"tokens,omitempty"`
	// Turns counts every turn in the subtree, whatever the provider reports.
	Turns int `json:"turns,omitempty"`
	// Seconds is wall-clock time from when the budget was set.
	Seconds float64 `json:"seconds,omitempty"`
}

// fleetBudgetIDPattern matches budget IDs, which agent names may not, so that
// the owner raising a budget by ID cannot mean an agent.
var fleetBudgetIDPattern = regexp.MustCompile(`^b[0-9]+$`)

func (b FleetBudget) zero() bool { return b.Tokens == 0 && b.Turns == 0 && b.Seconds == 0 }

func (b FleetBudget) validate() error {
	if b.Tokens < 0 || b.Turns < 0 || b.Seconds < 0 {
		return errors.New("a budget cannot be negative")
	}
	return nil
}

// FleetBudgetStatus is one budget as it stands.
type FleetBudgetStatus struct {
	ID          string      `json:"id"`
	Holder      string      `json:"holder"`
	Limits      FleetBudget `json:"limits"`
	SpentTokens int64       `json:"spent_tokens"`
	SpentTurns  int         `json:"spent_turns"`
	// SecondsLeft is set when there is a time limit.
	SecondsLeft *float64 `json:"seconds_left,omitempty"`
	// Spent names the limit reached, or is empty.
	Spent string `json:"spent,omitempty"`
}

type fleetBudgetState struct {
	Holder      string      `json:"holder"`
	Limits      FleetBudget `json:"limits"`
	Started     time.Time   `json:"started"`
	SpentTokens int64       `json:"spent_tokens,omitempty"`
	SpentTurns  int         `json:"spent_turns,omitempty"`
	// Notified means the owner has heard this budget is spent.
	Notified bool `json:"notified,omitempty"`
}

// spentBy names the limit reached at now, or returns "".
func (b *fleetBudgetState) spentBy(now time.Time) string {
	switch {
	case b.Limits.Tokens > 0 && b.SpentTokens >= b.Limits.Tokens:
		return fmt.Sprintf("tokens %d of %d", b.SpentTokens, b.Limits.Tokens)
	case b.Limits.Turns > 0 && b.SpentTurns >= b.Limits.Turns:
		return fmt.Sprintf("turns %d of %d", b.SpentTurns, b.Limits.Turns)
	case b.Limits.Seconds > 0 && !now.Before(b.Started.Add(time.Duration(b.Limits.Seconds*float64(time.Second)))):
		return fmt.Sprintf("time %s", fleetDuration(time.Duration(b.Limits.Seconds*float64(time.Second))))
	}
	return ""
}

func (k *FleetKernel) newBudgetLocked(group *fleetGroupState, holder string, limits FleetBudget) string {
	group.NextID++
	id := fmt.Sprintf("b%d", group.NextID)
	if group.Budgets == nil {
		group.Budgets = map[string]*fleetBudgetState{}
	}
	group.Budgets[id] = &fleetBudgetState{Holder: holder, Limits: limits, Started: k.clock.Now()}
	return id
}

// SetBudget sets the limits on a budget, named by its ID or by the agent that
// holds it, creating one over an agent and its descendants if it holds none. A
// budget's ID outlives its holder, so a spent budget can still be raised after
// the holder has gone. Raising a spent budget lets waiting work go on, and a
// time limit counts from now. Only the owner can do this.
func (k *FleetKernel) SetBudget(fleet, target string, limits FleetBudget) error {
	if err := limits.validate(); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	group, err := k.groupLocked(fleet)
	if err != nil {
		return err
	}
	agent := target
	if b := group.Budgets[target]; b != nil {
		b.Limits, b.Notified, b.Started = limits, false, k.clock.Now()
		agent = b.Holder
	} else if a := group.Agents[target]; a == nil {
		return fmt.Errorf("no agent or budget named %q", target)
	} else if b := group.Budgets[a.OwnBudget]; b != nil {
		b.Limits, b.Notified, b.Started = limits, false, k.clock.Now()
	} else {
		id := k.newBudgetLocked(group, agent, limits)
		a.OwnBudget = id
		a.Budgets = append(a.Budgets, id)
		for _, other := range group.Agents {
			if fleetDescends(group, other.Name, agent) {
				other.Budgets = append(other.Budgets, id)
			}
		}
	}
	k.eventLocked(fleet, &FleetEvent{Kind: FleetEventBudget, Agent: agent, Text: fmt.Sprintf("tokens %d, turns %d, seconds %g", limits.Tokens, limits.Turns, limits.Seconds)})
	for key, rt := range k.running {
		if strings.HasPrefix(key, fleet+"/") {
			rt.wake.Broadcast()
		}
	}
	return k.saveLocked()
}

// blockedLocked reports whether a's next turn must wait for a budget, and tells
// the owner once per budget that it is spent. The caller saves. k.mu held.
func (k *FleetKernel) blockedLocked(fleet string, group *fleetGroupState, a *fleetAgentState) bool {
	now := k.clock.Now()
	for _, id := range a.Budgets {
		b := group.Budgets[id]
		if b == nil {
			continue
		}
		reason := b.spentBy(now)
		if reason == "" {
			continue
		}
		if !b.Notified {
			b.Notified = true
			k.deliverLocked(fleet, &FleetMessage{Kind: FleetResponse, From: FleetBudgetSender, To: FleetOwner,
				Text: fmt.Sprintf("Budget %s, on agent %s's tree, is spent (%s). Agents under it take no new turns, and messages to them wait, until the owner raises budget %s.", id, b.Holder, reason, id)})
			if err := k.saveLocked(); err != nil {
				k.logf("fleet: save after budget notice", "fleet", fleet, "err", err)
			}
		}
		return true
	}
	return false
}

// chargeTurnLocked counts a turn against every budget over a, when it starts, so
// that agents starting turns at once cannot all slip under the same limit.
func (k *FleetKernel) chargeTurnLocked(group *fleetGroupState, a *fleetAgentState) {
	for _, id := range a.Budgets {
		if b := group.Budgets[id]; b != nil {
			b.SpentTurns++
		}
	}
}

// chargeTokensLocked records a finished turn's tokens. Tokens can overshoot a
// limit by what turns already in flight spend, because nobody knows a turn's
// cost before it runs.
func (k *FleetKernel) chargeTokensLocked(group *fleetGroupState, a *fleetAgentState, tokens int64) {
	for _, id := range a.Budgets {
		if b := group.Budgets[id]; b != nil {
			b.SpentTokens += tokens
		}
	}
}

// fleetTokens is what a budget counts of a seat's cumulative usage.
func fleetTokens(u Usage) int64 {
	return int64(u.InputTokens) + int64(u.OutputTokens) + int64(u.CacheCreationInputTokens)
}

// dropUnusedBudgetsLocked forgets budgets no agent is under any more.
func dropUnusedBudgetsLocked(group *fleetGroupState) {
	used := map[string]bool{}
	for _, a := range group.Agents {
		for _, id := range a.Budgets {
			used[id] = true
		}
	}
	for id := range group.Budgets {
		if !used[id] {
			delete(group.Budgets, id)
		}
	}
}

func (k *FleetKernel) budgetStatusLocked(group *fleetGroupState) []FleetBudgetStatus {
	now := k.clock.Now()
	out := make([]FleetBudgetStatus, 0, len(group.Budgets))
	for id, b := range group.Budgets {
		st := FleetBudgetStatus{ID: id, Holder: b.Holder, Limits: b.Limits, SpentTokens: b.SpentTokens, SpentTurns: b.SpentTurns, Spent: b.spentBy(now)}
		if b.Limits.Seconds > 0 {
			left := max(b.Started.Add(time.Duration(b.Limits.Seconds*float64(time.Second))).Sub(now).Seconds(), 0)
			st.SecondsLeft = &left
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

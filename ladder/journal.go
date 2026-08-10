// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"fmt"
	"sync"
)

// Episode is one ladder walk, recorded so it can be RE-RUN rather than
// merely counted.
//
// The reads matter more than they look. Without them this is a summary
// statistic and the ablation oracle cannot ask "what would have happened
// without this rule?" — which is the one question that separates a rule
// that has quietly stopped mattering from one that never mattered. It is
// also the whole reason the reader records at all.
type Episode struct {
	// ID correlates a later outcome with this decision.
	ID string

	// Stack identifies which stack produced it. Evidence pooled across a
	// flavour is only valid if the flavour is homogeneous, and a rule can
	// look excellent on the pool while being wrong for every member;
	// this is what makes that checkable rather than assumed.
	Stack string

	// Fingerprint is the rule set this ran under, so a consolidation
	// pass can tell which episodes reflect the rules it is reasoning
	// about rather than a world that no longer exists.
	Fingerprint string

	// The request as it arrived.
	Kind    string
	Payload any

	// AnsweredBy and Rule are the provenance of the answer.
	AnsweredBy string
	Rule       string
	Verdict    VerdictKind

	// Escalated reports whether the request passed a rung before being
	// answered.
	Escalated bool

	// Reads are what the answering rung looked up, in order, which is
	// what makes this episode replayable.
	Reads []ReadRecord

	// Outcome arrives later, by a second write. Nil means unjudged,
	// which is recorded rather than assumed favourable.
	Outcome *Outcome
}

// Outcome is a judgement about an episode, written when it is known.
type Outcome struct {
	// Delivered reports whether the request produced the work it was
	// for. Claudia cannot compute this.
	Delivered bool
	// Correction names a rule the consumer says decided wrongly. It is
	// the highest-quality demotion signal available and outweighs any
	// amount of consistency.
	Correction bool
	// Note carries the consumer's reason, so a correction explains
	// itself later.
	Note string
}

// Journal is episodic memory: where stacks deposit what happened.
//
// It is an interface with a no-op default, so a stack that should not
// record is simply not given one — replay and dry runs then record
// nothing without a flag to suppress them.
//
// The hot path WRITES here and READS the rule set, which is the concrete
// reason these are siblings rather than one object.
type Journal interface {
	// Record appends an episode.
	Record(ep *Episode) error
	// Judge attaches an outcome to an earlier episode by correlation id.
	// A decision's consequences may not be known for hours, so this is a
	// second write rather than a field filled in at decision time.
	Judge(episodeID string, out *Outcome) error
	// Episodes returns what is retained, oldest first.
	Episodes() []Episode
}

// NopJournal discards everything. It is the default: recording is opt-in.
type NopJournal struct{}

// Record implements [Journal].
func (NopJournal) Record(*Episode) error { return nil }

// Judge implements [Journal].
func (NopJournal) Judge(string, *Outcome) error { return nil }

// Episodes implements [Journal].
func (NopJournal) Episodes() []Episode { return nil }

// MemoryJournal keeps episodes in memory, bounded by a retention count.
//
// Episodic memory is SUPPOSED to decay — a journal that grows forever is
// a design error rather than a scaling problem — so a bound is part of
// the type rather than an afterthought. Use [RetentionFloor] to size it.
type MemoryJournal struct {
	retain int

	mu       sync.Mutex
	episodes []Episode
	index    map[string]int
}

// NewMemoryJournal returns a journal retaining at most `retain`
// episodes, dropping the oldest first. A retain of zero or less keeps
// everything, which is appropriate only for tests.
func NewMemoryJournal(retain int) *MemoryJournal {
	return &MemoryJournal{retain: retain, index: make(map[string]int)}
}

// Record implements [Journal].
func (j *MemoryJournal) Record(ep *Episode) error {
	if ep == nil {
		return fmt.Errorf("ladder: nil Episode")
	}
	if ep.ID == "" {
		return fmt.Errorf("ladder: an episode needs an ID — a later outcome has nothing to attach to without one")
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	if _, dup := j.index[ep.ID]; dup {
		return fmt.Errorf("ladder: episode %q already recorded", ep.ID)
	}
	j.episodes = append(j.episodes, *ep)
	j.index[ep.ID] = len(j.episodes) - 1
	j.evict()
	return nil
}

// evict drops the oldest episodes past the retention bound. Caller holds
// the lock.
func (j *MemoryJournal) evict() {
	if j.retain <= 0 || len(j.episodes) <= j.retain {
		return
	}
	drop := len(j.episodes) - j.retain
	j.episodes = append([]Episode(nil), j.episodes[drop:]...)
	j.index = make(map[string]int, len(j.episodes))
	for i := range j.episodes {
		j.index[j.episodes[i].ID] = i
	}
}

// Judge implements [Journal].
func (j *MemoryJournal) Judge(episodeID string, out *Outcome) error {
	if out == nil {
		return fmt.Errorf("ladder: nil Outcome")
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	i, ok := j.index[episodeID]
	if !ok {
		// An outcome for an episode that has already decayed is not an
		// error: the label simply arrived after its evidence expired.
		return nil
	}
	cp := *out
	j.episodes[i].Outcome = &cp
	return nil
}

// Episodes implements [Journal].
func (j *MemoryJournal) Episodes() []Episode {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]Episode(nil), j.episodes...)
}

// RetentionFloor is the smallest number of episodes worth keeping, given
// how hard the thresholds are to reach.
//
// Retention is DERIVED rather than picked: the journal must hold enough
// episodes to reach the largest promotion gate for the rarest class
// intended for promotion, with margin. rarestShare is that class's share
// of traffic (0.05 for a class that is one request in twenty); margin
// multiplies the result.
//
// The runtime computes the floor; enforcing it is the consumer's, since
// only it knows what an episode costs to keep.
func RetentionFloor(t *Thresholds, rarestShare, margin float64) int {
	if t == nil {
		t = ProductionThresholds()
	}
	if rarestShare <= 0 || rarestShare > 1 {
		rarestShare = 1
	}
	if margin < 1 {
		margin = 1
	}
	gate := max(t.AgentToHybridRuns, t.HybridToDeterministicRuns)
	return int(float64(gate) / rarestShare * margin)
}

// EpisodeFrom builds an episode from a completed walk. Payload is
// carried by reference, so a consumer holding large payloads should
// substitute a handle before recording.
func EpisodeFrom(id, stack, fingerprint string, req *Request, res *Result) *Episode {
	ep := &Episode{
		ID: id, Stack: stack, Fingerprint: fingerprint,
		Kind: req.Kind, Payload: req.Payload,
	}
	if res == nil {
		return ep
	}
	ep.AnsweredBy = res.Layer
	ep.Escalated = res.Escalated()
	if res.Verdict != nil {
		ep.Rule = res.Verdict.Rule
		ep.Verdict = res.Verdict.Kind
	}
	// The reads of the rung that answered are the ones that make this
	// episode replayable.
	for _, rung := range res.Rungs {
		if rung.Verdict != nil && rung.Layer == res.Layer {
			ep.Reads = rung.Reads
		}
	}
	return ep
}

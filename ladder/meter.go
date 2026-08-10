// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"maps"
	"slices"
	"sort"
	"sync"
)

// Observation is one request's outcome, offered to a [Meter].
type Observation struct {
	Request *Request
	Result  *Result
	// Err is what [Ladder.Evaluate] returned, if anything.
	Err error
	// Delivered reports whether the request produced the work it was
	// for. Claudia cannot know this — whether a decision was any good
	// is the consumer's judgement — and metering cost without it is
	// exactly the measurement a system optimising away its own
	// oversight would pass.
	Delivered bool
}

// ClassStats is what has been observed about one request class.
type ClassStats struct {
	Requests int
	// AnsweredBy counts answers per layer.
	AnsweredBy map[string]int
	// Escalated counts requests that passed at least one rung before
	// being answered.
	Escalated int
	// ReachedModel counts requests answered by a layer the consumer
	// declared model-backed.
	ReachedModel int
	// Delivered counts requests that produced their work.
	Delivered int
	// Unanswered counts requests that fell off the top of the ladder.
	Unanswered int
	// Defects counts rungs that returned neither a verdict nor an
	// error. A rung that passes through silently is the one forbidden
	// outcome, so this is expected to be zero and is worth an alert
	// when it is not.
	Defects int
	// RungFailures counts rungs that errored and escalated.
	RungFailures int
}

// EscalationRate is the share of requests that passed at least one rung.
//
// It is not an efficiency number. A collapse toward zero is
// indistinguishable from the ladder optimising away its own oversight,
// and is a defect signal until proven otherwise.
func (c *ClassStats) EscalationRate() float64 {
	if c.Requests == 0 {
		return 0
	}
	return float64(c.Escalated) / float64(c.Requests)
}

// ModelShare is the share of requests a model answered. This is the
// number the whole design is trying to reduce, and it means nothing
// without [ClassStats.DeliveredShare] beside it.
func (c *ClassStats) ModelShare() float64 {
	if c.Requests == 0 {
		return 0
	}
	return float64(c.ReachedModel) / float64(c.Requests)
}

// DeliveredShare is the share of requests that produced their work. It
// is the counterweight: cost falling while this falls with it is not a
// saving, it is the failure mode.
func (c *ClassStats) DeliveredShare() float64 {
	if c.Requests == 0 {
		return 0
	}
	return float64(c.Delivered) / float64(c.Requests)
}

// MeterConfig configures a [Meter].
type MeterConfig struct {
	// ModelLayers names the rungs that are model-backed. Claudia does
	// not infer this: a rung is an interface, and what sits behind it
	// is the consumer's business.
	ModelLayers []string
}

// Meter accumulates what the ladder did, so a consumer can wire cost and
// oversight to its own accounting.
//
// It counts; it concludes nothing. Whether a falling model share is a
// saving or a symptom depends on delivered work, and whether a given
// escalation rate is healthy depends on the domain.
type Meter struct {
	modelLayers []string

	mu      sync.Mutex
	classes map[string]*ClassStats
}

// NewMeter returns an empty meter.
func NewMeter(cfg *MeterConfig) *Meter {
	m := &Meter{classes: make(map[string]*ClassStats)}
	if cfg != nil {
		m.modelLayers = slices.Clone(cfg.ModelLayers)
	}
	return m
}

// Observe records one request.
func (m *Meter) Observe(o *Observation) {
	if o == nil || o.Request == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.classes[o.Request.Kind]
	if !ok {
		c = &ClassStats{AnsweredBy: make(map[string]int)}
		m.classes[o.Request.Kind] = c
	}
	c.Requests++
	if o.Delivered {
		c.Delivered++
	}

	res := o.Result
	if res == nil {
		c.Unanswered++
		return
	}

	for _, rung := range res.Rungs {
		if rung.Defect != "" {
			c.Defects++
		}
		if rung.Err != nil {
			c.RungFailures++
		}
	}

	if res.Verdict == nil {
		c.Unanswered++
		return
	}
	c.AnsweredBy[res.Layer]++
	if res.Escalated() {
		c.Escalated++
	}
	if slices.Contains(m.modelLayers, res.Layer) {
		c.ReachedModel++
	}
}

// Class returns a copy of one class's stats.
func (m *Meter) Class(kind string) ClassStats {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.classes[kind]
	if !ok {
		return ClassStats{AnsweredBy: map[string]int{}}
	}
	cp := *c
	cp.AnsweredBy = maps.Clone(c.AnsweredBy)
	return cp
}

// Classes returns the observed request classes, sorted.
func (m *Meter) Classes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.classes))
	for k := range m.classes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Totals returns the stats across every class, which is what a consumer
// reports alongside its token accounting.
func (m *Meter) Totals() ClassStats {
	m.mu.Lock()
	defer m.mu.Unlock()

	total := ClassStats{AnsweredBy: make(map[string]int)}
	for _, c := range m.classes {
		total.Requests += c.Requests
		total.Escalated += c.Escalated
		total.ReachedModel += c.ReachedModel
		total.Delivered += c.Delivered
		total.Unanswered += c.Unanswered
		total.Defects += c.Defects
		total.RungFailures += c.RungFailures
		for k, v := range c.AnsweredBy {
			total.AnsweredBy[k] += v
		}
	}
	return total
}

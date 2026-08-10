// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Parser answers a request without any inference at all.
//
// Dates, relative times, currency, durations and identifiers need no
// model, and reaching for inference where a parser would do is a defect
// rather than a harmless choice. Parsers run BEFORE the local model, so
// the cheapest thing that can answer does.
type Parser struct {
	// Name identifies the parser in verdicts.
	Name string
	// Parse answers, or returns nil to decline. Declining is how a
	// parser says "not mine" without denying.
	Parse func(req *Request) *Verdict
}

// Sizing records what a local model actually costs on this host.
//
// It is REQUIRED, and that is the point of the criterion it implements:
// model choice is sized against the host's real capacity rather than
// assumed, and the sizing is recorded rather than folded into code as a
// constant nobody can trace. A rung whose model was never measured
// refuses to run.
type Sizing struct {
	// Model as the backend names it.
	Model string
	// ContextTokens the model can hold on this host.
	ContextTokens int
	// MeasuredLatency for a representative request, on this host.
	//
	// This is the currency of a local rung. Its cost is LATENCY, not
	// tokens, which is what makes precomputation free and what a
	// consumer trades against when placing it in the ladder.
	MeasuredLatency time.Duration
	// MeasuredOn describes the host the numbers came from, because a
	// sizing carried to a different machine is a guess wearing a
	// measurement's clothes.
	MeasuredOn string
}

// Valid reports whether the sizing carries what makes it a measurement.
func (s *Sizing) Valid() error {
	switch {
	case s == nil:
		return fmt.Errorf("ladder: a local rung needs a Sizing — an unmeasured model is a guess")
	case s.Model == "":
		return fmt.Errorf("ladder: Sizing needs a Model")
	case s.ContextTokens <= 0:
		return fmt.Errorf("ladder: Sizing for %q needs a positive ContextTokens", s.Model)
	case s.MeasuredLatency <= 0:
		return fmt.Errorf("ladder: Sizing for %q needs a MeasuredLatency — latency is what a local rung costs", s.Model)
	case s.MeasuredOn == "":
		return fmt.Errorf("ladder: Sizing for %q needs MeasuredOn — a sizing from another host is not a measurement", s.Model)
	}
	return nil
}

// LocalConfig configures a [LocalRung].
type LocalConfig struct {
	Scope *Scope

	// Endpoint is an Ollama-compatible generate endpoint.
	Endpoint string

	// Sizing is the recorded measurement for the model behind it.
	Sizing *Sizing

	// HTTP client. Defaults to one with a timeout derived from the
	// measured latency, because a local rung that hangs is worse than
	// one that abstains.
	HTTP *http.Client

	// Parsers run before inference, cheapest first.
	Parsers []Parser

	// Prompt renders a request for the model.
	Prompt func(req *Request) string

	// Decide turns a model response into a verdict. Returning nil
	// abstains, which is how a local rung declines rather than guessing.
	Decide func(req *Request, response string) (*Verdict, error)
}

// LocalRung is a model-backed rung whose cost is latency rather than
// tokens.
//
// That difference has architectural consequences rather than merely
// economic ones: because inference here has no marginal token cost,
// classifications and briefs may be computed speculatively for requests
// that never arrive and thrown away. What is waste against a metered
// model is free against a local one.
type LocalRung struct {
	scope    *Scope
	endpoint string
	sizing   Sizing
	http     *http.Client
	parsers  []Parser
	prompt   func(*Request) string
	decide   func(*Request, string) (*Verdict, error)

	// ParserHits and InferenceCalls separate the two paths, so a
	// consumer can see how much never needed a model at all.
	mu             sync.Mutex
	parserHits     int
	inferenceCalls int
}

// NewLocalRung builds a local rung, refusing anything unmeasured.
func NewLocalRung(cfg *LocalConfig) (*LocalRung, error) {
	if cfg == nil || cfg.Scope == nil {
		return nil, fmt.Errorf("ladder: LocalConfig needs a Scope")
	}
	if err := cfg.Sizing.Valid(); err != nil {
		return nil, err
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("ladder: LocalConfig needs an Endpoint")
	}
	if cfg.Prompt == nil || cfg.Decide == nil {
		return nil, fmt.Errorf("ladder: LocalConfig needs both Prompt and Decide — claudia has no opinion about what to ask or what an answer means")
	}

	client := cfg.HTTP
	if client == nil {
		// Ten times the measured latency: generous enough for a slow
		// run, short enough that a wedged backend abstains rather than
		// stalling the ladder.
		client = &http.Client{Timeout: 10 * cfg.Sizing.MeasuredLatency}
	}
	return &LocalRung{
		scope:    cfg.Scope,
		endpoint: cfg.Endpoint,
		sizing:   *cfg.Sizing,
		http:     client,
		parsers:  cfg.Parsers,
		prompt:   cfg.Prompt,
		decide:   cfg.Decide,
	}, nil
}

// Scope implements [Layer].
func (l *LocalRung) Scope() *Scope { return l.scope }

// Sizing returns the recorded measurement this rung runs under.
func (l *LocalRung) Sizing() Sizing { return l.sizing }

// Counts returns how many requests were answered by a parser and how
// many reached inference.
func (l *LocalRung) Counts() (parserHits, inferenceCalls int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.parserHits, l.inferenceCalls
}

// Evaluate implements [Layer]: parsers first, then local inference.
func (l *LocalRung) Evaluate(ctx context.Context, req *Request, rd *Reader) (*Verdict, error) {
	for _, p := range l.parsers {
		if v := p.Parse(req); v != nil {
			if v.Rule == "" {
				v.Rule = p.Name
			}
			l.mu.Lock()
			l.parserHits++
			l.mu.Unlock()
			return v, nil
		}
	}

	l.mu.Lock()
	l.inferenceCalls++
	l.mu.Unlock()

	response, err := l.generate(ctx, l.prompt(req))
	if err != nil {
		// Fail upward: an unreachable local backend escalates rather
		// than deciding badly or dropping the request.
		return nil, err
	}
	return l.decide(req, response)
}

type generateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type generateResponse struct {
	Response string `json:"response"`
	Error    string `json:"error,omitempty"`
}

func (l *LocalRung) generate(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(generateRequest{Model: l.sizing.Model, Prompt: prompt})
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, l.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := l.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("ladder: local inference: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ladder: local inference: %s", resp.Status)
	}
	var out generateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("ladder: decode local response: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("ladder: local inference: %s", out.Error)
	}
	return out.Response, nil
}

// Speculator runs work ahead of demand and throws away what is never
// claimed.
//
// Free of tokens is not free of the machine, so speculation is BOUNDED
// and YIELDS: it never competes with a live request for the resource it
// needs. An unbounded speculative pipeline saturates the host and
// surfaces as latency on the one path that was supposed to be cheap.
type Speculator struct {
	limit chan struct{}

	mu      sync.Mutex
	ready   map[string]any
	live    int
	pending sync.WaitGroup
}

// NewSpeculator bounds speculation to at most limit concurrent tasks.
func NewSpeculator(limit int) *Speculator {
	if limit < 1 {
		limit = 1
	}
	return &Speculator{limit: make(chan struct{}, limit), ready: make(map[string]any)}
}

// Live marks the start and end of real work. While any live work is in
// flight, speculation stands down.
func (s *Speculator) Live(start bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if start {
		s.live++
		return
	}
	if s.live > 0 {
		s.live--
	}
}

func (s *Speculator) busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live > 0
}

// Precompute runs fn in the background and files the result under key.
//
// It gives up quietly if the bound is reached, if live work is in
// flight, or if the context is cancelled. Speculative work that cannot
// run is not an error: it is work that was never owed.
func (s *Speculator) Precompute(ctx context.Context, key string, fn func(context.Context) (any, error)) {
	if s.busy() {
		return
	}
	select {
	case s.limit <- struct{}{}:
	default:
		return
	}

	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		defer func() { <-s.limit }()

		if s.busy() || ctx.Err() != nil {
			return
		}
		v, err := fn(ctx)
		if err != nil || ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		s.ready[key] = v
		s.mu.Unlock()
	}()
}

// Take claims a precomputed result, removing it. A miss is ordinary:
// speculation that guessed wrong costs nothing but the latency already
// being paid.
func (s *Speculator) Take(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.ready[key]
	if ok {
		delete(s.ready, key)
	}
	return v, ok
}

// Wait blocks until in-flight speculation finishes. For tests and for
// orderly shutdown.
func (s *Speculator) Wait() { s.pending.Wait() }

// Discard drops everything precomputed but unclaimed.
func (s *Speculator) Discard() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.ready)
	s.ready = make(map[string]any)
	return n
}

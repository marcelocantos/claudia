// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package daemon implements the claudiad session-chain tracker.
//
// This is Slice 1 of target 🎯T1 in docs/targets.yaml: a minimal
// daemon that tracks (pid, session_id, cwd) tuples registered by
// the claudia library and watches the Claude Code projects directory
// for new JSONL files so it can attribute /clear rollovers to the
// owning process deterministically. Slices 2–4 (warm agent pool,
// live-event refactor, packaging) layer on top of this plumbing.
package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/marcelocantos/claudia/internal/daemonproto"
)

// State owns the in-memory chain tracker. All exported methods are
// safe for concurrent use; internal consistency is protected by a
// single mutex rather than an actor goroutine — the complexity of a
// command-channel model doesn't pay off at this size.
type State struct {
	mu sync.Mutex

	// pidInfo maps a live PID to the workdir it was registered with
	// (escaped) and the current head session ID of its chain.
	pidInfo map[int]pidRecord

	// escCwdPids maps an escaped cwd to the set of live PIDs that
	// registered against it. Used by Observe for attribution lookup.
	escCwdPids map[string]map[int]struct{}

	// chains maps a chain_id (the first sid in the chain) to the
	// ordered list of sids plus the worst confidence seen.
	chains map[string]*chainRecord

	// sidToChain maps any sid in any chain to its chain_id.
	sidToChain map[string]string

	// pending holds observations whose owning PID hasn't registered
	// yet. Keyed by (escCwd, sid) → observedAt. Reconciled on the
	// next matching Register call, expired by the sweep loop.
	pending map[pendingKey]time.Time

	// subscribers is the set of live subscribers to chain updates.
	subscribers map[string]*subscriber

	// Config.
	pendingTTL    time.Duration
	sweepInterval time.Duration
	projectsDir   string

	// Injectable dependencies for tests.
	pidAliveFn func(int) bool
	nowFn      func() time.Time

	// sweep lifecycle.
	sweepDone chan struct{}
	sweepOnce sync.Once
}

type pidRecord struct {
	escCwd  string
	headSID string
}

type chainRecord struct {
	sids       []string
	confidence string
}

type pendingKey struct {
	escCwd string
	sid    string
}

type subscriber struct {
	id string
	ch chan daemonproto.ChainUpdateParams
}

// Option configures a State.
type Option func(*State)

// WithProjectsDir overrides the Claude Code projects directory root
// (default: ~/.claude/projects). Tests point this at a temp dir.
func WithProjectsDir(dir string) Option {
	return func(s *State) { s.projectsDir = dir }
}

// WithPendingTTL overrides how long a pending observation is kept
// waiting for its owning Register call (default: 500ms).
func WithPendingTTL(d time.Duration) Option {
	return func(s *State) { s.pendingTTL = d }
}

// WithSweepInterval overrides the periodic sweep interval
// (default: 30s).
func WithSweepInterval(d time.Duration) Option {
	return func(s *State) { s.sweepInterval = d }
}

// WithPIDAliveFn overrides the liveness check used for pruning.
// Tests stub this to avoid depending on real process state.
func WithPIDAliveFn(fn func(int) bool) Option {
	return func(s *State) { s.pidAliveFn = fn }
}

// WithNowFn overrides the clock. Tests use this for deterministic
// pending-TTL behaviour.
func WithNowFn(fn func() time.Time) Option {
	return func(s *State) { s.nowFn = fn }
}

// NewState constructs a State with sensible defaults. Call Start to
// kick off the periodic sweep goroutine; Close to shut it down.
func NewState(opts ...Option) *State {
	s := &State{
		pidInfo:       make(map[int]pidRecord),
		escCwdPids:    make(map[string]map[int]struct{}),
		chains:        make(map[string]*chainRecord),
		sidToChain:    make(map[string]string),
		pending:       make(map[pendingKey]time.Time),
		subscribers:   make(map[string]*subscriber),
		pendingTTL:    500 * time.Millisecond,
		sweepInterval: 30 * time.Second,
		projectsDir:   defaultProjectsDir(),
		pidAliveFn:    defaultPIDAlive,
		nowFn:         time.Now,
		sweepDone:     make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ProjectsDir returns the Claude Code projects directory this state
// is configured to watch.
func (s *State) ProjectsDir() string { return s.projectsDir }

// Start kicks off the periodic sweep goroutine. Safe to call only
// once per State.
func (s *State) Start() {
	go s.sweepLoop()
}

// Close stops the sweep goroutine. Does not drop subscribers; callers
// that hold subscriber channels should stop reading after Close.
func (s *State) Close() {
	s.sweepOnce.Do(func() { close(s.sweepDone) })
}

// Register records a (cwd, session_id, pid) tuple. Calls are
// idempotent: re-registering the exact same tuple is a no-op, and
// re-registering a live PID with a different session_id is dropped —
// the daemon owns chain transitions, not the client.
//
// If any pending observations are waiting on this cwd, they are
// replayed inline so the chain reflects the full history.
func (s *State) Register(cwd, sid string, pid int) {
	escCwd := EscapeCwd(cwd)

	s.mu.Lock()
	defer s.mu.Unlock()

	if rec, ok := s.pidInfo[pid]; ok {
		if rec.headSID == sid {
			return // exact idempotent
		}
		return // conflicting re-register; drop silently
	}

	s.pidInfo[pid] = pidRecord{escCwd: escCwd, headSID: sid}
	if s.escCwdPids[escCwd] == nil {
		s.escCwdPids[escCwd] = make(map[int]struct{})
	}
	s.escCwdPids[escCwd][pid] = struct{}{}

	// Seed the chain if this sid isn't already part of one.
	if _, ok := s.sidToChain[sid]; !ok {
		s.chains[sid] = &chainRecord{
			sids:       []string{sid},
			confidence: daemonproto.ConfidenceDeterministic,
		}
		s.sidToChain[sid] = sid
	}

	// Replay any pending observations for this cwd that aren't our
	// own sid. Each replay appends the observed sid to this pid's
	// chain, advancing the head.
	for key := range s.pending {
		if key.escCwd != escCwd || key.sid == sid {
			continue
		}
		delete(s.pending, key)
		s.observeLocked(escCwd, key.sid)
	}
}

// ObserveEscaped records that a new JSONL file appeared for an escaped
// cwd. If a live PID owns that cwd, the sid is appended to its chain
// and subscribers are notified. Otherwise it is stashed in pending
// for a short TTL in case a Register call is about to arrive.
//
// The watcher calls this; Register also calls it internally during
// pending replay.
func (s *State) ObserveEscaped(escCwd, sid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observeLocked(escCwd, sid)
}

// observeLocked is the internal entry point. mu must be held.
func (s *State) observeLocked(escCwd, sid string) {
	// Already tracked — sid was registered or previously observed on
	// the same chain. No-op.
	if _, ok := s.sidToChain[sid]; ok {
		return
	}

	// Find live PIDs for this cwd, pruning dead ones as we go.
	pids := s.escCwdPids[escCwd]
	var live []int
	for pid := range pids {
		if s.pidAliveFn(pid) {
			live = append(live, pid)
		} else {
			s.dropPIDLocked(pid)
		}
	}

	if len(live) == 0 {
		// No owner yet — buffer under pending. The sweep loop will
		// expire it if no Register arrives within pendingTTL.
		s.pending[pendingKey{escCwd: escCwd, sid: sid}] = s.nowFn()
		return
	}

	// Pick the attribution target. One live PID is deterministic;
	// multiple live PIDs in the same cwd is ambiguous and the chain
	// is marked as such on every subscriber notification. Slice 1
	// picks the highest PID as a weak proxy for "most recent" —
	// future slices can read the new JSONL's parent sid field for
	// a deterministic answer.
	confidence := daemonproto.ConfidenceDeterministic
	chosen := live[0]
	if len(live) > 1 {
		confidence = daemonproto.ConfidenceAmbiguous
		for _, p := range live[1:] {
			if p > chosen {
				chosen = p
			}
		}
	}

	rec := s.pidInfo[chosen]
	chainID := s.sidToChain[rec.headSID]
	chain := s.chains[chainID]
	chain.sids = append(chain.sids, sid)
	if confidence == daemonproto.ConfidenceAmbiguous {
		chain.confidence = daemonproto.ConfidenceAmbiguous
	}
	s.sidToChain[sid] = chainID
	rec.headSID = sid
	s.pidInfo[chosen] = rec

	// Fanout to subscribers (non-blocking). Copy the chain because
	// subscribers may run in other goroutines after the lock is
	// released.
	chainCopy := append([]string(nil), chain.sids...)
	for _, sub := range s.subscribers {
		update := daemonproto.ChainUpdateParams{
			SubscriptionID: sub.id,
			Chain:          chainCopy,
			Confidence:     chain.confidence,
		}
		select {
		case sub.ch <- update:
		default:
			// Slow subscriber — drop. This is acceptable because the
			// chain is still queryable via LookupChain; chain.update
			// is a push-notification optimisation, not the source of
			// truth.
		}
	}
}

// dropPIDLocked forgets a dead PID. mu must be held.
func (s *State) dropPIDLocked(pid int) {
	rec, ok := s.pidInfo[pid]
	if !ok {
		return
	}
	delete(s.pidInfo, pid)
	if set := s.escCwdPids[rec.escCwd]; set != nil {
		delete(set, pid)
		if len(set) == 0 {
			delete(s.escCwdPids, rec.escCwd)
		}
	}
}

// LookupChain returns the ordered chain containing sid, plus the
// chain's confidence. The third return is false if sid is not known.
func (s *State) LookupChain(sid string) ([]string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	chainID, ok := s.sidToChain[sid]
	if !ok {
		return nil, "", false
	}
	rec := s.chains[chainID]
	chain := append([]string(nil), rec.sids...)
	return chain, rec.confidence, true
}

// Subscribe creates a new subscription for chain updates and returns
// its ID and the channel to read from. Always pair with Unsubscribe,
// even on disconnect — the server calls it on connection teardown.
func (s *State) Subscribe() (string, <-chan daemonproto.ChainUpdateParams) {
	sub := &subscriber{
		id: uuid.New().String(),
		ch: make(chan daemonproto.ChainUpdateParams, 64),
	}
	s.mu.Lock()
	s.subscribers[sub.id] = sub
	s.mu.Unlock()
	return sub.id, sub.ch
}

// Unsubscribe removes a subscription and closes its channel. Safe to
// call more than once or with an unknown ID.
func (s *State) Unsubscribe(id string) {
	s.mu.Lock()
	sub, ok := s.subscribers[id]
	if ok {
		delete(s.subscribers, id)
	}
	s.mu.Unlock()
	if ok {
		close(sub.ch)
	}
}

// Sweep prunes dead PIDs and expired pending observations. Called
// by the periodic sweep loop and exposed for tests.
func (s *State) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for pid := range s.pidInfo {
		if !s.pidAliveFn(pid) {
			s.dropPIDLocked(pid)
		}
	}
	now := s.nowFn()
	for key, observedAt := range s.pending {
		if now.Sub(observedAt) >= s.pendingTTL {
			delete(s.pending, key)
		}
	}
}

func (s *State) sweepLoop() {
	t := time.NewTicker(s.sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-s.sweepDone:
			return
		case <-t.C:
			s.Sweep()
		}
	}
}

// EscapeCwd applies Claude Code's workdir-escape scheme:
// non-alphanumeric/dash runes become '-'. Keep in sync with
// escapeWorkDir in the top-level claudia package.
func EscapeCwd(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func defaultProjectsDir() string {
	return filepath.Join(os.Getenv("HOME"), ".claude", "projects")
}

func defaultPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	// EPERM means the process exists but we lack permission to
	// signal it — still alive.
	return err == syscall.EPERM
}

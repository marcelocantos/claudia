// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// A fleet is a named tree of seats that manage and talk to each other (🎯T247).
// The kernel gives them mailboxes and two guarantees: every request gets a
// response, even if its target dies, and a delayed message arrives even though
// its sender is idle. A turn's text is its response. Turns woken by anything
// other than a request go up the parent tree, so they always end at the owner
// and cannot ping-pong.
//
// Everything else, including how to divide and verify work, is the agents'
// instructions. State is saved on every change, so a broker restart loses
// nothing: a turn it interrupted is delivered again.

const (
	// FleetOwner addresses whoever drives the fleet through the client API.
	FleetOwner = "owner"
	// FleetNoReply, at the end of a turn woken by anything but a request, keeps
	// the turn silent.
	FleetNoReply = "NOREPLY"

	// Message kinds.
	FleetRequest  = "request"  // Its turn text goes back to the sender.
	FleetResponse = "response" // Answers or reports. Its turn text goes up the tree.
	FleetTimer    = "timer"    // A delayed message to yourself.
	FleetNotice   = "notice"   // Owner news for an agent. Its turn text goes up the tree.

	// Event kinds.
	FleetEventMessage     = "message"
	FleetEventStarted     = "started"
	FleetEventStartFailed = "start_failed"
	FleetEventStop        = "stop"
	FleetEventRetired     = "retired"
	FleetEventTurn        = "turn"

	// Agent states in a status.
	FleetStateStarting = "starting"
	FleetStateIdle     = "idle"
	FleetStateWorking  = "working"

	fleetDefaultMaxAgents   = 12
	fleetDefaultMaxMessages = 2000
	fleetTokenBytes         = 16
	fleetMaxName            = 48
	// fleetMaxText keeps any one event inside a broker frame (1 MiB) even when
	// JSON escaping swells every byte sixfold.
	fleetMaxText = 128 << 10
	// fleetEventRing is how many recent events stay in memory per fleet.
	fleetEventRing = 512
	// fleetReadBytes bounds the encoded events one read returns, so that a reply
	// fits a broker frame. A read always returns at least one event.
	fleetReadBytes = 512 << 10
	// fleetStopWait bounds waiting for a stopped tree to be gone.
	fleetStopWait = time.Minute
	fleetPoll     = 10 * time.Millisecond
	// fleetMaxDeaths is how many times in a row a seat can die mid-work, with no
	// turn finished between, before its agent retires.
	fleetMaxDeaths = 3
	// fleetRetryMax caps the wait between tries to bring an agent's seat back.
	fleetRetryMax = time.Minute
	fleetRetryMin = time.Second

	fleetRedeliveredNote = "[Claudia restarted during your last turn on this message, so its reply was lost. Work you already did remains. Check it and answer now.]"
	fleetStateVersion    = 1
)

var (
	// No doubled hyphen, so "<fleet>--<agent>" is unambiguous.
	fleetNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

	errFleetStopped = errors.New("stopped")
	errFleetClosed  = errors.New("the fleet kernel is shut down")

	// ErrFleetSeatHeld means something outside the fleet currently forbids this
	// seat. The kernel keeps trying.
	ErrFleetSeatHeld = errors.New("the consumer's fleet policy holds this seat")
	// ErrFleetConversationLost means the provider says a seat's conversation can
	// never be resumed. Only this makes the kernel start the agent afresh: any
	// other failure may pass, and a fresh start would throw away work.
	ErrFleetConversationLost = errors.New("the seat's conversation cannot be resumed")
)

// fleetNameValid reports whether a fleet or agent name is acceptable.
func fleetNameValid(name string) bool {
	return len(name) <= fleetMaxName && fleetNamePattern.MatchString(name)
}

// FleetSeat is what the kernel needs of a running agent.
type FleetSeat interface {
	Send(text string) error
	WaitForResponse(ctx context.Context) (string, error)
	// Alive reports whether the seat's process is still there.
	Alive() bool
	// Stop ends the seat for good. When it returns, the seat is gone and its name
	// is free. Calling it again waits for the first call.
	Stop()
	Describe() string
	Usage() any
	LastActivity() time.Time
}

// FleetSeatSpec is what the broker needs to run one agent.
type FleetSeatSpec struct {
	Fleet   string
	Name    string
	Token   string
	WorkDir string
	Quality string
	Purpose string
	Model   string
	// Resume means reattach to this agent's seat and conversation. Otherwise any
	// seat it left behind is replaced by a fresh one.
	Resume bool
}

// FleetKernelArgs configures [OpenFleetKernel].
type FleetKernelArgs struct {
	// Dir holds the state file and one event log per fleet. Required.
	Dir string
	// Start runs a seat. Required.
	Start func(ctx context.Context, spec *FleetSeatSpec) (FleetSeat, error)
	// Discard removes a seat the kernel holds no handle on. Optional.
	Discard func(fleet, name string)
	// RetryWait is the first wait between tries to bring a seat back. Zero is a
	// second. Optional.
	RetryWait time.Duration
	// ResumeSpacing paces agents coming back after a restart, because each may
	// open a turn at once, and a wave of those meets a rate limit. Optional.
	ResumeSpacing time.Duration
	// Logger reports failures nobody is waiting on. Optional.
	Logger func(msg string, args ...any)
}

// FleetOptions configures a fleet when it is first opened.
type FleetOptions struct {
	// MaxAgents and MaxMessages are safety valves against runaway spend. Zero
	// selects a default, and a later open cannot change them.
	MaxAgents   int
	MaxMessages int
}

// FleetStartArgs describes an agent to start.
type FleetStartArgs struct {
	Name string
	// Instructions reach the agent with its first message. Set this or
	// InstructionsFile.
	Instructions     string
	InstructionsFile string
	// WorkDir defaults to the parent's. An owner's root agent must set it.
	WorkDir string
	Quality string
	Purpose string
	Model   string
}

// FleetMessage is one message between agents or with the owner.
type FleetMessage struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	From      string `json:"from"`
	To        string `json:"to"`
	InReplyTo string `json:"in_reply_to,omitempty"`
	Text      string `json:"text"`
	// Redelivered marks a message whose earlier turn a restart interrupted.
	Redelivered bool `json:"redelivered,omitempty"`
}

// FleetEvent is one entry of a fleet's log. The owner's messages are the
// FleetEventMessage entries addressed to [FleetOwner].
type FleetEvent struct {
	Seq     uint64        `json:"seq"`
	At      time.Time     `json:"at"`
	Kind    string        `json:"kind"`
	Agent   string        `json:"agent,omitempty"`
	Parent  string        `json:"parent,omitempty"`
	Root    string        `json:"root,omitempty"`
	Model   string        `json:"model,omitempty"`
	Cause   string        `json:"cause,omitempty"`
	Text    string        `json:"text,omitempty"`
	Usage   any           `json:"usage,omitempty"`
	Message *FleetMessage `json:"message,omitempty"`
}

// FleetAgentStatus is one agent as it is right now.
type FleetAgentStatus struct {
	Name        string `json:"name"`
	Parent      string `json:"parent"`
	Root        string `json:"root"`
	State       string `json:"state"`
	Model       string `json:"model,omitempty"`
	Queued      int    `json:"queued"`
	IdleSeconds int    `json:"idle_seconds"`
}

// FleetStatus is a fleet as it is right now.
type FleetStatus struct {
	Agents []FleetAgentStatus `json:"agents"`
	Timers int                `json:"timers"`
	// LastSeq is the newest event, so a new reader can skip history.
	LastSeq uint64 `json:"last_seq"`
}

type fleetAgentState struct {
	Name         string          `json:"name"`
	Parent       string          `json:"parent"`
	Root         string          `json:"root"`
	Instructions string          `json:"instructions"`
	Token        string          `json:"token"`
	WorkDir      string          `json:"workdir"`
	Quality      string          `json:"quality,omitempty"`
	Purpose      string          `json:"purpose,omitempty"`
	Model        string          `json:"model,omitempty"`
	Inbox        []*FleetMessage `json:"inbox,omitempty"`
	Current      *FleetMessage   `json:"current,omitempty"`
	Briefed      bool            `json:"briefed,omitempty"`
	Stopped      bool            `json:"stopped,omitempty"`

	// retiring means retireState owns this agent's end. Not saved: a kernel that
	// restarts sees Stopped and retires it again.
	retiring bool
}

type fleetTimerState struct {
	At      time.Time     `json:"at"`
	Message *FleetMessage `json:"message"`
}

type fleetGroupState struct {
	MaxAgents   int                         `json:"max_agents"`
	MaxMessages int                         `json:"max_messages"`
	NextID      uint64                      `json:"next_id"`
	Sent        int                         `json:"sent"`
	Closing     bool                        `json:"closing,omitempty"`
	Agents      map[string]*fleetAgentState `json:"agents"`
	Timers      map[string]*fleetTimerState `json:"timers,omitempty"`
}

type fleetState struct {
	Version int                         `json:"version"`
	Fleets  map[string]*fleetGroupState `json:"fleets"`
}

// fleetRuntime is what exists only while the kernel runs.
type fleetRuntime struct {
	fleet string
	state *fleetAgentState
	seat  FleetSeat
	wake  *sync.Cond
	// deaths counts deaths since this agent last finished a turn.
	deaths int
}

type fleetLog struct {
	file   *os.File
	path   string
	seq    uint64
	recent []FleetEvent
}

// FleetKernel runs every fleet on a broker. It is dormant until one is opened.
type FleetKernel struct {
	dir     string
	start   func(ctx context.Context, spec *FleetSeatSpec) (FleetSeat, error)
	discard func(fleet, name string)
	spacing time.Duration
	retry   time.Duration
	logf    func(msg string, args ...any)
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu       sync.Mutex
	state    fleetState
	running  map[string]*fleetRuntime
	starting map[string]bool
	// reviving holds agents whose seat is being brought back. Unlike a first
	// start, nobody is waiting on one, so it can be stopped.
	reviving map[string]bool
	// reviveStop cancels a revive's start in progress, so a stop need not wait
	// for a slow seat to finish starting.
	reviveStop map[string]context.CancelFunc
	timers     map[string]*time.Timer
	logs       map[string]*fleetLog
	changed    chan struct{}
	closed     bool
}

func fleetKey(fleet, name string) string { return fleet + "/" + name }

// OpenFleetKernel loads saved fleets. Damaged state is an error, never a silent
// reset, and the file is left untouched. Call [FleetKernel.Resume] once the
// kernel's MCP endpoint is being served, because a seat calls it while starting.
func OpenFleetKernel(args *FleetKernelArgs) (*FleetKernel, error) {
	if args == nil || args.Dir == "" || args.Start == nil {
		return nil, errors.New("fleet kernel: Dir and Start are required")
	}
	if err := os.MkdirAll(args.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("fleet kernel: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	k := &FleetKernel{
		dir: args.Dir, start: args.Start, discard: args.Discard, spacing: args.ResumeSpacing, logf: args.Logger, ctx: ctx, cancel: cancel,
		state:      fleetState{Version: fleetStateVersion, Fleets: map[string]*fleetGroupState{}},
		running:    map[string]*fleetRuntime{},
		starting:   map[string]bool{},
		reviving:   map[string]bool{},
		reviveStop: map[string]context.CancelFunc{},
		timers:     map[string]*time.Timer{},
		logs:       map[string]*fleetLog{},
		changed:    make(chan struct{}),
	}
	if k.logf == nil {
		k.logf = func(string, ...any) {}
	}
	k.retry = args.RetryWait
	if k.retry <= 0 {
		k.retry = fleetRetryMin
	}
	raw, err := os.ReadFile(k.statePath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		cancel()
		return nil, fmt.Errorf("fleet kernel: %w", err)
	default:
		if err := json.Unmarshal(raw, &k.state); err != nil {
			cancel()
			return nil, fmt.Errorf("fleet kernel: damaged state %s: %w", k.statePath(), err)
		}
		if k.state.Version != fleetStateVersion {
			cancel()
			return nil, fmt.Errorf("fleet kernel: state %s has version %d", k.statePath(), k.state.Version)
		}
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	for name, group := range k.state.Fleets {
		log, err := k.openLogLocked(name)
		if err != nil {
			cancel()
			return nil, err
		}
		k.logs[name] = log
		for _, a := range group.Agents {
			// A turn the restart interrupted goes round again, ahead of the rest.
			if a.Current != nil {
				a.Current.Redelivered = true
				a.Inbox = append([]*FleetMessage{a.Current}, a.Inbox...)
				a.Current = nil
			}
			k.reviving[fleetKey(name, a.Name)] = true
		}
	}
	if err := k.saveLocked(); err != nil {
		cancel()
		return nil, err
	}
	return k, nil
}

// Resume arms saved timers and reattaches every saved agent in the background.
func (k *FleetKernel) Resume() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return
	}
	order := 0
	for name, group := range k.state.Fleets {
		for id, timer := range group.Timers {
			k.armTimerLocked(name, id, timer)
		}
		for _, a := range group.Agents {
			k.wg.Add(1)
			go k.revive(name, a, 0, time.Duration(order)*k.spacing)
			order++
		}
	}
}

// Agents lists every fleet's agents, including ones not yet running.
func (k *FleetKernel) Agents() map[string][]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string][]string{}
	for fleet, group := range k.state.Fleets {
		for name := range group.Agents {
			out[fleet] = append(out[fleet], name)
		}
	}
	return out
}

// Close stops the kernel and leaves its seats running with their state saved,
// so the next kernel reattaches to them.
func (k *FleetKernel) Close() {
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return
	}
	k.closed = true
	for _, timer := range k.timers {
		timer.Stop()
	}
	for _, rt := range k.running {
		rt.wake.Broadcast()
	}
	k.mu.Unlock()
	k.cancel()
	k.wg.Wait()
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, log := range k.logs {
		_ = log.file.Close()
	}
}

// Open creates the fleet if it is new. It is safe to call on an existing one,
// which is how a client reattaches.
func (k *FleetKernel) Open(fleet string, opts *FleetOptions) error {
	if !fleetNameValid(fleet) {
		return fmt.Errorf("invalid fleet name %q: use lowercase letters, digits and hyphens", fleet)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return errFleetClosed
	}
	if k.state.Fleets[fleet] != nil {
		return nil
	}
	group := &fleetGroupState{MaxAgents: fleetDefaultMaxAgents, MaxMessages: fleetDefaultMaxMessages, Agents: map[string]*fleetAgentState{}}
	if opts != nil && opts.MaxAgents > 0 {
		group.MaxAgents = opts.MaxAgents
	}
	if opts != nil && opts.MaxMessages > 0 {
		group.MaxMessages = opts.MaxMessages
	}
	log, err := k.openLogLocked(fleet)
	if err != nil {
		return err
	}
	k.logs[fleet] = log
	k.state.Fleets[fleet] = group
	return k.saveLocked()
}

// Start launches an agent under caller, which is [FleetOwner] or an agent
// starting its own child, and returns once it is running.
func (k *FleetKernel) Start(ctx context.Context, fleet, caller string, args *FleetStartArgs) error {
	a, err := k.admit(fleet, caller, args)
	if err != nil {
		return err
	}
	return k.launch(ctx, fleet, a, false)
}

// startForAgent is Start for a tool call, which has too short a deadline to wait
// for a seat. A bad request fails now. A failure to launch reaches the caller as
// a message, and anything sent meanwhile waits in the new agent's mailbox.
func (k *FleetKernel) startForAgent(fleet, caller string, args *FleetStartArgs) error {
	a, err := k.admit(fleet, caller, args)
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return errFleetClosed
	}
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		_ = k.launch(k.ctx, fleet, a, true)
	}()
	return nil
}

// admit checks a start and records the agent, saved before its seat exists so
// that a crash cannot leave a seat with no record.
func (k *FleetKernel) admit(fleet, caller string, args *FleetStartArgs) (*fleetAgentState, error) {
	if args == nil || !fleetNameValid(args.Name) || args.Name == FleetOwner {
		name := ""
		if args != nil {
			name = args.Name
		}
		return nil, fmt.Errorf("invalid agent name %q: use lowercase letters, digits and single hyphens", name)
	}
	instructions := args.Instructions
	if args.InstructionsFile != "" {
		raw, err := os.ReadFile(args.InstructionsFile)
		if err != nil {
			return nil, fmt.Errorf("read instructions: %w", err)
		}
		instructions = string(raw)
	}
	if strings.TrimSpace(instructions) == "" {
		return nil, errors.New("an agent needs instructions")
	}
	if args.WorkDir != "" && !filepath.IsAbs(args.WorkDir) {
		return nil, fmt.Errorf("workdir %q is not absolute", args.WorkDir)
	}
	tokenBytes := make([]byte, fleetTokenBytes)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, err
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	group, err := k.groupLocked(fleet)
	if err != nil {
		return nil, err
	}
	if group.Closing {
		return nil, fmt.Errorf("fleet %q is shutting down", fleet)
	}
	a := &fleetAgentState{
		Name: args.Name, Parent: caller, Root: args.Name, Instructions: instructions, Token: hex.EncodeToString(tokenBytes),
		WorkDir: args.WorkDir, Quality: args.Quality, Purpose: args.Purpose, Model: args.Model,
	}
	if caller != FleetOwner {
		parent := group.Agents[caller]
		if parent == nil {
			return nil, fmt.Errorf("no agent named %q", caller)
		}
		a.Root = parent.Root
		if a.WorkDir == "" {
			a.WorkDir = parent.WorkDir
		}
	}
	if a.WorkDir == "" {
		return nil, errors.New("a root agent needs a workdir")
	}
	if group.Agents[args.Name] != nil {
		return nil, fmt.Errorf("agent %q already exists", args.Name)
	}
	if len(group.Agents) >= group.MaxAgents {
		return nil, fmt.Errorf("agent limit %d reached; stop an agent first", group.MaxAgents)
	}
	group.Agents[args.Name] = a
	key := fleetKey(fleet, args.Name)
	k.starting[key] = true
	if err := k.saveLocked(); err != nil {
		delete(group.Agents, args.Name)
		delete(k.starting, key)
		return nil, err
	}
	return a, nil
}

// launch gets an admitted agent its first seat. If that fails, the agent is
// forgotten, and any request already sent to it is answered with the failure.
func (k *FleetKernel) launch(ctx context.Context, fleet string, a *fleetAgentState, tellParent bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopCancel := context.AfterFunc(k.ctx, cancel)
	defer stopCancel()
	seat, err := k.start(ctx, k.specFor(fleet, a, false))

	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		// Kept on purpose: the next kernel reattaches to it.
		return errFleetClosed
	}
	delete(k.starting, fleetKey(fleet, a.Name))
	if err == nil {
		k.runLocked(fleet, a, seat, 0)
		return nil
	}
	group := k.state.Fleets[fleet]
	if group != nil {
		delete(group.Agents, a.Name)
	}
	k.eventLocked(fleet, &FleetEvent{Kind: FleetEventStartFailed, Agent: a.Name, Parent: a.Parent, Root: a.Root, Cause: err.Error()})
	failure := fmt.Sprintf("agent %s could not start: %v", a.Name, err)
	toldParent := k.answerOwedLocked(fleet, a, failure)
	if tellParent && !toldParent {
		k.deliverLocked(fleet, &FleetMessage{Kind: FleetResponse, From: a.Name, To: k.parentOfLocked(fleet, a), Text: failure})
	}
	if saveErr := k.saveLocked(); saveErr != nil {
		k.logf("fleet: save after failed start", "fleet", fleet, "agent", a.Name, "err", saveErr)
	}
	return err
}

// answerOwedLocked fails every request the agent held, and reports whether its
// parent was among those told. k.mu held.
func (k *FleetKernel) answerOwedLocked(fleet string, a *fleetAgentState, failure string) bool {
	owed := a.Inbox
	if a.Current != nil {
		owed = append([]*FleetMessage{a.Current}, owed...)
	}
	a.Inbox, a.Current = nil, nil
	toldParent := false
	for _, m := range owed {
		if m.Kind == FleetRequest {
			k.deliverLocked(fleet, &FleetMessage{Kind: FleetResponse, From: a.Name, To: m.From, InReplyTo: m.ID, Text: failure})
			toldParent = toldParent || m.From == a.Parent
		}
	}
	return toldParent
}

func (k *FleetKernel) specFor(fleet string, a *fleetAgentState, resume bool) *FleetSeatSpec {
	return &FleetSeatSpec{
		Fleet: fleet, Name: a.Name, Token: a.Token, WorkDir: a.WorkDir, Quality: a.Quality, Purpose: a.Purpose, Model: a.Model, Resume: resume,
	}
}

// runLocked joins a started seat to the kernel. k.mu held.
func (k *FleetKernel) runLocked(fleet string, a *fleetAgentState, seat FleetSeat, deaths int) {
	rt := &fleetRuntime{fleet: fleet, state: a, seat: seat, wake: sync.NewCond(&k.mu), deaths: deaths}
	k.running[fleetKey(fleet, a.Name)] = rt
	k.eventLocked(fleet, &FleetEvent{Kind: FleetEventStarted, Agent: a.Name, Parent: a.Parent, Root: a.Root, Model: seat.Describe()})
	k.wg.Add(1)
	go k.run(rt)
}

// revive brings back an agent's seat after a restart or its death, and keeps
// trying for as long as the agent exists, because the usual causes pass: an
// outage, a policy hold, a plan out of tokens. Its parent hears once if it is
// taking a while. If the conversation cannot be resumed, the agent starts on a
// fresh one, and its next message repeats its instructions.
func (k *FleetKernel) revive(fleet string, a *fleetAgentState, deaths int, delay time.Duration) {
	defer k.wg.Done()
	key := fleetKey(fleet, a.Name)
	k.mu.Lock()
	k.reviving[key] = true
	k.mu.Unlock()

	fresh, told := false, false
	wait := k.retry
	for {
		k.mu.Lock()
		closed, stopped, briefed := k.closed, a.Stopped, a.Briefed
		k.mu.Unlock()
		if closed {
			return
		}
		if stopped {
			k.retireState(fleet, a, nil, errFleetStopped)
			return
		}
		if delay > 0 {
			step := min(delay, fleetRetryMin)
			delay -= step
			select {
			case <-k.ctx.Done():
			case <-time.After(step):
			}
			continue
		}

		// An agent with no conversation yet has nothing to resume.
		resume := briefed && !fresh
		attempt, cancel := context.WithCancel(k.ctx)
		k.mu.Lock()
		k.reviveStop[key] = cancel
		k.mu.Unlock()
		seat, err := k.start(attempt, k.specFor(fleet, a, resume))
		cancel()

		k.mu.Lock()
		delete(k.reviveStop, key)
		if k.closed {
			// A seat that did start keeps running for the next kernel.
			k.mu.Unlock()
			return
		}
		if err == nil {
			delete(k.reviving, key)
			if briefed && !resume {
				a.Briefed = false
				k.deliverLocked(fleet, &FleetMessage{Kind: FleetResponse, From: a.Name, To: k.parentOfLocked(fleet, a),
					Text: fmt.Sprintf("agent %s lost its conversation in a restart and is starting on a fresh one", a.Name)})
			}
			if saveErr := k.saveLocked(); saveErr != nil {
				k.logf("fleet: save after revive", "fleet", fleet, "agent", a.Name, "err", saveErr)
			}
			k.runLocked(fleet, a, seat, deaths)
			k.mu.Unlock()
			return
		}
		if a.Stopped {
			// A stop cancelled this start. The loop retires it next.
			k.mu.Unlock()
			continue
		}
		if errors.Is(err, ErrFleetConversationLost) {
			fresh = true
			wait = k.retry
		}
		k.logf("fleet: cannot bring seat back", "fleet", fleet, "agent", a.Name, "err", err)
		if !told {
			told = true
			k.deliverLocked(fleet, &FleetMessage{Kind: FleetResponse, From: a.Name, To: k.parentOfLocked(fleet, a),
				Text: fmt.Sprintf("agent %s cannot come back yet and I will keep trying: %v", a.Name, err)})
			if saveErr := k.saveLocked(); saveErr != nil {
				k.logf("fleet: save after failed revive", "fleet", fleet, "agent", a.Name, "err", saveErr)
			}
		}
		k.mu.Unlock()
		delay = wait
		wait = min(wait*2, fleetRetryMax)
	}
}

// run delivers one message per turn until the agent is stopped or dies.
func (k *FleetKernel) run(rt *fleetRuntime) {
	defer k.wg.Done()
	a := rt.state
	for {
		k.mu.Lock()
		for len(a.Inbox) == 0 && !a.Stopped && !k.closed {
			rt.wake.Wait()
		}
		if k.closed {
			k.mu.Unlock()
			return
		}
		if a.Stopped {
			k.mu.Unlock()
			k.retireState(rt.fleet, a, rt, errFleetStopped)
			return
		}
		m := a.Inbox[0]
		a.Inbox = a.Inbox[1:]
		a.Current = m
		// A redelivery repeats the brief, since a restart can lose a seat's
		// conversation as well as its turn.
		brief := !a.Briefed || m.Redelivered
		a.Briefed = true
		if err := k.saveLocked(); err != nil {
			k.logf("fleet: save before turn", "fleet", rt.fleet, "agent", a.Name, "err", err)
		}
		instructions := a.Instructions
		k.mu.Unlock()

		text := formatFleetMessage(m)
		if m.Redelivered {
			text = fleetRedeliveredNote + "\n\n" + text
		}
		if brief {
			text = instructions + "\n\n---\n\n" + text
		}
		err := rt.seat.Send(text)
		var reply string
		if err == nil {
			reply, err = rt.seat.WaitForResponse(k.ctx)
		}

		k.mu.Lock()
		if k.closed {
			// Current stays saved, and the next kernel delivers it again.
			k.mu.Unlock()
			return
		}
		reply = strings.TrimSpace(reply)
		k.eventLocked(rt.fleet, &FleetEvent{Kind: FleetEventTurn, Agent: a.Name, Root: a.Root, Text: boundFleetText(reply), Usage: rt.seat.Usage(), Cause: errText(err)})
		// A seat stopped mid-turn can return an empty turn with no error. That
		// is a failure the requester must hear about, not a silent answer.
		if a.Stopped {
			k.mu.Unlock()
			k.retireState(rt.fleet, a, rt, errFleetStopped)
			return
		}
		if err != nil && !rt.seat.Alive() {
			rt.deaths++
			if rt.deaths >= fleetMaxDeaths {
				k.mu.Unlock()
				k.retireState(rt.fleet, a, rt, fmt.Errorf("its seat died %d times: %w", rt.deaths, err))
				return
			}
			// The seat goes round again, with this message first.
			m.Redelivered = true
			a.Inbox = append([]*FleetMessage{m}, a.Inbox...)
			a.Current = nil
			delete(k.running, fleetKey(rt.fleet, a.Name))
			if saveErr := k.saveLocked(); saveErr != nil {
				k.logf("fleet: save after death", "fleet", rt.fleet, "agent", a.Name, "err", saveErr)
			}
			k.wg.Add(1)
			go k.revive(rt.fleet, a, rt.deaths, 0)
			k.mu.Unlock()
			return
		}
		a.Current = nil
		if err != nil {
			// The turn failed but the agent is fine, as when a plan refuses it or a
			// provider goes silent. Whoever is waiting needs to hear, and can retry.
			failure := fmt.Sprintf("agent %s's turn failed, and it is still available: %v", a.Name, err)
			if m.Kind == FleetRequest {
				k.deliverLocked(rt.fleet, &FleetMessage{Kind: FleetResponse, From: a.Name, To: m.From, InReplyTo: m.ID, Text: failure})
			} else {
				k.deliverLocked(rt.fleet, &FleetMessage{Kind: FleetResponse, From: a.Name, To: k.parentOfLocked(rt.fleet, a), Text: failure})
			}
		} else {
			rt.deaths = 0
			k.routeReplyLocked(rt.fleet, a, m, reply)
		}
		if err := k.saveLocked(); err != nil {
			k.logf("fleet: save after turn", "fleet", rt.fleet, "agent", a.Name, "err", err)
		}
		k.mu.Unlock()
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// routeReplyLocked makes turn text the agent's response. Models narrate before
// deciding to stay silent, so ending with [FleetNoReply] is enough. k.mu held.
func (k *FleetKernel) routeReplyLocked(fleet string, a *fleetAgentState, m *FleetMessage, text string) {
	silent := text == "" || strings.HasSuffix(text, FleetNoReply)
	if m.Kind == FleetRequest {
		if silent {
			text = "(no reply)"
		}
		k.deliverLocked(fleet, &FleetMessage{Kind: FleetResponse, From: a.Name, To: m.From, InReplyTo: m.ID, Text: text})
		return
	}
	if !silent {
		k.deliverLocked(fleet, &FleetMessage{Kind: FleetResponse, From: a.Name, To: k.parentOfLocked(fleet, a), Text: text})
	}
}

func (k *FleetKernel) parentOfLocked(fleet string, a *fleetAgentState) string {
	group := k.state.Fleets[fleet]
	if group != nil && group.Agents[a.Parent] != nil {
		return a.Parent
	}
	return FleetOwner
}

// retireState removes an agent exactly once and answers everything it owed. The
// seat is gone before the name is free, so a successor can never be hit by its
// predecessor's stop.
func (k *FleetKernel) retireState(fleet string, a *fleetAgentState, rt *fleetRuntime, cause error) {
	k.mu.Lock()
	group := k.state.Fleets[fleet]
	if group == nil || group.Agents[a.Name] != a || a.retiring {
		k.mu.Unlock()
		return
	}
	a.retiring = true
	a.Stopped = true
	k.mu.Unlock()

	if rt != nil {
		rt.seat.Stop()
	} else if k.discard != nil {
		k.discard(fleet, a.Name)
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		// The next kernel finds it marked stopped and finishes this.
		return
	}
	key := fleetKey(fleet, a.Name)
	delete(group.Agents, a.Name)
	delete(k.running, key)
	delete(k.reviving, key)
	// Anyone it started now answers to the owner. Leaving the link would hand
	// them to the next agent to take this name.
	for _, child := range group.Agents {
		if child.Parent == a.Name {
			child.Parent = FleetOwner
		}
	}
	k.eventLocked(fleet, &FleetEvent{Kind: FleetEventRetired, Agent: a.Name, Root: a.Root, Cause: cause.Error()})
	toldParent := k.answerOwedLocked(fleet, a, fmt.Sprintf("agent %s is gone: %v", a.Name, cause))
	// A deliberate stop is already known to whoever called it. An unexpected
	// death with nothing owed to the parent would otherwise be silent.
	if !errors.Is(cause, errFleetStopped) && !toldParent {
		k.deliverLocked(fleet, &FleetMessage{Kind: FleetResponse, From: a.Name, To: k.parentOfLocked(fleet, a), Text: fmt.Sprintf("agent %s is gone: %v", a.Name, cause)})
	}
	if err := k.saveLocked(); err != nil {
		k.logf("fleet: save after retire", "fleet", fleet, "agent", a.Name, "err", err)
	}
}

// Send queues a message now or after a delay, and returns its ID. A delayed
// message to yourself is a timer: it wakes you and needs no response. kind is
// [FleetRequest] unless the owner sends a [FleetNotice].
func (k *FleetKernel) Send(fleet, from, to, kind, text string, after time.Duration) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	group, err := k.groupLocked(fleet)
	if err != nil {
		return "", err
	}
	if to != FleetOwner && group.Agents[to] == nil {
		return "", fmt.Errorf("no agent named %q", to)
	}
	if from == to && after <= 0 {
		return "", errors.New("a message to yourself needs a delay")
	}
	if kind == "" {
		kind = FleetRequest
	}
	if kind != FleetRequest && kind != FleetNotice {
		return "", fmt.Errorf("cannot send a message of kind %q", kind)
	}
	if group.Sent >= group.MaxMessages {
		return "", fmt.Errorf("message limit %d reached", group.MaxMessages)
	}
	if from == to {
		kind = FleetTimer
	}
	group.Sent++
	m := &FleetMessage{ID: k.nextIDLocked(group), Kind: kind, From: from, To: to, Text: boundFleetText(text)}
	if after <= 0 {
		k.deliverLocked(fleet, m)
	} else {
		timer := &fleetTimerState{At: time.Now().Add(after), Message: m}
		if group.Timers == nil {
			group.Timers = map[string]*fleetTimerState{}
		}
		group.Timers[m.ID] = timer
		k.armTimerLocked(fleet, m.ID, timer)
	}
	if err := k.saveLocked(); err != nil {
		return "", err
	}
	return m.ID, nil
}

func (k *FleetKernel) armTimerLocked(fleet, id string, timer *fleetTimerState) {
	key := fleetKey(fleet, id)
	k.timers[key] = time.AfterFunc(max(time.Until(timer.At), 0), func() {
		k.mu.Lock()
		defer k.mu.Unlock()
		delete(k.timers, key)
		group := k.state.Fleets[fleet]
		if k.closed || group == nil || group.Timers[id] != timer {
			return
		}
		delete(group.Timers, id)
		k.deliverLocked(fleet, timer.Message)
		if err := k.saveLocked(); err != nil {
			k.logf("fleet: save after timer", "fleet", fleet, "err", err)
		}
	})
}

func (k *FleetKernel) nextIDLocked(group *fleetGroupState) string {
	group.NextID++
	return fmt.Sprintf("r%d", group.NextID)
}

// deliverLocked is the only way a message enters a mailbox. A request to an
// agent that has gone is answered with a failure, which keeps the guarantee for
// messages that were delayed or in flight when it died. k.mu held; the caller
// saves.
func (k *FleetKernel) deliverLocked(fleet string, m *FleetMessage) {
	group := k.state.Fleets[fleet]
	if group == nil {
		return
	}
	if m.ID == "" {
		m.ID = k.nextIDLocked(group)
	}
	m.Text = boundFleetText(m.Text)
	k.eventLocked(fleet, &FleetEvent{Kind: FleetEventMessage, Message: m})
	if m.To == FleetOwner {
		return
	}
	if a := group.Agents[m.To]; a != nil && !a.Stopped {
		a.Inbox = append(a.Inbox, m)
		if rt := k.running[fleetKey(fleet, m.To)]; rt != nil {
			rt.wake.Signal()
		}
		return
	}
	if m.Kind == FleetRequest {
		k.deliverLocked(fleet, &FleetMessage{Kind: FleetResponse, From: m.To, To: m.From, InReplyTo: m.ID, Text: fmt.Sprintf("agent %s is gone", m.To)})
	}
}

func boundFleetText(text string) string {
	if len(text) <= fleetMaxText {
		return text
	}
	return text[:fleetMaxText] + "\n[truncated by Claudia]"
}

// Stop ends an agent; its run loop then answers what it owed. The owner can
// stop anything, and an agent can stop itself or its descendants. It returns
// before the seat is gone, because an agent may be stopping itself mid-turn.
func (k *FleetKernel) Stop(fleet, caller, name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	group, err := k.groupLocked(fleet)
	if err != nil {
		return err
	}
	a := group.Agents[name]
	if a == nil {
		return fmt.Errorf("no agent named %q", name)
	}
	if caller != FleetOwner && caller != name && !fleetDescends(group, name, caller) {
		return fmt.Errorf("agent %q may stop only itself and its descendants", caller)
	}
	key := fleetKey(fleet, name)
	rt := k.running[key]
	if rt == nil && !k.reviving[key] {
		return fmt.Errorf("agent %q is still starting", name)
	}
	if a.Stopped {
		return nil
	}
	a.Stopped = true
	if rt == nil {
		// Its revive loop notices and retires it.
		if cancel := k.reviveStop[key]; cancel != nil {
			cancel()
		}
		k.eventLocked(fleet, &FleetEvent{Kind: FleetEventStop, Agent: name, Root: a.Root, Parent: caller})
		if err := k.saveLocked(); err != nil {
			k.logf("fleet: save after stop", "fleet", fleet, "agent", name, "err", err)
		}
		return nil
	}
	rt.wake.Broadcast()
	k.eventLocked(fleet, &FleetEvent{Kind: FleetEventStop, Agent: name, Root: a.Root, Parent: caller})
	if err := k.saveLocked(); err != nil {
		k.logf("fleet: save after stop", "fleet", fleet, "agent", name, "err", err)
	}
	// Ending the seat unblocks a turn in progress. It runs apart from the
	// caller, whose own tool call may be part of that turn.
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		rt.seat.Stop()
	}()
	return nil
}

// fleetDescends walks up from name. Parent links always end at the owner,
// because a retiring agent hands its children to the owner, and a new agent's
// parent exists when it is created.
func fleetDescends(group *fleetGroupState, name, ancestor string) bool {
	seen := 0
	for a := group.Agents[name]; a != nil && seen <= len(group.Agents); a = group.Agents[a.Parent] {
		if a.Parent == ancestor {
			return true
		}
		seen++
	}
	return false
}

// StopTree stops everything under root and waits for it to be gone, so that a
// successor never shares a branch with a predecessor still running.
func (k *FleetKernel) StopTree(ctx context.Context, fleet, root string) error {
	ctx, cancel := context.WithTimeout(ctx, fleetStopWait)
	defer cancel()
	return k.stopAgents(ctx, fleet, func(a *fleetAgentState) bool { return a.Root == root })
}

// stopAgents stops every matching agent, including ones that appear meanwhile,
// and waits until none is left.
func (k *FleetKernel) stopAgents(ctx context.Context, fleet string, match func(*fleetAgentState) bool) error {
	for {
		k.mu.Lock()
		group := k.state.Fleets[fleet]
		var names []string
		if group != nil {
			for _, a := range group.Agents {
				if match(a) {
					names = append(names, a.Name)
				}
			}
		}
		k.mu.Unlock()
		if len(names) == 0 {
			return nil
		}
		for _, name := range names {
			// An agent still starting refuses; the next pass stops it.
			_ = k.Stop(fleet, FleetOwner, name)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopping agents: %w", ctx.Err())
		case <-time.After(fleetPoll):
		}
	}
}

// Shutdown stops every agent and deletes the fleet and its log. From the first
// call the fleet refuses new agents, so nothing can slip in behind it.
func (k *FleetKernel) Shutdown(ctx context.Context, fleet string) error {
	k.mu.Lock()
	group, err := k.groupLocked(fleet)
	if err == nil {
		group.Closing = true
		err = k.saveLocked()
	}
	k.mu.Unlock()
	if err != nil {
		return err
	}
	stopCtx, cancel := context.WithTimeout(ctx, fleetStopWait)
	defer cancel()
	if err := k.stopAgents(stopCtx, fleet, func(*fleetAgentState) bool { return true }); err != nil {
		// Not shut down after all, so it takes agents again.
		k.mu.Lock()
		if group := k.state.Fleets[fleet]; group != nil {
			group.Closing = false
			_ = k.saveLocked()
		}
		k.mu.Unlock()
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for key, timer := range k.timers {
		if strings.HasPrefix(key, fleet+"/") {
			timer.Stop()
			delete(k.timers, key)
		}
	}
	delete(k.state.Fleets, fleet)
	if log := k.logs[fleet]; log != nil {
		_ = log.file.Close()
		_ = os.Remove(log.path)
		delete(k.logs, fleet)
	}
	k.broadcastLocked()
	return k.saveLocked()
}

// Status reads what exists right now.
func (k *FleetKernel) Status(fleet string) (*FleetStatus, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	group, err := k.groupLocked(fleet)
	if err != nil {
		return nil, err
	}
	st := &FleetStatus{Agents: make([]FleetAgentStatus, 0, len(group.Agents)), Timers: len(group.Timers), LastSeq: k.logs[fleet].seq}
	for _, a := range group.Agents {
		row := FleetAgentStatus{Name: a.Name, Parent: a.Parent, Root: a.Root, Queued: len(a.Inbox), State: FleetStateStarting}
		if rt := k.running[fleetKey(fleet, a.Name)]; rt != nil {
			row.State = FleetStateIdle
			if a.Current != nil {
				row.State = FleetStateWorking
			}
			row.Model = rt.seat.Describe()
			row.IdleSeconds = int(time.Since(rt.seat.LastActivity()).Seconds())
		}
		st.Agents = append(st.Agents, row)
	}
	sort.Slice(st.Agents, func(i, j int) bool { return st.Agents[i].Name < st.Agents[j].Name })
	return st, nil
}

// Read returns the events after seq in order, waiting until there are some or
// ctx ends, and returns nil events in that case. Events persist across restarts
// and disconnects, so a client that was away loses nothing.
func (k *FleetKernel) Read(ctx context.Context, fleet string, after uint64) ([]FleetEvent, error) {
	for {
		k.mu.Lock()
		if _, err := k.groupLocked(fleet); err != nil {
			k.mu.Unlock()
			return nil, err
		}
		log := k.logs[fleet]
		last, path := log.seq, log.path
		events, cached := log.recentAfter(after)
		changed := k.changed
		k.mu.Unlock()
		if !cached && after < last {
			// Far behind: the whole file, read with nobody else waiting on it.
			return scanFleetEvents(path, after)
		}
		if len(events) > 0 {
			return events, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-k.ctx.Done():
			return nil, errFleetClosed
		case <-changed:
		}
	}
}

// AgentByToken finds the agent a seat's MCP URL belongs to. The token makes the
// URL its identity, so one seat cannot act as another.
func (k *FleetKernel) AgentByToken(token string) (fleet, name string, ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for fleetName, group := range k.state.Fleets {
		for _, a := range group.Agents {
			if a.Token == token {
				return fleetName, a.Name, true
			}
		}
	}
	return "", "", false
}

func (k *FleetKernel) groupLocked(fleet string) (*fleetGroupState, error) {
	if k.closed {
		return nil, errFleetClosed
	}
	group := k.state.Fleets[fleet]
	if group == nil {
		return nil, fmt.Errorf("no fleet named %q", fleet)
	}
	return group, nil
}

func (k *FleetKernel) statePath() string { return filepath.Join(k.dir, "fleet-state.json") }

// saveLocked writes all state atomically. It is small: messages leave the state
// as soon as they are answered, and the history lives in the event logs.
func (k *FleetKernel) saveLocked() error {
	raw, err := json.Marshal(&k.state)
	if err != nil {
		return fmt.Errorf("fleet state: %w", err)
	}
	tmp := k.statePath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("fleet state: %w", err)
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, k.statePath())
	}
	if err != nil {
		return fmt.Errorf("fleet state: %w", err)
	}
	return nil
}

func (k *FleetKernel) broadcastLocked() {
	close(k.changed)
	k.changed = make(chan struct{})
}

// eventLocked appends to the fleet's log and wakes readers. k.mu held.
func (k *FleetKernel) eventLocked(fleet string, e *FleetEvent) {
	log := k.logs[fleet]
	if log == nil {
		return
	}
	log.seq++
	e.Seq = log.seq
	// Every field that can carry outside text is bounded, so one event always
	// fits a read.
	e.Text = boundFleetText(e.Text)
	e.Cause = boundFleetText(e.Cause)
	e.At = time.Now().UTC()
	raw, err := json.Marshal(e)
	if err == nil {
		_, err = log.file.Write(append(raw, '\n'))
	}
	if err != nil {
		k.logf("fleet: event log", "fleet", fleet, "err", err)
	}
	log.recent = append(log.recent, *e)
	if len(log.recent) > fleetEventRing {
		log.recent = append([]FleetEvent(nil), log.recent[len(log.recent)-fleetEventRing:]...)
	}
	k.broadcastLocked()
}

func formatFleetMessage(m *FleetMessage) string {
	switch m.Kind {
	case FleetTimer:
		return "[timer " + m.ID + "]\n" + m.Text
	case FleetNotice:
		return "[notice from " + m.From + "]\n" + m.Text
	case FleetResponse:
		if m.InReplyTo != "" {
			return "[response from " + m.From + " to " + m.InReplyTo + "]\n" + m.Text
		}
		return "[message from " + m.From + "]\n" + m.Text
	default:
		return "[request " + m.ID + " from " + m.From + "]\n" + m.Text
	}
}

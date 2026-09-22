// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The broker wire contract (🎯T2.1). Six sub-targets (🎯T2.2–🎯T2.7) and the SDK
// pivot (🎯T3) are specified against what is in this file, so it is deliberately
// explicit rather than "whatever encoding/json does":
//
//   - Every message carries a version. An absent or unequal `v` is rejected; it
//     is never defaulted, because a silently-defaulted version is a wire break
//     that presents as a behaviour change.
//   - Decoding is strict in both directions. A field the broker does not know
//     is a typed unknown_field error, never a silent drop — the exact defect
//     class 🎯T24 hit this repo twice in one day.
//   - Unknown message types, and messages sent in the wrong direction, are
//     typed errors with their own codes rather than a dropped connection, so a
//     newer client talking to an older broker gets a diagnosable answer.
//
// Encoding is canonical: Encode is the inverse of Parse for every message the
// broker emits, which is what the golden vectors in testdata/wire pin.

// Version is the only wire-protocol version this broker speaks. A peer that
// sends anything else is answered with CodeUnsupportedVersion listing what is
// supported, rather than being interpreted under a guess.
const Version = 1

// MessageType is the discriminator carried in every envelope. Request types and
// response types share one namespace so that a message sent in the wrong
// direction is diagnosable (CodeNotARequest / CodeNotAResponse) rather than
// merely unknown.
type MessageType string

// Request types (client → broker). The four here are 🎯T2.1's declared minimum.
const (
	// TypeSpawn asks the broker for an agent.
	TypeSpawn MessageType = "spawn"
	// TypeRelease returns an agent the caller holds.
	TypeRelease MessageType = "release"
	// TypeStatus asks for a snapshot of what the broker is managing.
	TypeStatus MessageType = "status"
	// TypeTail converts the connection into a one-way event stream.
	TypeTail MessageType = "tail"
)

// Response types (broker → client).
const (
	// TypeSpawned answers TypeSpawn with the granted session.
	TypeSpawned MessageType = "spawned"
	// TypeReleased answers TypeRelease.
	TypeReleased MessageType = "released"
	// TypeStatusResult answers TypeStatus. It is spelled differently from the
	// request so that a message in the wrong direction is always detectable.
	TypeStatusResult MessageType = "status_result"
	// TypeTailing acknowledges TypeTail. It exists because the alternative —
	// answering a subscription with silence — makes it impossible for a caller
	// to know when it is subscribed, so every tail would race the first event
	// it wanted to see. The ack is written after the subscription is live.
	TypeTailing MessageType = "tailing"
	// TypeEvent is one lifecycle event pushed on a tail connection.
	TypeEvent MessageType = "event"
	// TypeError answers any request the broker declines to honour.
	TypeError MessageType = "error"
)

// Provider names the agent runtime the broker is asked to spawn.
type Provider string

// Providers. The broker honours claude only; anything else is refused by name
// rather than quietly spawning claude anyway.
const (
	// ProviderUnset means the caller did not choose; it normalises to claude.
	ProviderUnset Provider = ""
	// ProviderClaude is Claude Code.
	ProviderClaude Provider = "claude"
)

// Mode is the interaction shape of the requested agent.
type Mode string

// Modes.
const (
	// ModeSession is a long-lived interactive session.
	ModeSession Mode = "session"
	// ModeTask is a one-shot batch task.
	ModeTask Mode = "task"
)

// Intent is the caller's priority hint. Policy (🎯T2.6) turns it into a tier;
// the broker records it verbatim so the caller can see what was received.
type Intent string

// Intents.
const (
	// IntentUnset means the caller did not hint; it normalises to auto.
	IntentUnset Intent = ""
	// IntentAuto lets the broker infer priority from mode and recency.
	IntentAuto Intent = "auto"
	// IntentInteractive marks work a human is waiting on.
	IntentInteractive Intent = "interactive"
	// IntentBatch marks work that may be preempted.
	IntentBatch Intent = "batch"
)

// Disposition is what a release asks the broker to do with the agent.
type Disposition string

// Dispositions.
const (
	// DispositionStop tears the agent down.
	DispositionStop Disposition = "stop"
	// DispositionReuse returns the agent to the warm pool. Syntactically valid
	// on the wire from v1; the broker refuses it with CodeUnsupportedValue
	// until the shared pool (🎯T2.3) exists, rather than accepting it and
	// stopping the agent behind the caller's back.
	DispositionReuse Disposition = "reuse"
)

// EventKind classifies a lifecycle event on the tail stream.
type EventKind string

// Event kinds emitted at 🎯T2.1. Policy sub-targets add their own (throttle,
// reap, preempt, resume, cost).
const (
	// EventSpawn reports an agent granted to a consumer.
	EventSpawn EventKind = "spawn"
	// EventRelease reports an agent returned by its consumer.
	EventRelease EventKind = "release"
	// EventReclaim reports an agent the broker took back because its
	// consumer's connection went away. It is distinct from EventRelease
	// because the consumer never asked for it.
	EventReclaim EventKind = "reclaim"
)

// ErrorCode is the machine-readable half of a TypeError message. Callers switch
// on this, never on the human-readable message.
type ErrorCode string

// Error codes.
const (
	// CodeUnsupportedVersion means the envelope's v is not Version.
	CodeUnsupportedVersion ErrorCode = "unsupported_version"
	// CodeMalformed means the line was not the JSON the contract requires.
	CodeMalformed ErrorCode = "malformed"
	// CodeUnknownField means the peer set a field this broker has no meaning
	// for. It is an error precisely so it can never be a silent drop.
	CodeUnknownField ErrorCode = "unknown_field"
	// CodeUnknownType means the type discriminator is not in this version's
	// namespace at all.
	CodeUnknownType ErrorCode = "unknown_type"
	// CodeNotARequest means a response type arrived on the client → broker
	// direction.
	CodeNotARequest ErrorCode = "not_a_request"
	// CodeNotAResponse means a request type arrived on the broker → client
	// direction.
	CodeNotAResponse ErrorCode = "not_a_response"
	// CodeMissingField means a required field was absent or empty.
	CodeMissingField ErrorCode = "missing_field"
	// CodeUnsupportedValue means a known field carried a value this broker
	// cannot honour.
	CodeUnsupportedValue ErrorCode = "unsupported_value"
	// CodeUnknownSession means the named session is not one the broker holds.
	CodeUnknownSession ErrorCode = "unknown_session"
	// CodeNotOwner means the session exists but belongs to another connection.
	CodeNotOwner ErrorCode = "not_owner"
	// CodeSpawnFailed means the broker accepted the request but could not
	// start an agent.
	CodeSpawnFailed ErrorCode = "spawn_failed"
	// CodeReleaseFailed means the broker could not tear the agent down. The
	// session stays in the registry, because the agent is still out there.
	CodeReleaseFailed ErrorCode = "release_failed"
	// CodeFrameTooLarge means one line exceeded the wire's line limit. The
	// frame is dropped; the connection is not. It has its own code rather
	// than folding into CodeMalformed because the two call for opposite
	// answers: a malformed line means the peer is speaking a protocol this
	// broker does not know, while an oversized one means the peer is speaking
	// this protocol correctly about something too big to relay, and a producer
	// told that can bound what it sends next.
	CodeFrameTooLarge ErrorCode = "frame_too_large"
	// CodeTailLagged means a tail subscriber fell far enough behind that the
	// broker had to choose between blocking its own policy loop and dropping
	// events. It does neither silently: the subscriber is told its stream is
	// incomplete and the connection is closed, so a caller can never mistake a
	// truncated event history for the whole one.
	CodeTailLagged ErrorCode = "tail_lagged"
)

// Envelope is the outer frame of every message. One envelope per line; the
// stream is newline-delimited JSON.
type Envelope struct {
	// V is the wire version. Required, and required to equal Version.
	V int `json:"v"`
	// Type discriminates the body.
	Type MessageType `json:"type"`
	// ID correlates a response with its request. The broker echoes whatever
	// the caller sent, including the empty string.
	ID string `json:"id,omitempty"`
	// Body is the type-specific payload, absent for types that have none.
	Body json.RawMessage `json:"body,omitempty"`
}

// SpawnRequest asks the broker for an agent.
//
// Every field here is honoured: the broker either acts on it or records it and
// reports it back on the status snapshot. A field that could not be honoured
// would have to be refused with CodeUnsupportedValue instead — see
// TestEverySpawnRequestFieldIsHonouredOrRefused, which fails the build if a new
// field is added without deciding which it is.
type SpawnRequest struct {
	// Provider selects the agent runtime. Empty normalises to ProviderClaude.
	Provider Provider `json:"provider,omitempty"`
	// Mode is required: session or task.
	Mode Mode `json:"mode"`
	// Model is passed through to the provider binary verbatim.
	Model string `json:"model,omitempty"`
	// Intent is the priority hint. Empty normalises to IntentAuto.
	Intent Intent `json:"intent,omitempty"`
	// WorkDir is required: the directory the agent runs in.
	WorkDir string `json:"workdir"`
}

// Validate normalises defaults and reports the first field the broker cannot
// honour. It is called by ParseRequest, so a decoded SpawnRequest is always one
// the broker has agreed to.
func (r *SpawnRequest) Validate() error {
	switch r.Provider {
	case ProviderUnset:
		r.Provider = ProviderClaude
	case ProviderClaude:
	default:
		return &ProtocolError{Code: CodeUnsupportedValue, Field: "provider", Value: string(r.Provider),
			Msg: fmt.Sprintf("provider %q is not supported; this broker spawns %q only", r.Provider, ProviderClaude)}
	}
	switch r.Mode {
	case "":
		return &ProtocolError{Code: CodeMissingField, Field: "mode",
			Msg: fmt.Sprintf("mode is required (%q or %q)", ModeSession, ModeTask)}
	case ModeSession, ModeTask:
	default:
		return &ProtocolError{Code: CodeUnsupportedValue, Field: "mode", Value: string(r.Mode),
			Msg: fmt.Sprintf("mode %q is not one of %q, %q", r.Mode, ModeSession, ModeTask)}
	}
	switch r.Intent {
	case IntentUnset:
		r.Intent = IntentAuto
	case IntentAuto, IntentInteractive, IntentBatch:
	default:
		return &ProtocolError{Code: CodeUnsupportedValue, Field: "intent", Value: string(r.Intent),
			Msg: fmt.Sprintf("intent %q is not one of %q, %q, %q", r.Intent, IntentAuto, IntentInteractive, IntentBatch)}
	}
	if strings.TrimSpace(r.WorkDir) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "workdir",
			Msg: "workdir is required: the broker will not guess the agent's working directory"}
	}
	return nil
}

// ReleaseRequest returns an agent to the broker. Wire v1 keyed it by the
// spawn session id; the grant protocol keys it by grant name. Exactly one of
// the two is required.
type ReleaseRequest struct {
	// SessionID names the agent, as returned by SpawnResponse.
	SessionID string `json:"session_id,omitempty"`
	// Name names a granted seat (🎯T2.10).
	Name string `json:"name,omitempty"`
	// Disposition is what to do with it.
	Disposition Disposition `json:"disposition"`
	// KeepAliveSeconds, with reuse on a pooled seat, keeps the returned
	// seat warm for this long before the pool may evict it
	// (claudia.Agent.Release "keep_alive_for:<secs>").
	KeepAliveSeconds int64 `json:"keep_alive_seconds,omitempty"`
	// Force, with DispositionDetach, lets an operator on a third connection
	// clear a grant another connection owns (🎯T124). The old owner is sent
	// agent_detached; the seat process keeps running and can be re-granted.
	Force bool `json:"force,omitempty"`
}

// Validate checks the request is well formed on the wire. Whether this broker
// can honour a syntactically valid disposition is a capability question, and is
// answered by the server (DispositionReuse needs the shared pool, 🎯T2.3).
func (r *ReleaseRequest) Validate() error {
	if strings.TrimSpace(r.SessionID) == "" && strings.TrimSpace(r.Name) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "session_id",
			Msg: "session_id (spawned agent) or name (granted seat) is required"}
	}
	if r.Force && r.Disposition != DispositionDetach {
		return &ProtocolError{Code: CodeUnsupportedValue, Field: "force", Value: "true",
			Msg: "force applies only to detach"}
	}
	switch r.Disposition {
	case "":
		return &ProtocolError{Code: CodeMissingField, Field: "disposition",
			Msg: fmt.Sprintf("disposition is required (%q, %q or %q)", DispositionStop, DispositionReuse, DispositionDetach)}
	case DispositionStop, DispositionDetach:
		if r.KeepAliveSeconds != 0 {
			return &ProtocolError{Code: CodeUnsupportedValue, Field: "keep_alive_seconds", Value: strconv.FormatInt(r.KeepAliveSeconds, 10),
				Msg: "keep_alive_seconds applies only to reuse"}
		}
		return nil
	case DispositionReuse:
		if r.KeepAliveSeconds < 0 {
			return &ProtocolError{Code: CodeUnsupportedValue, Field: "keep_alive_seconds", Value: strconv.FormatInt(r.KeepAliveSeconds, 10),
				Msg: "keep_alive_seconds must not be negative"}
		}
		return nil
	default:
		return &ProtocolError{Code: CodeUnsupportedValue, Field: "disposition", Value: string(r.Disposition),
			Msg: fmt.Sprintf("disposition %q is not one of %q, %q, %q", r.Disposition, DispositionStop, DispositionReuse, DispositionDetach)}
	}
}

// StatusRequest asks for a snapshot. It has no fields: sending a body with one
// is an unknown_field error, so a caller cannot believe it filtered a snapshot
// that was never filtered.
type StatusRequest struct{}

// TailRequest subscribes the connection to the event stream. It has no fields,
// for the same reason StatusRequest has none.
type TailRequest struct{}

// SpawnResponse reports the granted agent.
type SpawnResponse struct {
	// SessionID is the handle the caller releases with.
	SessionID string `json:"session_id"`
	// PID is the provider process the broker started.
	PID int `json:"pid"`
	// Warm reports whether the agent came from the pool rather than a cold
	// spawn. Always false until 🎯T2.3.
	Warm bool `json:"warm"`
}

// ReleaseResponse confirms a release, echoing what the broker actually did.
type ReleaseResponse struct {
	// SessionID is the released agent (spawn path).
	SessionID string `json:"session_id,omitempty"`
	// Name is the released seat (grant path).
	Name string `json:"name,omitempty"`
	// Disposition is what the broker performed, not merely what was asked.
	Disposition Disposition `json:"disposition"`
}

// TailResponse acknowledges a tail subscription. It has no fields: everything
// a tailer needs arrives as events, and a field here would be a second place to
// learn the same thing.
type TailResponse struct{}

// SessionStatus is one managed agent in a status snapshot. It echoes the whole
// spawn request back, which is how a caller confirms that every field it set
// was honoured rather than dropped.
type SessionStatus struct {
	// SessionID is the agent's handle.
	SessionID string `json:"session_id"`
	// Provider is the runtime, after normalisation.
	Provider Provider `json:"provider"`
	// Mode is the requested interaction shape.
	Mode Mode `json:"mode"`
	// Model is the requested model, empty when the caller did not choose.
	Model string `json:"model,omitempty"`
	// Intent is the priority hint, after normalisation.
	Intent Intent `json:"intent"`
	// WorkDir is the agent's working directory.
	WorkDir string `json:"workdir"`
	// PID is the provider process.
	PID int `json:"pid"`
	// Warm reports whether the agent was pooled.
	Warm bool `json:"warm"`
}

// StatusResponse is the broker's snapshot.
type StatusResponse struct {
	// ProtocolVersion is the version the broker speaks.
	ProtocolVersion int `json:"protocol_version"`
	// ActiveAgents is len(Sessions), carried explicitly so a caller that only
	// wants the count need not walk the list.
	ActiveAgents int `json:"active_agents"`
	// WarmPool is the idle inventory. Always 0 until 🎯T2.3.
	WarmPool int `json:"warm_pool"`
	// Sessions is every agent the broker currently holds.
	Sessions []SessionStatus `json:"sessions"`
	// Grants is every named seat the daemon holds (🎯T2.10). Absent on a
	// bare protocol server.
	Grants []GrantStatus `json:"grants,omitempty"`
	// Tasks is how many task runs are in flight.
	Tasks int `json:"tasks,omitempty"`
	// UsageFetchedAt is when the daemon last refreshed plan usage (🎯T2.9).
	// Zero when it never has.
	UsageFetchedAt time.Time `json:"usage_fetched_at,omitzero"`
}

// EventMessage is one lifecycle event on a tail connection.
type EventMessage struct {
	// Kind is what happened.
	Kind EventKind `json:"kind"`
	// SessionID is the agent it happened to.
	SessionID string `json:"session_id,omitempty"`
	// Name is the grant it happened to (grant protocol events).
	Name string `json:"name,omitempty"`
	// Detail is a short human-readable qualifier (a provider name on
	// usage_update, a reason on agent_gone).
	Detail string `json:"detail,omitempty"`
	// At is stamped from the broker's injected Clock, never the wall clock,
	// so a replayed tape is reproducible (🎯T2.8).
	At time.Time `json:"at"`
}

// ErrorMessage is the broker's typed refusal.
type ErrorMessage struct {
	// Code is what callers switch on.
	Code ErrorCode `json:"code"`
	// Message is for humans and logs.
	Message string `json:"message"`
	// Field names the offending field when there is one.
	Field string `json:"field,omitempty"`
	// Value is the offending value when there is one.
	Value string `json:"value,omitempty"`
	// SupportedVersions is populated on CodeUnsupportedVersion so a peer can
	// renegotiate without a lookup table.
	SupportedVersions []int `json:"supported_versions,omitempty"`
}

// ProtocolError is a refusal in Go form. Every rejection path in this package
// produces one, so a caller can errors.As it and read Code rather than matching
// on strings.
type ProtocolError struct {
	// Code is the machine-readable classification.
	Code ErrorCode
	// Msg is the human-readable explanation.
	Msg string
	// Field is the offending field, when known.
	Field string
	// Value is the offending value, when known.
	Value string
	// SupportedVersions is set on CodeUnsupportedVersion.
	SupportedVersions []int
	// ID is the envelope id the error relates to, so the server can correlate
	// a refusal with the request that caused it even when parsing failed
	// before a Request was built. It is not part of the error body.
	ID string
}

// Error renders the refusal.
func (e *ProtocolError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("broker protocol: %s (%s=%q): %s", e.Code, e.Field, e.Value, e.Msg)
	}
	return fmt.Sprintf("broker protocol: %s: %s", e.Code, e.Msg)
}

// Unwrap names the sentinel behind a code that has one, so errors.Is works on
// a refusal this process raised and, identically, on one it decoded off the
// wire: the code is the identity, so a client can match a remote broker's
// oversized-frame refusal without reaching into the message text.
func (e *ProtocolError) Unwrap() error {
	if e.Code == CodeFrameTooLarge {
		return ErrFrameTooLarge
	}
	return nil
}

// Wire converts the error into the body the broker puts on the socket.
func (e *ProtocolError) Wire() *ErrorMessage {
	return &ErrorMessage{
		Code:              e.Code,
		Message:           e.Msg,
		Field:             e.Field,
		Value:             e.Value,
		SupportedVersions: e.SupportedVersions,
	}
}

// Err converts a received ErrorMessage back into a ProtocolError, so client
// code sees the same type the server raised.
func (m *ErrorMessage) Err() *ProtocolError {
	return &ProtocolError{
		Code:              m.Code,
		Msg:               m.Message,
		Field:             m.Field,
		Value:             m.Value,
		SupportedVersions: m.SupportedVersions,
	}
}

// Request is a decoded client → broker message. Exactly one of the body
// pointers is non-nil, selected by Type.
type Request struct {
	// ID is the caller's correlation id.
	ID string
	// Type is the discriminator.
	Type MessageType

	Spawn         *SpawnRequest
	Release       *ReleaseRequest
	Status        *StatusRequest
	Tail          *TailRequest
	Usage         *UsageRequest
	Resolve       *ResolveRequest
	TaskRun       *TaskRunRequest
	TaskCancel    *TaskCancelRequest
	Grant         *GrantRequest
	Send          *SendRequest
	Interrupt     *NamedRequest
	SetModel      *SetModelRequest
	Migrate       *MigrateRequest
	AgentInfo     *NamedRequest
	TermSubscribe *NamedRequest
	Resize        *ResizeRequest
	Grants        *GrantsRequest
	CloseGoal     *NamedRequest
	Rewind        *RewindRequest
	GoalVerdict   *GoalVerdictRequest
	Judge         *JudgeRequest
}

// Response is a decoded broker → client message. Exactly one of the body
// pointers is non-nil, selected by Type.
type Response struct {
	// ID echoes the request's correlation id.
	ID string
	// Type is the discriminator.
	Type MessageType

	Spawned          *SpawnResponse
	Released         *ReleaseResponse
	Status           *StatusResponse
	Tailing          *TailResponse
	Event            *EventMessage
	Error            *ErrorMessage
	Usage            *UsageResponse
	Resolved         *ResolveResponse
	TaskStarted      *TaskStartedResponse
	TaskEvent        *TaskEventMessage
	TaskDone         *TaskDoneMessage
	TaskRaw          *TaskRawMessage
	TaskCancelled    *TaskCancelledResponse
	Granted          *GrantResponse
	AgentEvent       *AgentEventMessage
	AgentTerm        *AgentTermMessage
	AgentGone        *AgentGoneMessage
	AgentDetached    *AgentDetachedMessage
	Sent             *SentResponse
	Interrupted      *NamedResponse
	ModelSet         *NamedResponse
	Migrated         *MigrateResponse
	AgentInfo        *AgentInfoResponse
	TermSubscribed   *TermSubscribedResponse
	Resized          *NamedResponse
	Grants           *GrantsResponse
	GoalClosed       *NamedResponse
	Rewound          *RewindResponse
	GoalCheck        *GoalCheckMessage
	GoalVerdictNoted *NamedResponse
	Judged           *JudgedResponse
}

// validator is implemented by bodies that normalise defaults or refuse
// values on receipt.
type validator interface{ Validate() error }

// bodySpec binds one message type to its body slot. alloc creates the body
// and stores it on the message; get returns the stored body (nil when the
// slot is empty); noBody marks types whose canonical encoding omits the
// body entirely (status, tail, tailing) — the transport still accepts an
// empty object for them.
type bodySpec[M any] struct {
	what   string
	alloc  func(*M) any
	get    func(*M) any
	noBody bool
}

func spec[M, B any](what string, slot func(*M) **B, noBody bool) bodySpec[M] {
	return bodySpec[M]{
		what: what,
		alloc: func(m *M) any {
			b := new(B)
			*slot(m) = b
			return b
		},
		get: func(m *M) any {
			if p := *slot(m); p != nil {
				return p
			}
			return nil
		},
		noBody: noBody,
	}
}

// requestSpecs is the client → broker namespace. TestEveryMessageTypeHasAVector
// walks it, so a type added here without a golden vector fails the build.
var requestSpecs = map[MessageType]bodySpec[Request]{
	TypeSpawn:         spec("spawn body", func(r *Request) **SpawnRequest { return &r.Spawn }, false),
	TypeRelease:       spec("release body", func(r *Request) **ReleaseRequest { return &r.Release }, false),
	TypeStatus:        spec("status body", func(r *Request) **StatusRequest { return &r.Status }, true),
	TypeTail:          spec("tail body", func(r *Request) **TailRequest { return &r.Tail }, true),
	TypeUsage:         spec("usage body", func(r *Request) **UsageRequest { return &r.Usage }, false),
	TypeResolve:       spec("resolve body", func(r *Request) **ResolveRequest { return &r.Resolve }, false),
	TypeTaskRun:       spec("task_run body", func(r *Request) **TaskRunRequest { return &r.TaskRun }, false),
	TypeTaskCancel:    spec("task_cancel body", func(r *Request) **TaskCancelRequest { return &r.TaskCancel }, false),
	TypeGrant:         spec("grant body", func(r *Request) **GrantRequest { return &r.Grant }, false),
	TypeSend:          spec("send body", func(r *Request) **SendRequest { return &r.Send }, false),
	TypeInterrupt:     spec("interrupt body", func(r *Request) **NamedRequest { return &r.Interrupt }, false),
	TypeSetModel:      spec("set_model body", func(r *Request) **SetModelRequest { return &r.SetModel }, false),
	TypeMigrate:       spec("migrate body", func(r *Request) **MigrateRequest { return &r.Migrate }, false),
	TypeAgentInfo:     spec("agent_info body", func(r *Request) **NamedRequest { return &r.AgentInfo }, false),
	TypeTermSubscribe: spec("term_subscribe body", func(r *Request) **NamedRequest { return &r.TermSubscribe }, false),
	TypeResize:        spec("resize body", func(r *Request) **ResizeRequest { return &r.Resize }, false),
	TypeGrants:        spec("grants body", func(r *Request) **GrantsRequest { return &r.Grants }, true),
	TypeCloseGoal:     spec("close_goal body", func(r *Request) **NamedRequest { return &r.CloseGoal }, false),
	TypeRewind:        spec("rewind body", func(r *Request) **RewindRequest { return &r.Rewind }, false),
	TypeGoalVerdict:   spec("goal_verdict body", func(r *Request) **GoalVerdictRequest { return &r.GoalVerdict }, false),
	TypeJudge:         spec("judge body", func(r *Request) **JudgeRequest { return &r.Judge }, false),
}

// responseSpecs is the broker → client namespace.
var responseSpecs = map[MessageType]bodySpec[Response]{
	TypeSpawned:          spec("spawned body", func(r *Response) **SpawnResponse { return &r.Spawned }, false),
	TypeReleased:         spec("released body", func(r *Response) **ReleaseResponse { return &r.Released }, false),
	TypeStatusResult:     spec("status_result body", func(r *Response) **StatusResponse { return &r.Status }, false),
	TypeTailing:          spec("tailing body", func(r *Response) **TailResponse { return &r.Tailing }, true),
	TypeEvent:            spec("event body", func(r *Response) **EventMessage { return &r.Event }, false),
	TypeError:            spec("error body", func(r *Response) **ErrorMessage { return &r.Error }, false),
	TypeUsageResult:      spec("usage_result body", func(r *Response) **UsageResponse { return &r.Usage }, false),
	TypeResolved:         spec("resolved body", func(r *Response) **ResolveResponse { return &r.Resolved }, false),
	TypeTaskStarted:      spec("task_started body", func(r *Response) **TaskStartedResponse { return &r.TaskStarted }, false),
	TypeTaskEvent:        spec("task_event body", func(r *Response) **TaskEventMessage { return &r.TaskEvent }, false),
	TypeTaskDone:         spec("task_done body", func(r *Response) **TaskDoneMessage { return &r.TaskDone }, false),
	TypeTaskRaw:          spec("task_raw body", func(r *Response) **TaskRawMessage { return &r.TaskRaw }, false),
	TypeTaskCancelled:    spec("task_cancelled body", func(r *Response) **TaskCancelledResponse { return &r.TaskCancelled }, false),
	TypeGranted:          spec("granted body", func(r *Response) **GrantResponse { return &r.Granted }, false),
	TypeAgentEvent:       spec("agent_event body", func(r *Response) **AgentEventMessage { return &r.AgentEvent }, false),
	TypeAgentTerm:        spec("agent_term body", func(r *Response) **AgentTermMessage { return &r.AgentTerm }, false),
	TypeAgentGone:        spec("agent_gone body", func(r *Response) **AgentGoneMessage { return &r.AgentGone }, false),
	TypeAgentDetached:    spec("agent_detached body", func(r *Response) **AgentDetachedMessage { return &r.AgentDetached }, false),
	TypeSent:             spec("sent body", func(r *Response) **SentResponse { return &r.Sent }, false),
	TypeInterrupted:      spec("interrupted body", func(r *Response) **NamedResponse { return &r.Interrupted }, false),
	TypeModelSet:         spec("model_set body", func(r *Response) **NamedResponse { return &r.ModelSet }, false),
	TypeMigrated:         spec("migrated body", func(r *Response) **MigrateResponse { return &r.Migrated }, false),
	TypeAgentInfoResult:  spec("agent_info_result body", func(r *Response) **AgentInfoResponse { return &r.AgentInfo }, false),
	TypeTermSubscribed:   spec("term_subscribed body", func(r *Response) **TermSubscribedResponse { return &r.TermSubscribed }, false),
	TypeResized:          spec("resized body", func(r *Response) **NamedResponse { return &r.Resized }, false),
	TypeGrantsResult:     spec("grants_result body", func(r *Response) **GrantsResponse { return &r.Grants }, false),
	TypeGoalClosed:       spec("goal_closed body", func(r *Response) **NamedResponse { return &r.GoalClosed }, false),
	TypeRewound:          spec("rewound body", func(r *Response) **RewindResponse { return &r.Rewound }, false),
	TypeGoalCheck:        spec("goal_check body", func(r *Response) **GoalCheckMessage { return &r.GoalCheck }, false),
	TypeGoalVerdictNoted: spec("goal_verdict_noted body", func(r *Response) **NamedResponse { return &r.GoalVerdictNoted }, false),
	TypeJudged:           spec("judged body", func(r *Response) **JudgedResponse { return &r.Judged }, false),
}

// RequestTypes lists every client → broker type in this wire version.
func RequestTypes() []MessageType { return sortedTypes(requestSpecs) }

// ResponseTypes lists every broker → client type in this wire version.
func ResponseTypes() []MessageType { return sortedTypes(responseSpecs) }

func sortedTypes[M any](m map[MessageType]bodySpec[M]) []MessageType {
	out := make([]MessageType, 0, len(m))
	for t := range m {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// unknownFieldPrefix is encoding/json's wording for the rejection
// DisallowUnknownFields produces. Parsing it is how the offending field name
// reaches the caller. TestUnknownFieldErrorShapeIsPinned fails if a Go release
// changes the wording, so the day that happens we lose the field name loudly
// rather than silently.
const unknownFieldPrefix = "json: unknown field "

// unknownField extracts the field name from an encoding/json strict-mode error.
func unknownField(err error) (string, bool) {
	rest, ok := strings.CutPrefix(err.Error(), unknownFieldPrefix)
	if !ok {
		return "", false
	}
	return strings.Trim(rest, `"`), true
}

// decodeStrict unmarshals one JSON value into v, refusing unknown fields and
// trailing data. what names the thing being decoded, for the error message.
func decodeStrict(data []byte, v any, what string) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if field, ok := unknownField(err); ok {
			return &ProtocolError{Code: CodeUnknownField, Field: field,
				Msg: fmt.Sprintf("%s has no field %q; the broker refuses fields it cannot honour rather than dropping them", what, field)}
		}
		return &ProtocolError{Code: CodeMalformed, Msg: fmt.Sprintf("%s: %v", what, err)}
	}
	if dec.More() {
		return &ProtocolError{Code: CodeMalformed, Msg: what + ": trailing data after the JSON value"}
	}
	return nil
}

// decodeBody decodes an envelope body into v. An absent body decodes as the
// empty object, so a request whose fields are all optional need not send one.
func decodeBody(body json.RawMessage, v any, what string) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	return decodeStrict(body, v, what)
}

// parseEnvelope decodes and version-checks the outer frame.
func parseEnvelope(line []byte) (*Envelope, error) {
	var env Envelope
	if err := decodeStrict(line, &env, "envelope"); err != nil {
		return nil, err
	}
	if env.V != Version {
		return nil, &ProtocolError{Code: CodeUnsupportedVersion, Field: "v", Value: strconv.Itoa(env.V),
			SupportedVersions: []int{Version}, ID: env.ID,
			Msg: fmt.Sprintf("wire version %d is not supported; this broker speaks version %d, and will not interpret an unversioned message under a guess", env.V, Version)}
	}
	return &env, nil
}

// parseBody allocates, decodes and validates one body under its spec.
func parseBody[M any](env *Envelope, sp bodySpec[M], m *M) error {
	body := sp.alloc(m)
	if err := decodeBody(env.Body, body, sp.what); err != nil {
		return withID(err, env.ID)
	}
	if v, ok := body.(validator); ok {
		if err := v.Validate(); err != nil {
			return withID(err, env.ID)
		}
	}
	return nil
}

// ParseRequest decodes one newline-delimited-JSON line as a client → broker
// message, validating the body. The returned error is always a *ProtocolError,
// carrying the envelope id when one was readable.
func ParseRequest(line []byte) (*Request, error) {
	env, err := parseEnvelope(line)
	if err != nil {
		return nil, err
	}
	req := &Request{ID: env.ID, Type: env.Type}
	if sp, ok := requestSpecs[env.Type]; ok {
		if err := parseBody(env, sp, req); err != nil {
			return nil, err
		}
		return req, nil
	}
	if _, ok := responseSpecs[env.Type]; ok {
		return nil, &ProtocolError{Code: CodeNotARequest, Field: "type", Value: string(env.Type), ID: env.ID,
			Msg: fmt.Sprintf("%q is a broker → client message; the broker does not accept it", env.Type)}
	}
	return nil, &ProtocolError{Code: CodeUnknownType, Field: "type", Value: string(env.Type), ID: env.ID,
		Msg: fmt.Sprintf("%q is not a request type in wire version %d", env.Type, Version)}
}

// ParseResponse decodes one line as a broker → client message. Clients use it;
// it is the mirror of ParseRequest, so a request type arriving on this
// direction is CodeNotAResponse rather than merely unknown.
func ParseResponse(line []byte) (*Response, error) {
	env, err := parseEnvelope(line)
	if err != nil {
		return nil, err
	}
	resp := &Response{ID: env.ID, Type: env.Type}
	if sp, ok := responseSpecs[env.Type]; ok {
		if err := parseBody(env, sp, resp); err != nil {
			return nil, err
		}
		return resp, nil
	}
	if _, ok := requestSpecs[env.Type]; ok {
		return nil, &ProtocolError{Code: CodeNotAResponse, Field: "type", Value: string(env.Type), ID: env.ID,
			Msg: fmt.Sprintf("%q is a client → broker message; a broker never sends it", env.Type)}
	}
	return nil, &ProtocolError{Code: CodeUnknownType, Field: "type", Value: string(env.Type), ID: env.ID,
		Msg: fmt.Sprintf("%q is not a response type in wire version %d", env.Type, Version)}
}

// withID stamps the envelope id onto a *ProtocolError so the server can echo it
// on the refusal.
func withID(err error, id string) error {
	pe, ok := err.(*ProtocolError)
	if !ok {
		return err
	}
	pe.ID = id
	return pe
}

// encodeMessage renders one envelope. body may be nil for types that carry
// none. The result has no trailing newline; framing is the transport's job.
func encodeMessage(t MessageType, id string, body any) ([]byte, error) {
	env := Envelope{V: Version, Type: t, ID: id}
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode %s body: %w", t, err)
		}
		env.Body = raw
	}
	line, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("encode %s envelope: %w", t, err)
	}
	return line, nil
}

// encodeUnder renders a message under its spec. Body-less types encode with no
// body at all; every other type encodes whatever sits in its slot, so a nil
// slot on a typed message renders as an absent body — the same bytes the
// parser accepts as the empty object.
func encodeUnder[M any](specs map[MessageType]bodySpec[M], t MessageType, id string, m *M, direction string) ([]byte, error) {
	sp, ok := specs[t]
	if !ok {
		return nil, &ProtocolError{Code: CodeUnknownType, Field: "type", Value: string(t),
			Msg: fmt.Sprintf("%q is not a %s type in wire version %d", t, direction, Version)}
	}
	if sp.noBody {
		return encodeMessage(t, id, nil)
	}
	return encodeMessage(t, id, sp.get(m))
}

// Encode renders the request canonically. Status, tail and grants carry no
// body, so their canonical form omits it entirely.
func (r *Request) Encode() ([]byte, error) {
	return encodeUnder(requestSpecs, r.Type, r.ID, r, "request")
}

// Encode renders the response canonically.
func (r *Response) Encode() ([]byte, error) {
	return encodeUnder(responseSpecs, r.Type, r.ID, r, "response")
}

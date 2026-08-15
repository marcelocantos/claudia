// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// ReleaseRequest returns an agent to the broker.
type ReleaseRequest struct {
	// SessionID names the agent, as returned by SpawnResponse.
	SessionID string `json:"session_id"`
	// Disposition is what to do with it.
	Disposition Disposition `json:"disposition"`
}

// Validate checks the request is well formed on the wire. Whether this broker
// can honour a syntactically valid disposition is a capability question, and is
// answered by the server (DispositionReuse needs the shared pool, 🎯T2.3).
func (r *ReleaseRequest) Validate() error {
	if strings.TrimSpace(r.SessionID) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "session_id",
			Msg: "session_id is required"}
	}
	switch r.Disposition {
	case "":
		return &ProtocolError{Code: CodeMissingField, Field: "disposition",
			Msg: fmt.Sprintf("disposition is required (%q or %q)", DispositionStop, DispositionReuse)}
	case DispositionStop, DispositionReuse:
		return nil
	default:
		return &ProtocolError{Code: CodeUnsupportedValue, Field: "disposition", Value: string(r.Disposition),
			Msg: fmt.Sprintf("disposition %q is not one of %q, %q", r.Disposition, DispositionStop, DispositionReuse)}
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
	// SessionID is the released agent.
	SessionID string `json:"session_id"`
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
}

// EventMessage is one lifecycle event on a tail connection.
type EventMessage struct {
	// Kind is what happened.
	Kind EventKind `json:"kind"`
	// SessionID is the agent it happened to.
	SessionID string `json:"session_id,omitempty"`
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
	// Spawn is set when Type is TypeSpawn.
	Spawn *SpawnRequest
	// Release is set when Type is TypeRelease.
	Release *ReleaseRequest
	// Status is set when Type is TypeStatus.
	Status *StatusRequest
	// Tail is set when Type is TypeTail.
	Tail *TailRequest
}

// Response is a decoded broker → client message. Exactly one of the body
// pointers is non-nil, selected by Type.
type Response struct {
	// ID echoes the request's correlation id.
	ID string
	// Type is the discriminator.
	Type MessageType
	// Spawned is set when Type is TypeSpawned.
	Spawned *SpawnResponse
	// Released is set when Type is TypeReleased.
	Released *ReleaseResponse
	// Status is set when Type is TypeStatusResult.
	Status *StatusResponse
	// Tailing is set when Type is TypeTailing.
	Tailing *TailResponse
	// Event is set when Type is TypeEvent.
	Event *EventMessage
	// Error is set when Type is TypeError.
	Error *ErrorMessage
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

// ParseRequest decodes one newline-delimited-JSON line as a client → broker
// message, validating the body. The returned error is always a *ProtocolError,
// carrying the envelope id when one was readable.
func ParseRequest(line []byte) (*Request, error) {
	env, err := parseEnvelope(line)
	if err != nil {
		return nil, err
	}
	req := &Request{ID: env.ID, Type: env.Type}
	fail := func(code ErrorCode, msg string) (*Request, error) {
		return nil, &ProtocolError{Code: code, Field: "type", Value: string(env.Type), ID: env.ID, Msg: msg}
	}
	switch env.Type {
	case TypeSpawn:
		req.Spawn = &SpawnRequest{}
		if err := decodeBody(env.Body, req.Spawn, "spawn body"); err != nil {
			return nil, withID(err, env.ID)
		}
		if err := req.Spawn.Validate(); err != nil {
			return nil, withID(err, env.ID)
		}
	case TypeRelease:
		req.Release = &ReleaseRequest{}
		if err := decodeBody(env.Body, req.Release, "release body"); err != nil {
			return nil, withID(err, env.ID)
		}
		if err := req.Release.Validate(); err != nil {
			return nil, withID(err, env.ID)
		}
	case TypeStatus:
		req.Status = &StatusRequest{}
		if err := decodeBody(env.Body, req.Status, "status body"); err != nil {
			return nil, withID(err, env.ID)
		}
	case TypeTail:
		req.Tail = &TailRequest{}
		if err := decodeBody(env.Body, req.Tail, "tail body"); err != nil {
			return nil, withID(err, env.ID)
		}
	case TypeSpawned, TypeReleased, TypeStatusResult, TypeTailing, TypeEvent, TypeError:
		return fail(CodeNotARequest, fmt.Sprintf("%q is a broker → client message; the broker does not accept it", env.Type))
	default:
		return fail(CodeUnknownType, fmt.Sprintf("%q is not a request type in wire version %d", env.Type, Version))
	}
	return req, nil
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
	fail := func(code ErrorCode, msg string) (*Response, error) {
		return nil, &ProtocolError{Code: code, Field: "type", Value: string(env.Type), ID: env.ID, Msg: msg}
	}
	switch env.Type {
	case TypeSpawned:
		resp.Spawned = &SpawnResponse{}
		if err := decodeBody(env.Body, resp.Spawned, "spawned body"); err != nil {
			return nil, withID(err, env.ID)
		}
	case TypeReleased:
		resp.Released = &ReleaseResponse{}
		if err := decodeBody(env.Body, resp.Released, "released body"); err != nil {
			return nil, withID(err, env.ID)
		}
	case TypeStatusResult:
		resp.Status = &StatusResponse{}
		if err := decodeBody(env.Body, resp.Status, "status_result body"); err != nil {
			return nil, withID(err, env.ID)
		}
	case TypeTailing:
		resp.Tailing = &TailResponse{}
		if err := decodeBody(env.Body, resp.Tailing, "tailing body"); err != nil {
			return nil, withID(err, env.ID)
		}
	case TypeEvent:
		resp.Event = &EventMessage{}
		if err := decodeBody(env.Body, resp.Event, "event body"); err != nil {
			return nil, withID(err, env.ID)
		}
	case TypeError:
		resp.Error = &ErrorMessage{}
		if err := decodeBody(env.Body, resp.Error, "error body"); err != nil {
			return nil, withID(err, env.ID)
		}
	case TypeSpawn, TypeRelease, TypeStatus, TypeTail:
		return fail(CodeNotAResponse, fmt.Sprintf("%q is a client → broker message; a broker never sends it", env.Type))
	default:
		return fail(CodeUnknownType, fmt.Sprintf("%q is not a response type in wire version %d", env.Type, Version))
	}
	return resp, nil
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

// Encode renders the request canonically. Status and tail carry no body, so
// their canonical form omits it entirely.
func (r *Request) Encode() ([]byte, error) {
	switch r.Type {
	case TypeSpawn:
		return encodeMessage(r.Type, r.ID, r.Spawn)
	case TypeRelease:
		return encodeMessage(r.Type, r.ID, r.Release)
	case TypeStatus, TypeTail:
		return encodeMessage(r.Type, r.ID, nil)
	default:
		return nil, &ProtocolError{Code: CodeUnknownType, Field: "type", Value: string(r.Type),
			Msg: fmt.Sprintf("%q is not a request type in wire version %d", r.Type, Version)}
	}
}

// Encode renders the response canonically.
func (r *Response) Encode() ([]byte, error) {
	switch r.Type {
	case TypeSpawned:
		return encodeMessage(r.Type, r.ID, r.Spawned)
	case TypeReleased:
		return encodeMessage(r.Type, r.ID, r.Released)
	case TypeStatusResult:
		return encodeMessage(r.Type, r.ID, r.Status)
	case TypeTailing:
		// No body, for the same reason the tail request has none.
		return encodeMessage(r.Type, r.ID, nil)
	case TypeEvent:
		return encodeMessage(r.Type, r.ID, r.Event)
	case TypeError:
		return encodeMessage(r.Type, r.ID, r.Error)
	default:
		return nil, &ProtocolError{Code: CodeUnknownType, Field: "type", Value: string(r.Type),
			Msg: fmt.Sprintf("%q is not a response type in wire version %d", r.Type, Version)}
	}
}

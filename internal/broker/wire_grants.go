// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The metaharness half of the wire (🎯T2.9 / 🎯T2.10 / 🎯T2.11). Wire v1's
// spawn/release/status/tail is a process allocator; the messages here are the
// grant protocol a consumer actually speaks: a seat named by the consumer,
// driven with send / interrupt / set_model / migrate, observed through a
// per-grant event stream, and a usage snapshot the daemon owns.
//
// Payloads whose schema belongs to the claudia package — an AgentDef, a
// TaskConfig, a claudia.Event, a []PlanUsage — travel as json.RawMessage.
// This package cannot import claudia (claudia imports it), and mirroring
// those types here would be a second schema that drifts. The claudia package
// owns the codecs (broker_wire.go) and pins their field sets with a census
// test; the golden vectors here pin the envelope and the embedded bytes.

// Request types (client → broker) added by the grant protocol.
const (
	// TypeUsage asks for the daemon's plan-usage snapshot (🎯T2.9).
	TypeUsage MessageType = "usage"
	// TypeResolve asks the daemon to pick a model from predicates.
	TypeResolve MessageType = "resolve"
	// TypeTaskRun runs one Task turn; events stream back on this connection.
	TypeTaskRun MessageType = "task_run"
	// TypeTaskCancel interrupts a running task.
	TypeTaskCancel MessageType = "task_cancel"
	// TypeGrant names a seat: start it, or reclaim it if the daemon already
	// holds it and no live connection owns it (🎯T2.10 / 🎯T2.11).
	TypeGrant MessageType = "grant"
	// TypeSend writes a user turn to a granted seat.
	TypeSend MessageType = "send"
	// TypeInterrupt cancels the seat's current turn.
	TypeInterrupt MessageType = "interrupt"
	// TypeSetModel switches the seat's model within its provider.
	TypeSetModel MessageType = "set_model"
	// TypeMigrate moves the seat to another provider with an inert seed.
	TypeMigrate MessageType = "migrate"
	// TypeAgentInfo reads the seat's live state.
	TypeAgentInfo MessageType = "agent_info"
	// TypeTermSubscribe starts streaming the seat's raw terminal bytes.
	TypeTermSubscribe MessageType = "term_subscribe"
	// TypeResize changes the seat's terminal size.
	TypeResize MessageType = "resize"
	// TypeGrants lists every grant the daemon holds.
	TypeGrants MessageType = "grants"
	// TypeCloseGoal stops the seat's host Goal continuation.
	TypeCloseGoal MessageType = "close_goal"
	// TypeRewind rolls the seat back by whole user turns and relaunches it.
	TypeRewind MessageType = "rewind"
	// TypeGoalVerdict answers a goal_check push with the owner's
	// completeness verdict.
	TypeGoalVerdict MessageType = "goal_verdict"
)

// Response types (broker → client) added by the grant protocol.
const (
	TypeUsageResult     MessageType = "usage_result"
	TypeResolved        MessageType = "resolved"
	TypeTaskStarted     MessageType = "task_started"
	TypeTaskEvent       MessageType = "task_event"
	TypeTaskDone        MessageType = "task_done"
	TypeTaskRaw         MessageType = "task_raw"
	TypeTaskCancelled   MessageType = "task_cancelled"
	TypeGranted         MessageType = "granted"
	TypeAgentEvent      MessageType = "agent_event"
	TypeAgentTerm       MessageType = "agent_term"
	TypeAgentGone       MessageType = "agent_gone"
	TypeSent            MessageType = "sent"
	TypeInterrupted     MessageType = "interrupted"
	TypeModelSet        MessageType = "model_set"
	TypeMigrated        MessageType = "migrated"
	TypeAgentInfoResult MessageType = "agent_info_result"
	TypeTermSubscribed  MessageType = "term_subscribed"
	TypeResized         MessageType = "resized"
	TypeGrantsResult    MessageType = "grants_result"
	TypeGoalClosed      MessageType = "goal_closed"
	TypeRewound         MessageType = "rewound"
	// TypeGoalCheck asks the seat's owner whether its Goal is complete
	// after a terminal turn that carried no GOAL_STATUS line.
	TypeGoalCheck        MessageType = "goal_check"
	TypeGoalVerdictNoted MessageType = "goal_verdict_noted"
)

// Error codes added by the grant protocol.
const (
	// CodeUnknownGrant means the named seat is not one the daemon holds.
	CodeUnknownGrant ErrorCode = "unknown_grant"
	// CodeGrantHeld means the seat exists and a live connection owns it. A
	// second consumer never silently steals a seat (🎯T2.6 no double-ownership).
	CodeGrantHeld ErrorCode = "grant_held"
	// CodeNotAvailable means this broker has no handler for the request: a
	// bare protocol server without the daemon runtime behind it.
	CodeNotAvailable ErrorCode = "not_available"
	// CodeAgentFailed means the daemon accepted the request and the provider
	// operation failed; Message carries the provider's error.
	CodeAgentFailed ErrorCode = "agent_failed"
	// CodeUnknownRun means the task run id is not one the daemon is running.
	CodeUnknownRun ErrorCode = "unknown_run"
)

// Grant dispositions on release. DispositionStop tears the seat down;
// DispositionDetach drops this connection's ownership and leaves the seat
// running for a later reclaim (a consumer upgrading itself, 🎯T2.11).
const DispositionDetach Disposition = "detach"

// Event kinds added by the grant protocol.
const (
	// EventGrant reports a seat granted to a consumer.
	EventGrant EventKind = "grant"
	// EventDetach reports a consumer connection leaving a seat running.
	EventDetach EventKind = "detach"
	// EventUsageUpdate reports a refreshed plan-usage snapshot.
	EventUsageUpdate EventKind = "usage_update"
	// EventTaskStart / EventTaskDone bracket one task run.
	EventTaskStart EventKind = "task_start"
	EventTaskDone  EventKind = "task_done"
	// EventGone reports a seat whose provider process stopped answering.
	EventGone EventKind = "agent_gone"
	// EventResume reports a seat the daemon brought back on boot; Detail is
	// "adopted" (process was still there) or "launched" (resumed from its
	// transcript). EventNudge follows a launched resume once the seat has
	// been told it was restarted. EventResumeFailed carries the reason.
	EventResume       EventKind = "resume"
	EventResumeFailed EventKind = "resume_failed"
	EventNudge        EventKind = "nudge"
)

// UsageRequest asks for the plan-usage snapshot.
type UsageRequest struct {
	// Refresh forces a fetch before answering.
	Refresh bool `json:"refresh,omitempty"`
}

// UsageResponse carries the snapshot.
type UsageResponse struct {
	// FetchedAt is when the daemon last completed a fetch. Zero when it has
	// not fetched yet.
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	// Backends is a JSON array of claudia.PlanUsage.
	Backends json.RawMessage `json:"backends"`
	// Error is the last fetch failure, when the snapshot is stale because
	// of it. Empty on a clean snapshot.
	Error string `json:"error,omitempty"`
}

// ResolveRequest carries claudia.ModelPredicates in wire form.
type ResolveRequest struct {
	Predicates json.RawMessage `json:"predicates"`
}

// Validate checks the predicates are present.
func (r *ResolveRequest) Validate() error {
	if len(r.Predicates) == 0 {
		return &ProtocolError{Code: CodeMissingField, Field: "predicates", Msg: "predicates is required"}
	}
	return nil
}

// ResolveResponse carries a claudia.ModelPick.
type ResolveResponse struct {
	Pick json.RawMessage `json:"pick"`
}

// TaskRunRequest runs one Task turn on the daemon.
type TaskRunRequest struct {
	// Task is a claudia.TaskConfig in wire form.
	Task json.RawMessage `json:"task"`
	// Prompt is the turn.
	Prompt string `json:"prompt"`
	// RawLog asks for the provider's raw output lines as task_raw pushes
	// (claudia.Task.SetRawLog). Omitted, nothing extra crosses the wire.
	RawLog bool `json:"raw_log,omitempty"`
}

// Validate checks the required fields.
func (r *TaskRunRequest) Validate() error {
	if len(r.Task) == 0 {
		return &ProtocolError{Code: CodeMissingField, Field: "task", Msg: "task config is required"}
	}
	return nil
}

// TaskStartedResponse names the run so it can be cancelled.
type TaskStartedResponse struct {
	RunID string `json:"run_id"`
}

// TaskEventMessage is one claudia.TaskEvent on a task_run connection.
type TaskEventMessage struct {
	RunID string          `json:"run_id"`
	Event json.RawMessage `json:"event"`
}

// TaskRawMessage is one raw provider output line on a task_run connection
// that asked for them. Lines arrive in the order the provider wrote them.
type TaskRawMessage struct {
	RunID string `json:"run_id"`
	Line  string `json:"line"`
}

// TaskDoneMessage ends a task_run stream.
type TaskDoneMessage struct {
	RunID string `json:"run_id"`
	// Error is set when the run failed to start or was cut short.
	Error string `json:"error,omitempty"`
}

// TaskCancelRequest interrupts a running task.
type TaskCancelRequest struct {
	RunID string `json:"run_id"`
}

// Validate checks the run id.
func (r *TaskCancelRequest) Validate() error {
	if strings.TrimSpace(r.RunID) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "run_id", Msg: "run_id is required"}
	}
	return nil
}

// TaskCancelledResponse acknowledges a cancel.
type TaskCancelledResponse struct {
	RunID string `json:"run_id"`
}

// GrantRequest names a seat. The daemon starts it when it does not hold it,
// reclaims it when it does and no live connection owns it, and refuses with
// CodeGrantHeld when another connection owns it.
type GrantRequest struct {
	// Name is the grant key, unique on this host. Required.
	Name string `json:"name"`
	// Def is a claudia.AgentDef in wire form plus the Session-only Config
	// fields (see claudia's grantDefWire).
	Def json.RawMessage `json:"def"`
	// Adopt asks the daemon to prefer reattaching a provider process it can
	// find (tmux window, connect-mode serve) over a cold start.
	Adopt bool `json:"adopt,omitempty"`
	// Fallback lets a failed adopt fall through to a cold start.
	Fallback bool `json:"fallback,omitempty"`
}

// Validate checks the required fields.
func (r *GrantRequest) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "name", Msg: "grant name is required"}
	}
	if len(r.Def) == 0 {
		return &ProtocolError{Code: CodeMissingField, Field: "def", Msg: "agent definition is required"}
	}
	return nil
}

// GrantResponse reports the granted seat. Paths are host-local: the consumer
// runs on the same machine as the daemon (the socket is AF_UNIX).
type GrantResponse struct {
	Name          string   `json:"name"`
	SessionID     string   `json:"session_id"`
	Provider      Provider `json:"provider"`
	Model         string   `json:"model,omitempty"`
	WindowID      string   `json:"window_id,omitempty"`
	JSONLPath     string   `json:"jsonl_path,omitempty"`
	TermLogPath   string   `json:"term_log_path,omitempty"`
	AttachCommand string   `json:"attach_command,omitempty"`
	ConnectURL    string   `json:"connect_url,omitempty"`
	ConnectPID    int      `json:"connect_pid,omitempty"`
	// Reclaimed reports that the seat was already running and this
	// connection took ownership rather than starting it.
	Reclaimed bool `json:"reclaimed"`
	// Replayed is how many events missed while unowned were replayed
	// ahead of this response on the stream.
	Replayed int `json:"replayed,omitempty"`
	// Lagged reports that the replay ring overflowed while the seat was
	// unowned: some events were lost and the consumer must not treat the
	// replayed history as complete.
	Lagged bool `json:"lagged,omitempty"`
	// TurnCaps is what the seat's provider can do with a busy turn
	// (🎯T72.3). Absent from a daemon that predates it.
	TurnCaps *TurnCaps `json:"turn_caps,omitempty"`
}

// AgentEventMessage is one claudia.Event on a grant connection.
type AgentEventMessage struct {
	Name  string          `json:"name"`
	Event json.RawMessage `json:"event"`
}

// AgentTermMessage is a chunk of raw terminal bytes on a grant connection.
type AgentTermMessage struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// AgentGoneMessage reports that the seat's provider process is no longer
// reachable. The grant stays in the daemon's table until released.
type AgentGoneMessage struct {
	Name   string `json:"name"`
	Reason string `json:"reason,omitempty"`
}

// NamedRequest is the body of every request that addresses one grant by
// name and carries nothing else.
type NamedRequest struct {
	Name string `json:"name"`
}

// Validate checks the name.
func (r *NamedRequest) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "name", Msg: "grant name is required"}
	}
	return nil
}

// NamedResponse acknowledges a NamedRequest.
type NamedResponse struct {
	Name string `json:"name"`
}

// SendMode is the host intent for one user-text delivery (🎯T72.3). It is
// the wire spelling of claudia.DeliveryMode, converted at the daemon
// boundary the way Provider is; this package cannot import claudia.
type SendMode string

// Send modes. An absent mode normalises to SendModeSubmit on receipt, so a
// pre-🎯T72 client keeps today's behaviour without knowing the field exists.
const (
	// SendModeSubmit starts a turn when the seat is idle (today's send).
	SendModeSubmit SendMode = "submit"
	// SendModeSteer folds the text into the running turn.
	SendModeSteer SendMode = "steer"
	// SendModeInterrupt cancels the open turn, then submits the text.
	SendModeInterrupt SendMode = "interrupt"
	// SendModeQueue asks the daemon to acknowledge a host-side enqueue;
	// nothing is written to the seat.
	SendModeQueue SendMode = "queue"
)

// SendModes lists every mode in wire order. TestEverySendModeHasAVector
// walks it.
func SendModes() []SendMode {
	return []SendMode{SendModeSubmit, SendModeSteer, SendModeInterrupt, SendModeQueue}
}

// SendRequest writes a user turn.
type SendRequest struct {
	Name string `json:"name"`
	Text string `json:"text"`
	// Mode is the delivery intent. Empty normalises to SendModeSubmit.
	Mode SendMode `json:"mode,omitempty"`
}

// Validate checks the name and normalises the mode.
func (r *SendRequest) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "name", Msg: "grant name is required"}
	}
	switch r.Mode {
	case "":
		r.Mode = SendModeSubmit
	case SendModeSubmit, SendModeSteer, SendModeInterrupt, SendModeQueue:
	default:
		return &ProtocolError{Code: CodeUnsupportedValue, Field: "mode", Value: string(r.Mode),
			Msg: fmt.Sprintf("mode %q is not one of %q, %q, %q, %q", r.Mode,
				SendModeSubmit, SendModeSteer, SendModeInterrupt, SendModeQueue)}
	}
	return nil
}

// SentResponse acknowledges a send and reports what the daemon actually
// did with it. Mode, Mechanism and PhaseBefore are additive (🎯T72.3): a
// daemon that predates them answers with the name alone.
type SentResponse struct {
	Name string `json:"name"`
	// Mode is the delivery mode the daemon acted on, after normalisation.
	Mode SendMode `json:"mode,omitempty"`
	// Mechanism records what ran on the provider (claudia
	// DeliveryOutcome.Mechanism), for logs and UI; it is not a second
	// source of truth for the seat's state.
	Mechanism string `json:"mechanism,omitempty"`
	// PhaseBefore is the seat's turn phase observed before delivery
	// (claudia.TurnPhase: "idle" or "in_turn").
	PhaseBefore string `json:"phase_before,omitempty"`
	// SupersededTurnID is set when a steer superseded an in-flight prompt
	// RPC (ACP); empty for mechanisms that fold text into the same turn.
	SupersededTurnID string `json:"superseded_turn_id,omitempty"`
}

// TurnCaps is the wire form of claudia.TurnCaps: what the seat's provider
// can do with a busy turn, for UI hinting. Field for field with the
// claudia type; the claudia package pins the mirror in its census.
type TurnCaps struct {
	CanInterrupt bool `json:"can_interrupt"`
	CanSteer     bool `json:"can_steer"`
	// SteerPolicy is claudia.SteerPolicy: breakpoint, finish_slice,
	// queue_until_idle or none.
	SteerPolicy string `json:"steer_policy,omitempty"`
	// BusyOnSecondSubmit is what the provider does with a second submit
	// while in a turn: reject, supersede or queue.
	BusyOnSecondSubmit string `json:"busy_on_second_submit,omitempty"`
}

// SetModelRequest switches the model within the provider.
type SetModelRequest struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

// Validate checks the fields.
func (r *SetModelRequest) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "name", Msg: "grant name is required"}
	}
	if strings.TrimSpace(r.Model) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "model", Msg: "model is required"}
	}
	return nil
}

// MigrateRequest moves the seat to another provider (claudia.MigrateArgs).
type MigrateRequest struct {
	Name     string   `json:"name"`
	Provider Provider `json:"provider"`
	Model    string   `json:"model,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Force    bool     `json:"force,omitempty"`
}

// Validate checks the fields.
func (r *MigrateRequest) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "name", Msg: "grant name is required"}
	}
	if strings.TrimSpace(string(r.Provider)) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "provider", Msg: "destination provider is required"}
	}
	return nil
}

// MigrateResponse reports the destination the seat now runs on.
// GoalCheckMessage asks the owner of a seat to judge its Goal against the
// turn that just ended (claudia.Config.GoalCompleteCheck). The owner answers
// with goal_verdict carrying the same CheckID.
type GoalCheckMessage struct {
	Name     string `json:"name"`
	CheckID  string `json:"check_id"`
	Goal     string `json:"goal"`
	TurnText string `json:"turn_text"`
}

// GoalVerdictRequest is the owner's answer to a goal_check. Answered is
// false when the owner has no check installed, which the daemon treats as
// not complete.
type GoalVerdictRequest struct {
	Name     string `json:"name"`
	CheckID  string `json:"check_id"`
	Complete bool   `json:"complete,omitempty"`
	Answered bool   `json:"answered,omitempty"`
}

// Validate checks the fields.
func (r *GoalVerdictRequest) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "name", Msg: "grant name is required"}
	}
	if strings.TrimSpace(r.CheckID) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "check_id", Msg: "check id is required"}
	}
	return nil
}

// RewindRequest rolls a seat back by Turns user turns.
type RewindRequest struct {
	Name  string `json:"name"`
	Turns int    `json:"turns"`
}

// Validate checks the fields.
func (r *RewindRequest) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "name", Msg: "grant name is required"}
	}
	if r.Turns < 1 {
		return &ProtocolError{Code: CodeUnsupportedValue, Field: "turns", Value: strconv.Itoa(r.Turns), Msg: "turns must be at least 1"}
	}
	return nil
}

// RewindResponse names the relaunched seat and what the rewind removed.
type RewindResponse struct {
	Name          string   `json:"name"`
	SessionID     string   `json:"session_id"`
	Provider      Provider `json:"provider"`
	Model         string   `json:"model,omitempty"`
	WindowID      string   `json:"window_id,omitempty"`
	JSONLPath     string   `json:"jsonl_path,omitempty"`
	TermLogPath   string   `json:"term_log_path,omitempty"`
	AttachCommand string   `json:"attach_command,omitempty"`
	TurnsRemoved  int      `json:"turns_removed"`
	LinesRemoved  int      `json:"lines_removed"`
	BytesRemoved  int64    `json:"bytes_removed"`
	BackupPath    string   `json:"backup_path,omitempty"`
}

type MigrateResponse struct {
	Name          string   `json:"name"`
	SessionID     string   `json:"session_id"`
	Provider      Provider `json:"provider"`
	Model         string   `json:"model,omitempty"`
	WindowID      string   `json:"window_id,omitempty"`
	JSONLPath     string   `json:"jsonl_path,omitempty"`
	TermLogPath   string   `json:"term_log_path,omitempty"`
	AttachCommand string   `json:"attach_command,omitempty"`
}

// AgentInfoResponse is the seat's live state.
type AgentInfoResponse struct {
	Name           string   `json:"name"`
	SessionID      string   `json:"session_id"`
	Provider       Provider `json:"provider"`
	Model          string   `json:"model,omitempty"`
	Alive          bool     `json:"alive"`
	PromptInFlight bool     `json:"prompt_in_flight"`
	// Usage is a claudia.Usage.
	Usage         json.RawMessage `json:"usage,omitempty"`
	WindowID      string          `json:"window_id,omitempty"`
	JSONLPath     string          `json:"jsonl_path,omitempty"`
	TermLogPath   string          `json:"term_log_path,omitempty"`
	AttachCommand string          `json:"attach_command,omitempty"`
	ConnectURL    string          `json:"connect_url,omitempty"`
	ConnectPID    int             `json:"connect_pid,omitempty"`
	// TurnCaps is what the seat's provider can do with a busy turn
	// (🎯T72.3). Absent from a daemon that predates it.
	TurnCaps *TurnCaps `json:"turn_caps,omitempty"`
}

// TermSubscribedResponse carries the retained terminal history.
type TermSubscribedResponse struct {
	Name    string `json:"name"`
	History []byte `json:"history,omitempty"`
}

// ResizeRequest changes the seat's terminal size.
type ResizeRequest struct {
	Name string `json:"name"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// Validate checks the fields.
func (r *ResizeRequest) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "name", Msg: "grant name is required"}
	}
	if r.Cols == 0 || r.Rows == 0 {
		return &ProtocolError{Code: CodeUnsupportedValue, Field: "cols",
			Value: fmt.Sprintf("%dx%d", r.Cols, r.Rows), Msg: "cols and rows must be positive"}
	}
	return nil
}

// GrantsRequest lists grants. No fields.
type GrantsRequest struct{}

// GrantStatus is one grant in a grants_result.
type GrantStatus struct {
	Name      string   `json:"name"`
	Provider  Provider `json:"provider"`
	Model     string   `json:"model,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
	WorkDir   string   `json:"workdir,omitempty"`
	Purpose   string   `json:"purpose,omitempty"`
	Parent    string   `json:"parent,omitempty"`
	// Owned reports that a live connection holds the seat.
	Owned bool `json:"owned"`
	// Alive reports that the provider process is reachable.
	Alive bool `json:"alive"`
	// Pending is how many events sit in the replay ring for an unowned seat.
	Pending int `json:"pending,omitempty"`
	// TurnCaps is what the seat's provider can do with a busy turn
	// (🎯T72.3). Absent for a seat with no live process.
	TurnCaps *TurnCaps `json:"turn_caps,omitempty"`
}

// GrantsResponse lists every grant.
type GrantsResponse struct {
	Grants []GrantStatus `json:"grants"`
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import "encoding/json"

const (
	TypeFleet       MessageType = "fleet"
	TypeFleetResult MessageType = "fleet_result"
)

// Fleet operations (🎯T247).
const (
	FleetOpOpen     = "open"
	FleetOpStart    = "start"
	FleetOpSend     = "send"
	FleetOpStop     = "stop"
	FleetOpStopTree = "stop_tree"
	FleetOpShutdown = "shutdown"
	FleetOpStatus   = "status"
	FleetOpRead     = "read"
	// 🎯T248: scheduled messages, budgets and checks.
	FleetOpCancel        = "cancel"
	FleetOpBudget        = "budget"
	FleetOpRegisterCheck = "register_check"
	FleetOpRunCheck      = "run_check"
	// 🎯T250: expediting a message by its ID (TimerID) and Mode.
	FleetOpExpedite = "expedite"
)

// FleetRequest is one owner operation on a fleet.
type FleetRequest struct {
	Op    string `json:"op"`
	Fleet string `json:"fleet"`

	// Open.
	MaxAgents   int `json:"max_agents,omitempty"`
	MaxMessages int `json:"max_messages,omitempty"`

	// Start, Stop and StopTree. For StopTree, Name is the root.
	Name             string `json:"name,omitempty"`
	Instructions     string `json:"instructions,omitempty"`
	InstructionsFile string `json:"instructions_file,omitempty"`
	WorkDir          string `json:"workdir,omitempty"`
	Quality          string `json:"quality,omitempty"`
	Purpose          string `json:"purpose,omitempty"`
	Model            string `json:"model,omitempty"`

	// Start: give the agent authority over everything, and choose the tools it
	// may call (🎯T249). A nil set means every tool.
	Authority string   `json:"authority,omitempty"`
	Tools     []string `json:"tools,omitempty"`
	// HasTools says Tools was chosen, so that an empty choice means no tools
	// rather than every tool.
	HasTools bool `json:"has_tools,omitempty"`

	// Start and Budget: a limit in up to three dimensions, when HasBudget.
	HasBudget     bool    `json:"has_budget,omitempty"`
	BudgetTokens  int64   `json:"budget_tokens,omitempty"`
	BudgetTurns   int     `json:"budget_turns,omitempty"`
	BudgetSeconds float64 `json:"budget_seconds,omitempty"`

	// Send. From names the source of a notice; a request always comes from
	// the owner.
	From         string  `json:"from,omitempty"`
	To           string  `json:"to,omitempty"`
	Kind         string  `json:"kind,omitempty"`
	Text         string  `json:"text,omitempty"`
	AfterSeconds float64 `json:"after_seconds,omitempty"`
	// At is an RFC 3339 time; EverySeconds repeats.
	At           string  `json:"at,omitempty"`
	EverySeconds float64 `json:"every_seconds,omitempty"`
	// AcceptWithinSeconds and AnswerWithinSeconds are the owner's
	// expectations; Mode expedites (🎯T250).
	AcceptWithinSeconds float64 `json:"accept_within_seconds,omitempty"`
	AnswerWithinSeconds float64 `json:"answer_within_seconds,omitempty"`
	Mode                string  `json:"mode,omitempty"`

	// Cancel, and Expedite's message ID.
	TimerID string `json:"timer_id,omitempty"`

	// RegisterCheck and RunCheck. Name is the check's name.
	Repo           string  `json:"repo,omitempty"`
	Script         string  `json:"script,omitempty"`
	AgentsMayRun   bool    `json:"agents_may_run,omitempty"`
	TimeoutSeconds float64 `json:"timeout_seconds,omitempty"`
	Commit         string  `json:"commit,omitempty"`

	// Read returns events after this sequence number, waiting up to WaitMillis
	// for some to exist.
	After      uint64 `json:"after,omitempty"`
	WaitMillis int64  `json:"wait_millis,omitempty"`
}

func (r *FleetRequest) Validate() error {
	switch r.Op {
	case FleetOpOpen, FleetOpStart, FleetOpSend, FleetOpStop, FleetOpStopTree, FleetOpShutdown, FleetOpStatus, FleetOpRead,
		FleetOpCancel, FleetOpBudget, FleetOpRegisterCheck, FleetOpRunCheck, FleetOpExpedite:
	default:
		return &ProtocolError{Code: CodeUnsupportedValue, Field: "op", Value: r.Op, Msg: "unknown fleet operation"}
	}
	if r.Fleet == "" {
		return &ProtocolError{Code: CodeMissingField, Field: "fleet", Msg: "a fleet operation names its fleet"}
	}
	return nil
}

// FleetResponse carries an operation's result as the library's own JSON.
type FleetResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
}

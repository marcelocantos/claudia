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

	// Send. From names the source of a notice; a request always comes from
	// the owner.
	From         string  `json:"from,omitempty"`
	To           string  `json:"to,omitempty"`
	Kind         string  `json:"kind,omitempty"`
	Text         string  `json:"text,omitempty"`
	AfterSeconds float64 `json:"after_seconds,omitempty"`

	// Read returns events after this sequence number, waiting up to WaitMillis
	// for some to exist.
	After      uint64 `json:"after,omitempty"`
	WaitMillis int64  `json:"wait_millis,omitempty"`
}

func (r *FleetRequest) Validate() error {
	switch r.Op {
	case FleetOpOpen, FleetOpStart, FleetOpSend, FleetOpStop, FleetOpStopTree, FleetOpShutdown, FleetOpStatus, FleetOpRead:
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

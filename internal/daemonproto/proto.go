// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package daemonproto defines the JSON-RPC 2.0 wire protocol used
// between the claudia library client and the claudiad daemon.
//
// The daemon is an optional backing service. When present, the library
// opportunistically reports each claude session it spawns so the daemon
// can track /clear rollovers and expose session chains. When absent,
// the library falls back to direct spawning and the RPC surface is
// unused.
//
// The protocol is JSON-RPC 2.0 over a single WebSocket connection over
// a Unix domain socket. Every envelope carries [Version] so future
// slices can add methods without a flag day.
package daemonproto

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Version is the wire protocol version.
const Version = 1

// Method names.
const (
	MethodSessionRegister                = "session.register"
	MethodSessionLookupChain             = "session.lookup_chain"
	MethodSessionSubscribeChainUpdates   = "session.subscribe_chain_updates"
	MethodSessionUnsubscribeChainUpdates = "session.unsubscribe_chain_updates"
	NotificationChainUpdate              = "chain.update"
)

// Confidence values for chain attributions.
const (
	ConfidenceDeterministic = "deterministic"
	ConfidenceAmbiguous     = "ambiguous"
)

// JSON-RPC 2.0 error codes.
const (
	ErrorCodeParse          = -32700
	ErrorCodeInvalidRequest = -32600
	ErrorCodeMethodNotFound = -32601
	ErrorCodeInvalidParams  = -32602
	ErrorCodeInternal       = -32603
)

// Request is a JSON-RPC 2.0 request envelope.
type Request struct {
	JSONRPC         string          `json:"jsonrpc"`
	ClaudiadVersion int             `json:"claudiad_version"`
	ID              json.RawMessage `json:"id,omitempty"`
	Method          string          `json:"method"`
	Params          json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response envelope. Also reused for
// server-pushed notifications by setting Method and Params (no ID).
type Response struct {
	JSONRPC         string          `json:"jsonrpc"`
	ClaudiadVersion int             `json:"claudiad_version"`
	ID              json.RawMessage `json:"id,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	Error           *Error          `json:"error,omitempty"`
	// Method and Params are set for server-pushed notifications.
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements the error interface.
func (e *Error) Error() string { return e.Message }

// SessionRegisterParams is the params for [MethodSessionRegister].
type SessionRegisterParams struct {
	Cwd       string `json:"cwd"`
	SessionID string `json:"session_id"`
	PID       int    `json:"pid"`
}

// SessionRegisterResult is the result for [MethodSessionRegister].
type SessionRegisterResult struct {
	OK bool `json:"ok"`
}

// SessionLookupChainParams is the params for [MethodSessionLookupChain].
type SessionLookupChainParams struct {
	SessionID string `json:"session_id"`
}

// SessionLookupChainResult is the result for [MethodSessionLookupChain].
// The chain is ordered from oldest to newest (sid0 first); for a
// never-rolled-over session the chain contains a single element.
type SessionLookupChainResult struct {
	Chain      []string `json:"chain"`
	Confidence string   `json:"confidence"`
}

// SessionSubscribeResult is the result for [MethodSessionSubscribeChainUpdates].
type SessionSubscribeResult struct {
	SubscriptionID string `json:"subscription_id"`
}

// SessionUnsubscribeParams is the params for [MethodSessionUnsubscribeChainUpdates].
type SessionUnsubscribeParams struct {
	SubscriptionID string `json:"subscription_id"`
}

// SessionUnsubscribeResult is the result for [MethodSessionUnsubscribeChainUpdates].
type SessionUnsubscribeResult struct {
	OK bool `json:"ok"`
}

// ChainUpdateParams is the params for a [NotificationChainUpdate]
// server-pushed notification.
type ChainUpdateParams struct {
	SubscriptionID string   `json:"subscription_id"`
	Chain          []string `json:"chain"`
	Confidence     string   `json:"confidence"`
}

// SocketPath returns the canonical Unix domain socket path at
// $XDG_STATE_HOME/claudia/claudiad.sock (defaulting to
// ~/.local/state/claudia/claudiad.sock).
func SocketPath() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return filepath.Join(stateHome, "claudia", "claudiad.sock")
}

// SocketDir returns the directory containing the socket.
func SocketDir() string {
	return filepath.Dir(SocketPath())
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"

	"github.com/marcelocantos/claudia/internal/broker"
)

// AgentFailed reports whether err is a broker-relayed provider failure
// (protocol code agent_failed) and, when it is, returns the provider's own
// message from the typed ProtocolError.Msg field.
//
// The check is on the typed *broker.ProtocolError (Code + Msg). It never
// inspects Error() text, so a string that merely quotes a refusal is not a
// match. Consumers that used to peel the "broker protocol: agent_failed: "
// prefix off Error() should call this instead (jevons 🎯T767).
func AgentFailed(err error) (msg string, ok bool) {
	var pe *broker.ProtocolError
	if !errors.As(err, &pe) || pe == nil {
		return "", false
	}
	if pe.Code != broker.CodeAgentFailed {
		return "", false
	}
	return pe.Msg, true
}

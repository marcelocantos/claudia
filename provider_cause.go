// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import "strings"

// ProviderCause is the stable code of a provider failure.
// Controls switch on it. The rendered text is not a cause.
type ProviderCause string

const (
	// CauseTurnInFlight is a prompt or turn the provider refused because one
	// is already running. The text differs by provider; the code does not.
	CauseTurnInFlight ProviderCause = "turn_in_flight"
	// CauseProcessNotRunning is a send refused because the provider process
	// is gone. The historical text is "claude process not running".
	CauseProcessNotRunning ProviderCause = "process_not_running"
	// CauseBusy is a task that refused a second run. It is not a turn.
	CauseBusy ProviderCause = "busy"
	// CauseSteerUnsupported is a steer the seat cannot perform.
	CauseSteerUnsupported ProviderCause = "steer_unsupported"
	// CauseTurnIdle is a steer with no open turn.
	CauseTurnIdle ProviderCause = "turn_idle"
	// CauseAuth is a provider authentication or authorization refusal.
	CauseAuth ProviderCause = "auth"
	// CauseRateLimit is a throttle or quota refusal. A billing refusal is
	// CauseSpendWall, not this.
	CauseRateLimit ProviderCause = "rate_limit"
	// CauseSpendWall is an account spend or billing refusal.
	CauseSpendWall ProviderCause = "spend_wall"
	// CauseBackendUnavailable is a provider outage or transport failure.
	CauseBackendUnavailable ProviderCause = "backend_unavailable"
	// CauseClientBug is a local request the provider rejected as invalid.
	CauseClientBug ProviderCause = "client_bug"
)

// CauseFromProviderCode maps a provider's own error code onto a cause.
// The code is the field the provider sent. The sentence beside it is not
// consulted. An unrecognised code has no cause.
func CauseFromProviderCode(code string) ProviderCause {
	switch code {
	case "rate_limit", "rate_limit_error":
		return CauseRateLimit
	case "billing_error":
		return CauseSpendWall
	case "authentication_failed", "authentication_error":
		return CauseAuth
	case "model_not_found":
		return CauseClientBug
	case "overloaded_error", "internal_error":
		return CauseBackendUnavailable
	default:
		return ""
	}
}

// CauseFromConnectError maps a provider turn-refusal errorMessage onto a
// cause. The provider's own code sits in a fixed position; the prose after
// it is not consulted.
//
//	Connect error <code>: …                 Cursor / Connect RPC
//	Anthropic stream error (<code>): …      Anthropic stream
//
// Without the Connect mapping, Cursor's quota refusal (resource_exhausted)
// reached Jevons as an untyped error, so no provider wall was raised and
// seats kept submitting into it (2026-10-27). Without the Anthropic mapping,
// `Anthropic stream error (rate_limit_error): Rate limited` reached Jevons
// as client_bug / untyped (2026-10-31).
func CauseFromConnectError(errorMessage string) ProviderCause {
	if code, ok := anthropicStreamErrorCode(errorMessage); ok {
		return CauseFromProviderCode(code)
	}
	if code, ok := connectErrorCode(errorMessage); ok {
		switch code {
		case "resource_exhausted":
			return CauseRateLimit
		case "unauthenticated":
			return CauseAuth
		case "unavailable":
			return CauseBackendUnavailable
		default:
			return ""
		}
	}
	return ""
}

func connectErrorCode(errorMessage string) (string, bool) {
	const prefix = "Connect error "
	i := strings.Index(errorMessage, prefix)
	if i < 0 {
		return "", false
	}
	code := errorMessage[i+len(prefix):]
	if j := strings.IndexAny(code, ": "); j >= 0 {
		code = code[:j]
	}
	return code, true
}

func anthropicStreamErrorCode(errorMessage string) (string, bool) {
	const prefix = "Anthropic stream error ("
	if !strings.HasPrefix(errorMessage, prefix) {
		return "", false
	}
	rest := errorMessage[len(prefix):]
	j := strings.Index(rest, "):")
	if j <= 0 {
		return "", false
	}
	code := rest[:j]
	// A provider code is one token, not prose or a parenthesized sentence.
	for i := 0; i < len(code); i++ {
		c := code[i]
		if (c < 'a' || c > 'z') && c != '_' && (i == 0 || c < '0' || c > '9') {
			return "", false
		}
	}
	if code[0] == '_' {
		return "", false
	}
	return code, true
}

// causedError is a provider failure whose cause is a code.
// Text is the historical sentence. It is not consulted by a control.
type causedError struct {
	cause ProviderCause
	text  string
	next  error
}

func (e *causedError) Error() string {
	if e == nil {
		return ""
	}
	return e.text
}

func (e *causedError) ProviderCause() string {
	if e == nil {
		return ""
	}
	return string(e.cause)
}

func (e *causedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.next
}

func caused(cause ProviderCause, text string, next error) error {
	return &causedError{cause: cause, text: text, next: next}
}

// ErrProcessNotRunning is the send refusal for a provider process that is
// already gone. Error text stays "claude process not running".
var ErrProcessNotRunning error = caused(CauseProcessNotRunning, "claude process not running", nil)

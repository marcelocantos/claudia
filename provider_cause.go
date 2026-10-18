// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

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
	case "rate_limit":
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

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package wallclockguard

import (
	"context"
	"testing"
	"time"
)

// TestTimeoutGrace is how far ahead of the test binary's own -timeout
// UntilTestTimeout ends: long enough for the test to fail by name, with its
// own message, before the harness panics with a goroutine dump.
const TestTimeoutGrace = time.Second

// UntilTestTimeout is the backstop a hermetic test waits under when the
// event it needs might never come: a context that ends when the test does,
// or just before `go test -timeout` would kill the binary, whichever is
// first. Without a -timeout it ends only with the test.
//
// It replaces the per-test failsafes — time.After(2*time.Second) racing a
// channel, a 30s context handed to a fake — that a loaded host could expire
// before a correct build answered (🎯T97). Those decided the verdict by the
// host's speed. This clock is `go test -timeout`, the one a hermetic test is
// allowed: any run it ends is a run the harness was about to fail anyway,
// and TestTimeoutGrace only buys a named failure instead of a panic. It
// lives outside the scanned test files, so it needs no exemption marker.
func UntilTestTimeout(t *testing.T) context.Context {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		return t.Context()
	}
	if time.Until(deadline) > 2*TestTimeoutGrace {
		deadline = deadline.Add(-TestTimeoutGrace)
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	t.Cleanup(cancel)
	return ctx
}

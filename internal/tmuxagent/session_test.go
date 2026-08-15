// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import "testing"

func TestSessionWindowNameTruncates(t *testing.T) {
	if got := SessionWindowName("abcdefgh-xxxx"); got != "claudia-abcdefgh" {
		t.Fatalf("got %q", got)
	}
	if got := SessionWindowName("short"); got != "claudia-short" {
		t.Fatalf("got %q", got)
	}
}

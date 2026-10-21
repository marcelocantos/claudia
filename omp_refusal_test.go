// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"strings"
	"testing"
)

func TestOmpSidecarRefusalWantsPathHints(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New("spawn bash ENOENT"), true},
		{errors.New("command not found: bun"), true},
		{errors.New("existing conversation required for this seat and session; refusing to mint a replacement session"), false},
		{errors.New("existing conversation required; store /tmp/s1.jsonl has no message records"), false},
	}
	for _, tc := range cases {
		if got := ompSidecarRefusalWantsPathHints(tc.err); got != tc.want {
			t.Errorf("ompSidecarRefusalWantsPathHints(%q) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestExplainOMPSidecarRefusalResumeOmitsPathHints(t *testing.T) {
	base := errors.New("omp: sidecar said \"error\", want ready: existing conversation required; store /tmp/omp-seats/seat/s1.jsonl has no message records")
	wrapped := explainOMPSidecarRefusal(base, ProviderCursor, "/tmp/omp.sock")
	msg := wrapped.Error()
	if strings.Contains(msg, "~/.bun/bin") || strings.Contains(msg, "supervisorctl restart claudia") {
		t.Fatalf("resume refusal should not carry PATH appendix:\n%s", msg)
	}
	if !strings.Contains(msg, "existing conversation required") {
		t.Fatalf("missing sidecar words:\n%s", msg)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bannedInPolicy matches wall-clock and blocking-timer calls that must never
// appear in broker policy code. Policy reads time through Clock and takes
// backpressure (429s, turn completions) as injected events, so every timing
// decision stays deterministically testable. See docs/broker-oracles.md (T2.0).
var bannedInPolicy = regexp.MustCompile(`\btime\.(Now|After|Sleep|Tick|NewTimer|NewTicker)\(`)

// exemptFiles may legitimately touch the wall clock: the Clock seam itself is
// the one place time.Now/time.After are allowed.
var exemptFiles = map[string]bool{
	"clock.go": true,
}

// TestNoDirectWallclockInPolicy enforces the Clock seam structurally. It scans
// every non-test .go file in this package (bar the exempt seam) and fails if
// any reads the wall clock directly. It passes today because no policy code
// exists yet; it exists to bite the moment AIMD/reaping/preemption land and
// someone reaches for time.Now instead of the injected Clock.
func TestNoDirectWallclockInPolicy(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || exemptFiles[name] {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		if loc := bannedInPolicy.FindIndex(src); loc != nil {
			line := 1 + strings.Count(string(src[:loc[0]]), "\n")
			t.Errorf("%s:%d: policy code must read time through Clock, not %q (T2.0 oracle seam)",
				name, line, string(src[loc[0]:loc[1]]))
		}
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package brokertest provides test doubles for the claudia broker's oracle
// harness. Its centrepiece is a behavioural fake `claude` binary that emits
// canned JSONL turns, canned usage blocks, an injectable 429 rate-limit error,
// and the real readiness prompt-box pattern on demand — letting the broker's
// policy loops (AIMD, cost tracking, reaping, preemption) run headless under
// `go test -race` with zero API credit and no real claude binary. See
// docs/broker-oracles.md (🎯T2.0, 🎯T2.8).
//
// The wire shapes themselves live in the leaf package fakewire, shared with the
// fake binary so the double and the code that reads it cannot drift apart.
package brokertest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker/brokertest/fakewire"
)

// The scenario vocabulary is re-exported so tests need only import brokertest.
type (
	// Scenario describes one fake claude run.
	Scenario = fakewire.Scenario
	// UsageBlock is one canned token-usage record.
	UsageBlock = fakewire.UsageBlock
	// Readiness selects which terminal frame the fake writes to stderr.
	Readiness = fakewire.Readiness
)

// Readiness frames.
const (
	ReadyNone      = fakewire.ReadyNone
	ReadyPromptBox = fakewire.ReadyPromptBox
	ReadySplash    = fakewire.ReadySplash
)

// PromptBoxFrame and SplashFrame are the captured-pane frames the fake writes,
// exposed so a test can assert directly against tmuxagent's readiness matcher.
var (
	PromptBoxFrame = fakewire.PromptBoxFrame
	SplashFrame    = fakewire.SplashFrame
)

// The fake binary is compiled once per test binary and shared across tests.
var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

const fakePkg = "github.com/marcelocantos/claudia/internal/broker/brokertest/cmd/fakeclaude"

// FakeClaude is a compiled fake `claude` binary plus a per-test scenario
// directory.
type FakeClaude struct {
	// Bin is the absolute path to a binary named "claude".
	Bin string
	dir string
}

// Build compiles the fake claude binary (once per test binary) and returns a
// FakeClaude. Prepend Dir() to PATH so code under test resolves "claude" to the
// fake.
func Build(t *testing.T) *FakeClaude {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fake-claude-bin")
		if err != nil {
			buildErr = err
			return
		}
		out := filepath.Join(dir, "claude")
		cmd := exec.Command("go", "build", "-o", out, fakePkg)
		if b, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build fake claude: %v\n%s", err, b)
			return
		}
		binPath = out
	})
	if buildErr != nil {
		t.Fatalf("brokertest.Build: %v", buildErr)
	}
	return &FakeClaude{Bin: binPath, dir: t.TempDir()}
}

// Dir returns the directory containing the fake binary, for prepending to PATH.
func (f *FakeClaude) Dir() string { return filepath.Dir(f.Bin) }

// Command returns an *exec.Cmd that runs the fake with the given scenario. The
// scenario is serialised to a fresh temp file referenced via the
// FAKE_CLAUDE_SCENARIO env var, so successive calls do not clobber each other.
func (f *FakeClaude) Command(t *testing.T, s Scenario) *exec.Cmd {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	fh, err := os.CreateTemp(f.dir, "scenario-*.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(f.Bin)
	cmd.Env = append(os.Environ(), "FAKE_CLAUDE_SCENARIO="+fh.Name())
	return cmd
}

// ParseUsage extracts the canned usage blocks from a captured stdout stream.
// It reads only assistant turns, so the closing result line is not
// double-counted.
func ParseUsage(t *testing.T, stdout []byte) []UsageBlock {
	t.Helper()
	var out []UsageBlock
	for line := range strings.SplitSeq(string(stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var turn struct {
			Type    string `json:"type"`
			Message struct {
				Usage UsageBlock `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &turn); err != nil {
			continue
		}
		if turn.Type == fakewire.AssistantTurnType {
			out = append(out, turn.Message.Usage)
		}
	}
	return out
}

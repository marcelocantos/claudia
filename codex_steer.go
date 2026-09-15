// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// codexAppServerTurnSteerParams is turn/steer: fold input into the turn
// named by expectedTurnId. The server refuses when that turn is not the
// active one, which is the precondition claudia wants — a steer that
// lands on a turn the caller did not see is a stray prompt.
type codexAppServerTurnSteerParams struct {
	ThreadID       string                    `json:"threadId"`
	ExpectedTurnID string                    `json:"expectedTurnId"`
	Input          []codexAppServerUserInput `json:"input"`
}

func codexAppServerTurnSteer(id int, threadID, turnID, text string) codexAppServerRequest {
	return codexAppServerRequest{
		Method: "turn/steer",
		ID:     intPtr(id),
		Params: codexAppServerTurnSteerParams{
			ThreadID:       threadID,
			ExpectedTurnID: turnID,
			Input:          []codexAppServerUserInput{{Type: "text", Text: text}},
		},
	}
}

// Steer satisfies the turnSteerer seam for the Codex app-server client.
// ctx is accepted for seam symmetry; the client's request path has no
// context plumbing and settles on the response like every other RPC.
func (c *codexAppServerClient) Steer(_ context.Context, text string) (string, error) {
	c.mu.Lock()
	closed, threadID, turnID, inFlight := c.closed, c.threadID, c.turnID, c.inFlight
	c.mu.Unlock()
	if closed {
		return MechanismCodexTurnSteer, fmt.Errorf("codex app-server: client closed")
	}
	if threadID == "" {
		return MechanismCodexTurnSteer, fmt.Errorf("codex app-server: no thread")
	}
	if !inFlight || turnID == "" {
		return MechanismCodexTurnSteer, ErrTurnIdle
	}
	_, err := c.request(codexAppServerTurnSteer(0, threadID, turnID, text))
	return MechanismCodexTurnSteer, err
}

// SupersededTurnID is always empty: turn/steer folds input into the same
// turn rather than pushing a second prompt over the first.
func (c *codexAppServerClient) SupersededTurnID() string { return "" }

// codexSteerSchemaFile is the JSON-schema file `codex app-server
// generate-json-schema` emits for the turn/steer request on a CLI that
// has the method. Its absence is the honest "no turn/steer here".
const codexSteerSchemaFile = "v2/TurnSteerParams.json"

// codexSteerProbeTimeout bounds the schema-generation probe so a wedged
// or unexpected binary cannot stall Start.
const codexSteerProbeTimeout = 10 * time.Second

// codexSteerProbeCache remembers the answer per binary path: the CLI
// does not grow methods between two Starts of the same install.
var codexSteerProbeCache sync.Map // bin → bool

// codexAppServerSupportsSteer reports whether the Codex CLI at bin lists
// turn/steer in its app-server protocol. It asks the binary itself, via
// generate-json-schema into a scratch directory, rather than trusting a
// version threshold — the CLI ships inside ChatGPT.app and its version
// string is not the contract, its schema is.
func codexAppServerSupportsSteer(bin string) bool {
	if v, ok := codexSteerProbeCache.Load(bin); ok {
		return v.(bool)
	}
	supported := codexAppServerSupportsSteerFrom(bin, runCodexSchemaProbe)
	codexSteerProbeCache.Store(bin, supported)
	return supported
}

// codexAppServerSupportsSteerFrom is the injectable core: generate runs
// the schema dump into outDir, and the answer is whether the steer
// request's schema file appeared.
func codexAppServerSupportsSteerFrom(bin string, generate func(bin, outDir string) error) bool {
	outDir, err := os.MkdirTemp("", "claudia-codex-schema-*")
	if err != nil {
		return false
	}
	defer os.RemoveAll(outDir)
	if err := generate(bin, outDir); err != nil {
		return false
	}
	return codexSchemaListsTurnSteer(outDir)
}

// codexSchemaListsTurnSteer is the file-level check on a schema dump.
func codexSchemaListsTurnSteer(outDir string) bool {
	info, err := os.Stat(filepath.Join(outDir, codexSteerSchemaFile))
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func runCodexSchemaProbe(bin, outDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), codexSteerProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "app-server", "generate-json-schema", "--out", outDir)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// codexTurnCaps is the Codex contract as installed: the provider table's
// steer claim stands only when the CLI's schema lists turn/steer.
func codexTurnCaps(steerSupported bool) TurnCaps {
	caps := ProviderTurnCaps(ProviderCodex)
	if !steerSupported {
		caps = caps.withoutSteer()
	}
	return caps
}

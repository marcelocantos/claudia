// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"runtime"
)

// runLegacyACP exercises a retained ACP backend without changing the product
// provider route, which now starts the OMP sidecar for Grok and Cursor.
func runLegacyACP(ctx context.Context, prompt string, cfg Config, backend agentBackend) (string, error) {
	agent, err := startWithBackendContext(ctx, cfg, backend)
	if err != nil {
		return "", err
	}
	defer agent.Stop()

	type outcome struct {
		text string
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		text, err := agent.WaitForResponse(ctx)
		ch <- outcome{text, err}
	}()
	runtime.Gosched()

	if err := agent.Send(prompt); err != nil {
		return "", fmt.Errorf("send prompt: %w", err)
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case out := <-ch:
		return out.text, out.err
	}
}

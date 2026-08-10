// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package grok

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveConnect opens one real session against the xAI Realtime
// endpoint and closes it. Gated on CLAUDIA_GROK_LIVE=1 or
// CLAUDIA_LIVE=1 plus XAI_API_KEY — it spends API credit and needs the
// network, so it never runs in the default suite.
func TestLiveConnect(t *testing.T) {
	if os.Getenv("CLAUDIA_GROK_LIVE") == "" && os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_GROK_LIVE/CLAUDIA_LIVE not set (network / API credit)")
	}
	key := os.Getenv("XAI_API_KEY")
	if key == "" {
		t.Skip("XAI_API_KEY not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ready := make(chan struct{}, 1)
	c, err := Connect(ctx, Config{
		APIKey:         key,
		SystemPrompt:   "Reply with a single word.",
		ManualCommit:   true,
		OnSessionReady: func() { ready <- struct{}{} },
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("no session.updated from the live endpoint")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

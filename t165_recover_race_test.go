// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// 🎯T165: a seat refused on a revoked token and a recovery of the same plan,
// together, rotate the plan's token once. On 2026-09-30 a Jevons sweep's
// recovery raced the seats' own refreshes: the loser presented a spent
// refresh token, got invalid_grant, and fell into a sign-in nobody had
// asked for. Whichever goes second now finds the first one's token.
func TestT165SeatRefusalAndRecoveryRotateTheTokenOnce(t *testing.T) {
	var refreshes atomic.Int32
	s := startT141Sidecar(t)
	t141Plan(t, "revoked", &refreshes, "recovered")
	// A refresh that takes a moment, so the two overlap.
	run := ompLogin.Run
	ompLogin.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		time.Sleep(50 * time.Millisecond)
		return run(ctx, name, args...)
	}
	agent, conn := startT141Seat(t, s)
	t.Cleanup(func() { omp.ResetKeychainShot() })
	omp.MarkRejected(omp.Anthropic, "OAuth access token has been revoked")

	var wg sync.WaitGroup
	var recoverErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		recoverErr = RecoverOMPPlan(context.Background(), &OMPPlanRecovery{Plan: omp.Anthropic})
	}()
	// The pump handles the refusal before the marker behind it, so seeing
	// the marker means the seat's side is done.
	seen := make(chan struct{}, 1)
	tok := agent.SubscribeEvents(func(ev Event) {
		if ev.Type == "assistant" && ev.Text == "t165-marker" {
			select {
			case seen <- struct{}{}:
			default:
			}
		}
	})
	defer agent.UnsubscribeEvents(tok)
	t141Reject(t, conn)
	t141Write(t, conn, `{"type":"text","text":"t165-marker"}`)
	<-seen
	wg.Wait()
	if recoverErr != nil {
		t.Fatal(recoverErr)
	}
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("the plan's token was rotated %d times, want once", n)
	}
	if tok, err := planStore().AccessToken(context.Background(), omp.Anthropic); err != nil || tok != "recovered" {
		t.Fatalf("plan token = %q, %v", tok, err)
	}
}

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// 🎯T165: on 2026-10-01 the development broker logged 34 renewals of the
// Anthropic login in 25 minutes, every one "seat <name> refused". Seat A's
// refusal renewed the token, which revoked it under seat B's request already
// in flight; by the time B's 401 arrived B had been moved to the new token,
// so B's refusal looked like one of the current token and renewed again.
// A refusal just after a renewal is of the token that renewal replaced.
func TestT165InFlightRefusalAfterARenewalDoesNotRotateAgain(t *testing.T) {
	var refreshes atomic.Int32
	t141Plan(t, "t0", &refreshes, "t1")

	socket := filepath.Join(os.TempDir(), fmt.Sprintf("claudia-t165c-%d.sock", os.Getpid()))
	t.Cleanup(func() { os.Remove(socket) })
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				r := bufio.NewReader(c)
				for {
					if _, err := r.ReadBytes('\n'); err != nil {
						return
					}
				}
			}()
		}
	}()
	seat := func(name string) *ompControl {
		conn, err := omp.Dial(context.Background(), socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		c := &ompControl{conn: conn, token: "t0", provider: omp.Anthropic, seat: name}
		ompSeats.Store(c, struct{}{})
		t.Cleanup(func() { ompSeats.Delete(c) })
		return c
	}
	a, b := seat("jevons-po"), seat("jevons")

	a.recoverRejectedToken() // A's 401 on t0: one renewal, to t1
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("A's refusal renewed %d times, want 1", n)
	}
	if b.currentToken() != "t1" {
		t.Fatalf("B was not moved to the renewed token: %q", b.currentToken())
	}
	// B's request, in flight on t0, now fails; B already holds t1.
	b.recoverRejectedToken()
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("an in-flight refusal just after a renewal rotated the token again (%d renewals)", n)
	}
}

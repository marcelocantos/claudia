// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package omp

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// TestT161SeatHasHistoryStopsAtTheNewestConversation: a seat with a
// conversation in the newest day is answered from that day. The older day
// here is a FIFO nobody writes, so opening it blocks forever: the old
// check, which read every day oldest first and every line of each, never
// returns.
func TestT161SeatHasHistoryStopsAtTheNewestConversation(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "events-2026-09-25.log")
	if err := syscall.Mkfifo(older, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	t.Cleanup(func() {
		// Release a search stuck opening the FIFO.
		if f, err := os.OpenFile(older, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})
	writeDay(t, dir, "2026-09-30",
		`{"ts":"2026-09-30T01:00:00.000Z","seat":"claudia-po","type":"ready"}`,
		`{"ts":"2026-09-30T01:00:01.000Z","seat":"claudia-po","type":"accepted"}`)
	done := make(chan bool, 1)
	go func() { done <- SeatHasHistory(dir, "claudia-po") }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("claudia-po's accepted record was not found")
		}
	case <-wallclockguard.UntilTestTimeout(t).Done():
		t.Fatal("the search read past the newest day's conversation record into older days")
	}
}

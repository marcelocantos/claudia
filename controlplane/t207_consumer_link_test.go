// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/wallclockguard"
)

// 🎯T207: when a consumer peer is gone, the next control link is admitted.
func TestT207ConsumerRelinksAfterDeadPeer(t *testing.T) {
	var slot consumerLinkSlot
	old := linkPeerAlive
	linkPeerAlive = func(pid int) bool { return pid == 42 }
	defer func() { linkPeerAlive = old }()

	first := &stallConn{closed: make(chan struct{}), writing: make(chan struct{}, 1)}
	if !slot.begin(first) {
		t.Fatal("first consumer did not take the slot")
	}
	second := &stallConn{closed: make(chan struct{}), writing: make(chan struct{}, 1)}
	if slot.begin(second) {
		t.Fatal("second consumer linked while the first peer was still alive")
	}
	linkPeerAlive = func(int) bool { return false }
	go func() {
		first.Close()
		slot.end()
	}()
	backstop := wallclockguard.UntilTestTimeout(t)
	for backstop.Err() == nil {
		if slot.begin(second) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("replacement consumer was refused while the dead peer still held the slot")
}

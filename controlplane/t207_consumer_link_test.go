// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"net"
	"testing"
)

func restoreLinkSlotHooks(alive func(int) bool, pidOf func(net.Conn) int) {
	linkPeerAlive = alive
	peerPIDOf = pidOf
}

// 🎯T207: when a consumer peer is gone, the next control link is admitted.
func TestT207ConsumerRelinksAfterDeadPeer(t *testing.T) {
	var slot consumerLinkSlot
	defer restoreLinkSlotHooks(linkPeerAlive, peerPIDOf)

	first := &stallConn{closed: make(chan struct{}), writing: make(chan struct{}, 1)}
	second := &stallConn{closed: make(chan struct{}), writing: make(chan struct{}, 1)}
	peerPIDOf = func(c net.Conn) int {
		if c == first {
			return 42
		}
		return 43
	}
	linkPeerAlive = func(pid int) bool { return pid == 42 }

	if _, ok := slot.begin(first); !ok {
		t.Fatal("first consumer did not take the slot")
	}
	if _, ok := slot.begin(second); ok {
		t.Fatal("second consumer linked while the first peer was still alive")
	}
	linkPeerAlive = func(int) bool { return false }
	if _, ok := slot.begin(second); !ok {
		t.Fatal("replacement consumer was refused while the dead peer still held the slot")
	}
}

// 🎯T227: a same-pid reconnect while the previous runLink is still unwinding
// takes the slot immediately. The previous end must not clear the new occupant
// (generation-scoped). The in-process replica tests, and a jevonsd that redials
// before the broker-side goroutine has called end, share one pid that is still
// alive.
func TestSamePIDReconnectTakesSlotFromPriorLink(t *testing.T) {
	var slot consumerLinkSlot
	defer restoreLinkSlotHooks(linkPeerAlive, peerPIDOf)

	first := &stallConn{closed: make(chan struct{}), writing: make(chan struct{}, 1)}
	second := &stallConn{closed: make(chan struct{}), writing: make(chan struct{}, 1)}
	third := &stallConn{closed: make(chan struct{}), writing: make(chan struct{}, 1)}
	peerPIDOf = func(c net.Conn) int {
		if c == third {
			return 8
		}
		return 7
	}
	linkPeerAlive = func(int) bool { return true }

	gen1, ok := slot.begin(first)
	if !ok {
		t.Fatal("first consumer did not take the slot")
	}
	gen2, ok := slot.begin(second)
	if !ok {
		t.Fatal("same-pid reconnect refused while the previous link was still held")
	}
	if gen2 == gen1 {
		t.Fatal("same-pid reconnect did not bump the slot generation")
	}
	slot.end(gen1)
	if _, ok := slot.begin(third); ok {
		t.Fatal("previous end released the stolen slot; a different live pid was admitted")
	}
	slot.end(gen2)
	if _, ok := slot.begin(third); !ok {
		t.Fatal("slot stayed busy after the occupant that stole it ended")
	}
}

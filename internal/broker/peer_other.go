//go:build !darwin && !linux

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import "net"

// peerPID is unavailable on this platform.
func peerPID(net.Conn) int { return 0 }

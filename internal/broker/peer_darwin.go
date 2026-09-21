// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerPID reads the peer process id of a unix socket (LOCAL_PEERPID).
func peerPID(nc net.Conn) int {
	uc, ok := nc.(*net.UnixConn)
	if !ok {
		return 0
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0
	}
	pid := 0
	_ = raw.Control(func(fd uintptr) {
		if v, err := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID); err == nil {
			pid = v
		}
	})
	return pid
}

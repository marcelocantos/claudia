// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerPID reads the peer process id of a unix socket (SO_PEERCRED).
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
		if cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); err == nil {
			pid = int(cred.Pid)
		}
	})
	return pid
}

//go:build linux

package api

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID returns the connecting process's effective UID via SO_PEERCRED.
func peerUID(conn net.Conn) (uid int, ok bool) {
	uc, isUnix := conn.(*net.UnixConn)
	if !isUnix {
		return 0, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || cerr != nil || cred == nil {
		return 0, false
	}
	return int(cred.Uid), true
}

//go:build !linux && !darwin

package api

import "net"

// peerUID is unavailable on this platform; the caller falls back to the
// socket's 0600 permissions plus the token.
func peerUID(net.Conn) (int, bool) { return 0, false }

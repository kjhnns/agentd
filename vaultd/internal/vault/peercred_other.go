//go:build !linux

package vault

import (
	"errors"
	"net"
)

// peerUID is implemented for Linux only (SO_PEERCRED). Elsewhere (the macOS
// test peer) every admin connection is refused.
func peerUID(*net.UnixConn) (int, error) {
	return -1, errors.New("peer credentials not supported here")
}

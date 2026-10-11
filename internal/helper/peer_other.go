//go:build !linux

package helper

import (
	"errors"
	"net"
)

// peerUID refuses: the helper serves only on Linux, where SO_PEERCRED
// names the connecting user.
func peerUID(*net.UnixConn) (int, error) {
	return 0, errors.New("the helper checks its peer on Linux only")
}

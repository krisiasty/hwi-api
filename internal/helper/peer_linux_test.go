//go:build linux

package helper

import (
	"os"
	"strconv"
	"syscall"
	"testing"
)

func TestPeerUIDReadsTheConnectingUser(t *testing.T) {
	ours, peer := socketPair(t)
	defer syscall.Close(peer)
	env := map[string]string{"LISTEN_PID": strconv.Itoa(os.Getpid()), "LISTEN_FDS": "1"}
	conn, err := activatedConn(func(k string) string { return env[k] }, os.Getpid(), uintptr(ours))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if uid, err := peerUID(conn); err != nil || uid != os.Getuid() {
		t.Errorf("peerUID = %d, %v; want %d", uid, err, os.Getuid())
	}
}

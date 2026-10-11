package helper

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
)

// activatedConn returns the connection systemd passes an instance it
// started for one accepted connection (the socket's Accept=yes): file
// descriptor fd, 3, announced by LISTEN_PID and LISTEN_FDS. getenv and pid
// are os.Getenv and os.Getpid, for tests.
func activatedConn(getenv func(string) string, pid int, fd uintptr) (*net.UnixConn, error) {
	if getenv("LISTEN_PID") != strconv.Itoa(pid) || getenv("LISTEN_FDS") != "1" {
		return nil, errors.New("not started by socket activation: the helper runs from hwi-api-helper.socket, not by hand")
	}
	f := os.NewFile(fd, "connection")
	defer func() { _ = f.Close() }() // the duplicate made by FileConn is what we keep
	c, err := net.FileConn(f)
	if err != nil {
		return nil, fmt.Errorf("the socket activation connection: %w", err)
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		_ = c.Close()
		return nil, fmt.Errorf("the socket activation connection is %T, not a Unix socket", c)
	}
	return uc, nil
}

package helper

import (
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func socketPair(t *testing.T) (int, int) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	return fds[0], fds[1]
}

func TestActivatedConnTakesTheConnectionSystemdPasses(t *testing.T) {
	ours, peer := socketPair(t)
	defer syscall.Close(peer)
	env := map[string]string{"LISTEN_PID": strconv.Itoa(os.Getpid()), "LISTEN_FDS": "1"}
	conn, err := activatedConn(func(k string) string { return env[k] }, os.Getpid(), uintptr(ours))
	if err != nil {
		t.Fatalf("activatedConn: %v", err)
	}
	defer conn.Close()
	if _, err := syscall.Write(peer, []byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping\n" {
		t.Errorf("read %q, %v", buf, err)
	}
}

// Run by hand, or for another process, it refuses rather than serving
// whatever file descriptor 3 happens to be.
func TestActivatedConnRefusesWithoutActivation(t *testing.T) {
	pid := strconv.Itoa(os.Getpid())
	for name, env := range map[string]map[string]string{
		"no environment":  {},
		"another process": {"LISTEN_PID": "1", "LISTEN_FDS": "1"},
		"two sockets":     {"LISTEN_PID": pid, "LISTEN_FDS": "2"},
	} {
		_, err := activatedConn(func(k string) string { return env[k] }, os.Getpid(), 3)
		if err == nil || !strings.Contains(err.Error(), "socket activation") {
			t.Errorf("%s: %v, want a socket activation error", name, err)
		}
	}
}

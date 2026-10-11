//go:build linux

package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pairedConn is the helper's end of a socket pair, taken as activatedConn
// takes it, and the peer's end. The returned file owns the peer's
// descriptor: a test may close it early, to end the connection, and the
// cleanup's second Close then fails harmlessly instead of closing whatever
// has reused the descriptor number since.
func pairedConn(t *testing.T) (*net.UnixConn, *os.File) {
	t.Helper()
	ours, peerFD := socketPair(t)
	peer := os.NewFile(uintptr(peerFD), "peer")
	t.Cleanup(func() { _ = peer.Close() })
	env := map[string]string{"LISTEN_PID": strconv.Itoa(os.Getpid()), "LISTEN_FDS": "1"}
	conn, err := activatedConn(func(k string) string { return env[k] }, os.Getpid(), uintptr(ours))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, peer
}

func currentUser(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skipf("no current user: %v", err)
	}
	return u.Username
}

func TestServeRefusesAnotherUser(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, the user the test would name")
	}
	conn, _ := pairedConn(t)
	err := serve(context.Background(), conn, "root", hostRoot())
	if err == nil || !strings.Contains(err.Error(), "refused a connection") {
		t.Errorf("serve = %v, want a refusal", err)
	}
}

func TestServeAnswersItsPeer(t *testing.T) {
	conn, f := pairedConn(t)
	done := make(chan error, 1)
	go func() { done <- serve(context.Background(), conn, currentUser(t), hostRoot()) }()

	if _, err := f.WriteString(`{"op":"dmi","name":"product_serial"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil || string(resp.Data) != "EXSERIAL0001\n" {
		t.Errorf("answer %q (%v), want the serial", line, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve after the peer closed: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("serve did not return after the peer closed")
	}
}

// systemd stops an instance with SIGTERM, which main turns into a
// cancelled context; an idle connection must not keep the helper alive.
func TestServeStopsWhenItsContextIsCancelled(t *testing.T) {
	conn, _ := pairedConn(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, conn, currentUser(t), hostRoot()) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve after cancel: %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve still blocked after its context was cancelled")
	}
}

//go:build linux

package ethtool

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
)

const (
	receiveBufferSize = 64 << 10        // a verbose GET reply is a few KiB; the kernel flags a larger one MSG_TRUNC, which is an error
	replyTimeout      = 5 * time.Second // how long one request, its send included, may take when the Dial context allows longer
	// abandonGrace is how long past its deadline a request is waited for before it is abandoned as stuck in the kernel. The receives give up at
	// the deadline themselves, so a request that merely got no reply returns in time and leaves nothing behind. A request therefore takes at
	// most replyTimeout+abandonGrace, 5.1 s, and overruns the Dial context's deadline by at most abandonGrace.
	abandonGrace = 100 * time.Millisecond
)

// socket is the exchanger over a NETLINK_GENERIC socket.
type socket struct {
	fd int
}

// Dial opens a generic-netlink socket and resolves the ethtool family over it: ErrNoFamily when the kernel has none, ErrBusy while an earlier
// request is still blocked in the kernel or another session is open, or the socket's error. Each request, its send included, is bounded by
// replyTimeout or the context's deadline, whichever comes first, plus abandonGrace. One session is open at a time, process-wide: it holds its
// place until Close, and after Close until any request still blocked in the kernel returns, so a caller must always Close the Client.
func Dial(ctx context.Context) (*Client, error) {
	return dialBounded(ctx, replyTimeout, abandonGrace, openSocket)
}

func openSocket() (exchanger, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_GENERIC)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, errors.Join(fmt.Errorf("netlink bind: %w", err), syscall.Close(fd))
	}
	return &socket{fd: fd}, nil
}

// exchange sends req to the kernel and receives until the reply to seq is complete, each receive giving up at deadline. The send has no bound:
// boundedConn stops waiting for it.
func (s *socket) exchange(req []byte, seq uint32, deadline time.Time) ([]message, error) {
	if err := s.send(req); err != nil {
		return nil, err
	}
	var reply []message
	for {
		msgs, err := s.receive(deadline)
		if err != nil {
			return nil, err
		}
		got, done := replyMessages(msgs, seq)
		reply = append(reply, got...)
		if done {
			return reply, nil
		}
	}
}

func (s *socket) send(req []byte) error {
	for {
		err := syscall.Sendto(s.fd, req, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK})
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("netlink send: %w", err)
		}
		return nil
	}
}

// receive returns the messages of one datagram from the kernel. Each receive gets a fresh buffer because parsed messages alias it, and the
// caller keeps them past the next receive.
func (s *socket) receive(deadline time.Time) ([]message, error) {
	for {
		// SO_RCVTIMEO is reset before every receive, retries included, so the wait never runs past deadline. A zero timeout would block forever,
		// so less than the timeout's microsecond resolution left counts as expired.
		d := time.Until(deadline)
		if d < time.Microsecond {
			return nil, errNoReply()
		}
		tv := syscall.NsecToTimeval(d.Nanoseconds())
		if err := syscall.SetsockoptTimeval(s.fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
			return nil, fmt.Errorf("netlink receive timeout: %w", err)
		}
		buf := make([]byte, receiveBufferSize)
		n, _, flags, from, err := syscall.Recvmsg(s.fd, buf, nil, 0)
		switch {
		case err == syscall.EINTR: // a receive with a timeout is not restarted after a signal; Go's runtime sends them routinely
			continue
		case err == syscall.EAGAIN:
			return nil, errNoReply()
		case err != nil:
			return nil, fmt.Errorf("netlink receive: %w", err)
		}
		if flags&syscall.MSG_TRUNC != 0 {
			return nil, fmt.Errorf("netlink receive: reply truncated to the %d-byte buffer", receiveBufferSize)
		}
		// Another process can send to this socket's port id; only the kernel (port 0) answers requests.
		if sa, ok := from.(*syscall.SockaddrNetlink); !ok || sa.Pid != 0 {
			continue
		}
		return parseMessages(buf[:n])
	}
}

// closeFD closes the socket; boundedConn calls it once, when no exchange is using it.
func (s *socket) closeFD() error {
	return syscall.Close(s.fd)
}

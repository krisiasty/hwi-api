package ethtool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// exchanger is the blocking half of a conn: exchange sends one request and receives until its reply to seq is complete, its receives giving up
// at deadline; closeFD closes the socket. Nothing bounds exchange's send: the kernel answers a netlink request inside the send itself, taking RTNL
// and calling the driver, so a stalled driver or a long-held RTNL blocks it for as long as they last, and no socket timeout or signal ends it.
type exchanger interface {
	exchange(req []byte, seq uint32, deadline time.Time) ([]message, error)
	closeFD() error
}

// sessionToken is held by the one session open at a time, process-wide, from Dial until its socket is closed. A session whose exchange timed out
// holds it until that exchange returns, however long the kernel takes: the exchange is a thread blocked in the kernel that nothing can cancel, and
// a Dial meanwhile gets ErrBusy rather than leaking a further thread on each refresh.
var sessionToken = make(chan struct{}, 1)

// releaseSession frees sessionToken; each session calls it exactly once, as its socket is closed.
func releaseSession() { <-sessionToken }

// dialBounded opens a session over the exchanger open returns, unless another session holds sessionToken (ErrBusy). Each round trip, the family
// lookup included, gets until timeout or ctx's deadline, whichever comes first; its exchange is abandoned only once grace more has passed.
func dialBounded(ctx context.Context, timeout, grace time.Duration, open func() (exchanger, error)) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case sessionToken <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	e, err := open()
	if err != nil {
		releaseSession()
		return nil, err
	}
	b := &boundedConn{inner: e, timeout: timeout, grace: grace}
	b.deadline, _ = ctx.Deadline()
	// On a failed family lookup newClient closes b, which releases the token, at once or when the abandoned exchange returns.
	return newClient(b)
}

// boundedConn is a conn that runs each exchange in its own goroutine and stops waiting for it grace after its deadline. The exchange's receives
// give up at the deadline themselves, so only one whose send is stuck in the kernel overruns it, and is abandoned. A request that times out
// either way poisons the connection, which then fails every request without sending it. A close while an exchange runs is left to the
// exchange's goroutine, which closes the socket and releases sessionToken once the exchange returns, so that the descriptor is never closed,
// and perhaps reused, under a running syscall.
type boundedConn struct {
	inner    exchanger
	timeout  time.Duration
	grace    time.Duration
	deadline time.Time // the Dial context's deadline; zero when it has none

	mu       sync.Mutex
	inFlight bool // an exchange is running
	timedOut bool // a request timed out, its exchange perhaps abandoned: the connection is poisoned
	closed   bool
	released bool // the socket is closed and sessionToken released; each happens once, whoever closes
}

// result is what one exchange returned.
type result struct {
	msgs []message
	err  error
}

func (b *boundedConn) roundTrip(req []byte, seq uint32) ([]message, error) {
	deadline := time.Now().Add(b.timeout)
	if !b.deadline.IsZero() && b.deadline.Before(deadline) {
		deadline = b.deadline
	}
	b.mu.Lock()
	switch {
	case b.closed:
		b.mu.Unlock()
		return nil, fmt.Errorf("netlink: %w", net.ErrClosed)
	case b.timedOut:
		b.mu.Unlock()
		return nil, errNoReply()
	case !time.Now().Before(deadline):
		b.mu.Unlock()
		return nil, errNoReply()
	}
	b.inFlight = true
	b.mu.Unlock()

	done := make(chan result, 1)
	go b.run(req, seq, deadline, done)
	timer := time.NewTimer(time.Until(deadline) + b.grace)
	defer timer.Stop()
	var r result
	select {
	case r = <-done:
	case <-timer.C:
		b.mu.Lock()
		if b.inFlight {
			// The send is stuck in the kernel: abandon the exchange.
			b.timedOut = true
			b.mu.Unlock()
			return nil, errNoReply()
		}
		b.mu.Unlock()
		r = <-done // the exchange returned as the timer fired; run sent its result before clearing inFlight
	}
	if errors.Is(r.err, os.ErrDeadlineExceeded) {
		// The reply may still come; no later request is to read it as its own.
		b.mu.Lock()
		b.timedOut = true
		b.mu.Unlock()
	}
	return r.msgs, r.err
}

// run runs one exchange and delivers its result. If the connection was closed while the exchange ran, run closes the socket and releases
// sessionToken.
func (b *boundedConn) run(req []byte, seq uint32, deadline time.Time, done chan<- result) {
	msgs, err := b.inner.exchange(req, seq, deadline)
	b.mu.Lock()
	defer b.mu.Unlock()
	done <- result{msgs, err}
	b.inFlight = false
	if b.closed {
		_ = b.releaseLocked() // close returned long ago; nobody is left to report it to
	}
}

// close closes the socket and releases sessionToken, or, while an exchange still uses the socket, leaves both to the exchange's goroutine.
// Closing it again does nothing.
func (b *boundedConn) close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	if b.inFlight {
		return nil
	}
	return b.releaseLocked()
}

// releaseLocked closes the socket and releases sessionToken, unless this connection already has: were two exchanges to run at once, which only
// concurrent use of a Client can cause, each would finish the deferred close, and a second release would close a descriptor perhaps reused and
// take another session's token. b.mu must be held.
func (b *boundedConn) releaseLocked() error {
	if b.released {
		return nil
	}
	b.released = true
	defer releaseSession()
	return b.inner.closeFD()
}

func errNoReply() error {
	return fmt.Errorf("netlink: no reply: %w", os.ErrDeadlineExceeded)
}

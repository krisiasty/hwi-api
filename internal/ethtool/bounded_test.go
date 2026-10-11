package ethtool

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testTimeout = 50 * time.Millisecond // bounds each round trip in these tests; the stub's blocked exchanges never answer within it
	testGrace   = 10 * time.Millisecond
)

// stubExchanger answers the family lookup at once and blocks every later exchange until release is closed, as a send stuck in the kernel
// would, then answers it with reply. answer, when set, answers the later exchanges instead; stallFamily blocks the family lookup too.
type stubExchanger struct {
	family      []message
	reply       []message
	answer      func(deadline time.Time) ([]message, error)
	stallFamily bool
	release     chan struct{}
	once        sync.Once
	calls       atomic.Int32
	closes      atomic.Int32
}

func newStubExchanger(t *testing.T, reply []message) *stubExchanger {
	t.Helper()
	s := &stubExchanger{
		family: genlReply(t, ctrlCmdNewFamily, stringAttr(ctrlAttrFamilyName, ethtoolFamilyName),
			encodeAttr(ctrlAttrFamilyID, binary.NativeEndian.AppendUint16(nil, testFamily))),
		reply:   reply,
		release: make(chan struct{}),
	}
	// A test must leave no exchange blocked and no session open: every later Dial in the package would get ErrBusy.
	t.Cleanup(func() {
		s.unblock()
		waitSessionFree(t)
	})
	return s
}

func (s *stubExchanger) exchange(_ []byte, _ uint32, deadline time.Time) ([]message, error) {
	if s.calls.Add(1) == 1 && !s.stallFamily {
		return s.family, nil
	}
	if s.answer != nil {
		return s.answer(deadline)
	}
	<-s.release
	return s.reply, nil
}

func (s *stubExchanger) closeFD() error {
	s.closes.Add(1)
	return nil
}

func (s *stubExchanger) unblock() { s.once.Do(func() { close(s.release) }) }

// waitSessionFree waits until no session holds sessionToken.
func waitSessionFree(t *testing.T) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); len(sessionToken) != 0; {
		if time.Now().After(end) {
			t.Fatal("sessionToken is still held")
		}
		time.Sleep(time.Millisecond)
	}
}

// waitFor waits until cond holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); !cond(); {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// dialStub opens a Client over s as Dial does, with round trips bounded by timeout and ctx's deadline, abandoned grace later.
func dialStub(ctx context.Context, s *stubExchanger, timeout, grace time.Duration) (*Client, error) {
	return dialBounded(ctx, timeout, grace, func() (exchanger, error) { return s, nil })
}

// mustDialStub opens a Client over s and closes it when the test ends, before s's cleanup waits for the session to be released.
func mustDialStub(t *testing.T, ctx context.Context, s *stubExchanger, timeout, grace time.Duration) *Client {
	t.Helper()
	c, err := dialStub(ctx, s, timeout, grace)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// stall sends a FEATURES query that the stub blocks, and checks that it fails as a timeout within the bound rather than waiting for it.
func stall(t *testing.T, c *Client) {
	t.Helper()
	start := time.Now()
	_, err := c.Features(1, "eth1")
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Features returned after %v, want about %v", elapsed, testTimeout+testGrace)
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Features = %v, want os.ErrDeadlineExceeded", err)
	}
	if want := "features: netlink: no reply: i/o timeout"; err.Error() != want {
		t.Errorf("Features = %q, want %q", err, want)
	}
}

func TestBoundedRoundTripTimesOut(t *testing.T) {
	s := newStubExchanger(t, nil)
	c := mustDialStub(t, context.Background(), s, testTimeout, testGrace)
	stall(t, c)

	// The connection is poisoned: the next request fails at once, without being sent.
	if _, err := c.FEC(1, "eth1"); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("FEC after a timeout = %v, want os.ErrDeadlineExceeded", err)
	}
	if n := s.calls.Load(); n != 2 {
		t.Errorf("%d exchanges, want 2: the family lookup and the stalled query only", n)
	}
}

// The Dial context's deadline bounds a round trip when it is earlier than the timeout.
func TestBoundedRoundTripContextDeadline(t *testing.T) {
	s := newStubExchanger(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	stall(t, mustDialStub(t, ctx, s, time.Hour, testGrace))
}

// An exchange whose own receive times out within the grace is not abandoned: its timeout is reported, and the session closes and frees its place
// at once.
func TestBoundedTimeoutWithinGrace(t *testing.T) {
	const grace = 100 * time.Millisecond // wide, so that the exchange's return at half of it never races the timer
	s := newStubExchanger(t, nil)
	s.answer = func(deadline time.Time) ([]message, error) {
		time.Sleep(time.Until(deadline.Add(grace / 2)))
		return nil, errNoReply()
	}
	c := mustDialStub(t, context.Background(), s, testTimeout, grace)
	if _, err := c.Features(1, "eth1"); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Features = %v, want os.ErrDeadlineExceeded", err)
	}
	// Its late reply could still come: the connection is poisoned all the same.
	if _, err := c.FEC(1, "eth1"); !errors.Is(err, os.ErrDeadlineExceeded) || s.calls.Load() != 2 {
		t.Errorf("FEC after a timeout = %v after %d exchanges; want os.ErrDeadlineExceeded after 2", err, s.calls.Load())
	}

	if err := c.Close(); err != nil || s.closes.Load() != 1 {
		t.Fatalf("Close = %v, closes %d; want nil, 1 at once", err, s.closes.Load())
	}
	if len(sessionToken) != 0 {
		t.Fatal("the session still holds its place after Close")
	}
	next, err := dialStub(context.Background(), newStubExchanger(t, nil), testTimeout, testGrace)
	if err != nil {
		t.Fatalf("dial right after Close = %v, want a client", err)
	}
	_ = next.Close()
}

func TestDialOneSessionAtATime(t *testing.T) {
	first := mustDialStub(t, context.Background(), newStubExchanger(t, nil), testTimeout, testGrace)

	opened := false
	open := func() (exchanger, error) { opened = true; return newStubExchanger(t, nil), nil }
	if c, err := dialBounded(context.Background(), testTimeout, testGrace, open); !errors.Is(err, ErrBusy) {
		t.Errorf("dial while a session is open = %v, %v; want ErrBusy", c, err)
	}
	if opened {
		t.Error("dial opened a socket while a session is open")
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second, err := dialBounded(context.Background(), testTimeout, testGrace, open)
	if err != nil {
		t.Fatalf("dial after Close = %v, want a client", err)
	}
	_ = second.Close()
}

func TestDialBusyWhileExchangeStuck(t *testing.T) {
	s := newStubExchanger(t, nil)
	c := mustDialStub(t, context.Background(), s, testTimeout, testGrace)
	stall(t, c)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	opened := false
	open := func() (exchanger, error) { opened = true; return newStubExchanger(t, nil), nil }
	if c, err := dialBounded(context.Background(), testTimeout, testGrace, open); !errors.Is(err, ErrBusy) {
		t.Errorf("dial while an exchange is stuck = %v, %v; want ErrBusy", c, err)
	}
	if opened {
		t.Error("dial opened a socket while an exchange is stuck")
	}

	s.unblock()
	waitSessionFree(t)
	next, err := dialBounded(context.Background(), testTimeout, testGrace, open)
	if err != nil {
		t.Fatalf("dial after the stuck exchange returned = %v, want a client", err)
	}
	_ = next.Close()
}

func TestBoundedCloseWaitsForStuckExchange(t *testing.T) {
	s := newStubExchanger(t, nil)
	c := mustDialStub(t, context.Background(), s, testTimeout, testGrace)
	stall(t, c)

	for range 2 {
		if err := c.Close(); err != nil {
			t.Errorf("Close = %v, want nil", err)
		}
	}
	if n := s.closes.Load(); n != 0 {
		t.Fatalf("the socket was closed %d times while its exchange was still running", n)
	}

	s.unblock()
	waitSessionFree(t)
	if n := s.closes.Load(); n != 1 {
		t.Errorf("the socket was closed %d times after its exchange returned, want once", n)
	}
	if err := c.Close(); err != nil || s.closes.Load() != 1 {
		t.Errorf("Close again = %v, closes %d; want nil, 1", err, s.closes.Load())
	}
}

// A close while an exchange that is not stuck is running is still done, once the exchange returns.
func TestBoundedCloseDuringExchange(t *testing.T) {
	s := newStubExchanger(t, genlReply(t, ethtoolMsgFeaturesGetReply))
	c := mustDialStub(t, context.Background(), s, time.Hour, testGrace)
	queried := make(chan error, 1)
	go func() {
		_, err := c.Features(1, "eth1")
		queried <- err
	}()
	waitFor(t, "the query's exchange", func() bool { return s.calls.Load() == 2 })

	if err := c.Close(); err != nil || s.closes.Load() != 0 {
		t.Fatalf("Close during the exchange = %v, closes %d; want nil, 0", err, s.closes.Load())
	}
	s.unblock()
	if err := <-queried; err != nil {
		t.Errorf("Features = %v, want its reply", err)
	}
	waitSessionFree(t)
	if n := s.closes.Load(); n != 1 {
		t.Errorf("the socket was closed %d times after its exchange returned, want once", n)
	}
}

func TestBoundedRoundTripAnswers(t *testing.T) {
	s := newStubExchanger(t, genlReply(t, ethtoolMsgFeaturesGetReply,
		verboseBitset(ethtoolAFeaturesActive, true, testBit{index: 1, name: "rx-checksum"})))
	s.unblock()
	c := mustDialStub(t, context.Background(), s, testTimeout, testGrace)
	features, err := c.Features(1, "eth1")
	if err != nil {
		t.Fatalf("Features: %v", err)
	}
	if !features["rx-checksum"].Active {
		t.Errorf("Features = %+v, want rx-checksum active", features)
	}

	if err := c.Close(); err != nil || s.closes.Load() != 1 || len(sessionToken) != 0 {
		t.Fatalf("Close = %v, closes %d, session held %v; want nil, 1, false", err, s.closes.Load(), len(sessionToken) != 0)
	}
	if err := c.Close(); err != nil || s.closes.Load() != 1 {
		t.Errorf("Close again = %v, closes %d; want nil, 1", err, s.closes.Load())
	}
	// A closed connection sends nothing more.
	if _, err := c.Features(1, "eth1"); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Features after Close = %v, want net.ErrClosed", err)
	}
	if n := s.calls.Load(); n != 2 {
		t.Errorf("%d exchanges, want 2", n)
	}
}

// A dial that fails after taking the session's place gives it back: at once, or, when the family lookup is stuck, once it returns.
func TestDialReleasesSessionOnFailure(t *testing.T) {
	t.Run("open fails", func(t *testing.T) {
		failed := errors.New("netlink socket: no")
		if _, err := dialBounded(context.Background(), testTimeout, testGrace, func() (exchanger, error) { return nil, failed }); !errors.Is(err, failed) {
			t.Errorf("dial = %v, want %v", err, failed)
		}
		if len(sessionToken) != 0 {
			t.Error("a failed open kept the session's place")
		}
	})

	t.Run("no family", func(t *testing.T) {
		s := newStubExchanger(t, nil)
		s.family = errorReply(t, -2)
		if _, err := dialStub(context.Background(), s, testTimeout, testGrace); !errors.Is(err, ErrNoFamily) {
			t.Errorf("dial = %v, want ErrNoFamily", err)
		}
		if s.closes.Load() != 1 || len(sessionToken) != 0 {
			t.Errorf("closes %d, session held %v; want 1, false", s.closes.Load(), len(sessionToken) != 0)
		}
	})

	t.Run("family lookup stuck", func(t *testing.T) {
		s := newStubExchanger(t, nil)
		s.stallFamily = true
		if _, err := dialStub(context.Background(), s, testTimeout, testGrace); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("dial = %v, want os.ErrDeadlineExceeded", err)
		}
		if s.closes.Load() != 0 || len(sessionToken) == 0 {
			t.Fatalf("while the lookup is stuck: closes %d, session held %v; want 0, true", s.closes.Load(), len(sessionToken) != 0)
		}
		s.unblock()
		waitSessionFree(t)
		if n := s.closes.Load(); n != 1 {
			t.Errorf("the socket was closed %d times after the lookup returned, want once", n)
		}
	})
}

// Misuse, two concurrent round trips on one connection, leaves two exchanges to finish a deferred close: the socket is closed and the session's
// place given back once, so that a later session's place is not taken from it.
func TestBoundedReleasesOnce(t *testing.T) {
	s := newStubExchanger(t, nil)
	c := mustDialStub(t, context.Background(), s, time.Hour, testGrace)
	var wg sync.WaitGroup
	for seq := range uint32(2) {
		wg.Go(func() { _, _ = c.c.roundTrip(nil, 10+seq) })
	}
	waitFor(t, "both exchanges", func() bool { return s.calls.Load() == 3 })
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s.unblock()
	wg.Wait()
	waitSessionFree(t)
	if n := s.closes.Load(); n != 1 {
		t.Errorf("the socket was closed %d times, want once", n)
	}

	second, err := dialStub(context.Background(), newStubExchanger(t, nil), testTimeout, testGrace)
	if err != nil {
		t.Fatalf("second dial = %v, want a client", err)
	}
	defer func() { _ = second.Close() }()
	// Give a second release, were there one, time to take the second session's place.
	time.Sleep(10 * time.Millisecond)
	if c, err := dialStub(context.Background(), newStubExchanger(t, nil), testTimeout, testGrace); !errors.Is(err, ErrBusy) {
		t.Errorf("third dial while the second session is open = %v, %v; want ErrBusy", c, err)
	}
}

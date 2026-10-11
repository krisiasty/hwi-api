package inventory

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Netlink message types and attributes the synthetic dumps use.
const (
	testNLMsgNoop    = 1
	testNLMsgError   = 2
	testNLMsgDone    = 3
	testRTMNewLink   = 16
	testIFLAAddress  = 1
	testIFLAIfname   = 3
	testIFLAPermAddr = 54
)

// nlMessage is one netlink message of type typ carrying body, in native byte order and padded to 4 bytes as the kernel sends it.
func nlMessage(typ uint16, body []byte) []byte {
	b := make([]byte, 16, 16+len(body)+3)
	binary.NativeEndian.PutUint32(b[0:], uint32(16+len(body)))
	binary.NativeEndian.PutUint16(b[4:], typ)
	binary.NativeEndian.PutUint32(b[8:], 1) // seq
	b = append(b, body...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// rtAttr is one route attribute, padded to 4 bytes.
func rtAttr(typ uint16, data []byte) []byte {
	b := make([]byte, 4, 4+len(data)+3)
	binary.NativeEndian.PutUint16(b[0:], uint16(4+len(data)))
	binary.NativeEndian.PutUint16(b[2:], typ)
	b = append(b, data...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// newLinkMessage is an RTM_NEWLINK message for the interface index, with attrs after its ifinfomsg.
func newLinkMessage(index int32, attrs ...[]byte) []byte {
	body := make([]byte, 16) // ifinfomsg: family, pad, type, index, flags, change
	binary.NativeEndian.PutUint16(body[2:], 1)
	binary.NativeEndian.PutUint32(body[4:], uint32(index))
	for _, a := range attrs {
		body = append(body, a...)
	}
	return nlMessage(testRTMNewLink, body)
}

func concat(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

// linkDump is a dump of five links: two with a permanent address, one without, one with an all-zero one, and one after NLMSG_DONE that the
// parser must not read; a message of another type sits among them.
func linkDump() []byte {
	return concat(
		newLinkMessage(2,
			rtAttr(testIFLAIfname, []byte("ens1f0\x00")),
			rtAttr(testIFLAAddress, []byte{0x52, 0x54, 0x00, 0x00, 0x00, 0x0a}),
			rtAttr(testIFLAPermAddr, []byte{0x3c, 0xfd, 0xfe, 0xa1, 0xb2, 0xc3})),
		newLinkMessage(3, rtAttr(testIFLAIfname, []byte("br0\x00")), rtAttr(testIFLAAddress, []byte{0x52, 0x54, 0x00, 0x00, 0x00, 0x0b})),
		nlMessage(testNLMsgNoop, nil),
		newLinkMessage(4, rtAttr(testIFLAPermAddr, make([]byte, 6))),
		newLinkMessage(5, rtAttr(testIFLAPermAddr, []byte{0xB8, 0x3F, 0xD2, 0x00, 0x00, 0x0A}), rtAttr(testIFLAIfname, []byte("ens1f1\x00"))),
		nlMessage(testNLMsgDone, make([]byte, 4)),
		newLinkMessage(9, rtAttr(testIFLAPermAddr, []byte{0x3c, 0xfd, 0xfe, 0x00, 0x00, 0x09})),
	)
}

func TestParsePermanentAddresses(t *testing.T) {
	got, err := parsePermanentAddresses(linkDump())
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint32]string{2: "3c:fd:fe:a1:b2:c3", 5: "b8:3f:d2:00:00:0a"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("permanent addresses = %v, want %v", got, want)
	}
}

// A malformed dump is an error, never a panic, and never a partial map.
func TestParsePermanentAddressesRejectsMalformedMessages(t *testing.T) {
	valid := newLinkMessage(2, rtAttr(testIFLAPermAddr, []byte{0x3c, 0xfd, 0xfe, 0xa1, 0xb2, 0xc3}))
	withLen := func(b []byte, at int, n uint16) []byte {
		b = append([]byte(nil), b...)
		binary.NativeEndian.PutUint16(b[at:], n)
		return b
	}
	withMsgLen := func(b []byte, n uint32) []byte {
		b = append([]byte(nil), b...)
		binary.NativeEndian.PutUint32(b, n)
		return b
	}
	for name, msgs := range map[string][]byte{
		"a truncated header":             valid[:10],
		"a length beyond the dump":       withMsgLen(valid, uint32(len(valid)+8)),
		"a length shorter than a header": withMsgLen(valid, 8),
		"a body shorter than ifinfomsg":  nlMessage(testRTMNewLink, make([]byte, 8)),
		// The first attribute's header follows the netlink header and ifinfomsg, at byte 32.
		"an attribute shorter than its header": withLen(valid, 32, 2),
		"an attribute beyond its message":      withLen(valid, 32, 200),
		"an error message":                     nlMessage(testNLMsgError, make([]byte, 20)),
		// netlink_dump puts the dump callback's errno, negative, in NLMSG_DONE's body when the dump fails partway.
		"a dump the kernel ended with an error": concat(valid, nlMessage(testNLMsgDone, binary.NativeEndian.AppendUint32(nil, uint32(0xffffffea)))),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parsePermanentAddresses(msgs)
			if err == nil {
				t.Errorf("parse succeeded with %v, want an error", got)
			}
			if got != nil {
				t.Errorf("parse returned %v with its error, want nil", got)
			}
		})
	}
}

// Fewer than an attribute header's 4 bytes left at the end of a message are ignored, as syscall.ParseNetlinkRouteAttr ignores them.
func TestParsePermanentAddressesIgnoresATrailingPartialAttributeHeader(t *testing.T) {
	got, err := parsePermanentAddresses(nlMessage(testRTMNewLink, append(make([]byte, 16), 10, 0)))
	if err != nil || len(got) != 0 {
		t.Errorf("parse = %v, %v; want no addresses and no error", got, err)
	}
}

// Every prefix of a dump, whatever it cuts through, parses or fails without a panic.
func TestParsePermanentAddressesSurvivesEveryTruncation(t *testing.T) {
	dump := linkDump()
	for n := range len(dump) {
		_, _ = parsePermanentAddresses(dump[:n])
	}
}

func FuzzParsePermanentAddresses(f *testing.F) {
	f.Add(linkDump())
	f.Add(newLinkMessage(2, rtAttr(testIFLAPermAddr, nil)))
	f.Fuzz(func(_ *testing.T, msgs []byte) {
		_, _ = parsePermanentAddresses(msgs)
	})
}

// shortenPermDumpTimeout makes the dump's timeout d for the test.
func shortenPermDumpTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := permDumpTimeout
	permDumpTimeout = d
	t.Cleanup(func() { permDumpTimeout = old })
}

func TestBoundedPermanentAddresses(t *testing.T) {
	got, err := boundedPermanentAddresses(func() ([]byte, error) { return linkDump(), nil })
	if err != nil {
		t.Fatal(err)
	}
	if want := map[uint32]string{2: "3c:fd:fe:a1:b2:c3", 5: "b8:3f:d2:00:00:0a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("permanent addresses = %v, want %v", got, want)
	}

	_, err = boundedPermanentAddresses(func() ([]byte, error) { return nil, errors.New("operation not permitted") })
	if want := "permanent addresses: operation not permitted"; err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}

	_, err = boundedPermanentAddresses(func() ([]byte, error) { return nlMessage(testNLMsgError, make([]byte, 20)), nil })
	if err == nil || !strings.HasPrefix(err.Error(), "permanent addresses: ") {
		t.Errorf("error = %v, want one starting with %q", err, "permanent addresses: ")
	}
}

// A dump stuck in the kernel is abandoned at the timeout, and no other is started until it returns: at most one goroutine is ever stuck.
func TestBoundedPermanentAddressesAbandonsAStuckDump(t *testing.T) {
	shortenPermDumpTimeout(t, 50*time.Millisecond)
	release := make(chan struct{})
	// However the test ends, the abandoned dump is released and its goroutine has cleared permDumpRunning, so that no later test starts
	// with the flag set.
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		for deadline := time.Now().Add(5 * time.Second); permDumpRunning.Load(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Error("the abandoned dump's goroutine never cleared permDumpRunning")
				return
			}
		}
	})

	start := time.Now()
	_, err := boundedPermanentAddresses(func() ([]byte, error) {
		<-release
		return linkDump(), nil
	})
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("error = %v, want one wrapping os.ErrDeadlineExceeded", err)
	}
	if want := "permanent addresses: no reply: "; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error = %q, want it to start with %q", err, want)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the call took %v, want about the 50ms timeout", elapsed)
	}

	// The dump would run in its own goroutine.
	var called atomic.Bool
	_, err = boundedPermanentAddresses(func() ([]byte, error) {
		called.Store(true)
		return linkDump(), nil
	})
	if want := "permanent addresses: a previous request is still blocked in the kernel"; err == nil || err.Error() != want {
		t.Errorf("error while the first dump is stuck = %v, want %q", err, want)
	}
	if called.Load() {
		t.Error("a second dump was started while the first is still stuck")
	}

	close(release)
	// The abandoned dump's goroutine clears the flag as it returns, which the test cannot wait on directly.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := boundedPermanentAddresses(func() ([]byte, error) { return linkDump(), nil })
		if err == nil {
			if len(got) != 2 {
				t.Errorf("permanent addresses after the release = %v, want two", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still failing after the stuck dump returned: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

// withDumpIntr sets NLM_F_DUMP_INTR on the message at offset off of msgs.
func withDumpIntr(msgs []byte, off int) []byte {
	b := slices.Clone(msgs)
	binary.NativeEndian.PutUint16(b[off+6:], binary.NativeEndian.Uint16(b[off+6:])|0x10)
	return b
}

// The kernel flags a dump NLM_F_DUMP_INTR, on any of its messages, when links changed while it ran: it may have missed some, so its map is
// never used.
func TestParsePermanentAddressesRejectsAnInterruptedDump(t *testing.T) {
	dump := linkDump()
	doneAt := bytes.Index(dump, nlMessage(testNLMsgDone, make([]byte, 4)))
	for name, msgs := range map[string][]byte{
		"on a link message": withDumpIntr(dump, 0),
		"on NLMSG_DONE":     withDumpIntr(dump, doneAt),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parsePermanentAddresses(msgs)
			if !errors.Is(err, errDumpInterrupted) || got != nil {
				t.Errorf("parse = %v, %v; want no map and errDumpInterrupted", got, err)
			}
		})
	}
}

// An interrupted dump is asked for again, within the same request; one that keeps being interrupted is reported.
func TestBoundedPermanentAddressesRetriesAnInterruptedDump(t *testing.T) {
	calls := 0
	got, err := boundedPermanentAddresses(func() ([]byte, error) {
		calls++
		if calls == 1 {
			return withDumpIntr(linkDump(), 0), nil
		}
		return linkDump(), nil
	})
	if err != nil || calls != 2 || got[2] != "3c:fd:fe:a1:b2:c3" {
		t.Errorf("after one interrupted dump: %v, %v, %d calls; want the map, no error, 2 calls", got, err, calls)
	}

	calls = 0
	got, err = boundedPermanentAddresses(func() ([]byte, error) {
		calls++
		return withDumpIntr(linkDump(), 0), nil
	})
	if !errors.Is(err, errDumpInterrupted) || got != nil || calls != permDumpAttempts {
		t.Errorf("always interrupted: %v, %v, %d calls; want errDumpInterrupted after %d calls", got, err, calls, permDumpAttempts)
	}
	if !strings.HasPrefix(fmt.Sprint(err), "permanent addresses: ") {
		t.Errorf("error %q lacks the permanent addresses: prefix", err)
	}
}

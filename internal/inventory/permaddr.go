package inventory

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Netlink and rtnetlink values the parser needs. They are spelled out rather than taken from syscall, which defines them on Linux only, so
// that the parser builds and is tested on every GOOS.
const (
	nlmsgHdrLen     = 16     // struct nlmsghdr
	ifInfoMsgLen    = 16     // struct ifinfomsg
	rtAttrHdrLen    = 4      // struct rtattr
	nlmsgError      = 2      // NLMSG_ERROR
	nlmsgDone       = 3      // NLMSG_DONE
	nlmFDumpIntr    = 0x10   // NLM_F_DUMP_INTR: links changed while the dump ran
	rtmNewLink      = 16     // RTM_NEWLINK
	nlaTypeMask     = 0x3fff // NLA_TYPE_MASK: the type without NLA_F_NESTED and NLA_F_NET_BYTEORDER
	iflaPermAddress = 54     // IFLA_PERM_ADDRESS, Linux 5.6 and later; syscall does not define it
)

// permDumpTimeout is how long PermanentAddresses waits for the kernel's link dump, its retries included; tests shorten it.
var permDumpTimeout = 5 * time.Second

// permDumpAttempts is how many times a link dump the kernel flags as interrupted is asked for, the first time included, before the
// interruption is reported.
const permDumpAttempts = 3

// errDumpInterrupted is the parser's error for a dump the kernel flagged NLM_F_DUMP_INTR: links changed while it ran, so it may have missed
// some.
var errDumpInterrupted = errors.New("the dump was interrupted by link changes")

// permDumpRunning is set while a link dump runs, from its start until it returns, process-wide. The network probe, its only caller, runs
// one at a time, so a dump still running when a probe asks for another is one whose caller gave up on it: it is blocked in the kernel, such
// as behind a stalled driver or a long-held RTNL, and nothing can cancel it. Until it returns, no other is started, so at most one goroutine
// is ever stuck.
var permDumpRunning atomic.Bool

// errPermDumpBusy is PermanentAddresses' error while an abandoned dump is still blocked in the kernel.
var errPermDumpBusy = errors.New("permanent addresses: a previous request is still blocked in the kernel")

// boundedPermanentAddresses runs dump, which returns the kernel's RTM_GETLINK dump, in its own goroutine, waits for it up to
// permDumpTimeout, and parses its messages into each interface's permanent address by index. A dump that does not return in time is
// abandoned with an error wrapping os.ErrDeadlineExceeded; until it returns, every call fails at once with errPermDumpBusy.
func boundedPermanentAddresses(dump func() ([]byte, error)) (map[uint32]string, error) {
	if !permDumpRunning.CompareAndSwap(false, true) {
		return nil, errPermDumpBusy
	}
	type result struct {
		addrs map[uint32]string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		// A dump the kernel flags as interrupted may lack links; another, a moment later, is very likely to be whole.
		for range permDumpAttempts {
			msgs, err := dump()
			if err != nil {
				r = result{err: err}
				break
			}
			r.addrs, r.err = parsePermanentAddresses(msgs)
			if !errors.Is(r.err, errDumpInterrupted) {
				break
			}
		}
		// Cleared before the result is delivered, so that a caller that got it can dump again at once.
		permDumpRunning.Store(false)
		done <- r
	}()
	timer := time.NewTimer(permDumpTimeout)
	defer timer.Stop()
	var r result
	select {
	case r = <-done:
	case <-timer.C:
		return nil, fmt.Errorf("permanent addresses: no reply: %w", os.ErrDeadlineExceeded)
	}
	if r.err != nil {
		return nil, fmt.Errorf("permanent addresses: %w", r.err)
	}
	return r.addrs, nil
}

// errMalformedDump is the parser's error for a dump whose lengths do not add up.
var errMalformedDump = errors.New("malformed netlink message")

// parsePermanentAddresses reads the RTM_NEWLINK messages of a link dump, in native byte order, up to NLMSG_DONE, into each interface's
// IFLA_PERM_ADDRESS by index, formatted as sysfs formats an address: lower-case hex bytes separated by colons. An interface without the
// attribute, as the kernel sends for one with no permanent address, such as a virtual interface, or with an all-zero one, is left out.
// A message or attribute whose length runs past what holds it, an NLMSG_ERROR, an NLMSG_DONE carrying an error, or a link message too short
// for its ifinfomsg, fails the whole dump, with no map. So does NLM_F_DUMP_INTR on any message, with errDumpInterrupted: the kernel sets it
// when links changed while the dump ran, which may have left some out.
func parsePermanentAddresses(msgs []byte) (map[uint32]string, error) {
	addrs := map[uint32]string{}
	interrupted := false
	for len(msgs) > 0 {
		if len(msgs) < nlmsgHdrLen {
			return nil, fmt.Errorf("%w: %d bytes left, short of a header", errMalformedDump, len(msgs))
		}
		n := binary.NativeEndian.Uint32(msgs[0:4])
		typ := binary.NativeEndian.Uint16(msgs[4:6])
		interrupted = interrupted || binary.NativeEndian.Uint16(msgs[6:8])&nlmFDumpIntr != 0
		if n < nlmsgHdrLen || uint64(n) > uint64(len(msgs)) {
			return nil, fmt.Errorf("%w: length %d with %d bytes left", errMalformedDump, n, len(msgs))
		}
		body := msgs[nlmsgHdrLen:n]
		msgs = msgs[min(nlmsgAlign(n), len(msgs)):]
		switch typ {
		case nlmsgDone:
			// netlink_dump puts the dump callback's negative errno here when the dump fails partway; what came before it is incomplete.
			if len(body) >= 4 {
				if errno := int32(binary.NativeEndian.Uint32(body[0:4])); errno < 0 {
					return nil, fmt.Errorf("the kernel ended the dump with errno %d", -errno)
				}
			}
			if interrupted {
				return nil, errDumpInterrupted
			}
			return addrs, nil
		case nlmsgError:
			return nil, errors.New("the kernel answered with an error")
		case rtmNewLink:
		default:
			continue
		}
		if len(body) < ifInfoMsgLen {
			return nil, fmt.Errorf("%w: link message of %d bytes", errMalformedDump, len(body))
		}
		index := binary.NativeEndian.Uint32(body[4:8])
		perm, err := permAddressAttr(body[ifInfoMsgLen:])
		if err != nil {
			return nil, err
		}
		if perm != "" {
			addrs[index] = perm
		}
	}
	if interrupted {
		return nil, errDumpInterrupted
	}
	return addrs, nil
}

// permAddressAttr finds IFLA_PERM_ADDRESS among a link message's attributes, returning it formatted, or "" when it is absent, empty or all
// zeros. Fewer bytes than an attribute header left at the end are ignored, as syscall.ParseNetlinkRouteAttr ignores them.
func permAddressAttr(attrs []byte) (string, error) {
	perm := ""
	for len(attrs) >= rtAttrHdrLen {
		n := binary.NativeEndian.Uint16(attrs[0:2])
		typ := binary.NativeEndian.Uint16(attrs[2:4]) & nlaTypeMask
		if n < rtAttrHdrLen || int(n) > len(attrs) {
			return "", fmt.Errorf("%w: attribute length %d with %d bytes left", errMalformedDump, n, len(attrs))
		}
		if typ == iflaPermAddress {
			perm = formatPermAddress(attrs[rtAttrHdrLen:n])
		}
		attrs = attrs[min(nlmsgAlign(uint32(n)), len(attrs)):]
	}
	return perm, nil
}

// formatPermAddress formats addr as sysfs does, or returns "" for an empty or all-zero address, which is none.
func formatPermAddress(addr []byte) string {
	for _, b := range addr {
		if b != 0 {
			return net.HardwareAddr(addr).String()
		}
	}
	return ""
}

// isZeroMAC reports whether a formatted address, such as sysfs's perm_hwaddr, is empty or all zeros.
func isZeroMAC(addr string) bool {
	return strings.Trim(addr, "0:") == ""
}

// nlmsgAlign rounds n up to netlink's 4-byte alignment, as an int for slicing.
func nlmsgAlign(n uint32) int {
	return int((uint64(n) + 3) &^ 3)
}

// Package ethtool asks the kernel's ethtool generic-netlink family about network interfaces: offload features, link modes, port and FEC.
package ethtool

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Netlink framing, from include/uapi/linux/netlink.h. Every multi-byte field is in the host's byte order.
const (
	nlmsgHdrLen = 16 // nlmsghdr: len u32, type u16, flags u16, seq u32, pid u32
	nlaHdrLen   = 4  // nlattr: len u16, type u16

	nlmsgError = 2 // NLMSG_ERROR: an int32 error code (0 is an ACK) then the echoed request header

	nlaFNested       = 0x8000 // NLA_F_NESTED
	nlaFNetByteorder = 0x4000 // NLA_F_NET_BYTEORDER
	nlaTypeMask      = ^uint16(nlaFNested | nlaFNetByteorder)
)

// attr is one netlink attribute. data aliases the buffer it was parsed from.
type attr struct {
	typ  uint16 // NLA_F_NESTED and NLA_F_NET_BYTEORDER masked off
	data []byte
}

// message is one netlink message: its header fields and payload (after the 16-byte nlmsghdr). data aliases the buffer it was parsed from.
type message struct {
	typ   uint16
	flags uint16
	seq   uint32
	data  []byte
}

// align4 rounds n up to the 4-byte boundary that netlink messages and attributes start on.
func align4(n int) int {
	return (n + 3) &^ 3
}

// The agent sends two messages, familyRequest and request, and builds both with the header writers below. Each takes the length as its field's
// own type, so a length must be converted to reach one, and no conversion can truncate: familyRequest's lengths are constants, whose
// conversion the compiler checks, and request's are bounded by its check of the interface name. There is no general encoder that a length
// too long for its field could reach.

// appendMessageHeader appends the header of a message of n bytes, header and payload but not padding, as the kernel's NLMSG_LENGTH counts,
// with a zero port id.
func appendMessageHeader(b []byte, typ, flags uint16, seq, n uint32) []byte {
	b = binary.NativeEndian.AppendUint32(b, n)
	b = binary.NativeEndian.AppendUint16(b, typ)
	b = binary.NativeEndian.AppendUint16(b, flags)
	b = binary.NativeEndian.AppendUint32(b, seq)
	return binary.NativeEndian.AppendUint32(b, 0)
}

// appendAttrHeader appends the header of an attribute of n bytes, header and data but not padding.
func appendAttrHeader(b []byte, typ, n uint16) []byte {
	b = binary.NativeEndian.AppendUint16(b, n)
	return binary.NativeEndian.AppendUint16(b, typ)
}

// appendU32Attr appends a 32-bit attribute to b.
func appendU32Attr(b []byte, typ uint16, v uint32) []byte {
	return binary.NativeEndian.AppendUint32(appendAttrHeader(b, typ, nlaHdrLen+4), v)
}

// parseMessages splits a receive buffer, which may hold several messages, into its messages.
func parseMessages(b []byte) ([]message, error) {
	var msgs []message
	for off := 0; off < len(b); {
		rest := b[off:]
		if len(rest) < nlmsgHdrLen {
			return nil, fmt.Errorf("netlink message at offset %d: %d bytes left, fewer than a header", off, len(rest))
		}
		n := binary.NativeEndian.Uint32(rest[0:])
		if n < nlmsgHdrLen || uint64(n) > uint64(len(rest)) {
			return nil, fmt.Errorf("netlink message at offset %d: length %d outside %d..%d", off, n, nlmsgHdrLen, len(rest))
		}
		msgs = append(msgs, message{
			typ:   binary.NativeEndian.Uint16(rest[4:]),
			flags: binary.NativeEndian.Uint16(rest[6:]),
			seq:   binary.NativeEndian.Uint32(rest[8:]),
			data:  rest[nlmsgHdrLen:n:n],
		})
		// The last message of a buffer may end without its padding, so step past the padding only as far as the buffer goes.
		off += min(align4(int(n)), len(rest))
	}
	return msgs, nil
}

// parseAttributes splits an attribute stream (a message payload after any family header, or a nest's data) into its attributes.
func parseAttributes(b []byte) ([]attr, error) {
	var attrs []attr
	for off := 0; off < len(b); {
		rest := b[off:]
		if len(rest) < nlaHdrLen {
			return nil, fmt.Errorf("netlink attribute at offset %d: %d bytes left, fewer than a header", off, len(rest))
		}
		n := int(binary.NativeEndian.Uint16(rest[0:]))
		if n < nlaHdrLen || n > len(rest) {
			return nil, fmt.Errorf("netlink attribute at offset %d: length %d outside %d..%d", off, n, nlaHdrLen, len(rest))
		}
		attrs = append(attrs, attr{
			typ:  binary.NativeEndian.Uint16(rest[2:]) & nlaTypeMask,
			data: rest[nlaHdrLen:n:n],
		})
		off += min(align4(n), len(rest))
	}
	return attrs, nil
}

func (a attr) u8() (uint8, error) {
	if len(a.data) != 1 {
		return 0, a.sizeError(1)
	}
	return a.data[0], nil
}

func (a attr) u16() (uint16, error) {
	if len(a.data) != 2 {
		return 0, a.sizeError(2)
	}
	return binary.NativeEndian.Uint16(a.data), nil
}

func (a attr) u32() (uint32, error) {
	if len(a.data) != 4 {
		return 0, a.sizeError(4)
	}
	return binary.NativeEndian.Uint32(a.data), nil
}

// sizeError reports a scalar read of the wrong width: the kernel sends each attribute at exactly its declared size, so any other length means the
// attribute is not the type the caller thinks it is.
func (a attr) sizeError(want int) error {
	return fmt.Errorf("netlink attribute %d: %d bytes, want %d", a.typ, len(a.data), want)
}

// str returns a string attribute's value with its trailing NULs trimmed.
func (a attr) str() string {
	return string(bytes.TrimRight(a.data, "\x00"))
}

// errorCode returns an NLMSG_ERROR message's error code: 0 for an ACK, otherwise a negative errno.
func errorCode(m message) (int32, error) {
	if m.typ != nlmsgError {
		return 0, fmt.Errorf("netlink message type %d is not NLMSG_ERROR", m.typ)
	}
	if len(m.data) < 4 {
		return 0, errors.New("NLMSG_ERROR payload is shorter than its error code")
	}
	return int32(binary.NativeEndian.Uint32(m.data)), nil
}

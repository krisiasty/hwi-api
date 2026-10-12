package ethtool

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

// testAttr, testNested, testStringAttr and testMessage build fixtures with
// the encoders, failing the test on the error each returns for an
// attribute or message too long for its length field. No fixture comes
// near that limit.
func testAttr(t testing.TB, typ uint16, data []byte) []byte {
	t.Helper()
	b, err := encodeAttr(typ, data)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testNested(t testing.TB, typ uint16, attrs ...[]byte) []byte {
	t.Helper()
	b, err := encodeNested(typ, attrs...)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testStringAttr(t testing.TB, typ uint16, s string) []byte {
	t.Helper()
	b, err := stringAttr(typ, s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testMessage(t testing.TB, typ, flags uint16, seq uint32, payload []byte) []byte {
	t.Helper()
	b, err := encodeMessage(typ, flags, seq, payload)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An attribute too long for nla_len, a uint16, is an error: never a
// truncated length, which would corrupt the request, nor a panic.
func TestEncodersRefuseWhatTheirLengthFieldCannotHold(t *testing.T) {
	if _, err := encodeAttr(1, make([]byte, math.MaxUint16-nlaHdrLen)); err != nil {
		t.Errorf("an attribute at the nla_len limit: %v, want it encoded", err)
	}
	if b, err := encodeAttr(1, make([]byte, math.MaxUint16-nlaHdrLen+1)); err == nil {
		t.Errorf("an attribute one byte over the nla_len limit encoded to %d bytes, want an error", len(b))
	}
	// Attributes that fit on their own can be too long together.
	half := testAttr(t, 1, make([]byte, 40000))
	if b, err := encodeNested(2, half, half); err == nil {
		t.Errorf("a nest of two 40004-byte attributes encoded to %d bytes, want an error", len(b))
	}
	if b, err := stringAttr(3, strings.Repeat("x", math.MaxUint16)); err == nil {
		t.Errorf("a %d-byte string attribute encoded to %d bytes, want an error", math.MaxUint16, len(b))
	}
}

// u32Attr skips the length check, since its attribute is always 8 bytes,
// but must encode exactly as encodeAttr does.
func TestU32AttrEncodesAsEncodeAttr(t *testing.T) {
	if got, want := u32Attr(5, 0x01020304), testAttr(t, 5, binary.NativeEndian.AppendUint32(nil, 0x01020304)); !bytes.Equal(got, want) {
		t.Errorf("u32Attr = %x, encodeAttr = %x", got, want)
	}
}

func TestMessageRoundTrip(t *testing.T) {
	payload := append(u32Attr(1, 7), testStringAttr(t, 2, "eth0")...)
	b := testMessage(t, 0x1b, 0x5, 42, payload)
	if len(b)%4 != 0 {
		t.Fatalf("encoded message is %d bytes, not 4-byte aligned", len(b))
	}

	msgs, err := parseMessages(b)
	if err != nil {
		t.Fatalf("parseMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.typ != 0x1b || m.flags != 0x5 || m.seq != 42 {
		t.Errorf("header = type %#x flags %#x seq %d, want 0x1b 0x5 42", m.typ, m.flags, m.seq)
	}
	if !bytes.Equal(m.data, payload) {
		t.Errorf("payload = %x, want %x", m.data, payload)
	}
}

func TestParseMessagesUnalignedPayload(t *testing.T) {
	// A 5-byte payload makes nlmsg_len 21; the encoder pads the buffer to 24 and the parser hands back exactly the 5 bytes.
	b := testMessage(t, 1, 0, 1, []byte{1, 2, 3, 4, 5})
	if got := binary.NativeEndian.Uint32(b); got != 21 {
		t.Errorf("nlmsg_len = %d, want 21 (header plus payload, padding excluded)", got)
	}
	if len(b) != 24 {
		t.Errorf("buffer is %d bytes, want 24", len(b))
	}
	msgs, err := parseMessages(append(b, testMessage(t, 2, 0, 2, nil)...))
	if err != nil {
		t.Fatalf("parseMessages: %v", err)
	}
	if len(msgs) != 2 || !bytes.Equal(msgs[0].data, []byte{1, 2, 3, 4, 5}) || msgs[1].typ != 2 {
		t.Errorf("got %+v, want the 5-byte payload then a type-2 message", msgs)
	}
}

func TestParseMessagesTwoInOneBuffer(t *testing.T) {
	b := append(testMessage(t, 0x1b, 0, 1, u32Attr(1, 10)), testMessage(t, 3, 2, 1, []byte{0, 0, 0, 0})...)
	msgs, err := parseMessages(b)
	if err != nil {
		t.Fatalf("parseMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].typ != 0x1b || msgs[1].typ != 3 || msgs[1].flags != 2 {
		t.Errorf("types/flags = %#x, %#x/%#x; want 0x1b, 3/2", msgs[0].typ, msgs[1].typ, msgs[1].flags)
	}
	attrs, err := parseAttributes(msgs[0].data)
	if err != nil || len(attrs) != 1 {
		t.Fatalf("first message attributes = %v, %v; want one", attrs, err)
	}
	if v, err := attrs[0].u32(); err != nil || v != 10 {
		t.Errorf("first message u32 = %d, %v; want 10", v, err)
	}
}

func TestParseMessagesRejectsBadLength(t *testing.T) {
	valid := testMessage(t, 1, 0, 1, u32Attr(1, 1))
	withLen := func(n uint32) []byte {
		b := bytes.Clone(valid)
		binary.NativeEndian.PutUint32(b, n)
		return b
	}
	tests := []struct {
		name string
		b    []byte
	}{
		{"length below the header", withLen(15)},
		{"length zero", withLen(0)},
		{"length past the buffer", withLen(uint32(len(valid) + 4))},
		{"buffer shorter than a header", valid[:10]},
		{"trailing partial header", append(bytes.Clone(valid), 1, 2, 3, 4)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if msgs, err := parseMessages(tt.b); err == nil {
				t.Errorf("parseMessages = %+v, nil; want an error", msgs)
			}
		})
	}
}

func TestParseMessagesEmpty(t *testing.T) {
	msgs, err := parseMessages(nil)
	if err != nil || len(msgs) != 0 {
		t.Errorf("parseMessages(nil) = %v, %v; want no messages and no error", msgs, err)
	}
}

func TestAttributePadding(t *testing.T) {
	five := testAttr(t, 1, []byte{1, 2, 3, 4, 5})
	if len(five) != 12 {
		t.Fatalf("5-byte attribute encodes to %d bytes, want 12 (4 header, 5 data, 3 padding)", len(five))
	}
	if got := binary.NativeEndian.Uint16(five); got != 9 {
		t.Errorf("nla_len = %d, want 9 (header plus data, padding excluded)", got)
	}
	if !bytes.Equal(five[9:], []byte{0, 0, 0}) {
		t.Errorf("padding = %x, want zeros", five[9:])
	}

	attrs, err := parseAttributes(append(five, u32Attr(2, 0xdeadbeef)...))
	if err != nil {
		t.Fatalf("parseAttributes: %v", err)
	}
	if len(attrs) != 2 {
		t.Fatalf("got %d attributes, want 2", len(attrs))
	}
	if attrs[0].typ != 1 || !bytes.Equal(attrs[0].data, []byte{1, 2, 3, 4, 5}) {
		t.Errorf("first attribute = %+v, want type 1 with the 5 data bytes", attrs[0])
	}
	if v, err := attrs[1].u32(); attrs[1].typ != 2 || err != nil || v != 0xdeadbeef {
		t.Errorf("second attribute = type %d u32 %#x, %v; want type 2 0xdeadbeef", attrs[1].typ, v, err)
	}
}

func TestAttributeLastUnpadded(t *testing.T) {
	// The final attribute of a buffer may end without its padding; the parser must not demand bytes that are not there.
	b := testAttr(t, 1, []byte{9})[:5]
	attrs, err := parseAttributes(b)
	if err != nil || len(attrs) != 1 {
		t.Fatalf("parseAttributes = %v, %v; want one attribute", attrs, err)
	}
	if v, err := attrs[0].u8(); err != nil || v != 9 {
		t.Errorf("u8 = %d, %v; want 9", v, err)
	}
}

func TestNestedAttributes(t *testing.T) {
	b := testNested(t, 1, u32Attr(1, 3), testStringAttr(t, 2, "eno1"), testNested(t, 3, u32Attr(1, 7)))
	if got := binary.NativeEndian.Uint16(b[2:]); got != 1|nlaFNested {
		t.Errorf("nest type on the wire = %#x, want %#x", got, 1|nlaFNested)
	}

	outer, err := parseAttributes(b)
	if err != nil || len(outer) != 1 {
		t.Fatalf("parseAttributes(outer) = %v, %v; want one", outer, err)
	}
	if outer[0].typ != 1 {
		t.Errorf("nest typ = %#x, want 1 (NLA_F_NESTED masked off)", outer[0].typ)
	}
	inner, err := parseAttributes(outer[0].data)
	if err != nil || len(inner) != 3 {
		t.Fatalf("parseAttributes(inner) = %v, %v; want three", inner, err)
	}
	if v, err := inner[0].u32(); err != nil || v != 3 {
		t.Errorf("inner u32 = %d, %v; want 3", v, err)
	}
	if s := inner[1].str(); s != "eno1" {
		t.Errorf("inner str = %q, want eno1", s)
	}
	if inner[2].typ != 3 {
		t.Errorf("nested nest typ = %#x, want 3", inner[2].typ)
	}
	deepest, err := parseAttributes(inner[2].data)
	if err != nil || len(deepest) != 1 {
		t.Fatalf("parseAttributes(deepest) = %v, %v; want one", deepest, err)
	}
	if v, err := deepest[0].u32(); err != nil || v != 7 {
		t.Errorf("deepest u32 = %d, %v; want 7", v, err)
	}
}

func TestAttributeTypeFlagsMasked(t *testing.T) {
	b := testAttr(t, 5|nlaFNested|nlaFNetByteorder, []byte{1, 2, 3, 4})
	attrs, err := parseAttributes(b)
	if err != nil || len(attrs) != 1 {
		t.Fatalf("parseAttributes = %v, %v; want one", attrs, err)
	}
	if attrs[0].typ != 5 {
		t.Errorf("typ = %#x, want 5", attrs[0].typ)
	}
}

func TestParseAttributesRejectsOverrun(t *testing.T) {
	valid := u32Attr(1, 1)
	withLen := func(n uint16) []byte {
		b := bytes.Clone(valid)
		binary.NativeEndian.PutUint16(b, n)
		return b
	}
	tests := []struct {
		name string
		b    []byte
	}{
		{"length below the header", withLen(3)},
		{"length zero", withLen(0)},
		{"length past the buffer", withLen(9)},
		{"buffer shorter than a header", valid[:3]},
		{"trailing partial header", append(bytes.Clone(valid), 8, 0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if attrs, err := parseAttributes(tt.b); err == nil {
				t.Errorf("parseAttributes = %+v, nil; want an error", attrs)
			}
		})
	}
}

func TestParseAttributesRejectsChildOverrunningParent(t *testing.T) {
	// The child's nla_len of 12 runs 4 bytes past its 8-byte parent into the attribute that follows the nest; those bytes exist in the buffer, so
	// only the parent's bound stops the child from swallowing them.
	nest := testNested(t, 1, u32Attr(2, 1))
	binary.NativeEndian.PutUint16(nest[nlaHdrLen:], 12)
	outer, err := parseAttributes(append(nest, u32Attr(3, 5)...))
	if err != nil || len(outer) != 2 {
		t.Fatalf("parseAttributes(outer) = %v, %v; want the nest and the attribute after it", outer, err)
	}
	if inner, err := parseAttributes(outer[0].data); err == nil {
		t.Errorf("parseAttributes(nest) = %+v, nil; want an error for the child overrunning its parent", inner)
	}
}

func TestAttributeScalars(t *testing.T) {
	if v, err := (attr{data: []byte{0xab}}).u8(); err != nil || v != 0xab {
		t.Errorf("u8 = %#x, %v; want 0xab", v, err)
	}
	u16 := binary.NativeEndian.AppendUint16(nil, 0xbeef)
	if v, err := (attr{data: u16}).u16(); err != nil || v != 0xbeef {
		t.Errorf("u16 = %#x, %v; want 0xbeef", v, err)
	}
	if v, err := (attr{data: binary.NativeEndian.AppendUint32(nil, 0xcafef00d)}).u32(); err != nil || v != 0xcafef00d {
		t.Errorf("u32 = %#x, %v; want 0xcafef00d", v, err)
	}

	wrong := []struct {
		name string
		get  func() error
	}{
		{"u8 of 0 bytes", func() error { _, err := (attr{}).u8(); return err }},
		{"u8 of 2 bytes", func() error { _, err := (attr{data: []byte{1, 2}}).u8(); return err }},
		{"u16 of 1 byte", func() error { _, err := (attr{data: []byte{1}}).u16(); return err }},
		{"u32 of 3 bytes", func() error { _, err := (attr{data: []byte{1, 2, 3}}).u32(); return err }},
		{"u32 of 8 bytes", func() error { _, err := (attr{data: make([]byte, 8)}).u32(); return err }},
	}
	for _, tt := range wrong {
		if err := tt.get(); err == nil {
			t.Errorf("%s: no error", tt.name)
		}
	}
}

func TestStringAttr(t *testing.T) {
	b := testStringAttr(t, 2, "ethtool")
	if got := binary.NativeEndian.Uint16(b); got != 4+8 {
		t.Errorf("nla_len = %d, want 12 (header plus 7 characters plus NUL)", got)
	}
	attrs, err := parseAttributes(b)
	if err != nil || len(attrs) != 1 {
		t.Fatalf("parseAttributes = %v, %v; want one", attrs, err)
	}
	if attrs[0].data[len(attrs[0].data)-1] != 0 {
		t.Errorf("data %x is not NUL-terminated", attrs[0].data)
	}
	if s := attrs[0].str(); s != "ethtool" {
		t.Errorf("str = %q, want ethtool", s)
	}
	if s := (attr{data: []byte("eth0\x00\x00\x00")}).str(); s != "eth0" {
		t.Errorf("str with several NULs = %q, want eth0", s)
	}
	if s := (attr{}).str(); s != "" {
		t.Errorf("str of empty = %q, want empty", s)
	}
}

func TestErrorCode(t *testing.T) {
	request := testMessage(t, 0x1b, 1|4, 9, nil)
	errMsg := func(code int32) message {
		payload := binary.NativeEndian.AppendUint32(nil, uint32(code))
		payload = append(payload, request[:16]...)
		msgs, err := parseMessages(testMessage(t, nlmsgError, 0, 9, payload))
		if err != nil || len(msgs) != 1 {
			t.Fatalf("parseMessages = %v, %v", msgs, err)
		}
		return msgs[0]
	}

	if code, err := errorCode(errMsg(0)); err != nil || code != 0 {
		t.Errorf("errorCode(ACK) = %d, %v; want 0", code, err)
	}
	if code, err := errorCode(errMsg(-95)); err != nil || code != -95 {
		t.Errorf("errorCode(EOPNOTSUPP) = %d, %v; want -95", code, err)
	}
	if _, err := errorCode(message{typ: nlmsgError, data: []byte{1, 2, 3}}); err == nil {
		t.Error("errorCode of a 3-byte NLMSG_ERROR payload: no error")
	}
	if _, err := errorCode(message{typ: 0x1b, data: make([]byte, 20)}); err == nil {
		t.Error("errorCode of a non-error message: no error")
	}
}

func FuzzParseMessages(f *testing.F) {
	f.Add(testMessage(f, 0x1b, 0, 1, testNested(f, 1, u32Attr(1, 2), testStringAttr(f, 2, "eth0"))))
	f.Add(append(testMessage(f, 1, 0, 1, []byte{1, 2, 3, 4, 5}), testMessage(f, 3, 2, 1, nil)...))
	f.Add(testMessage(f, nlmsgError, 0, 1, binary.NativeEndian.AppendUint32(nil, uint32(0xffffffa1))))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		msgs, err := parseMessages(b)
		if err != nil {
			return
		}
		for _, m := range msgs {
			_, _ = errorCode(m)
			walkAttributes(m.data, 0)
		}
	})
}

// walkAttributes parses b and every attribute's data as a nest, as a reply parser probing an unknown attribute would, reading each scalar on the way.
func walkAttributes(b []byte, depth int) {
	if depth > 8 {
		return
	}
	attrs, err := parseAttributes(b)
	if err != nil {
		return
	}
	for _, a := range attrs {
		_, _ = a.u8()
		_, _ = a.u16()
		_, _ = a.u32()
		_ = a.str()
		walkAttributes(a.data, depth+1)
	}
}

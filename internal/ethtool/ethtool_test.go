package ethtool

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"syscall"
	"testing"
)

// genlReply returns the messages of a one-message generic-netlink reply carrying cmd and attrs, as the kernel sends it to family 0x1b.
func genlReply(t *testing.T, cmd uint8, attrs ...[]byte) []message {
	t.Helper()
	payload := append([]byte{cmd, genlVersion, 0, 0}, bytes.Join(attrs, nil)...)
	msgs, err := parseMessages(testMessage(t, 0x1b, 0, 1, payload))
	if err != nil {
		t.Fatalf("parseMessages: %v", err)
	}
	return msgs
}

// errorReply returns the messages of an NLMSG_ERROR reply with code (0 for an ACK, otherwise a negative errno).
func errorReply(t *testing.T, code int32) []message {
	t.Helper()
	payload := binary.NativeEndian.AppendUint32(nil, uint32(code))
	payload = append(payload, testMessage(t, 0x1b, nlmFRequest, 1, nil)...) // the echoed request header
	msgs, err := parseMessages(testMessage(t, nlmsgError, 0, 1, payload))
	if err != nil {
		t.Fatalf("parseMessages: %v", err)
	}
	return msgs
}

func u8Attr(t testing.TB, typ uint16, v uint8) []byte {
	t.Helper()
	return testAttr(t, typ, []byte{v})
}

func TestFamilyRequest(t *testing.T) {
	req, err := familyRequest(7)
	if err != nil {
		t.Fatalf("familyRequest: %v", err)
	}
	msgs, err := parseMessages(req)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("parseMessages = %v, %v; want one message", msgs, err)
	}
	m := msgs[0]
	if m.typ != genlIDCtrl || m.flags != nlmFRequest || m.seq != 7 {
		t.Errorf("header = type %#x flags %#x seq %d; want %#x %#x 7", m.typ, m.flags, m.seq, genlIDCtrl, nlmFRequest)
	}
	if len(m.data) < genlHdrLen || m.data[0] != ctrlCmdGetFamily {
		t.Fatalf("genlmsghdr = %x, want cmd %d", m.data, ctrlCmdGetFamily)
	}
	attrs, err := parseAttributes(m.data[genlHdrLen:])
	if err != nil || len(attrs) != 1 {
		t.Fatalf("parseAttributes = %v, %v; want one", attrs, err)
	}
	if attrs[0].typ != ctrlAttrFamilyName || attrs[0].str() != "ethtool" {
		t.Errorf("attribute = type %d %q; want type %d \"ethtool\"", attrs[0].typ, attrs[0].str(), ctrlAttrFamilyName)
	}
}

func TestParseFamilyID(t *testing.T) {
	reply := genlReply(t, ctrlCmdNewFamily, testStringAttr(t, ctrlAttrFamilyName, "ethtool"),
		testAttr(t, ctrlAttrFamilyID, binary.NativeEndian.AppendUint16(nil, 0x1b)))
	if id, err := parseFamilyID(reply); err != nil || id != 0x1b {
		t.Errorf("parseFamilyID = %#x, %v; want 0x1b", id, err)
	}
	if id, err := parseFamilyID(errorReply(t, -2)); !errors.Is(err, ErrNoFamily) {
		t.Errorf("parseFamilyID(ENOENT) = %#x, %v; want ErrNoFamily", id, err)
	}
	// The payload's ethtool_error quotes it: a kernel too old and one built without CONFIG_ETHTOOL_NETLINK both lack the family.
	if want := "the kernel has no ethtool netlink family (Linux 5.6+ with CONFIG_ETHTOOL_NETLINK)"; ErrNoFamily.Error() != want {
		t.Errorf("ErrNoFamily = %q, want %q", ErrNoFamily, want)
	}
	if id, err := parseFamilyID(errorReply(t, -1)); !errors.Is(err, syscall.EPERM) {
		t.Errorf("parseFamilyID(EPERM) = %#x, %v; want EPERM", id, err)
	}
	if id, err := parseFamilyID(genlReply(t, ctrlCmdNewFamily, testStringAttr(t, ctrlAttrFamilyName, "ethtool"))); err == nil {
		t.Errorf("parseFamilyID without a family id = %#x, nil; want an error", id)
	}
}

func TestRequest(t *testing.T) {
	req, err := request(0x1b, ethtoolMsgFeaturesGet, 9, 4, "eth4")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	msgs, err := parseMessages(req)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("parseMessages = %v, %v; want one message", msgs, err)
	}
	m := msgs[0]
	if m.typ != 0x1b || m.flags != nlmFRequest || m.seq != 9 {
		t.Errorf("header = type %#x flags %#x seq %d; want 0x1b %#x 9", m.typ, m.flags, m.seq, nlmFRequest)
	}
	if !bytes.Equal(m.data[:genlHdrLen], []byte{ethtoolMsgFeaturesGet, 1, 0, 0}) {
		t.Errorf("genlmsghdr = %x, want cmd %d version 1 reserved 0", m.data[:genlHdrLen], ethtoolMsgFeaturesGet)
	}
	attrs, err := parseAttributes(m.data[genlHdrLen:])
	if err != nil || len(attrs) != 1 || attrs[0].typ != ethtoolAHeader {
		t.Fatalf("parseAttributes = %v, %v; want only the header nest", attrs, err)
	}
	if got := binary.NativeEndian.Uint16(m.data[genlHdrLen+2:]); got&nlaFNested == 0 {
		t.Errorf("header nest type on the wire = %#x, want NLA_F_NESTED set", got)
	}
	header, err := parseAttributes(attrs[0].data)
	if err != nil || len(header) != 3 {
		t.Fatalf("header nest = %v, %v; want three attributes", header, err)
	}
	if v, err := header[0].u32(); header[0].typ != ethtoolAHeaderDevIndex || err != nil || v != 4 {
		t.Errorf("header[0] = type %d u32 %d, %v; want DEV_INDEX 4", header[0].typ, v, err)
	}
	// Both the index and the name, so that the kernel answers ENODEV, rather than about another interface, when they no longer match.
	if header[1].typ != ethtoolAHeaderDevName || !bytes.Equal(header[1].data, []byte("eth4\x00")) {
		t.Errorf("header[1] = type %d %q; want DEV_NAME \"eth4\" NUL-terminated", header[1].typ, header[1].data)
	}
	if v, err := header[2].u32(); header[2].typ != ethtoolAHeaderFlags || err != nil || v != 0 {
		t.Errorf("header[2] = type %d u32 %d, %v; want FLAGS 0 (verbose bitsets)", header[2].typ, v, err)
	}
}

func TestReplyError(t *testing.T) {
	tests := []struct {
		code int32
		want error
	}{
		{-95, ErrNotSupported},
		{-19, ErrNoDevice},
		{-1, syscall.EPERM},
	}
	for _, tt := range tests {
		if err := replyError(errorReply(t, tt.code)); !errors.Is(err, tt.want) {
			t.Errorf("replyError(%d) = %v, want %v", tt.code, err, tt.want)
		}
	}
	if err := replyError(errorReply(t, 0)); err != nil {
		t.Errorf("replyError(ACK) = %v, want nil", err)
	}
	if err := replyError(genlReply(t, ethtoolMsgFeaturesGetReply)); err != nil {
		t.Errorf("replyError(reply without NLMSG_ERROR) = %v, want nil", err)
	}
	// An ACK before the error does not hide it.
	if err := replyError(append(errorReply(t, 0), errorReply(t, -19)...)); !errors.Is(err, ErrNoDevice) {
		t.Errorf("replyError(ACK, ENODEV) = %v, want ErrNoDevice", err)
	}
	if err := replyError([]message{{typ: nlmsgError, data: []byte{1}}}); err == nil {
		t.Error("replyError of a truncated NLMSG_ERROR: no error")
	}
}

func TestParseFeatures(t *testing.T) {
	reply := genlReply(t, ethtoolMsgFeaturesGetReply,
		testNested(t, ethtoolAHeader, u32Attr(ethtoolAHeaderDevIndex, 4)),
		verboseBitset(t, ethtoolAFeaturesHW, true, testBit{index: 1, name: "rx-checksum"}, testBit{index: 2, name: "rx-gro-hw"}),
		verboseBitset(t, ethtoolAFeaturesWanted, true, testBit{index: 1, name: "rx-checksum"}),
		verboseBitset(t, ethtoolAFeaturesActive, true, testBit{index: 1, name: "rx-checksum"}, testBit{index: 3, name: "tx-checksum-ip-generic"}),
		verboseBitset(t, ethtoolAFeaturesNochange, true, testBit{index: 3, name: "tx-checksum-ip-generic"}),
		u32Attr(99, 1), // an attribute a newer kernel might add
	)
	got, err := parseFeatures(reply)
	if err != nil {
		t.Fatalf("parseFeatures: %v", err)
	}
	want := map[string]Feature{
		"rx-checksum":            {Active: true, Requested: true, Fixed: false},
		"rx-gro-hw":              {Active: false, Requested: false, Fixed: false},
		"tx-checksum-ip-generic": {Active: true, Requested: false, Fixed: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseFeatures = %+v, want %+v", got, want)
	}
}

func TestParseFeaturesErrors(t *testing.T) {
	if got, err := parseFeatures(errorReply(t, -95)); !errors.Is(err, ErrNotSupported) {
		t.Errorf("parseFeatures(EOPNOTSUPP) = %v, %v; want ErrNotSupported", got, err)
	}
	compact := testNested(t, ethtoolAFeaturesActive, testAttr(t, bitsetNomask, nil), u32Attr(bitsetSize, 32), testAttr(t, bitsetValue, []byte{1, 0, 0, 0}))
	if got, err := parseFeatures(genlReply(t, ethtoolMsgFeaturesGetReply, compact)); err == nil {
		t.Errorf("parseFeatures(compact) = %v, nil; want an error", got)
	}
	if got, err := parseFeatures(genlReply(t, ethtoolMsgLinkModesGetReply)); err == nil {
		t.Errorf("parseFeatures(a link-modes reply) = %v, nil; want an error", got)
	}
	if got, err := parseFeatures(nil); err == nil {
		t.Errorf("parseFeatures(no messages) = %v, nil; want an error", got)
	}
}

func TestParseLinkModes(t *testing.T) {
	reply := genlReply(t, ethtoolMsgLinkModesGetReply,
		testNested(t, ethtoolAHeader, u32Attr(ethtoolAHeaderDevIndex, 4)),
		u8Attr(t, ethtoolALinkModesAutoneg, 0),
		verboseBitset(t, ethtoolALinkModesOurs, false,
			testBit{index: 42, name: "10000baseCR/Full", set: true},
			testBit{index: 31, name: "25000baseCR/Full", set: true},
			testBit{index: 6, name: "Autoneg", set: true},
			testBit{index: 10, name: "FIBRE"},
		),
		verboseBitset(t, ethtoolALinkModesPeer, true),
		u32Attr(ethtoolALinkModesSpeed, 25000),
		u8Attr(t, ethtoolALinkModesDuplex, 1),
		u32Attr(ethtoolALinkModesLanes, 1),
		u32Attr(99, 1),
	)
	var l Link
	if err := parseLinkModes(reply, &l); err != nil {
		t.Fatalf("parseLinkModes: %v", err)
	}
	if l.Autoneg == nil || *l.Autoneg {
		t.Errorf("Autoneg = %v, want false", l.Autoneg)
	}
	if l.SpeedMbps != 25000 || l.Duplex != "full" || l.Lanes != 1 {
		t.Errorf("speed/duplex/lanes = %d/%q/%d, want 25000/full/1", l.SpeedMbps, l.Duplex, l.Lanes)
	}
	// The bitset's listed order is kept, not re-sorted (the kernel lists by bit index), with Autoneg and FIBRE dropped.
	modes := []string{"10000baseCR/Full", "25000baseCR/Full"}
	if !reflect.DeepEqual(l.Supported, modes) || !reflect.DeepEqual(l.Advertised, modes) {
		t.Errorf("Supported/Advertised = %q/%q, want %q for both", l.Supported, l.Advertised, modes)
	}
	if l.Partner == nil || len(l.Partner) != 0 {
		t.Errorf("Partner = %#v, want an empty, non-nil list", l.Partner)
	}
	if l.Port != "" {
		t.Errorf("Port = %q, want empty (LINKMODES does not carry it)", l.Port)
	}
}

func TestParseLinkModesAdvertisedSubsetAndPeer(t *testing.T) {
	reply := genlReply(t, ethtoolMsgLinkModesGetReply,
		u8Attr(t, ethtoolALinkModesAutoneg, 1),
		verboseBitset(t, ethtoolALinkModesOurs, false,
			testBit{index: 31, name: "25000baseCR/Full", set: true},
			testBit{index: 42, name: "10000baseCR/Full"},
		),
		verboseBitset(t, ethtoolALinkModesPeer, true, testBit{index: 13, name: "Pause"}, testBit{index: 31, name: "25000baseCR/Full"}),
	)
	var l Link
	if err := parseLinkModes(reply, &l); err != nil {
		t.Fatalf("parseLinkModes: %v", err)
	}
	if l.Autoneg == nil || !*l.Autoneg {
		t.Errorf("Autoneg = %v, want true", l.Autoneg)
	}
	if want := []string{"25000baseCR/Full", "10000baseCR/Full"}; !reflect.DeepEqual(l.Supported, want) {
		t.Errorf("Supported = %q, want %q", l.Supported, want)
	}
	if want := []string{"25000baseCR/Full"}; !reflect.DeepEqual(l.Advertised, want) {
		t.Errorf("Advertised = %q, want %q", l.Advertised, want)
	}
	if want := []string{"25000baseCR/Full"}; !reflect.DeepEqual(l.Partner, want) {
		t.Errorf("Partner = %q, want %q (NOMASK: every listed mode)", l.Partner, want)
	}
}

func TestParseLinkModesUnknowns(t *testing.T) {
	reply := genlReply(t, ethtoolMsgLinkModesGetReply,
		u32Attr(ethtoolALinkModesSpeed, speedUnknown),
		u8Attr(t, ethtoolALinkModesDuplex, duplexUnknown),
	)
	var l Link
	if err := parseLinkModes(reply, &l); err != nil {
		t.Fatalf("parseLinkModes: %v", err)
	}
	if l.SpeedMbps != 0 || l.Duplex != "" || l.Autoneg != nil || l.Lanes != 0 {
		t.Errorf("link = %+v, want speed 0, duplex \"\", no autoneg, lanes 0", l)
	}
	if l.Supported == nil || l.Advertised == nil || l.Partner == nil {
		t.Errorf("mode lists = %#v/%#v/%#v, want empty, non-nil lists", l.Supported, l.Advertised, l.Partner)
	}

	half := genlReply(t, ethtoolMsgLinkModesGetReply, u8Attr(t, ethtoolALinkModesDuplex, 0))
	if err := parseLinkModes(half, &l); err != nil || l.Duplex != "half" {
		t.Errorf("parseLinkModes(DUPLEX 0) = Duplex %q, %v; want half", l.Duplex, err)
	}
}

func TestParseLinkModesErrors(t *testing.T) {
	var l Link
	if err := parseLinkModes(errorReply(t, -19), &l); !errors.Is(err, ErrNoDevice) {
		t.Errorf("parseLinkModes(ENODEV) = %v, want ErrNoDevice", err)
	}
	if err := parseLinkModes(genlReply(t, ethtoolMsgLinkModesGetReply, testAttr(t, ethtoolALinkModesSpeed, []byte{1, 2})), &l); err == nil {
		t.Error("parseLinkModes with a 2-byte SPEED: no error")
	}
}

func TestParseLinkInfo(t *testing.T) {
	tests := []struct {
		port uint8
		want string
	}{
		{0x00, "TP"},
		{0x01, "AUI"},
		{0x02, "MII"},
		{0x03, "FIBRE"},
		{0x04, "BNC"},
		{0x05, "DA"},
		{0xef, "NONE"},
		{0xff, "OTHER"},
		{0x42, "unknown"},
	}
	for _, tt := range tests {
		var l Link
		reply := genlReply(t, ethtoolMsgLinkInfoGetReply, testNested(t, ethtoolAHeader, u32Attr(ethtoolAHeaderDevIndex, 4)),
			u8Attr(t, ethtoolALinkInfoPort, tt.port), u8Attr(t, 3, 0))
		if err := parseLinkInfo(reply, &l); err != nil || l.Port != tt.want {
			t.Errorf("parseLinkInfo(PORT %#x) = %q, %v; want %q", tt.port, l.Port, err, tt.want)
		}
	}

	var l Link
	if err := parseLinkInfo(genlReply(t, ethtoolMsgLinkInfoGetReply), &l); err != nil || l.Port != "" {
		t.Errorf("parseLinkInfo(no PORT) = %q, %v; want \"\"", l.Port, err)
	}
	if err := parseLinkInfo(errorReply(t, -95), &l); !errors.Is(err, ErrNotSupported) {
		t.Errorf("parseLinkInfo(EOPNOTSUPP) = %v, want ErrNotSupported", err)
	}
}

func TestParseFEC(t *testing.T) {
	modes := verboseBitset(t, ethtoolAFECModes, true, testBit{index: 49, name: "None"}, testBit{index: 50, name: "RS"})
	tests := []struct {
		name   string
		active uint32
		want   string
	}{
		{"active in the configured modes", 50, "RS"},
		{"active from the table", 74, "LLRS"},
		{"active unknown", 200, "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reply := genlReply(t, ethtoolMsgFECGetReply, testNested(t, ethtoolAHeader, u32Attr(ethtoolAHeaderDevIndex, 4)),
				modes, u8Attr(t, ethtoolAFECAuto, 0), u32Attr(ethtoolAFECActive, tt.active), u32Attr(99, 1))
			got, err := parseFEC(reply)
			if err != nil {
				t.Fatalf("parseFEC: %v", err)
			}
			if want := []string{"None", "RS"}; !reflect.DeepEqual(got.Configured, want) {
				t.Errorf("Configured = %q, want %q", got.Configured, want)
			}
			if got.Active != tt.want {
				t.Errorf("Active = %q, want %q", got.Active, tt.want)
			}
			if got.Auto == nil || *got.Auto {
				t.Errorf("Auto = %v, want false", got.Auto)
			}
		})
	}
}

func TestParseFECMinimal(t *testing.T) {
	// The kernel omits ACTIVE when no FEC mode is active; nothing configured is an empty list, not a missing one.
	got, err := parseFEC(genlReply(t, ethtoolMsgFECGetReply, verboseBitset(t, ethtoolAFECModes, true), u8Attr(t, ethtoolAFECAuto, 1)))
	if err != nil {
		t.Fatalf("parseFEC: %v", err)
	}
	if got.Configured == nil || len(got.Configured) != 0 || got.Active != "" || got.Auto == nil || !*got.Auto {
		t.Errorf("parseFEC = %+v, want no configured modes, no active mode, auto true", got)
	}
	if got, err := parseFEC(errorReply(t, -95)); !errors.Is(err, ErrNotSupported) {
		t.Errorf("parseFEC(EOPNOTSUPP) = %+v, %v; want ErrNotSupported", got, err)
	}
}

func FuzzParseFeatures(f *testing.F) {
	reply := func(cmd uint8, attrs ...[]byte) []byte {
		return testMessage(f, 0x1b, 0, 1, append([]byte{cmd, genlVersion, 0, 0}, bytes.Join(attrs, nil)...))
	}
	f.Add(reply(ethtoolMsgFeaturesGetReply,
		verboseBitset(f, ethtoolAFeaturesHW, true, testBit{index: 1, name: "rx-checksum"}),
		verboseBitset(f, ethtoolAFeaturesActive, false, testBit{index: 1, name: "rx-checksum", set: true})))
	f.Add(reply(ethtoolMsgLinkModesGetReply, verboseBitset(f, ethtoolALinkModesOurs, false, testBit{index: 31, name: "25000baseCR/Full"}),
		u32Attr(ethtoolALinkModesSpeed, 25000)))
	f.Add(reply(ethtoolMsgFECGetReply, verboseBitset(f, ethtoolAFECModes, true, testBit{index: 50, name: "RS"}), u32Attr(ethtoolAFECActive, 50)))
	f.Add(testMessage(f, nlmsgError, 0, 1, binary.NativeEndian.AppendUint32(nil, uint32(0xffffffa1))))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		msgs, err := parseMessages(b)
		if err != nil {
			return
		}
		_, _ = parseFeatures(msgs)
		// The other reply parsers share the bitset and attribute walking, so the same input exercises them too.
		var l Link
		_ = parseLinkModes(msgs, &l)
		_ = parseLinkInfo(msgs, &l)
		_, _ = parseFEC(msgs)
		_, _ = parseFamilyID(msgs)
	})
}

package ethtool

import (
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"syscall"
	"testing"
)

const testFamily = 0x1b

// sentRequest is what fakeConn decoded from one request.
type sentRequest struct {
	typ     uint16
	cmd     uint8
	seq     uint32
	ifindex uint32 // 0 for the family lookup, which names no interface
	name    string // "" for the family lookup
}

// fakeConn answers each request with the messages replies holds for its generic-netlink command, and records what was asked.
type fakeConn struct {
	t       *testing.T
	replies map[uint8][]message
	fails   map[uint8]error // a transport error in place of the reply
	sent    []sentRequest
	closed  bool
}

func (f *fakeConn) roundTrip(req []byte, seq uint32) ([]message, error) {
	f.t.Helper()
	msgs, err := parseMessages(req)
	if err != nil || len(msgs) != 1 {
		f.t.Fatalf("request: parseMessages = %v, %v; want one message", msgs, err)
	}
	m := msgs[0]
	if m.seq != seq || m.flags != nlmFRequest {
		f.t.Fatalf("request header: seq %d flags %#x; want seq %d (the roundTrip argument) and NLM_F_REQUEST only", m.seq, m.flags, seq)
	}
	if len(m.data) < genlHdrLen {
		f.t.Fatalf("request of %d bytes has no genlmsghdr", len(m.data))
	}
	s := sentRequest{typ: m.typ, cmd: m.data[0], seq: m.seq}
	if m.typ != genlIDCtrl {
		attrs, err := parseAttributes(m.data[genlHdrLen:])
		if err != nil || len(attrs) == 0 || attrs[0].typ != ethtoolAHeader {
			f.t.Fatalf("request attributes = %v, %v; want the header nest first", attrs, err)
		}
		header, err := parseAttributes(attrs[0].data)
		if err != nil || len(header) < 2 || header[0].typ != ethtoolAHeaderDevIndex || header[1].typ != ethtoolAHeaderDevName {
			f.t.Fatalf("request header nest = %v, %v; want DEV_INDEX then DEV_NAME", header, err)
		}
		if s.ifindex, err = header[0].u32(); err != nil {
			f.t.Fatalf("DEV_INDEX: %v", err)
		}
		s.name = header[1].str()
	}
	f.sent = append(f.sent, s)
	if err := f.fails[s.cmd]; err != nil {
		return nil, err
	}
	reply, ok := f.replies[s.cmd]
	if !ok {
		f.t.Fatalf("unexpected request for command %d", s.cmd)
	}
	return reply, nil
}

func (f *fakeConn) close() error {
	f.closed = true
	return nil
}

// newFakeClient returns a Client over a fakeConn whose family lookup resolves the ethtool family to testFamily; replies adds the ethtool
// commands' replies.
func newFakeClient(t *testing.T, replies map[uint8][]message) (*Client, *fakeConn) {
	t.Helper()
	all := map[uint8][]message{
		ctrlCmdGetFamily: genlReply(t, ctrlCmdNewFamily, stringAttr(ctrlAttrFamilyName, ethtoolFamilyName),
			encodeAttr(ctrlAttrFamilyID, binary.NativeEndian.AppendUint16(nil, testFamily))),
	}
	maps.Copy(all, replies)
	f := &fakeConn{t: t, replies: all}
	c, err := newClient(f)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return c, f
}

func TestNewClientResolvesFamily(t *testing.T) {
	c, f := newFakeClient(t, nil)
	if c.family != testFamily {
		t.Errorf("family = %#x, want %#x", c.family, testFamily)
	}
	want := []sentRequest{{typ: genlIDCtrl, cmd: ctrlCmdGetFamily, seq: 1}}
	if !reflect.DeepEqual(f.sent, want) {
		t.Errorf("sent %+v, want %+v", f.sent, want)
	}
}

func TestNewClientNoFamily(t *testing.T) {
	f := &fakeConn{t: t, replies: map[uint8][]message{ctrlCmdGetFamily: errorReply(t, -2)}}
	if c, err := newClient(f); !errors.Is(err, ErrNoFamily) {
		t.Errorf("newClient(ENOENT) = %v, %v; want ErrNoFamily", c, err)
	}
	if !f.closed {
		t.Error("newClient left the connection open after the family lookup failed")
	}
}

func TestClientFeatures(t *testing.T) {
	c, f := newFakeClient(t, map[uint8][]message{
		ethtoolMsgFeaturesGet: genlReply(t, ethtoolMsgFeaturesGetReply,
			verboseBitset(ethtoolAFeaturesHW, true, testBit{index: 1, name: "rx-checksum"}),
			verboseBitset(ethtoolAFeaturesActive, true, testBit{index: 1, name: "rx-checksum"})),
	})
	got, err := c.Features(4, "eth4")
	if err != nil {
		t.Fatalf("Features: %v", err)
	}
	if want := map[string]Feature{"rx-checksum": {Active: true}}; !reflect.DeepEqual(got, want) {
		t.Errorf("Features = %+v, want %+v", got, want)
	}
	if want := (sentRequest{typ: testFamily, cmd: ethtoolMsgFeaturesGet, seq: 2, ifindex: 4, name: "eth4"}); f.sent[1] != want {
		t.Errorf("request = %+v, want %+v", f.sent[1], want)
	}
}

func TestClientLink(t *testing.T) {
	linkModes := genlReply(t, ethtoolMsgLinkModesGetReply,
		verboseBitset(ethtoolALinkModesOurs, false, testBit{index: 31, name: "25000baseCR/Full", set: true}),
		u32Attr(ethtoolALinkModesSpeed, 25000))
	modes := []string{"25000baseCR/Full"}

	t.Run("with port", func(t *testing.T) {
		c, f := newFakeClient(t, map[uint8][]message{
			ethtoolMsgLinkModesGet: linkModes,
			ethtoolMsgLinkInfoGet:  genlReply(t, ethtoolMsgLinkInfoGetReply, u8Attr(ethtoolALinkInfoPort, 0x05)),
		})
		got, err := c.Link(7, "eth7")
		if err != nil {
			t.Fatalf("Link: %v", err)
		}
		want := &Link{Port: "DA", SpeedMbps: 25000, Supported: modes, Advertised: modes, Partner: []string{}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Link = %+v, want %+v", got, want)
		}
		wantSent := []sentRequest{
			{typ: testFamily, cmd: ethtoolMsgLinkModesGet, seq: 2, ifindex: 7, name: "eth7"},
			{typ: testFamily, cmd: ethtoolMsgLinkInfoGet, seq: 3, ifindex: 7, name: "eth7"},
		}
		if !reflect.DeepEqual(f.sent[1:], wantSent) {
			t.Errorf("requests = %+v, want %+v", f.sent[1:], wantSent)
		}
	})

	t.Run("link info not supported", func(t *testing.T) {
		c, _ := newFakeClient(t, map[uint8][]message{
			ethtoolMsgLinkModesGet: linkModes,
			ethtoolMsgLinkInfoGet:  errorReply(t, -95),
		})
		got, err := c.Link(7, "eth7")
		if err != nil {
			t.Fatalf("Link: %v", err)
		}
		if got.Port != "" || got.SpeedMbps != 25000 || !reflect.DeepEqual(got.Supported, modes) {
			t.Errorf("Link = %+v, want the link modes with Port \"\"", got)
		}
	})

	t.Run("link info fails", func(t *testing.T) {
		c, _ := newFakeClient(t, map[uint8][]message{
			ethtoolMsgLinkModesGet: linkModes,
			ethtoolMsgLinkInfoGet:  errorReply(t, -19),
		})
		if got, err := c.Link(7, "eth7"); !errors.Is(err, ErrNoDevice) {
			t.Errorf("Link = %+v, %v; want ErrNoDevice", got, err)
		}
	})

	t.Run("link modes fail", func(t *testing.T) {
		c, f := newFakeClient(t, map[uint8][]message{ethtoolMsgLinkModesGet: errorReply(t, -95)})
		if got, err := c.Link(7, "eth7"); !errors.Is(err, ErrNotSupported) {
			t.Errorf("Link = %+v, %v; want ErrNotSupported", got, err)
		}
		if len(f.sent) != 2 {
			t.Errorf("sent %+v; want no LINKINFO request after LINKMODES failed", f.sent)
		}
	})
}

func TestClientFEC(t *testing.T) {
	c, f := newFakeClient(t, map[uint8][]message{
		ethtoolMsgFECGet: genlReply(t, ethtoolMsgFECGetReply,
			verboseBitset(ethtoolAFECModes, true, testBit{index: 50, name: "RS"}), u32Attr(ethtoolAFECActive, 50)),
	})
	got, err := c.FEC(3, "eth3")
	if err != nil {
		t.Fatalf("FEC: %v", err)
	}
	if want := (&FEC{Configured: []string{"RS"}, Active: "RS"}); !reflect.DeepEqual(got, want) {
		t.Errorf("FEC = %+v, want %+v", got, want)
	}
	if want := (sentRequest{typ: testFamily, cmd: ethtoolMsgFECGet, seq: 2, ifindex: 3, name: "eth3"}); f.sent[1] != want {
		t.Errorf("request = %+v, want %+v", f.sent[1], want)
	}
}

func TestClientSeqIncreases(t *testing.T) {
	c, f := newFakeClient(t, map[uint8][]message{
		ethtoolMsgFeaturesGet: genlReply(t, ethtoolMsgFeaturesGetReply),
		ethtoolMsgFECGet:      genlReply(t, ethtoolMsgFECGetReply),
	})
	for range 2 {
		if _, err := c.Features(1, "eth1"); err != nil {
			t.Fatalf("Features: %v", err)
		}
		if _, err := c.FEC(1, "eth1"); err != nil {
			t.Fatalf("FEC: %v", err)
		}
	}
	for i, s := range f.sent {
		if s.seq != uint32(i+1) {
			t.Errorf("request %d has seq %d, want %d", i, s.seq, i+1)
		}
	}
}

func TestClientClose(t *testing.T) {
	c, f := newFakeClient(t, nil)
	if err := c.Close(); err != nil || !f.closed {
		t.Errorf("Close = %v, closed %v; want nil, true", err, f.closed)
	}
}

// Each query's error names the query once, whether the kernel or the socket failed, and still matches its cause.
func TestClientErrorsNameTheQuery(t *testing.T) {
	linkModes := genlReply(t, ethtoolMsgLinkModesGetReply, u32Attr(ethtoolALinkModesSpeed, 1000))
	queries := []struct {
		name string
		cmd  uint8
		ask  func(*Client) error
		// earlier holds the replies to the commands the query sends before cmd.
		earlier map[uint8][]message
	}{
		{"features", ethtoolMsgFeaturesGet, func(c *Client) error { _, err := c.Features(2, "eth2"); return err }, nil},
		{"link modes", ethtoolMsgLinkModesGet, func(c *Client) error { _, err := c.Link(2, "eth2"); return err }, nil},
		{"link info", ethtoolMsgLinkInfoGet, func(c *Client) error { _, err := c.Link(2, "eth2"); return err },
			map[uint8][]message{ethtoolMsgLinkModesGet: linkModes}},
		{"fec", ethtoolMsgFECGet, func(c *Client) error { _, err := c.FEC(2, "eth2"); return err }, nil},
	}
	timeout := fmt.Errorf("netlink: no reply: %w", os.ErrDeadlineExceeded) // as the Linux socket reports it
	failures := []struct {
		name  string
		reply []message
		fail  error
		want  error
		text  string
	}{
		{"EOPNOTSUPP", errorReply(t, -95), nil, ErrNotSupported, ErrNotSupported.Error()},
		{"ENODEV", errorReply(t, -19), nil, ErrNoDevice, ErrNoDevice.Error()},
		{"EIO", errorReply(t, -5), nil, syscall.EIO, syscall.EIO.Error()},
		{"timeout", nil, timeout, os.ErrDeadlineExceeded, timeout.Error()},
	}
	for _, q := range queries {
		for _, f := range failures {
			if q.name == "link info" && f.name == "EOPNOTSUPP" {
				continue // Link returns the link modes without a port; TestClientLink covers it.
			}
			t.Run(q.name+"/"+f.name, func(t *testing.T) {
				replies := maps.Clone(q.earlier)
				if replies == nil {
					replies = map[uint8][]message{}
				}
				replies[q.cmd] = f.reply
				c, conn := newFakeClient(t, replies)
				if f.fail != nil {
					conn.fails = map[uint8]error{q.cmd: f.fail}
				}
				err := q.ask(c)
				if !errors.Is(err, f.want) {
					t.Errorf("error = %v, want it to match %v", err, f.want)
				}
				if want := q.name + ": " + f.text; err == nil || err.Error() != want {
					t.Errorf("error = %v, want %q", err, want)
				}
			})
		}
	}
}

// encodeTestMessage returns one encoded message of type typ and sequence number seq with an empty generic-netlink payload of command cmd.
func encodeTestMessage(typ uint16, seq uint32, cmd uint8) []byte {
	if typ < nlmsgMinType {
		return encodeMessage(typ, 0, seq, binary.NativeEndian.AppendUint32(nil, 0))
	}
	return encodeMessage(typ, 0, seq, []byte{cmd, genlVersion, 0, 0})
}

func TestReplyMessages(t *testing.T) {
	tests := []struct {
		name     string
		buf      []byte
		wantSeqs []uint32
		wantTyps []uint16
		done     bool
	}{
		{
			name:     "the reply",
			buf:      encodeTestMessage(testFamily, 5, 11),
			wantSeqs: []uint32{5}, wantTyps: []uint16{testFamily}, done: true,
		},
		{
			name: "a stale reply is dropped",
			buf:  encodeTestMessage(testFamily, 4, 11),
		},
		{
			name:     "a stale reply then the reply",
			buf:      append(encodeTestMessage(testFamily, 4, 11), encodeTestMessage(testFamily, 5, 11)...),
			wantSeqs: []uint32{5}, wantTyps: []uint16{testFamily}, done: true,
		},
		{
			name:     "an error",
			buf:      encodeTestMessage(nlmsgError, 5, 0),
			wantSeqs: []uint32{5}, wantTyps: []uint16{nlmsgError}, done: true,
		},
		{
			name:     "done",
			buf:      encodeTestMessage(nlmsgDone, 5, 0),
			wantSeqs: []uint32{5}, wantTyps: []uint16{nlmsgDone}, done: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs, err := parseMessages(tt.buf)
			if err != nil {
				t.Fatalf("parseMessages: %v", err)
			}
			got, done := replyMessages(msgs, 5)
			var seqs []uint32
			var typs []uint16
			for _, m := range got {
				seqs = append(seqs, m.seq)
				typs = append(typs, m.typ)
			}
			if !reflect.DeepEqual(seqs, tt.wantSeqs) || !reflect.DeepEqual(typs, tt.wantTyps) || done != tt.done {
				t.Errorf("replyMessages = seqs %v types %v done %v; want %v %v %v", seqs, typs, done, tt.wantSeqs, tt.wantTyps, tt.done)
			}
		})
	}
}

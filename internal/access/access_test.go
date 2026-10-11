package access

import (
	"net/netip"
	"testing"
)

func TestMatcher(t *testing.T) {
	tests := []struct {
		name  string
		allow []string
		deny  []string
		peer  string
		want  bool
	}{
		{name: "empty allow list denies everything", peer: "10.0.0.5", want: false},
		{name: "address inside an allowed prefix", allow: []string{"10.0.0.0/16"}, peer: "10.0.5.9", want: true},
		{name: "address outside every allowed prefix", allow: []string{"10.0.0.0/16"}, peer: "192.168.1.1", want: false},
		{name: "bare address entry is an exact match", allow: []string{"10.0.0.5"}, peer: "10.0.0.5", want: true},
		{name: "bare address entry does not match a neighbour", allow: []string{"10.0.0.5"}, peer: "10.0.0.6", want: false},
		{name: "deny wins over allow", allow: []string{"10.0.0.0/16"}, deny: []string{"10.0.7.13/32"}, peer: "10.0.7.13", want: false},
		{name: "deny of a wider range wins", allow: []string{"10.0.0.0/16"}, deny: []string{"10.0.0.0/24"}, peer: "10.0.0.9", want: false},
		{name: "ipv6 prefix", allow: []string{"fd00::/48"}, peer: "fd00::5", want: true},
		{name: "ipv6 outside prefix", allow: []string{"fd00::/48"}, peer: "fd01::5", want: false},
		{name: "ipv4-mapped ipv6 peer matches an ipv4 rule", allow: []string{"10.0.0.0/16"}, peer: "::ffff:10.0.5.9", want: true},
		{name: "loopback v4", allow: []string{"127.0.0.1/32"}, peer: "127.0.0.1", want: true},
		{name: "loopback v6", allow: []string{"::1/128"}, peer: "::1", want: true},
		{name: "deny rule in mapped CIDR form blocks plain IPv4 peer", allow: []string{"10.0.0.0/16"}, deny: []string{"::ffff:10.0.5.9/128"}, peer: "10.0.5.9", want: false},
		{name: "allow rule in mapped CIDR form admits plain IPv4 peer", allow: []string{"::ffff:10.0.0.0/112"}, peer: "10.0.5.9", want: true},
		{name: "mapped full IPv4 space allow rule admits any IPv4 peer", allow: []string{"::ffff:0:0/96"}, peer: "10.0.5.9", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := New(tt.allow, tt.deny)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got := m.Allowed(netip.MustParseAddr(tt.peer)); got != tt.want {
				t.Errorf("Allowed(%s) = %v, want %v", tt.peer, got, tt.want)
			}
		})
	}
}

func TestNewRejectsMalformedEntries(t *testing.T) {
	for _, entry := range []string{"10.0.0.0/64", "not-an-address", "10.0.0.0/", "10.0.0.0-10.0.0.9"} {
		if _, err := New([]string{entry}, nil); err == nil {
			t.Errorf("New(%q) succeeded, want an error", entry)
		}
	}
}

func TestMappedAddressWithIPv6PrefixLengthRejected(t *testing.T) {
	// IPv4-mapped address with IPv6-scale prefix length is ambiguous and must be rejected
	if _, err := New([]string{"::ffff:10.0.0.0/64"}, nil); err == nil {
		t.Error("New(\"::ffff:10.0.0.0/64\") succeeded, want an error")
	}
}

func TestDeniesEverything(t *testing.T) {
	m, err := New(nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !m.DeniesEverything() {
		t.Error("an empty allow list must report that it denies everything")
	}

	m, err = New([]string{"10.0.0.0/8"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.DeniesEverything() {
		t.Error("a populated allow list must not report that it denies everything")
	}
}

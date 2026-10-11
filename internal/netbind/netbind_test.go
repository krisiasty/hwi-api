package netbind

import (
	"fmt"
	"net/netip"
	"testing"
)

// fakeLister answers from a fixed table so tests never touch real NICs.
type fakeLister map[string][]string

func (f fakeLister) InterfaceAddrs(name string) ([]netip.Addr, error) {
	raw, ok := f[name]
	if !ok {
		return nil, fmt.Errorf("no such interface %q", name)
	}
	out := make([]netip.Addr, 0, len(raw))
	for _, s := range raw {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

func strs(addrs []netip.Addr) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if !a.IsValid() {
			out = append(out, "*")
			continue
		}
		out = append(out, a.String())
	}
	return out
}

func TestResolve(t *testing.T) {
	lister := fakeLister{
		"eth0":  {"10.0.0.5", "fd00::5", "fe80::1"},
		"bond0": {"10.0.0.5", "10.0.1.7"},
		"empty": {},
	}

	tests := []struct {
		name       string
		addresses  []string
		interfaces []string
		want       []string
		wantErr    bool
	}{
		{
			name: "nothing configured means wildcard",
			want: []string{"*"},
		},
		{
			name:      "literal addresses are kept in sorted order",
			addresses: []string{"10.0.0.5", "fd00::5"},
			want:      []string{"10.0.0.5", "fd00::5"},
		},
		{
			name:       "interface names resolve to their addresses",
			interfaces: []string{"eth0"},
			want:       []string{"10.0.0.5", "fd00::5"},
		},
		{
			name:       "link-local addresses are skipped",
			interfaces: []string{"eth0"},
			want:       []string{"10.0.0.5", "fd00::5"},
		},
		{
			name:       "addresses and interfaces are merged and deduplicated",
			addresses:  []string{"10.0.0.5"},
			interfaces: []string{"bond0"},
			want:       []string{"10.0.0.5", "10.0.1.7"},
		},
		{
			name:       "an interface with no usable address is an error",
			interfaces: []string{"empty"},
			wantErr:    true,
		},
		{
			name:       "an unknown interface is an error",
			interfaces: []string{"nope0"},
			wantErr:    true,
		},
		{
			name:      "a malformed address is an error",
			addresses: []string{"10.0.0.999"},
			wantErr:   true,
		},
		{
			name:      "a host:port pair is rejected as an address",
			addresses: []string{"10.0.0.5:8443"},
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.addresses, tt.interfaces, lister)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Resolve() = %v, want an error", strs(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			gotStrs := strs(got)
			if len(gotStrs) != len(tt.want) {
				t.Fatalf("Resolve() = %v, want %v", gotStrs, tt.want)
			}
			for i := range gotStrs {
				if gotStrs[i] != tt.want[i] {
					t.Fatalf("Resolve() = %v, want %v", gotStrs, tt.want)
				}
			}
		})
	}
}

func TestResolveIsDeterministic(t *testing.T) {
	lister := fakeLister{"eth0": {"fd00::5", "10.0.0.5"}}
	first, err := Resolve(nil, []string{"eth0"}, lister)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := Resolve(nil, []string{"eth0"}, lister)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if strs(again)[0] != strs(first)[0] {
			t.Fatalf("ordering varies between calls: %v then %v", strs(first), strs(again))
		}
	}
}

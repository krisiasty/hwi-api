// Package access decides whether a peer address may reach a listener.
package access

import (
	"fmt"
	"net/netip"
	"strings"
)

// Matcher evaluates a peer address against a deny list and an allow list.
// Deny is evaluated first and wins; an empty allow list denies everything.
type Matcher struct {
	allow []netip.Prefix
	deny  []netip.Prefix
}

// New compiles the two lists. Entries are CIDR prefixes or bare addresses,
// in either family.
func New(allow, deny []string) (*Matcher, error) {
	a, err := parseAll("allow", allow)
	if err != nil {
		return nil, err
	}
	d, err := parseAll("deny", deny)
	if err != nil {
		return nil, err
	}
	return &Matcher{allow: a, deny: d}, nil
}

// Allowed reports whether the peer may proceed.
func (m *Matcher) Allowed(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range m.deny {
		if p.Contains(a) {
			return false
		}
	}
	for _, p := range m.allow {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// DeniesEverything reports whether no peer can ever be admitted, so startup
// can warn about a listener that will reject every client.
func (m *Matcher) DeniesEverything() bool {
	return len(m.allow) == 0
}

func parseAll(list string, entries []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(entries))
	for _, raw := range entries {
		p, err := parseEntry(raw)
		if err != nil {
			return nil, fmt.Errorf("%s list entry %q: %w", list, raw, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// parseEntry accepts a prefix or a bare address, the latter treated as a
// single-host prefix. Prefixes are masked so that 10.0.0.5/16 behaves as
// 10.0.0.0/16 rather than silently never matching. IPv4-mapped addresses are
// normalized to their IPv4 form to ensure consistent matching.
func parseEntry(raw string) (netip.Prefix, error) {
	s := strings.TrimSpace(raw)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		// Normalize IPv4-mapped prefixes to IPv4 form
		if p.Addr().Is4In6() {
			bits := p.Bits()
			if bits < 96 {
				return netip.Prefix{}, fmt.Errorf("an IPv4-mapped address with an IPv6 prefix length is ambiguous; write it as an IPv4 prefix such as \"10.0.0.0/16\"")
			}
			unmapped := p.Addr().Unmap()
			return netip.PrefixFrom(unmapped, bits-96).Masked(), nil
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("not an address or CIDR prefix: %w", err)
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

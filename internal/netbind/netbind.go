// Package netbind turns configured addresses and interface names into the
// concrete set of addresses to listen on.
package netbind

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
)

// Lister reports the addresses assigned to a named interface.
type Lister interface {
	InterfaceAddrs(name string) ([]netip.Addr, error)
}

// System is the Lister backed by the host's real interfaces.
type System struct{}

// InterfaceAddrs implements Lister.
func (System) InterfaceAddrs(name string) ([]netip.Addr, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, fmt.Errorf("interface %q: %w", name, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("interface %q: %w", name, err)
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		prefix, err := netip.ParsePrefix(a.String())
		if err != nil {
			continue
		}
		out = append(out, prefix.Addr())
	}
	return out, nil
}

// Resolve merges literal addresses and interface names into a deduplicated,
// deterministically ordered set of bind addresses.
//
// With neither configured the result is a single invalid netip.Addr, which
// means "every address": the caller formats that as a wildcard ":port".
func Resolve(addresses, interfaces []string, l Lister) ([]netip.Addr, error) {
	seen := make(map[netip.Addr]struct{})
	var out []netip.Addr

	add := func(a netip.Addr) {
		a = a.Unmap()
		if _, dup := seen[a]; dup {
			return
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}

	for _, s := range addresses {
		a, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("bind address %q is not a valid IP address "+
				"(a port belongs in the separate port setting): %w", s, err)
		}
		add(a)
	}

	for _, name := range interfaces {
		addrs, err := l.InterfaceAddrs(name)
		if err != nil {
			return nil, fmt.Errorf("resolving bind interface %q: %w", name, err)
		}
		usable := 0
		for _, a := range addrs {
			if skip(a) {
				continue
			}
			usable++
			add(a)
		}
		if usable == 0 {
			return nil, fmt.Errorf("bind interface %q has no usable address; "+
				"link-local addresses are not eligible", name)
		}
	}

	if len(out) == 0 {
		return []netip.Addr{{}}, nil
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out, nil
}

// skip reports whether an address discovered on an interface is unsuitable
// to bind. Binding an IPv6 link-local address requires a zone, and neither
// family's link-local range is ever a management address.
func skip(a netip.Addr) bool {
	return a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast()
}

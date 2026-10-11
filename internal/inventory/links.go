package inventory

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"sort"
	"strings"
)

// Link is per-interface data: interface index, MTU, administrative and
// operational state, MAC address, and assigned addresses.
type Link struct {
	Name  string `json:"name"`
	Index int    `json:"index"`
	MTU   int    `json:"mtu"`
	// AdminState is "up" when the interface is administratively enabled
	// (ip link set ... up), else "down".
	AdminState string `json:"admin_state"`
	// OperState is the kernel's RFC 2863 operational state, exactly as
	// /sys/class/net/<name>/operstate reports it: "up", "down",
	// "lowerlayerdown", "dormant", "testing", "notpresent" or "unknown".
	// An interface can be administratively up and operationally down, for
	// example after losing its carrier. "unknown" is the kernel's own
	// value, typical of lo and of virtual or tunnel interfaces with no
	// carrier to sense; it never means the agent failed to read the state,
	// which fails the whole probe instead.
	OperState  string   `json:"oper_state"`
	MACAddress string   `json:"mac_address,omitempty"`
	Addresses  []string `json:"addresses,omitempty"`
}

// LinkLister reports the host's interfaces.
type LinkLister interface {
	Links() ([]Link, error)
}

// SystemLinks reads the real host: the interfaces and their addresses
// through net.Interfaces, and each one's operational state from sysfs.
// RootFS is the host's root filesystem; the operational state is read from
// sys/class/net/<name>/operstate under it.
type SystemLinks struct {
	RootFS fs.FS
}

// Links implements LinkLister.
func (s SystemLinks) Links() ([]Link, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("listing interfaces: %w", err)
	}
	return buildLinks(ifaces, func(i net.Interface) ([]net.Addr, error) { return i.Addrs() }, s.RootFS)
}

// buildLinks turns ifaces into Links, reading each one's addresses through
// addrs and its operational state from rootFS, sorted by name. It fails,
// rather than returning a partial list, if any interface's addresses or
// operational state cannot be read: a list that silently drops an
// interface, or reports it with guessed details, looks exactly like a host
// where that is really so. An interface removed between the listing and
// these reads fails the refresh in the same way; the next one succeeds.
func buildLinks(ifaces []net.Interface, addrs func(net.Interface) ([]net.Addr, error), rootFS fs.FS) ([]Link, error) {
	out := make([]Link, 0, len(ifaces))
	for _, i := range ifaces {
		l := Link{
			Name:       i.Name,
			Index:      i.Index,
			MTU:        i.MTU,
			AdminState: "down",
			MACAddress: i.HardwareAddr.String(),
		}
		if i.Flags&net.FlagUp != 0 {
			l.AdminState = "up"
		}

		state, err := fs.ReadFile(rootFS, "sys/class/net/"+i.Name+"/operstate")
		if err != nil {
			return nil, fmt.Errorf("reading operational state of %s: %w", i.Name, err)
		}
		l.OperState = strings.TrimSpace(string(state))

		as, err := addrs(i)
		if err != nil {
			return nil, fmt.Errorf("reading addresses of %s: %w", i.Name, err)
		}
		for _, a := range as {
			l.Addresses = append(l.Addresses, a.String())
		}
		sort.Strings(l.Addresses)
		out = append(out, l)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// LinksInfo is the links subsystem's payload.
type LinksInfo struct {
	Links []Link `json:"links"`
}

// NewLinksProber reports the host's interfaces as the links subsystem. It
// is a subsystem of its own, not merged into the network payload, so that a
// failure here is an ordinary probe failure: the store keeps the last-good
// list and the envelope names the error, while the network subsystem stays
// fresh. It never reports an empty list in place of a failed enumeration.
func NewLinksProber(l LinkLister) Prober {
	return ProberFunc(func(context.Context) (any, error) {
		links, err := l.Links()
		if err != nil {
			return nil, err
		}
		if links == nil {
			links = []Link{}
		}
		return LinksInfo{Links: links}, nil
	})
}

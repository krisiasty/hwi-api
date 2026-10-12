package inventory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/krisiasty/hwi-api/internal/ethtool"
	"github.com/krisiasty/hwi-api/internal/pciaddr"
)

// classNetDir lists every network interface, each a link to its directory under sys/devices.
const classNetDir = "sys/class/net"

// Network is the network payload: the host's network interfaces from sysfs, each with what ethtool reports about it.
type Network struct {
	NICs []NIC `json:"nics"`
	// EthtoolError says why no NIC, or no NIC from some point on, has ethtool's blocks: the kernel's ethtool netlink family or the socket is
	// unavailable, a request of an earlier probe is still blocked in the kernel, or the queries stopped because the socket stopped answering.
	EthtoolError string `json:"ethtool_error,omitempty"`
	// PermanentMACError says why the kernel's permanent addresses could not be dumped; the NICs then carry the bonding driver's record of a
	// bond member's permanent address only.
	PermanentMACError string `json:"permanent_mac_error,omitempty"`
}

// NIC is one network interface. Link, FEC and Features are absent when the driver does not support their query, or the query failed.
type NIC struct {
	Name string `json:"name"`
	// Index is the kernel's interface index. It joins a NIC to `links` and to `udev-net`'s `n<index>` entry and, unlike the name, does not change
	// when the interface is renamed.
	Index uint32 `json:"index"`
	// MACAddress is the interface's current address, which for a bond member is usually the bond's.
	MACAddress string `json:"mac_address"`
	// PermanentMACAddress is the interface's permanent (burned-in) address: the kernel's IFLA_PERM_ADDRESS, or else the bonding driver's
	// bonding_slave/perm_hwaddr, which is the member's address when it was enslaved: the burned-in one unless the address was changed
	// before, as by a cloned MAC or a .link file's MACAddress=. It is absent when neither has one, or only zeros, as for most virtual
	// interfaces; a few virtual drivers, such as ipvlan and macsec, report their parent's address as their own permanent one.
	PermanentMACAddress string `json:"permanent_mac_address,omitempty"`
	// Virtual is true when the interface's sysfs directory is under /devices/virtual/: a bridge, bond, VLAN, tunnel or the like.
	Virtual bool `json:"virtual"`
	// PCIAddress is the PCI function the interface belongs to, directly or through a virtio device; absent for any other parent, or none.
	PCIAddress string                `json:"pci_address,omitempty"`
	Link       *NICLink              `json:"link,omitempty"`
	FEC        *NICFEC               `json:"fec,omitempty"`
	Features   map[string]NICFeature `json:"features,omitempty"`
	// EthtoolError is the first of this interface's ethtool queries to fail, other than as unsupported, naming the query.
	EthtoolError string `json:"ethtool_error,omitempty"`
}

// NICLink is the interface's link modes and port, as `ethtool <if>` prints them. Unknown values are absent; the mode lists are never null.
type NICLink struct {
	Port            string   `json:"port,omitempty"`
	Autonegotiation *bool    `json:"autonegotiation,omitempty"`
	SpeedMbps       uint32   `json:"speed_mbps,omitempty"`
	Duplex          string   `json:"duplex,omitempty"`
	Lanes           uint32   `json:"lanes,omitempty"`
	SupportedModes  []string `json:"supported_modes"`
	AdvertisedModes []string `json:"advertised_modes"`
	PartnerModes    []string `json:"partner_modes"`
}

// NICFEC is the interface's forward error correction setting, as `ethtool --show-fec <if>` prints it.
type NICFEC struct {
	Configured []string `json:"configured"`
	Active     string   `json:"active,omitempty"`
	Auto       *bool    `json:"auto,omitempty"`
}

// NICFeature is one offload feature, as `ethtool -k <if>` prints it; Fixed is its [fixed].
type NICFeature struct {
	Active    bool `json:"active"`
	Requested bool `json:"requested"`
	Fixed     bool `json:"fixed"`
}

// EthtoolSession answers the ethtool queries for one probe; *ethtool.Client implements it.
type EthtoolSession interface {
	Features(ifindex uint32, name string) (map[string]ethtool.Feature, error)
	Link(ifindex uint32, name string) (*ethtool.Link, error)
	FEC(ifindex uint32, name string) (*ethtool.FEC, error)
	Close() error
}

// DialEthtool opens a netlink session with the kernel's ethtool family.
func DialEthtool(ctx context.Context) (EthtoolSession, error) {
	c, err := ethtool.Dial(ctx)
	if err != nil {
		// Not c itself: a nil *ethtool.Client in the interface would not be nil.
		return nil, err
	}
	return c, nil
}

// NewNetworkProber reports every interface in rootFS's sys/class/net but lo, sorted by name, asking a session from dial about each. No
// ethtool binary is involved. perm, once per probe and before the ethtool queries, dumps the permanent addresses by interface index; an
// interface it has none for falls back to sysfs's bonding_slave/perm_hwaddr. perm may be nil, as for a file system that is not the running host's,
// which has no netlink to ask: then sysfs is the only source, and no error is reported. A failed dump is reported in the payload. Only failing to list the
// interfaces fails the probe: an ethtool failure is reported in the payload, for one interface or, when there is no session, for all of
// them. The queries stop, leaving the remaining interfaces with their sysfs fields only, when one times out. They do not stop when ctx is
// done: a forced refresh whose client goes away still stores its result as fresh, so the result must be complete, and each query, its send
// included, is bounded by the session's own timeout.
func NewNetworkProber(rootFS fs.FS, dial func(context.Context) (EthtoolSession, error), perm func() (map[uint32]string, error)) Prober {
	return ProberFunc(func(ctx context.Context) (any, error) {
		// fs.ReadDir sorts the entries by name.
		entries, err := fs.ReadDir(rootFS, classNetDir)
		if err != nil {
			return nil, fmt.Errorf("listing network interfaces: %w", err)
		}
		type iface struct {
			nic     NIC
			ifindex uint32
		}
		var ifaces []iface
		for _, e := range entries {
			// A regular file here, such as the bonding driver's bonding_masters, is not an interface.
			if e.Name() == "lo" || e.Type().IsRegular() {
				continue
			}
			if nic, ifindex, ok := readSysfsNIC(rootFS, e.Name()); ok {
				ifaces = append(ifaces, iface{nic, ifindex})
			}
		}

		out := &Network{NICs: make([]NIC, 0, len(ifaces))}
		var perms map[uint32]string
		if perm != nil {
			if perms, err = perm(); err != nil {
				out.PermanentMACError = err.Error()
			}
		}
		for i := range ifaces {
			ifaces[i].nic.PermanentMACAddress = permanentMAC(rootFS, perms, ifaces[i].nic.Name, ifaces[i].ifindex)
		}

		// The dial ignores ctx's cancellation, as the queries do: a forced refresh whose client has gone still stores its result as fresh, and
		// a dial failed by that cancellation would store a payload without ethtool's blocks. WithoutCancel also drops ctx's deadline; every
		// request is still bounded by the session's own timeout.
		session, err := dial(context.WithoutCancel(ctx))
		if err != nil {
			out.EthtoolError = err.Error()
			for _, i := range ifaces {
				out.NICs = append(out.NICs, i.nic)
			}
			return out, nil
		}
		// The session only read; closing it cannot lose anything the payload reports.
		defer func() { _ = session.Close() }()
		var stalled error
		for _, i := range ifaces {
			if stalled == nil {
				var keep bool
				if keep, stalled = addEthtool(session, rootFS, i.ifindex, &i.nic); !keep {
					continue
				}
			}
			out.NICs = append(out.NICs, i.nic)
		}
		if stalled != nil {
			out.EthtoolError = "ethtool queries stopped: " + stalled.Error()
		}
		return out, nil
	})
}

// readSysfsNIC reads the interface name's index, MAC address and parent device from sysfs. It reports false when the interface has no
// readable index or address: it was removed after the listing.
func readSysfsNIC(fsys fs.FS, name string) (NIC, uint32, bool) {
	ifindex, ok := readIfindex(fsys, name)
	if !ok {
		return NIC{}, 0, false
	}
	dir := classNetDir + "/" + name
	mac, err := fs.ReadFile(fsys, dir+"/address")
	if err != nil {
		return NIC{}, 0, false
	}
	nic := NIC{Name: name, Index: ifindex, MACAddress: strings.TrimSpace(string(mac))}

	// The listing links to the interface's directory, such as ../../devices/virtual/net/br0, and links in that directory resolve from it.
	if target, err := fs.ReadLink(fsys, dir); err == nil {
		nic.Virtual = strings.Contains(target, "/devices/virtual/")
		dir = path.Join(classNetDir, target)
	}
	if device, err := fs.ReadLink(fsys, dir+"/device"); err == nil {
		nic.PCIAddress = nicPCIAddress(path.Join(dir, device))
	}
	return nic, ifindex, true
}

// permanentMAC is the permanent address of the interface name with index ifindex: perms' entry for it, or else the bonding driver's
// record of a bond member's own address, or "" when neither has one other than zeros.
func permanentMAC(fsys fs.FS, perms map[uint32]string, name string, ifindex uint32) string {
	if addr := perms[ifindex]; !isZeroMAC(addr) {
		return addr
	}
	addr, err := fs.ReadFile(fsys, classNetDir+"/"+name+"/bonding_slave/perm_hwaddr")
	if err != nil {
		return ""
	}
	if s := strings.TrimSpace(string(addr)); !isZeroMAC(s) {
		return s
	}
	return ""
}

// readIfindex reads the interface name's index from sysfs, reporting false when sysfs no longer lists the interface.
func readIfindex(fsys fs.FS, name string) (uint32, bool) {
	index, err := fs.ReadFile(fsys, classNetDir+"/"+name+"/ifindex")
	if err != nil {
		return 0, false
	}
	ifindex, err := strconv.ParseUint(strings.TrimSpace(string(index)), 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(ifindex), true
}

// nicPCIAddress is the PCI address of an interface's parent device, the sysfs path device: the device itself, or, for a virtio-net
// interface, whose parent is virtio<N>, the PCI device the virtio device sits on. It is "" for any other parent, such as a USB interface.
func nicPCIAddress(device string) string {
	name := path.Base(device)
	if pciaddr.Valid(name) {
		return name
	}
	if strings.HasPrefix(name, "virtio") {
		if parent := path.Base(path.Dir(device)); pciaddr.Valid(parent) {
			return parent
		}
	}
	return ""
}

// addEthtool fills nic's link, FEC and features from session. keep is false when the interface is gone, so that it is dropped like one
// removed from sysfs. stalled is the error of a query that timed out: the socket is not answering, so asking about this interface or any
// other again would only wait out the timeout once more.
func addEthtool(session EthtoolSession, fsys fs.FS, ifindex uint32, nic *NIC) (keep bool, stalled error) {
	queries := []func() error{
		func() error {
			l, err := session.Link(ifindex, nic.Name)
			if err == nil {
				nic.Link = &NICLink{
					Port: l.Port, Autonegotiation: l.Autoneg, SpeedMbps: l.SpeedMbps, Duplex: l.Duplex, Lanes: l.Lanes,
					SupportedModes: nonNil(l.Supported), AdvertisedModes: nonNil(l.Advertised), PartnerModes: nonNil(l.Partner),
				}
			}
			return err
		},
		func() error {
			f, err := session.FEC(ifindex, nic.Name)
			if err == nil {
				nic.FEC = &NICFEC{Configured: nonNil(f.Configured), Active: f.Active, Auto: f.Auto}
			}
			return err
		},
		func() error {
			features, err := session.Features(ifindex, nic.Name)
			if err == nil {
				nic.Features = make(map[string]NICFeature, len(features))
				for name, f := range features {
					nic.Features[name] = NICFeature{Active: f.Active, Requested: f.Requested, Fixed: f.Fixed}
				}
			}
			return err
		},
	}
	for _, query := range queries {
		err := query()
		if err == nil || errors.Is(err, ethtool.ErrNotSupported) {
			continue
		}
		// ENODEV means the interface is gone, or that it is still registered but its device is not present (a driver reset, a fatal
		// firmware error, suspend), which the kernel checks for every query but FEATURES. Only sysfs tells the two apart: a detached
		// interface keeps its entry and index, and is reported with the error.
		if errors.Is(err, ethtool.ErrNoDevice) {
			if index, ok := readIfindex(fsys, nic.Name); !ok || index != ifindex {
				return false, nil
			}
		}
		// The session names the query in its error.
		if nic.EthtoolError == "" {
			nic.EthtoolError = err.Error()
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return true, err
		}
	}
	return true, nil
}

// nonNil is s, or an empty list for nil, so that a list in the payload is [] rather than null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

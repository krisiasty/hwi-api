package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/krisiasty/hwi-api/internal/ethtool"
)

// addNetInterface adds an interface whose sysfs directory is dir, under
// sys/devices, listed in sys/class/net as name, with its parent device at
// device (under sys/devices, "" for none).
func addNetInterface(m fstest.MapFS, name, dir string, ifindex int, mac, device string) {
	listing := "sys/class/net/" + name
	m[listing] = sysLink(listing, "sys/devices/"+dir)
	m["sys/devices/"+dir+"/ifindex"] = &fstest.MapFile{Data: fmt.Appendf(nil, "%d\n", ifindex)}
	m["sys/devices/"+dir+"/address"] = &fstest.MapFile{Data: []byte(mac + "\n")}
	if device != "" {
		link := "sys/devices/" + dir + "/device"
		m[link] = sysLink(link, "sys/devices/"+device)
	}
}

// netHost is a host with:
//   - enp59s0f0np0 (ifindex 4), a PCI NIC at 0000:3b:00.0 below a root port;
//   - eth0 (2), a virtio-net NIC whose device, virtio0, sits on PCI device 0000:00:03.0;
//   - br0 (5), a bridge, virtual and with no device link;
//   - eno1 (3), an onboard PCI NIC at 0000:00:1f.6;
//   - ens9 (6), a PCI NIC the kernel removes before ethtool asks about it (netHostEthtool removes it);
//   - ens10, listed but gone from sysfs by the time it is read;
//   - lo (1), and bonding_masters, the regular file the bonding driver adds.
func netHost() fstest.MapFS {
	m := fstest.MapFS{
		"sys/class/net/bonding_masters": {Data: []byte("bond0\n")},
		"sys/class/net/ens10":           sysLink("sys/class/net/ens10", "sys/devices/pci0000:00/0000:00:05.0/net/ens10"),
	}
	addNetInterface(m, "enp59s0f0np0", "pci0000:3a/0000:3a:00.0/0000:3b:00.0/net/enp59s0f0np0", 4, "b8:3f:d2:00:00:01",
		"pci0000:3a/0000:3a:00.0/0000:3b:00.0")
	addNetInterface(m, "eth0", "pci0000:00/0000:00:03.0/virtio0/net/eth0", 2, "52:54:00:00:00:02", "pci0000:00/0000:00:03.0/virtio0")
	addNetInterface(m, "br0", "virtual/net/br0", 5, "52:54:00:00:00:05", "")
	addNetInterface(m, "eno1", "pci0000:00/0000:00:1f.6/net/eno1", 3, "3c:ec:ef:00:00:03", "pci0000:00/0000:00:1f.6")
	addNetInterface(m, "ens9", "pci0000:00/0000:00:04.0/net/ens9", 6, "3c:ec:ef:00:00:06", "pci0000:00/0000:00:04.0")
	addNetInterface(m, "lo", "virtual/net/lo", 1, "00:00:00:00:00:00", "")
	return m
}

// fakeNICAnswers is what the fake ethtool session answers for one interface. A nil block with a nil error is unsupported.
type fakeNICAnswers struct {
	link        *ethtool.Link
	linkErr     error
	fec         *ethtool.FEC
	fecErr      error
	features    map[string]ethtool.Feature
	featuresErr error
}

// fakeEthtool answers by ifindex; an interface it does not know is gone.
type fakeEthtool struct {
	nics map[uint32]fakeNICAnswers
	// vanish, when set, is called with the ifindex of every query answered with ErrNoDevice, so that a test can take the interface out of
	// sysfs as the kernel would have.
	vanish func(ifindex uint32)
	asked  []fakeQuery // every query, in order
	closed bool
}

// fakeQuery is the interface one query asked about.
type fakeQuery struct {
	ifindex uint32
	name    string
}

func (f *fakeEthtool) Link(ifindex uint32, name string) (*ethtool.Link, error) {
	a, err := f.lookup(ifindex, name, func(a fakeNICAnswers) (bool, error) { return a.link != nil, a.linkErr })
	return a.link, err
}

func (f *fakeEthtool) FEC(ifindex uint32, name string) (*ethtool.FEC, error) {
	a, err := f.lookup(ifindex, name, func(a fakeNICAnswers) (bool, error) { return a.fec != nil, a.fecErr })
	return a.fec, err
}

func (f *fakeEthtool) Features(ifindex uint32, name string) (map[string]ethtool.Feature, error) {
	a, err := f.lookup(ifindex, name, func(a fakeNICAnswers) (bool, error) { return a.features != nil, a.featuresErr })
	return a.features, err
}

// lookup returns the answers for ifindex, or the error the kernel would give: ENODEV for an unknown interface, the block's error, or
// EOPNOTSUPP for a block with neither a value nor an error.
func (f *fakeEthtool) lookup(ifindex uint32, name string, block func(fakeNICAnswers) (bool, error)) (fakeNICAnswers, error) {
	f.asked = append(f.asked, fakeQuery{ifindex, name})
	a, err := f.answer(ifindex, block)
	if errors.Is(err, ethtool.ErrNoDevice) && f.vanish != nil {
		f.vanish(ifindex)
	}
	return a, err
}

func (f *fakeEthtool) answer(ifindex uint32, block func(fakeNICAnswers) (bool, error)) (fakeNICAnswers, error) {
	a, ok := f.nics[ifindex]
	if !ok {
		return fakeNICAnswers{}, ethtool.ErrNoDevice
	}
	has, err := block(a)
	switch {
	case err != nil:
		return fakeNICAnswers{}, err
	case !has:
		return fakeNICAnswers{}, ethtool.ErrNotSupported
	}
	return a, nil
}

// removeNetInterface takes name out of sys/class/net, as the kernel does when it unregisters the interface.
func removeNetInterface(m fstest.MapFS, name string) {
	delete(m, "sys/class/net/"+name)
}

func (f *fakeEthtool) Close() error {
	f.closed = true
	return nil
}

var (
	pciLink = &ethtool.Link{
		Port: "DA", Autoneg: new(false), SpeedMbps: 25000, Duplex: "full", Lanes: 1,
		Supported:  []string{"10000baseCR/Full", "25000baseCR/Full"},
		Advertised: []string{"25000baseCR/Full"},
		Partner:    []string{},
	}
	pciFEC      = &ethtool.FEC{Configured: []string{"RS", "BASER"}, Active: "RS", Auto: new(false)}
	pciFeatures = map[string]ethtool.Feature{
		"rx-checksum":     {Active: true, Requested: true},
		"rx-gro-hw":       {},
		"vlan-challenged": {Fixed: true},
	}
)

// netHostEthtool answers for netHost's interfaces in m, removing ens9 from m when it is asked about.
func netHostEthtool(m fstest.MapFS) *fakeEthtool {
	return &fakeEthtool{vanish: func(ifindex uint32) {
		if ifindex == 6 {
			removeNetInterface(m, "ens9")
		}
	}, nics: map[uint32]fakeNICAnswers{
		4: {link: pciLink, fec: pciFEC, features: pciFeatures},
		2: {
			link:     &ethtool.Link{Duplex: "full", Supported: []string{}, Advertised: []string{}, Partner: []string{}},
			features: map[string]ethtool.Feature{"tx-checksum-ip-generic": {Active: true, Requested: true}},
		},
		5: {features: map[string]ethtool.Feature{"tx-checksum-ip-generic": {Active: true, Requested: true, Fixed: true}}},
		3: {fec: &ethtool.FEC{Configured: []string{}}, featuresErr: fmt.Errorf("features: %w", syscall.EIO)},
		1: {features: map[string]ethtool.Feature{"loopback": {Active: true, Fixed: true}}},
	}}
}

func probeNetwork(t *testing.T, fsys fstest.MapFS, dial func(context.Context) (EthtoolSession, error)) *Network {
	t.Helper()
	v, err := NewNetworkProber(fsys, dial, nil).Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v.(*Network)
}

func TestNetworkFromSysfsAndEthtool(t *testing.T) {
	host := netHost()
	session := netHostEthtool(host)
	got := probeNetwork(t, host, func(context.Context) (EthtoolSession, error) { return session, nil })

	want := &Network{NICs: []NIC{
		{
			Name: "br0", Index: 5, MACAddress: "52:54:00:00:00:05", Virtual: true,
			Features: map[string]NICFeature{"tx-checksum-ip-generic": {Active: true, Requested: true, Fixed: true}},
		},
		{
			Name: "eno1", Index: 3, MACAddress: "3c:ec:ef:00:00:03", PCIAddress: "0000:00:1f.6",
			FEC:          &NICFEC{Configured: []string{}},
			EthtoolError: "features: input/output error",
		},
		{
			Name: "enp59s0f0np0", Index: 4, MACAddress: "b8:3f:d2:00:00:01", PCIAddress: "0000:3b:00.0",
			Link: &NICLink{
				Port: "DA", Autonegotiation: new(false), SpeedMbps: 25000, Duplex: "full", Lanes: 1,
				SupportedModes:  []string{"10000baseCR/Full", "25000baseCR/Full"},
				AdvertisedModes: []string{"25000baseCR/Full"},
				PartnerModes:    []string{},
			},
			FEC: &NICFEC{Configured: []string{"RS", "BASER"}, Active: "RS", Auto: new(false)},
			Features: map[string]NICFeature{
				"rx-checksum":     {Active: true, Requested: true},
				"rx-gro-hw":       {},
				"vlan-challenged": {Fixed: true},
			},
		},
		{
			Name: "eth0", Index: 2, MACAddress: "52:54:00:00:00:02", PCIAddress: "0000:00:03.0",
			Link:     &NICLink{Duplex: "full", SupportedModes: []string{}, AdvertisedModes: []string{}, PartnerModes: []string{}},
			Features: map[string]NICFeature{"tx-checksum-ip-generic": {Active: true, Requested: true}},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("network =\n%+v\nwant\n%+v", got, want)
	}
	if !session.closed {
		t.Error("the ethtool session was not closed")
	}
}

// A failed dial is reported once, at the top level: the kernel has no ethtool family, or an earlier request is still stuck in the kernel.
func TestNetworkWithoutEthtool(t *testing.T) {
	for _, dialErr := range []error{ethtool.ErrNoFamily, ethtool.ErrBusy} {
		t.Run(dialErr.Error(), func(t *testing.T) {
			got := probeNetwork(t, netHost(), func(context.Context) (EthtoolSession, error) {
				return nil, dialErr
			})

			if got.EthtoolError != dialErr.Error() {
				t.Errorf("ethtool_error = %q, want %q", got.EthtoolError, dialErr)
			}
			// Without ethtool nothing says ens9 is gone, so it stays.
			want := []NIC{
				{Name: "br0", Index: 5, MACAddress: "52:54:00:00:00:05", Virtual: true},
				{Name: "eno1", Index: 3, MACAddress: "3c:ec:ef:00:00:03", PCIAddress: "0000:00:1f.6"},
				{Name: "enp59s0f0np0", Index: 4, MACAddress: "b8:3f:d2:00:00:01", PCIAddress: "0000:3b:00.0"},
				{Name: "ens9", Index: 6, MACAddress: "3c:ec:ef:00:00:06", PCIAddress: "0000:00:04.0"},
				{Name: "eth0", Index: 2, MACAddress: "52:54:00:00:00:02", PCIAddress: "0000:00:03.0"},
			}
			if !reflect.DeepEqual(got.NICs, want) {
				t.Errorf("nics =\n%+v\nwant\n%+v", got.NICs, want)
			}
		})
	}
}

// Only the first failure is kept, as the session words it: *ethtool.Client names the query in every error.
func TestNetworkKeepsTheFirstEthtoolError(t *testing.T) {
	m := fstest.MapFS{}
	addNetInterface(m, "eno1", "pci0000:00/0000:00:1f.6/net/eno1", 3, "3c:ec:ef:00:00:03", "pci0000:00/0000:00:1f.6")
	for _, tc := range []struct {
		name    string
		answers fakeNICAnswers
		want    string
	}{
		{"link", fakeNICAnswers{linkErr: fmt.Errorf("link modes: %w", syscall.EIO), fecErr: errors.New("fec: busy")}, "link modes: input/output error"},
		{"fec", fakeNICAnswers{fecErr: errors.New("fec: busy"), featuresErr: fmt.Errorf("features: %w", syscall.EIO)}, "fec: busy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := &fakeEthtool{nics: map[uint32]fakeNICAnswers{3: tc.answers}}
			got := probeNetwork(t, m, func(context.Context) (EthtoolSession, error) { return session, nil })
			if len(got.NICs) != 1 || got.NICs[0].EthtoolError != tc.want {
				t.Errorf("nics = %+v, want one with ethtool_error %q", got.NICs, tc.want)
			}
		})
	}
}

// The payload's field names, and the mode lists' [] rather than null, are what consumers read.
func TestNetworkJSON(t *testing.T) {
	n := Network{NICs: []NIC{{
		Name: "eth0", Index: 2, MACAddress: "52:54:00:00:00:02", PermanentMACAddress: "3c:fd:fe:a1:b2:c3", PCIAddress: "0000:00:03.0",
		Link:     &NICLink{Duplex: "full", SupportedModes: []string{}, AdvertisedModes: []string{}, PartnerModes: []string{}},
		FEC:      &NICFEC{Configured: []string{}},
		Features: map[string]NICFeature{"rx-checksum": {Active: true, Requested: true}},
	}, {Name: "br0", Index: 5, MACAddress: "52:54:00:00:00:05", Virtual: true, EthtoolError: "features: input/output error"}},
		EthtoolError:      "ethtool queries stopped: fec: netlink: no reply: i/o timeout",
		PermanentMACError: "permanent addresses: operation not permitted",
	}
	got, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"nics":[` +
		`{"name":"eth0","index":2,"mac_address":"52:54:00:00:00:02","permanent_mac_address":"3c:fd:fe:a1:b2:c3","virtual":false,` +
		`"pci_address":"0000:00:03.0",` +
		`"link":{"duplex":"full","supported_modes":[],"advertised_modes":[],"partner_modes":[]},` +
		`"fec":{"configured":[]},` +
		`"features":{"rx-checksum":{"active":true,"requested":true,"fixed":false}}},` +
		`{"name":"br0","index":5,"mac_address":"52:54:00:00:00:05","virtual":true,"ethtool_error":"features: input/output error"}],` +
		`"ethtool_error":"ethtool queries stopped: fec: netlink: no reply: i/o timeout",` +
		`"permanent_mac_error":"permanent addresses: operation not permitted"}`
	if string(got) != want {
		t.Errorf("JSON =\n%s\nwant\n%s", got, want)
	}

	empty, err := json.Marshal(probeNetwork(t, fstest.MapFS{"sys/class/net/lo": sysLink("sys/class/net/lo", "sys/devices/virtual/net/lo")},
		func(context.Context) (EthtoolSession, error) { return &fakeEthtool{}, nil }))
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != `{"nics":[]}` {
		t.Errorf("a host with only lo = %s, want an empty list", empty)
	}
}

func TestNetworkFailsOnlyWhenTheListingFails(t *testing.T) {
	dialed := false
	_, err := NewNetworkProber(fstest.MapFS{}, func(context.Context) (EthtoolSession, error) {
		dialed = true
		return &fakeEthtool{}, nil
	}, nil).Probe(context.Background())
	if err == nil {
		t.Fatal("Probe succeeded without sys/class/net")
	}
	if dialed {
		t.Error("ethtool was dialed although the listing failed")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error = %v, want it to wrap fs.ErrNotExist", err)
	}
}

// ENODEV from a later query drops the interface as much as from the first, when sysfs no longer has it, or has another interface of the
// same name in its place.
func TestNetworkDropsAnInterfaceGoneMidQuery(t *testing.T) {
	const dir = "pci0000:00/0000:00:1f.6/net/eno1"
	gone := map[string]func(fstest.MapFS){
		"removed": func(m fstest.MapFS) { removeNetInterface(m, "eno1") },
		"replaced": func(m fstest.MapFS) {
			m["sys/devices/"+dir+"/ifindex"] = &fstest.MapFile{Data: []byte("9\n")}
		},
	}
	for _, tc := range []struct {
		name    string
		answers fakeNICAnswers
	}{
		{"fec", fakeNICAnswers{link: pciLink, fecErr: fmt.Errorf("fec: %w", ethtool.ErrNoDevice)}},
		{"features", fakeNICAnswers{link: pciLink, fec: pciFEC, featuresErr: fmt.Errorf("features: %w", ethtool.ErrNoDevice)}},
	} {
		for how, vanish := range gone {
			t.Run(tc.name+"/"+how, func(t *testing.T) {
				m := fstest.MapFS{}
				addNetInterface(m, "eno1", dir, 3, "3c:ec:ef:00:00:03", "pci0000:00/0000:00:1f.6")
				session := &fakeEthtool{nics: map[uint32]fakeNICAnswers{3: tc.answers}, vanish: func(uint32) { vanish(m) }}
				if got := probeNetwork(t, m, func(context.Context) (EthtoolSession, error) { return session, nil }); len(got.NICs) != 0 {
					t.Errorf("nics = %+v, want none", got.NICs)
				}
			})
		}
	}
}

// The kernel also answers ENODEV about an interface that is still registered but whose device is not present: a driver reset, a fatal
// firmware error or suspend. Sysfs still lists it under the same index, so it is kept, with the error, and asked the remaining queries.
func TestNetworkKeepsADetachedInterface(t *testing.T) {
	m := fstest.MapFS{}
	addNetInterface(m, "eno1", "pci0000:00/0000:00:1f.6/net/eno1", 3, "3c:ec:ef:00:00:03", "pci0000:00/0000:00:1f.6")
	session := &fakeEthtool{nics: map[uint32]fakeNICAnswers{3: {
		linkErr:  fmt.Errorf("link modes: %w", ethtool.ErrNoDevice),
		fecErr:   fmt.Errorf("fec: %w", ethtool.ErrNoDevice),
		features: pciFeatures,
	}}}
	got := probeNetwork(t, m, func(context.Context) (EthtoolSession, error) { return session, nil })

	want := []NIC{{
		Name: "eno1", Index: 3, MACAddress: "3c:ec:ef:00:00:03", PCIAddress: "0000:00:1f.6",
		Features: map[string]NICFeature{
			"rx-checksum":     {Active: true, Requested: true},
			"rx-gro-hw":       {},
			"vlan-challenged": {Fixed: true},
		},
		EthtoolError: "link modes: no such device",
	}}
	if !reflect.DeepEqual(got.NICs, want) {
		t.Errorf("nics =\n%+v\nwant\n%+v", got.NICs, want)
	}
	if got.EthtoolError != "" {
		t.Errorf("ethtool_error = %q, want none: the session still answers", got.EthtoolError)
	}
}

// A session that stops answering is not asked about the remaining interfaces, which keep their sysfs fields.
func TestNetworkStopsAskingAStalledSession(t *testing.T) {
	m := fstest.MapFS{}
	addNetInterface(m, "eno1", "pci0000:00/0000:00:1f.6/net/eno1", 3, "3c:ec:ef:00:00:03", "pci0000:00/0000:00:1f.6")
	addNetInterface(m, "eno2", "pci0000:00/0000:00:1f.7/net/eno2", 4, "3c:ec:ef:00:00:04", "pci0000:00/0000:00:1f.7")
	timeout := fmt.Errorf("fec: netlink: no reply: %w", os.ErrDeadlineExceeded)
	session := &fakeEthtool{nics: map[uint32]fakeNICAnswers{
		3: {link: pciLink, fecErr: timeout, features: pciFeatures},
		4: {link: pciLink, fec: pciFEC, features: pciFeatures},
	}}
	got := probeNetwork(t, m, func(context.Context) (EthtoolSession, error) { return session, nil })

	if want := "ethtool queries stopped: " + timeout.Error(); got.EthtoolError != want {
		t.Errorf("ethtool_error = %q, want %q", got.EthtoolError, want)
	}
	if want := []fakeQuery{{3, "eno1"}, {3, "eno1"}}; !reflect.DeepEqual(session.asked, want) {
		t.Errorf("queries asked about %v, want %v: link and fec of eno1 only", session.asked, want)
	}
	want := []NIC{
		{
			Name: "eno1", Index: 3, MACAddress: "3c:ec:ef:00:00:03", PCIAddress: "0000:00:1f.6",
			Link: &NICLink{
				Port: "DA", Autonegotiation: new(false), SpeedMbps: 25000, Duplex: "full", Lanes: 1,
				SupportedModes:  []string{"10000baseCR/Full", "25000baseCR/Full"},
				AdvertisedModes: []string{"25000baseCR/Full"},
				PartnerModes:    []string{},
			},
			EthtoolError: timeout.Error(),
		},
		{Name: "eno2", Index: 4, MACAddress: "3c:ec:ef:00:00:04", PCIAddress: "0000:00:1f.7"},
	}
	if !reflect.DeepEqual(got.NICs, want) {
		t.Errorf("nics =\n%+v\nwant\n%+v", got.NICs, want)
	}
	if !session.closed {
		t.Error("the ethtool session was not closed")
	}
}

// A probe whose context is done still asks about every interface: a forced refresh whose client goes away stores its result as fresh, so it
// must not be a degraded one. Each query is bounded by the session's own timeout instead.
func TestNetworkIgnoresADoneContext(t *testing.T) {
	probe := func(ctx context.Context) *Network {
		t.Helper()
		host := netHost()
		session := netHostEthtool(host)
		v, err := NewNetworkProber(host, func(context.Context) (EthtoolSession, error) { return session, nil }, nil).Probe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return v.(*Network)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, want := probe(ctx), probe(context.Background()); !reflect.DeepEqual(got, want) {
		t.Errorf("network with a cancelled context =\n%+v\nwant the same as with a live one\n%+v", got, want)
	}
}

// A forced refresh whose client has gone still opens ethtool: the dial must neither fail on the cancelled context nor leave the payload
// without ethtool's blocks.
func TestNetworkDialsDespiteADoneContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	host := netHost()
	session := netHostEthtool(host)
	v, err := NewNetworkProber(host, func(ctx context.Context) (EthtoolSession, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return session, nil
	}, nil).Probe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.(*Network); got.EthtoolError != "" {
		t.Errorf("ethtool_error = %q, want none: the dial got the probe's cancelled context", got.EthtoolError)
	}
	if len(session.asked) == 0 {
		t.Error("no ethtool query was asked")
	}
}

// bondHost is a host with bond0 (ifindex 7) over two PCI NICs, ens1f0 (3) and ens1f1 (4), and a bridge br0 (5). Both members carry
// bond0's address, which is ens1f0's own; ens1f1's own address is only its permanent one. The bonding driver's
// bonding_slave/perm_hwaddr holds each member's permanent address when withPermHwaddr is set. The bridge has no permanent address anywhere.
func bondHost(withPermHwaddr bool) fstest.MapFS {
	m := fstest.MapFS{"sys/class/net/bonding_masters": {Data: []byte("bond0\n")}}
	addNetInterface(m, "ens1f0", "pci0000:3a/0000:3a:00.0/0000:3b:00.0/net/ens1f0", 3, "3c:fd:fe:00:00:30", "pci0000:3a/0000:3a:00.0/0000:3b:00.0")
	addNetInterface(m, "ens1f1", "pci0000:3a/0000:3a:00.0/0000:3b:00.1/net/ens1f1", 4, "3c:fd:fe:00:00:30", "pci0000:3a/0000:3a:00.0/0000:3b:00.1")
	addNetInterface(m, "bond0", "virtual/net/bond0", 7, "3c:fd:fe:00:00:30", "")
	addNetInterface(m, "br0", "virtual/net/br0", 5, "52:54:00:00:00:05", "")
	if withPermHwaddr {
		m["sys/devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0/net/ens1f0/bonding_slave/perm_hwaddr"] = &fstest.MapFile{Data: []byte("3c:fd:fe:00:00:30\n")}
		m["sys/devices/pci0000:3a/0000:3a:00.0/0000:3b:00.1/net/ens1f1/bonding_slave/perm_hwaddr"] = &fstest.MapFile{Data: []byte("3c:fd:fe:00:00:31\n")}
	}
	return m
}

// bondHostEthtool answers for bondHost's interfaces, each supporting no query.
func bondHostEthtool() *fakeEthtool {
	return &fakeEthtool{nics: map[uint32]fakeNICAnswers{3: {}, 4: {}, 5: {}, 7: {}}}
}

func TestNetworkPermanentAddresses(t *testing.T) {
	// The dump: each member's own address; bond0 and br0 have none, as the kernel reports none for a virtual interface.
	dump := map[uint32]string{3: "3c:fd:fe:00:00:30", 4: "3c:fd:fe:00:00:31"}
	dumpErr := errors.New("permanent addresses: no reply: i/o timeout")
	members := []NIC{
		{Name: "ens1f0", Index: 3, MACAddress: "3c:fd:fe:00:00:30", PermanentMACAddress: "3c:fd:fe:00:00:30", PCIAddress: "0000:3b:00.0"},
		{Name: "ens1f1", Index: 4, MACAddress: "3c:fd:fe:00:00:30", PermanentMACAddress: "3c:fd:fe:00:00:31", PCIAddress: "0000:3b:00.1"},
	}
	noPermanent := []NIC{
		{Name: "ens1f0", Index: 3, MACAddress: "3c:fd:fe:00:00:30", PCIAddress: "0000:3b:00.0"},
		{Name: "ens1f1", Index: 4, MACAddress: "3c:fd:fe:00:00:30", PCIAddress: "0000:3b:00.1"},
	}
	virtual := []NIC{
		{Name: "bond0", Index: 7, MACAddress: "3c:fd:fe:00:00:30", Virtual: true},
		{Name: "br0", Index: 5, MACAddress: "52:54:00:00:00:05", Virtual: true},
	}
	for _, tc := range []struct {
		name           string
		withPermHwaddr bool
		perm           func() (map[uint32]string, error)
		want           []NIC
		wantErr        string
	}{
		{"from the dump", false, func() (map[uint32]string, error) { return dump, nil }, members, ""},
		{"from sysfs without a dump", true, nil, members, ""},
		{"from sysfs when the dump fails", true, func() (map[uint32]string, error) { return nil, dumpErr }, members, dumpErr.Error()},
		{"from sysfs when the dump has none", true, func() (map[uint32]string, error) { return map[uint32]string{}, nil }, members, ""},
		{
			"from the dump before sysfs", true,
			func() (map[uint32]string, error) {
				return map[uint32]string{3: "3c:fd:fe:00:00:40", 4: "3c:fd:fe:00:00:41"}, nil
			},
			[]NIC{
				{Name: "ens1f0", Index: 3, MACAddress: "3c:fd:fe:00:00:30", PermanentMACAddress: "3c:fd:fe:00:00:40", PCIAddress: "0000:3b:00.0"},
				{Name: "ens1f1", Index: 4, MACAddress: "3c:fd:fe:00:00:30", PermanentMACAddress: "3c:fd:fe:00:00:41", PCIAddress: "0000:3b:00.1"},
			},
			"",
		},
		{"nowhere", false, nil, noPermanent, ""},
		{"nowhere when the dump fails", false, func() (map[uint32]string, error) { return nil, dumpErr }, noPermanent, dumpErr.Error()},
		{
			"never all zeros", false,
			func() (map[uint32]string, error) {
				return map[uint32]string{3: "00:00:00:00:00:00", 4: "00:00:00:00:00:00", 5: "00:00:00:00:00:00", 7: "00:00:00:00:00:00"}, nil
			},
			noPermanent, "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := bondHost(tc.withPermHwaddr)
			session := bondHostEthtool()
			perm := tc.perm
			calls := 0
			if perm != nil {
				perm = func() (map[uint32]string, error) {
					calls++
					if len(session.asked) != 0 {
						t.Error("the permanent addresses were dumped after the ethtool queries")
					}
					return tc.perm()
				}
			}
			v, err := NewNetworkProber(host, func(context.Context) (EthtoolSession, error) { return session, nil }, perm).
				Probe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got := v.(*Network)
			want := &Network{NICs: append(append([]NIC{}, virtual...), tc.want...), PermanentMACError: tc.wantErr}
			// sorted by name: bond0, br0, ens1f0, ens1f1
			if !reflect.DeepEqual(got, want) {
				t.Errorf("network =\n%+v\nwant\n%+v", got, want)
			}
			if perm != nil && calls != 1 {
				t.Errorf("the permanent addresses were dumped %d times, want once", calls)
			}
		})
	}
}

// A sysfs perm_hwaddr of zeros is no permanent address either.
func TestNetworkIgnoresAZeroPermHwaddr(t *testing.T) {
	host := bondHost(false)
	host["sys/devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0/net/ens1f0/bonding_slave/perm_hwaddr"] = &fstest.MapFile{Data: []byte("00:00:00:00:00:00\n")}
	got := probeNetwork(t, host, func(context.Context) (EthtoolSession, error) { return bondHostEthtool(), nil })
	for _, nic := range got.NICs {
		if nic.PermanentMACAddress != "" {
			t.Errorf("%s has permanent_mac_address %q, want none", nic.Name, nic.PermanentMACAddress)
		}
	}
}

// Without ethtool the NICs still carry their permanent addresses, and both errors are reported.
func TestNetworkPermanentAddressesWithoutEthtool(t *testing.T) {
	dumpErr := errors.New("permanent addresses: a previous request is still blocked in the kernel")
	v, err := NewNetworkProber(bondHost(true), func(context.Context) (EthtoolSession, error) { return nil, ethtool.ErrNoFamily },
		func() (map[uint32]string, error) { return nil, dumpErr }).Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := v.(*Network)
	if got.EthtoolError != ethtool.ErrNoFamily.Error() || got.PermanentMACError != dumpErr.Error() {
		t.Errorf("ethtool_error = %q, permanent_mac_error = %q; want %q and %q", got.EthtoolError, got.PermanentMACError, ethtool.ErrNoFamily,
			dumpErr)
	}
	perms := map[string]string{}
	for _, nic := range got.NICs {
		perms[nic.Name] = nic.PermanentMACAddress
	}
	if want := map[string]string{"bond0": "", "br0": "", "ens1f0": "3c:fd:fe:00:00:30", "ens1f1": "3c:fd:fe:00:00:31"}; !reflect.DeepEqual(perms, want) {
		t.Errorf("permanent addresses = %v, want %v", perms, want)
	}
}

// A listing that fails fails the probe before the dump is made.
func TestNetworkDoesNotDumpWhenTheListingFails(t *testing.T) {
	dumped := false
	_, err := NewNetworkProber(fstest.MapFS{}, func(context.Context) (EthtoolSession, error) { return &fakeEthtool{}, nil },
		func() (map[uint32]string, error) {
			dumped = true
			return nil, nil
		}).Probe(context.Background())
	if err == nil {
		t.Fatal("Probe succeeded without sys/class/net")
	}
	if dumped {
		t.Error("the permanent addresses were dumped although the listing failed")
	}
}

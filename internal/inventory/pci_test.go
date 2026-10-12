package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/krisiasty/hwi-api/internal/helper"
	"github.com/krisiasty/hwi-api/internal/pciids"
)

// sysLink is a symlink at from pointing to to, both relative to the root,
// with a relative target as sysfs writes its links.
func sysLink(from, to string) *fstest.MapFile {
	up := strings.Repeat("../", strings.Count(from, "/"))
	return &fstest.MapFile{Data: []byte(up + to), Mode: fs.ModeSymlink}
}

// addPCIDevice adds a device at tree, its path under sys/devices, and its
// listing in sys/bus/pci/devices. files maps names under the device's
// directory to contents; a value starting with "->" is instead a symlink
// to the rest, a path relative to the root.
func addPCIDevice(m fstest.MapFS, tree string, files map[string]string) {
	listing := pciDevicesDir + "/" + path.Base(tree)
	m[listing] = sysLink(listing, "sys/devices/"+tree)
	for name, v := range files {
		p := "sys/devices/" + tree + "/" + name
		if target, ok := strings.CutPrefix(v, "->"); ok {
			m[p] = sysLink(p, target)
			continue
		}
		m[p] = &fstest.MapFile{Data: []byte(v + "\n")}
	}
}

// pciDeviceDir is the path a device's files are read through.
func pciDeviceDir(address string) string { return pciDevicesDir + "/" + address }

// pciHost is a host with:
//   - a root port, 0000:3a:00.0, directly under host bridge pci0000:3a;
//   - a ConnectX-6 Dx physical function, 0000:3b:00.0, below it in slot 3,
//     with two virtual functions configured;
//   - the first of those, 0000:3b:00.2, which shares the card's slot and,
//     like every VF, has link files reading "Unknown" and 0;
//   - an onboard NIC, 0000:00:1f.6, with a firmware label, no PCIe link and
//     no NUMA affinity;
//   - an NVMe drive, 0000:5e:00.0, in slot 5, below a root port the fixture
//     does not list;
//
// and an empty slot 7.
func pciHost() fstest.MapFS {
	m := fstest.MapFS{
		"sys/bus/pci/drivers/pcieport/bind":    {Data: nil},
		"sys/bus/pci/drivers/mlx5_core/module": sysLink("sys/bus/pci/drivers/mlx5_core/module", "sys/module/mlx5_core"),
		"sys/bus/pci/drivers/e1000e/module":    sysLink("sys/bus/pci/drivers/e1000e/module", "sys/module/e1000e"),
		"sys/bus/pci/drivers/nvme/module":      sysLink("sys/bus/pci/drivers/nvme/module", "sys/module/nvme"),
		"sys/bus/pci/slots/3/address":          {Data: []byte("0000:3b:00\n")},
		"sys/bus/pci/slots/5/address":          {Data: []byte("0000:5e:00\n")},
		"sys/bus/pci/slots/7/address":          {Data: []byte("0000:d8:00\n")},
	}
	addPCIDevice(m, "pci0000:3a/0000:3a:00.0", map[string]string{
		"vendor": "0x8086", "device": "0x347a", "subsystem_vendor": "0x8086", "subsystem_device": "0x0000",
		"class": "0x060400", "revision": "0x04", "driver": "->sys/bus/pci/drivers/pcieport",
		"iommu_group": "->sys/kernel/iommu_groups/7", "numa_node": "0", "power_state": "D0",
		"current_link_speed": "16.0 GT/s PCIe", "current_link_width": "16",
		"max_link_speed": "16.0 GT/s PCIe", "max_link_width": "16",
	})
	addPCIDevice(m, "pci0000:3a/0000:3a:00.0/0000:3b:00.0", map[string]string{
		"vendor": "0x15b3", "device": "0x101d", "subsystem_vendor": "0x15b3", "subsystem_device": "0x0016",
		"class": "0x020000", "revision": "0x00", "driver": "->sys/bus/pci/drivers/mlx5_core",
		"iommu_group": "->sys/kernel/iommu_groups/42", "numa_node": "0", "power_state": "D0",
		"current_link_speed": "8.0 GT/s PCIe", "current_link_width": "8",
		"max_link_speed": "16.0 GT/s PCIe", "max_link_width": "16",
		"sriov_totalvfs": "8", "sriov_numvfs": "2",
		"virtfn0":                     "->sys/devices/pci0000:3a/0000:3a:00.0/0000:3b:00.2",
		"virtfn1":                     "->sys/devices/pci0000:3a/0000:3a:00.0/0000:3b:00.3",
		"net/enp59s0f0np0/ifindex":    "4",
		"infiniband/mlx5_0/node_type": "1: CA",
	})
	addPCIDevice(m, "pci0000:3a/0000:3a:00.0/0000:3b:00.2", map[string]string{
		"vendor": "0x15b3", "device": "0x101e", "subsystem_vendor": "0x15b3", "subsystem_device": "0x0016",
		"class": "0x020000", "revision": "0x00", "driver": "->sys/bus/pci/drivers/mlx5_core",
		"iommu_group": "->sys/kernel/iommu_groups/43", "numa_node": "0",
		"current_link_speed": "Unknown", "current_link_width": "0",
		"max_link_speed": "Unknown", "max_link_width": "0",
		"physfn":                  "->sys/devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0",
		"net/enp59s0f0v0/ifindex": "9",
	})
	addPCIDevice(m, "pci0000:00/0000:00:1f.6", map[string]string{
		"vendor": "0x8086", "device": "0x15bb", "subsystem_vendor": "0x1028", "subsystem_device": "0x0a20",
		"class": "0x020000", "revision": "0x10", "driver": "->sys/bus/pci/drivers/e1000e",
		"iommu_group": "->sys/kernel/iommu_groups/12", "numa_node": "-1", "power_state": "D0",
		"label":            "Embedded NIC 1",
		"net/eno1/ifindex": "2",
	})
	addPCIDevice(m, "pci0000:5d/0000:5d:02.0/0000:5e:00.0", map[string]string{
		"vendor": "0x144d", "device": "0xa80a", "subsystem_vendor": "0x144d", "subsystem_device": "0xa813",
		"class": "0x010802", "revision": "0x00", "driver": "->sys/bus/pci/drivers/nvme",
		"iommu_group": "->sys/kernel/iommu_groups/60", "numa_node": "1", "power_state": "D0",
		"current_link_speed": "16.0 GT/s PCIe", "current_link_width": "4",
		"max_link_speed": "16.0 GT/s PCIe", "max_link_width": "4",
		"nvme/nvme0/dev": "259:0",
	})
	return m
}

// deniedFS is fsys with the named paths unreadable, as a file mode or a
// kernel permission check makes them for a non-root reader.
type deniedFS struct {
	fs.FS
	denied map[string]bool
}

func (d deniedFS) Open(name string) (fs.File, error) {
	if d.denied[name] {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	return d.FS.Open(name)
}

func (d deniedFS) ReadLink(name string) (string, error) {
	if d.denied[name] {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrPermission}
	}
	return fs.ReadLink(d.FS, name)
}

func (d deniedFS) Lstat(name string) (fs.FileInfo, error) { return fs.Lstat(d.FS, name) }

// testIDs names the fixture's devices. The ConnectX-6 Dx lists one
// subsystem ID under two vendors, the other first, so a lookup that ignores
// the subsystem vendor picks the wrong name.
const testIDs = "15b3  Mellanox Technologies\n" +
	"\t101d  MT2892 Family [ConnectX-6 Dx]\n" +
	"\t\t1028 0016  Dell-branded ConnectX-6 Dx\n" +
	"\t\t15b3 0016  ConnectX-6 Dx EN adapter card\n" +
	"8086  Intel Corporation\n" +
	"10de  NVIDIA Corporation\n" +
	"\t2bb5  GB202GL [RTX PRO 6000 Blackwell Server Edition]\n" +
	"1a03  ASPEED Technology, Inc.\n" +
	"\t2000  ASPEED Graphics Family\n" +
	"C 02  Network controller\n" +
	"\t00  Ethernet controller\n" +
	"C 03  Display controller\n" +
	"\t00  VGA compatible controller\n" +
	"\t02  3D controller\n" +
	"C 0c  Serial bus controller\n" +
	"\t03  USB controller\n" +
	"\t\t30  XHCI\n"

func testDB(t *testing.T) *pciids.DB {
	t.Helper()
	db, err := pciids.Parse(strings.NewReader(testIDs))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func probePCI(t *testing.T, fsys fs.FS, db *pciids.DB) *PCI {
	t.Helper()
	return probePCIWith(t, fsys, db, nil)
}

func probePCIWith(t *testing.T, fsys fs.FS, db *pciids.DB, privileged fs.FS) *PCI {
	t.Helper()
	v, err := NewPCIProber(fsys, db, privileged).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return v.(*PCI)
}

func pciDevice(t *testing.T, p *PCI, address string) PCIDevice {
	t.Helper()
	for _, d := range p.Devices {
		if d.Address == address {
			return d
		}
	}
	t.Fatalf("no device %s in %+v", address, p.Devices)
	return PCIDevice{}
}

func TestPCIProberListsDevicesInAddressOrder(t *testing.T) {
	p := probePCI(t, pciHost(), testDB(t))

	var got []string
	for _, d := range p.Devices {
		got = append(got, d.Address)
	}
	want := []string{"0000:00:1f.6", "0000:3a:00.0", "0000:3b:00.0", "0000:3b:00.2", "0000:5e:00.0"}
	if !slices.Equal(got, want) {
		t.Errorf("addresses = %v, want %v", got, want)
	}
}

// The wire shape of one fully described device, field by field.
func TestPCIDevicePayload(t *testing.T) {
	d := pciDevice(t, probePCI(t, pciHost(), testDB(t)), "0000:3b:00.0")
	got, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"address":"0000:3b:00.0","parent_address":"0000:3a:00.0",` +
		`"vendor":{"id":"15b3","name":"Mellanox Technologies"},` +
		`"product":{"id":"101d","name":"MT2892 Family [ConnectX-6 Dx]"},` +
		`"subsystem":{"vendor_id":"15b3","id":"0016","name":"ConnectX-6 Dx EN adapter card"},` +
		`"class":{"id":"02","name":"Network controller"},` +
		`"subclass":{"id":"00","name":"Ethernet controller"},` +
		`"programming_interface":{"id":"00","name":"unknown"},` +
		`"revision":"0x00","driver":"mlx5_core","driver_module":"mlx5_core","iommu_group":"42",` +
		`"numa_node":0,"power_state":"D0","slot":{"name":"3"},` +
		`"kernel_names":{"infiniband":["mlx5_0"],"net":["enp59s0f0np0"]},` +
		`"link":{"current_speed":"8.0 GT/s PCIe","current_width":8,"max_speed":"16.0 GT/s PCIe","max_width":16},` +
		`"sriov":{"total_vfs":8,"num_vfs":2,"vfs":["0000:3b:00.2","0000:3b:00.3"]}}`
	if string(got) != want {
		t.Errorf("payload =\n%s\nwant\n%s", got, want)
	}
}

func TestPCIProberPlacesEachDevice(t *testing.T) {
	p := probePCI(t, pciHost(), testDB(t))
	zero, one := 0, 1
	tests := []struct {
		address      string
		parent       string
		numa         *int
		slot         string
		label        string
		driver       string
		driverModule string
		iommuGroup   string
		kernelNames  map[string][]string
	}{
		// Directly under a host bridge, so no parent; a built-in driver has
		// no module.
		{address: "0000:3a:00.0", numa: &zero, driver: "pcieport", iommuGroup: "7"},
		// A virtual function shares its card's slot: the slot address has
		// no function number.
		{address: "0000:3b:00.2", parent: "0000:3a:00.0", numa: &zero, slot: "3", driver: "mlx5_core", driverModule: "mlx5_core",
			iommuGroup: "43", kernelNames: map[string][]string{"net": {"enp59s0f0v0"}}},
		// numa_node -1 means no affinity: omitted, not 0 or -1.
		{address: "0000:00:1f.6", label: "Embedded NIC 1", driver: "e1000e", driverModule: "e1000e", iommuGroup: "12",
			kernelNames: map[string][]string{"net": {"eno1"}}},
		// The parent is reported even though the fixture does not list it.
		{address: "0000:5e:00.0", parent: "0000:5d:02.0", numa: &one, slot: "5", driver: "nvme", driverModule: "nvme", iommuGroup: "60",
			kernelNames: map[string][]string{"nvme": {"nvme0"}}},
	}
	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			d := pciDevice(t, p, tt.address)
			if d.ParentAddress != tt.parent {
				t.Errorf("ParentAddress = %q, want %q", d.ParentAddress, tt.parent)
			}
			if (d.NUMANode == nil) != (tt.numa == nil) || (d.NUMANode != nil && *d.NUMANode != *tt.numa) {
				t.Errorf("NUMANode = %v, want %v", d.NUMANode, tt.numa)
			}
			gotSlot := ""
			if d.Slot != nil {
				gotSlot = d.Slot.Name
			}
			if gotSlot != tt.slot {
				t.Errorf("Slot = %q, want %q", gotSlot, tt.slot)
			}
			if d.Label != tt.label {
				t.Errorf("Label = %q, want %q", d.Label, tt.label)
			}
			if d.Driver != tt.driver || d.DriverModule != tt.driverModule {
				t.Errorf("Driver, DriverModule = %q, %q, want %q, %q", d.Driver, d.DriverModule, tt.driver, tt.driverModule)
			}
			if d.IOMMUGroup != tt.iommuGroup {
				t.Errorf("IOMMUGroup = %q, want %q", d.IOMMUGroup, tt.iommuGroup)
			}
			if len(d.KernelNames) != len(tt.kernelNames) {
				t.Fatalf("KernelNames = %v, want %v", d.KernelNames, tt.kernelNames)
			}
			for kind, names := range tt.kernelNames {
				if !slices.Equal(d.KernelNames[kind], names) {
					t.Errorf("KernelNames[%s] = %v, want %v", kind, d.KernelNames[kind], names)
				}
			}
		})
	}
}

// A file that exists but cannot be read gives "unknown"; one that does not
// exist leaves the field out.
func TestPCIProberReportsUnreadableFieldsAsUnknown(t *testing.T) {
	dir := pciDeviceDir("0000:5e:00.0")
	fsys := deniedFS{FS: pciHost(), denied: map[string]bool{
		dir + "/vendor": true, dir + "/class": true, dir + "/power_state": true, dir + "/driver": true,
	}}
	d := pciDevice(t, probePCI(t, fsys, testDB(t)), "0000:5e:00.0")

	if d.Vendor != (PCIIdent{ID: "unknown", Name: "unknown"}) {
		t.Errorf("Vendor = %+v", d.Vendor)
	}
	unknown := PCIIdent{ID: "unknown", Name: "unknown"}
	if d.Class != unknown || d.Subclass != unknown || d.ProgrammingInterface != unknown {
		t.Errorf("Class, Subclass, ProgrammingInterface = %+v, %+v, %+v", d.Class, d.Subclass, d.ProgrammingInterface)
	}
	if d.PowerState != "unknown" || d.Driver != "unknown" {
		t.Errorf("PowerState, Driver = %q, %q, want unknown", d.PowerState, d.Driver)
	}
	if d.Product.ID != "a80a" {
		t.Errorf("Product.ID = %q; the readable fields must still be read", d.Product.ID)
	}
}

// An enumeration that fails is a probe failure, never an empty list.
func TestPCIProberFailsWhenDevicesCannotBeListed(t *testing.T) {
	_, err := NewPCIProber(fstest.MapFS{}, testDB(t), nil).Probe(context.Background())
	if err == nil {
		t.Fatal("Probe succeeded without sys/bus/pci/devices")
	}
}

func TestPCIProberReportsAnEmptyBus(t *testing.T) {
	fsys := fstest.MapFS{pciDevicesDir: {Mode: fs.ModeDir | 0o755}}
	p := probePCI(t, fsys, testDB(t))
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"devices":[]`)) {
		t.Errorf("payload = %s, want an empty devices list, not null", b)
	}
}

// A device removed between the listing and its reads is left out.
func TestPCIProberSkipsAVanishedDevice(t *testing.T) {
	m := pciHost()
	gone := pciDeviceDir("0000:af:00.0")
	m[gone] = sysLink(gone, "sys/devices/pci0000:ae/0000:ae:00.0/0000:af:00.0")

	p := probePCI(t, m, testDB(t))
	if len(p.Devices) != 5 {
		t.Errorf("%d devices, want the 5 still present", len(p.Devices))
	}
	for _, d := range p.Devices {
		if d.Address == "0000:af:00.0" {
			t.Errorf("vanished device listed: %+v", d)
		}
	}
}

func TestPCIAddressPattern(t *testing.T) {
	for s, want := range map[string]bool{
		"0000:3b:00.0":  true,
		"10000:01:00.0": true, // an Intel VMD domain
		"pci0000:3a":    false,
		"0000:3b:00.8":  false,
		"0000:3b:00":    false,
	} {
		if got := pciAddress.MatchString(s); got != want {
			t.Errorf("pciAddress matches %q = %v, want %v", s, got, want)
		}
	}
}

func TestPCISlotAddress(t *testing.T) {
	for in, want := range map[string]string{
		"0000:3b:00.2":  "0000:3b:00",
		"10000:01:00.0": "10000:01:00",
		"no-function":   "no-function",
	} {
		if got := pciSlotAddress(in); got != want {
			t.Errorf("pciSlotAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

// With ARI, a physical function's VFs often sit at device numbers the
// slot's address does not cover, such as 0000:3b:01.0 for a card in slot
// 0000:3b:00. Each VF takes its physical function's slot.
func TestPCIProberGivesAVFItsPFsSlot(t *testing.T) {
	m := pciHost()
	addPCIDevice(m, "pci0000:3a/0000:3a:00.0/0000:3b:01.0", map[string]string{
		"vendor": "0x15b3", "device": "0x101e", "class": "0x020000",
		"physfn": "->sys/devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0",
	})

	d := pciDevice(t, probePCI(t, m, testDB(t)), "0000:3b:01.0")
	if d.Slot == nil || d.Slot.Name != "3" {
		t.Errorf("Slot = %+v, want its physical function's slot 3", d.Slot)
	}
}

// vanishingFS removes a device, as a hot-unplug or a VF teardown does, the
// moment trigger is opened: partway through the device's reads.
type vanishingFS struct {
	m       fstest.MapFS
	trigger string
	listing string
}

func (v vanishingFS) Open(name string) (fs.File, error) {
	f, err := v.m.Open(name)
	if name == v.trigger {
		delete(v.m, v.listing)
	}
	return f, err
}

func (v vanishingFS) ReadLink(name string) (string, error) { return fs.ReadLink(v.m, name) }

func (v vanishingFS) Lstat(name string) (fs.FileInfo, error) { return fs.Lstat(v.m, name) }

// A device removed while its files are being read is left out, not
// reported with whatever its remaining reads gave.
func TestPCIProberSkipsADeviceRemovedDuringItsReads(t *testing.T) {
	dir := pciDeviceDir("0000:5e:00.0")
	fsys := vanishingFS{m: pciHost(), trigger: dir + "/vendor", listing: dir}

	p := probePCI(t, fsys, testDB(t))
	for _, d := range p.Devices {
		if d.Address == "0000:5e:00.0" {
			t.Errorf("device removed during its reads listed: %+v", d)
		}
	}
	if len(p.Devices) != 4 {
		t.Errorf("%d devices, want the 4 still present", len(p.Devices))
	}
}

// A slot whose address has no device number, domain:bus, as the kernel
// writes it for a slot not tied to one device number, covers every device
// on that bus without a slot of its own, as lspci reads it.
func TestPCIProberMatchesABusWideSlot(t *testing.T) {
	m := pciHost()
	m["sys/bus/pci/slots/8/address"] = &fstest.MapFile{Data: []byte("0000:5e\n")}
	m["sys/bus/pci/slots/9/address"] = &fstest.MapFile{Data: []byte("0000:3a\n")}
	p := probePCI(t, m, testDB(t))

	if s := pciDevice(t, p, "0000:3a:00.0").Slot; s == nil || s.Name != "9" {
		t.Errorf("root port Slot = %+v, want the bus-wide slot 9", s)
	}
	if s := pciDevice(t, p, "0000:5e:00.0").Slot; s == nil || s.Name != "5" {
		t.Errorf("NVMe Slot = %+v, want its own slot 5 over the bus-wide 8", s)
	}
}

// A virtio device's kernel names sit one level down, under virtio<N>, as
// a VM's virtio NICs do.
func TestPCIProberReadsVirtioKernelNames(t *testing.T) {
	m := pciHost()
	addPCIDevice(m, "pci0000:00/0000:00:03.0", map[string]string{
		"vendor": "0x1af4", "device": "0x1041", "class": "0x020000",
		"virtio1/net/eth0/ifindex": "2",
		"virtio1/features":         "0110",
	})

	d := pciDevice(t, probePCI(t, m, testDB(t)), "0000:00:03.0")
	if got := d.KernelNames["net"]; !slices.Equal(got, []string{"eth0"}) || len(d.KernelNames) != 1 {
		t.Errorf("KernelNames = %v, want net: [eth0]", d.KernelNames)
	}
}

// mlxVPD is the ConnectX card's VPD in pciHost.
func mlxVPD() []byte {
	return vpdImage("ConnectX-6 Dx EN adapter card, 25GbE", []string{
		"PN", "MCX621102AN-ADAT", "EC", "A6", "SN", "MT2140X12345", "V0", "PCIeGen4 x8",
	}, nil)
}

// withVPD is pciHost with a vpd file on the ConnectX physical function.
// The daemon only checks that the file exists; it reads it through the
// helper.
func withVPD() fstest.MapFS {
	m := pciHost()
	m["sys/devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0/vpd"] = &fstest.MapFile{Mode: 0o600}
	return m
}

func TestPCIProberReadsVPDThroughTheHelper(t *testing.T) {
	privileged := fstest.MapFS{"vpd/0000:3b:00.0": {Data: mlxVPD()}}
	p := probePCIWith(t, withVPD(), testDB(t), privileged)
	d := pciDevice(t, p, "0000:3b:00.0")
	if d.VPDError != "" || d.VPD == nil || d.VPD.PartNumber != "MCX621102AN-ADAT" || d.VPD.SerialNumber != "MT2140X12345" {
		t.Errorf("vpd = %+v, vpd_error %q", d.VPD, d.VPDError)
	}
	// A device without a vpd file has neither.
	if nic := pciDevice(t, p, "0000:00:1f.6"); nic.VPD != nil || nic.VPDError != "" {
		t.Errorf("onboard NIC: vpd %+v, vpd_error %q; want neither", nic.VPD, nic.VPDError)
	}
}

// Review focus 3: whatever stops one device's VPD costs that VPD only.
func TestPCIProberReportsWhyAVPDIsMissing(t *testing.T) {
	down := helper.NewFS(func(helper.Request) (helper.Response, error) {
		return helper.Response{}, fmt.Errorf("%w: connect: no such file or directory", helper.ErrUnavailable)
	})
	hung := helper.NewFS(func(helper.Request) (helper.Response, error) {
		return helper.Response{Error: helper.ErrCodeTimeout, Message: "the read did not finish in time"}, nil
	})
	for name, tc := range map[string]struct {
		privileged fs.FS
		want       string
	}{
		"no helper":      {nil, "the privileged helper is not configured"},
		"helper down":    {down, "unavailable"},
		"read hung":      {hung, "timeout"},
		"device gone":    {fstest.MapFS{}, "file does not exist"},
		"malformed data": {fstest.MapFS{"vpd/0000:3b:00.0": {Data: []byte{0x90, 9, 0}}}, "decoding the VPD"},
	} {
		d := pciDevice(t, probePCIWith(t, withVPD(), testDB(t), tc.privileged), "0000:3b:00.0")
		if d.VPD != nil || !strings.Contains(d.VPDError, tc.want) {
			t.Errorf("%s: vpd %+v, vpd_error %q; want none and an error containing %q", name, d.VPD, d.VPDError, tc.want)
		}
		if d.Driver != "mlx5_core" {
			t.Errorf("%s: driver %q; the rest of the device must still be read", name, d.Driver)
		}
	}
}

// A display controller's VPD is read like any other device's.
func TestPCIProberReadsADisplayControllersVPD(t *testing.T) {
	m := pciHost()
	addPCIDevice(m, "pci0000:00/0000:00:02.0", map[string]string{
		"vendor": "0x10de", "device": "0x20b5", "class": "0x030200", "vpd": "",
	})
	privileged := fstest.MapFS{"vpd/0000:00:02.0": {Data: vpdImage("A100 80GB PCIe", []string{"PN", "900-21001-0020-100"}, nil)}}
	d := pciDevice(t, probePCIWith(t, m, testDB(t), privileged), "0000:00:02.0")
	if d.VPD == nil || d.VPD.PartNumber != "900-21001-0020-100" {
		t.Errorf("vpd = %+v, vpd_error %q", d.VPD, d.VPDError)
	}
}

// Display controllers are listed with class 03 whatever their driver: one
// bound to vfio-pci for passthrough, one with no driver at all (a virtual
// machine's display adapter), and the BMC's VGA with its DRM card.
func TestPCIProberListsDisplayControllersWhateverTheirDriver(t *testing.T) {
	m := pciHost()
	addPCIDevice(m, "pci0000:c0/0000:c0:01.1/0000:c1:00.0", map[string]string{
		"vendor": "0x10de", "device": "0x2bb5", "class": "0x030200", "revision": "0xa1",
		"driver": "->sys/bus/pci/drivers/vfio-pci",
	})
	addPCIDevice(m, "pci0000:00/0000:00:02.0", map[string]string{"vendor": "0x1234", "device": "0x1111", "class": "0x030000", "revision": "0x02"})
	addPCIDevice(m, "pci0000:00/0000:00:1c.0/0000:02:00.0", map[string]string{
		"vendor": "0x1a03", "device": "0x2000", "class": "0x030000", "revision": "0x52",
		"driver": "->sys/bus/pci/drivers/ast", "drm/card0/dev": "226:0",
	})
	m["sys/bus/pci/drivers/vfio-pci/bind"] = &fstest.MapFile{}
	m["sys/bus/pci/drivers/ast/module"] = sysLink("sys/bus/pci/drivers/ast/module", "sys/module/ast")
	p := probePCI(t, m, testDB(t))

	for address, driver := range map[string]string{"0000:c1:00.0": "vfio-pci", "0000:00:02.0": "", "0000:02:00.0": "ast"} {
		d := pciDevice(t, p, address)
		if d.Class.ID != "03" || d.Driver != driver {
			t.Errorf("%s: class %q, driver %q; want class 03, driver %q", address, d.Class.ID, d.Driver, driver)
		}
	}
	if d := pciDevice(t, p, "0000:c1:00.0"); len(d.KernelNames) != 0 || d.Product.Name != "GB202GL [RTX PRO 6000 Blackwell Server Edition]" ||
		d.Class.Name != "Display controller" {
		t.Errorf("passthrough GPU: kernel names %v, product %q, class %q", d.KernelNames, d.Product.Name, d.Class.Name)
	}
	if d := pciDevice(t, p, "0000:02:00.0"); !slices.Equal(d.KernelNames["drm"], []string{"card0"}) || d.DriverModule != "ast" ||
		d.Vendor.Name != "ASPEED Technology, Inc." || d.Product.Name != "ASPEED Graphics Family" {
		t.Errorf("BMC VGA = %+v", d)
	}
}

// slotTable gives pciHost's ConnectX card a firmware slot recorded at its
// root port, 0000:3a:00.0, and the NVMe drive one recorded at its own
// address, plus an empty slot.
func slotTable() fstest.MapFS {
	return fstest.MapFS{SMBIOSTableFile: {Data: table(
		slotStructure("CPU1 SLOT3 PCI-E 4.0 X16", 0xBD, 0x0D, 4, 0, 0x3a, 0x00),
		slotStructure("NVMe Bay 5", 0x24, 0x0A, 4, 0, 0x5e, 0x00),
		slotStructure("CPU2 SLOT7 PCI-E 4.0 X8", 0xBC, 0x0B, 3, 0xFFFF, 0xFF, 0xFF),
	)}}
}

func TestPCIProberLabelsDevicesWithTheirFirmwareSlot(t *testing.T) {
	p := probePCIWith(t, pciHost(), testDB(t), slotTable())
	for addr, want := range map[string]PCISlot{
		"0000:3a:00.0": {Designation: "CPU1 SLOT3 PCI-E 4.0 X16"},            // the port the slot is recorded at
		"0000:3b:00.0": {Name: "3", Designation: "CPU1 SLOT3 PCI-E 4.0 X16"}, // the card below it
		"0000:3b:00.2": {Name: "3", Designation: "CPU1 SLOT3 PCI-E 4.0 X16"}, // its VF
		"0000:5e:00.0": {Name: "5", Designation: "NVMe Bay 5"},
	} {
		if d := pciDevice(t, p, addr); d.Slot == nil || *d.Slot != want {
			t.Errorf("%s: slot %+v, want %+v", addr, d.Slot, want)
		}
	}
	if d := pciDevice(t, p, "0000:00:1f.6"); d.Slot != nil {
		t.Errorf("onboard NIC: slot %+v, want none", d.Slot)
	}
}

// Without the table, devices keep their kernel slot only.
func TestPCIProberWithoutTheSMBIOSTable(t *testing.T) {
	d := pciDevice(t, probePCIWith(t, pciHost(), testDB(t), nil), "0000:3b:00.0")
	if d.Slot == nil || *d.Slot != (PCISlot{Name: "3"}) {
		t.Errorf("slot %+v, want the kernel's alone", d.Slot)
	}
}

// Review focus 5: a slot recorded at one root port names that port and the
// devices below it, not the port's sibling functions; a card's own
// functions all match a slot recorded at any one of them.
func TestSlotDesignationMatchesBridgesExactly(t *testing.T) {
	slots := []SystemSlot{
		{Designation: "Slot A", Address: "0000:00:01.0"},
		{Designation: "Slot B", Address: "0000:5e:00.0"},
		{Designation: "Empty"},
		{Designation: "Slot C", Address: "0000:6e:00.1"},
		{Designation: "Slot D", Address: "0000:6e:00.0"},
		{Designation: "Slot E", Address: "0000:70:01.0"},
		{Designation: "Slot F", Address: "0000:71:02.0"},
	}
	for _, tc := range []struct {
		address   string
		bridge    bool
		ancestors []string
		want      string
	}{
		{"0000:00:01.0", true, nil, "Slot A"},
		{"0000:00:01.1", true, nil, ""}, // a sibling root port
		{"0000:02:00.0", false, []string{"0000:00:01.0"}, "Slot A"},
		{"0000:03:00.0", false, []string{"0000:00:01.1"}, ""},
		{"0000:5e:00.1", false, []string{"0000:5d:02.0"}, "Slot B"}, // another function of the card
		{"0000:5f:00.0", false, []string{"0000:5e:00.0"}, "Slot B"},
		{"0000:6e:00.0", false, nil, "Slot D"},                                      // an exact match beats an earlier sibling function's
		{"0000:72:00.0", false, []string{"0000:71:02.0", "0000:70:01.0"}, "Slot F"}, // the nearer ancestor wins
	} {
		if got := slotDesignation(slots, tc.address, tc.bridge, tc.ancestors); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.address, got, tc.want)
		}
	}
}

func TestPCIAncestorsAreNearestFirst(t *testing.T) {
	m := pciHost()
	addPCIDevice(m, "pci0000:3a/0000:3a:00.0/0000:3b:00.0/0000:3c:00.0", map[string]string{"vendor": "0x8086"})
	if got, want := pciAncestors(m, "0000:3c:00.0"), []string{"0000:3b:00.0", "0000:3a:00.0"}; !slices.Equal(got, want) {
		t.Errorf("pciAncestors = %v, want %v", got, want)
	}
	if got := pciAncestors(m, "0000:3a:00.0"); len(got) != 0 {
		t.Errorf("a device under a host bridge has ancestors %v", got)
	}
}

// An AMD host's dummy host bridge, a bridge-class device that is no port,
// sits beside the root ports at other functions of its device number; it
// must not take the slot of the port beside it.
func TestPCIProberDummyHostBridgeTakesNoSlot(t *testing.T) {
	m := pciHost()
	addPCIDevice(m, "pci0000:40/0000:40:01.0", map[string]string{"vendor": "0x1022", "class": "0x060000"})
	addPCIDevice(m, "pci0000:40/0000:40:01.1", map[string]string{"vendor": "0x1022", "class": "0x060400"})
	addPCIDevice(m, "pci0000:40/0000:40:01.1/0000:41:00.0", map[string]string{"vendor": "0x15b3", "class": "0x020000"})
	fsys := fstest.MapFS{SMBIOSTableFile: {Data: table(slotStructure("CPU1 SLOT1", 0xBD, 0x0D, 4, 0, 0x40, 0x09))}}
	p := probePCIWith(t, m, testDB(t), fsys)
	if d := pciDevice(t, p, "0000:40:01.0"); d.Slot != nil {
		t.Errorf("dummy host bridge: slot %+v, want none", d.Slot)
	}
	for _, addr := range []string{"0000:40:01.1", "0000:41:00.0"} {
		if d := pciDevice(t, p, addr); d.Slot == nil || d.Slot.Designation != "CPU1 SLOT1" {
			t.Errorf("%s: slot %+v, want CPU1 SLOT1", addr, d.Slot)
		}
	}
}

// connectXDir is the ConnectX physical function's directory in pciHost.
const (
	connectXDir   = "sys/devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0"
	connectXVFDir = "sys/devices/pci0000:3a/0000:3a:00.0/0000:3b:00.2"
)

// vpdHelper is a helper file system answering each VPD request with answer,
// and every other request, the SMBIOS table's, as a file the host lacks. It
// counts the VPD requests.
func vpdHelper(answer func(helper.Request) (helper.Response, error)) (fs.FS, *int) {
	asked := new(int)
	return helper.NewFS(func(req helper.Request) (helper.Response, error) {
		if req.Op != helper.OpVPD {
			return helper.Response{Error: helper.ErrCodeNotFound}, nil
		}
		*asked++
		return answer(req)
	}), asked
}

const vpdPoweredDown = "not read: the device is powered down, and reading its VPD would wake it"

// Reading VPD resumes a runtime-suspended device and its port, and powers
// up one in D3cold: a timed probe must not wake idle cards.
func TestPCIProberDoesNotWakeAPoweredDownDevice(t *testing.T) {
	for name, file := range map[string][2]string{
		"runtime suspended": {"power/runtime_status", "suspended"},
		"D3cold":            {"power_state", "D3cold"},
	} {
		m := withVPD()
		m[connectXDir+"/"+file[0]] = &fstest.MapFile{Data: []byte(file[1] + "\n")}
		privileged, _ := vpdHelper(func(req helper.Request) (helper.Response, error) {
			t.Errorf("%s: the helper was asked for %+v", name, req)
			return helper.Response{Data: mlxVPD()}, nil
		})
		d := pciDevice(t, probePCIWith(t, m, testDB(t), privileged), "0000:3b:00.0")
		if d.VPD != nil || d.VPDError != vpdPoweredDown {
			t.Errorf("%s: vpd %+v, vpd_error %q; want none and %q", name, d.VPD, d.VPDError, vpdPoweredDown)
		}
	}
}

// vpdProbes returns a function that runs the next probe of one prober of
// m, so its VPD cache carries over from probe to probe.
func vpdProbes(t *testing.T, m fs.FS, privileged fs.FS) func() *PCI {
	prober := NewPCIProber(m, testDB(t), privileged)
	return func() *PCI {
		t.Helper()
		v, err := prober.Probe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return v.(*PCI)
	}
}

// setConnectX writes one of the ConnectX card's sysfs files.
func setConnectX(m fstest.MapFS, name, value string) {
	m[connectXDir+"/"+name] = &fstest.MapFile{Data: []byte(value + "\n")}
}

// mlxHelper answers every VPD request with the ConnectX card's VPD.
func mlxHelper() (fs.FS, *int) {
	return vpdHelper(func(helper.Request) (helper.Response, error) {
		return helper.Response{Data: mlxVPD()}, nil
	})
}

// A powered-down device reports the VPD the prober last read from it while
// it was awake, unless a different card now sits at its address or its last
// read failed.
func TestPCIProberReportsTheVPDLastReadWhileAwake(t *testing.T) {
	m := withVPD()
	answer := helper.Response{Data: mlxVPD()}
	asleep := false
	privileged, asked := vpdHelper(func(req helper.Request) (helper.Response, error) {
		if asleep {
			t.Errorf("the helper was asked for %+v while the device was powered down", req)
		}
		return answer, nil
	})
	probe := vpdProbes(t, m, privileged)
	setStatus := func(status string) {
		asleep = status == "suspended"
		setConnectX(m, "power/runtime_status", status)
	}

	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD == nil || d.VPDError != "" {
		t.Fatalf("awake: vpd %+v, vpd_error %q", d.VPD, d.VPDError)
	}
	setStatus("suspended")
	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPDError != "" || d.VPD == nil || d.VPD.SerialNumber != "MT2140X12345" {
		t.Errorf("suspended after a read: vpd %+v, vpd_error %q; want the VPD read while awake", d.VPD, d.VPDError)
	}
	if *asked != 1 {
		t.Errorf("the helper was asked %d times, want once", *asked)
	}

	// A different card at the address does not inherit the VPD.
	setConnectX(m, "device", "0x101f")
	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD != nil || d.VPDError != vpdPoweredDown {
		t.Errorf("another card: vpd %+v, vpd_error %q; want none and %q", d.VPD, d.VPDError, vpdPoweredDown)
	}
	setConnectX(m, "device", "0x101d")

	// A failed read of the awake device forgets its VPD.
	setStatus("active")
	answer = helper.Response{Error: helper.ErrCodeRead, Message: "input/output error"}
	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD != nil || !strings.Contains(d.VPDError, "read_failed") {
		t.Errorf("failed read: vpd %+v, vpd_error %q", d.VPD, d.VPDError)
	}
	setStatus("suspended")
	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD != nil || d.VPDError != vpdPoweredDown {
		t.Errorf("suspended after a failed read: vpd %+v, vpd_error %q; want none and %q", d.VPD, d.VPDError, vpdPoweredDown)
	}
}

// PCI IDs name a card's model, not the card: a replacement of the same
// model at the same address must not inherit the removed card's VPD. A
// probe that finds the address empty forgets what was read there, so the
// replacement, found powered down, is reported unread rather than with the
// removed card's serial number.
func TestPCIProberForgetsTheVPDOfARemovedCard(t *testing.T) {
	m := withVPD()
	privileged, asked := mlxHelper()
	probe := vpdProbes(t, m, privileged)

	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD == nil || d.VPD.SerialNumber != "MT2140X12345" {
		t.Fatalf("awake: vpd %+v, vpd_error %q", d.VPD, d.VPDError)
	}

	// Unplug the card: the listing entries and device directories of its
	// physical function and of its VF, which the kernel removes with it,
	// go.
	card := fstest.MapFS{}
	for name, f := range m {
		for _, dir := range []string{connectXDir, connectXVFDir} {
			if name == pciDeviceDir(path.Base(dir)) || strings.HasPrefix(name, dir+"/") {
				card[name] = f
				delete(m, name)
			}
		}
	}
	for _, d := range probe().Devices {
		if strings.HasPrefix(d.Address, "0000:3b:") {
			t.Fatalf("the unplugged card is still listed: %+v", d)
		}
	}

	// A card of the same model goes in at the same address, and is
	// suspended by the next probe.
	maps.Copy(m, card)
	setConnectX(m, "power/runtime_status", "suspended")
	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD != nil || d.VPDError != vpdPoweredDown {
		t.Errorf("replacement: vpd %+v, vpd_error %q; want none and %q", d.VPD, d.VPDError, vpdPoweredDown)
	}
	if *asked != 1 {
		t.Errorf("the helper was asked %d times, want once: the replacement must not be woken", *asked)
	}
}

// A refresh that finds another model at the address has seen the card go,
// so a later card of the first model, found powered down, must not get the
// first card's VPD back.
func TestPCIProberForgetsTheVPDOfACardSeenReplacedByAnotherModel(t *testing.T) {
	m := withVPD()
	privileged, asked := mlxHelper()
	probe := vpdProbes(t, m, privileged)

	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD == nil || d.VPD.SerialNumber != "MT2140X12345" {
		t.Fatalf("awake: vpd %+v, vpd_error %q", d.VPD, d.VPDError)
	}
	setConnectX(m, "power/runtime_status", "suspended")
	setConnectX(m, "device", "0x101f")
	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD != nil || d.VPDError != vpdPoweredDown {
		t.Errorf("another model: vpd %+v, vpd_error %q; want none and %q", d.VPD, d.VPDError, vpdPoweredDown)
	}
	setConnectX(m, "device", "0x101d")
	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD != nil || d.VPDError != vpdPoweredDown {
		t.Errorf("the first model again: vpd %+v, vpd_error %q; want none and %q", d.VPD, d.VPDError, vpdPoweredDown)
	}
	if *asked != 1 {
		t.Errorf("the helper was asked %d times, want once", *asked)
	}
}

// Seeing another model at the address proves the card has gone even when
// that model has no VPD of its own, and so is never looked up in the
// cache: a later card of the first model, found powered down, must still
// not get the first card's VPD back.
func TestPCIProberForgetsTheVPDOfACardSeenReplacedByAModelWithoutVPD(t *testing.T) {
	for _, between := range []string{"active", "suspended"} {
		t.Run(between, func(t *testing.T) {
			m := withVPD()
			privileged, asked := mlxHelper()
			probe := vpdProbes(t, m, privileged)

			if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD == nil || d.VPD.SerialNumber != "MT2140X12345" {
				t.Fatalf("awake: vpd %+v, vpd_error %q", d.VPD, d.VPDError)
			}

			// Another model, with no VPD at all.
			vpd := m[connectXDir+"/vpd"]
			delete(m, connectXDir+"/vpd")
			setConnectX(m, "device", "0x101f")
			setConnectX(m, "power/runtime_status", between)
			if d := pciDevice(t, probe(), "0000:3b:00.0"); d.Product.ID != "101f" || d.VPD != nil || d.VPDError != "" {
				t.Fatalf("model without VPD: product %q, vpd %+v, vpd_error %q", d.Product.ID, d.VPD, d.VPDError)
			}

			// A card of the first model again, powered down.
			m[connectXDir+"/vpd"] = vpd
			setConnectX(m, "device", "0x101d")
			setConnectX(m, "power/runtime_status", "suspended")
			if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD != nil || d.VPDError != vpdPoweredDown {
				t.Errorf("the first model again: vpd %+v, vpd_error %q; want none and %q", d.VPD, d.VPDError, vpdPoweredDown)
			}
			if *asked != 1 {
				t.Errorf("the helper was asked %d times, want once", *asked)
			}
		})
	}
}

// A card found awake but not read, because an earlier VPD read in the
// refresh failed, keeps the VPD read from it: a failed read elsewhere says
// nothing about this card, and a helper restart, or one card whose read
// always hangs, must not cost every card after it its VPD once it powers
// down. (A same-model swap that no refresh reads is the documented limit.)
func TestPCIProberKeepsTheVPDOfACardItSkipped(t *testing.T) {
	m := withVPD()
	// The onboard NIC sorts before the ConnectX card, so its read comes
	// first in every refresh.
	m["sys/devices/pci0000:00/0000:00:1f.6/vpd"] = &fstest.MapFile{Mode: 0o600}
	hang := false
	connectXReads := 0
	privileged, _ := vpdHelper(func(req helper.Request) (helper.Response, error) {
		if req.Address == "0000:00:1f.6" {
			if hang {
				return helper.Response{Error: helper.ErrCodeTimeout, Message: "the read did not finish in time"}, nil
			}
			return helper.Response{Error: helper.ErrCodeNotFound}, nil
		}
		connectXReads++
		return helper.Response{Data: mlxVPD()}, nil
	})
	probe := vpdProbes(t, m, privileged)

	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD == nil || d.VPD.SerialNumber != "MT2140X12345" {
		t.Fatalf("awake: vpd %+v, vpd_error %q", d.VPD, d.VPDError)
	}
	// The card is awake, and not read because the onboard NIC's read hangs.
	hang = true
	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD != nil || !strings.Contains(d.VPDError, "an earlier VPD read in this refresh failed") {
		t.Errorf("skipped: vpd %+v, vpd_error %q", d.VPD, d.VPDError)
	}
	setConnectX(m, "power/runtime_status", "suspended")
	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPDError != "" || d.VPD == nil || d.VPD.SerialNumber != "MT2140X12345" {
		t.Errorf("powered down after a skipped read: vpd %+v, vpd_error %q; want the VPD read while awake", d.VPD, d.VPDError)
	}
	if connectXReads != 1 {
		t.Errorf("the ConnectX card was read %d times, want once", connectXReads)
	}
}

// IDs that cannot be read are no evidence that the card has gone: a
// refresh that cannot read them reports a powered-down card unread, since
// it cannot tell which card it is, but keeps the VPD for the refreshes
// that can. Found awake, the card is read, and keeps the cached VPD when
// it reads the same.
func TestPCIProberKeepsTheVPDOfACardWhoseIDsCannotBeRead(t *testing.T) {
	for status, wantReads := range map[string]int{"suspended": 1, "active": 2} {
		t.Run(status, func(t *testing.T) {
			m := withVPD()
			privileged, asked := mlxHelper()
			probe := vpdProbes(t, m, privileged)

			if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD == nil || d.VPD.SerialNumber != "MT2140X12345" {
				t.Fatalf("awake: vpd %+v, vpd_error %q", d.VPD, d.VPDError)
			}
			subsystem := m[connectXDir+"/subsystem_device"]
			setConnectX(m, "subsystem_device", "garbage")
			setConnectX(m, "power/runtime_status", status)
			d := pciDevice(t, probe(), "0000:3b:00.0")
			if d.Subsystem.ID != unknown {
				t.Fatalf("subsystem id %q, want %q", d.Subsystem.ID, unknown)
			}
			if status == "suspended" && (d.VPD != nil || d.VPDError != vpdPoweredDown) {
				t.Errorf("unreadable ids, powered down: vpd %+v, vpd_error %q; want none and %q", d.VPD, d.VPDError, vpdPoweredDown)
			}
			if status == "active" && (d.VPD == nil || d.VPDError != "") {
				t.Errorf("unreadable ids, awake: vpd %+v, vpd_error %q; want the VPD read now", d.VPD, d.VPDError)
			}

			m[connectXDir+"/subsystem_device"] = subsystem
			setConnectX(m, "power/runtime_status", "suspended")
			if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPDError != "" || d.VPD == nil || d.VPD.SerialNumber != "MT2140X12345" {
				t.Errorf("ids readable again, powered down: vpd %+v, vpd_error %q; want the VPD read while awake", d.VPD, d.VPDError)
			}
			if *asked != wantReads {
				t.Errorf("the helper was asked %d times, want %d", *asked, wantReads)
			}
		})
	}
}

// A card whose IDs cannot be read cannot be told from another card whose
// IDs cannot be read either, so what is read from it is never cached.
func TestPCIProberNeverCachesTheVPDOfACardWhoseIDsCannotBeRead(t *testing.T) {
	m := withVPD()
	privileged, asked := mlxHelper()
	probe := vpdProbes(t, m, privileged)
	for _, name := range []string{"vendor", "device", "subsystem_vendor", "subsystem_device"} {
		setConnectX(m, name, "garbage")
	}

	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD == nil || d.VPDError != "" {
		t.Fatalf("awake: vpd %+v, vpd_error %q", d.VPD, d.VPDError)
	}
	setConnectX(m, "power/runtime_status", "suspended")
	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD != nil || d.VPDError != vpdPoweredDown {
		t.Errorf("powered down: vpd %+v, vpd_error %q; want none and %q", d.VPD, d.VPDError, vpdPoweredDown)
	}
	if *asked != 1 {
		t.Errorf("the helper was asked %d times, want once", *asked)
	}
}

// The helper failing on the card's own read, unavailable or timed out,
// says no more about the card than a read skipped after it fails on
// another: the card keeps the VPD read from it.
func TestPCIProberKeepsTheVPDOfACardWhoseOwnReadTheHelperFailed(t *testing.T) {
	for name, fail := range map[string]func() (helper.Response, error){
		"read hung": func() (helper.Response, error) {
			return helper.Response{Error: helper.ErrCodeTimeout, Message: "the read did not finish in time"}, nil
		},
		"helper down": func() (helper.Response, error) {
			return helper.Response{}, fmt.Errorf("%w: connect: connection refused", helper.ErrUnavailable)
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := withVPD()
			failing := false
			privileged, _ := vpdHelper(func(helper.Request) (helper.Response, error) {
				if failing {
					return fail()
				}
				return helper.Response{Data: mlxVPD()}, nil
			})
			probe := vpdProbes(t, m, privileged)

			if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD == nil || d.VPD.SerialNumber != "MT2140X12345" {
				t.Fatalf("awake: vpd %+v, vpd_error %q", d.VPD, d.VPDError)
			}
			failing = true
			if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD != nil || d.VPDError == "" {
				t.Errorf("helper failed: vpd %+v, vpd_error %q", d.VPD, d.VPDError)
			}
			setConnectX(m, "power/runtime_status", "suspended")
			if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPDError != "" || d.VPD == nil || d.VPD.SerialNumber != "MT2140X12345" {
				t.Errorf("powered down after the helper failed: vpd %+v, vpd_error %q; want the VPD read while awake", d.VPD, d.VPDError)
			}
		})
	}
}

// An unreadable ID says nothing either way, but the IDs that can be read
// still do: another device ID with the subsystem ID unreadable is another
// model, and the cached VPD must not outlive it.
func TestPCIProberForgetsTheVPDOfACardSeenReplacedByAModelWithAnUnreadableID(t *testing.T) {
	for _, between := range []string{"suspended", "skipped"} {
		t.Run(between, func(t *testing.T) {
			m := withVPD()
			// The onboard NIC sorts first; once hang is set, its read times
			// out and every later read in the refresh is skipped.
			m["sys/devices/pci0000:00/0000:00:1f.6/vpd"] = &fstest.MapFile{Mode: 0o600}
			hang := false
			privileged, _ := vpdHelper(func(req helper.Request) (helper.Response, error) {
				if req.Address == "0000:00:1f.6" {
					if hang {
						return helper.Response{Error: helper.ErrCodeTimeout, Message: "the read did not finish in time"}, nil
					}
					return helper.Response{Error: helper.ErrCodeNotFound}, nil
				}
				return helper.Response{Data: mlxVPD()}, nil
			})
			probe := vpdProbes(t, m, privileged)

			if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD == nil || d.VPD.SerialNumber != "MT2140X12345" {
				t.Fatalf("awake: vpd %+v, vpd_error %q", d.VPD, d.VPDError)
			}

			// Another model, its subsystem ID unreadable, and not read.
			subsystem := m[connectXDir+"/subsystem_device"]
			setConnectX(m, "device", "0x101f")
			setConnectX(m, "subsystem_device", "garbage")
			if between == "suspended" {
				setConnectX(m, "power/runtime_status", "suspended")
			} else {
				hang = true
			}
			if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD != nil {
				t.Fatalf("another model: vpd %+v", d.VPD)
			}

			// A card of the first model again, fully identified, powered down.
			hang = false
			setConnectX(m, "device", "0x101d")
			m[connectXDir+"/subsystem_device"] = subsystem
			setConnectX(m, "power/runtime_status", "suspended")
			if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD != nil || d.VPDError != vpdPoweredDown {
				t.Errorf("the first model again: vpd %+v, vpd_error %q; want none and %q", d.VPD, d.VPDError, vpdPoweredDown)
			}
		})
	}
}

// A card whose IDs cannot be read is told from the cached card by the
// identity in its VPD, its serial and part numbers. Its read-write
// keywords, which the card can rewrite itself, do not count.
func TestPCIProberIdentifiesACardWhoseIDsCannotBeReadByItsVPD(t *testing.T) {
	ro := func(serial string) []string {
		return []string{"PN", "MCX621102AN-ADAT", "EC", "A6", "SN", serial, "V0", "PCIeGen4 x8"}
	}
	for name, tc := range map[string]struct {
		reread   []byte
		wantKept bool
	}{
		"read-write keywords rewritten": {vpdImage("ConnectX-6 Dx EN adapter card, 25GbE", ro("MT2140X12345"), []string{"V1", "asset-7"}), true},
		"another serial number":         {vpdImage("ConnectX-6 Dx EN adapter card, 25GbE", ro("MT2140X99999"), nil), false},
	} {
		t.Run(name, func(t *testing.T) {
			m := withVPD()
			answer := mlxVPD()
			privileged, _ := vpdHelper(func(helper.Request) (helper.Response, error) {
				return helper.Response{Data: answer}, nil
			})
			probe := vpdProbes(t, m, privileged)
			if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD == nil {
				t.Fatalf("awake: vpd_error %q", d.VPDError)
			}

			// Awake, IDs unreadable, VPD read again.
			subsystem := m[connectXDir+"/subsystem_device"]
			setConnectX(m, "subsystem_device", "garbage")
			answer = tc.reread
			if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD == nil || d.VPDError != "" {
				t.Fatalf("re-read: vpd %+v, vpd_error %q", d.VPD, d.VPDError)
			}

			m[connectXDir+"/subsystem_device"] = subsystem
			setConnectX(m, "power/runtime_status", "suspended")
			d := pciDevice(t, probe(), "0000:3b:00.0")
			if tc.wantKept && (d.VPD == nil || d.VPD.SerialNumber != "MT2140X12345") {
				t.Errorf("powered down: vpd %+v, vpd_error %q; want the VPD read while awake", d.VPD, d.VPDError)
			}
			if !tc.wantKept && (d.VPD != nil || d.VPDError != vpdPoweredDown) {
				t.Errorf("powered down: vpd %+v, vpd_error %q; want none and %q", d.VPD, d.VPDError, vpdPoweredDown)
			}
		})
	}
}

// A vpd file whose existence cannot be checked says nothing about the
// card: it keeps the VPD read from it.
func TestPCIProberKeepsTheVPDOfACardWhoseVPDFileCannotBeChecked(t *testing.T) {
	m := withVPD()
	fsys := deniedFS{FS: m, denied: map[string]bool{}}
	privileged, asked := mlxHelper()
	probe := vpdProbes(t, fsys, privileged)

	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD == nil || d.VPD.SerialNumber != "MT2140X12345" {
		t.Fatalf("awake: vpd %+v, vpd_error %q", d.VPD, d.VPDError)
	}
	fsys.denied[pciDeviceDir("0000:3b:00.0")+"/vpd"] = true
	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD != nil {
		t.Fatalf("vpd file unreadable: vpd %+v", d.VPD)
	}
	delete(fsys.denied, pciDeviceDir("0000:3b:00.0")+"/vpd")
	setConnectX(m, "power/runtime_status", "suspended")
	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPDError != "" || d.VPD == nil || d.VPD.SerialNumber != "MT2140X12345" {
		t.Errorf("powered down: vpd %+v, vpd_error %q; want the VPD read while awake", d.VPD, d.VPDError)
	}
	if *asked != 1 {
		t.Errorf("the helper was asked %d times, want once", *asked)
	}
}

// A card removed while its VPD is being read is left out of the probe,
// and so is what was read from it: a same-model card found powered down
// at its address later must not get it.
func TestPCIProberForgetsTheVPDOfACardRemovedDuringItsRead(t *testing.T) {
	m := withVPD()
	listing := pciDeviceDir("0000:3b:00.0")
	entry := m[listing]
	privileged, asked := vpdHelper(func(helper.Request) (helper.Response, error) {
		delete(m, listing) // unplugged mid-read
		return helper.Response{Data: mlxVPD()}, nil
	})
	probe := vpdProbes(t, m, privileged)

	for _, d := range probe().Devices {
		if d.Address == "0000:3b:00.0" {
			t.Fatalf("the card removed during its read is listed: %+v", d)
		}
	}
	m[listing] = entry
	setConnectX(m, "power/runtime_status", "suspended")
	if d := pciDevice(t, probe(), "0000:3b:00.0"); d.VPD != nil || d.VPDError != vpdPoweredDown {
		t.Errorf("replacement: vpd %+v, vpd_error %q; want none and %q", d.VPD, d.VPDError, vpdPoweredDown)
	}
	if *asked != 1 {
		t.Errorf("the helper was asked %d times, want once", *asked)
	}
}

// One card whose VPD read hangs, or a helper that does not answer, costs
// one read per refresh, not one per device.
func TestPCIProberStopsAskingTheHelperAfterItFails(t *testing.T) {
	m := withVPD()
	m["sys/devices/pci0000:5d/0000:5d:02.0/0000:5e:00.0/vpd"] = &fstest.MapFile{Mode: 0o600}
	for name, tc := range map[string]struct {
		answer func(helper.Request) (helper.Response, error)
		trips  bool
		want   string
	}{
		"read hung": {func(helper.Request) (helper.Response, error) {
			return helper.Response{Error: helper.ErrCodeTimeout, Message: "the read did not finish in time"}, nil
		}, true, "the helper answered timeout"},
		"helper down": {func(helper.Request) (helper.Response, error) {
			return helper.Response{}, fmt.Errorf("%w: connect: connection refused", helper.ErrUnavailable)
		}, true, "the privileged helper is unavailable"},
		"read failed": {func(helper.Request) (helper.Response, error) {
			return helper.Response{Error: helper.ErrCodeRead, Message: "input/output error"}, nil
		}, false, "read_failed"},
		"device gone": {func(helper.Request) (helper.Response, error) {
			return helper.Response{Error: helper.ErrCodeNotFound}, nil
		}, false, "file does not exist"},
	} {
		privileged, asked := vpdHelper(tc.answer)
		prober := NewPCIProber(m, testDB(t), privileged)
		for refresh := 1; refresh <= 2; refresh++ {
			v, err := prober.Probe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			p := v.(*PCI)
			if d := pciDevice(t, p, "0000:3b:00.0"); !strings.Contains(d.VPDError, tc.want) {
				t.Errorf("%s: first device's vpd_error %q, want it to contain %q", name, d.VPDError, tc.want)
			}
			second := pciDevice(t, p, "0000:5e:00.0").VPDError
			wantAsked := refresh * 2
			if tc.trips {
				wantAsked = refresh
				prefix := "not read: an earlier VPD read in this refresh failed ("
				if !strings.HasPrefix(second, prefix) || !strings.Contains(second, tc.want) {
					t.Errorf("%s: second device's vpd_error %q, want %q and the first error", name, second, prefix)
				}
			} else if !strings.Contains(second, tc.want) {
				t.Errorf("%s: second device's vpd_error %q, want it read and to contain %q", name, second, tc.want)
			}
			if *asked != wantAsked {
				t.Errorf("%s: after refresh %d the helper was asked %d times, want %d", name, refresh, *asked, wantAsked)
			}
		}
	}
}

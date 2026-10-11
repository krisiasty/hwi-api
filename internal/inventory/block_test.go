package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// blockHost is a host root with /sys/block and the device tree behind it,
// its symlinks relative as the kernel writes them.
type blockHost fstest.MapFS

// device adds the device directory sys/devices/<devpath> and its listing
// sys/block/<name>. files maps names under the device's directory to
// contents; a value starting with "->" is instead a symlink to the rest, a
// path relative to the root, and a name ending in "/" an empty directory.
func (h blockHost) device(name, devpath string, files map[string]string) {
	h.link("sys/block/"+name, "sys/devices/"+devpath)
	h.files("sys/devices/"+devpath, files)
}

// files adds files under dir, as device does.
func (h blockHost) files(dir string, files map[string]string) {
	for name, v := range files {
		p := dir + "/" + name
		switch {
		case strings.HasSuffix(name, "/"):
			h[strings.TrimSuffix(p, "/")] = &fstest.MapFile{Mode: fs.ModeDir | 0o755}
		case strings.HasPrefix(v, "->"):
			h.link(p, strings.TrimPrefix(v, "->"))
		default:
			h[p] = &fstest.MapFile{Data: []byte(v + "\n")}
		}
	}
}

func (h blockHost) link(name, target string) { h[name] = sysLink(name, target) }

const (
	sataSSDPath = "pci0000:00/0000:00:17.0/ata1/host0/target0:0:0/0:0:0:0/block/sda"
	nvmeCtrl    = "pci0000:00/0000:00:1d.0/0000:3b:00.0/nvme/nvme0"
	fabricCtrl  = "virtual/nvme-fabrics/ctl/nvme2"
)

// testBlockHost is a host with one of each kind of block device the
// collector tells apart.
func testBlockHost() blockHost {
	h := blockHost{}
	// A SATA SSD with two partitions, the second an LVM physical volume,
	// and the directories of a disk that are not partitions.
	h.device("sda", sataSSDPath, map[string]string{
		"dev": "8:0", "size": "937703088", "removable": "0", "ro": "0",
		"queue/logical_block_size": "512", "queue/physical_block_size": "4096", "queue/rotational": "0",
		"holders/": "", "power/control": "auto", "device": "->sys/devices/pci0000:00/0000:00:17.0/ata1/host0/target0:0:0/0:0:0:0",
		"sda1/partition": "1", "sda1/start": "2048", "sda1/size": "2097152", "sda1/ro": "0", "sda1/dev": "8:1",
		"sda2/partition": "2", "sda2/start": "2099200", "sda2/size": "935601888", "sda2/ro": "0", "sda2/dev": "8:2",
		"sda2/holders/dm-0": "->sys/devices/virtual/block/dm-0",
	})
	h.files("sys/devices/pci0000:00/0000:00:17.0/ata1/host0/target0:0:0/0:0:0:0", map[string]string{
		"vendor": "ATA     ", "model": "SAMSUNG MZ7LH480        ", "wwid": "naa.5002538e00000000", "type": "0",
	})

	// A SATA HDD, its partitions numbered so that their names sort the
	// other way.
	h.device("sdc", "pci0000:00/0000:00:17.0/ata2/host1/target1:0:0/1:0:0:0/block/sdc", map[string]string{
		"size": "7814037168", "queue/logical_block_size": "512", "queue/physical_block_size": "4096", "queue/rotational": "1",
		"removable": "0", "ro": "0",
		"sdc10/partition": "10", "sdc10/start": "4096", "sdc10/size": "2048", "sdc10/ro": "1",
		"sdc2/partition": "2", "sdc2/start": "2048", "sdc2/size": "2048", "sdc2/ro": "0",
	})

	// A SAS disk behind an HBA and a RAID controller's logical drive.
	h.device("sdd", "pci0000:17/0000:17:00.0/0000:18:00.0/host2/port-2:0/end_device-2:0/target2:0:0/2:0:0:0/block/sdd",
		map[string]string{"size": "16", "queue/rotational": "1"})
	h.device("sde", "pci0000:3a/0000:3a:00.0/0000:5e:00.0/host3/target3:2:0/3:2:0:0/block/sde",
		map[string]string{"size": "16", "queue/rotational": "1"})

	// A PCIe NVMe namespace behind a root port: the controller is the
	// last PCI address on the path, not the port.
	h.device("nvme0n1", nvmeCtrl+"/nvme0n1", map[string]string{
		"size": "1953525168", "queue/logical_block_size": "512", "queue/physical_block_size": "512", "queue/rotational": "0",
		"removable": "0", "ro": "0", "wwid": "eui.0025385b71b0a1c2  ", "device": "->sys/devices/" + nvmeCtrl,
		"holders/md126":       "->sys/devices/virtual/block/md126",
		"nvme0n1p1/partition": "1", "nvme0n1p1/start": "2048", "nvme0n1p1/size": "1048576", "nvme0n1p1/ro": "0",
		"nvme0n1p2/partition": "2", "nvme0n1p2/start": "1050624", "nvme0n1p2/size": "1952474511", "nvme0n1p2/ro": "0",
	})
	h.files("sys/devices/"+nvmeCtrl, map[string]string{
		"model": "Samsung SSD 980 PRO 1TB                 ", "serial": "EXSSD000000002      ", "transport": "pcie",
	})
	// A per-path namespace of native NVMe multipath, hidden by the kernel.
	h.device("nvme0c0n1", nvmeCtrl+"/nvme0c0n1", map[string]string{"size": "16", "hidden": "1"})
	// A native multipath head, which the kernel puts under the virtual
	// tree and links to its subsystem.
	h.device("nvme1n1", "virtual/nvme-subsystem/nvme-subsys1/nvme1n1", map[string]string{
		"size": "16", "queue/rotational": "0", "hidden": "0", "device": "->sys/devices/virtual/nvme-subsystem/nvme-subsys1",
		"holders/md126": "->sys/devices/virtual/block/md126",
	})
	h.files("sys/devices/virtual/nvme-subsystem/nvme-subsys1", map[string]string{"model": "Dell Ent NVMe", "serial": "EXNVM004"})

	// An NVMe over TCP namespace.
	h.device("nvme2n1", fabricCtrl+"/nvme2n1", map[string]string{
		"size": "16", "queue/rotational": "0", "device": "->sys/devices/" + fabricCtrl,
	})
	h.files("sys/devices/"+fabricCtrl, map[string]string{"model": "Linux", "transport": "tcp"})

	// A virtio disk, rotational as QEMU reports it.
	h.device("vda", "pci0000:00/0000:00:04.0/virtio1/block/vda", map[string]string{"size": "16", "queue/rotational": "1"})

	// A removable USB disk.
	h.device("sdb", "pci0000:00/0000:00:14.0/usb1/1-1/1-1:1.0/host6/target6:0:0/6:0:0:0/block/sdb", map[string]string{
		"size": "16", "queue/rotational": "0", "removable": "1",
		"device": "->sys/devices/pci0000:00/0000:00:14.0/usb1/1-1/1-1:1.0/host6/target6:0:0/6:0:0:0",
	})
	h.files("sys/devices/pci0000:00/0000:00:14.0/usb1/1-1/1-1:1.0/host6/target6:0:0/6:0:0:0",
		map[string]string{"vendor": "SanDisk ", "model": "Cruzer          "})

	// An optical drive.
	h.device("sr0", "pci0000:00/0000:00:17.0/ata3/host4/target4:0:0/4:0:0:0/block/sr0", map[string]string{
		"size": "2097151", "queue/rotational": "1", "removable": "1", "ro": "0", "device/type": "5",
	})

	// An unused loop device and one backing a file.
	h.device("loop0", "virtual/block/loop0", map[string]string{"size": "0"})
	h.device("loop1", "virtual/block/loop1", map[string]string{"size": "8", "ro": "1", "queue/rotational": "0"})

	// An LVM volume on sda2.
	h.device("dm-0", "virtual/block/dm-0", map[string]string{
		"size": "16", "dm/name": "vg0-root", "dm/uuid": "LVM-Wb3fQx0uYJ1oVhxMEnYlA1EzP3gMsTyG\n",
		"slaves/sda2": "->sys/devices/" + sataSSDPath + "/sda2",
	})

	// A RAID 1 array of two NVMe namespaces, with one partition.
	h.device("md126", "virtual/block/md126", map[string]string{
		"size": "16", "md/level": "raid1",
		"slaves/nvme1n1":    "->sys/devices/virtual/nvme-subsystem/nvme-subsys1/nvme1n1",
		"slaves/nvme0n1":    "->sys/devices/" + nvmeCtrl + "/nvme0n1",
		"md126p1/partition": "1", "md126p1/start": "8", "md126p1/size": "8", "md126p1/ro": "0",
	})
	return h
}

func probeBlock(t *testing.T, fsys fs.FS) *Block {
	t.Helper()
	got, err := NewBlockProber(fsys).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	b, ok := got.(*Block)
	if !ok {
		t.Fatalf("Probe returned %T, want *Block", got)
	}
	return b
}

func blockDevice(t *testing.T, b *Block, name string) BlockDevice {
	t.Helper()
	for _, d := range b.Devices {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no device %s in %v", name, blockNames(b))
	return BlockDevice{}
}

func blockNames(b *Block) []string {
	var names []string
	for _, d := range b.Devices {
		names = append(names, d.Name)
	}
	return names
}

func checkBlockDevice(t *testing.T, got, want BlockDevice) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("device %s:\n got %+v\nwant %+v", want.Name, got, want)
	}
}

func TestBlockProber(t *testing.T) {
	b := probeBlock(t, fstest.MapFS(testBlockHost()))

	t.Run("listing", func(t *testing.T) {
		// Sorted as /sys/block lists them, without the unused loop device
		// and the hidden multipath path.
		want := []string{"dm-0", "loop1", "md126", "nvme0n1", "nvme1n1", "nvme2n1", "sda", "sdb", "sdc", "sdd", "sde", "sr0", "vda"}
		if got := blockNames(b); !slices.Equal(got, want) {
			t.Errorf("devices = %v, want %v", got, want)
		}
	})

	t.Run("SATA SSD", func(t *testing.T) {
		checkBlockDevice(t, blockDevice(t, b, "sda"), BlockDevice{
			Name: "sda", Type: "disk", SizeBytes: 480103981056, LogicalBlockSizeBytes: 512, PhysicalBlockSizeBytes: 4096,
			DriveType: "ssd", Transport: "sata", PCIAddress: "0000:00:17.0",
			Vendor: "ATA", Model: "SAMSUNG MZ7LH480", WWN: "naa.5002538e00000000",
			Partitions: []Partition{
				{Name: "sda1", Number: 1, StartBytes: 2048 * 512, SizeBytes: 2097152 * 512},
				{Name: "sda2", Number: 2, StartBytes: 2099200 * 512, SizeBytes: 935601888 * 512, Holders: []string{"dm-0"}},
			},
		})
	})

	t.Run("SATA HDD", func(t *testing.T) {
		checkBlockDevice(t, blockDevice(t, b, "sdc"), BlockDevice{
			Name: "sdc", Type: "disk", SizeBytes: 7814037168 * 512, LogicalBlockSizeBytes: 512, PhysicalBlockSizeBytes: 4096,
			DriveType: "hdd", Transport: "sata", PCIAddress: "0000:00:17.0",
			Partitions: []Partition{
				{Name: "sdc2", Number: 2, StartBytes: 2048 * 512, SizeBytes: 2048 * 512},
				{Name: "sdc10", Number: 10, StartBytes: 4096 * 512, SizeBytes: 2048 * 512, ReadOnly: true},
			},
		})
	})

	t.Run("SAS disk and RAID logical drive", func(t *testing.T) {
		checkBlockDevice(t, blockDevice(t, b, "sdd"), BlockDevice{
			Name: "sdd", Type: "disk", SizeBytes: 16 * 512, DriveType: "hdd", Transport: "sas", PCIAddress: "0000:18:00.0",
			Partitions: []Partition{},
		})
		checkBlockDevice(t, blockDevice(t, b, "sde"), BlockDevice{
			Name: "sde", Type: "disk", SizeBytes: 16 * 512, DriveType: "hdd", PCIAddress: "0000:5e:00.0",
			Partitions: []Partition{},
		})
	})

	t.Run("NVMe namespace", func(t *testing.T) {
		checkBlockDevice(t, blockDevice(t, b, "nvme0n1"), BlockDevice{
			Name: "nvme0n1", Type: "disk", SizeBytes: 1953525168 * 512, LogicalBlockSizeBytes: 512, PhysicalBlockSizeBytes: 512,
			DriveType: "ssd", Transport: "nvme", PCIAddress: "0000:3b:00.0",
			Model: "Samsung SSD 980 PRO 1TB", SerialNumber: "EXSSD000000002", WWN: "eui.0025385b71b0a1c2",
			Holders: []string{"md126"},
			Partitions: []Partition{
				{Name: "nvme0n1p1", Number: 1, StartBytes: 2048 * 512, SizeBytes: 1048576 * 512},
				{Name: "nvme0n1p2", Number: 2, StartBytes: 1050624 * 512, SizeBytes: 1952474511 * 512},
			},
		})
	})

	t.Run("NVMe multipath head", func(t *testing.T) {
		// Under the virtual tree, yet an SSD on NVMe, with no PCI address:
		// its subsystem links no controller.
		checkBlockDevice(t, blockDevice(t, b, "nvme1n1"), BlockDevice{
			Name: "nvme1n1", Type: "disk", SizeBytes: 16 * 512, DriveType: "ssd", Transport: "nvme",
			Model: "Dell Ent NVMe", SerialNumber: "EXNVM004", Holders: []string{"md126"}, Partitions: []Partition{},
		})
	})

	t.Run("NVMe over TCP", func(t *testing.T) {
		checkBlockDevice(t, blockDevice(t, b, "nvme2n1"), BlockDevice{
			Name: "nvme2n1", Type: "disk", SizeBytes: 16 * 512, DriveType: "ssd", Transport: "tcp",
			Model: "Linux", Partitions: []Partition{},
		})
	})

	t.Run("virtio disk", func(t *testing.T) {
		checkBlockDevice(t, blockDevice(t, b, "vda"), BlockDevice{
			Name: "vda", Type: "disk", SizeBytes: 16 * 512, DriveType: "virtual", Transport: "virtio", PCIAddress: "0000:00:04.0",
			Partitions: []Partition{},
		})
	})

	t.Run("USB disk", func(t *testing.T) {
		checkBlockDevice(t, blockDevice(t, b, "sdb"), BlockDevice{
			Name: "sdb", Type: "disk", SizeBytes: 16 * 512, DriveType: "ssd", Removable: true, Transport: "usb",
			PCIAddress: "0000:00:14.0", Vendor: "SanDisk", Model: "Cruzer", Partitions: []Partition{},
		})
	})

	t.Run("optical drive", func(t *testing.T) {
		checkBlockDevice(t, blockDevice(t, b, "sr0"), BlockDevice{
			Name: "sr0", Type: "rom", SizeBytes: 2097151 * 512, DriveType: "optical", Removable: true, Transport: "sata",
			PCIAddress: "0000:00:17.0", Partitions: []Partition{},
		})
	})

	t.Run("loop device", func(t *testing.T) {
		checkBlockDevice(t, blockDevice(t, b, "loop1"), BlockDevice{
			Name: "loop1", Type: "loop", SizeBytes: 8 * 512, ReadOnly: true, Partitions: []Partition{},
		})
	})

	t.Run("LVM volume", func(t *testing.T) {
		checkBlockDevice(t, blockDevice(t, b, "dm-0"), BlockDevice{
			Name: "dm-0", Type: "dm", SizeBytes: 16 * 512, DMName: "vg0-root", DMUUID: "LVM-Wb3fQx0uYJ1oVhxMEnYlA1EzP3gMsTyG",
			Slaves: []string{"sda2"}, Partitions: []Partition{},
		})
	})

	t.Run("RAID array", func(t *testing.T) {
		checkBlockDevice(t, blockDevice(t, b, "md126"), BlockDevice{
			Name: "md126", Type: "md", SizeBytes: 16 * 512, MDLevel: "raid1", Slaves: []string{"nvme0n1", "nvme1n1"},
			Partitions: []Partition{{Name: "md126p1", Number: 1, StartBytes: 8 * 512, SizeBytes: 8 * 512}},
		})
	})
}

// multipathHead is a host with one native NVMe multipath head, nvme0n1, in
// the subsystem nvme-subsys0, whose directory links each of ctrls, by
// controller name, to its directory under sys/devices; transports gives
// each controller's transport file.
func multipathHead(ctrls, transports map[string]string) blockHost {
	h := blockHost{}
	sub := "virtual/nvme-subsystem/nvme-subsys0"
	h.device("nvme0n1", sub+"/nvme0n1", map[string]string{
		"size": "16", "queue/rotational": "0", "hidden": "0", "device": "->sys/devices/" + sub,
	})
	h.files("sys/devices/"+sub, map[string]string{"model": "Dell Ent NVMe", "serial": "EXNVM004"})
	for name, ctrl := range ctrls {
		h.link("sys/devices/"+sub+"/"+name, "sys/devices/"+ctrl)
		h.files("sys/devices/"+ctrl, map[string]string{"transport": transports[name]})
	}
	return h
}

func TestBlockProberNVMeMultipath(t *testing.T) {
	const otherCtrl = "pci0000:00/0000:00:1d.1/0000:3c:00.0/nvme/nvme1"
	cases := []struct {
		name              string
		ctrls, transports map[string]string
		transport, pci    string
	}{
		{"PCIe", map[string]string{"nvme0": nvmeCtrl}, map[string]string{"nvme0": "pcie"}, "nvme", "0000:3b:00.0"},
		{"fabrics", map[string]string{"nvme2": fabricCtrl}, map[string]string{"nvme2": "tcp"}, "tcp", ""},
		{"dual-port", map[string]string{"nvme0": nvmeCtrl, "nvme1": otherCtrl},
			map[string]string{"nvme0": "pcie", "nvme1": "pcie"}, "nvme", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := probeBlock(t, fstest.MapFS(multipathHead(c.ctrls, c.transports)))
			checkBlockDevice(t, blockDevice(t, b, "nvme0n1"), BlockDevice{
				Name: "nvme0n1", Type: "disk", SizeBytes: 16 * 512, DriveType: "ssd", Transport: c.transport, PCIAddress: c.pci,
				Model: "Dell Ent NVMe", SerialNumber: "EXNVM004", Partitions: []Partition{},
			})
		})
	}
}

func TestBlockProberUnusedDevices(t *testing.T) {
	// A loop, nbd, ram or zram device of size 0 is unused or unconfigured;
	// an optical drive of size 0 is one without a disc.
	cases := []struct {
		name, devpath, size string
		listed              bool
	}{
		{"loop0", "virtual/block/loop0", "0", false},
		{"nbd0", "virtual/block/nbd0", "0", false},
		{"ram0", "virtual/block/ram0", "0", false},
		{"zram0", "virtual/block/zram0", "0", false},
		{"zram0", "virtual/block/zram0", "8", true},
		{"sr0", "pci0000:00/0000:00:17.0/ata3/host4/target4:0:0/4:0:0:0/block/sr0", "0", true},
	}
	for _, c := range cases {
		t.Run(c.name+" of size "+c.size, func(t *testing.T) {
			h := blockHost{}
			h.device(c.name, c.devpath, map[string]string{"size": c.size})
			got := blockNames(probeBlock(t, fstest.MapFS(h)))
			if listed := slices.Contains(got, c.name); listed != c.listed {
				t.Errorf("devices = %v, want %s listed: %v", got, c.name, c.listed)
			}
		})
	}
}

func TestBlockProberUnreadable(t *testing.T) {
	h := blockHost{}
	h.device("sda", sataSSDPath, map[string]string{
		"size": "16", "queue/rotational": "0", "device/model": "ST4000NM", "device/vendor": "ATA",
	})
	dir := "sys/devices/" + sataSSDPath
	fsys := deniedFS{FS: fstest.MapFS(h), denied: map[string]bool{
		dir + "/device/model": true, dir + "/size": true, dir + "/queue/rotational": true,
	}}
	checkBlockDevice(t, blockDevice(t, probeBlock(t, fsys), "sda"), BlockDevice{
		Name: "sda", Type: "disk", DriveType: "unknown", Transport: "sata", PCIAddress: "0000:00:17.0",
		Vendor: "ATA", Model: "unknown", Partitions: []Partition{},
	})
}

func TestBlockProberNoBlockDevices(t *testing.T) {
	b := probeBlock(t, fstest.MapFS{"sys/devices/system/cpu/online": &fstest.MapFile{Data: []byte("0\n")}})
	got, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"devices":[]}` {
		t.Errorf("payload = %s, want {\"devices\":[]}", got)
	}
}

func TestBlockProberUnlistable(t *testing.T) {
	h := testBlockHost()
	fsys := deniedFS{FS: fstest.MapFS(h), denied: map[string]bool{"sys/block": true}}
	_, err := NewBlockProber(fsys).Probe(context.Background())
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("Probe error = %v, want a permission error", err)
	}
}

// jsonKeys returns the keys of the JSON object b in their order.
func jsonKeys(t *testing.T, b []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", b)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, fmt.Sprint(tok))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestBlockJSON(t *testing.T) {
	marshal := func(t *testing.T, v any) []byte {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	one := []string{"x"}

	t.Run("device field order", func(t *testing.T) {
		full := BlockDevice{
			Name: "x", Type: "x", SizeBytes: 1, LogicalBlockSizeBytes: 1, PhysicalBlockSizeBytes: 1, DriveType: "x",
			Removable: true, ReadOnly: true, Transport: "x", PCIAddress: "x", BusPath: "x", Vendor: "x", Model: "x",
			SerialNumber: "x", WWN: "x", DMName: "x", DMUUID: "x", MDLevel: "x", PartitionTableType: "x",
			FilesystemType: "x", FilesystemLabel: "x", FilesystemUUID: "x", MountPoints: one, Slaves: one, Holders: one,
			Partitions: []Partition{},
		}
		want := []string{
			"name", "type", "size_bytes", "logical_block_size_bytes", "physical_block_size_bytes", "drive_type",
			"removable", "read_only", "transport", "pci_address", "bus_path", "vendor", "model", "serial_number", "wwn",
			"dm_name", "dm_uuid", "md_level", "partition_table_type", "filesystem_type", "filesystem_label",
			"filesystem_uuid", "mount_points", "slaves", "holders", "partitions",
		}
		if got := jsonKeys(t, marshal(t, full)); !slices.Equal(got, want) {
			t.Errorf("keys = %v\nwant %v", got, want)
		}
	})

	t.Run("partition field order", func(t *testing.T) {
		full := Partition{
			Name: "x", Number: 1, StartBytes: 1, SizeBytes: 1, ReadOnly: true, PartitionType: "x", PartitionLabel: "x",
			PartitionUUID: "x", FilesystemType: "x", FilesystemLabel: "x", FilesystemUUID: "x", MountPoints: one, Holders: one,
		}
		want := []string{
			"name", "number", "start_bytes", "size_bytes", "read_only", "partition_type", "partition_label",
			"partition_uuid", "filesystem_type", "filesystem_label", "filesystem_uuid", "mount_points", "holders",
		}
		if got := jsonKeys(t, marshal(t, full)); !slices.Equal(got, want) {
			t.Errorf("keys = %v\nwant %v", got, want)
		}
		if got, want := jsonKeys(t, marshal(t, Partition{})), want[:5]; !slices.Equal(got, want) {
			t.Errorf("empty partition keys = %v, want %v", got, want)
		}
	})

	// A host whose mount table mounts nothing of its block devices.
	h := testBlockHost()
	h["proc/1/mountinfo"] = &fstest.MapFile{Data: []byte("37 1 0:30 / /run rw shared:7 - tmpfs tmpfs rw\n")}
	b := probeBlock(t, fstest.MapFS(h))

	t.Run("SATA SSD", func(t *testing.T) {
		want := []string{
			"name", "type", "size_bytes", "logical_block_size_bytes", "physical_block_size_bytes", "drive_type",
			"removable", "read_only", "transport", "pci_address", "vendor", "model", "wwn", "partitions",
		}
		if got := jsonKeys(t, marshal(t, blockDevice(t, b, "sda"))); !slices.Equal(got, want) {
			t.Errorf("keys = %v\nwant %v", got, want)
		}
	})

	t.Run("no partitions", func(t *testing.T) {
		got := string(marshal(t, blockDevice(t, b, "loop1")))
		want := `{"name":"loop1","type":"loop","size_bytes":4096,"logical_block_size_bytes":0,"physical_block_size_bytes":0,` +
			`"removable":false,"read_only":true,"partitions":[]}`
		if got != want {
			t.Errorf("loop1 = %s\nwant %s", got, want)
		}
	})

	t.Run("no mounts error", func(t *testing.T) {
		if keys := jsonKeys(t, marshal(t, b)); !slices.Equal(keys, []string{"devices"}) {
			t.Errorf("keys = %v, want [devices]", keys)
		}
	})
}

// udevEntry is a udev database file holding props as E: records, with a
// link and a tag around them as udev writes them.
func udevEntry(props map[string]string) *fstest.MapFile {
	var b strings.Builder
	b.WriteString("S:disk/by-path/synthetic\n")
	for _, k := range slices.Sorted(maps.Keys(props)) {
		fmt.Fprintf(&b, "E:%s=%s\n", k, props[k])
	}
	b.WriteString("G:systemd\n")
	return &fstest.MapFile{Data: []byte(b.String())}
}

// udevDisk is a host with one SATA disk, sda (8:0), its sysfs files under
// its device directory given by files and its udev properties by props;
// nil props leaves the disk without a udev entry.
func udevDisk(files, props map[string]string) blockHost {
	h := blockHost{}
	h.device("sda", sataSSDPath, map[string]string{"dev": "8:0", "size": "16", "queue/rotational": "0"})
	h.files("sys/devices/"+sataSSDPath, files)
	if props != nil {
		h[udevDataDir+"/b8:0"] = udevEntry(props)
	}
	return h
}

func TestBlockProberUdev(t *testing.T) {
	h := blockHost{}
	h.device("sda", sataSSDPath, map[string]string{
		"dev": "8:0", "size": "16", "queue/rotational": "0",
		"device/vendor": "ATA     ", "device/model": "SAMSUNG MZ7LH480        ", "device/wwid": "naa.5002538e00000000",
		"sda1/partition": "1", "sda1/start": "2048", "sda1/size": "8", "sda1/ro": "0", "sda1/dev": "8:1",
	})
	h[udevDataDir+"/b8:0"] = udevEntry(map[string]string{
		"ID_SERIAL":             "SAMSUNG_MZ7LH480_EXSSD000000001",
		"ID_SERIAL_SHORT":       "EXSSD000000001",
		"ID_WWN":                "0x5002538e00000000",
		"ID_WWN_WITH_EXTENSION": "0x5002538e00000000ext",
		"ID_PATH":               "pci-0000:00:17.0-ata-1.0",
		"ID_PART_TABLE_TYPE":    "gpt",
		"ID_MODEL":              "SAMSUNG_MZ7LH480",
		"ID_PART_TABLE_UUID":    "0b3c9a8e-5d1f-4e2a-9c7b-1f2e3d4c5b6a",
		"DEVNAME":               "/dev/sda",
	})
	h[udevDataDir+"/b8:1"] = udevEntry(map[string]string{
		"ID_PART_ENTRY_TYPE": "0fc63daf-8483-4772-8e79-3d69d8477de4",
		"ID_PART_ENTRY_NAME": "boot",
		"ID_PART_ENTRY_UUID": "7d4f1c2a-3b5e-4f6a-8c9d-0e1f2a3b4c5d",
		"ID_FS_TYPE":         "xfs",
		"ID_FS_LABEL":        "boot",
		"ID_FS_UUID":         "4a5b6c7d-8e9f-4a0b-9c1d-2e3f4a5b6c7d",
	})
	// An LVM volume whose filesystem signature is on the whole device.
	h.device("dm-0", "virtual/block/dm-0", map[string]string{
		"dev": "253:0", "size": "16", "dm/name": "vg0-root", "dm/uuid": "LVM-synthetic",
	})
	h[udevDataDir+"/b253:0"] = udevEntry(map[string]string{
		"ID_FS_TYPE": "xfs", "ID_FS_LABEL": "root", "ID_FS_UUID": "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", "DM_NAME": "vg0-root",
	})
	b := probeBlock(t, fstest.MapFS(h))

	t.Run("disk and partition", func(t *testing.T) {
		// The model stays sysfs's, which udev's ID_MODEL mangles.
		checkBlockDevice(t, blockDevice(t, b, "sda"), BlockDevice{
			Name: "sda", Type: "disk", SizeBytes: 16 * 512, DriveType: "ssd", Transport: "sata", PCIAddress: "0000:00:17.0",
			BusPath: "pci-0000:00:17.0-ata-1.0", Vendor: "ATA", Model: "SAMSUNG MZ7LH480", SerialNumber: "EXSSD000000001",
			WWN: "0x5002538e00000000ext", PartitionTableType: "gpt",
			Partitions: []Partition{{
				Name: "sda1", Number: 1, StartBytes: 2048 * 512, SizeBytes: 8 * 512,
				PartitionType: "0fc63daf-8483-4772-8e79-3d69d8477de4", PartitionLabel: "boot",
				PartitionUUID:  "7d4f1c2a-3b5e-4f6a-8c9d-0e1f2a3b4c5d",
				FilesystemType: "xfs", FilesystemLabel: "boot", FilesystemUUID: "4a5b6c7d-8e9f-4a0b-9c1d-2e3f4a5b6c7d",
			}},
		})
	})

	t.Run("filesystem on the whole device", func(t *testing.T) {
		checkBlockDevice(t, blockDevice(t, b, "dm-0"), BlockDevice{
			Name: "dm-0", Type: "dm", SizeBytes: 16 * 512, DMName: "vg0-root", DMUUID: "LVM-synthetic",
			FilesystemType: "xfs", FilesystemLabel: "root", FilesystemUUID: "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d",
			Partitions: []Partition{},
		})
	})
}

func TestBlockProberUdevPrecedence(t *testing.T) {
	// Each property set holds the winner and every source after it.
	serials := []struct {
		name         string
		files, props map[string]string
		want         string
	}{
		{"SCSI_IDENT_SERIAL", map[string]string{"device/serial": "SYSFS  "},
			map[string]string{"SCSI_IDENT_SERIAL": "IDENT", "ID_SCSI_SERIAL": "SCSI", "ID_SERIAL_SHORT": "SHORT"}, "IDENT"},
		{"ID_SCSI_SERIAL", map[string]string{"device/serial": "SYSFS  "},
			map[string]string{"ID_SCSI_SERIAL": "SCSI", "ID_SERIAL_SHORT": "SHORT"}, "SCSI"},
		{"ID_SERIAL_SHORT", map[string]string{"device/serial": "SYSFS  "},
			map[string]string{"ID_SERIAL_SHORT": "SHORT", "ID_SERIAL": "ATA_MODEL_SHORT"}, "SHORT"},
		{"sysfs serial", map[string]string{"device/serial": "SYSFS  "},
			map[string]string{"ID_SERIAL": "ATA_MODEL_SHORT"}, "SYSFS"},
		{"ID_SERIAL alone is not used", nil, map[string]string{"ID_SERIAL": "ATA_MODEL_SHORT"}, ""},
	}
	for _, c := range serials {
		t.Run("serial/"+c.name, func(t *testing.T) {
			if got := blockDevice(t, probeBlock(t, fstest.MapFS(udevDisk(c.files, c.props))), "sda").SerialNumber; got != c.want {
				t.Errorf("serial_number = %q, want %q", got, c.want)
			}
		})
	}

	sysfsWWIDs := map[string]string{"wwid": "naa.wwid  ", "device/wwid": "naa.device"}
	wwns := []struct {
		name         string
		files, props map[string]string
		want         string
	}{
		{"ID_WWN_WITH_EXTENSION", sysfsWWIDs,
			map[string]string{"ID_WWN_WITH_EXTENSION": "0xext", "ID_WWN": "0xwwn", "DM_WWN": "0xdm"}, "0xext"},
		{"ID_WWN", sysfsWWIDs, map[string]string{"ID_WWN": "0xwwn", "DM_WWN": "0xdm"}, "0xwwn"},
		{"DM_WWN", sysfsWWIDs, map[string]string{"DM_WWN": "0xdm"}, "0xdm"},
		{"sysfs wwid", sysfsWWIDs, map[string]string{}, "naa.wwid"},
		{"sysfs device/wwid", map[string]string{"device/wwid": "naa.device"}, map[string]string{}, "naa.device"},
	}
	for _, c := range wwns {
		t.Run("wwn/"+c.name, func(t *testing.T) {
			if got := blockDevice(t, probeBlock(t, fstest.MapFS(udevDisk(c.files, c.props))), "sda").WWN; got != c.want {
				t.Errorf("wwn = %q, want %q", got, c.want)
			}
		})
	}

	t.Run("model from ID_MODEL without device/model", func(t *testing.T) {
		got := blockDevice(t, probeBlock(t, fstest.MapFS(udevDisk(nil, map[string]string{"ID_MODEL": "QEMU_HARDDISK"}))), "sda")
		if got.Model != "QEMU_HARDDISK" {
			t.Errorf("model = %q, want QEMU_HARDDISK", got.Model)
		}
	})
}

func TestBlockProberNoUdevEntry(t *testing.T) {
	files := map[string]string{
		"device/vendor": "ATA", "device/model": "ST4000NM", "device/serial": "EXHDD003", "device/wwid": "naa.5000c500a0000000",
		"sda1/partition": "1", "sda1/start": "2048", "sda1/size": "8", "sda1/ro": "0", "sda1/dev": "8:1",
	}
	want := BlockDevice{
		Name: "sda", Type: "disk", SizeBytes: 16 * 512, DriveType: "ssd", Transport: "sata", PCIAddress: "0000:00:17.0",
		Vendor: "ATA", Model: "ST4000NM", SerialNumber: "EXHDD003", WWN: "naa.5000c500a0000000",
		Partitions: []Partition{{Name: "sda1", Number: 1, StartBytes: 2048 * 512, SizeBytes: 8 * 512}},
	}

	t.Run("no entry for the device", func(t *testing.T) {
		h := udevDisk(files, nil)
		h[udevDataDir+"/b8:16"] = udevEntry(map[string]string{"ID_PATH": "pci-0000:00:17.0-ata-2.0", "ID_FS_TYPE": "xfs"})
		checkBlockDevice(t, blockDevice(t, probeBlock(t, fstest.MapFS(h)), "sda"), want)
	})

	t.Run("no udev database", func(t *testing.T) {
		checkBlockDevice(t, blockDevice(t, probeBlock(t, fstest.MapFS(udevDisk(files, nil))), "sda"), want)
	})
}

// mountHost is testBlockHost with dev numbers on its LVM volume and a
// second one, vg0-data, a btrfs filesystem's device.
func mountHost() blockHost {
	h := testBlockHost()
	h.files("sys/devices/virtual/block/dm-0", map[string]string{"dev": "253:0"})
	h.device("dm-1", "virtual/block/dm-1", map[string]string{
		"dev": "253:1", "size": "16", "dm/name": "vg0-data", "dm/uuid": "LVM-synthetic",
	})
	return h
}

// testMountInfo has a partition mounted twice over itself and bound
// elsewhere, an LVM volume at / and at a path with an escaped space, two
// btrfs subvolumes with anonymous device numbers, a tmpfs and lines that
// do not parse.
const testMountInfo = `22 1 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:5 - proc proc rw
30 1 253:0 / / rw,relatime shared:1 - xfs /dev/mapper/vg0-root rw,attr2,inode64
31 30 8:1 / /boot rw,relatime shared:2 - xfs /dev/sda1 rw,attr2
32 31 8:1 / /boot rw,relatime shared:3 master:2 - xfs /dev/sda1 rw,attr2
33 30 8:1 /grub /srv/grub rw,relatime shared:2 - xfs /dev/sda1 rw,attr2
34 30 253:0 /var/lib/x\040y /var/lib/x\040y rw,relatime - xfs /dev/mapper/vg0-root rw
35 30 0:45 /@home /home rw,relatime shared:4 - btrfs /dev/sda2 rw,subvol=/@home
36 30 0:46 / /data rw,relatime shared:6 - btrfs /dev/mapper/vg0-data rw,subvol=/
37 30 0:30 / /run rw,nosuid,nodev shared:7 - tmpfs tmpfs rw,mode=755
not a mount line
38 30 x:y / /mnt rw - ext4 /dev/sdc2 rw
39 30 8:34 / /mnt rw shared:8
`

// mountPoints maps every device and partition with mount points to them.
func mountPoints(b *Block) map[string][]string {
	got := map[string][]string{}
	for _, d := range b.Devices {
		if d.MountPoints != nil {
			got[d.Name] = d.MountPoints
		}
		for _, p := range d.Partitions {
			if p.MountPoints != nil {
				got[p.Name] = p.MountPoints
			}
		}
	}
	return got
}

func TestBlockProberMountPoints(t *testing.T) {
	want := map[string][]string{
		"sda1": {"/boot", "/srv/grub"},
		"dm-0": {"/", "/var/lib/x y"},
		"sda2": {"/home"},
		"dm-1": {"/data"},
	}

	t.Run("host mount table", func(t *testing.T) {
		h := mountHost()
		h["proc/1/mountinfo"] = &fstest.MapFile{Data: []byte(testMountInfo)}
		// The agent's own table, which PrivateTmp gives extra binds.
		h["proc/self/mountinfo"] = &fstest.MapFile{Data: []byte(testMountInfo +
			"40 30 253:0 /tmp/systemd-private-x/tmp /tmp rw - xfs /dev/mapper/vg0-root rw\n")}
		b := probeBlock(t, fstest.MapFS(h))
		if got := mountPoints(b); !reflect.DeepEqual(got, want) {
			t.Errorf("mount points = %v\nwant %v", got, want)
		}
		if b.MountsError != "" {
			t.Errorf("mounts_error = %q, want none", b.MountsError)
		}
	})

	selfOnly := func(t *testing.T, fsys fs.FS) {
		t.Helper()
		b := probeBlock(t, fsys)
		if got := mountPoints(b); !reflect.DeepEqual(got, want) {
			t.Errorf("mount points = %v\nwant %v", got, want)
		}
		if b.MountsError != "" {
			t.Errorf("mounts_error = %q, want none", b.MountsError)
		}
	}

	t.Run("host table missing", func(t *testing.T) {
		h := mountHost()
		// The agent's own table, with PrivateTmp's bind of the root
		// filesystem over /tmp, which the host does not have.
		h["proc/self/mountinfo"] = &fstest.MapFile{Data: []byte(testMountInfo +
			"40 30 253:0 /tmp/systemd-private-abc-hwi-api.service-xyz/tmp /tmp rw,relatime shared:9 - xfs /dev/mapper/vg0-root rw\n")}
		selfOnly(t, fstest.MapFS(h))
	})

	t.Run("host table denied", func(t *testing.T) {
		h := mountHost()
		h["proc/1/mountinfo"] = &fstest.MapFile{Data: []byte("31 30 8:1 / /wrong rw - xfs /dev/sda1 rw\n")}
		h["proc/self/mountinfo"] = &fstest.MapFile{Data: []byte(testMountInfo)}
		selfOnly(t, deniedFS{FS: fstest.MapFS(h), denied: map[string]bool{"proc/1/mountinfo": true}})
	})

	// A host whose /proc hides PID 1 (hidepid): the agent reads its own
	// table, which holds the unit's ExecPaths= bind of the binary onto
	// itself, a mount on no host, and a bind of one file over another that
	// the host made itself.
	ownTableWithBinds := func() fs.FS {
		h := mountHost()
		h["proc/1/mountinfo"] = &fstest.MapFile{Data: []byte(testMountInfo)}
		h["proc/self/mountinfo"] = &fstest.MapFile{Data: []byte(testMountInfo +
			"41 30 253:0 /usr/bin/hwi-api /usr/bin/hwi-api ro,relatime shared:1 - xfs /dev/mapper/vg0-root rw\n" +
			"42 30 253:0 /etc/hosts.source /etc/hosts rw,relatime - xfs /dev/mapper/vg0-root rw\n")}
		h["usr/bin/hwi-api"] = &fstest.MapFile{Data: []byte("binary"), Mode: 0o755}
		h["etc/hosts"] = &fstest.MapFile{Data: []byte("127.0.0.1 localhost\n")}
		return deniedFS{FS: fstest.MapFS(h), denied: map[string]bool{"proc/1/mountinfo": true}}
	}

	t.Run("host table denied: the unit's self-bind is left out, a host file bind kept", func(t *testing.T) {
		got, err := NewBlockProber(ownTableWithBinds(), "/usr/bin/hwi-api").Probe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		b := got.(*Block)
		want := maps.Clone(want)
		want["dm-0"] = []string{"/", "/var/lib/x y", "/etc/hosts"}
		if got := mountPoints(b); !reflect.DeepEqual(got, want) {
			t.Errorf("mount points = %v\nwant %v", got, want)
		}
	})

	// The bind's root is relative to the file system it comes from, not to
	// /, so it differs from its mount point when the binary is on its own
	// /usr or in a Btrfs subvolume; only the mount point names the binary.
	for _, tc := range []struct {
		name, lines, dev string
		want             []string
	}{
		{
			name: "separate /usr",
			lines: "43 30 8:1 / /usr rw,relatime - xfs /dev/sda1 rw\n" +
				"44 43 8:1 /bin/hwi-api /usr/bin/hwi-api ro,relatime - xfs /dev/sda1 rw\n",
			dev:  "sda1",
			want: []string{"/boot", "/srv/grub", "/usr"},
		},
		{
			name:  "Btrfs subvolume",
			lines: "45 30 0:46 /@root/usr/bin/hwi-api /usr/bin/hwi-api ro,relatime - btrfs /dev/mapper/vg0-data rw,subvol=/@root\n",
			dev:   "dm-1",
			want:  []string{"/data"},
		},
	} {
		t.Run("host table denied: the unit's self-bind from a "+tc.name+" is left out", func(t *testing.T) {
			h := mountHost()
			h["proc/1/mountinfo"] = &fstest.MapFile{Data: []byte(testMountInfo)}
			h["proc/self/mountinfo"] = &fstest.MapFile{Data: []byte(testMountInfo + tc.lines)}
			fsys := deniedFS{FS: fstest.MapFS(h), denied: map[string]bool{"proc/1/mountinfo": true}}
			got, err := NewBlockProber(fsys, "/usr/bin/hwi-api").Probe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := maps.Clone(want)
			want[tc.dev] = tc.want
			if got := mountPoints(got.(*Block)); !reflect.DeepEqual(got, want) {
				t.Errorf("mount points = %v\nwant %v", got, want)
			}
		})
	}

	t.Run("host table readable: the unit's self-bind path is not filtered", func(t *testing.T) {
		// The host's table is the host's: a self-bind there is a real mount.
		h := mountHost()
		h["proc/1/mountinfo"] = &fstest.MapFile{Data: []byte(testMountInfo +
			"41 30 253:0 /usr/bin/hwi-api /usr/bin/hwi-api ro,relatime shared:1 - xfs /dev/mapper/vg0-root rw\n")}
		got, err := NewBlockProber(fstest.MapFS(h), "/usr/bin/hwi-api").Probe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := maps.Clone(want)
		want["dm-0"] = []string{"/", "/var/lib/x y", "/usr/bin/hwi-api"}
		if got := mountPoints(got.(*Block)); !reflect.DeepEqual(got, want) {
			t.Errorf("mount points = %v\nwant %v", got, want)
		}
	})

	t.Run("neither readable", func(t *testing.T) {
		h := mountHost()
		h["proc/1/mountinfo"] = &fstest.MapFile{Data: []byte(testMountInfo)}
		b := probeBlock(t, deniedFS{FS: fstest.MapFS(h), denied: map[string]bool{"proc/1/mountinfo": true}})
		if got := mountPoints(b); len(got) != 0 {
			t.Errorf("mount points = %v, want none", got)
		}
		if !strings.HasPrefix(b.MountsError, "reading the mount table: ") {
			t.Errorf("mounts_error = %q, want reading the mount table: <error>", b.MountsError)
		}
		if n := len(b.Devices); n != 14 {
			t.Errorf("%d devices listed, want 14", n)
		}
		got, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		if keys := jsonKeys(t, got); !slices.Equal(keys, []string{"devices", "mounts_error"}) {
			t.Errorf("keys = %v, want [devices mounts_error]", keys)
		}
	})
}

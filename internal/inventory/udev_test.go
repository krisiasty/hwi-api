package inventory

import (
	"context"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/krisiasty/hwi-api/apiv1"
)

// udevHost is a host root with a udev database and the sysfs behind it,
// its symlinks relative as the kernel writes them.
type udevHost fstest.MapFS

func (h udevHost) entry(id, content string) {
	h["run/udev/data/"+id] = &fstest.MapFile{Data: []byte(content)}
}
func (h udevHost) file(name, content string) { h[name] = &fstest.MapFile{Data: []byte(content)} }

// link adds the sysfs symlink name pointing at target, both relative to
// the root, written as a relative link.
func (h udevHost) link(name, target string) {
	// Both paths are absolute, so Rel cannot fail.
	rel, _ := filepath.Rel(path.Dir("/"+name), "/"+target)
	h[name] = &fstest.MapFile{Data: []byte(filepath.ToSlash(rel)), Mode: fs.ModeSymlink}
}

const (
	sdaPath  = "sys/devices/pci0000:00/0000:00:17.0/ata1/host0/target0:0:0/0:0:0:0/block/sda"
	nicPath  = "sys/devices/pci0000:00/0000:00:1c.0/0000:3b:00.0/net/eno1"
	sgPath   = "sys/devices/pci0000:00/0000:00:17.0/ata1/host0/target0:0:0/0:0:0:0/scsi_generic/sg0"
	cardPath = "sys/devices/pci0000:00/0000:00:02.0/drm/card0"
	vfioPath = "sys/devices/pci0000:00/0000:00:1c.0/0000:3b:00.1/vfio-dev/vfio!dev0"
)

func testUdevHost() udevHost {
	h := udevHost{}
	h.entry("b8:0", "S:disk/by-id/wwn-0x5000c500a1b2c3d4\nS:disk/by-path/pci-0000:00:17.0-ata-1\n"+
		"W:4\nI:123456789\nE:ID_WWN=0x5000c500a1b2c3d4\nE:ID_MODEL=ST4000NM\nE:ID_SERIAL_SHORT=EXHDD003\n"+
		"E:ID_PATH=pci-0000:00:17.0-ata-1\nG:systemd\nQ:systemd\nV:1\nL:0\nmalformed line\nE:=nokey\n")
	h.link("sys/dev/block/8:0", sdaPath)
	h.file(sdaPath+"/uevent", "MAJOR=8\nMINOR=0\nDEVNAME=sda\nDEVTYPE=disk\n")

	h.entry("n2", "I:1\nE:ID_NET_NAME_PATH=enp59s0f0\nE:ID_NET_NAME_ONBOARD=eno1\nE:ID_NET_DRIVER=i40e\nG:systemd\n")
	h.link("sys/class/net/eno1", nicPath)
	h.file(nicPath+"/ifindex", "2\n")
	h.file(nicPath+"/uevent", "INTERFACE=eno1\nIFINDEX=2\n")
	h.link("sys/class/net/lo", "sys/devices/virtual/net/lo")
	h.file("sys/devices/virtual/net/lo/ifindex", "1\n")

	h.entry("c21:0", "E:ID_PATH=pci-0000:00:17.0-ata-1\nG:systemd\n")
	h.link("sys/dev/char/21:0", sgPath)
	h.link(sgPath+"/subsystem", "sys/class/scsi_generic")
	h.file(sgPath+"/uevent", "MAJOR=21\nMINOR=0\nDEVNAME=sg0\n")

	h.entry("c226:0", "E:ID_PATH=pci-0000:00:02.0\nG:seat\nG:master-of-seat\n")
	h.link("sys/dev/char/226:0", cardPath)
	h.link(cardPath+"/subsystem", "sys/class/drm")
	h.file(cardPath+"/uevent", "MAJOR=226\nMINOR=0\nDEVNAME=dri/card0\nDEVTYPE=drm_minor\n")

	h.entry("c4:64", "E:ID_PATH=platform-serial8250\n")
	h.link("sys/dev/char/4:64", "sys/devices/platform/serial8250/tty/ttyS0")
	h.link("sys/devices/platform/serial8250/tty/ttyS0/subsystem", "sys/class/tty")

	h.entry("+pci:0000:3b:00.0", "E:ID_VENDOR_FROM_DATABASE=Intel Corporation\nE:ID_PCI_CLASS_FROM_DATABASE=Network controller\n")
	h.link("sys/bus/pci/devices/0000:3b:00.0", "sys/devices/pci0000:00/0000:00:1c.0/0000:3b:00.0")
	h.file("sys/devices/pci0000:00/0000:00:1c.0/0000:3b:00.0/uevent", "DRIVER=i40e\nPCI_SLOT_NAME=0000:3b:00.0\n")

	h.entry("+scsi_host:host0", "E:ID_PATH=pci-0000:00:17.0-ata-1\n")
	h.link("sys/class/scsi_host/host0", "sys/devices/pci0000:00/0000:00:17.0/ata1/host0/scsi_host/host0")
	h.file("sys/devices/pci0000:00/0000:00:17.0/ata1/host0/scsi_host/host0/uevent", "")

	h.entry("+nvme-subsystem:nvme-subsys0", "E:ID_MODEL=unresolved\n") // no sysfs entry

	// A sysname with a "!", which sysfs names the same way.
	h.entry("+vfio-dev:vfio!dev0", "G:systemd\n")
	h.link("sys/class/vfio-dev/vfio!dev0", vfioPath)
	h.file(vfioPath+"/uevent", "")

	h.entry("+memory:memory0", "E:ID_X=1\n")
	return h
}

func probeUdev(t *testing.T, root fs.FS, sub apiv1.Subsystem) []UdevDevice {
	t.Helper()
	v, err := NewUdevProber(root, UdevKinds[sub]).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe(%s): %v", sub, err)
	}
	return v.(*Udev).Devices
}

func ids(devs []UdevDevice) []string {
	var out []string
	for _, d := range devs {
		out = append(out, d.ID)
	}
	return out
}

// Each udev-* subsystem reports exactly its kinds of device.
func TestUdevProberKeepsOnlyItsKinds(t *testing.T) {
	h := testUdevHost()
	for sub, want := range map[apiv1.Subsystem][]string{
		apiv1.SubsystemUdevBlock: {"b8:0"},
		apiv1.SubsystemUdevNet:   {"n2"},
		apiv1.SubsystemUdevPCI:   {"+pci:0000:3b:00.0"},
		apiv1.SubsystemUdevNVMe:  {"+nvme-subsystem:nvme-subsys0"},
		apiv1.SubsystemUdevSCSI:  {"+scsi_host:host0", "c21:0"},
		apiv1.SubsystemUdevDRM:   {"c226:0"},
		apiv1.SubsystemUdevVFIO:  {"+vfio-dev:vfio!dev0"},
	} {
		if got := ids(probeUdev(t, fstest.MapFS(h), sub)); !slices.Equal(got, want) {
			t.Errorf("%s devices = %v, want %v", sub, got, want)
		}
	}
}

func TestUdevDiskEntry(t *testing.T) {
	d := probeUdev(t, fstest.MapFS(testUdevHost()), apiv1.SubsystemUdevBlock)[0]
	if d.Subsystem != "block" || d.Devpath != strings.TrimPrefix(sdaPath, "sys") || d.Devname != "/dev/sda" {
		t.Errorf("resolved as %q, %q, %q", d.Subsystem, d.Devpath, d.Devname)
	}
	if !slices.Equal(d.Links, []string{"disk/by-id/wwn-0x5000c500a1b2c3d4", "disk/by-path/pci-0000:00:17.0-ata-1"}) {
		t.Errorf("links = %v, want both, in udev's order", d.Links)
	}
	if !slices.Equal(d.Tags, []string{"systemd"}) || !slices.Equal(d.CurrentTags, []string{"systemd"}) {
		t.Errorf("tags = %v, current tags = %v", d.Tags, d.CurrentTags)
	}
	// The bookkeeping lines (I, L, V, W) and malformed ones are left out.
	want := map[string]string{
		"ID_WWN": "0x5000c500a1b2c3d4", "ID_MODEL": "ST4000NM", "ID_SERIAL_SHORT": "EXHDD003", "ID_PATH": "pci-0000:00:17.0-ata-1",
	}
	if len(d.Properties) != len(want) {
		t.Errorf("properties = %v, want only %v", d.Properties, want)
	}
	for k, v := range want {
		if d.Properties[k] != v {
			t.Errorf("property %s = %q, want %q", k, d.Properties[k], v)
		}
	}
}

func TestUdevResolvesEachIDForm(t *testing.T) {
	h := fstest.MapFS(testUdevHost())
	for _, tc := range []struct {
		sub                          apiv1.Subsystem
		id, subsystem, path, devname string
	}{
		{apiv1.SubsystemUdevNet, "n2", "net", "/" + strings.TrimPrefix(nicPath, "sys/"), ""},
		{apiv1.SubsystemUdevSCSI, "c21:0", "scsi_generic", "/" + strings.TrimPrefix(sgPath, "sys/"), "/dev/sg0"},
		{apiv1.SubsystemUdevDRM, "c226:0", "drm", "/" + strings.TrimPrefix(cardPath, "sys/"), "/dev/dri/card0"},
		{apiv1.SubsystemUdevPCI, "+pci:0000:3b:00.0", "pci", "/devices/pci0000:00/0000:00:1c.0/0000:3b:00.0", ""},
		{apiv1.SubsystemUdevSCSI, "+scsi_host:host0", "scsi_host", "/devices/pci0000:00/0000:00:17.0/ata1/host0/scsi_host/host0", ""},
		{apiv1.SubsystemUdevNVMe, "+nvme-subsystem:nvme-subsys0", "nvme-subsystem", "unknown", "unknown"},
		{apiv1.SubsystemUdevVFIO, "+vfio-dev:vfio!dev0", "vfio-dev", "/" + strings.TrimPrefix(vfioPath, "sys/"), ""},
	} {
		i := slices.IndexFunc(probeUdev(t, h, tc.sub), func(d UdevDevice) bool { return d.ID == tc.id })
		if i < 0 {
			t.Errorf("%s: %s not reported", tc.sub, tc.id)
			continue
		}
		d := probeUdev(t, h, tc.sub)[i]
		if d.Subsystem != tc.subsystem || d.Devpath != tc.path || d.Devname != tc.devname {
			t.Errorf("%s resolved as (%q, %q, %q), want (%q, %q, %q)",
				tc.id, d.Subsystem, d.Devpath, d.Devname, tc.subsystem, tc.path, tc.devname)
		}
	}
}

// An entry with no properties, links or tags still reports empty lists and
// an empty map, never null.
func TestUdevEmptyEntry(t *testing.T) {
	h := udevHost{}
	h.entry("b7:0", "I:1\n")
	d := probeUdev(t, fstest.MapFS(h), apiv1.SubsystemUdevBlock)[0]
	if d.Links == nil || d.Tags == nil || d.CurrentTags == nil || d.Properties == nil {
		t.Errorf("entry = %+v, want empty, non-nil lists and map", d)
	}
	if d.Devpath != "unknown" || d.Devname != "unknown" || d.Subsystem != "block" {
		t.Errorf("unresolvable entry = (%q, %q, %q)", d.Subsystem, d.Devpath, d.Devname)
	}
}

// A host without udev's database, such as a container without udevd,
// reports no devices rather than failing.
func TestUdevWithoutADatabase(t *testing.T) {
	devs := probeUdev(t, fstest.MapFS{}, apiv1.SubsystemUdevBlock)
	if devs == nil || len(devs) != 0 {
		t.Errorf("devices = %v, want an empty, non-nil list", devs)
	}
}

// A database that exists but cannot be listed fails the probe, so the last
// good list is kept.
func TestUdevUnlistableDatabaseFails(t *testing.T) {
	root := fstest.MapFS{udevDataDir: {Data: []byte("not a directory")}}
	if _, err := NewUdevProber(root, []string{"block"}).Probe(context.Background()); err == nil {
		t.Error("Probe succeeded with an unlistable database")
	}
}

// Every udev-* subsystem has kinds to report.
func TestEveryUdevSubsystemHasKinds(t *testing.T) {
	for _, s := range apiv1.Subsystems() {
		if !strings.HasPrefix(string(s), "udev-") {
			continue
		}
		if len(UdevKinds[s]) == 0 {
			t.Errorf("%s has no udev kinds", s)
		}
	}
	if len(UdevKinds) != 7 {
		t.Errorf("UdevKinds has %d subsystems, want 7", len(UdevKinds))
	}
}

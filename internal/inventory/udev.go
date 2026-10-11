package inventory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/krisiasty/hwi-api/apiv1"
)

// Udev is the payload of a udev-* subsystem: the devices of its kinds in
// udev's database.
type Udev struct {
	Devices []UdevDevice `json:"devices"`
}

// UdevDevice is one entry of the udev database, /run/udev/data/<id>, with
// the device's subsystem, sysfs path and node read from sysfs, which the
// database does not record. Properties, links and tags are exactly what
// udev stored; links and tags keep its order.
type UdevDevice struct {
	// ID is the entry's file name: b<major>:<minor> for a block device,
	// c<major>:<minor> for another device node, n<ifindex> for a network
	// interface, and +<subsystem>:<sysname> for a device with neither.
	ID string `json:"id"`
	// Subsystem, Devpath and Devname are "unknown" when sysfs does not
	// resolve the device; Devname is "" for a device without a node.
	Subsystem   string            `json:"subsystem"`
	Devpath     string            `json:"devpath"` // under /sys, e.g. /devices/pci0000:00/.../block/sda
	Devname     string            `json:"devname"` // e.g. /dev/sda
	Links       []string          `json:"links"`   // under /dev/, e.g. disk/by-id/wwn-0x5000c500a1b2c3d4
	Tags        []string          `json:"tags"`
	CurrentTags []string          `json:"current_tags"`
	Properties  map[string]string `json:"properties"`
}

const udevDataDir = "run/udev/data"

// UdevKinds maps each udev-* subsystem to the udev subsystems it reports.
var UdevKinds = map[apiv1.Subsystem][]string{
	apiv1.SubsystemUdevBlock: {"block"},
	apiv1.SubsystemUdevNet:   {"net"},
	apiv1.SubsystemUdevPCI:   {"pci"},
	apiv1.SubsystemUdevNVMe:  {"nvme", "nvme-subsystem"},
	apiv1.SubsystemUdevSCSI:  {"scsi", "scsi_host", "scsi_disk", "scsi_generic"},
	apiv1.SubsystemUdevDRM:   {"drm"},
	apiv1.SubsystemUdevVFIO:  {"vfio", "vfio-dev"},
}

// NewUdevProber reports the devices of the udev subsystems kinds, reading
// udev's database and sysfs from rootFS, the host root. An entry's kind
// comes from its database ID, or for a c<major>:<minor> one from sysfs;
// entries of other kinds are skipped before anything else is read. A host
// without a database, such as a container without udevd, reports no
// devices; one whose database cannot be listed fails the probe. An entry
// that cannot be read is skipped, and one whose device sysfs does not
// resolve keeps its udev data.
func NewUdevProber(rootFS fs.FS, kinds []string) Prober {
	return ProberFunc(func(context.Context) (any, error) {
		entries, err := fs.ReadDir(rootFS, udevDataDir)
		if errors.Is(err, fs.ErrNotExist) {
			return &Udev{Devices: []UdevDevice{}}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("listing the udev database: %w", err)
		}

		r := sysfsResolver{rootFS: rootFS}
		out := &Udev{Devices: make([]UdevDevice, 0, len(entries))}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			if kind, ok := r.kind(e.Name()); !ok || !slices.Contains(kinds, kind) {
				continue
			}
			b, err := fs.ReadFile(rootFS, udevDataDir+"/"+e.Name())
			if err != nil {
				continue
			}
			d := parseUdevEntry(b)
			d.ID = e.Name()
			d.Subsystem, d.Devpath, d.Devname = r.resolve(d.ID)
			out.Devices = append(out.Devices, d)
		}
		return out, nil
	})
}

// parseUdevEntry reads a database file as systemd's
// device_read_db_internal_filename does: one "<key>:<value>" line per
// record. E is a property, S a link under /dev/, G a tag and Q a current
// tag; the rest (I, L, V, W) is udev's bookkeeping and is left out.
// Malformed lines are skipped, as systemd skips them.
func parseUdevEntry(b []byte) UdevDevice {
	d := UdevDevice{Links: []string{}, Tags: []string{}, CurrentTags: []string{}, Properties: map[string]string{}}
	for line := range strings.SplitSeq(string(b), "\n") {
		if len(line) < 2 || line[1] != ':' {
			continue
		}
		value := line[2:]
		switch line[0] {
		case 'E':
			if k, v, ok := strings.Cut(value, "="); ok && k != "" {
				d.Properties[k] = v
			}
		case 'S':
			d.Links = append(d.Links, value)
		case 'G':
			d.Tags = append(d.Tags, value)
		case 'Q':
			d.CurrentTags = append(d.CurrentTags, value)
		}
	}
	return d
}

// sysfsResolver finds a database entry's device in sysfs. It reads the
// network interfaces' indexes once per probe.
type sysfsResolver struct {
	rootFS fs.FS
	netDev map[string]string // ifindex to interface name
}

// kind returns the udev subsystem of the device a database ID names:
// block for b, net for n, the named one for +<subsystem>:<sysname>, and
// for a c<major>:<minor> device node the one sysfs links it to.
func (r *sysfsResolver) kind(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	switch id[0] {
	case 'b':
		return "block", true
	case 'n':
		return "net", true
	case '+':
		sub, _, ok := strings.Cut(id[1:], ":")
		return sub, ok && sub != ""
	case 'c':
		p, ok := r.link("sys/dev/char/" + id[1:])
		if !ok {
			return "", false
		}
		s, ok := r.link("sys" + p + "/subsystem")
		return path.Base(s), ok
	}
	return "", false
}

// resolve returns the subsystem, devpath and devname of the device a
// database ID names, "unknown" for any it cannot find.
func (r *sysfsResolver) resolve(id string) (subsystem, devpath, devname string) {
	subsystem, devpath, devname = unknown, unknown, unknown
	if id == "" {
		return
	}
	// The kind is known from the ID even when sysfs does not resolve the
	// device, except for a c entry, whose kind only sysfs gives.
	if kind, ok := r.kind(id); ok {
		subsystem = kind
	}
	switch id[0] {
	case 'b', 'c':
		class := "block"
		if id[0] == 'c' {
			class = "char"
		}
		p, ok := r.link("sys/dev/" + class + "/" + id[1:])
		if !ok {
			return
		}
		devpath = p
	case 'n':
		name, ok := r.interfaceByIndex(id[1:])
		if !ok {
			return
		}
		if p, ok := r.link("sys/class/net/" + name); ok {
			devpath = p
		}
	case '+':
		sub, rest, ok := strings.Cut(id[1:], ":")
		if !ok {
			return
		}
		devpath = r.plusDevpath(sub, rest)
	default:
		return
	}
	if devpath != unknown {
		devname = r.devname(devpath)
	}
	return
}

// plusDevpath finds a +<subsystem>:<sysname> device under its bus or its
// class. The sysname is used as the ID has it: udev takes it from the sysfs
// entry's name, where a "/" in the kernel's name is already a "!".
func (r *sysfsResolver) plusDevpath(sub, sysname string) string {
	for _, p := range []string{"sys/bus/" + sub + "/devices/" + sysname, "sys/class/" + sub + "/" + sysname} {
		if devpath, ok := r.link(p); ok {
			return devpath
		}
	}
	return unknown
}

// link resolves the sysfs symlink at name, relative to the host root, to a
// devpath: the target's path under /sys.
func (r *sysfsResolver) link(name string) (string, bool) {
	target, err := fs.ReadLink(r.rootFS, name)
	if err != nil {
		return "", false
	}
	if !path.IsAbs(target) {
		target = path.Join("/", path.Dir(name), target)
	}
	devpath, ok := strings.CutPrefix(path.Clean(target), "/sys/")
	if !ok {
		return "", false
	}
	return "/" + devpath, true
}

// devname reads DEVNAME from the device's uevent: its node under /dev, or
// "" for a device without one.
func (r *sysfsResolver) devname(devpath string) string {
	b, err := fs.ReadFile(r.rootFS, "sys"+devpath+"/uevent")
	if err != nil {
		return unknown
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if name, ok := strings.CutPrefix(line, "DEVNAME="); ok && name != "" {
			return "/dev/" + name
		}
	}
	return ""
}

func (r *sysfsResolver) interfaceByIndex(index string) (string, bool) {
	if r.netDev == nil {
		r.netDev = map[string]string{}
		entries, _ := fs.ReadDir(r.rootFS, "sys/class/net")
		for _, e := range entries {
			b, err := fs.ReadFile(r.rootFS, "sys/class/net/"+e.Name()+"/ifindex")
			if err != nil {
				continue
			}
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				r.netDev[strconv.Itoa(n)] = e.Name()
			}
		}
	}
	name, ok := r.netDev[index]
	return name, ok
}

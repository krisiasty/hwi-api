package inventory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/krisiasty/hwi-api/internal/pciaddr"
)

// blockDir lists every block device the kernel has, as a symlink, named by
// the device's kernel name, to its directory in the sysfs device tree.
const blockDir = "sys/block"

// sectorSize is the unit of a block device's size, a partition's start and
// size in sysfs: the kernel counts 512-byte sectors whatever the device's
// block size.
const sectorSize = 512

// hostMountTable is the mount table of PID 1, the host's: the agent's own,
// selfMountTable, has the mounts systemd adds to the agent's namespace,
// such as PrivateTmp's binds of the root filesystem over /tmp and
// /var/tmp, which the host does not have. The agent's is read only when a
// /proc mounted with hidepid hides the host's.
const (
	hostMountTable = "proc/1/mountinfo"
	selfMountTable = "proc/self/mountinfo"
)

// Block is the block payload.
type Block struct {
	// Devices lists every block device in /sys/block's order, except an
	// unused loop, nbd, ram or zram device and a path of an NVMe multipath
	// namespace, which the kernel hides behind the namespace's head.
	Devices []BlockDevice `json:"devices"`
	// MountsError says why no device has mount points: neither the host's
	// mount table nor the agent's could be read.
	MountsError string `json:"mounts_error,omitempty"`
}

// BlockDevice is one block device: a disk, an optical drive, a loop device,
// or a device-mapper or MD device built on others. A field whose source
// does not exist does not apply to the device and is omitted; one whose
// source exists but cannot be read is "unknown" or zero, as in every
// payload. Fields from udev are omitted for a device udev has no entry for.
type BlockDevice struct {
	// Name is the kernel's name, as /dev/<name> names the device's node.
	Name string `json:"name"`
	// Type is "disk", "rom", "loop", "dm" or "md".
	Type                   string `json:"type"`
	SizeBytes              uint64 `json:"size_bytes"`
	LogicalBlockSizeBytes  uint64 `json:"logical_block_size_bytes"`
	PhysicalBlockSizeBytes uint64 `json:"physical_block_size_bytes"`
	// DriveType is "hdd", "ssd", "optical", "virtual" or "unknown" for a
	// disk or an optical drive, and omitted for anything built in software.
	DriveType string `json:"drive_type,omitempty"`
	Removable bool   `json:"removable"`
	ReadOnly  bool   `json:"read_only"`
	// Transport is how the device is attached: "nvme" (or an NVMe over
	// fabrics transport such as "tcp"), "sata", "sas", "usb", "iscsi",
	// "fc", "virtio" or "mmc". It is omitted when the device path names
	// none, as for a RAID controller's logical drive.
	Transport string `json:"transport,omitempty"`
	// PCIAddress is the controller the device hangs off, joining the pci
	// payload.
	PCIAddress string `json:"pci_address,omitempty"`
	// BusPath is udev's ID_PATH, such as pci-0000:00:17.0-ata-1.0.
	BusPath      string `json:"bus_path,omitempty"`
	Vendor       string `json:"vendor,omitempty"`
	Model        string `json:"model,omitempty"`
	SerialNumber string `json:"serial_number,omitempty"`
	WWN          string `json:"wwn,omitempty"`
	// DMName and DMUUID name a device-mapper device; the UUID's prefix,
	// such as LVM-, CRYPT- or mpath-, says what made it.
	DMName string `json:"dm_name,omitempty"`
	DMUUID string `json:"dm_uuid,omitempty"`
	// MDLevel is an MD device's RAID level, such as "raid1" or "container".
	MDLevel string `json:"md_level,omitempty"`
	// PartitionTableType is udev's ID_PART_TABLE_TYPE, "gpt" or "dos".
	PartitionTableType string `json:"partition_table_type,omitempty"`
	// FilesystemType, FilesystemLabel and FilesystemUUID describe a
	// signature on the whole device: a filesystem, an LVM physical volume
	// or a RAID member.
	FilesystemType  string   `json:"filesystem_type,omitempty"`
	FilesystemLabel string   `json:"filesystem_label,omitempty"`
	FilesystemUUID  string   `json:"filesystem_uuid,omitempty"`
	MountPoints     []string `json:"mount_points,omitempty"`
	// Slaves are the devices this one is built on and Holders the ones
	// built on it, by kernel name, so a consumer can walk from a mounted
	// volume to its disks and back.
	Slaves     []string    `json:"slaves,omitempty"`
	Holders    []string    `json:"holders,omitempty"`
	Partitions []Partition `json:"partitions"`
}

// Partition is one partition of a block device, by the same rules.
type Partition struct {
	Name            string   `json:"name"`
	Number          int      `json:"number"`
	StartBytes      uint64   `json:"start_bytes"`
	SizeBytes       uint64   `json:"size_bytes"`
	ReadOnly        bool     `json:"read_only"`
	PartitionType   string   `json:"partition_type,omitempty"`
	PartitionLabel  string   `json:"partition_label,omitempty"`
	PartitionUUID   string   `json:"partition_uuid,omitempty"`
	FilesystemType  string   `json:"filesystem_type,omitempty"`
	FilesystemLabel string   `json:"filesystem_label,omitempty"`
	FilesystemUUID  string   `json:"filesystem_uuid,omitempty"`
	MountPoints     []string `json:"mount_points,omitempty"`
	Holders         []string `json:"holders,omitempty"`
}

// NewBlockProber reports the block devices in rootFS, the host root. A
// host without /sys/block, such as a container without block devices,
// reports none; one whose /sys/block cannot be listed fails the probe. A
// device whose files cannot be read is still listed with what could be.
//
// selfBinds are the paths the agent's unit binds onto themselves, such as
// its binary for ExecPaths=: they are mounts in the agent's own mount
// namespace only, so they are left out when the host's mount table cannot
// be read and the agent's own is used instead.
func NewBlockProber(rootFS fs.FS, selfBinds ...string) Prober {
	return ProberFunc(func(context.Context) (any, error) { return readBlock(rootFS, selfBinds) })
}

func readBlock(fsys fs.FS, selfBinds []string) (*Block, error) {
	entries, err := fs.ReadDir(fsys, blockDir)
	if errors.Is(err, fs.ErrNotExist) {
		return &Block{Devices: []BlockDevice{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing block devices: %w", err)
	}
	out := &Block{Devices: make([]BlockDevice, 0, len(entries))}
	mounts, err := readMountTable(fsys, selfBinds)
	if err != nil {
		out.MountsError = err.Error()
	}
	for _, e := range entries {
		name := e.Name()
		dir := blockDevicePath(fsys, name)
		if skipBlockDevice(fsys, name, dir) {
			continue
		}
		out.Devices = append(out.Devices, readBlockDevice(fsys, mounts, name, dir))
	}
	return out, nil
}

// blockDevicePath resolves the device's listing to its directory in the
// device tree, relative to the root, such as
// sys/devices/pci0000:00/0000:00:17.0/ata1/host0/target0:0:0/0:0:0:0/block/sda.
// Every rule that reads the path, the transport, the PCI address and
// whether the device is virtual, reads this one. A listing that is not a
// link is read where it is, as a path without a controller.
func blockDevicePath(fsys fs.FS, name string) string {
	return resolveLink(fsys, blockDir+"/"+name)
}

// resolveLink is the path, relative to the root, that the symlink link
// points at, or link itself when it is not a link.
func resolveLink(fsys fs.FS, link string) string {
	target, err := fs.ReadLink(fsys, link)
	if err != nil {
		return link
	}
	if path.IsAbs(target) {
		return strings.TrimPrefix(path.Clean(target), "/")
	}
	return path.Join(path.Dir(link), target)
}

// unusedWhenEmpty names the devices the kernel creates ahead of use, which
// have size 0 until they are given something: a loop device without a
// file behind it, an nbd device without a server, a ram disk and a zram
// device not yet configured. An optical drive of size 0 is not one of
// them: it is a drive without a disc.
var unusedWhenEmpty = []string{"loop", "nbd", "ram", "zram"}

// skipBlockDevice reports the devices left out: an unused one, and a
// device the kernel marks hidden, a path of an NVMe multipath namespace
// whose head is listed instead.
func skipBlockDevice(fsys fs.FS, name, dir string) bool {
	if readOptional(fsys, dir+"/hidden") == "1" {
		return true
	}
	if slices.ContainsFunc(unusedWhenEmpty, func(p string) bool { return strings.HasPrefix(name, p) }) {
		if size, ok := readUint(fsys, dir+"/size"); ok && size == 0 {
			return true
		}
	}
	return false
}

func readBlockDevice(fsys fs.FS, mounts mountTable, name, dir string) BlockDevice {
	dev := readDevNumber(fsys, dir)
	udev := udevProperties(fsys, dev)
	d := BlockDevice{
		Name:                   name,
		Type:                   blockType(fsys, name, dir),
		SizeBytes:              readSectors(fsys, dir+"/size"),
		LogicalBlockSizeBytes:  readUintOrZero(fsys, dir+"/queue/logical_block_size"),
		PhysicalBlockSizeBytes: readUintOrZero(fsys, dir+"/queue/physical_block_size"),
		Removable:              readFlag(fsys, dir+"/removable"),
		ReadOnly:               readFlag(fsys, dir+"/ro"),
		Transport:              transport(fsys, name, dir),
		PCIAddress:             devicePCIAddress(fsys, dir),
		BusPath:                udev["ID_PATH"],
		Vendor:                 readOptional(fsys, dir+"/device/vendor"),
		// An NVMe namespace's device is its controller, which has the
		// model and serial number; a SCSI device has the model only.
		// sysfs's model is the drive's own, where udev's ID_MODEL has its
		// spaces replaced, so udev's is only the fallback.
		Model: cmp.Or(readOptional(fsys, dir+"/device/model"), udev["ID_MODEL"]),
		// udev's serial numbers and WWNs come first, in this order: they
		// are what the drive reports through SCSI or ATA inquiries, where
		// sysfs has them only for some transports. ID_SERIAL is not one:
		// it prefixes the serial with the vendor and model.
		SerialNumber: cmp.Or(udev["SCSI_IDENT_SERIAL"], udev["ID_SCSI_SERIAL"], udev["ID_SERIAL_SHORT"],
			readOptional(fsys, dir+"/device/serial")),
		// An NVMe namespace has its own wwid; a SCSI disk's is its
		// device's.
		WWN: cmp.Or(udev["ID_WWN_WITH_EXTENSION"], udev["ID_WWN"], udev["DM_WWN"],
			readFirstOptional(fsys, dir+"/wwid", dir+"/device/wwid")),
		PartitionTableType: udev["ID_PART_TABLE_TYPE"],
		FilesystemType:     udev["ID_FS_TYPE"],
		FilesystemLabel:    udev["ID_FS_LABEL"],
		FilesystemUUID:     udev["ID_FS_UUID"],
		Slaves:             entryNames(fsys, dir+"/slaves"),
		Holders:            entryNames(fsys, dir+"/holders"),
		Partitions:         readPartitions(fsys, mounts, dir),
	}
	d.DriveType = driveType(fsys, name, dir, d.Type)
	switch d.Type {
	case "dm":
		d.DMName = readOptional(fsys, dir+"/dm/name")
		d.DMUUID = readOptional(fsys, dir+"/dm/uuid")
	case "md":
		d.MDLevel = readOptional(fsys, dir+"/md/level")
	}
	d.MountPoints = mounts.mountPoints(dev, name, d.DMName)
	return d
}

// blockType tells a device-mapper or MD device by the directory the kernel
// gives it, a loop device by its name, and an optical drive by its name or
// its SCSI peripheral type, 5 for a CD or DVD drive.
func blockType(fsys fs.FS, name, dir string) string {
	switch {
	case isDir(fsys, dir+"/dm"):
		return "dm"
	case isDir(fsys, dir+"/md"):
		return "md"
	case strings.HasPrefix(name, "loop"):
		return "loop"
	case strings.HasPrefix(name, "sr") || readOptional(fsys, dir+"/device/type") == "5":
		return "rom"
	}
	return "disk"
}

// driveType applies to a disk or an optical drive only. An NVMe namespace
// is an SSD, even when it sits under the virtual tree as a
// multipath head or a fabrics namespace does. A disk a hypervisor or the
// kernel emulates is virtual, since its rotational flag says nothing about
// the storage behind it; any other disk is what its rotational flag says.
func driveType(fsys fs.FS, name, dir, typ string) string {
	switch {
	case typ == "rom":
		return "optical"
	case typ != "disk":
		return ""
	case isNVMe(name, dir):
		return "ssd"
	case isVirtualDisk(name, dir):
		return "virtual"
	}
	switch readOptional(fsys, dir+"/queue/rotational") {
	case "1":
		return "hdd"
	case "0":
		return "ssd"
	}
	return unknown
}

// isNVMe tells an NVMe namespace by its controller or subsystem on the
// path, or, for a fabrics namespace, whose path has neither, by its name.
func isNVMe(name, dir string) bool {
	return hasComponent(dir, func(c string) bool { return c == "nvme" || c == "nvme-subsystem" }) ||
		strings.HasPrefix(name, "nvme")
}

// isVirtualDisk tells a virtio or Xen disk, and one the kernel builds
// itself under /devices/virtual, such as zram, rbd or nbd.
func isVirtualDisk(name, dir string) bool {
	return hasComponent(dir, func(c string) bool { return strings.HasPrefix(c, "virtio") || strings.HasPrefix(c, "vbd-") }) ||
		strings.HasPrefix(name, "xvd") ||
		strings.HasPrefix(dir, "sys/devices/virtual/")
}

// transportPrefixes maps a prefix of a device path's component to the
// transport it names, in the order the rules are tried.
var transportPrefixes = []struct{ prefix, transport string }{
	{"usb", "usb"},
	{"ata", "sata"},
	{"end_device-", "sas"},
	{"session", "iscsi"},
	{"rport-", "fc"},
	{"virtio", "virtio"},
	{"mmc_host", "mmc"},
}

// transport reads how the device is attached from its path, as lsblk's
// tran does. An NVMe namespace reports its controller's transport when it
// is a fabric's, such as tcp, rdma or fc, and nvme otherwise; a multipath
// head, whose device is its subsystem, reports the first of its
// controllers' that is a fabric's. A path that names none, such as a RAID
// controller's logical drive or anything built in software, gives "".
func transport(fsys fs.FS, name, dir string) string {
	if isNVMe(name, dir) {
		ctrls, ok := nvmeSubsystemControllers(fsys, dir)
		if !ok {
			ctrls = []string{dir + "/device"}
		}
		for _, c := range ctrls {
			switch t := readOptional(fsys, c+"/transport"); t {
			case "", "pcie", unknown:
			default:
				return t
			}
		}
		return "nvme"
	}
	for _, r := range transportPrefixes {
		if hasComponent(dir, func(c string) bool { return strings.HasPrefix(c, r.prefix) }) {
			return r.transport
		}
	}
	if strings.HasPrefix(name, "mmcblk") {
		return "mmc"
	}
	return ""
}

// devicePCIAddress is the last PCI address on the device's path: the
// controller, not a bridge or root port above it. An NVMe multipath head's
// path has none, so its controllers' paths give it, when every one that
// has a PCI address has the same: a dual-port drive's two controllers are
// two PCI functions, neither of them the drive's. It is "" for a device
// with no PCI device under it, such as one built in software, a fabrics
// namespace or a Xen disk.
func devicePCIAddress(fsys fs.FS, dir string) string {
	ctrls, ok := nvmeSubsystemControllers(fsys, dir)
	if !ok {
		return lastPCIAddress(dir)
	}
	addr := ""
	for _, c := range ctrls {
		switch a := lastPCIAddress(c); {
		case a == "":
		case addr == "":
			addr = a
		case a != addr:
			return ""
		}
	}
	return addr
}

// lastPCIAddress is the last component of the path p that is a PCI
// address, or "".
func lastPCIAddress(p string) string {
	for _, c := range slices.Backward(strings.Split(p, "/")) {
		if pciaddr.Valid(c) {
			return c
		}
	}
	return ""
}

// nvmeSubsystemControllers lists, for a native NVMe multipath head, whose
// path is under its subsystem's directory, the subsystem's controllers in
// name order: the links named nvme<N> the kernel puts in that directory,
// resolved. The heads beside them, nvme<N>n<M>, are not controllers. It
// reports false for a device not under an NVMe subsystem.
func nvmeSubsystemControllers(fsys fs.FS, dir string) ([]string, bool) {
	parts := strings.Split(dir, "/")
	i := slices.Index(parts, "nvme-subsystem")
	if i < 0 || i+1 >= len(parts) {
		return nil, false
	}
	sub := strings.Join(parts[:i+2], "/")
	entries, _ := fs.ReadDir(fsys, sub)
	var ctrls []string
	for _, e := range entries {
		if n, found := strings.CutPrefix(e.Name(), "nvme"); found && isDecimal(n) {
			ctrls = append(ctrls, resolveLink(fsys, sub+"/"+e.Name()))
		}
	}
	return ctrls, true
}

// readPartitions lists the device's partitions, the subdirectories with a
// partition file, by number. The others, such as holders, queue or power,
// are not partitions.
func readPartitions(fsys fs.FS, mounts mountTable, dir string) []Partition {
	parts := []Partition{}
	entries, _ := fs.ReadDir(fsys, dir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := dir + "/" + e.Name()
		if _, err := fs.Stat(fsys, p+"/partition"); err != nil {
			continue
		}
		dev := readDevNumber(fsys, p)
		udev := udevProperties(fsys, dev)
		parts = append(parts, Partition{
			Name:            e.Name(),
			Number:          readInt(fsys, p+"/partition"),
			StartBytes:      readSectors(fsys, p+"/start"),
			SizeBytes:       readSectors(fsys, p+"/size"),
			ReadOnly:        readFlag(fsys, p+"/ro"),
			PartitionType:   udev["ID_PART_ENTRY_TYPE"],
			PartitionLabel:  udev["ID_PART_ENTRY_NAME"],
			PartitionUUID:   udev["ID_PART_ENTRY_UUID"],
			FilesystemType:  udev["ID_FS_TYPE"],
			FilesystemLabel: udev["ID_FS_LABEL"],
			FilesystemUUID:  udev["ID_FS_UUID"],
			MountPoints:     mounts.mountPoints(dev, e.Name(), ""),
			Holders:         entryNames(fsys, p+"/holders"),
		})
	}
	slices.SortStableFunc(parts, func(a, b Partition) int { return cmp.Compare(a.Number, b.Number) })
	return parts
}

// readDevNumber is the <major>:<minor> of the device or partition in dir,
// which names its udev entry and its lines in the mount table, or "" when
// it cannot be read.
func readDevNumber(fsys fs.FS, dir string) string {
	if dev := readOptional(fsys, dir+"/dev"); dev != unknown {
		return dev
	}
	return ""
}

// udevProperties is the properties udev stored for the block device dev,
// or nil when dev is unknown, udev has no entry for it or the host has no
// udev database: every udev-sourced field is then omitted.
func udevProperties(fsys fs.FS, dev string) map[string]string {
	if dev == "" {
		return nil
	}
	b, err := fs.ReadFile(fsys, udevDataDir+"/b"+dev)
	if err != nil {
		return nil
	}
	return parseUdevEntry(b).Properties
}

// mountTable maps a device to its mount points in the mount table's order:
// by <major>:<minor>, or, for a line with major 0, by its source under
// /dev, such as /dev/sda2 or /dev/mapper/vg0-data. btrfs gives each
// subvolume an anonymous device number, so its source is all that names
// the device. A nil table, one that could not be read, has no mount
// points.
type mountTable map[string][]string

// mountPoints is the mount points of the device or partition numbered dev
// and named name, and for a device-mapper device also dmName, with the
// duplicates of a filesystem mounted over itself removed.
func (m mountTable) mountPoints(dev, name, dmName string) []string {
	keys := []string{dev, "/dev/" + name}
	if dmName != "" && dmName != unknown {
		keys = append(keys, "/dev/mapper/"+dmName)
	}
	var out []string
	for _, k := range keys {
		if k == "" {
			continue
		}
		for _, mp := range m[k] {
			if !slices.Contains(out, mp) {
				out = append(out, mp)
			}
		}
	}
	return out
}

// readMountTable reads the host's mount table, or the agent's when the
// host's cannot be read, as on a host whose /proc hides PID 1 (hidepid).
// The agent's own table also holds the mounts systemd makes for its unit:
// PrivateTmp's binds, which parseMountLine drops, and the unit's binds of a
// path onto itself, selfBinds, dropped here by their mount point. Other
// binds, of a directory or of a single file, are the host's and are kept.
func readMountTable(fsys fs.FS, selfBinds []string) (mountTable, error) {
	b, err := fs.ReadFile(fsys, hostMountTable)
	own := err != nil
	if own {
		b, err = fs.ReadFile(fsys, selfMountTable)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the mount table: %w", err)
	}
	m := mountTable{}
	for line := range strings.SplitSeq(string(b), "\n") {
		if own && isSelfBind(line, selfBinds) {
			continue
		}
		if key, mountPoint, ok := parseMountLine(line); ok {
			m[key] = append(m[key], mountPoint)
		}
	}
	return m, nil
}

// isSelfBind tells whether a mountinfo line is mounted on one of paths,
// the paths the agent's unit binds onto themselves. Only the mount point
// can name such a path: the line's root, the path it binds, is relative
// to the root of the file system it comes from, so it differs from the
// mount point when the path is on a separate /usr or in a Btrfs subvolume.
func isSelfBind(line string, paths []string) bool {
	f := strings.Fields(line)
	if len(f) < 5 {
		return false
	}
	return slices.Contains(paths, unescapeMount(f[4]))
}

// parseMountLine reads one line of a mountinfo file, "<id> <parent>
// <major>:<minor> <root> <mount point> <options> [optional fields] -
// <fstype> <source> <super options>", into its mountTable key and mount
// point. The optional fields end at a lone "-". A line that does not
// parse, one with major 0 whose source is not under /dev, such as a tmpfs
// or proc, and a PrivateTmp bind give false.
func parseMountLine(line string) (key, mountPoint string, ok bool) {
	f := strings.Fields(line)
	if len(f) < 7 || isPrivateTmpBind(f[3]) {
		return "", "", false
	}
	sep := slices.Index(f[6:], "-")
	if sep < 0 || 6+sep+2 >= len(f) {
		return "", "", false
	}
	major, minor, found := strings.Cut(f[2], ":")
	if !found || !isDecimal(major) || !isDecimal(minor) {
		return "", "", false
	}
	key = f[2]
	if major == "0" {
		key = unescapeMount(f[6+sep+2])
		if !strings.HasPrefix(key, "/dev/") {
			return "", "", false
		}
	}
	return key, unescapeMount(f[4]), true
}

// isPrivateTmpBind tells, by its root, a bind systemd's PrivateTmp= makes
// in a service's namespace from a directory it creates on the host, such
// as /tmp/systemd-private-<boot id>-<unit>-<suffix>/tmp, over /tmp or
// /var/tmp. The agent's own mount table has these, and is read when the
// host's cannot be, but the host has no such mounts.
func isPrivateTmpBind(root string) bool {
	return hasComponent(root, func(c string) bool { return strings.HasPrefix(c, "systemd-private-") })
}

func isDecimal(s string) bool {
	_, err := strconv.ParseUint(s, 10, 32)
	return err == nil
}

// mountEscapes undoes the octal escapes the kernel writes in a mountinfo
// path for the characters that would break the line into fields.
var mountEscapes = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

func unescapeMount(s string) string { return mountEscapes.Replace(s) }

// hasComponent reports whether any element of the path p satisfies match.
func hasComponent(p string, match func(string) bool) bool {
	return slices.ContainsFunc(strings.Split(p, "/"), match)
}

func isDir(fsys fs.FS, name string) bool {
	fi, err := fs.Stat(fsys, name)
	return err == nil && fi.IsDir()
}

// entryNames lists the entries of dir by name, sorted, or nil when it is
// empty or missing.
func entryNames(fsys fs.FS, dir string) []string {
	entries, _ := fs.ReadDir(fsys, dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// readFirstOptional is the first of names that exists and is not empty,
// as readOptional reads it. It is "unknown" when none is readable but one
// exists, and "" when none exists.
func readFirstOptional(fsys fs.FS, names ...string) string {
	value := ""
	for _, name := range names {
		switch v := readOptional(fsys, name); v {
		case "":
		case unknown:
			value = v
		default:
			return v
		}
	}
	return value
}

// readFlag reads a sysfs boolean, true when it reads 1 and false when it
// reads anything else or cannot be read.
func readFlag(fsys fs.FS, name string) bool {
	return readOptional(fsys, name) == "1"
}

// readUint reads name as a decimal unsigned integer, reporting false when
// it cannot be read or parsed.
func readUint(fsys fs.FS, name string) (uint64, bool) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func readUintOrZero(fsys fs.FS, name string) uint64 {
	n, _ := readUint(fsys, name)
	return n
}

// readSectors reads a count of 512-byte sectors in bytes, or 0 when it
// cannot be read.
func readSectors(fsys fs.FS, name string) uint64 {
	return readUintOrZero(fsys, name) * sectorSize
}

package inventory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/krisiasty/hwi-api/internal/helper"
	"github.com/krisiasty/hwi-api/internal/pciids"
)

// pciDevicesDir lists every PCI device as a symlink, named by its address,
// to the device's directory in the sysfs device tree.
const pciDevicesDir = "sys/bus/pci/devices"

// pciSlotsDir has a directory for each physical slot the kernel knows of,
// named by the slot and holding its address.
const pciSlotsDir = "sys/bus/pci/slots"

// pciKernelNameKinds are the device subdirectories naming what the kernel
// built on a PCI device.
var pciKernelNameKinds = []string{"drm", "infiniband", "net", "nvme"}

// pciAddress matches a PCI address, domain:bus:device.function. A domain is
// usually four hex digits, but an Intel VMD domain has five, such as 10000;
// see helper.PCIAddressPattern, which the helper shares.
var pciAddress = regexp.MustCompile(helper.PCIAddressPattern)

// PCI is the pci payload.
type PCI struct {
	// Devices lists every PCI function in address order.
	Devices []PCIDevice `json:"devices"`
}

// PCIIdent is an ID the device reports, in lowercase hex, and its name from
// the PCI ID database bundled with the agent, or "unknown" for an ID it does
// not list.
type PCIIdent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PCISubsystem identifies the board built around the chip. Its vendor is
// often not the chip's.
type PCISubsystem struct {
	VendorID string `json:"vendor_id"`
	ID       string `json:"id"`
	Name     string `json:"name"`
}

// PCISlot is the physical slot a device sits in, as the kernel and the
// firmware name it; either can be missing.
type PCISlot struct {
	// Name is the kernel's name for the slot, usually its number as the
	// firmware or the slot's hotplug controller gives it, such as "3".
	Name string `json:"name,omitempty"`
	// Designation is the firmware's label for the slot, from SMBIOS, such
	// as "CPU1 SLOT3 PCI-E 4.0 X16"; the slots payload lists it too.
	Designation string `json:"designation,omitempty"`
}

// PCIDevice is one PCI function. A field whose source does not exist does
// not apply to the device and is omitted; one whose source exists but
// cannot be read is "unknown" or zero, as in every payload.
type PCIDevice struct {
	Address string `json:"address"`
	// ParentAddress is the bridge or port above the device, or "" for a
	// device directly under a host bridge.
	ParentAddress        string       `json:"parent_address,omitempty"`
	Vendor               PCIIdent     `json:"vendor"`
	Product              PCIIdent     `json:"product"`
	Subsystem            PCISubsystem `json:"subsystem"`
	Class                PCIIdent     `json:"class"`
	Subclass             PCIIdent     `json:"subclass"`
	ProgrammingInterface PCIIdent     `json:"programming_interface"`
	Revision             string       `json:"revision"`
	// Driver is the bound driver, or "" when none is bound.
	Driver string `json:"driver,omitempty"`
	// DriverModule is the kernel module providing Driver, or "" for a
	// driver built into the kernel.
	DriverModule string `json:"driver_module,omitempty"`
	IOMMUGroup   string `json:"iommu_group,omitempty"`
	// NUMANode is nil when the kernel gives the device no NUMA affinity.
	NUMANode *int `json:"numa_node,omitempty"`
	// PowerState is "D0" to "D3cold", on kernels that report it.
	PowerState string   `json:"power_state,omitempty"`
	Slot       *PCISlot `json:"slot,omitempty"`
	// Label is the firmware's name for an onboard device.
	Label string `json:"label,omitempty"`
	// KernelNames lists, by kind (pciKernelNameKinds), the names of what
	// the kernel built on the device, such as {"net": ["eno1"]}.
	KernelNames map[string][]string `json:"kernel_names,omitempty"`
	// Link is the PCI Express link, or nil for a device without one.
	Link *PCILink `json:"link,omitempty"`
	// SRIOV is the SR-IOV capability, on a physical function that has one.
	SRIOV *PCISRIOV `json:"sriov,omitempty"`
	// VFOf is, on a virtual function, its physical function's address.
	VFOf string `json:"vf_of,omitempty"`
	// VPD is the card's Vital Product Data, read through the privileged
	// helper, on a device that has a vpd file whose data could be read and
	// decoded.
	VPD *PCIVPD `json:"vpd,omitempty"`
	// VPDError says why a device with a vpd file has no VPD.
	VPDError string `json:"vpd_error,omitempty"`
}

// pciSources is what reading one device takes besides its own directory:
// the host root, the PCI ID database (nil names nothing) and the kernel's
// slots (pciSlots).
type pciSources struct {
	root  fs.FS
	db    *pciids.DB
	slots map[string]string
	// systemSlots are the firmware's slots that give an address, from the
	// SMBIOS table; none without the helper or the table, which the slots
	// payload reports.
	systemSlots []SystemSlot
	// vpd reads the devices' VPD for this probe through the privileged
	// helper's file system, or reports why it cannot.
	vpd *vpdReader
}

// newPCISources gathers one probe's sources. privileged is the helper's
// file system, nil without the helper; cache is the prober's own, kept
// across its probes.
func newPCISources(root fs.FS, db *pciids.DB, privileged fs.FS, cache *vpdCache) pciSources {
	src := pciSources{root: root, db: db, slots: pciSlots(root), vpd: &vpdReader{privileged: privileged, cache: cache, cached: cache.snapshot()}}
	if table, err := readSMBIOSTable(privileged); err == nil {
		if slots, err := systemSlots(table); err == nil {
			for _, s := range slots {
				if s.Address != "" {
					src.systemSlots = append(src.systemSlots, s)
				}
			}
		}
	}
	return src
}

// NewPCIProber reports the host's PCI devices from sysfs under rootFS, the
// host root, naming them from db, the bundled PCI ID database
// (pciids.Bundled); a nil db names nothing. It reads each device's VPD
// through privileged, the helper's file system, or none when it is nil,
// keeping what it read for when the device is powered down (vpdReader.read).
// Only a failure to list the devices fails the probe.
func NewPCIProber(rootFS fs.FS, db *pciids.DB, privileged fs.FS) Prober {
	cache := newVPDCache()
	return ProberFunc(func(context.Context) (any, error) {
		entries, err := fs.ReadDir(rootFS, pciDevicesDir)
		if err != nil {
			return nil, fmt.Errorf("listing PCI devices: %w", err)
		}
		out := &PCI{Devices: make([]PCIDevice, 0, len(entries))}
		src := newPCISources(rootFS, db, privileged, cache)
		for _, e := range entries {
			if d, ok := src.readDevice(e.Name()); ok {
				out.Devices = append(out.Devices, d)
			}
		}
		src.vpd.commit(out.Devices)
		return out, nil
	})
}

// readDevice reads the device at address, naming it from src.db, which may
// be nil, and placing it in src.slots. It reports false when the
// device has gone, removed before or during these reads, as a hot-unplug or
// a VF teardown does: leaving it out is accurate, where reporting it with
// whatever its remaining reads gave is not.
func (src pciSources) readDevice(address string) (PCIDevice, bool) {
	fsys, db, slots := src.root, src.db, src.slots
	dir := pciDevicesDir + "/" + address
	if !pciDeviceExists(fsys, dir) {
		return PCIDevice{}, false
	}

	vendor := readHexID(fsys, dir+"/vendor", 4)
	product := readHexID(fsys, dir+"/device", 4)
	subVendor := readHexID(fsys, dir+"/subsystem_vendor", 4)
	subID := readHexID(fsys, dir+"/subsystem_device", 4)
	class, subclass, progIf := splitPCIClass(readHexID(fsys, dir+"/class", 6))

	d := PCIDevice{
		Address:              address,
		ParentAddress:        pciParentAddress(fsys, address),
		Vendor:               PCIIdent{ID: vendor, Name: pciName(db.Vendor(vendor))},
		Product:              PCIIdent{ID: product, Name: pciName(db.Product(vendor, product))},
		Subsystem:            PCISubsystem{VendorID: subVendor, ID: subID, Name: pciName(db.Subsystem(vendor, product, subVendor, subID))},
		Class:                PCIIdent{ID: class, Name: pciName(db.Class(class))},
		Subclass:             PCIIdent{ID: subclass, Name: pciName(db.Subclass(class, subclass))},
		ProgrammingInterface: PCIIdent{ID: progIf, Name: pciName(db.ProgIf(class, subclass, progIf))},
		Revision:             readTrimmed(fsys, dir+"/revision"),
		Driver:               readLinkBase(fsys, dir+"/driver"),
		DriverModule:         readLinkBase(fsys, dir+"/driver/module"),
		IOMMUGroup:           readLinkBase(fsys, dir+"/iommu_group"),
		NUMANode:             readNUMANode(fsys, dir+"/numa_node"),
		PowerState:           readOptional(fsys, dir+"/power_state"),
		Label:                readOptional(fsys, dir+"/label"),
		KernelNames:          readKernelNames(fsys, dir),
		SRIOV:                readPCISRIOV(fsys, dir),
		VFOf:                 readLinkBase(fsys, dir+"/physfn"),
	}
	// A VF has no link of its own: its link files read registers the
	// PCIe specification reserves for VFs, which give "Unknown" and 0. Its
	// link is its physical function's.
	if d.VFOf == "" {
		d.Link = readPCILink(fsys, dir)
	}
	// A VF takes its physical function's slot: with ARI, VFs often sit at
	// device numbers the slot's address does not cover.
	slotOf := address
	if pciAddress.MatchString(d.VFOf) {
		slotOf = d.VFOf
	}
	bridge := class == "06"
	designation := slotDesignation(src.systemSlots, slotOf, bridge && slotOf == address, pciAncestors(src.root, slotOf))
	name, _ := pciSlotName(slots, slotOf)
	if name != "" || designation != "" {
		d.Slot = &PCISlot{Name: name, Designation: designation}
	}
	// The vpd file is root-only, but its existence is not: the daemon may
	// look, and asks the helper to read.
	if _, err := fs.Stat(src.root, dir+"/vpd"); err == nil {
		card := pciCardOf(d)
		asleep := d.PowerState == "D3cold" || readOptional(fsys, dir+"/power/runtime_status") == "suspended"
		d.VPD, d.VPDError = src.vpd.read(card, asleep)
	}
	if !pciDeviceExists(fsys, dir) {
		return PCIDevice{}, false
	}
	return d, true
}

// pciCard identifies the card at an address: a VPD read from it is reported
// again only while the same card is there. Its IDs name the card's model,
// not the card, so the prober also forgets an address's VPD whenever a
// probe sees the card go (vpdCache.update). A same-model swap between two
// probes is beyond it until a probe reads the new card's VPD: nothing
// sysfs offers without waking the card tells them apart.
type pciCard struct {
	address, vendor, device, subVendor, subDevice string
}

// identified reports whether every one of the card's IDs could be read. A
// card with an unreadable ID cannot be told from another such card by its
// IDs, so they neither match a cached card nor prove that one has gone,
// and what is read from it is not cached.
func (c pciCard) identified() bool {
	return c.vendor != unknown && c.device != unknown && c.subVendor != unknown && c.subDevice != unknown
}

// pciCardOf is the card a probe reported d as.
func pciCardOf(d PCIDevice) pciCard {
	return pciCard{address: d.Address, vendor: d.Vendor.ID, device: d.Product.ID, subVendor: d.Subsystem.VendorID, subDevice: d.Subsystem.ID}
}

// vpdCache keeps, for each address, the VPD last read from the card there
// while it was awake, until a probe sees the card go: the address empty,
// another model there, or the card's own VPD gone, unreadable or naming
// another card. Nothing else forgets it: a read the helper skipped or
// failed, or an ID or vpd file that could not be read, says nothing about
// the card. Each probe reads one snapshot and replaces the map when it
// ends, never changing a map once published, so the two are safe for
// concurrent use; but a probe's evidence is only as current as its
// listing, so the prober's probes must not overlap, as the Refresher's
// per-subsystem slot ensures.
type vpdCache struct {
	mu   sync.Mutex
	last map[string]cachedVPD
}

type cachedVPD struct {
	card pciCard
	vpd  *PCIVPD
}

func newVPDCache() *vpdCache { return &vpdCache{} }

// snapshot is the cache as it stands, for one probe to read.
func (c *vpdCache) snapshot() map[string]cachedVPD {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// update applies one probe's evidence. present is the card the probe
// reported at each address, fresh the VPD it read from identified cards,
// and gone the addresses whose card's own VPD showed it has gone. A cached
// card is forgotten when its address is not present, holds another model,
// or is gone; a fresh read is kept only for a card the probe reported, not
// for one removed while it was read.
func (c *vpdCache) update(present map[string]pciCard, fresh map[string]cachedVPD, gone map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[string]cachedVPD, len(c.last)+len(fresh))
	for address, e := range c.last {
		card, ok := present[address]
		if !ok || gone[address] || (card.identified() && card != e.card) {
			continue
		}
		next[address] = e
	}
	for address, e := range fresh {
		if present[address] == e.card {
			next[address] = e
		}
	}
	c.last = next
}

// vpdReader reads the devices' VPD through privileged, the helper's file
// system, for one probe.
type vpdReader struct {
	privileged fs.FS
	cache      *vpdCache
	// cached is the cache as the probe began, which it never changes.
	cached map[string]cachedVPD
	// fresh and gone are what the probe has learnt, for commit: the VPD
	// read from identified cards, and the addresses whose card's own VPD
	// showed it has gone. Both stay nil until needed.
	fresh map[string]cachedVPD
	gone  map[string]bool
	// failed is this probe's first VPD read that the helper did not answer,
	// or answered as hung. Each such read costs up to the client's timeout,
	// and one faulty card or a missing helper would cost it again for every
	// function and VF, stalling the refresh; so once one fails, the helper
	// is not asked again until the next probe.
	failed error
}

// read returns the VPD of card, or why there is none. A device that is
// asleep, runtime-suspended or in D3cold, is not read: the kernel resumes
// it, and its port, for a VPD read, so a timed probe would keep waking idle
// cards. It reports the VPD last read from it while awake instead, if any
// and if its IDs show it is the same card.
func (r *vpdReader) read(card pciCard, asleep bool) (*PCIVPD, string) {
	if r.privileged == nil {
		return nil, "the privileged helper is not configured"
	}
	cached, ok := r.cached[card.address]
	if asleep {
		if ok && card.identified() && cached.card == card {
			return cached.vpd, ""
		}
		return nil, "not read: the device is powered down, and reading its VPD would wake it"
	}
	if r.failed != nil {
		return nil, fmt.Sprintf("not read: an earlier VPD read in this refresh failed (%v)", r.failed)
	}
	v, why, cardFault := r.readHelper(card.address)
	switch {
	case cardFault:
		r.markGone(card.address)
	case v == nil:
		// The helper failed, which says nothing about the card.
	case card.identified():
		if r.fresh == nil {
			r.fresh = map[string]cachedVPD{}
		}
		r.fresh[card.address] = cachedVPD{card: card, vpd: v}
	case ok && !sameVPDCard(v, cached.vpd):
		// The IDs cannot say which card this is, but its VPD names
		// another card than the cached one.
		r.markGone(card.address)
	}
	return v, why
}

func (r *vpdReader) markGone(address string) {
	if r.gone == nil {
		r.gone = map[string]bool{}
	}
	r.gone[address] = true
}

// commit applies what this probe learnt, given the devices it reported.
// With nothing cached and nothing read, as on a host without the helper or
// without VPD, it does nothing.
func (r *vpdReader) commit(devices []PCIDevice) {
	if len(r.cached) == 0 && len(r.fresh) == 0 {
		return
	}
	present := make(map[string]pciCard, len(devices))
	for _, d := range devices {
		present[d.Address] = pciCardOf(d)
	}
	r.cache.update(present, r.fresh, r.gone)
}

// sameVPDCard reports whether two VPDs name the same card: the same serial
// and part numbers. The other keywords do not count, the read-write ones
// least, since a card can rewrite them itself. Two cards with neither
// number cannot be told apart this way, as two of a model cannot by IDs.
func sameVPDCard(a, b *PCIVPD) bool {
	return a.SerialNumber == b.SerialNumber && a.PartNumber == b.PartNumber
}

// readHelper asks the helper for the VPD at address and decodes it.
// cardFault reports a failure that is the card's own, its VPD gone,
// unreadable or malformed, rather than the helper's, which says nothing
// about the card.
func (r *vpdReader) readHelper(address string) (v *PCIVPD, why string, cardFault bool) {
	b, err := fs.ReadFile(r.privileged, "vpd/"+address)
	if err != nil {
		var answer *helper.AnswerError
		if errors.Is(err, helper.ErrUnavailable) || (errors.As(err, &answer) && answer.Code == helper.ErrCodeTimeout) {
			r.failed = err
			return nil, "reading the VPD: " + err.Error(), false
		}
		return nil, "reading the VPD: " + err.Error(), true
	}
	v, err = parseVPD(b)
	if err != nil {
		return nil, "decoding the VPD: " + err.Error(), true
	}
	return v, "", false
}

// pciDeviceExists reports whether the device directory dir is still there.
// One that cannot be examined counts as there.
func pciDeviceExists(fsys fs.FS, dir string) bool {
	_, err := fs.Stat(fsys, dir)
	return !errors.Is(err, fs.ErrNotExist)
}

// pciName is a name from the PCI ID database, or unknown for an ID it
// does not list, including one that could not be read.
func pciName(name string, ok bool) string {
	if !ok {
		return unknown
	}
	return name
}

// readHexID reads a sysfs ID file, such as vendor's "0x15b3", as the
// lowercase hex digits the PCI ID database is keyed by. It gives
// unknown when the file cannot be read or does not hold digits hex
// digits.
func readHexID(fsys fs.FS, name string, digits int) string {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return unknown
	}
	id := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(string(b)), "0x"))
	if len(id) != digits {
		return unknown
	}
	if _, err := strconv.ParseUint(id, 16, 32); err != nil {
		return unknown
	}
	return id
}

// splitPCIClass splits the class file's six hex digits into the class,
// subclass and programming interface IDs.
func splitPCIClass(id string) (string, string, string) {
	if len(id) != 6 {
		return unknown, unknown, unknown
	}
	return id[0:2], id[2:4], id[4:6]
}

// pciParentAddress is the parent's address, from where the device's listing
// links into the device tree: .../0000:3a:00.0/0000:3b:00.0 has the parent
// 0000:3a:00.0. A device directly under a host bridge, such as
// .../pci0000:3a/0000:3a:00.0, has none and gives "".
func pciParentAddress(fsys fs.FS, address string) string {
	target, err := fs.ReadLink(fsys, pciDevicesDir+"/"+address)
	if err != nil {
		return unknown
	}
	parent := path.Base(path.Dir(target))
	if !pciAddress.MatchString(parent) {
		return ""
	}
	return parent
}

// readLinkBase is the last element of the target of the symlink name, such
// as the driver's name from driver -> .../bus/pci/drivers/mlx5_core. It is
// "" when the link does not exist and unknown when it cannot be read.
func readLinkBase(fsys fs.FS, name string) string {
	target, err := fs.ReadLink(fsys, name)
	switch {
	case err == nil:
		return path.Base(target)
	case errors.Is(err, fs.ErrNotExist):
		return ""
	default:
		return unknown
	}
}

// readOptional is name's whitespace-trimmed content, "" when it does not
// exist, or unknown when it exists but cannot be read.
func readOptional(fsys fs.FS, name string) string {
	b, err := fs.ReadFile(fsys, name)
	switch {
	case err == nil:
		return strings.TrimSpace(string(b))
	case errors.Is(err, fs.ErrNotExist):
		return ""
	default:
		return unknown
	}
}

// readNUMANode reads numa_node, or nil when the kernel gives no affinity
// (-1) or the file cannot be read: zero is a real node, so it cannot stand
// in for an unknown one.
func readNUMANode(fsys fs.FS, name string) *int {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 {
		return nil
	}
	return &n
}

// readKernelNames lists the entries of dir's pciKernelNameKinds
// subdirectories, sorted, or nil when there are none. A virtio device's
// names sit one level down, under virtio<N>, so those are read too.
func readKernelNames(fsys fs.FS, dir string) map[string][]string {
	parents := []string{dir}
	entries, _ := fs.ReadDir(fsys, dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "virtio") {
			parents = append(parents, dir+"/"+e.Name())
		}
	}

	var out map[string][]string
	for _, kind := range pciKernelNameKinds {
		var names []string
		for _, parent := range parents {
			entries, err := fs.ReadDir(fsys, parent+"/"+kind)
			if err != nil {
				continue
			}
			for _, e := range entries {
				names = append(names, e.Name())
			}
		}
		if len(names) == 0 {
			continue
		}
		slices.Sort(names)
		if out == nil {
			out = map[string][]string{}
		}
		out[kind] = names
	}
	return out
}

// pciSlots maps each slot's address to the slot's name. The address is
// domain:bus:device, with no function, or domain:bus for a slot the kernel
// does not tie to one device number. A host whose kernel knows no slots
// gives an empty map; most VMs, and hosts without the pci_slot or a hotplug
// driver, have none. Where two slots claim one address, the first by name
// wins.
func pciSlots(fsys fs.FS) map[string]string {
	entries, err := fs.ReadDir(fsys, pciSlotsDir)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := fs.ReadFile(fsys, pciSlotsDir+"/"+e.Name()+"/address")
		if err != nil {
			continue
		}
		addr := strings.TrimSpace(string(b))
		if _, taken := out[addr]; !taken {
			out[addr] = e.Name()
		}
	}
	return out
}

// pciSlotName names the slot of the device at address, from pciSlots. A
// slot with the device's own domain:bus:device wins over one covering its
// whole bus, as lspci matches them.
func pciSlotName(slots map[string]string, address string) (string, bool) {
	slot := pciSlotAddress(address)
	if name, ok := slots[slot]; ok {
		return name, true
	}
	if i := strings.LastIndexByte(slot, ':'); i >= 0 {
		name, ok := slots[slot[:i]]
		return name, ok
	}
	return "", false
}

// pciSlotAddress is address without its function, the form a slot's
// address takes, so every function of a card matches the card's slot.
func pciSlotAddress(address string) string {
	if i := strings.LastIndexByte(address, '.'); i >= 0 {
		return address[:i]
	}
	return address
}

// pciAncestors lists the PCI addresses above the device at address, nearest
// first, from the path its listing links to: .../pci0000:3a/0000:3a:00.0/
// 0000:3b:00.0 has the one ancestor 0000:3a:00.0.
func pciAncestors(fsys fs.FS, address string) []string {
	target, err := fs.ReadLink(fsys, pciDevicesDir+"/"+address)
	if err != nil {
		return nil
	}
	parts := strings.Split(path.Dir(target), "/")
	var out []string
	for i := len(parts) - 1; i >= 0 && pciAddress.MatchString(parts[i]); i-- {
		out = append(out, parts[i])
	}
	return out
}

// slotDesignation names the firmware slot of the device at address, a
// bridge-class device or not, with ancestors above it, nearest first.
// Firmware records a slot at the card, at any one of its functions, or at
// the port or bridge above it. So a slot matches every function of the
// device it is recorded at, and every device below that address. A
// bridge-class device matches only a slot recorded at exactly its own
// address: a root port's sibling functions are other ports, or a dummy host
// bridge, leading to other slots. An exact match beats a sibling function's.
func slotDesignation(slots []SystemSlot, address string, bridge bool, ancestors []string) string {
	for _, s := range slots {
		if s.Address == address {
			return s.Designation
		}
	}
	if !bridge {
		for _, s := range slots {
			if s.Address != "" && pciSlotAddress(s.Address) == pciSlotAddress(address) {
				return s.Designation
			}
		}
	}
	for _, a := range ancestors {
		for _, s := range slots {
			if s.Address == a {
				return s.Designation
			}
		}
	}
	return ""
}

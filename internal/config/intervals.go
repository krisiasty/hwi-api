package config

import (
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
)

// DefaultIntervals is each subsystem's refresh period, chosen by how fast
// its data moves and what the probe costs.
//
// cpu, memory and topology share an interval on purpose: topology is a
// composite of the same per-node CPU lists and memory areas, so a slower
// topology would show a hot-added CPU in cpu while it was still missing
// from its NUMA node. pci sits with block because PCIe hotplug, notably
// NVMe hot-swap, should surface in both views together; its probe costs
// about 25 ms, which is affordable at this rate.
//
// links refreshes every minute because addresses and link states change
// often, and its probe is cheap: one netlink listing plus one small sysfs
// read per interface. network sits with pci: NIC hardware rarely changes,
// and each probe asks the kernel's ethtool netlink family four questions
// per NIC.
//
// The udev-* subsystems follow the hardware they describe: udev-drm refreshes
// less often because DRM devices rarely change, and the rest follow block,
// network and pci. udev-vfio refreshes as often as the rest because
// devices can be bound for passthrough as VMs move.
//
// slots refreshes with the DMI subsystems: the firmware writes its SMBIOS
// table at boot and never updates it while the host runs.
var DefaultIntervals = map[apiv1.Subsystem]time.Duration{
	apiv1.SubsystemNetwork:        5 * time.Minute,
	apiv1.SubsystemLinks:          time.Minute,
	apiv1.SubsystemCPU:            5 * time.Minute,
	apiv1.SubsystemMemory:         5 * time.Minute,
	apiv1.SubsystemTopology:       5 * time.Minute,
	apiv1.SubsystemBlock:          5 * time.Minute,
	apiv1.SubsystemPCI:            5 * time.Minute,
	apiv1.SubsystemOS:             15 * time.Minute,
	apiv1.SubsystemSlots:          time.Hour,
	apiv1.SubsystemChassis:        time.Hour,
	apiv1.SubsystemBIOS:           time.Hour,
	apiv1.SubsystemBaseboard:      time.Hour,
	apiv1.SubsystemProduct:        time.Hour,
	apiv1.SubsystemVirtualization: 5 * time.Minute,
	apiv1.SubsystemUdevBlock:      5 * time.Minute,
	apiv1.SubsystemUdevNet:        5 * time.Minute,
	apiv1.SubsystemUdevPCI:        5 * time.Minute,
	apiv1.SubsystemUdevNVMe:       5 * time.Minute,
	apiv1.SubsystemUdevSCSI:       5 * time.Minute,
	apiv1.SubsystemUdevDRM:        30 * time.Minute,
	apiv1.SubsystemUdevVFIO:       5 * time.Minute,
}

// EffectiveInterval is how often sub is actually refreshed: its override
// when one is set, else its DefaultIntervals entry, raised to the base
// Interval when lower: the base interval is the floor for every
// subsystem. The refresh loop runs each subsystem at exactly this
// interval, and validation warnings compare the same values, so the two
// can never disagree about a subsystem's interval.
func (r RefreshConfig) EffectiveInterval(sub apiv1.Subsystem) time.Duration {
	floor := r.Interval.Duration()
	if floor <= 0 {
		floor = time.Minute
	}
	interval := DefaultIntervals[sub]
	if override, ok := r.Subsystems[sub]; ok && override.Interval > 0 {
		interval = override.Interval.Duration()
	}
	return max(interval, floor)
}

// IsEnabled reports whether sub is collected: unless its entry disables
// it, it is.
func (r RefreshConfig) IsEnabled(sub apiv1.Subsystem) bool {
	entry, ok := r.Subsystems[sub]
	return !ok || entry.IsEnabled()
}

// Package apiv1 defines the wire contract served at /api/v1.
//
// It contains types and constants only. It is the one package outside
// internal/, because it is the Go form of a contract already published over
// HTTP; importing it commits a consumer to nothing else.
package apiv1

import (
	"encoding/json"
	"time"
)

// SchemaVersion identifies the envelope format.
const SchemaVersion = "v1"

// Subsystem names one collectable area of the inventory.
type Subsystem string

// The addressable subsystems.
const (
	SubsystemCPU            Subsystem = "cpu"
	SubsystemMemory         Subsystem = "memory"
	SubsystemTopology       Subsystem = "topology"
	SubsystemBlock          Subsystem = "block"
	SubsystemNetwork        Subsystem = "network"
	SubsystemLinks          Subsystem = "links"
	SubsystemPCI            Subsystem = "pci"
	SubsystemSlots          Subsystem = "slots"
	SubsystemChassis        Subsystem = "chassis"
	SubsystemBIOS           Subsystem = "bios"
	SubsystemBaseboard      Subsystem = "baseboard"
	SubsystemProduct        Subsystem = "product"
	SubsystemUdevBlock      Subsystem = "udev-block"
	SubsystemUdevNet        Subsystem = "udev-net"
	SubsystemUdevPCI        Subsystem = "udev-pci"
	SubsystemUdevNVMe       Subsystem = "udev-nvme"
	SubsystemUdevSCSI       Subsystem = "udev-scsi"
	SubsystemUdevDRM        Subsystem = "udev-drm"
	SubsystemUdevVFIO       Subsystem = "udev-vfio"
	SubsystemOS             Subsystem = "os"
	SubsystemVirtualization Subsystem = "virtualization"
)

// SubsystemAll labels a full snapshot. It is not addressable as an endpoint
// and never appears in Subsystems.
const SubsystemAll Subsystem = "all"

// ordered is the canonical order, used for snapshots, configuration
// listings and documentation so that output never depends on map iteration.
var ordered = []Subsystem{
	SubsystemCPU, SubsystemMemory, SubsystemTopology, SubsystemBlock,
	SubsystemNetwork, SubsystemLinks, SubsystemPCI, SubsystemSlots, SubsystemChassis,
	SubsystemBIOS, SubsystemBaseboard, SubsystemProduct,
	SubsystemUdevBlock, SubsystemUdevNet, SubsystemUdevPCI, SubsystemUdevNVMe,
	SubsystemUdevSCSI, SubsystemUdevDRM, SubsystemUdevVFIO,
	SubsystemOS, SubsystemVirtualization,
}

// Subsystems returns every addressable subsystem in canonical order. The
// returned slice is a copy; callers may sort or filter it freely.
func Subsystems() []Subsystem {
	out := make([]Subsystem, len(ordered))
	copy(out, ordered)
	return out
}

// ParseSubsystem resolves a path segment to a subsystem. Matching is exact
// and case sensitive, and "all" is rejected: the snapshot has its own route.
func ParseSubsystem(s string) (Subsystem, bool) {
	for _, known := range ordered {
		if Subsystem(s) == known {
			return known, true
		}
	}
	return "", false
}

// Envelope wraps every successful response.
type Envelope struct {
	SchemaVersion string    `json:"schema_version"`
	Subsystem     Subsystem `json:"subsystem"`
	// HostID identifies the machine: derived from its machine ID and
	// stable per operating system installation. It is "" when no machine
	// ID is readable; it is never omitted. The hostname is deliberately not
	// here: it is a label, not an identity, and the os payload reports it,
	// updated on each os refresh. DMI serials are absent for the same
	// reason: they are frequently unset, placeholder, or duplicated across
	// a batch of machines, so they are reported inside the product,
	// baseboard and chassis payloads as data rather than promoted to an
	// identity.
	HostID string `json:"host_id"`
	// CollectedAt must be supplied in UTC.
	CollectedAt time.Time `json:"collected_at"`
	Complete    bool      `json:"complete"`
	// Disabled lists subsystems switched off in configuration. They are
	// intentionally absent and do not affect Complete.
	Disabled []Subsystem `json:"disabled,omitempty"`
	// Errors names subsystems whose most recent collection failed. A
	// subsystem may appear here and still have data, which means last-good
	// data is being served; CollectedAt shows how old it is.
	Errors map[Subsystem]string `json:"errors,omitempty"`
	Data   json.RawMessage      `json:"data"`
}

// SnapshotEntry is one subsystem's slot inside a full snapshot's Data.
// Each carries its own collection time because subsystems refresh on
// different schedules.
type SnapshotEntry struct {
	CollectedAt time.Time       `json:"collected_at"`
	Data        json.RawMessage `json:"data"`
}

// Error is a machine-readable failure. Messages are deliberately generic:
// filesystem paths and upstream error text stay in the log.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrorResponse is the body of every non-2xx response.
type ErrorResponse struct {
	Error Error `json:"error"`
}

// Error codes.
const (
	CodeNotFound             = "not_found"
	CodeSubsystemDisabled    = "subsystem_disabled"
	CodeSubsystemUnavailable = "subsystem_unavailable"
	CodeUnauthorized         = "unauthorized"
	CodeForbidden            = "forbidden"
	CodeMethodNotAllowed     = "method_not_allowed"
	CodeTooManyRequests      = "too_many_requests"
	CodeInternal             = "internal"
)

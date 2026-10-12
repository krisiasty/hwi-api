// Package helper reads the root-only hardware data the daemon may not read
// itself, on its behalf: the DMI identity fields, the SMBIOS table and PCI
// Vital Product Data.
//
// The daemon runs with no capabilities, as a network-facing process should.
// The helper runs as `hwi-api helper`, one short-lived process per
// connection, started by systemd on a Unix socket only the daemon's group
// can reach, confined to /sys with CAP_DAC_READ_SEARCH as its only
// capability (packaging/hwi-api-helper@.service). It answers a fixed set
// of operations and nothing else: no paths, no commands, no patterns.
//
// The protocol is one JSON Request per line, each answered by one JSON
// Response line, any number per connection.
package helper

import "time"

// The operations.
const (
	// OpDMI reads one of the DMI names, named by Request.Name.
	OpDMI = "dmi"
	// OpVPD reads a PCI device's Vital Product Data, by Request.Address.
	OpVPD = "vpd"
)

// SMBIOSTable is the DMI name for the raw SMBIOS table.
const SMBIOSTable = "DMI"

// dmiNames is the allowlist of names OpDMI accepts, each mapped to the file
// it reads, relative to the host root: the four DMI identity fields the
// kernel makes root-only, and the SMBIOS table. The names are the files' own,
// as the daemon's collectors know them (inventory.RootOnlyDMIFiles and
// inventory.SMBIOSTableFile; a test there keeps them in step). Read it
// through DMIFile.
var dmiNames = map[string]string{
	"product_uuid":   "sys/class/dmi/id/product_uuid",
	"product_serial": "sys/class/dmi/id/product_serial",
	"board_serial":   "sys/class/dmi/id/board_serial",
	"chassis_serial": "sys/class/dmi/id/chassis_serial",
	SMBIOSTable:      "sys/firmware/dmi/tables/DMI",
}

// DMIFile returns the file, relative to the host root, that OpDMI reads for
// name, and whether name is on the allowlist.
func DMIFile(name string) (path string, ok bool) {
	path, ok = dmiNames[name]
	return path, ok
}

// Request is one request: an operation and its one argument.
type Request struct {
	Op      string `json:"op"`
	Name    string `json:"name,omitempty"`    // OpDMI: a DMI name (see DMIFile)
	Address string `json:"address,omitempty"` // OpVPD: a PCI address
}

// Response answers one Request: the file's contents in Data, or an Error
// code, with a Message for a person.
type Response struct {
	Data    []byte `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

// Error codes.
const (
	// ErrCodeNotFound means the host has no such file: a VM without
	// board_serial, or a device that has gone.
	ErrCodeNotFound = "not_found"
	// ErrCodeInvalid refuses a request that names no operation, file or
	// address the helper knows.
	ErrCodeInvalid = "invalid_request"
	// ErrCodeTooLarge refuses a file larger than its operation's cap.
	ErrCodeTooLarge = "too_large"
	// ErrCodeTimeout reports a read that did not finish; the helper exits
	// after answering it.
	ErrCodeTimeout = "timeout"
	// ErrCodeRead reports a read the kernel failed.
	ErrCodeRead = "read_failed"
)

// Size caps, one per kind of file, above anything real hardware gives.
const (
	maxDMIField    = 4 << 10
	maxSMBIOSTable = 1 << 20
	// maxVPD is the VPD format's own limit: its addresses are 15 bits.
	maxVPD = 32 << 10
	// maxRequestBytes bounds one request line.
	maxRequestBytes = 1 << 10
)

// DefaultReadTimeout is how long the helper waits for one read. A VPD read
// on faulty hardware can stall in the kernel; the helper then answers
// ErrCodeTimeout and exits.
const DefaultReadTimeout = 5 * time.Second

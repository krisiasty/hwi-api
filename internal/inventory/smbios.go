package inventory

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// MemoryModule is one SMBIOS Type 17 (Memory Device) structure: a slot,
// and the module in it when one is installed. Unreadable values follow the
// agent's convention: "unknown" for a string and zero otherwise. For an empty slot only Label, Location, Installed and
// FormFactor, which describes the slot, are read; everything else describes
// a module and is left unknown.
type MemoryModule struct {
	Label              string `json:"label"`    // bank locator, e.g. "P0 CHANNEL A"
	Location           string `json:"location"` // device locator, e.g. "DIMM_A1"
	Installed          bool   `json:"installed"`
	SizeBytes          int64  `json:"size_bytes"`
	Type               string `json:"type"` // e.g. "DDR5"
	FormFactor         string `json:"form_factor"`
	SpeedMTs           uint32 `json:"speed_mts"`
	ConfiguredSpeedMTs uint32 `json:"configured_speed_mts"`
	Rank               uint8  `json:"rank"`
	Vendor             string `json:"vendor"`
	PartNumber         string `json:"part_number"`
	SerialNumber       string `json:"serial_number"`
	// TotalWidthBits exceeds DataWidthBits on a module with ECC.
	TotalWidthBits uint16 `json:"total_width_bits"`
	DataWidthBits  uint16 `json:"data_width_bits"`
}

const (
	smbiosTypeSystemSlot   = 9
	smbiosTypeMemoryDevice = 17
	smbiosTypeEndOfTable   = 127
)

// smbiosStructure is one structure of the table: its formatted area,
// header included, and its strings, numbered from 1.
type smbiosStructure struct {
	typ       byte
	formatted []byte
	strings   []string
}

// parseSMBIOSTable splits a raw SMBIOS table, as in
// /sys/firmware/dmi/tables/DMI, into its structures. Each is a 4-byte
// header (type, length, handle), the rest of a formatted area of that
// length, and a string set ending in two NULs. It stops at the end-of-table
// structure, or at the end of the data when that falls exactly between two
// structures, as it does in a table written without the end marker. It fails
// on an empty table and on one that ends inside a structure, header
// included: whatever was cut off cannot be decoded, and serving the rest
// would under-report the modules without saying so.
func parseSMBIOSTable(table []byte) ([]smbiosStructure, error) {
	if len(table) == 0 {
		return nil, errors.New("the table is empty")
	}
	var out []smbiosStructure
	off := 0
	for off < len(table) {
		if off+4 > len(table) {
			return nil, fmt.Errorf("the table ends inside the header of a structure at offset %#x", off)
		}
		typ, length := table[off], int(table[off+1])
		if length < 4 {
			return nil, fmt.Errorf("structure at offset %#x has length %d, shorter than its header", off, length)
		}
		if off+length > len(table) {
			return nil, fmt.Errorf("structure at offset %#x is truncated", off)
		}
		end := bytes.Index(table[off+length:], []byte{0, 0})
		if end < 0 {
			return nil, fmt.Errorf("strings of the structure at offset %#x are truncated", off)
		}
		s := smbiosStructure{typ: typ, formatted: table[off : off+length]}
		// An empty string set is written as the two NULs alone.
		if set := table[off+length : off+length+end]; len(set) > 0 {
			for str := range bytes.SplitSeq(set, []byte{0}) {
				s.strings = append(s.strings, printable(str))
			}
		}
		if typ == smbiosTypeEndOfTable {
			return out, nil
		}
		out = append(out, s)
		off += length + end + 2
	}
	return out, nil
}

// printable replaces bytes outside printable ASCII with '.', as dmidecode
// does, so firmware noise cannot break the JSON or a terminal.
func printable(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c < 32 || c >= 127 {
			c = '.'
		}
		out[i] = c
	}
	return string(out)
}

// memoryModules decodes every Type 17 structure in table, in table order,
// which is the order the firmware lists its slots in.
func memoryModules(table []byte) ([]MemoryModule, error) {
	structures, err := parseSMBIOSTable(table)
	if err != nil {
		return nil, err
	}
	modules := []MemoryModule{}
	for _, s := range structures {
		if s.typ != smbiosTypeMemoryDevice {
			continue
		}
		m, err := decodeMemoryDevice(s)
		if err != nil {
			return nil, err
		}
		modules = append(modules, m)
	}
	return modules, nil
}

// decodeMemoryDevice reads a Type 17 structure. Offsets and value encodings
// follow DSP0134 section 7.18 as dmidecode decodes it; each field is read
// only when the structure is long enough to hold it, since older SMBIOS
// versions end earlier.
func decodeMemoryDevice(s smbiosStructure) (MemoryModule, error) {
	f := s.formatted
	if len(f) < 0x15 {
		return MemoryModule{}, fmt.Errorf("memory device structure has length %#x, shorter than SMBIOS 2.1's %#x", len(f), 0x15)
	}
	word := func(o int) uint16 { return binary.LittleEndian.Uint16(f[o:]) }
	dword := func(o int) uint32 { return binary.LittleEndian.Uint32(f[o:]) }

	m := MemoryModule{
		Label: s.str(0x11), Location: s.str(0x10), FormFactor: enumName(memoryFormFactors, f[0x0E]),
		Type: unknown, Vendor: unknown, PartNumber: unknown, SerialNumber: unknown,
	}
	size := word(0x0C)
	if size == 0 {
		return m, nil // no module installed
	}
	m.Installed = true
	switch {
	case size == 0xFFFF:
		// Unknown size: left 0.
	case size == 0x7FFF && len(f) >= 0x20:
		m.SizeBytes = int64(dword(0x1C)&0x7FFFFFFF) << 20
	case size&0x8000 != 0:
		m.SizeBytes = int64(size&0x7FFF) << 10
	default:
		m.SizeBytes = int64(size) << 20
	}

	m.TotalWidthBits = knownWidth(word(0x08))
	m.DataWidthBits = knownWidth(word(0x0A))
	m.Type = enumName(memoryTypes, f[0x12])

	if len(f) >= 0x17 {
		m.SpeedMTs = speed(word(0x15), f, 0x54)
	}
	if len(f) >= 0x1B {
		m.Vendor, m.SerialNumber, m.PartNumber = s.str(0x17), s.str(0x18), s.str(0x1A)
	}
	if len(f) >= 0x1C {
		m.Rank = f[0x1B] & 0x0F
	}
	if len(f) >= 0x22 {
		m.ConfiguredSpeedMTs = speed(word(0x20), f, 0x58)
	}
	return m, nil
}

// speed decodes a 16-bit speed in MT/s. 0 is unknown, and 0xFFFF defers to
// the 32-bit extended speed at ext, which SMBIOS 3.3 added (length 0x5C).
func speed(v uint16, f []byte, ext int) uint32 {
	if v != 0xFFFF {
		return uint32(v)
	}
	if len(f) >= 0x5C {
		return binary.LittleEndian.Uint32(f[ext:])
	}
	return 0
}

func knownWidth(v uint16) uint16 {
	if v == 0xFFFF {
		return 0
	}
	return v
}

// enumName maps an SMBIOS enumeration code to its name, with "unknown" for
// the spec's own Unknown and for codes outside the table.
func enumName(names []string, code byte) string {
	if code == 0 || int(code) > len(names) || names[code-1] == "Unknown" || names[code-1] == "" {
		return unknown
	}
	return names[code-1]
}

// memoryFormFactors are the Type 17 form factors from code 0x01 (7.18.1).
var memoryFormFactors = []string{
	"Other", "Unknown", "SIMM", "SIP", "Chip", "DIP", "ZIP", "Proprietary Card",
	"DIMM", "TSOP", "Row Of Chips", "RIMM", "SODIMM", "SRIMM", "FB-DIMM", "Die",
}

// memoryTypes are the Type 17 memory types from code 0x01 (7.18.2); the
// empty entries are reserved codes.
var memoryTypes = []string{
	"Other", "Unknown", "DRAM", "EDRAM", "VRAM", "SRAM", "RAM", "ROM",
	"Flash", "EEPROM", "FEPROM", "EPROM", "CDRAM", "3DRAM", "SDRAM", "SGRAM",
	"RDRAM", "DDR", "DDR2", "DDR2 FB-DIMM", "", "", "", "DDR3",
	"FBD2", "DDR4", "LPDDR", "LPDDR2", "LPDDR3", "LPDDR4", "Logical non-volatile device", "HBM",
	"HBM2", "DDR5", "LPDDR5", "HBM3",
}

// str is the string the formatted area's byte at o numbers, trimmed, or
// "unknown" when the structure is too short to hold that byte, the byte is
// 0 (no string) or out of range, or the string is blank.
func (s smbiosStructure) str(o int) string {
	if o >= len(s.formatted) {
		return unknown
	}
	i := int(s.formatted[o])
	if i == 0 || i > len(s.strings) {
		return unknown
	}
	if v := strings.TrimSpace(s.strings[i-1]); v != "" {
		return v
	}
	return unknown
}

// SystemSlot is one SMBIOS Type 9 (System Slots) structure: an expansion
// slot as the firmware describes it, empty or not. Unreadable values are
// "unknown", as in MemoryModule.
type SystemSlot struct {
	// Designation is the slot's name as the chassis or board labels it,
	// such as "PCIe Slot 3" or "CPU1 SLOT6 PCI-E 5.0 X16".
	Designation string `json:"designation"`
	// Type is the slot's type, with its generation and width where the
	// firmware gives them, such as "PCI Express 5 x16".
	Type string `json:"type"`
	// Width is the slot's data bus width, such as "x16".
	Width string `json:"width"`
	// InUse is true for a slot holding a card and false for an available
	// one; it is absent when the firmware does not say.
	InUse *bool `json:"in_use,omitempty"`
	// Address is the PCI address the firmware gives for the slot, from
	// SMBIOS 2.6. It is often the address of the port the slot hangs off,
	// not of a card in it. It is absent when the firmware gives none.
	Address string `json:"address,omitempty"`
}

// systemSlots decodes every Type 9 structure in table, in table order.
func systemSlots(table []byte) ([]SystemSlot, error) {
	structures, err := parseSMBIOSTable(table)
	if err != nil {
		return nil, err
	}
	slots := []SystemSlot{}
	for _, s := range structures {
		if s.typ != smbiosTypeSystemSlot {
			continue
		}
		slot, err := decodeSystemSlot(s)
		if err != nil {
			return nil, err
		}
		slots = append(slots, slot)
	}
	return slots, nil
}

// decodeSystemSlot reads a Type 9 structure. Offsets and encodings follow
// DSP0134 section 7.10, as dmidecode decodes them.
func decodeSystemSlot(s smbiosStructure) (SystemSlot, error) {
	f := s.formatted
	if len(f) < 0x0C {
		return SystemSlot{}, fmt.Errorf("system slot structure has length %#x, shorter than SMBIOS 2.0's %#x", len(f), 0x0C)
	}
	slot := SystemSlot{Designation: s.str(0x04), Type: slotType(f[0x05]), Width: enumName(slotWidths, f[0x06])}
	switch f[0x07] {
	case 3:
		slot.InUse = new(false)
	case 4:
		slot.InUse = new(true)
	}
	if len(f) >= 0x11 {
		slot.Address = slotAddress(binary.LittleEndian.Uint16(f[0x0D:]), f[0x0F], f[0x10])
	}
	return slot, nil
}

// slotAddress formats a slot's segment, bus and device/function as a PCI
// address, or "" for the values firmware writes when it gives none: 0xFF
// in the bus or device/function, 0xFFFF in the segment, or all zeros,
// which would name the host bridge.
func slotAddress(seg uint16, bus, devfn byte) string {
	if seg == 0xFFFF || bus == 0xFF || devfn == 0xFF || seg == 0 && bus == 0 && devfn == 0 {
		return ""
	}
	return fmt.Sprintf("%04x:%02x:%02x.%x", seg, bus, devfn>>3, devfn&7)
}

// slotType names a Type 9 slot type code (7.10.1).
func slotType(code byte) string {
	switch {
	case code >= 0xA0:
		return enumName(slotTypesA0, code-0xA0+1)
	case code == 0x30:
		return "CXL Flexbus 1.0"
	default:
		return enumName(slotTypes, code)
	}
}

// slotTypes are the slot types from code 0x01.
var slotTypes = []string{
	"Other", "Unknown", "ISA", "MCA", "EISA", "PCI", "PC Card (PCMCIA)", "VLB",
	"Proprietary", "Processor Card", "Proprietary Memory Card", "I/O Riser Card", "NuBus", "PCI-66", "AGP", "AGP 2x",
	"AGP 4x", "PCI-X", "AGP 8x", "M.2 Socket 1-DP", "M.2 Socket 1-SD", "M.2 Socket 2", "M.2 Socket 3", "MXM Type I",
	"MXM Type II", "MXM Type III", "MXM Type III-HE", "MXM Type IV", "MXM 3.0 Type A", "MXM 3.0 Type B",
	"PCI Express 2 SFF-8639 (U.2)", "PCI Express 3 SFF-8639 (U.2)",
	"PCI Express Mini 52-pin with bottom-side keep-outs", "PCI Express Mini 52-pin without bottom-side keep-outs",
	"PCI Express Mini 76-pin", "PCI Express 4 SFF-8639 (U.2)", "PCI Express 5 SFF-8639 (U.2)",
	"OCP NIC 3.0 Small Form Factor (SFF)", "OCP NIC 3.0 Large Form Factor (LFF)", "OCP NIC Prior to 3.0",
}

// slotTypesA0 are the slot types from code 0xA0; the empty entry is a code
// the specification leaves out.
var slotTypesA0 = []string{
	"PC-98/C20", "PC-98/C24", "PC-98/E", "PC-98/Local Bus", "PC-98/Card",
	"PCI Express", "PCI Express x1", "PCI Express x2", "PCI Express x4", "PCI Express x8", "PCI Express x16",
	"PCI Express 2", "PCI Express 2 x1", "PCI Express 2 x2", "PCI Express 2 x4", "PCI Express 2 x8", "PCI Express 2 x16",
	"PCI Express 3", "PCI Express 3 x1", "PCI Express 3 x2", "PCI Express 3 x4", "PCI Express 3 x8", "PCI Express 3 x16",
	"",
	"PCI Express 4", "PCI Express 4 x1", "PCI Express 4 x2", "PCI Express 4 x4", "PCI Express 4 x8", "PCI Express 4 x16",
	"PCI Express 5", "PCI Express 5 x1", "PCI Express 5 x2", "PCI Express 5 x4", "PCI Express 5 x8", "PCI Express 5 x16",
	"PCI Express 6+", "EDSFF E1", "EDSFF E3",
}

// slotWidths are the slot data bus widths from code 0x01 (7.10.2).
var slotWidths = []string{
	"Other", "Unknown", "8 bit", "16 bit", "32 bit", "64 bit", "128 bit",
	"x1", "x2", "x4", "x8", "x12", "x16", "x32",
}

package inventory

import (
	"encoding/binary"
	"strings"
	"testing"
)

// structure builds one SMBIOS structure: a formatted area of length bytes,
// header included, filled in by set, then its strings.
func structure(typ byte, length int, set func(f []byte), strs ...string) []byte {
	f := make([]byte, length)
	f[0], f[1] = typ, byte(length)
	binary.LittleEndian.PutUint16(f[2:], 0x1100)
	if set != nil {
		set(f)
	}
	if len(strs) == 0 {
		return append(f, 0, 0)
	}
	for _, s := range strs {
		f = append(append(f, s...), 0)
	}
	return append(f, 0)
}

func table(structures ...[]byte) []byte {
	var out []byte
	for _, s := range structures {
		out = append(out, s...)
	}
	return append(out, structure(smbiosTypeEndOfTable, 4, nil)...)
}

func putWord(f []byte, o int, v uint16)  { binary.LittleEndian.PutUint16(f[o:], v) }
func putDword(f []byte, o int, v uint32) { binary.LittleEndian.PutUint32(f[o:], v) }

// ddr5 is an SMBIOS 3.3+ memory device whose size and speeds need the
// extended fields.
func ddr5() []byte {
	return structure(smbiosTypeMemoryDevice, 0x5C, func(f []byte) {
		putWord(f, 0x08, 80)
		putWord(f, 0x0A, 64)
		putWord(f, 0x0C, 0x7FFF)
		f[0x0E] = 0x09 // DIMM
		f[0x10], f[0x11] = 1, 2
		f[0x12] = 0x22 // DDR5
		putWord(f, 0x15, 0xFFFF)
		f[0x17], f[0x18], f[0x19], f[0x1A] = 3, 4, 5, 6
		f[0x1B] = 0x02
		putDword(f, 0x1C, 32768) // MiB
		putWord(f, 0x20, 0xFFFF)
		putDword(f, 0x54, 8000)
		putDword(f, 0x58, 6400)
	}, "DIMM_A1", "P0 CHANNEL A", "Samsung", "S123", "Asset", "M321R4GA3BB6-CQK  ")
}

func decodeOne(t *testing.T, s []byte) MemoryModule {
	t.Helper()
	modules, err := memoryModules(table(s))
	if err != nil {
		t.Fatalf("memoryModules: %v", err)
	}
	if len(modules) != 1 {
		t.Fatalf("got %d modules, want 1", len(modules))
	}
	return modules[0]
}

func TestMemoryDeviceSMBIOS33(t *testing.T) {
	got := decodeOne(t, ddr5())
	want := MemoryModule{
		Label: "P0 CHANNEL A", Location: "DIMM_A1", Installed: true, SizeBytes: 32 << 30,
		Type: "DDR5", FormFactor: "DIMM", SpeedMTs: 8000, ConfiguredSpeedMTs: 6400, Rank: 2,
		Vendor: "Samsung", PartNumber: "M321R4GA3BB6-CQK", SerialNumber: "S123",
		TotalWidthBits: 80, DataWidthBits: 64,
	}
	if got != want {
		t.Errorf("module =\n%+v\nwant\n%+v", got, want)
	}
}

// SMBIOS 2.3 ends after the part number: no rank, extended size or
// configured speed.
func TestMemoryDeviceSMBIOS23(t *testing.T) {
	got := decodeOne(t, structure(smbiosTypeMemoryDevice, 0x1B, func(f []byte) {
		putWord(f, 0x08, 64)
		putWord(f, 0x0A, 64)
		putWord(f, 0x0C, 8192) // MiB
		f[0x0E] = 0x0D         // SODIMM
		f[0x10], f[0x11] = 1, 2
		f[0x12] = 0x18 // DDR3
		putWord(f, 0x15, 1600)
		f[0x17], f[0x18], f[0x1A] = 3, 4, 5
	}, "ChannelA-DIMM0", "BANK 0", "Kingston", "0A1B2C3D", "KVR16S11/8"))

	want := MemoryModule{
		Label: "BANK 0", Location: "ChannelA-DIMM0", Installed: true, SizeBytes: 8 << 30,
		Type: "DDR3", FormFactor: "SODIMM", SpeedMTs: 1600,
		Vendor: "Kingston", PartNumber: "KVR16S11/8", SerialNumber: "0A1B2C3D",
		TotalWidthBits: 64, DataWidthBits: 64,
	}
	if got != want {
		t.Errorf("module =\n%+v\nwant\n%+v", got, want)
	}
}

// SMBIOS 2.1 has no speed or strings beyond the locators.
func TestMemoryDeviceSMBIOS21(t *testing.T) {
	got := decodeOne(t, structure(smbiosTypeMemoryDevice, 0x15, func(f []byte) {
		putWord(f, 0x0C, 512)
		f[0x0E] = 0x09
		f[0x10] = 1
		f[0x12] = 0x0F // SDRAM
	}, "DIMM0"))
	if got.SizeBytes != 512<<20 || got.Type != "SDRAM" || got.Location != "DIMM0" || got.Label != "unknown" ||
		got.Vendor != "unknown" || got.SpeedMTs != 0 {
		t.Errorf("module = %+v", got)
	}
}

func TestMemoryDeviceSizeEncodings(t *testing.T) {
	for _, tc := range []struct {
		name      string
		size      uint16
		ext       uint32
		installed bool
		want      int64
	}{
		{"no module installed", 0, 0, false, 0},
		{"unknown size", 0xFFFF, 0, true, 0},
		{"megabytes", 16384, 0, true, 16 << 30},
		{"kilobytes", 0x8000 | 512, 0, true, 512 << 10},
		{"extended", 0x7FFF, 1 << 20, true, 1 << 40},
		{"extended ignores its top bit", 0x7FFF, 1<<31 | 2048, true, 2 << 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeOne(t, structure(smbiosTypeMemoryDevice, 0x28, func(f []byte) {
				putWord(f, 0x0C, tc.size)
				putDword(f, 0x1C, tc.ext)
			}))
			if got.Installed != tc.installed || got.SizeBytes != tc.want {
				t.Errorf("installed %v, %d bytes; want %v, %d", got.Installed, got.SizeBytes, tc.installed, tc.want)
			}
		})
	}
}

// An empty slot reads only its locators and form factor, which describe the
// slot: the rest describes a module, and firmware often leaves stale or
// placeholder values there.
func TestMemoryDeviceEmptySlot(t *testing.T) {
	got := decodeOne(t, structure(smbiosTypeMemoryDevice, 0x28, func(f []byte) {
		putWord(f, 0x08, 72)
		putWord(f, 0x0C, 0)
		f[0x0E], f[0x12] = 0x09, 0x1A
		f[0x10], f[0x11], f[0x17] = 1, 2, 3
		putWord(f, 0x15, 3200)
	}, "DIMM_B2", "P1 CHANNEL B", "NO DIMM"))
	want := MemoryModule{
		Label: "P1 CHANNEL B", Location: "DIMM_B2", FormFactor: "DIMM",
		Type: "unknown", Vendor: "unknown", PartNumber: "unknown", SerialNumber: "unknown",
	}
	if got != want {
		t.Errorf("module =\n%+v\nwant\n%+v", got, want)
	}
}

// Unreadable values follow the "unknown" or zero convention.
func TestMemoryDeviceUnknownValues(t *testing.T) {
	got := decodeOne(t, structure(smbiosTypeMemoryDevice, 0x28, func(f []byte) {
		putWord(f, 0x08, 0xFFFF)
		putWord(f, 0x0A, 0xFFFF)
		putWord(f, 0x0C, 1024)
		f[0x0E] = 0x02 // the spec's own Unknown
		f[0x10] = 0    // no string
		f[0x11] = 9    // past the last string
		f[0x12] = 0x15 // reserved
		putWord(f, 0x15, 0xFFFF)
		f[0x17] = 1 // whitespace only
		f[0x1A] = 2
		putWord(f, 0x20, 0)
	}, "   ", "PN\x01\xff"))
	if got.Location != "unknown" || got.Label != "unknown" || got.Vendor != "unknown" {
		t.Errorf("strings = %q, %q, %q; want unknown", got.Location, got.Label, got.Vendor)
	}
	if got.FormFactor != "unknown" || got.Type != "unknown" {
		t.Errorf("enums = %q, %q; want unknown", got.FormFactor, got.Type)
	}
	// 0xFFFF defers to the extended speed, which a 0x28-byte structure lacks.
	if got.SpeedMTs != 0 || got.ConfiguredSpeedMTs != 0 || got.TotalWidthBits != 0 || got.DataWidthBits != 0 {
		t.Errorf("numbers = %+v, want zero", got)
	}
	if got.PartNumber != "PN.." {
		t.Errorf("part number = %q, want non-printable bytes as dots", got.PartNumber)
	}
	if got.Type == "" || got.FormFactor == "" {
		t.Error("an enum decoded to an empty string")
	}
	for _, code := range []byte{0x00, 0x25, 0xFF} {
		if n := enumName(memoryTypes, code); n != "unknown" {
			t.Errorf("memory type %#x = %q, want unknown", code, n)
		}
	}
}

// Other structure types are skipped, modules keep the firmware's order,
// and nothing after the end-of-table structure is read.
func TestMemoryModulesWalksTheTable(t *testing.T) {
	bios := structure(0, 0x18, nil, "Vendor", "1.0")
	slot := func(loc string) []byte {
		return structure(smbiosTypeMemoryDevice, 0x15, func(f []byte) { f[0x10] = 1 }, loc)
	}
	raw := table(bios, slot("B"), structure(16, 0x17, nil), slot("A"))
	raw = append(raw, 0xFF, 0x02) // garbage after the end of table

	modules, err := memoryModules(raw)
	if err != nil {
		t.Fatalf("memoryModules: %v", err)
	}
	if len(modules) != 2 || modules[0].Location != "B" || modules[1].Location != "A" {
		t.Errorf("modules = %+v, want B then A", modules)
	}
}

// A table without the end-of-table structure still decodes when it ends
// exactly between two structures, as dmidecode also accepts.
func TestMemoryModulesWithoutTheEndMarker(t *testing.T) {
	modules, err := memoryModules(append(ddr5(), ddr5()...))
	if err != nil || len(modules) != 2 {
		t.Errorf("modules = %d, %v; want both modules and no error", len(modules), err)
	}
}

func TestMemoryModulesWithoutMemoryDevices(t *testing.T) {
	modules, err := memoryModules(table(structure(0, 0x18, nil)))
	if err != nil || modules == nil || len(modules) != 0 {
		t.Errorf("modules = %v, %v; want an empty, non-nil list", modules, err)
	}
}

func TestMemoryModulesRejectsMalformedTables(t *testing.T) {
	good := ddr5()
	for _, tc := range []struct {
		name, want string
		raw        []byte
	}{
		{"header length below 4", "shorter than its header", []byte{17, 2, 0, 0, 0, 0}},
		{"formatted area past the end", "truncated", good[:0x30]},
		{"strings without their terminator", "strings", good[:len(good)-1]},
		{"memory device shorter than SMBIOS 2.1", "shorter than SMBIOS 2.1", table(structure(smbiosTypeMemoryDevice, 0x10, nil))},
		// A zero-byte copy must not read as a host with no memory slots.
		{"empty table", "empty", nil},
		// Cut off inside the next header: the modules after it are lost, so
		// installed_bytes would be too low without saying so.
		{"one byte of a header left", "inside the header", append(append([]byte{}, good...), 17)},
		{"three bytes of a header left", "inside the header", append(append([]byte{}, good...), 17, 0x5C, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := memoryModules(tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// Any input either decodes or returns an error; none may panic or read out
// of bounds.
func FuzzMemoryModules(f *testing.F) {
	f.Add(table(ddr5()))
	f.Add(table(structure(smbiosTypeMemoryDevice, 0x15, nil)))
	f.Add([]byte{17, 0x5C, 0, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = memoryModules(raw)
	})
}

package inventory

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// vpdResource is a large resource: tag, 16-bit little-endian length, data.
func vpdResource(tag byte, data []byte) []byte {
	return append([]byte{tag, byte(len(data)), byte(len(data) >> 8)}, data...)
}

// vpdKeywordBytes is one keyword: its name, its length and its value.
func vpdKeywordBytes(key, value string) []byte {
	return append([]byte{key[0], key[1], byte(len(value))}, value...)
}

// vpdImage builds an intact VPD image: the product name, the read-only
// keywords ro with a correct RV checksum, the read-write keywords rw (none
// when nil) followed by an RW area, and the end tag. ro and rw alternate
// names and values.
func vpdImage(product string, ro, rw []string) []byte {
	b := vpdResource(0x82, []byte(product))
	var r []byte
	for i := 0; i < len(ro); i += 2 {
		r = append(r, vpdKeywordBytes(ro[i], ro[i+1])...)
	}
	r = append(r, 'R', 'V', 4, 0, 0, 0, 0) // the checksum byte, then 3 reserved
	b = append(b, vpdResource(0x90, r)...)
	at := len(b) - 4
	var sum byte
	for _, c := range b[:at] {
		sum += c
	}
	b[at] = -sum
	if rw != nil {
		var w []byte
		for i := 0; i < len(rw); i += 2 {
			w = append(w, vpdKeywordBytes(rw[i], rw[i+1])...)
		}
		w = append(w, 'R', 'W', 8, 0, 0, 0, 0, 0, 0, 0, 0)
		b = append(b, vpdResource(0x91, w)...)
	}
	return append(b, 0x78)
}

func TestParseVPDReadsTheStandardKeywords(t *testing.T) {
	got, err := parseVPD(vpdImage("Dual-port 25GbE SFP28 adapter", []string{
		"PN", "AOC-S25G-TEST", "EC", "A01", "SN", "SN0123456789", "MN", "1028",
		"V0", "FFV16.35.2000", "V1", "DSV1028VPDR.VER2.1",
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := &PCIVPD{
		ProductName: "Dual-port 25GbE SFP28 adapter", PartNumber: "AOC-S25G-TEST", EngineeringChange: "A01",
		SerialNumber: "SN0123456789", ManufacturerID: "1028",
		VendorSpecific: map[string]string{"V0": "FFV16.35.2000", "V1": "DSV1028VPDR.VER2.1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseVPD = %+v\nwant %+v", got, want)
	}
}

func TestParseVPDReadsTheReadWriteKeywords(t *testing.T) {
	got, err := parseVPD(vpdImage("NIC", []string{"PN", "P1"}, []string{"V9", "site-tag", "YA", "asset 42"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"V9": "site-tag", "YA": "asset 42"}; !reflect.DeepEqual(got.ReadWrite, want) {
		t.Errorf("ReadWrite = %v, want %v (and no RW)", got.ReadWrite, want)
	}
}

// Review focus 4: fill bytes, control bytes and empty values.
func TestParseVPDCleansValues(t *testing.T) {
	got, err := parseVPD(vpdImage("  NIC\x00\x00", []string{"SN", "ABC\x01DEF  \xff\xff", "V2", "\x00\x00"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.ProductName != "NIC" || got.SerialNumber != "ABC.DEF" || got.VendorSpecific != nil {
		t.Errorf("parseVPD = %+v; want NIC, ABC.DEF and no vendor_specific", got)
	}
}

func TestParseVPDStopsAtPadding(t *testing.T) {
	img := append(vpdResource(0x90, append(vpdKeywordBytes("PN", "P1"), 0, 0, 0, 0)), 0x78)
	if got, err := parseVPD(img); err != nil || got.PartNumber != "P1" {
		t.Errorf("parseVPD = %+v, %v; want P1", got, err)
	}
}

// Padding must run to the end of its resource: a keyword after it would be
// dropped, along with the RV check, in a partial report.
func TestParseVPDRejectsDataAfterPadding(t *testing.T) {
	for name, tag := range map[string]byte{"VPD-R": 0x90, "VPD-W": 0x91} {
		data := append(vpdKeywordBytes("PN", "P1"), 0)
		data = append(data, vpdKeywordBytes("SN", "S1")...)
		data = append(data, 'R', 'V', 1, 0)
		img := append(vpdResource(tag, data), 0x78)
		if v, err := parseVPD(img); err == nil || !strings.Contains(err.Error(), "padding") {
			t.Errorf("%s: parsed as %+v, %v; want a padding error", name, v, err)
		}
	}
}

// lspci accepts a VPD-R without RV; so does the agent, which then has no
// checksum to check.
func TestParseVPDAcceptsAMissingChecksum(t *testing.T) {
	img := append(append(vpdResource(0x82, []byte("NIC")), vpdResource(0x90, vpdKeywordBytes("PN", "P1"))...), 0x78)
	if got, err := parseVPD(img); err != nil || got.PartNumber != "P1" {
		t.Errorf("parseVPD = %+v, %v; want P1", got, err)
	}
}

func TestParseVPDRejectsABadChecksum(t *testing.T) {
	img := vpdImage("NIC", []string{"SN", "SN0123456789"}, nil)
	img[bytes.Index(img, []byte("SN0123"))+3] ^= 1
	if _, err := parseVPD(img); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("parseVPD = %v, want a checksum error", err)
	}
}

// A VPD cut off anywhere is an error, never a partial report.
func TestParseVPDRejectsEveryTruncation(t *testing.T) {
	img := vpdImage("NIC", []string{"PN", "P1", "SN", "S1"}, []string{"V9", "x"})
	for n := range len(img) {
		if v, err := parseVPD(img[:n]); err == nil {
			t.Errorf("the first %d of %d bytes parsed as %+v", n, len(img), v)
		}
	}
}

func TestParseVPDRejectsMalformedKeywords(t *testing.T) {
	for name, img := range map[string][]byte{
		"value past its resource": append(vpdResource(0x90, []byte{'P', 'N', 10, 'x'}), 0x78),
		"invalid keyword name":    append(vpdResource(0x90, []byte{'p', 'n', 1, 'x'}), 0x78),
		"unknown small resource":  {0x10, 0x78},
	} {
		if v, err := parseVPD(img); err == nil {
			t.Errorf("%s: parsed as %+v, want an error", name, v)
		}
	}
}

func FuzzParseVPD(f *testing.F) {
	f.Add(vpdImage("NIC", []string{"PN", "P1", "SN", "S1"}, []string{"V9", "x"}))
	f.Add([]byte{0x78})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = parseVPD(b) // must not panic
	})
}

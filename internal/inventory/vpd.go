package inventory

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// PCIVPD is a card's Vital Product Data, from the card's own EEPROM: the
// orderable product and the physical card, which the PCI IDs of the chip it
// is built on cannot name. Every field is absent when the card does not
// set it.
type PCIVPD struct {
	// ProductName is the identifier string, such as "Intel(R) Ethernet
	// Network Adapter E810-XXV-2".
	ProductName       string `json:"product_name,omitempty"`
	PartNumber        string `json:"part_number,omitempty"`        // PN
	SerialNumber      string `json:"serial_number,omitempty"`      // SN
	EngineeringChange string `json:"engineering_change,omitempty"` // EC
	ManufacturerID    string `json:"manufacturer_id,omitempty"`    // MN
	// VendorSpecific holds every other read-only keyword, by name: the
	// V0-VZ vendor fields, such as firmware versions or port MAC
	// addresses, and any other the card sets.
	VendorSpecific map[string]string `json:"vendor_specific,omitempty"`
	// ReadWrite holds the read-write keywords, by name, without the unused
	// RW area.
	ReadWrite map[string]string `json:"read_write,omitempty"`
}

// The VPD resources (PCI Local Bus 3.0, section 6.4).
const (
	vpdTagIdentifier = 0x82
	vpdTagReadOnly   = 0x90
	vpdTagReadWrite  = 0x91
	// vpdSmallEnd is the end tag's small resource name: 0x78 is name 0xF,
	// length 0.
	vpdSmallEnd = 0x0f
)

// vpdKeyword matches a keyword's two-character name.
var vpdKeyword = regexp.MustCompile(`^[A-Z0-9]{2}$`)

// parseVPD decodes a VPD image as a device's vpd file gives it: large
// resources (a tag, a 16-bit length and the data) for the identifier
// string, the read-only keywords (VPD-R) and the read-write keywords
// (VPD-W), until the small end tag. It fails on an empty image, one that
// ends before its end tag, a keyword that runs past its resource, and a
// read-only checksum (RV) that does not sum the image to zero: a VPD that
// is not intact is not reported in part. A VPD-R without RV is accepted,
// as lspci does, so it has no checksum to check.
func parseVPD(b []byte) (*PCIVPD, error) {
	if len(b) == 0 {
		return nil, errors.New("the VPD is empty")
	}
	v := &PCIVPD{}
	for off := 0; off < len(b); {
		tag := b[off]
		if tag&0x80 == 0 {
			if tag>>3 == vpdSmallEnd {
				return v, nil
			}
			return nil, fmt.Errorf("unexpected small resource %#x at offset %#x", tag, off)
		}
		if off+3 > len(b) {
			return nil, fmt.Errorf("the VPD ends inside the header of the resource at offset %#x", off)
		}
		start := off + 3
		end := start + int(binary.LittleEndian.Uint16(b[off+1:]))
		if end > len(b) {
			return nil, fmt.Errorf("resource %#x at offset %#x is truncated", tag, off)
		}
		var err error
		switch tag {
		case vpdTagIdentifier:
			v.ProductName = vpdString(b[start:end])
		case vpdTagReadOnly:
			err = v.readOnly(b, start, end)
		case vpdTagReadWrite:
			err = v.readWrite(b, start, end)
		}
		// Any other large resource is skipped: the specification defines
		// none, and its length still lets the walk go on.
		if err != nil {
			return nil, err
		}
		off = end
	}
	return nil, errors.New("the VPD has no end tag")
}

// readOnly reads the VPD-R keywords in b[start:end]. RV ends them: its
// first byte makes every byte of the image up to and including it sum to
// zero, and the rest of it is reserved.
func (v *PCIVPD) readOnly(b []byte, start, end int) error {
	for i := start; i < end; {
		key, vs, ve, err := vpdNextKeyword(b, i, end)
		if err != nil || key == "" {
			return err
		}
		if key == "RV" {
			if ve == vs {
				return errors.New("the RV checksum keyword is empty")
			}
			var sum byte
			for _, c := range b[:vs+1] {
				sum += c
			}
			if sum != 0 {
				return fmt.Errorf("bad checksum: the read-only data sums to %#x, not 0", sum)
			}
			return nil
		}
		value := vpdString(b[vs:ve])
		switch key {
		case "PN":
			v.PartNumber = value
		case "EC":
			v.EngineeringChange = value
		case "SN":
			v.SerialNumber = value
		case "MN":
			v.ManufacturerID = value
		default:
			v.VendorSpecific = vpdPut(v.VendorSpecific, key, value)
		}
		i = ve
	}
	return nil
}

// readWrite reads the VPD-W keywords in b[start:end], up to RW, the unused
// rest of the writable area.
func (v *PCIVPD) readWrite(b []byte, start, end int) error {
	for i := start; i < end; {
		key, vs, ve, err := vpdNextKeyword(b, i, end)
		if err != nil || key == "" || key == "RW" {
			return err
		}
		v.ReadWrite = vpdPut(v.ReadWrite, key, vpdString(b[vs:ve]))
		i = ve
	}
	return nil
}

// vpdNextKeyword reads the keyword at b[i], within a resource ending at
// end: a two-character name, a length byte, and the value at b[vs:ve]. An
// empty key means padding, which must run (as 0x00 or 0xFF fill) to the end
// of the resource.
func vpdNextKeyword(b []byte, i, end int) (key string, vs, ve int, err error) {
	if b[i] == 0 {
		for j := i; j < end; j++ {
			if b[j] != 0 && b[j] != 0xff {
				return "", 0, 0, fmt.Errorf("data after the padding at offset %#x", i)
			}
		}
		return "", 0, 0, nil
	}
	if i+3 > end {
		return "", 0, 0, fmt.Errorf("the keyword at offset %#x is truncated", i)
	}
	key = string(b[i : i+2])
	if !vpdKeyword.MatchString(key) {
		return "", 0, 0, fmt.Errorf("invalid keyword %q at offset %#x", key, i)
	}
	vs, ve = i+3, i+3+int(b[i+2])
	if ve > end {
		return "", 0, 0, fmt.Errorf("keyword %s at offset %#x runs past its resource", key, i)
	}
	return key, vs, ve, nil
}

// vpdString is a value as text: trailing NUL and 0xFF fill dropped, bytes
// outside printable ASCII replaced by '.', as for SMBIOS strings, and
// surrounding spaces trimmed.
func vpdString(b []byte) string {
	return strings.TrimSpace(printable(bytes.TrimRight(b, "\x00\xff")))
}

// vpdPut sets m[key] to value unless value is empty, creating m as needed.
func vpdPut(m map[string]string, key, value string) map[string]string {
	if value == "" {
		return m
	}
	if m == nil {
		m = map[string]string{}
	}
	m[key] = value
	return m
}

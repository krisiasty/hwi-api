package inventory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// dmiDir is the sysfs directory the kernel publishes the SMBIOS strings in.
const dmiDir = "sys/class/dmi/id"

// unknown is what a string field reads as when its source exists but cannot
// be read: the field is there, but its value is not known.
const unknown = "unknown"

// RootOnlyDMIFiles lists the /sys/class/dmi/id files the kernel makes
// root-only. The agent runs with no capabilities, so the privileged helper
// (internal/helper) reads them for it; see the privileged argument of the
// probers below.
var RootOnlyDMIFiles = []string{"product_uuid", "product_serial", "board_serial", "chassis_serial"}

// Product is the product payload: the system's identity as the firmware
// reports it. Identifiers assesses SerialNumber and UUID.
type Product struct {
	Family       string                `json:"family,omitempty"`
	Name         string                `json:"name,omitempty"`
	Vendor       string                `json:"vendor,omitempty"`
	SerialNumber string                `json:"serial_number,omitempty"`
	UUID         string                `json:"uuid,omitempty"`
	SKU          string                `json:"sku,omitempty"`
	Version      string                `json:"version,omitempty"`
	Identifiers  map[string]Identifier `json:"identifiers,omitempty"`
}

// Baseboard is the baseboard payload. Identifiers assesses SerialNumber.
type Baseboard struct {
	AssetTag     string                `json:"asset_tag,omitempty"`
	SerialNumber string                `json:"serial_number,omitempty"`
	Vendor       string                `json:"vendor,omitempty"`
	Version      string                `json:"version,omitempty"`
	Product      string                `json:"product,omitempty"`
	Identifiers  map[string]Identifier `json:"identifiers,omitempty"`
}

// Chassis is the chassis payload. Type is the SMBIOS code in decimal and
// TypeDescription its name. Identifiers assesses SerialNumber.
type Chassis struct {
	AssetTag        string                `json:"asset_tag,omitempty"`
	SerialNumber    string                `json:"serial_number,omitempty"`
	Type            string                `json:"type,omitempty"`
	TypeDescription string                `json:"type_description,omitempty"`
	Vendor          string                `json:"vendor,omitempty"`
	Version         string                `json:"version,omitempty"`
	Identifiers     map[string]Identifier `json:"identifiers,omitempty"`
}

// BIOS is the bios payload.
type BIOS struct {
	Vendor  string `json:"vendor,omitempty"`
	Version string `json:"version,omitempty"`
	Date    string `json:"date,omitempty"`
}

// chassisTypeDescriptions names the SMBIOS chassis types 1 to 36.
var chassisTypeDescriptions = map[string]string{
	"1": "Other", "2": "Unknown", "3": "Desktop", "4": "Low profile desktop", "5": "Pizza box", "6": "Mini tower",
	"7": "Tower", "8": "Portable", "9": "Laptop", "10": "Notebook", "11": "Hand held", "12": "Docking station",
	"13": "All in one", "14": "Sub notebook", "15": "Space-saving", "16": "Lunch box", "17": "Main server chassis",
	"18": "Expansion chassis", "19": "SubChassis", "20": "Bus Expansion chassis", "21": "Peripheral chassis",
	"22": "RAID chassis", "23": "Rack mount chassis", "24": "Sealed-case PC", "25": "Multi-system chassis",
	"26": "Compact PCI", "27": "Advanced TCA", "28": "Blade", "29": "Blade enclosure", "30": "Tablet",
	"31": "Convertible", "32": "Detachable", "33": "IoT gateway", "34": "Embedded PC", "35": "Mini PC", "36": "Stick PC",
}

// dmiReader reads DMI files, collecting the first error that must fail the
// probe so the callers stay a flat list of fields.
type dmiReader struct {
	root       fs.FS
	privileged fs.FS
	err        error
}

// read reads a world-readable file. A file the host lacks is absent, which
// leaves the field out; any other failure makes the field "unknown".
func (r *dmiReader) read(name string) (value string, present bool) {
	b, err := fs.ReadFile(r.root, dmiDir+"/"+name)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	if err != nil {
		return unknown, true
	}
	return strings.TrimSpace(string(b)), true
}

// rootOnly reads a root-only file through the helper, or, without one,
// from the host root like any other file: an agent run as root reads it
// there, and one without the capability gets "unknown". A helper that
// does not answer, or cannot read the file, fails the probe, so the store
// keeps the last good payload rather than replacing a known serial number
// with "unknown". Once one read has failed the probe, the rest are not
// asked for, so a hung helper costs one timeout per probe. A file the host
// lacks reads as "", which leaves the field out.
func (r *dmiReader) rootOnly(name string) string {
	if r.privileged == nil {
		value, _ := r.read(name)
		return value
	}
	if r.err != nil {
		return ""
	}
	b, err := fs.ReadFile(r.privileged, name)
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		r.err = fmt.Errorf("reading the DMI field %s through the helper: %w", name, err)
		return ""
	}
	return strings.TrimSpace(string(b))
}

// identify assesses the named fields that carry a value. An empty field is
// left out of the payload, so it has nothing to assess either.
func identify(fields map[string]string) map[string]Identifier {
	var ids map[string]Identifier
	for name, value := range fields {
		if value == "" {
			continue
		}
		if ids == nil {
			ids = map[string]Identifier{}
		}
		ids[name] = NormaliseIdentifier(value)
	}
	return ids
}

// NewProductProber reads the product from rootFS, a host root, and its
// root-only serial number and UUID through privileged, the helper's file
// view, or from rootFS too when privileged is nil (no helper configured).
func NewProductProber(rootFS, privileged fs.FS) Prober {
	return ProberFunc(func(context.Context) (any, error) {
		r := &dmiReader{root: rootFS, privileged: privileged}
		var p Product
		p.Family, _ = r.read("product_family")
		p.Name, _ = r.read("product_name")
		p.Vendor, _ = r.read("sys_vendor")
		serial := r.rootOnly("product_serial")
		uuid := r.rootOnly("product_uuid")
		p.SKU, _ = r.read("product_sku")
		p.Version, _ = r.read("product_version")
		if r.err != nil {
			return nil, r.err
		}
		p.SerialNumber, p.UUID = serial, uuid
		p.Identifiers = identify(map[string]string{"serial_number": serial, "uuid": uuid})
		return p, nil
	})
}

// NewBaseboardProber reads the baseboard as NewProductProber reads the
// product.
func NewBaseboardProber(rootFS, privileged fs.FS) Prober {
	return ProberFunc(func(context.Context) (any, error) {
		r := &dmiReader{root: rootFS, privileged: privileged}
		var b Baseboard
		b.AssetTag, _ = r.read("board_asset_tag")
		serial := r.rootOnly("board_serial")
		b.Vendor, _ = r.read("board_vendor")
		b.Version, _ = r.read("board_version")
		b.Product, _ = r.read("board_name")
		if r.err != nil {
			return nil, r.err
		}
		b.SerialNumber = serial
		b.Identifiers = identify(map[string]string{"serial_number": serial})
		return b, nil
	})
}

// NewChassisProber reads the chassis as NewProductProber reads the product.
func NewChassisProber(rootFS, privileged fs.FS) Prober {
	return ProberFunc(func(context.Context) (any, error) {
		r := &dmiReader{root: rootFS, privileged: privileged}
		var c Chassis
		c.AssetTag, _ = r.read("chassis_asset_tag")
		serial := r.rootOnly("chassis_serial")
		typ, hasType := r.read("chassis_type")
		c.Vendor, _ = r.read("chassis_vendor")
		c.Version, _ = r.read("chassis_version")
		if r.err != nil {
			return nil, r.err
		}
		c.SerialNumber = serial
		if hasType && typ != "" {
			c.Type = typ
			c.TypeDescription = unknown
			if d, ok := chassisTypeDescriptions[typ]; ok {
				c.TypeDescription = d
			}
		}
		c.Identifiers = identify(map[string]string{"serial_number": serial})
		return c, nil
	})
}

// NewBIOSProber reads the BIOS from rootFS. It has no root-only field, so it
// needs no helper.
func NewBIOSProber(rootFS fs.FS) Prober {
	return ProberFunc(func(context.Context) (any, error) {
		r := &dmiReader{root: rootFS}
		var b BIOS
		b.Vendor, _ = r.read("bios_vendor")
		b.Version, _ = r.read("bios_version")
		b.Date, _ = r.read("bios_date")
		return b, nil
	})
}

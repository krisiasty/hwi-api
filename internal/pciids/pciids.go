// Package pciids names PCI vendors, devices, subsystems and classes from
// the PCI ID database (pci.ids) bundled into the binary, so that names do
// not depend on the copy, and its age, on each host.
//
// pci.ids.gz is the PCI ID Project's pci.ids (https://pci-ids.ucw.cz/),
// compressed with gzip -9 -n so that it changes only when its contents do.
// A scheduled workflow (.github/workflows/update-pci-ids.yml) proposes a
// fresh copy every week. The database is dual-licensed under the GNU GPL
// version 2 or later and the 3-clause BSD license; this project
// distributes it under the latter, whose notice is in
// THIRD_PARTY_NOTICES.md.
package pciids

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

//go:embed pci.ids.gz
var bundled []byte

// DB is a parsed PCI ID database. A nil *DB names nothing.
type DB struct {
	// Version is the database's version, such as "2026.10.01", from the
	// "Version:" line of its header.
	Version string

	vendors map[string]*vendor
	classes map[string]*class
}

type vendor struct {
	name     string
	products map[string]*product
}

type product struct {
	name string
	// subsystems is keyed by subvendor and subdevice ID, concatenated:
	// one product can list the same subdevice ID under several vendors.
	subsystems map[string]string
}

type class struct {
	name       string
	subclasses map[string]*subclass
}

type subclass struct {
	name    string
	progIfs map[string]string
}

// Bundled returns the bundled database, parsed once.
var Bundled = sync.OnceValues(func() (*DB, error) {
	text, err := BundledText()
	if err != nil {
		return nil, err
	}
	return Parse(bytes.NewReader(text))
})

// BundledText returns the bundled pci.ids itself, for tools such as lspci
// that read the file.
func BundledText() ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(bundled))
	if err != nil {
		return nil, fmt.Errorf("the bundled pci.ids: %w", err)
	}
	text, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("the bundled pci.ids: %w", err)
	}
	return text, nil
}

// Parse reads a pci.ids database. It reports a malformed line, naming it,
// rather than panicking or skipping it; it reports a read error rather
// than taking it for the end of the file; and it refuses a database without
// vendors or classes, as a copy cut short before its class list is.
func Parse(r io.Reader) (*DB, error) {
	db := &DB{vendors: map[string]*vendor{}, classes: map[string]*class{}}
	var (
		v   *vendor
		p   *product
		c   *class
		sc  *subclass
		inC bool
	)
	sc0 := bufio.NewScanner(r)
	for n := 1; sc0.Scan(); n++ {
		line := strings.TrimRight(sc0.Text(), " \r")
		if line == "" {
			continue
		}
		if line[0] == '#' {
			if version, ok := strings.CutPrefix(strings.TrimLeft(line[1:], " \t"), "Version:"); ok && db.Version == "" {
				db.Version = strings.TrimSpace(version)
			}
			continue
		}
		bad := func(format string, args ...any) error {
			return fmt.Errorf("pci.ids line %d: %s: %q", n, fmt.Sprintf(format, args...), line)
		}

		tabs := len(line) - len(strings.TrimLeft(line, "\t"))
		rest := line[tabs:]
		switch {
		case tabs == 0 && strings.HasPrefix(rest, "C "):
			id, name, err := record(rest[2:], 2)
			if err != nil {
				return nil, bad("class: %v", err)
			}
			c = &class{name: name, subclasses: map[string]*subclass{}}
			db.classes[id] = c
			sc, inC = nil, true
		case tabs == 0:
			id, name, err := record(rest, 4)
			if err != nil {
				return nil, bad("vendor: %v", err)
			}
			v = &vendor{name: name, products: map[string]*product{}}
			db.vendors[id] = v
			p, inC = nil, false
		case tabs == 1 && inC:
			id, name, err := record(rest, 2)
			if err != nil {
				return nil, bad("subclass: %v", err)
			}
			sc = &subclass{name: name, progIfs: map[string]string{}}
			c.subclasses[id] = sc
		case tabs == 1:
			if v == nil {
				return nil, bad("device before any vendor")
			}
			id, name, err := record(rest, 4)
			if err != nil {
				return nil, bad("device: %v", err)
			}
			p = &product{name: name, subsystems: map[string]string{}}
			v.products[id] = p
		case tabs == 2 && inC:
			if sc == nil {
				return nil, bad("programming interface before any subclass")
			}
			id, name, err := record(rest, 2)
			if err != nil {
				return nil, bad("programming interface: %v", err)
			}
			sc.progIfs[id] = name
		case tabs == 2:
			if p == nil {
				return nil, bad("subsystem before any device")
			}
			sub, name, err := record(rest, 9)
			if err != nil || sub[4] != ' ' || !isHex(sub[5:]) {
				return nil, bad("subsystem: want subvendor and subdevice IDs and a name")
			}
			p.subsystems[sub[:4]+sub[5:]] = name
		default:
			return nil, bad("%d tabs", tabs)
		}
	}
	if err := sc0.Err(); err != nil {
		return nil, fmt.Errorf("reading pci.ids: %w", err)
	}
	if len(db.vendors) == 0 {
		return nil, errors.New("pci.ids lists no vendors")
	}
	if len(db.classes) == 0 {
		return nil, errors.New("pci.ids lists no device classes; it is probably cut short")
	}
	return db, nil
}

// record splits "id  name", where id is width characters, the first of
// them hex digits.
func record(s string, width int) (id, name string, err error) {
	if len(s) < width+2 || s[width:width+2] != "  " {
		return "", "", fmt.Errorf("want %d characters of ID, two spaces and a name", width)
	}
	id = strings.ToLower(s[:width])
	if !isHex(id[:min(width, 4)]) {
		return "", "", errors.New("ID is not hexadecimal")
	}
	return id, s[width+2:], nil
}

func isHex(s string) bool {
	for _, r := range s {
		hex := r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
		if !hex {
			return false
		}
	}
	return s != ""
}

// Vendor names a vendor ID, in lowercase hex.
func (db *DB) Vendor(v string) (string, bool) {
	if vd := db.vendor(v); vd != nil {
		return vd.name, true
	}
	return "", false
}

func (db *DB) vendor(v string) *vendor {
	if db == nil {
		return nil
	}
	return db.vendors[v]
}

func (db *DB) product(v, p string) *product {
	if vd := db.vendor(v); vd != nil {
		return vd.products[p]
	}
	return nil
}

// Product names a vendor's device ID.
func (db *DB) Product(v, p string) (string, bool) {
	if pd := db.product(v, p); pd != nil {
		return pd.name, true
	}
	return "", false
}

// Subsystem names the board a device is built into, by its subsystem
// vendor and subsystem device IDs.
func (db *DB) Subsystem(v, p, subVendor, subDevice string) (string, bool) {
	if pd := db.product(v, p); pd != nil {
		name, ok := pd.subsystems[subVendor+subDevice]
		return name, ok
	}
	return "", false
}

func (db *DB) class(c string) *class {
	if db == nil {
		return nil
	}
	return db.classes[c]
}

func (db *DB) subclass(c, s string) *subclass {
	if cl := db.class(c); cl != nil {
		return cl.subclasses[s]
	}
	return nil
}

// Class names a class ID.
func (db *DB) Class(c string) (string, bool) {
	if cl := db.class(c); cl != nil {
		return cl.name, true
	}
	return "", false
}

// Subclass names a class's subclass ID.
func (db *DB) Subclass(c, s string) (string, bool) {
	if sc := db.subclass(c, s); sc != nil {
		return sc.name, true
	}
	return "", false
}

// ProgIf names a subclass's programming interface ID.
func (db *DB) ProgIf(c, s, pi string) (string, bool) {
	if sc := db.subclass(c, s); sc != nil {
		name, ok := sc.progIfs[pi]
		return name, ok
	}
	return "", false
}

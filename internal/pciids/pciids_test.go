package pciids

import (
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
)

// testIDs is a pci.ids in the real format. The ConnectX-6 Dx lists one
// subsystem ID under two vendors, the other first. The file ends inside a
// vendor's and a class's last blocks, which must still be read.
const testIDs = "#\n#\tList of PCI IDs\n#\n#\tVersion: 2026.10.01\n#\tDate:    2026-10-01 03:15:02\n#\n\n" +
	"# Vendors, devices and subsystems.\n" +
	"15b3  Mellanox Technologies\n" +
	"\t101d  MT2892 Family [ConnectX-6 Dx]\n" +
	"\t\t1028 0016  Dell-branded ConnectX-6 Dx\n" +
	"\t\t15b3 0016  ConnectX-6 Dx EN adapter card\n" +
	"# A comment between blocks.\n" +
	"\t101e  ConnectX Family mlx5Gen Virtual Function\n" +
	"8086  Intel Corporation\n" +
	"\t15bb  Ethernet Connection (10) I219-LM\n" +
	"\t\t1028 0a20  Ethernet Connection (10) I219-LM\n" +
	"\n# List of known device classes, subclasses and programming interfaces\n\n" +
	"C 02  Network controller\n" +
	"\t00  Ethernet controller\n" +
	"\t80  Network controller\n" +
	"C 0c  Serial bus controller\n" +
	"\t03  USB controller\n" +
	"\t\t00  UHCI\n" +
	"\t\t30  XHCI\n"

func parse(t *testing.T, text string) *DB {
	t.Helper()
	db, err := Parse(strings.NewReader(text))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return db
}

func TestParseReadsEveryRecord(t *testing.T) {
	db := parse(t, testIDs)

	if db.Version != "2026.10.01" {
		t.Errorf("Version = %q", db.Version)
	}
	for _, c := range []struct {
		what string
		got  func() (string, bool)
		want string
	}{
		{"vendor", func() (string, bool) { return db.Vendor("15b3") }, "Mellanox Technologies"},
		{"product", func() (string, bool) { return db.Product("15b3", "101d") }, "MT2892 Family [ConnectX-6 Dx]"},
		{"product after a comment", func() (string, bool) { return db.Product("15b3", "101e") }, "ConnectX Family mlx5Gen Virtual Function"},
		{"subsystem", func() (string, bool) { return db.Subsystem("15b3", "101d", "15b3", "0016") }, "ConnectX-6 Dx EN adapter card"},
		{"subsystem of another vendor", func() (string, bool) { return db.Subsystem("15b3", "101d", "1028", "0016") }, "Dell-branded ConnectX-6 Dx"},
		{"last vendor's subsystem", func() (string, bool) { return db.Subsystem("8086", "15bb", "1028", "0a20") }, "Ethernet Connection (10) I219-LM"},
		{"class", func() (string, bool) { return db.Class("02") }, "Network controller"},
		{"subclass", func() (string, bool) { return db.Subclass("02", "00") }, "Ethernet controller"},
		{"last class's subclass", func() (string, bool) { return db.Subclass("0c", "03") }, "USB controller"},
		{"last programming interface", func() (string, bool) { return db.ProgIf("0c", "03", "30") }, "XHCI"},
	} {
		if got, ok := c.got(); !ok || got != c.want {
			t.Errorf("%s = %q, %v; want %q", c.what, got, ok, c.want)
		}
	}
}

func TestLookupsOfUnlistedIDs(t *testing.T) {
	db := parse(t, testIDs)
	for what, ok := range map[string]bool{
		"vendor":            pick(db.Vendor("ffff")),
		"product":           pick(db.Product("15b3", "ffff")),
		"subsystem":         pick(db.Subsystem("15b3", "101d", "8086", "0016")),
		"class":             pick(db.Class("ff")),
		"subclass":          pick(db.Subclass("02", "ff")),
		"prog-if":           pick(db.ProgIf("02", "00", "00")),
		"unread vendor ID":  pick(db.Vendor("unknown")),
		"nil database":      pick((*DB)(nil).Vendor("15b3")),
		"nil database prog": pick((*DB)(nil).ProgIf("0c", "03", "30")),
	} {
		if ok {
			t.Errorf("%s found", what)
		}
	}
}

func pick(_ string, ok bool) bool { return ok }

// A malformed line is an error naming the line, never a panic or a
// silently skipped record.
func TestParseReportsMalformedLines(t *testing.T) {
	for name, c := range map[string]struct {
		text string
		line string
	}{
		"short vendor line":         {"15b3\n", "line 1"},
		"vendor ID not hex":         {"zzzz  Bad\n", "line 1"},
		"device before any vendor":  {"\t101d  Orphan\n", "line 1"},
		"subsystem before a device": {"15b3  Mellanox\n\t\t15b3 0016  Orphan\n", "line 2"},
		"short subsystem line":      {"15b3  Mellanox\n\t101d  X\n\t\t15b3\n", "line 3"},
		"short class line":          {"15b3  M\nC 0\n", "line 2"},
		"subclass ID not hex":       {"15b3  M\nC 02  Network\n\tzz  Bad\n", "line 3"},
		"three tabs":                {"15b3  M\n\t101d  X\n\t\t\tdeep\n", "line 3"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(c.text))
			if err == nil || !strings.Contains(err.Error(), c.line) {
				t.Errorf("Parse = %v, want an error naming %s", err, c.line)
			}
		})
	}
}

// A file cut short before its class list, as a truncated copy is, is
// refused rather than read as a database without classes.
func TestParseRefusesADatabaseWithoutClasses(t *testing.T) {
	text, _, _ := strings.Cut(testIDs, "C 02")
	if _, err := Parse(strings.NewReader(text)); err == nil {
		t.Error("Parse accepted a database with no classes")
	}
	if _, err := Parse(strings.NewReader("# only comments\n")); err == nil {
		t.Error("Parse accepted a database with no vendors")
	}
}

// A read error is reported, not taken for the end of the file.
func TestParseReportsReadErrors(t *testing.T) {
	boom := errors.New("disk on fire")
	r := io.MultiReader(strings.NewReader(testIDs), &failingReader{err: boom})
	if _, err := Parse(r); !errors.Is(err, boom) {
		t.Errorf("Parse = %v, want the read error", err)
	}
}

type failingReader struct{ err error }

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }

// The bundled database is a complete, current pci.ids.
func TestBundled(t *testing.T) {
	db, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled: %v", err)
	}
	if !regexp.MustCompile(`^\d{4}\.\d{2}\.\d{2}$`).MatchString(db.Version) {
		t.Errorf("Version = %q", db.Version)
	}
	if n := len(db.vendors); n < 2000 {
		t.Errorf("%d vendors; a complete database has over 2000", n)
	}
	if n := len(db.classes); n < 20 {
		t.Errorf("%d classes; a complete database has over 20", n)
	}
	for _, c := range []struct {
		what string
		got  func() (string, bool)
		want string
	}{
		{"NVIDIA", func() (string, bool) { return db.Vendor("10de") }, "NVIDIA Corporation"},
		{"L40S", func() (string, bool) { return db.Product("10de", "26b9") }, "AD102GL [L40S]"},
		{"display class", func() (string, bool) { return db.Class("03") }, "Display controller"},
		{"3D controller", func() (string, bool) { return db.Subclass("03", "02") }, "3D controller"},
	} {
		if got, ok := c.got(); !ok || got != c.want {
			t.Errorf("%s = %q, %v; want %q", c.what, got, ok, c.want)
		}
	}
	again, _ := Bundled()
	if again != db {
		t.Error("Bundled parsed the database again")
	}
}

func TestBundledTextIsTheBundledDatabase(t *testing.T) {
	text, err := BundledText()
	if err != nil {
		t.Fatal(err)
	}
	db := parse(t, string(text))
	bundled, _ := Bundled()
	if db.Version != bundled.Version || len(db.vendors) != len(bundled.vendors) {
		t.Errorf("BundledText is version %s with %d vendors; Bundled is %s with %d", db.Version, len(db.vendors), bundled.Version, len(bundled.vendors))
	}
}

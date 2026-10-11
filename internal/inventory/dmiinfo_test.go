package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/krisiasty/hwi-api/internal/helper"
)

const dmiTestDir = "sys/class/dmi/id"

// dmiHost builds a host root holding the given /sys/class/dmi/id files.
func dmiHost(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for name, data := range files {
		m[dmiTestDir+"/"+name] = &fstest.MapFile{Data: []byte(data)}
	}
	return m
}

func privFS(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for name, data := range files {
		m[name] = &fstest.MapFile{Data: []byte(data)}
	}
	return m
}

func fullDMIHost() fstest.MapFS {
	return dmiHost(map[string]string{
		"product_family": " Family X \n", "product_name": "PowerEdge R760\n", "sys_vendor": "Dell Inc.\n",
		"product_sku": "SKU-1\n", "product_version": "1.2\n",
		"board_asset_tag": "BAT\n", "board_vendor": "Dell Inc.\n", "board_version": "A01\n", "board_name": "0ABC\n",
		"chassis_asset_tag": "CAT\n", "chassis_type": "23\n", "chassis_vendor": "Dell Inc.\n", "chassis_version": "C1\n",
		"bios_vendor": "Dell Inc.\n", "bios_version": "2.1.0\n", "bios_date": "01/02/2024\n",
	})
}

func fullPrivileged() fs.FS {
	return privFS(map[string]string{
		"product_serial": "  SVC123 \n", "product_uuid": "4c4c4544-0001-0002-0003-000000000004\n",
		"board_serial": "BSN1\n", "chassis_serial": "CSN1\n",
	})
}

func probeAs[T any](t *testing.T, p Prober) T {
	t.Helper()
	v, err := p.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	got, ok := v.(T)
	if !ok {
		t.Fatalf("Probe returned %T", v)
	}
	return got
}

func unavailableFS() fs.FS {
	return helper.NewFS(func(helper.Request) (helper.Response, error) {
		return helper.Response{}, fmt.Errorf("%w: no socket", helper.ErrUnavailable)
	})
}

func TestProductProberReadsFullHost(t *testing.T) {
	p := probeAs[Product](t, NewProductProber(fullDMIHost(), fullPrivileged()))
	want := Product{
		Family: "Family X", Name: "PowerEdge R760", Vendor: "Dell Inc.", SerialNumber: "SVC123",
		UUID: "4c4c4544-0001-0002-0003-000000000004", SKU: "SKU-1", Version: "1.2",
	}
	id := p.Identifiers
	p.Identifiers = nil
	if !reflect.DeepEqual(p, want) {
		t.Errorf("Product = %+v, want %+v", p, want)
	}
	if len(id) != 2 || !id["serial_number"].Usable || id["serial_number"].Value != "SVC123" || !id["uuid"].Usable {
		t.Errorf("Identifiers = %+v", id)
	}
}

func TestProductJSONFieldOrder(t *testing.T) {
	b, err := json.Marshal(probeAs[Product](t, NewProductProber(fullDMIHost(), fullPrivileged())))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	last := -1
	for _, k := range []string{"family", "name", "vendor", "serial_number", "uuid", "sku", "version", "identifiers"} {
		i := strings.Index(s, `"`+k+`":`)
		if i < 0 || i < last {
			t.Fatalf("field %q missing or out of order in %s", k, s)
		}
		last = i
	}
	var m struct {
		Identifiers map[string]json.RawMessage `json:"identifiers"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Identifiers) != 2 || m.Identifiers["serial_number"] == nil || m.Identifiers["uuid"] == nil {
		t.Errorf("identifiers = %v", m.Identifiers)
	}
}

func TestBaseboardAndChassisProbersReadFullHost(t *testing.T) {
	b := probeAs[Baseboard](t, NewBaseboardProber(fullDMIHost(), fullPrivileged()))
	if b.AssetTag != "BAT" || b.SerialNumber != "BSN1" || b.Vendor != "Dell Inc." || b.Version != "A01" || b.Product != "0ABC" {
		t.Errorf("Baseboard = %+v", b)
	}
	if len(b.Identifiers) != 1 || b.Identifiers["serial_number"].Value != "BSN1" {
		t.Errorf("Baseboard identifiers = %+v", b.Identifiers)
	}
	c := probeAs[Chassis](t, NewChassisProber(fullDMIHost(), fullPrivileged()))
	if c.AssetTag != "CAT" || c.SerialNumber != "CSN1" || c.Type != "23" || c.TypeDescription != "Rack mount chassis" ||
		c.Vendor != "Dell Inc." || c.Version != "C1" {
		t.Errorf("Chassis = %+v", c)
	}
	if len(c.Identifiers) != 1 || c.Identifiers["serial_number"].Value != "CSN1" {
		t.Errorf("Chassis identifiers = %+v", c.Identifiers)
	}
}

func TestBIOSProberTrimsFields(t *testing.T) {
	got := probeAs[BIOS](t, NewBIOSProber(fullDMIHost()))
	if got != (BIOS{Vendor: "Dell Inc.", Version: "2.1.0", Date: "01/02/2024"}) {
		t.Errorf("BIOS = %+v", got)
	}
}

// A VM lacks the board files and product_sku: their fields are omitted.
func TestDMIProbersOmitMissingFiles(t *testing.T) {
	host := dmiHost(map[string]string{"product_name": "KVM\n", "sys_vendor": "QEMU\n", "chassis_type": "1\n"})
	priv := privFS(map[string]string{"product_uuid": "u-1\n"})

	p := probeAs[Product](t, NewProductProber(host, priv))
	if p.SKU != "" || p.SerialNumber != "" || p.Family != "" || p.UUID != "u-1" {
		t.Errorf("Product = %+v", p)
	}
	if _, ok := p.Identifiers["serial_number"]; ok || len(p.Identifiers) != 1 {
		t.Errorf("Product identifiers = %+v", p.Identifiers)
	}
	b := probeAs[Baseboard](t, NewBaseboardProber(host, priv))
	if len(b.Identifiers) != 0 {
		t.Errorf("Baseboard = %+v", b)
	}
	if out, _ := json.Marshal(b); string(out) != "{}" {
		t.Errorf("Baseboard JSON = %s, want {}", out)
	}
	if out, _ := json.Marshal(p); strings.Contains(string(out), "sku") {
		t.Errorf("Product JSON has sku: %s", out)
	}
	c := probeAs[Chassis](t, NewChassisProber(host, priv))
	if c.Type != "1" || c.TypeDescription != "Other" || c.SerialNumber != "" {
		t.Errorf("Chassis = %+v", c)
	}
}

func TestDMIProberReportsUnreadableFileAsUnknown(t *testing.T) {
	host := deniedFS{FS: fullDMIHost(), denied: map[string]bool{dmiTestDir + "/product_name": true, dmiTestDir + "/bios_date": true}}
	p := probeAs[Product](t, NewProductProber(host, fullPrivileged()))
	if p.Name != "unknown" || p.Vendor != "Dell Inc." {
		t.Errorf("Product = %+v", p)
	}
	if b := probeAs[BIOS](t, NewBIOSProber(host)); b.Date != "unknown" || b.Vendor != "Dell Inc." {
		t.Errorf("BIOS = %+v", b)
	}
}

// Without the helper the agent reads the root-only files from the host
// root like any other, as it can when it runs as root: a readable file
// gives its value, a denied one "unknown", a missing one nothing.
func TestDMIProbersWithoutHelperReadRootOnlyFilesFromTheHostRoot(t *testing.T) {
	host := fullDMIHost()
	host[dmiTestDir+"/product_serial"] = &fstest.MapFile{Data: []byte("SVC123\n")}
	host[dmiTestDir+"/product_uuid"] = &fstest.MapFile{Data: []byte("4c4c4544-0001-0002-0003-000000000004\n")}
	host[dmiTestDir+"/chassis_serial"] = &fstest.MapFile{Data: []byte("CSN1\n")}
	root := deniedFS{FS: host, denied: map[string]bool{dmiTestDir + "/product_serial": true}}

	p := probeAs[Product](t, NewProductProber(root, nil))
	if p.SerialNumber != "unknown" || p.UUID != "4c4c4544-0001-0002-0003-000000000004" || p.Name != "PowerEdge R760" {
		t.Errorf("Product = %+v, want a denied serial unknown and the readable UUID", p)
	}
	if id, ok := p.Identifiers["serial_number"]; !ok || id.Usable || id.Raw != "unknown" {
		t.Errorf("identifier serial_number = %+v, ok=%v, want an unusable unknown", id, ok)
	}
	if id := p.Identifiers["uuid"]; !id.Usable {
		t.Errorf("identifier uuid = %+v, want usable", id)
	}
	b := probeAs[Baseboard](t, NewBaseboardProber(root, nil))
	if _, ok := b.Identifiers["serial_number"]; b.SerialNumber != "" || ok {
		t.Errorf("Baseboard = %+v, want the missing board_serial omitted", b)
	}
	c := probeAs[Chassis](t, NewChassisProber(root, nil))
	if c.SerialNumber != "CSN1" || !c.Identifiers["serial_number"].Usable {
		t.Errorf("Chassis = %+v, want the readable chassis_serial", c)
	}
}

// An empty file is omitted from the payload, so it has nothing to assess
// either.
func TestDMIProbersOmitAnEmptyRootOnlyFieldAndItsIdentifier(t *testing.T) {
	priv := privFS(map[string]string{"product_serial": "\n", "product_uuid": "4c4c4544-0001-0002-0003-000000000004\n"})
	p := probeAs[Product](t, NewProductProber(fullDMIHost(), priv))
	if _, ok := p.Identifiers["serial_number"]; p.SerialNumber != "" || ok {
		t.Errorf("Product = %+v, want no serial_number and no identifier for it", p)
	}
	if _, ok := p.Identifiers["uuid"]; !ok {
		t.Errorf("Product = %+v, want the uuid identifier", p)
	}
}

// A helper that answers but cannot read the file fails the probe as one
// that does not answer does.
func TestDMIProbersFailWhenTheHelperCannotReadTheFile(t *testing.T) {
	priv := helper.NewFS(func(helper.Request) (helper.Response, error) {
		return helper.Response{Error: helper.ErrCodeRead, Message: "input/output error"}, nil
	})
	if _, err := NewProductProber(fullDMIHost(), priv).Probe(context.Background()); err == nil {
		t.Error("product probe succeeded, want the helper's read failure")
	}
}

func TestBaseboardOmitsSerialTheHelperLacks(t *testing.T) {
	b := probeAs[Baseboard](t, NewBaseboardProber(fullDMIHost(), privFS(map[string]string{"product_serial": "x\n"})))
	if b.SerialNumber != "" || len(b.Identifiers) != 0 {
		t.Errorf("Baseboard = %+v", b)
	}
}

func TestDMIProbersFailWhenTheHelperIsUnavailable(t *testing.T) {
	host, priv := fullDMIHost(), unavailableFS()
	for name, p := range map[string]Prober{
		"product":   NewProductProber(host, priv),
		"baseboard": NewBaseboardProber(host, priv),
		"chassis":   NewChassisProber(host, priv),
	} {
		if _, err := p.Probe(context.Background()); !errors.Is(err, helper.ErrUnavailable) {
			t.Errorf("%s: err = %v, want ErrUnavailable", name, err)
		}
	}
	if _, err := NewBIOSProber(host).Probe(context.Background()); err != nil {
		t.Errorf("bios: %v", err)
	}
}

func TestChassisTypeDescription(t *testing.T) {
	for typ, want := range map[string]string{"23": "Rack mount chassis", "99": "unknown", "abc": "unknown", "36": "Stick PC", "0": "unknown", "": ""} {
		host := dmiHost(map[string]string{"chassis_type": typ + "\n"})
		c := probeAs[Chassis](t, NewChassisProber(host, fstest.MapFS{}))
		if c.Type != typ || c.TypeDescription != want {
			t.Errorf("type %q: Chassis = %+v, want description %q", typ, c, want)
		}
	}
	denied := deniedFS{FS: fullDMIHost(), denied: map[string]bool{dmiTestDir + "/chassis_type": true}}
	c := probeAs[Chassis](t, NewChassisProber(denied, fullPrivileged()))
	if c.Type != "unknown" || c.TypeDescription != "unknown" {
		t.Errorf("unreadable type: Chassis = %+v", c)
	}
}

// A helper that does not answer costs one timeout per probe, not one per
// root-only field: product stops asking after its first failed read.
func TestProductProberAsksAnUnavailableHelperOnce(t *testing.T) {
	asked := 0
	priv := helper.NewFS(func(helper.Request) (helper.Response, error) {
		asked++
		return helper.Response{}, fmt.Errorf("%w: timed out", helper.ErrUnavailable)
	})
	if _, err := NewProductProber(fullDMIHost(), priv).Probe(context.Background()); !errors.Is(err, helper.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if asked != 1 {
		t.Errorf("asked the helper %d times, want 1", asked)
	}
}

// The helper must serve every file the collectors ask it for, by the name
// they use and from the file the kernel puts it in.
func TestHelperServesEveryRootOnlyDMIFile(t *testing.T) {
	for _, name := range RootOnlyDMIFiles {
		if got, ok := helper.DMIFile(name); !ok || got != dmiTestDir+"/"+name {
			t.Errorf("helper.DMIFile(%q) = %q, %v, want %s/%s", name, got, ok, dmiTestDir, name)
		}
	}
	table, ok := helper.DMIFile(SMBIOSTableFile)
	if SMBIOSTableFile != helper.SMBIOSTable || !ok || table != "sys/firmware/dmi/tables/DMI" {
		t.Errorf("the memory prober reads %q; the helper serves the SMBIOS table as %q from %q (found %v)",
			SMBIOSTableFile, helper.SMBIOSTable, table, ok)
	}
}

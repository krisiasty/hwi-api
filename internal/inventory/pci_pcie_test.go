package inventory

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
	"testing/fstest"
)

func TestPCIProberReadsTheLink(t *testing.T) {
	p := probePCI(t, pciHost(), testDB(t))

	if got := pciDevice(t, p, "0000:3a:00.0").Link; got == nil ||
		*got != (PCILink{CurrentSpeed: "16.0 GT/s PCIe", CurrentWidth: 16, MaxSpeed: "16.0 GT/s PCIe", MaxWidth: 16}) {
		t.Errorf("root port link = %+v", got)
	}
	// A card trained below what it supports is reported as it is, with no
	// verdict: a lower speed may be power management.
	if got := pciDevice(t, p, "0000:3b:00.0").Link; got == nil ||
		*got != (PCILink{CurrentSpeed: "8.0 GT/s PCIe", CurrentWidth: 8, MaxSpeed: "16.0 GT/s PCIe", MaxWidth: 16}) {
		t.Errorf("PF link = %+v", got)
	}
	// No link: a conventional PCI device, which has no link files, and a
	// VF, whose link files read registers reserved for VFs. A VF's link is
	// its physical function's.
	for _, address := range []string{"0000:00:1f.6", "0000:3b:00.2"} {
		if got := pciDevice(t, p, address).Link; got != nil {
			t.Errorf("%s link = %+v, want none", address, got)
		}
	}
}

// Link files that exist but cannot be read or parsed give "unknown" and 0;
// the link is still reported.
func TestPCIProberReportsAnUnreadableLinkAsUnknown(t *testing.T) {
	m := pciHost()
	m["sys/devices/pci0000:5d/0000:5d:02.0/0000:5e:00.0/max_link_width"] = &fstest.MapFile{Data: []byte("x4\n")}
	dir := pciDeviceDir("0000:5e:00.0")
	fsys := deniedFS{FS: m, denied: map[string]bool{dir + "/current_link_speed": true, dir + "/current_link_width": true}}

	got := pciDevice(t, probePCI(t, fsys, testDB(t)), "0000:5e:00.0").Link
	want := PCILink{CurrentSpeed: "unknown", CurrentWidth: 0, MaxSpeed: "16.0 GT/s PCIe", MaxWidth: 0}
	if got == nil || *got != want {
		t.Errorf("link = %+v, want %+v", got, want)
	}
}

func TestPCIProberReadsSRIOV(t *testing.T) {
	p := probePCI(t, pciHost(), testDB(t))

	pf := pciDevice(t, p, "0000:3b:00.0")
	if pf.SRIOV == nil || pf.SRIOV.TotalVFs != 8 || pf.SRIOV.NumVFs != 2 ||
		!slices.Equal(pf.SRIOV.VFs, []string{"0000:3b:00.2", "0000:3b:00.3"}) {
		t.Errorf("PF SRIOV = %+v", pf.SRIOV)
	}
	if pf.VFOf != "" {
		t.Errorf("PF VFOf = %q", pf.VFOf)
	}

	vf := pciDevice(t, p, "0000:3b:00.2")
	if vf.SRIOV != nil || vf.VFOf != "0000:3b:00.0" {
		t.Errorf("VF SRIOV, VFOf = %+v, %q", vf.SRIOV, vf.VFOf)
	}
	if nvme := pciDevice(t, p, "0000:5e:00.0"); nvme.SRIOV != nil || nvme.VFOf != "" {
		t.Errorf("non-SR-IOV device SRIOV, VFOf = %+v, %q", nvme.SRIOV, nvme.VFOf)
	}
}

// VFs are listed by index, so virtfn10 follows virtfn2, as it would not in
// directory order.
func TestPCIProberOrdersVFsByIndex(t *testing.T) {
	m := pciHost()
	pf := "sys/devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0/"
	m[pf+"virtfn2"] = sysLink(pf+"virtfn2", "sys/devices/pci0000:3a/0000:3a:00.0/0000:3b:00.4")
	m[pf+"virtfn10"] = sysLink(pf+"virtfn10", "sys/devices/pci0000:3a/0000:3a:00.0/0000:3b:01.4")

	got := pciDevice(t, probePCI(t, m, testDB(t)), "0000:3b:00.0").SRIOV.VFs
	want := []string{"0000:3b:00.2", "0000:3b:00.3", "0000:3b:00.4", "0000:3b:01.4"}
	if !slices.Equal(got, want) {
		t.Errorf("VFs = %v, want %v", got, want)
	}
}

// A PF with SR-IOV but no VFs configured lists none, as an empty list.
func TestPCIProberReportsAPFWithoutVFs(t *testing.T) {
	m := pciHost()
	pf := "sys/devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0/"
	m[pf+"sriov_numvfs"] = &fstest.MapFile{Data: []byte("0\n")}
	delete(m, pf+"virtfn0")
	delete(m, pf+"virtfn1")

	s := pciDevice(t, probePCI(t, m, testDB(t)), "0000:3b:00.0").SRIOV
	if s == nil || s.TotalVFs != 8 || s.NumVFs != 0 {
		t.Fatalf("SRIOV = %+v", s)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"vfs":[]`)) {
		t.Errorf("SRIOV = %s, want an empty vfs list, not null", b)
	}
}

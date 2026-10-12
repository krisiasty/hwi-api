package inventory

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/krisiasty/hwi-api/internal/helper"
)

// slotStructure is an SMBIOS 2.6 Type 9 structure.
func slotStructure(designation string, typ, width, usage byte, seg uint16, bus, devfn byte) []byte {
	return structure(smbiosTypeSystemSlot, 0x11, func(f []byte) {
		f[0x04], f[0x05], f[0x06], f[0x07] = 1, typ, width, usage
		putWord(f, 0x0D, seg)
		f[0x0F], f[0x10] = bus, devfn
	}, designation)
}

func TestSystemSlotsDecodesTheTable(t *testing.T) {
	slots, err := systemSlots(table(
		slotStructure("CPU1 SLOT3 PCI-E 5.0 X16", 0xC3, 0x0D, 4, 0, 0x3a, 0x00), // in use, at a root port
		slotStructure("PCIe Slot 7", 0xB6, 0x0B, 3, 0xFFFF, 0xFF, 0xFF),         // available, address unset
		slotStructure("M.2", 0x17, 0x02, 2, 0, 0, 0),                            // usage unknown, all-zero address
	))
	if err != nil {
		t.Fatal(err)
	}
	want := []SystemSlot{
		{Designation: "CPU1 SLOT3 PCI-E 5.0 X16", Type: "PCI Express 5 x16", Width: "x16", InUse: new(true), Address: "0000:3a:00.0"},
		{Designation: "PCIe Slot 7", Type: "PCI Express 3 x16", Width: "x8", InUse: new(false)},
		{Designation: "M.2", Type: "M.2 Socket 3", Width: "unknown"},
	}
	if !reflect.DeepEqual(slots, want) {
		t.Errorf("systemSlots =\n%+v\nwant\n%+v", slots, want)
	}
}

// A pre-2.6 structure has no address fields.
func TestSystemSlotsBeforeSMBIOS26(t *testing.T) {
	s := structure(smbiosTypeSystemSlot, 0x0D, func(f []byte) {
		f[0x04], f[0x05], f[0x06], f[0x07] = 1, 0x06, 0x05, 4
	}, "PCI1")
	slots, err := systemSlots(table(s))
	if err != nil || len(slots) != 1 || slots[0].Address != "" || slots[0].Type != "PCI" || slots[0].Width != "32 bit" {
		t.Fatalf("systemSlots = %+v, %v", slots, err)
	}
	if in := slots[0].InUse; in == nil || !*in {
		t.Errorf("InUse = %v, want true for usage 4", in)
	}
}

// Usage codes other than available (3) and in use (4) say nothing about
// whether a card is fitted, so InUse is absent.
func TestSystemSlotsUsageWithoutAnAnswer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage byte
	}{{"Other", 1}, {"Unavailable", 5}} {
		s := structure(smbiosTypeSystemSlot, 0x0D, func(f []byte) {
			f[0x04], f[0x05], f[0x06], f[0x07] = 1, 0x06, 0x05, tc.usage
		}, "PCI1")
		slots, err := systemSlots(table(s))
		if err != nil || len(slots) != 1 {
			t.Fatalf("%s: systemSlots = %+v, %v", tc.name, slots, err)
		}
		if slots[0].InUse != nil {
			t.Errorf("%s (usage %d): InUse = %v, want absent", tc.name, tc.usage, *slots[0].InUse)
		}
	}
}

func TestSystemSlotsRejectsAShortStructure(t *testing.T) {
	if _, err := systemSlots(table(structure(smbiosTypeSystemSlot, 0x08, nil))); err == nil {
		t.Error("a Type 9 structure shorter than SMBIOS 2.0's was decoded")
	}
}

func TestSlotsProber(t *testing.T) {
	quiet := slog.New(slog.DiscardHandler)
	probe := func(privileged fstest.MapFS) *Slots {
		t.Helper()
		var p Prober
		if privileged == nil {
			p = NewSlotsProber(nil, quiet)
		} else {
			p = NewSlotsProber(privileged, quiet)
		}
		v, err := p.Probe(context.Background())
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		return v.(*Slots)
	}
	got := probe(fstest.MapFS{helper.SMBIOSTable: {Data: table(slotStructure("Slot 1", 0xA5, 0x0D, 3, 0, 0x17, 0))}})
	if got.SlotsError != "" || len(got.Slots) != 1 || got.Slots[0].Designation != "Slot 1" {
		t.Errorf("slots = %+v", got)
	}
	// A VM's table has no Type 9: an empty list, not an error.
	if got := probe(fstest.MapFS{helper.SMBIOSTable: {Data: table()}}); got.SlotsError != "" || got.Slots == nil || len(got.Slots) != 0 {
		t.Errorf("no Type 9: %+v, want an empty list", got)
	}
	if got := probe(nil); got.Slots != nil || !strings.Contains(got.SlotsError, "not configured") {
		t.Errorf("no helper: %+v", got)
	}
	// A helper on a host without the table, as some VMs are: null slots,
	// not an empty list, which would say the firmware lists none.
	if got := probe(fstest.MapFS{}); got.Slots != nil || !strings.Contains(got.SlotsError, "the host has none") {
		t.Errorf("no table: %+v", got)
	} else if b, err := json.Marshal(got); err != nil || !strings.Contains(string(b), `"slots":null`) {
		t.Errorf("no table: payload %s (%v), want null slots", b, err)
	}
	if got := probe(fstest.MapFS{helper.SMBIOSTable: {Data: []byte{9, 2, 0, 0}}}); got.Slots != nil || !strings.Contains(got.SlotsError, "decoding") {
		t.Errorf("broken table: %+v", got)
	}
}

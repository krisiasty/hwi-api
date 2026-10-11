package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
)

const testMeminfo = `MemTotal:       263846644 kB
MemFree:        250000000 kB
HugePages_Total:       0
Hugepagesize:       2048 kB
Hugetlb:         4194304 kB
`

// hostFS is a host root with three 128 MiB memory blocks, two of them
// online, and two huge page sizes.
func hostFS() fstest.MapFS {
	hp := "sys/kernel/mm/hugepages/"
	return fstest.MapFS{
		"proc/meminfo": {Data: []byte(testMeminfo)},
		"sys/devices/system/memory/block_size_bytes": {Data: []byte("8000000\n")},
		"sys/devices/system/memory/memory0/state":    {Data: []byte("online\n")},
		"sys/devices/system/memory/memory1/state":    {Data: []byte("offline\n")},
		"sys/devices/system/memory/memory32/state":   {Data: []byte("online\n")},
		"sys/devices/system/memory/power/state":      {Data: []byte("online\n")},
		hp + "hugepages-1048576kB/nr_hugepages":      {Data: []byte("4\n")},
		hp + "hugepages-1048576kB/free_hugepages":    {Data: []byte("3\n")},
		hp + "hugepages-1048576kB/surplus_hugepages": {Data: []byte("0\n")},
		hp + "hugepages-1048576kB/resv_hugepages":    {Data: []byte("1\n")},
		hp + "hugepages-2048kB/nr_hugepages":         {Data: []byte("0\n")},
		hp + "hugepages-2048kB/free_hugepages":       {Data: []byte("0\n")},
		hp + "hugepages-2048kB/surplus_hugepages":    {Data: []byte("0\n")},
		hp + "hugepages-2048kB/resv_hugepages":       {Data: []byte("0\n")},
	}
}

// twoSlots is a table with a 32 GiB module and an empty slot.
func twoSlots() fstest.MapFS {
	empty := structure(smbiosTypeMemoryDevice, 0x28, func(f []byte) { f[0x10] = 1 }, "DIMM_A2")
	return fstest.MapFS{SMBIOSTableFile: {Data: table(ddr5(), empty)}}
}

func probeMemory(t *testing.T, root, privileged fs.FS) (*Memory, string) {
	t.Helper()
	var logs bytes.Buffer
	v, err := NewMemoryProber(root, privileged, slog.New(slog.NewTextHandler(&logs, nil))).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return v.(*Memory), logs.String()
}

func TestMemoryProber(t *testing.T) {
	m, logs := probeMemory(t, hostFS(), twoSlots())

	if m.TotalUsableBytes != 263846644*1024 || m.TotalPhysicalBytes != 2*128<<20 {
		t.Errorf("totals = %d usable, %d physical", m.TotalUsableBytes, m.TotalPhysicalBytes)
	}
	if m.DefaultHugePageSize != 2<<20 || m.TotalHugePageBytes != 4<<30 {
		t.Errorf("huge pages = %d default size, %d total", m.DefaultHugePageSize, m.TotalHugePageBytes)
	}
	// Directory order.
	if len(m.SupportedPageSizes) != 2 || m.SupportedPageSizes[0] != 1<<30 || m.SupportedPageSizes[1] != 2<<20 {
		t.Errorf("supported page sizes = %v", m.SupportedPageSizes)
	}
	if got := m.HugePageAmountsBySize[1<<30]; got != (HugePageAmounts{Total: 4, Free: 3, Reserved: 1}) {
		t.Errorf("1 GiB huge pages = %+v", got)
	}
	if len(m.Modules) != 2 || !m.Modules[0].Installed || m.Modules[1].Installed || m.Modules[1].Location != "DIMM_A2" {
		t.Errorf("modules = %+v, want the DDR5 module then the empty slot", m.Modules)
	}
	if m.InstalledBytes != 32<<30 || m.ModulesError != "" {
		t.Errorf("installed %d bytes, modules error %q", m.InstalledBytes, m.ModulesError)
	}
	if logs != "" {
		t.Errorf("logged %q, want nothing", logs)
	}
}

// Without the kernel's memory blocks, the physical total falls back to the
// usable one.
func TestMemoryProberWithoutMemoryBlocks(t *testing.T) {
	root := fstest.MapFS{"proc/meminfo": {Data: []byte(testMeminfo)}}
	m, _ := probeMemory(t, root, twoSlots())
	if m.TotalPhysicalBytes != m.TotalUsableBytes || m.SupportedPageSizes != nil {
		t.Errorf("physical %d, usable %d, page sizes %v", m.TotalPhysicalBytes, m.TotalUsableBytes, m.SupportedPageSizes)
	}
}

// Without the modules, the probe still succeeds with the totals, and says
// why the modules are missing. Only a table that exists but is broken is
// logged: a host without one is an expected state.
func TestMemoryProberWithoutModules(t *testing.T) {
	for _, tc := range []struct {
		name       string
		privileged fs.FS
		want       string
		logged     bool
	}{
		{"no helper configured", nil, "not configured", false},
		{"host without a table", fstest.MapFS{}, "the host has none", false},
		{"broken table", fstest.MapFS{SMBIOSTableFile: {Data: []byte{17, 2, 0, 0}}}, "decoding the SMBIOS table", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, logs := probeMemory(t, hostFS(), tc.privileged)
			if m.TotalUsableBytes == 0 || m.Modules != nil || m.InstalledBytes != 0 {
				t.Errorf("usable %d, modules %v, installed %d; want the totals and no modules",
					m.TotalUsableBytes, m.Modules, m.InstalledBytes)
			}
			if !strings.Contains(m.ModulesError, tc.want) {
				t.Errorf("modules error = %q, want it to mention %q", m.ModulesError, tc.want)
			}
			if logged := strings.Contains(logs, "memory modules not reported"); logged != tc.logged {
				t.Errorf("logged = %v (%q), want %v", logged, logs, tc.logged)
			}

			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), `"modules":null,"modules_error":`) {
				t.Errorf("payload %s, want modules null followed by modules_error", b)
			}
		})
	}
}

func TestMemoryProberPayloadOmitsAnEmptyModulesError(t *testing.T) {
	m, _ := probeMemory(t, hostFS(), twoSlots())
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "modules_error") {
		t.Errorf("payload %s has modules_error with the modules read", b)
	}
}

func TestMemoryProberFailsWithoutMemTotal(t *testing.T) {
	root := fstest.MapFS{"proc/meminfo": {Data: []byte("MemFree: 1 kB\n")}}
	if _, err := NewMemoryProber(root, nil, quietLogger()).Probe(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "MemTotal") {
		t.Errorf("Probe error = %v, want one about MemTotal", err)
	}
	if _, err := NewMemoryProber(fstest.MapFS{}, nil, quietLogger()).Probe(context.Background()); err == nil {
		t.Error("Probe without /proc/meminfo succeeded")
	}
}

// Probing an unchanged host twice gives the same bytes, so the payload's ETag only changes when the memory does: the huge page sizes and the
// modules are lists whose order must not depend on map iteration.
func TestMemoryProberIsByteStable(t *testing.T) {
	payload := func() string {
		m, _ := probeMemory(t, hostFS(), twoSlots())
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	first := payload()
	for range 10 {
		if again := payload(); again != first {
			t.Fatalf("a second probe differs:\n got %s\nwant %s", again, first)
		}
	}
}

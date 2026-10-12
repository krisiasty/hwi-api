package main

import (
	"testing"
	"testing/fstest"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
	"github.com/krisiasty/hwi-api/internal/inventory"
)

// virtStore holds a cpu entry with the given capabilities and a product
// entry naming a QEMU/KVM guest, as the cpu and product probers would
// store them.
func virtStore(t *testing.T, vendor string, capabilities ...string) *inventory.Store {
	t.Helper()
	store := inventory.NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true, apiv1.SubsystemProduct: true})
	now := time.Now()
	cpu := inventory.CPU{Processors: []inventory.Processor{{Vendor: vendor, Capabilities: capabilities}}}
	if err := store.Put(apiv1.SubsystemCPU, cpu, now); err != nil {
		t.Fatal(err)
	}
	product := inventory.Product{Vendor: "QEMU", Name: "KVM Virtual Machine"}
	if err := store.Put(apiv1.SubsystemProduct, product, now); err != nil {
		t.Fatal(err)
	}
	return store
}

// cpuinfoRoot is a host root whose /proc/cpuinfo lists one CPU's
// capabilities under key, "flags" on x86 or "Features" on arm64.
func cpuinfoRoot(key, capabilities string) fstest.MapFS {
	return fstest.MapFS{"proc/cpuinfo": &fstest.MapFile{Data: []byte("processor\t: 0\n" + key + "\t\t: " + capabilities + "\n\n")}}
}

// Only x86's cpuinfo flags carry a hypervisor bit; arm64's Features never
// do, so their presence must not count as bare-metal evidence that
// overrides a hypervisor named in DMI. Which one the host reports is read
// from its cpuinfo, never from the agent's GOARCH: under user-mode
// emulation an amd64 agent reads an arm64 host's Features, so each case
// here holds whatever architecture the test itself runs on.
func TestCollectVirtSignalsOnlyTrustsTheHypervisorFlagFromX86Flags(t *testing.T) {
	tests := []struct {
		name           string
		root           fstest.MapFS
		vendor         string
		capabilities   []string
		wantType       string
		wantTechnology string
		wantConfidence string
	}{
		{"arm64 guest named by DMI", cpuinfoRoot("Features", "fp asimd aes"), "ARM", []string{"fp", "asimd", "aes"}, inventory.TypeVirtual, "kvm", "medium"},
		{"x86 guest with the hypervisor flag", cpuinfoRoot("flags", "fpu hypervisor sse2"), "GenuineIntel", []string{"fpu", "hypervisor", "sse2"}, inventory.TypeVirtual, "kvm", "high"},
		// A bare-metal instance can carry a hypervisor vendor's DMI
		// strings; on x86 the missing flag still outranks them.
		{"x86 without the hypervisor flag", cpuinfoRoot("flags", "fpu sse2"), "GenuineIntel", []string{"fpu", "sse2"}, inventory.TypePhysical, "", "high"},
		// Without cpuinfo the capabilities' source is unknown, so they are
		// not evidence either way.
		{"capabilities but no cpuinfo", fstest.MapFS{}, "GenuineIntel", []string{"fpu", "sse2"}, inventory.TypeVirtual, "kvm", "medium"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := virtStore(t, tt.vendor, tt.capabilities...)
			got := inventory.Detect(collectVirtSignals(store, tt.root))
			if got.Type != tt.wantType || got.Technology != tt.wantTechnology || got.Confidence != tt.wantConfidence {
				t.Errorf("Detect = %s/%q/%s, want %s/%q/%s (evidence %v)",
					got.Type, got.Technology, got.Confidence, tt.wantType, tt.wantTechnology, tt.wantConfidence, got.Evidence)
			}
		})
	}
}

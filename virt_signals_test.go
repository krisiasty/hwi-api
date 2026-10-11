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

// Only x86 reports a hypervisor bit among its CPU capabilities. arm64's
// Features never include one, so their presence must not count as
// bare-metal evidence that overrides a hypervisor named in DMI.
func TestCollectVirtSignalsOnlyTrustsTheHypervisorFlagOnX86(t *testing.T) {
	tests := []struct {
		name           string
		arch           string
		vendor         string
		capabilities   []string
		wantType       string
		wantTechnology string
		wantConfidence string
	}{
		{"arm64 guest named by DMI", "arm64", "ARM", []string{"fp", "asimd", "aes"}, inventory.TypeVirtual, "kvm", "medium"},
		{"x86 guest with the hypervisor flag", "amd64", "GenuineIntel", []string{"fpu", "hypervisor", "sse2"}, inventory.TypeVirtual, "kvm", "high"},
		// A bare-metal instance can carry a hypervisor vendor's DMI
		// strings; on x86 the missing flag still outranks them.
		{"x86 without the hypervisor flag", "amd64", "GenuineIntel", []string{"fpu", "sse2"}, inventory.TypePhysical, "", "high"},
		{"386 without the hypervisor flag", "386", "GenuineIntel", []string{"fpu", "sse2"}, inventory.TypePhysical, "", "high"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := virtStore(t, tt.vendor, tt.capabilities...)
			got := inventory.Detect(collectVirtSignals(store, fstest.MapFS{}, tt.arch))
			if got.Type != tt.wantType || got.Technology != tt.wantTechnology || got.Confidence != tt.wantConfidence {
				t.Errorf("Detect = %s/%q/%s, want %s/%q/%s (evidence %v)",
					got.Type, got.Technology, got.Confidence, tt.wantType, tt.wantTechnology, tt.wantConfidence, got.Evidence)
			}
		})
	}
}

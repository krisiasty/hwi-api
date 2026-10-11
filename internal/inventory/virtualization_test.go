package inventory

import "testing"

func TestDetect(t *testing.T) {
	tests := []struct {
		name           string
		signals        VirtSignals
		wantType       string
		wantTechnology string
		wantConfidence string
	}{
		{
			name: "vmware guest",
			signals: VirtSignals{
				CPUCapabilities: []string{"sse", "hypervisor"},
				DMI:             map[string]string{"sys_vendor": "VMware, Inc.", "product_name": "VMware Virtual Platform"},
				CPUDataPresent:  true,
			},
			wantType: "virtual", wantTechnology: "vmware", wantConfidence: "high",
		},
		{
			name: "kvm guest",
			signals: VirtSignals{
				CPUCapabilities: []string{"hypervisor"},
				DMI:             map[string]string{"sys_vendor": "QEMU", "product_name": "Standard PC (Q35 + ICH9, 2009)"},
				CPUDataPresent:  true,
			},
			wantType: "virtual", wantTechnology: "kvm", wantConfidence: "high",
		},
		{
			name: "hyper-v guest",
			signals: VirtSignals{
				CPUCapabilities: []string{"hypervisor"},
				DMI:             map[string]string{"sys_vendor": "Microsoft Corporation", "product_name": "Virtual Machine"},
				CPUDataPresent:  true,
			},
			wantType: "virtual", wantTechnology: "hyperv", wantConfidence: "high",
		},
		{
			name: "virtualbox guest",
			signals: VirtSignals{
				CPUCapabilities: []string{"hypervisor"},
				DMI:             map[string]string{"sys_vendor": "innotek GmbH", "product_name": "VirtualBox"},
				CPUDataPresent:  true,
			},
			wantType: "virtual", wantTechnology: "virtualbox", wantConfidence: "high",
		},
		{
			name: "xen paravirtualized guest with no cpu flag",
			signals: VirtSignals{
				CPUDataPresent: true,
				HypervisorType: "xen",
			},
			wantType: "virtual", wantTechnology: "xen", wantConfidence: "high",
		},
		{
			name: "ordinary physical server",
			signals: VirtSignals{
				CPUCapabilities: []string{"sse", "avx2"},
				DMI:             map[string]string{"sys_vendor": "Dell Inc.", "product_name": "PowerEdge R650"},
				CPUDataPresent:  true,
			},
			wantType: "physical", wantConfidence: "high",
		},
		{
			name: "bare-metal cloud instance must not be called virtual",
			signals: VirtSignals{
				CPUCapabilities: []string{"sse", "avx2"},
				DMI:             map[string]string{"sys_vendor": "Amazon EC2", "product_name": "c5.metal"},
				CPUDataPresent:  true,
			},
			wantType: "physical", wantConfidence: "high",
		},
		{
			name: "virtualized cloud instance",
			signals: VirtSignals{
				CPUCapabilities: []string{"hypervisor"},
				DMI:             map[string]string{"sys_vendor": "Amazon EC2", "product_name": "m5.large"},
				CPUDataPresent:  true,
			},
			wantType: "virtual", wantTechnology: "amazon", wantConfidence: "high",
		},
		{
			name: "arm64 guest identified by device tree",
			signals: VirtSignals{
				CPUDataPresent:       false,
				DeviceTreeCompatible: "linux,kvm",
			},
			wantType: "virtual", wantTechnology: "kvm", wantConfidence: "high",
		},
		{
			name: "arm64 guest identified only by dmi",
			signals: VirtSignals{
				CPUDataPresent: false,
				DMI:            map[string]string{"sys_vendor": "QEMU"},
			},
			wantType: "virtual", wantTechnology: "kvm", wantConfidence: "medium",
		},
		{
			name:     "nothing known at all is unknown, never physical",
			signals:  VirtSignals{CPUDataPresent: false},
			wantType: "unknown", wantConfidence: "low",
		},
		{
			name: "xen hvm guest with both hypervisor flag and hypervisor type set",
			signals: VirtSignals{
				CPUDataPresent:  true,
				CPUCapabilities: []string{"hypervisor"},
				HypervisorType:  "xen",
			},
			wantType: "virtual", wantTechnology: "xen", wantConfidence: "high",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Detect(tt.signals)
			if got.Type != tt.wantType {
				t.Errorf("Type = %q, want %q (evidence: %+v)", got.Type, tt.wantType, got.Evidence)
			}
			if tt.wantTechnology != "" && got.Technology != tt.wantTechnology {
				t.Errorf("Technology = %q, want %q", got.Technology, tt.wantTechnology)
			}
			if got.Confidence != tt.wantConfidence {
				t.Errorf("Confidence = %q, want %q", got.Confidence, tt.wantConfidence)
			}
			if len(got.Evidence) == 0 && got.Type != "unknown" {
				t.Error("a verdict other than unknown must cite its evidence")
			}
		})
	}
}

func TestDetectRecordsEvidenceForASurprisingVerdict(t *testing.T) {
	got := Detect(VirtSignals{
		CPUCapabilities: []string{"sse"},
		CPUDataPresent:  true,
		DMI:             map[string]string{"sys_vendor": "Amazon EC2"},
	})

	var sawVendor, sawAbsentFlag bool
	for _, e := range got.Evidence {
		if e.Signal == "dmi.sys_vendor" && e.Value == "Amazon EC2" {
			sawVendor = true
		}
		if e.Signal == "cpu.capability" {
			sawAbsentFlag = true
		}
	}
	if !sawVendor || !sawAbsentFlag {
		t.Errorf("evidence should record both the cloud vendor and the absent CPU flag: %+v", got.Evidence)
	}
}

package inventory

import (
	"context"
	"sort"
	"strings"
)

// Verdict values.
const (
	TypePhysical = "physical"
	TypeVirtual  = "virtual"
	TypeUnknown  = "unknown"
)

// Virtualization is the verdict, the technology behind it, and why.
type Virtualization struct {
	Type       string     `json:"type"`
	Technology string     `json:"technology,omitempty"`
	Confidence string     `json:"confidence"`
	Evidence   []Evidence `json:"evidence"`
}

// Evidence is one observation that informed the verdict. It exists so a
// surprising answer can be diagnosed from the response alone.
type Evidence struct {
	Signal string `json:"signal"`
	Value  string `json:"value"`
}

// VirtSignals is everything detection looks at.
type VirtSignals struct {
	// CPUDataPresent distinguishes "the CPU reported no hypervisor flag",
	// which is strong evidence of bare metal, from "no CPU flags were
	// available", which is no evidence at all. arm64 is the latter.
	CPUDataPresent  bool
	CPUCapabilities []string
	// DMI holds sys_vendor, product_name, bios_vendor, board_vendor and
	// chassis_vendor as read from the corresponding DMI payloads.
	DMI                  map[string]string
	HypervisorType       string
	DeviceTreeCompatible string
}

// dmiSignatures maps a lowercase substring of a DMI vendor or product
// string to the technology it identifies. Order is irrelevant because the
// keys do not overlap.
var dmiSignatures = map[string]string{
	"vmware":                "vmware",
	"qemu":                  "kvm",
	"kvm":                   "kvm",
	"bochs":                 "kvm",
	"microsoft corporation": "hyperv",
	"innotek":               "virtualbox",
	"virtualbox":            "virtualbox",
	"xen":                   "xen",
	"parallels":             "parallels",
	"bhyve":                 "bhyve",
	"amazon ec2":            "amazon",
	"google":                "gce",
	"alibaba":               "alibaba",
	"nutanix":               "nutanix",
	"oracle":                "virtualbox",
}

// Detect weighs the signals and returns a verdict.
//
// The CPU hypervisor capability outranks every DMI string. A bare-metal
// cloud instance reports a cloud vendor in DMI but sets no hypervisor flag,
// and calling it virtual would create the wrong kind of inventory object.
func Detect(s VirtSignals) Virtualization {
	v := Virtualization{Type: TypeUnknown, Confidence: "low"}

	// The technology candidate is computed once, independent of which
	// branch below decides the verdict. Otherwise an earlier verdict arm
	// (say, the CPU hypervisor flag) short-circuits the switch and silently
	// discards a technology that a later arm (say, HypervisorType) would
	// have named — as happens for a Xen HVM guest that sets both signals.
	// Priority: DMI names the actual product and is most specific, then
	// HypervisorType, then the device-tree compatible string.
	technology, techEvidence := matchDMI(s.DMI)
	if technology == "" && s.HypervisorType != "" {
		technology = strings.ToLower(s.HypervisorType)
	}
	if technology == "" && s.DeviceTreeCompatible != "" {
		technology = dtTechnology(s.DeviceTreeCompatible)
	}

	hasFlag := false
	for _, c := range s.CPUCapabilities {
		if c == "hypervisor" {
			hasFlag = true
			break
		}
	}

	switch {
	case hasFlag:
		v.Type, v.Confidence = TypeVirtual, "high"
		v.Evidence = append(v.Evidence, Evidence{Signal: "cpu.capability", Value: "hypervisor"})

	case s.HypervisorType != "":
		v.Type, v.Confidence = TypeVirtual, "high"
		v.Evidence = append(v.Evidence, Evidence{Signal: "sys.hypervisor.type", Value: s.HypervisorType})

	case s.DeviceTreeCompatible != "":
		v.Type, v.Confidence = TypeVirtual, "high"
		v.Evidence = append(v.Evidence, Evidence{Signal: "devicetree.hypervisor", Value: s.DeviceTreeCompatible})

	case s.CPUDataPresent:
		// CPU flags were readable and the hypervisor bit is absent. That is
		// the strong bare-metal signal, and it overrides a cloud vendor
		// string in DMI.
		v.Type, v.Confidence = TypePhysical, "high"
		v.Evidence = append(v.Evidence, Evidence{Signal: "cpu.capability", Value: "hypervisor absent"})
		technology = ""

	case technology != "":
		// No CPU data, as on arm64, but DMI names a hypervisor vendor.
		v.Type, v.Confidence = TypeVirtual, "medium"
	}

	if technology != "" {
		v.Technology = technology
	}
	if len(techEvidence) > 0 && v.Type != TypeUnknown {
		v.Evidence = append(v.Evidence, techEvidence...)
	}
	return v
}

// matchDMI finds the first vendor signature across the DMI fields, checking
// them in a fixed order so the result never depends on map iteration.
func matchDMI(dmi map[string]string) (string, []Evidence) {
	fields := []string{"sys_vendor", "product_name", "bios_vendor", "board_vendor", "chassis_vendor"}
	keys := sortedKeys(dmiSignatures)
	var evidence []Evidence
	technology := ""

	for _, f := range fields {
		value := strings.TrimSpace(dmi[f])
		if value == "" {
			continue
		}
		lower := strings.ToLower(value)
		for _, key := range keys {
			if !strings.Contains(lower, key) {
				continue
			}
			if technology == "" {
				technology = dmiSignatures[key]
			}
			evidence = append(evidence, Evidence{Signal: "dmi." + f, Value: value})
			break
		}
	}
	return technology, evidence
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dtTechnology(compatible string) string {
	lower := strings.ToLower(compatible)
	switch {
	case strings.Contains(lower, "kvm"):
		return "kvm"
	case strings.Contains(lower, "xen"):
		return "xen"
	default:
		return ""
	}
}

// NewVirtualizationProber wraps a signal collector. The collector is a
// parameter so that the detection logic above is testable without any host
// access at all.
func NewVirtualizationProber(collect func(context.Context) (VirtSignals, error)) Prober {
	return ProberFunc(func(ctx context.Context) (any, error) {
		signals, err := collect(ctx)
		if err != nil {
			return nil, err
		}
		return Detect(signals), nil
	})
}

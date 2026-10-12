package inventory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// cpuSysDir has a directory per logical CPU, cpu<N>, and the kernel's CPU
// lists: possible, present and online.
const cpuSysDir = "sys/devices/system/cpu"

// cpuInfoPath is the kernel's description of each online logical CPU.
const cpuInfoPath = "proc/cpuinfo"

// CPU is the cpu payload: the host's processor packages, their cores and
// the logical CPUs (hardware threads) of each core.
type CPU struct {
	TotalCores           int         `json:"total_cores"`
	TotalHardwareThreads int         `json:"total_hardware_threads"`
	Processors           []Processor `json:"processors"`
	// OfflineLogicalProcessors lists the CPUs that are present but not
	// online, which the counts above leave out. It is omitted when there
	// are none or when the kernel's CPU lists cannot be read: it is a hint,
	// not a count.
	OfflineLogicalProcessors []int `json:"offline_logical_processors,omitempty"`
}

// Processor is one processor package. Its cpuinfo fields (Vendor, Model,
// Microcode and Capabilities) come from its lowest-numbered online CPU that
// has a cpuinfo block and are omitted when cpuinfo lacks them; Vendor and
// Model are "unknown" when /proc/cpuinfo exists but cannot be read.
type Processor struct {
	// ID is the kernel's physical_package_id, or -1 for CPUs whose package
	// cannot be read, so they are not merged into a real one.
	ID     int    `json:"id"`
	Vendor string `json:"vendor,omitempty"`
	Model  string `json:"model,omitempty"`
	// Microcode is the loaded microcode revision, which vulnerability
	// tracking needs; arm64 does not report one.
	Microcode string `json:"microcode,omitempty"`
	// Capabilities are cpuinfo's flags (x86) or Features (arm64), in the
	// kernel's order.
	Capabilities         []string        `json:"capabilities,omitempty"`
	TotalCores           int             `json:"total_cores"`
	TotalHardwareThreads int             `json:"total_hardware_threads"`
	Cores                []ProcessorCore `json:"cores"`
}

// ProcessorCore is one core: the online CPUs of one sibling set. Its ID is
// the kernel's core_id, which is unique only within a die, so two cores of
// a multi-die package can share one.
type ProcessorCore struct {
	ID                   int   `json:"id"`
	TotalHardwareThreads int   `json:"total_hardware_threads"`
	LogicalProcessors    []int `json:"logical_processors"`
}

// NewCPUProber reports the processors of rootFS, the host root, from sysfs
// and /proc/cpuinfo. A host whose sys/devices/system/cpu is missing or
// cannot be listed, or lists no online CPU, fails the probe, so the store
// keeps the last good payload: an empty one would read as a host without
// processors.
func NewCPUProber(rootFS fs.FS) Prober {
	return ProberFunc(func(context.Context) (any, error) { return readCPU(rootFS) })
}

// logicalCPU is one online logical CPU as its sysfs topology describes it.
type logicalCPU struct {
	n, pkg, core int
	// siblings is the CPU's sibling set, or nil when it has none readable.
	siblings []int
}

func readCPU(fsys fs.FS) (*CPU, error) {
	cpus, err := readLogicalCPUs(fsys)
	if err != nil {
		return nil, err
	}
	if len(cpus) == 0 {
		return nil, fmt.Errorf("no online CPU in %s", cpuSysDir)
	}
	info, infoErr := readCPUInfo(fsys)

	byPackage := map[int][]logicalCPU{}
	for _, c := range cpus {
		byPackage[c.pkg] = append(byPackage[c.pkg], c)
	}
	out := &CPU{OfflineLogicalProcessors: offlineCPUs(fsys)}
	for _, id := range slices.Sorted(maps.Keys(byPackage)) {
		members := byPackage[id]
		p := Processor{ID: id, Cores: groupCores(members), TotalHardwareThreads: len(members)}
		p.TotalCores = len(p.Cores)
		setCPUInfo(&p, info, infoErr, members)
		out.Processors = append(out.Processors, p)
		out.TotalCores += p.TotalCores
		out.TotalHardwareThreads += p.TotalHardwareThreads
	}
	return out, nil
}

// readLogicalCPUs reads the online CPUs' topology, sorted by number. A CPU
// whose online file reads 0 is offline and has no topology to read, and so
// is one whose package id has gone missing since: the kernel removes the
// topology directory of a CPU that goes offline mid-probe. One without an
// online file, as cpu0 on most x86 hosts, cannot go offline.
func readLogicalCPUs(fsys fs.FS) ([]logicalCPU, error) {
	entries, err := fs.ReadDir(fsys, cpuSysDir)
	if err != nil {
		return nil, fmt.Errorf("listing CPUs: %w", err)
	}
	var cpus []logicalCPU
	for _, e := range entries {
		digits, ok := strings.CutPrefix(e.Name(), "cpu")
		if !ok {
			continue
		}
		n, err := parseCPUNumber(digits)
		if err != nil {
			continue // cpufreq, cpuidle and the like
		}
		dir := cpuSysDir + "/" + e.Name()
		if b, err := fs.ReadFile(fsys, dir+"/online"); err == nil && strings.TrimSpace(string(b)) == "0" {
			continue
		}
		pkg, err := readTopologyID(fsys, dir+"/topology/physical_package_id")
		if errors.Is(err, fs.ErrNotExist) {
			continue // went offline since its online file was read
		}
		core, _ := readTopologyID(fsys, dir+"/topology/core_id")
		cpus = append(cpus, logicalCPU{
			n:        n,
			pkg:      pkg,
			core:     core,
			siblings: readSiblings(fsys, dir),
		})
	}
	// ReadDir sorts by name, which puts cpu10 before cpu2.
	slices.SortFunc(cpus, func(a, b logicalCPU) int { return cmp.Compare(a.n, b.n) })
	return cpus, nil
}

// readTopologyID reads a topology ID, or returns -1 and the error when it
// cannot be read or parsed: 0 would merge the CPU into a real package or
// core.
func readTopologyID(fsys fs.FS, name string) (int, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return -1, err
	}
	id, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return -1, err
	}
	return id, nil
}

// readSiblings reads the CPU's sibling set from thread_siblings_list, or
// from core_cpus_list, its newer name, on a kernel without the old one. It
// is nil when neither can be read or parsed.
func readSiblings(fsys fs.FS, dir string) []int {
	b, err := fs.ReadFile(fsys, dir+"/topology/thread_siblings_list")
	if errors.Is(err, fs.ErrNotExist) {
		b, err = fs.ReadFile(fsys, dir+"/topology/core_cpus_list")
	}
	if err != nil {
		return nil
	}
	set, err := parseCPUList(string(b))
	if err != nil {
		return nil
	}
	return set
}

// groupCores groups one package's CPUs, sorted by number, into cores by
// their sibling sets rather than by core_id, which is unique only within a
// die: on a multi-die package two cores can share one. A CPU without a
// sibling set is a core of its own. The cores are sorted by ID, then by
// their lowest CPU.
func groupCores(cpus []logicalCPU) []ProcessorCore {
	index := map[string]int{}
	var cores []ProcessorCore
	for _, c := range cpus {
		key := fmt.Sprint(c.siblings)
		if len(c.siblings) == 0 {
			key = "cpu" + strconv.Itoa(c.n)
		}
		i, ok := index[key]
		if !ok {
			i = len(cores)
			index[key] = i
			cores = append(cores, ProcessorCore{ID: c.core})
		}
		cores[i].LogicalProcessors = append(cores[i].LogicalProcessors, c.n)
		cores[i].TotalHardwareThreads++
	}
	slices.SortFunc(cores, func(a, b ProcessorCore) int {
		return cmp.Or(cmp.Compare(a.ID, b.ID), cmp.Compare(a.LogicalProcessors[0], b.LogicalProcessors[0]))
	})
	return cores
}

// offlineCPUs lists the CPUs that are present but not online. possible is
// not used: a VM declares room for vCPUs it may hot-add, which are not
// offline but absent. It is nil when there are none or when either list
// cannot be read or parsed.
func offlineCPUs(fsys fs.FS) []int {
	present, err := readCPUList(fsys, cpuSysDir+"/present")
	if err != nil {
		return nil
	}
	online, err := readCPUList(fsys, cpuSysDir+"/online")
	if err != nil {
		return nil
	}
	var offline []int
	for _, n := range present {
		if _, found := slices.BinarySearch(online, n); !found {
			offline = append(offline, n)
		}
	}
	return offline
}

func readCPUList(fsys fs.FS, name string) ([]int, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	return parseCPUList(string(b))
}

// maxCPUNumber and maxCPURange bound a CPU list, well above any kernel's
// NR_CPUS, so that a corrupt present or online file cannot make
// parseCPUList exhaust memory.
const (
	maxCPUNumber = 1 << 20
	maxCPURange  = 1 << 16
)

// parseCPUList parses a kernel CPU list such as 0-3,8-11, 5 or an empty
// one into its CPUs, sorted and without duplicates. A range wider than
// maxCPURange, a number above maxCPUNumber or a list of more CPUs than
// there are numbers is malformed.
func parseCPUList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	cpus := []int{}
	if s == "" {
		return cpus, nil
	}
	for part := range strings.SplitSeq(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		first, err := parseCPUNumber(lo)
		if err != nil {
			return nil, fmt.Errorf("CPU list %q: %w", s, err)
		}
		last := first
		if isRange {
			if last, err = parseCPUNumber(hi); err != nil {
				return nil, fmt.Errorf("CPU list %q: %w", s, err)
			}
			if last < first {
				return nil, fmt.Errorf("CPU list %q: range %s ends before it starts", s, part)
			}
			if last-first >= maxCPURange {
				return nil, fmt.Errorf("CPU list %q: range %s is wider than %d CPUs", s, part, maxCPURange)
			}
		}
		if len(cpus)+last-first >= maxCPUNumber+1 {
			return nil, fmt.Errorf("CPU list %q: more than %d CPUs", s, maxCPUNumber+1)
		}
		for n := first; n <= last; n++ {
			cpus = append(cpus, n)
		}
	}
	slices.Sort(cpus)
	return slices.Compact(cpus), nil
}

// parseCPUNumber parses a CPU number: decimal digits only, so that no sign
// or blank slips through strconv.Atoi, and no more than maxCPUNumber.
func parseCPUNumber(s string) (int, error) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, fmt.Errorf("invalid CPU number %q", s)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if n > maxCPUNumber {
		return 0, fmt.Errorf("CPU number %s is above %d", s, maxCPUNumber)
	}
	return n, nil
}

// ReportsHypervisorFlag reports whether rootFS's /proc/cpuinfo lists the
// CPU capabilities as x86 flags, the only cpuinfo field that carries a
// hypervisor bit when the kernel runs as a guest. arm64 lists Features,
// which never do, so a missing bit there says nothing about the host. It
// reads the file the cpu prober parses rather than going by the agent's
// GOARCH, which under user-mode emulation can differ from the host's. A
// missing or unreadable cpuinfo reports false.
func ReportsHypervisorFlag(rootFS fs.FS) bool {
	info, err := readCPUInfo(rootFS)
	if err != nil {
		return false
	}
	for _, fields := range info {
		if _, ok := fields["flags"]; ok {
			return true
		}
	}
	return false
}

// readCPUInfo reads /proc/cpuinfo into its blocks' fields by CPU number.
func readCPUInfo(fsys fs.FS) (map[int]map[string]string, error) {
	b, err := fs.ReadFile(fsys, cpuInfoPath)
	if err != nil {
		return nil, err
	}
	return parseCPUInfo(string(b)), nil
}

// parseCPUInfo splits cpuinfo into blocks, each a run of key: value lines
// ended by a blank line, and keys each by its processor field. A block
// without one, such as the Hardware and Revision lines some arm kernels
// append, is dropped.
func parseCPUInfo(s string) map[int]map[string]string {
	blocks := map[int]map[string]string{}
	fields := map[string]string{}
	flush := func() {
		if n, err := parseCPUNumber(fields["processor"]); err == nil {
			blocks[n] = fields
		}
		fields = map[string]string{}
	}
	for line := range strings.Lines(s) {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if key, value, ok := strings.Cut(line, ":"); ok {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	flush()
	return blocks
}

// setCPUInfo fills p's cpuinfo fields from the block of the lowest-numbered
// of its members, sorted by number, that has one, so that each package
// reports its own model and flags rather than another package's. infoErr
// is readCPUInfo's error: a missing cpuinfo leaves the fields out, an
// unreadable one makes Vendor and Model "unknown".
func setCPUInfo(p *Processor, info map[int]map[string]string, infoErr error, members []logicalCPU) {
	if errors.Is(infoErr, fs.ErrNotExist) {
		return
	}
	if infoErr != nil {
		p.Vendor, p.Model = unknown, unknown
		return
	}
	var f map[string]string
	for _, c := range members {
		if b, ok := info[c.n]; ok {
			f = b
			break
		}
	}
	if f == nil {
		return
	}
	p.Vendor, p.Model, p.Microcode = f["vendor_id"], f["model name"], f["microcode"]
	// arm64 names neither vendor nor model, only the implementer and part
	// codes they derive from.
	if implementer, ok := f["CPU implementer"]; ok {
		if p.Vendor == "" {
			p.Vendor = armVendor(implementer)
		}
		if part, ok := f["CPU part"]; ok && p.Model == "" {
			p.Model = armModel(implementer, part)
		}
	}
	flags, ok := f["flags"]
	if !ok {
		flags = f["Features"]
	}
	if caps := strings.Fields(flags); len(caps) > 0 {
		p.Capabilities = caps
	}
}

// armVendors names the arm64 CPU implementer codes.
var armVendors = map[string]string{
	"0x41": "ARM", "0x42": "Broadcom", "0x43": "Cavium", "0x46": "Fujitsu", "0x48": "HiSilicon",
	"0x4e": "NVIDIA", "0x50": "APM", "0x51": "Qualcomm", "0x61": "Apple", "0xc0": "Ampere",
}

// armModels names the server cores by implementer and part code.
var armModels = map[[2]string]string{
	{"0x41", "0xd0c"}: "Neoverse-N1", {"0x41", "0xd40"}: "Neoverse-V1", {"0x41", "0xd49"}: "Neoverse-N2",
	{"0x41", "0xd4f"}: "Neoverse-V2", {"0x41", "0xd8e"}: "Neoverse-N3", {"0x41", "0xd84"}: "Neoverse-V3",
	{"0xc0", "0xac3"}: "Ampere-1", {"0xc0", "0xac4"}: "Ampere-1a", {"0x48", "0xd01"}: "Kunpeng-920",
}

// armVendor names an arm64 CPU implementer, or returns its code as given
// when it is not one of armVendors.
func armVendor(implementer string) string {
	if v, ok := armVendors[strings.ToLower(implementer)]; ok {
		return v
	}
	return implementer
}

// armModel names an arm64 core by its implementer and part, or returns
// "<implementer>:<part>" for one not in armModels, so the codes are kept
// for a reader with a fuller table.
func armModel(implementer, part string) string {
	if m, ok := armModels[[2]string{strings.ToLower(implementer), strings.ToLower(part)}]; ok {
		return m
	}
	return implementer + ":" + part
}

package inventory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
)

// Memory is the memory payload. The fields it shares with each topology
// node's memory have the same names and meanings there; InstalledBytes, the
// module details and ModulesError have no per-node counterpart.
type Memory struct {
	// TotalPhysicalBytes is the memory the kernel has online: its memory
	// blocks, or TotalUsableBytes where the kernel does not expose them.
	// It leaves out firmware-reserved ranges, so it is usually less than
	// InstalledBytes.
	TotalPhysicalBytes int64 `json:"total_physical_bytes"`
	// TotalUsableBytes is MemTotal from /proc/meminfo.
	TotalUsableBytes int64 `json:"total_usable_bytes"`
	// InstalledBytes is the sum of the installed modules' sizes, or 0 when
	// the modules could not be read.
	InstalledBytes        int64                      `json:"installed_bytes"`
	SupportedPageSizes    []uint64                   `json:"supported_page_sizes"`
	DefaultHugePageSize   uint64                     `json:"default_huge_page_size"`
	TotalHugePageBytes    int64                      `json:"total_huge_page_bytes"`
	HugePageAmountsBySize map[uint64]HugePageAmounts `json:"huge_page_amounts_by_size"`
	// Modules lists every memory slot the firmware describes, in its order,
	// or is null when the SMBIOS table could not be read, with
	// ModulesError saying why.
	Modules      []MemoryModule `json:"modules"`
	ModulesError string         `json:"modules_error,omitempty"`
}

// HugePageAmounts counts the huge pages of one size.
type HugePageAmounts struct {
	Total    int64 `json:"total"`
	Free     int64 `json:"free"`
	Surplus  int64 `json:"surplus"`
	Reserved int64 `json:"reserved"`
}

// NewMemoryProber reads the memory totals and huge pages from rootFS, which
// is the host root, and the modules from the SMBIOS table in privileged, the
// helper's file system. The table is root-only, so without the helper
// (privileged nil) or with a table that cannot be read or decoded, the
// probe still succeeds: the totals are still worth serving, and
// ModulesError reports what went wrong. A table that exists but cannot be
// read or decoded is also logged as a warning; a host without one is an
// expected state and is not. Only a missing MemTotal fails the probe.
func NewMemoryProber(rootFS, privileged fs.FS, log *slog.Logger) Prober {
	return ProberFunc(func(context.Context) (any, error) {
		meminfo, err := fs.ReadFile(rootFS, "proc/meminfo")
		if err != nil {
			return nil, fmt.Errorf("reading /proc/meminfo: %w", err)
		}
		fields := parseMeminfo(meminfo)
		total, ok := fields["MemTotal"]
		if !ok {
			return nil, errors.New("/proc/meminfo has no MemTotal")
		}

		m := &Memory{
			TotalUsableBytes:    total,
			DefaultHugePageSize: uint64(fields["Hugepagesize"]),
			TotalHugePageBytes:  fields["Hugetlb"],
		}
		m.TotalPhysicalBytes = onlineMemoryBytes(rootFS, memorySysDir)
		if m.TotalPhysicalBytes <= 0 {
			m.TotalPhysicalBytes = total
		}
		m.SupportedPageSizes, m.HugePageAmountsBySize = hugePages(rootFS, hugePagesSysDir)

		modules, err := readModules(privileged)
		if err != nil {
			m.ModulesError = err.Error()
			if !errors.Is(err, errNoSMBIOSTable) {
				log.Warn("memory modules not reported", "error", err)
			}
			return m, nil
		}
		m.Modules = modules
		for _, mod := range modules {
			m.InstalledBytes += mod.SizeBytes
		}
		return m, nil
	})
}

// errNoSMBIOSTable marks the expected cases of no table to read.
var errNoSMBIOSTable = errors.New("no SMBIOS table")

func readModules(privileged fs.FS) ([]MemoryModule, error) {
	table, err := readSMBIOSTable(privileged)
	if err != nil {
		return nil, err
	}
	modules, err := memoryModules(table)
	if err != nil {
		return nil, fmt.Errorf("decoding the SMBIOS table: %w", err)
	}
	return modules, nil
}

// parseMeminfo reads /proc/meminfo's "Key:   value [kB]" lines, in bytes.
func parseMeminfo(b []byte) map[string]int64 {
	out := map[string]int64{}
	for line := range strings.SplitSeq(string(b), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && fields[1] == "kB" {
			v *= 1024
		}
		out[strings.TrimSpace(key)] = v
	}
	return out
}

// memorySysDir has the kernel's memory blocks, memory<M>, and their size.
const memorySysDir = "sys/devices/system/memory"

// hugePagesSysDir has the system's huge pages, a hugepages-<kB>kB directory
// per size.
const hugePagesSysDir = "sys/kernel/mm/hugepages"

// onlineMemoryBytes counts the online memory blocks among dir's memory<M>
// entries: memorySysDir for the system, or a NUMA node's directory for its
// own. Blocks have one size, memorySysDir's block_size_bytes, wherever they
// are listed. It returns 0 where dir or the block size is absent or
// unreadable.
func onlineMemoryBytes(rootFS fs.FS, dir string) int64 {
	b, err := fs.ReadFile(rootFS, memorySysDir+"/block_size_bytes")
	if err != nil {
		return 0
	}
	blockSize, err := strconv.ParseInt(strings.TrimSpace(string(b)), 16, 64)
	if err != nil {
		return 0
	}
	entries, err := fs.ReadDir(rootFS, dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "memory") {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "memory")); err != nil {
			continue
		}
		state, err := fs.ReadFile(rootFS, dir+"/"+e.Name()+"/state")
		if err != nil || strings.TrimSpace(string(state)) != "online" {
			continue
		}
		total += blockSize
	}
	return total
}

// hugePages lists the huge page sizes under dir, hugePagesSysDir for the
// system or a NUMA node's hugepages directory for its own, in directory
// order, and their counts. A size whose counts cannot be read is still
// listed, with zero counts. Reserved is read from resv_hugepages, which
// only the system's sizes have: the kernel does not reserve per node.
func hugePages(rootFS fs.FS, dir string) ([]uint64, map[uint64]HugePageAmounts) {
	entries, err := fs.ReadDir(rootFS, dir)
	if err != nil {
		return nil, nil
	}
	var sizes []uint64
	amounts := map[uint64]HugePageAmounts{}
	for _, e := range entries {
		kb, ok := strings.CutPrefix(e.Name(), "hugepages-")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(kb, "kB"), 10, 64)
		if err != nil || !strings.HasSuffix(kb, "kB") {
			continue
		}
		size := n * 1024
		sizes = append(sizes, size)
		count := func(name string) int64 {
			b, err := fs.ReadFile(rootFS, dir+"/"+e.Name()+"/"+name)
			if err != nil {
				return 0
			}
			v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
			if err != nil {
				return 0
			}
			return v
		}
		amounts[size] = HugePageAmounts{
			Total:    count("nr_hugepages"),
			Free:     count("free_hugepages"),
			Surplus:  count("surplus_hugepages"),
			Reserved: count("resv_hugepages"),
		}
	}
	return sizes, amounts
}

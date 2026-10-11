package inventory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math/bits"
	"slices"
	"strconv"
	"strings"
)

// nodeSysDir has a directory per NUMA node, node<N>. A kernel built
// without NUMA support has none.
const nodeSysDir = "sys/devices/system/node"

// localDistance is the kernel's distance from a node to itself, which is
// what numactl shows for the one node of a host without NUMA.
const localDistance = 10

// Topology is the topology payload: the host's NUMA nodes with the cores,
// caches and memory of each. Its cores and memory follow the cpu and memory
// payloads.
type Topology struct {
	// Architecture is "smp" for a host of one node and "numa" for more.
	Architecture string `json:"architecture"`
	// Nodes are sorted by ID, never empty: a host without NUMA is one node.
	Nodes []TopologyNode `json:"nodes"`
}

// TopologyNode is one NUMA node. A node without CPUs, such as one of CXL or
// HBM memory only, has empty Cores and Caches.
type TopologyNode struct {
	ID int `json:"id"`
	// Cores are the cpu payload's cores, grouped the same way, restricted
	// to the node's online CPUs and in the same order: by package, then by
	// core.
	Cores  []ProcessorCore `json:"cores"`
	Caches []Cache         `json:"caches"`
	// Distances[i] is the distance from this node to Nodes[i]. It is
	// omitted when the kernel's distance file cannot be read or parsed, or
	// does not have one entry per node.
	Distances []int      `json:"distances,omitempty"`
	Memory    NodeMemory `json:"memory"`
}

// Cache is one CPU cache as the node's online CPUs report it.
type Cache struct {
	// Level is 0 when the kernel's level file cannot be read.
	Level int `json:"level"`
	// Type is "data", "instruction" or "unified", or "unknown" when the
	// kernel's type file cannot be read.
	Type string `json:"type"`
	// SizeBytes is 0 when the kernel's size cannot be read or parsed.
	SizeBytes uint64 `json:"size_bytes"`
	// LogicalProcessors are the node's online CPUs that share this cache,
	// sorted.
	LogicalProcessors []int `json:"logical_processors"`
}

// NodeMemory is a node's memory. Its fields keep the names of the memory
// payload's fields of the same meaning, counted for the node alone.
type NodeMemory struct {
	// TotalPhysicalBytes is the node's online memory blocks, or
	// TotalUsableBytes where the kernel does not expose them.
	TotalPhysicalBytes int64 `json:"total_physical_bytes"`
	// TotalUsableBytes is the node's MemTotal, or 0 when its meminfo
	// cannot be read.
	TotalUsableBytes int64 `json:"total_usable_bytes"`
	// SupportedPageSizes are the node's huge page sizes, ascending.
	SupportedPageSizes    []uint64                 `json:"supported_page_sizes,omitempty"`
	TotalHugePageBytes    int64                    `json:"total_huge_page_bytes"`
	HugePageAmountsBySize map[uint64]NodeHugePages `json:"huge_page_amounts_by_size,omitempty"`
}

// NodeHugePages counts a node's huge pages of one size. It has no reserved
// count: the kernel reserves huge pages system-wide, not per node.
type NodeHugePages struct {
	Total   int64 `json:"total"`
	Free    int64 `json:"free"`
	Surplus int64 `json:"surplus"`
}

// NewTopologyProber reports the NUMA topology of rootFS, the host root,
// from sysfs and, on a host without NUMA, /proc/meminfo. A host whose
// sys/devices/system/cpu cannot be listed or lists no online CPU, or whose
// sys/devices/system/node exists but cannot be listed, fails the probe, as
// cpu does: a payload without the CPUs or with one node in place of several
// would misdescribe the host, and a failed probe keeps the last good one.
func NewTopologyProber(rootFS fs.FS) Prober {
	return ProberFunc(func(context.Context) (any, error) { return readTopology(rootFS) })
}

func readTopology(fsys fs.FS) (*Topology, error) {
	cpus, err := readLogicalCPUs(fsys)
	if err != nil {
		return nil, err
	}
	if len(cpus) == 0 {
		return nil, fmt.Errorf("no online CPU in %s", cpuSysDir)
	}
	nodes, err := readNodes(fsys, cpus)
	if err != nil {
		return nil, err
	}
	arch := "smp"
	if len(nodes) > 1 {
		arch = "numa"
	}
	return &Topology{Architecture: arch, Nodes: nodes}, nil
}

// readNodes reads the NUMA nodes, sorted by ID, given the online CPUs. A
// host without nodeSysDir, or with no node in it, is one node holding every
// online CPU and the system's memory.
func readNodes(fsys fs.FS, cpus []logicalCPU) ([]TopologyNode, error) {
	entries, err := fs.ReadDir(fsys, nodeSysDir)
	if errors.Is(err, fs.ErrNotExist) {
		return []TopologyNode{systemNode(fsys, cpus)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing NUMA nodes: %w", err)
	}
	var ids []int
	for _, e := range entries {
		// Node numbers are written as CPU numbers are.
		if digits, ok := strings.CutPrefix(e.Name(), "node"); ok {
			if id, err := parseCPUNumber(digits); err == nil {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return []TopologyNode{systemNode(fsys, cpus)}, nil
	}
	// ReadDir sorts by name, which puts node10 before node2; the kernel's
	// distance lists are in ID order.
	slices.Sort(ids)
	nodes := make([]TopologyNode, 0, len(ids))
	for _, id := range ids {
		dir := fmt.Sprintf("%s/node%d", nodeSysDir, id)
		n := newNode(fsys, id, nodeCPUs(fsys, dir, cpus))
		n.Distances = readDistances(fsys, dir+"/distance")
		// The kernel writes one distance per online node when the file is
		// read; a node onlined or removed since the listing leaves a row
		// that does not line up with nodes.
		if len(n.Distances) != len(ids) {
			n.Distances = nil
		}
		n.Memory = readNodeMemory(fsys, dir+"/meminfo", fmt.Sprintf("Node %d MemTotal", id), dir, dir+"/hugepages")
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// systemNode is the one node of a host without NUMA: every online CPU, the
// local distance and the system's memory.
func systemNode(fsys fs.FS, cpus []logicalCPU) TopologyNode {
	n := newNode(fsys, 0, cpus)
	n.Distances = []int{localDistance}
	n.Memory = readNodeMemory(fsys, "proc/meminfo", "MemTotal", memorySysDir, hugePagesSysDir)
	return n
}

// newNode is node id with the cores and caches of its online CPUs, cpus,
// sorted by number. Cores and caches are empty rather than null for a node
// without CPUs.
func newNode(fsys fs.FS, id int, cpus []logicalCPU) TopologyNode {
	// groupCores groups one package's CPUs, so group each package's and
	// list them in package order, as readCPU does.
	byPackage := map[int][]logicalCPU{}
	for _, c := range cpus {
		byPackage[c.pkg] = append(byPackage[c.pkg], c)
	}
	cores := []ProcessorCore{}
	for _, pkg := range slices.Sorted(maps.Keys(byPackage)) {
		cores = append(cores, groupCores(byPackage[pkg])...)
	}
	return TopologyNode{ID: id, Cores: cores, Caches: readCaches(fsys, cpus)}
}

// nodeCPUs returns those of the online CPUs, cpus, that belong to the node
// at dir, by its cpulist, or by its cpu<N> entries when the list is missing
// or cannot be read or parsed. An empty list is a node without CPUs.
func nodeCPUs(fsys fs.FS, dir string, cpus []logicalCPU) []logicalCPU {
	members, err := readCPUList(fsys, dir+"/cpulist")
	if err != nil {
		members = nil
		entries, _ := fs.ReadDir(fsys, dir)
		for _, e := range entries {
			if digits, ok := strings.CutPrefix(e.Name(), "cpu"); ok {
				if n, err := parseCPUNumber(digits); err == nil {
					members = append(members, n)
				}
			}
		}
		slices.Sort(members)
	}
	var out []logicalCPU
	for _, c := range cpus {
		if _, found := slices.BinarySearch(members, c.n); found {
			out = append(out, c)
		}
	}
	return out
}

// cacheTypeOrder sorts the cache types the kernel writes; a type it does
// not list sorts after them.
var cacheTypeOrder = map[string]int{"data": 0, "instruction": 1, "unified": 2, "unknown": 3}

// readCaches reads the caches of cpus, which are sorted by number. A cache
// that several of the CPUs share is listed once, with all of them. The
// caches are sorted by level, then type, then first CPU. A CPU without a
// cache directory, as in many VMs, adds none.
func readCaches(fsys fs.FS, cpus []logicalCPU) []Cache {
	index := map[string]int{}
	caches := []Cache{}
	for _, c := range cpus {
		dir := fmt.Sprintf("%s/cpu%d/cache", cpuSysDir, c.n)
		entries, _ := fs.ReadDir(fsys, dir)
		for _, e := range entries {
			digits, ok := strings.CutPrefix(e.Name(), "index")
			if !ok {
				continue
			}
			if _, err := parseCPUNumber(digits); err != nil {
				continue
			}
			key, cache := readCache(fsys, dir+"/"+e.Name())
			i, ok := index[key]
			if !ok {
				i = len(caches)
				index[key] = i
				caches = append(caches, cache)
			}
			caches[i].LogicalProcessors = append(caches[i].LogicalProcessors, c.n)
		}
	}
	slices.SortStableFunc(caches, func(a, b Cache) int {
		return cmp.Or(
			cmp.Compare(a.Level, b.Level),
			cmp.Compare(cacheTypeRank(a.Type), cacheTypeRank(b.Type)),
			cmp.Compare(a.Type, b.Type),
			cmp.Compare(a.LogicalProcessors[0], b.LogicalProcessors[0]),
		)
	})
	return caches
}

func cacheTypeRank(t string) int {
	if r, ok := cacheTypeOrder[t]; ok {
		return r
	}
	return len(cacheTypeOrder)
}

// readCache reads the cache at dir, a cpu<N>/cache/index<M> directory,
// without its CPUs, and the key that identifies it among the caches of
// other CPUs: its level, type and id, or, where the kernel gives no id,
// its shared_cpu_list. Each cache's size is its own index's, not
// index<level>/size, which would give the L1 data cache the L1 instruction
// cache's size.
func readCache(fsys fs.FS, dir string) (string, Cache) {
	read := func(name string) (string, error) {
		b, err := fs.ReadFile(fsys, dir+"/"+name)
		return strings.TrimSpace(string(b)), err
	}
	var c Cache
	if s, err := read("level"); err == nil {
		c.Level, _ = strconv.Atoi(s)
	}
	c.Type = "unknown"
	if s, err := read("type"); err == nil {
		c.Type = strings.ToLower(s)
	}
	if s, err := read("size"); err == nil {
		c.SizeBytes = parseCacheSize(s)
	}
	if s, err := read("id"); err == nil {
		if id, err := strconv.Atoi(s); err == nil {
			return fmt.Sprintf("%d/%s/id %d", c.Level, c.Type, id), c
		}
	}
	shared, _ := read("shared_cpu_list")
	return fmt.Sprintf("%d/%s/cpus %s", c.Level, c.Type, shared), c
}

// parseCacheSize parses a cache size as the kernel writes it, such as 48K:
// a decimal number with a K, M or G suffix for binary multiples, or bytes
// without one. Anything else, or a size that overflows, is 0.
func parseCacheSize(s string) uint64 {
	s = strings.TrimSpace(s)
	shift := 0
	switch {
	case strings.HasSuffix(s, "K"):
		shift = 10
	case strings.HasSuffix(s, "M"):
		shift = 20
	case strings.HasSuffix(s, "G"):
		shift = 30
	}
	if shift > 0 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || bits.LeadingZeros64(n) < shift {
		return 0
	}
	return n << shift
}

// readDistances reads a node's distance file, the distances to every
// online node in ID order. It is nil when the file cannot be read or holds
// anything but integers.
func readDistances(fsys fs.FS, name string) []int {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil
	}
	var out []int
	for f := range strings.FieldsSeq(string(b)) {
		d, err := strconv.Atoi(f)
		if err != nil {
			return nil
		}
		out = append(out, d)
	}
	return out
}

// readNodeMemory reads a node's memory: memTotalKey of the meminfo file,
// the online memory blocks listed in blocksDir and the huge pages under
// hugePagesDir. A node's huge pages are its own, counted from its own
// pages, not the system's Hugetlb.
func readNodeMemory(fsys fs.FS, meminfo, memTotalKey, blocksDir, hugePagesDir string) NodeMemory {
	var m NodeMemory
	if b, err := fs.ReadFile(fsys, meminfo); err == nil {
		m.TotalUsableBytes = parseMeminfo(b)[memTotalKey]
	}
	m.TotalPhysicalBytes = onlineMemoryBytes(fsys, blocksDir)
	if m.TotalPhysicalBytes <= 0 {
		m.TotalPhysicalBytes = m.TotalUsableBytes
	}
	sizes, amounts := hugePages(fsys, hugePagesDir)
	if len(sizes) == 0 {
		return m
	}
	slices.Sort(sizes)
	m.SupportedPageSizes = sizes
	m.HugePageAmountsBySize = make(map[uint64]NodeHugePages, len(amounts))
	for size, a := range amounts {
		m.HugePageAmountsBySize[size] = NodeHugePages{Total: a.Total, Free: a.Free, Surplus: a.Surplus}
		m.TotalHugePageBytes += a.Total * int64(size)
	}
	return m
}

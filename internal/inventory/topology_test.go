package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// cache adds index m of CPU n's cache directory. An empty id leaves the id
// file out, as kernels without cache ids do.
func (h cpuHost) cache(n, m, level int, typ, size, id, shared string) {
	dir := fmt.Sprintf("%s/cpu%d/cache/index%d/", cpuSysDir, n, m)
	h.file(dir+"level", fmt.Sprintf("%d\n", level))
	h.file(dir+"type", typ+"\n")
	h.file(dir+"size", size+"\n")
	h.file(dir+"shared_cpu_list", shared+"\n")
	if id != "" {
		h.file(dir+"id", id+"\n")
	}
}

// coreCaches gives CPU n the caches of a server core: a 48K L1 data, a 32K
// L1 instruction and a 2M L2 cache, each shared by the core's siblings and
// identified by core, and a 32M L3 cache shared by l3CPUs and identified by
// the package.
func (h cpuHost) coreCaches(n, core, pkg int, siblings, l3CPUs string) {
	id := fmt.Sprint(core)
	h.cache(n, 0, 1, "Data", "48K", id, siblings)
	h.cache(n, 1, 1, "Instruction", "32K", id, siblings)
	h.cache(n, 2, 2, "Unified", "2048K", id, siblings)
	h.cache(n, 3, 3, "Unified", "32768K", fmt.Sprint(pkg), l3CPUs)
}

// node adds NUMA node id with its cpulist, its distance line and the
// MemTotal of its meminfo, in kB.
func (h cpuHost) node(id int, cpulist, distance string, memTotalKB int64) {
	dir := fmt.Sprintf("%s/node%d/", nodeSysDir, id)
	h.file(dir+"cpulist", cpulist+"\n")
	if distance != "" {
		h.file(dir+"distance", distance+"\n")
	}
	h.file(dir+"meminfo", fmt.Sprintf("Node %d MemTotal:       %d kB\nNode %d MemFree:        %d kB\n", id, memTotalKB, id, memTotalKB/2))
}

// memoryBlocks gives dir, a node's directory or the system's memory
// directory, the memory blocks first.., each online or not.
func (h cpuHost) memoryBlocks(dir string, first int, online ...bool) {
	for i, on := range online {
		state := "offline"
		if on {
			state = "online"
		}
		h.file(fmt.Sprintf("%s/memory%d/state", dir, first+i), state+"\n")
	}
}

// hugePageSize gives dir, a hugepages directory, a size in kB with its
// counts.
func (h cpuHost) hugePageSize(dir string, kB, nr, free, surplus int) {
	sub := fmt.Sprintf("%s/hugepages-%dkB/", dir, kB)
	h.file(sub+"nr_hugepages", fmt.Sprintf("%d\n", nr))
	h.file(sub+"free_hugepages", fmt.Sprintf("%d\n", free))
	h.file(sub+"surplus_hugepages", fmt.Sprintf("%d\n", surplus))
}

// memBlock is the fixtures' memory block size, 128 MiB.
const memBlock = 128 << 20

// twoNodeHost is a 2-socket host with a NUMA node per socket: node 0 holds
// package 0's cpus 0-3 and node 1 package 1's cpus 4-7, two cores of two
// threads each, with asymmetric distances. Each node has 1 GiB of MemTotal
// (2 GiB for node 1), two online and one offline memory block and four of
// its own 2 MiB huge pages, two of them free.
func twoNodeHost() cpuHost {
	h := cpuHost{}
	for n := range 8 {
		pkg, core := n/4, (n/2)%2
		lo := n &^ 1
		siblings := fmt.Sprintf("%d-%d", lo, lo+1)
		l3 := fmt.Sprintf("%d-%d", pkg*4, pkg*4+3)
		h.cpu(n, pkg, core, siblings)
		h.coreCaches(n, n/2, pkg, siblings, l3)
		if n > 0 {
			h.file(fmt.Sprintf("%s/cpu%d/online", cpuSysDir, n), "1\n")
		}
	}
	h.file(cpuSysDir+"/online", "0-7\n")
	h.file(nodeSysDir+"/online", "0-1\n")
	h.file(nodeSysDir+"/possible", "0-1\n")
	h.node(0, "0-3", "10 21", 1<<20)
	h.node(1, "4-7", "31 10", 2<<20)
	h.file("sys/devices/system/memory/block_size_bytes", "8000000\n")
	h.memoryBlocks(nodeSysDir+"/node0", 0, true, true, false)
	h.memoryBlocks(nodeSysDir+"/node1", 3, true, true, false)
	for id := range 2 {
		hp := fmt.Sprintf("%s/node%d/hugepages", nodeSysDir, id)
		h.hugePageSize(hp, 2048, 4, 2, 0)
		h.hugePageSize(hp, 1048576, 0, 0, 0)
	}
	// The system-wide huge pages, which no node must report as its own.
	h.hugePageSize("sys/kernel/mm/hugepages", 2048, 8, 4, 0)
	h.hugePageSize("sys/kernel/mm/hugepages", 1048576, 0, 0, 0)
	h.file("proc/meminfo", "MemTotal:        3145728 kB\nHugetlb:           16384 kB\n")
	return h
}

// serverCaches are the caches twoNodeHost gives a node of two cores, the
// first core's threads first.
func serverCaches(cpus ...int) []Cache {
	a, b := cpus[:2], cpus[2:]
	return []Cache{
		{Level: 1, Type: "data", SizeBytes: 48 << 10, LogicalProcessors: a},
		{Level: 1, Type: "data", SizeBytes: 48 << 10, LogicalProcessors: b},
		{Level: 1, Type: "instruction", SizeBytes: 32 << 10, LogicalProcessors: a},
		{Level: 1, Type: "instruction", SizeBytes: 32 << 10, LogicalProcessors: b},
		{Level: 2, Type: "unified", SizeBytes: 2 << 20, LogicalProcessors: a},
		{Level: 2, Type: "unified", SizeBytes: 2 << 20, LogicalProcessors: b},
		{Level: 3, Type: "unified", SizeBytes: 32 << 20, LogicalProcessors: cpus},
	}
}

// nodeHugePages is twoNodeHost's huge pages of one node.
func nodeHugePages() (sizes []uint64, total int64, amounts map[uint64]NodeHugePages) {
	return []uint64{2 << 20, 1 << 30}, 4 * 2 << 20, map[uint64]NodeHugePages{
		2 << 20: {Total: 4, Free: 2},
		1 << 30: {},
	}
}

func twoNodeWant() *Topology {
	sizes, total, amounts := nodeHugePages()
	return &Topology{
		Architecture: "numa",
		Nodes: []TopologyNode{
			{
				ID: 0,
				Cores: []ProcessorCore{
					{ID: 0, TotalHardwareThreads: 2, LogicalProcessors: []int{0, 1}},
					{ID: 1, TotalHardwareThreads: 2, LogicalProcessors: []int{2, 3}},
				},
				Caches:    serverCaches(0, 1, 2, 3),
				Distances: []int{10, 21},
				Memory: NodeMemory{
					TotalPhysicalBytes: 2 * memBlock, TotalUsableBytes: 1 << 30,
					SupportedPageSizes: sizes, TotalHugePageBytes: total, HugePageAmountsBySize: amounts,
				},
			},
			{
				ID: 1,
				Cores: []ProcessorCore{
					{ID: 0, TotalHardwareThreads: 2, LogicalProcessors: []int{4, 5}},
					{ID: 1, TotalHardwareThreads: 2, LogicalProcessors: []int{6, 7}},
				},
				Caches:    serverCaches(4, 5, 6, 7),
				Distances: []int{31, 10},
				Memory: NodeMemory{
					TotalPhysicalBytes: 2 * memBlock, TotalUsableBytes: 2 << 30,
					SupportedPageSizes: sizes, TotalHugePageBytes: total, HugePageAmountsBySize: amounts,
				},
			},
		},
	}
}

func probeTopology(t *testing.T, fsys fs.FS) *Topology {
	t.Helper()
	v, err := NewTopologyProber(fsys).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	top, ok := v.(*Topology)
	if !ok {
		t.Fatalf("Probe returned %T, want *Topology", v)
	}
	return top
}

func checkTopology(t *testing.T, got, want *Topology) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		w, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("topology =\n%s\nwant\n%s", g, w)
	}
}

func TestTopologyProber(t *testing.T) {
	t.Run("two NUMA nodes", func(t *testing.T) {
		checkTopology(t, probeTopology(t, fstest.MapFS(twoNodeHost())), twoNodeWant())
	})

	t.Run("single node", func(t *testing.T) {
		h := cpuHost{}
		for n := range 2 {
			h.cpu(n, 0, n, fmt.Sprint(n))
		}
		h.node(0, "0-1", "10", 1<<20)
		got := probeTopology(t, fstest.MapFS(h))
		checkTopology(t, got, &Topology{
			Architecture: "smp",
			Nodes: []TopologyNode{{
				ID: 0,
				Cores: []ProcessorCore{
					{ID: 0, TotalHardwareThreads: 1, LogicalProcessors: []int{0}},
					{ID: 1, TotalHardwareThreads: 1, LogicalProcessors: []int{1}},
				},
				Caches:    []Cache{},
				Distances: []int{10},
				Memory:    NodeMemory{TotalPhysicalBytes: 1 << 30, TotalUsableBytes: 1 << 30},
			}},
		})
	})

	t.Run("no NUMA directory", func(t *testing.T) {
		h := twoNodeHost()
		for name := range h {
			if strings.HasPrefix(name, nodeSysDir+"/") {
				delete(h, name)
			}
		}
		h.memoryBlocks("sys/devices/system/memory", 0, true, true, true, false)
		// The system's reserved huge pages are not a node's to report.
		h.file("sys/kernel/mm/hugepages/hugepages-2048kB/resv_hugepages", "1\n")
		got := probeTopology(t, fstest.MapFS(h))
		want := twoNodeWant()
		n0, n1 := want.Nodes[0], want.Nodes[1]
		checkTopology(t, got, &Topology{
			Architecture: "smp",
			Nodes: []TopologyNode{{
				ID: 0,
				// In package order, then core order, as cpu lists them.
				Cores: []ProcessorCore{n0.Cores[0], n0.Cores[1], n1.Cores[0], n1.Cores[1]},
				// Sorted by level, type, then first CPU.
				Caches: []Cache{
					n0.Caches[0], n0.Caches[1], n1.Caches[0], n1.Caches[1],
					n0.Caches[2], n0.Caches[3], n1.Caches[2], n1.Caches[3],
					n0.Caches[4], n0.Caches[5], n1.Caches[4], n1.Caches[5],
					n0.Caches[6], n1.Caches[6],
				},
				Distances: []int{10},
				Memory: NodeMemory{
					TotalPhysicalBytes: 3 * memBlock, TotalUsableBytes: 3 << 30,
					SupportedPageSizes:    []uint64{2 << 20, 1 << 30},
					TotalHugePageBytes:    8 * 2 << 20,
					HugePageAmountsBySize: map[uint64]NodeHugePages{2 << 20: {Total: 8, Free: 4}, 1 << 30: {}},
				},
			}},
		})
	})

	t.Run("one node of two packages", func(t *testing.T) {
		h := twoNodeHost()
		for name := range h {
			if strings.HasPrefix(name, nodeSysDir+"/") {
				delete(h, name)
			}
		}
		h.node(0, "0-7", "10", 3<<20)
		got := probeTopology(t, fstest.MapFS(h))
		if len(got.Nodes) != 1 {
			t.Fatalf("%d nodes, want 1", len(got.Nodes))
		}
		c, err := readCPU(fstest.MapFS(h))
		if err != nil {
			t.Fatal(err)
		}
		var want []ProcessorCore
		for _, p := range c.Processors {
			want = append(want, p.Cores...)
		}
		if !reflect.DeepEqual(got.Nodes[0].Cores, want) {
			t.Errorf("node cores = %+v, want cpu's cores in order %+v", got.Nodes[0].Cores, want)
		}
	})

	t.Run("distance row of the wrong length", func(t *testing.T) {
		h := twoNodeHost()
		// A node onlined between listing the directories and reading.
		h.file(nodeSysDir+"/node1/distance", "31 10 20\n")
		got := probeTopology(t, fstest.MapFS(h))
		want := twoNodeWant()
		want.Nodes[1].Distances = nil
		checkTopology(t, got, want)
	})

	t.Run("offline CPU", func(t *testing.T) {
		h := twoNodeHost()
		// The kernel removes an offline CPU's topology; its cache files
		// are left here to show they are not read.
		for name := range h {
			if strings.HasPrefix(name, cpuSysDir+"/cpu3/topology/") {
				delete(h, name)
			}
		}
		h.file(cpuSysDir+"/cpu3/online", "0\n")
		got := probeTopology(t, fstest.MapFS(h))
		want := twoNodeWant()
		n0 := &want.Nodes[0]
		n0.Cores[1] = ProcessorCore{ID: 1, TotalHardwareThreads: 1, LogicalProcessors: []int{2}}
		for i := range n0.Caches {
			n0.Caches[i].LogicalProcessors = slices.DeleteFunc(slices.Clone(n0.Caches[i].LogicalProcessors), func(n int) bool { return n == 3 })
		}
		checkTopology(t, got, want)
	})

	t.Run("eleven nodes", func(t *testing.T) {
		h := cpuHost{}
		const nodes = 11
		for id := range nodes {
			h.cpu(id, id, 0, fmt.Sprint(id))
			var d []string
			for j := range nodes {
				d = append(d, fmt.Sprint(distance(id, j)))
			}
			h.node(id, fmt.Sprint(id), strings.Join(d, " "), 1<<20)
		}
		got := probeTopology(t, fstest.MapFS(h))
		if got.Architecture != "numa" || len(got.Nodes) != nodes {
			t.Fatalf("architecture %q with %d nodes, want numa with %d", got.Architecture, len(got.Nodes), nodes)
		}
		for i, n := range got.Nodes {
			if n.ID != i {
				t.Errorf("nodes[%d].id = %d", i, n.ID)
			}
			if len(n.Cores) != 1 || n.Cores[0].LogicalProcessors[0] != i {
				t.Errorf("nodes[%d].cores = %+v, want cpu %d", i, n.Cores, i)
			}
			for j, d := range n.Distances {
				if d != distance(n.ID, got.Nodes[j].ID) {
					t.Errorf("nodes[%d].distances[%d] = %d, want the distance from node %d to node %d, %d", i, j, d, n.ID, got.Nodes[j].ID, distance(n.ID, got.Nodes[j].ID))
				}
			}
		}
	})

	t.Run("multi-die package", func(t *testing.T) {
		// core_id is unique only within a die: two dies of one package
		// each have a core 0 and a core 1, with two threads each.
		h := cpuHost{}
		for n := range 8 {
			h.cpu(n, 0, n%2, fmt.Sprintf("%d,%d", n%4, n%4+4))
		}
		h.node(0, "0-7", "10", 1<<20)
		got := probeTopology(t, fstest.MapFS(h))
		cpu := probeCPUHost(t, fstest.MapFS(h))
		if !reflect.DeepEqual(got.Nodes[0].Cores, cpu.Processors[0].Cores) {
			t.Errorf("node cores = %+v\nwant the cpu payload's %+v", got.Nodes[0].Cores, cpu.Processors[0].Cores)
		}
		if len(got.Nodes[0].Cores) != 4 {
			t.Errorf("node has %d cores, want 4", len(got.Nodes[0].Cores))
		}
	})

	t.Run("CPU-less node", func(t *testing.T) {
		h := twoNodeHost()
		h.node(0, "0-3", "10 21 41", 1<<20)
		h.node(1, "4-7", "31 10 41", 2<<20)
		h.node(2, "", "41 41 10", 4<<20)
		h.memoryBlocks(nodeSysDir+"/node2", 64, true)
		got := probeTopology(t, fstest.MapFS(h))
		want := twoNodeWant()
		want.Nodes[0].Distances = []int{10, 21, 41}
		want.Nodes[1].Distances = []int{31, 10, 41}
		want.Nodes = append(want.Nodes, TopologyNode{
			ID: 2, Cores: []ProcessorCore{}, Caches: []Cache{}, Distances: []int{41, 41, 10},
			Memory: NodeMemory{TotalPhysicalBytes: memBlock, TotalUsableBytes: 4 << 30},
		})
		checkTopology(t, got, want)
		b, err := json.Marshal(got.Nodes[2])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b, []byte(`"cores":[],"caches":[]`)) {
			t.Errorf("CPU-less node = %s, want empty cores and caches", b)
		}
	})

	t.Run("cache without id, CPU without caches", func(t *testing.T) {
		h := cpuHost{}
		h.cpu(0, 0, 0, "0-1")
		h.cpu(1, 0, 0, "0-1")
		h.cpu(2, 0, 1, "2")
		h.cache(0, 0, 1, "Data", "32K", "", "0-1")
		h.cache(1, 0, 1, "Data", "32K", "", "0-1")
		h.cache(0, 1, 2, "Unified", "1M", "", "0-1")
		h.cache(1, 1, 2, "Unified", "1M", "", "0-1")
		h.node(0, "0-2", "10", 1<<20)
		got := probeTopology(t, fstest.MapFS(h))
		want := []Cache{
			{Level: 1, Type: "data", SizeBytes: 32 << 10, LogicalProcessors: []int{0, 1}},
			{Level: 2, Type: "unified", SizeBytes: 1 << 20, LogicalProcessors: []int{0, 1}},
		}
		if !reflect.DeepEqual(got.Nodes[0].Caches, want) {
			t.Errorf("caches = %+v\nwant %+v", got.Nodes[0].Caches, want)
		}
	})

	t.Run("unreadable files", func(t *testing.T) {
		h := twoNodeHost()
		// node 0's cpulist is unreadable and node 1 has none, so their CPUs
		// come from their cpu<N> entries.
		delete(h, nodeSysDir+"/node1/cpulist")
		for n := range 8 {
			h[fmt.Sprintf("%s/node%d/cpu%d", nodeSysDir, n/4, n)] = &fstest.MapFile{Mode: fs.ModeDir}
		}
		fsys := deniedFS{FS: fstest.MapFS(h), denied: map[string]bool{
			nodeSysDir + "/node0/distance":                                  true,
			nodeSysDir + "/node0/cpulist":                                   true,
			cpuSysDir + "/cpu0/cache/index0/size":                           true,
			cpuSysDir + "/cpu1/cache/index0/size":                           true,
			cpuSysDir + "/cpu4/cache/index2/type":                           true,
			cpuSysDir + "/cpu5/cache/index2/type":                           true,
			cpuSysDir + "/cpu6/cache/index2/level":                          true,
			cpuSysDir + "/cpu7/cache/index2/level":                          true,
			nodeSysDir + "/node1/meminfo":                                   true,
			nodeSysDir + "/node1/memory3/state":                             true,
			nodeSysDir + "/node1/memory4/state":                             true,
			nodeSysDir + "/node1/hugepages":                                 true,
			nodeSysDir + "/node0/hugepages/hugepages-2048kB/free_hugepages": true,
		}}
		got := probeTopology(t, fsys)
		want := twoNodeWant()
		n0, n1 := &want.Nodes[0], &want.Nodes[1]
		n0.Caches[0].SizeBytes = 0
		n0.Distances = nil
		n0.Memory.HugePageAmountsBySize = map[uint64]NodeHugePages{2 << 20: {Total: 4}, 1 << 30: {}}
		n1.Caches = []Cache{
			{Level: 0, Type: "unified", SizeBytes: 2 << 20, LogicalProcessors: []int{6, 7}},
			n1.Caches[0], n1.Caches[1], n1.Caches[2], n1.Caches[3],
			{Level: 2, Type: "unknown", SizeBytes: 2 << 20, LogicalProcessors: []int{4, 5}},
			n1.Caches[6],
		}
		n1.Memory = NodeMemory{}
		checkTopology(t, got, want)

		// A size the kernel would not write.
		fsys.denied = map[string]bool{}
		for _, n := range []int{0, 1} {
			h.file(fmt.Sprintf("%s/cpu%d/cache/index0/size", cpuSysDir, n), "48Q\n")
		}
		if c := probeTopology(t, fsys).Nodes[0].Caches[0]; c.SizeBytes != 0 {
			t.Errorf("cache with size 48Q = %+v, want size_bytes 0", c)
		}
	})
}

// distance is the eleven-node fixture's distance from node i to node j:
// each pair its own, so a distance credited to the wrong node shows.
func distance(i, j int) int {
	if i == j {
		return 10
	}
	return 100 + 10*i + j
}

func TestTopologyProberFails(t *testing.T) {
	t.Run("node directory unreadable", func(t *testing.T) {
		fsys := deniedFS{FS: fstest.MapFS(twoNodeHost()), denied: map[string]bool{nodeSysDir: true}}
		if _, err := NewTopologyProber(fsys).Probe(context.Background()); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("Probe error = %v, want a permission error", err)
		}
	})

	t.Run("cpu directory missing", func(t *testing.T) {
		h := twoNodeHost()
		for name := range h {
			if strings.HasPrefix(name, cpuSysDir+"/") {
				delete(h, name)
			}
		}
		if _, err := NewTopologyProber(fstest.MapFS(h)).Probe(context.Background()); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Probe error = %v, want a not-exist error", err)
		}
	})

	t.Run("no online CPU", func(t *testing.T) {
		// cpu fails here too: an empty topology must not replace the
		// last good one.
		h := twoNodeHost()
		for name := range h {
			if strings.HasPrefix(name, cpuSysDir+"/cpu") {
				delete(h, name)
			}
		}
		h.file(cpuSysDir+"/online", "\n")
		h.file(cpuSysDir+"/cpufreq/boost", "1\n")
		if _, err := NewTopologyProber(fstest.MapFS(h)).Probe(context.Background()); err == nil {
			t.Error("Probe succeeded on a host without online CPUs")
		}
	})
}

// TestTopologyNodeMemoryAddsUp checks that the nodes' MemTotals add up to
// the memory payload's for the same host, so that the two payloads count
// the same memory.
func TestTopologyNodeMemoryAddsUp(t *testing.T) {
	fsys := fstest.MapFS(twoNodeHost())
	top := probeTopology(t, fsys)
	mem, _ := probeMemory(t, fsys, nil)
	var sum int64
	for _, n := range top.Nodes {
		sum += n.Memory.TotalUsableBytes
	}
	if sum != mem.TotalUsableBytes {
		t.Errorf("nodes' total_usable_bytes add up to %d, memory's total_usable_bytes is %d", sum, mem.TotalUsableBytes)
	}
}

func TestTopologyParseCacheSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want uint64
	}{
		{"48K\n", 48 << 10},
		{"2048K", 2 << 20},
		{"32M", 32 << 20},
		{"1G", 1 << 30},
		{"512", 512},
		{"", 0},
		{"K", 0},
		{"48Q", 0},
		{"-1K", 0},
		{"99999999999999999999K", 0},
		{"18014398509481984K", 0}, // 2^54 KiB overflows uint64.
	} {
		if got := parseCacheSize(tc.in); got != tc.want {
			t.Errorf("parseCacheSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestTopologyJSON(t *testing.T) {
	marshal := func(t *testing.T, v any) []byte {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	fsys := fstest.MapFS(twoNodeHost())
	first := marshal(t, probeTopology(t, fsys))

	var top struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(first, &top); err != nil {
		t.Fatal(err)
	}
	checkKeys := func(what string, b []byte, want ...string) {
		t.Helper()
		if got := jsonKeys(t, b); !slices.Equal(got, want) {
			t.Errorf("%s keys = %v\nwant %v", what, got, want)
		}
	}
	checkKeys("topology", first, "architecture", "nodes")
	for _, n := range top.Nodes {
		checkKeys("node", n, "id", "cores", "caches", "distances", "memory")
		var node struct {
			Cores  []json.RawMessage `json:"cores"`
			Caches []json.RawMessage `json:"caches"`
			Memory json.RawMessage   `json:"memory"`
		}
		if err := json.Unmarshal(n, &node); err != nil {
			t.Fatal(err)
		}
		for _, c := range node.Cores {
			checkKeys("core", c, "id", "total_hardware_threads", "logical_processors")
		}
		for _, c := range node.Caches {
			checkKeys("cache", c, "level", "type", "size_bytes", "logical_processors")
		}
		checkKeys("memory", node.Memory,
			"total_physical_bytes", "total_usable_bytes", "supported_page_sizes", "total_huge_page_bytes", "huge_page_amounts_by_size")
		var mem struct {
			Amounts map[string]json.RawMessage `json:"huge_page_amounts_by_size"`
		}
		if err := json.Unmarshal(node.Memory, &mem); err != nil {
			t.Fatal(err)
		}
		for _, a := range mem.Amounts {
			checkKeys("huge page amounts", a, "total", "free", "surplus")
		}
	}
	for range 10 {
		if again := marshal(t, probeTopology(t, fsys)); !bytes.Equal(again, first) {
			t.Fatalf("payload changed between probes:\n%s\n%s", first, again)
		}
	}
}

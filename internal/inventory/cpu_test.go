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

// cpuHost is a host root with /proc/cpuinfo and /sys/devices/system/cpu.
type cpuHost fstest.MapFS

func (h cpuHost) file(name, data string) { h[name] = &fstest.MapFile{Data: []byte(data)} }

// cpu adds logical CPU n with its topology: its package, its core_id and
// its thread_siblings_list.
func (h cpuHost) cpu(n, pkg, core int, siblings string) {
	dir := fmt.Sprintf("%s/cpu%d/topology/", cpuSysDir, n)
	h.file(dir+"physical_package_id", fmt.Sprintf("%d\n", pkg))
	h.file(dir+"core_id", fmt.Sprintf("%d\n", core))
	h.file(dir+"thread_siblings_list", siblings+"\n")
}

// cpuinfo sets /proc/cpuinfo to blocks, each ended by a blank line as the
// kernel writes them.
func (h cpuHost) cpuinfo(blocks ...string) { h.file("proc/cpuinfo", strings.Join(blocks, "")) }

// cpuinfoBlock is the cpuinfo block of CPU n with the fields given as key,
// value pairs, padded with tabs as the kernel pads them.
func cpuinfoBlock(n int, kv ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "processor\t: %d\n", n)
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, "%s\t\t: %s\n", kv[i], kv[i+1])
	}
	b.WriteString("\n")
	return b.String()
}

// x86Block is an x86 cpuinfo block; an empty microcode leaves the field
// out, as a VM's cpuinfo does.
func x86Block(n int, model, flags, microcode string) string {
	kv := []string{"vendor_id", "GenuineIntel", "cpu family", "6", "model", "106", "model name", model, "stepping", "6"}
	if microcode != "" {
		kv = append(kv, "microcode", microcode)
	}
	kv = append(kv, "cpu MHz", "2000.000", "flags", flags, "bogomips", "4000.00")
	return cpuinfoBlock(n, kv...)
}

func probeCPUHost(t *testing.T, fsys fs.FS) *CPU {
	t.Helper()
	v, err := NewCPUProber(fsys).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	c, ok := v.(*CPU)
	if !ok {
		t.Fatalf("Probe returned %T, want *CPU", v)
	}
	return c
}

func checkCPU(t *testing.T, got, want *CPU) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		w, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("cpu =\n%s\nwant\n%s", g, w)
	}
}

const (
	modelA = "Synthetic(R) CPU A @ 2.00GHz"
	modelB = "Synthetic(R) CPU B @ 2.10GHz"
	flagsA = "fpu vme de pse"
	flagsB = "fpu vme sse sse2 avx512f"
)

// twoSocketHost is a 2-socket server with SMT: packages 0 and 1 of two
// cores of two threads each, cpus 0-3 on package 0 and 4-7 on package 1.
// siblings names each CPU's sibling list. Each package has its own model,
// flags and microcode, so a processor that takes its cpuinfo fields from
// another package's CPU shows.
func twoSocketHost(siblings [8]string, cores [8]int) cpuHost {
	h := cpuHost{}
	var blocks []string
	for n := range 8 {
		pkg, model, flags, microcode := 0, modelA, flagsA, "0xd0003e7"
		if n >= 4 {
			pkg, model, flags, microcode = 1, modelB, flagsB, "0xd0003f1"
		}
		h.cpu(n, pkg, cores[n], siblings[n])
		if n > 0 {
			h.file(fmt.Sprintf("%s/cpu%d/online", cpuSysDir, n), "1\n")
		}
		blocks = append(blocks, x86Block(n, model, flags, microcode))
	}
	h.cpuinfo(blocks...)
	h.file(cpuSysDir+"/possible", "0-7\n")
	h.file(cpuSysDir+"/present", "0-7\n")
	h.file(cpuSysDir+"/online", "0-7\n")
	return h
}

func twoSocketWant(cores0, cores1 []ProcessorCore) *CPU {
	return &CPU{
		TotalCores:           4,
		TotalHardwareThreads: 8,
		Processors: []Processor{
			{
				ID: 0, Vendor: "GenuineIntel", Model: modelA, Microcode: "0xd0003e7", Capabilities: strings.Fields(flagsA),
				TotalCores: 2, TotalHardwareThreads: 4, Cores: cores0,
			},
			{
				ID: 1, Vendor: "GenuineIntel", Model: modelB, Microcode: "0xd0003f1", Capabilities: strings.Fields(flagsB),
				TotalCores: 2, TotalHardwareThreads: 4, Cores: cores1,
			},
		},
	}
}

// commaSiblingsHost lists siblings as 0,2-style lists, as the kernel does
// when it numbers every package's first threads before their siblings.
func commaSiblingsHost() cpuHost {
	return twoSocketHost(
		[8]string{"0,2", "1,3", "0,2", "1,3", "4,6", "5,7", "4,6", "5,7"},
		[8]int{0, 1, 0, 1, 0, 1, 0, 1},
	)
}

func TestCPUProber(t *testing.T) {
	t.Run("two sockets, comma sibling lists", func(t *testing.T) {
		got := probeCPUHost(t, fstest.MapFS(commaSiblingsHost()))
		checkCPU(t, got, twoSocketWant(
			[]ProcessorCore{{ID: 0, TotalHardwareThreads: 2, LogicalProcessors: []int{0, 2}}, {ID: 1, TotalHardwareThreads: 2, LogicalProcessors: []int{1, 3}}},
			[]ProcessorCore{{ID: 0, TotalHardwareThreads: 2, LogicalProcessors: []int{4, 6}}, {ID: 1, TotalHardwareThreads: 2, LogicalProcessors: []int{5, 7}}},
		))
	})

	t.Run("two sockets, range sibling lists", func(t *testing.T) {
		h := twoSocketHost(
			[8]string{"0-1", "0-1", "2-3", "2-3", "4-5", "4-5", "6-7", "6-7"},
			[8]int{0, 0, 1, 1, 0, 0, 1, 1},
		)
		got := probeCPUHost(t, fstest.MapFS(h))
		checkCPU(t, got, twoSocketWant(
			[]ProcessorCore{{ID: 0, TotalHardwareThreads: 2, LogicalProcessors: []int{0, 1}}, {ID: 1, TotalHardwareThreads: 2, LogicalProcessors: []int{2, 3}}},
			[]ProcessorCore{{ID: 0, TotalHardwareThreads: 2, LogicalProcessors: []int{4, 5}}, {ID: 1, TotalHardwareThreads: 2, LogicalProcessors: []int{6, 7}}},
		))
	})

	t.Run("single socket, SMT off", func(t *testing.T) {
		h := cpuHost{}
		var blocks []string
		for n := range 4 {
			h.cpu(n, 0, n, fmt.Sprint(n))
			blocks = append(blocks, x86Block(n, modelA, flagsA, "0x1"))
		}
		h.cpuinfo(blocks...)
		got := probeCPUHost(t, fstest.MapFS(h))
		checkCPU(t, got, &CPU{
			TotalCores: 4, TotalHardwareThreads: 4,
			Processors: []Processor{{
				ID: 0, Vendor: "GenuineIntel", Model: modelA, Microcode: "0x1", Capabilities: strings.Fields(flagsA),
				TotalCores: 4, TotalHardwareThreads: 4,
				Cores: []ProcessorCore{
					{ID: 0, TotalHardwareThreads: 1, LogicalProcessors: []int{0}},
					{ID: 1, TotalHardwareThreads: 1, LogicalProcessors: []int{1}},
					{ID: 2, TotalHardwareThreads: 1, LogicalProcessors: []int{2}},
					{ID: 3, TotalHardwareThreads: 1, LogicalProcessors: []int{3}},
				},
			}},
		})
	})

	t.Run("a CPU offline", func(t *testing.T) {
		h := cpuHost{}
		var blocks []string
		for n := range 3 {
			h.cpu(n, 0, n, fmt.Sprint(n))
			blocks = append(blocks, x86Block(n, modelA, flagsA, "0x1"))
		}
		h.file(cpuSysDir+"/cpu1/online", "1\n")
		h.file(cpuSysDir+"/cpu2/online", "1\n")
		// The kernel removes an offline CPU's topology directory and its
		// cpuinfo block.
		h.file(cpuSysDir+"/cpu3/online", "0\n")
		h.cpuinfo(blocks...)
		h.file(cpuSysDir+"/possible", "0-3\n")
		h.file(cpuSysDir+"/present", "0-3\n")
		h.file(cpuSysDir+"/online", "0-2\n")
		got := probeCPUHost(t, fstest.MapFS(h))
		checkCPU(t, got, &CPU{
			TotalCores: 3, TotalHardwareThreads: 3,
			Processors: []Processor{{
				ID: 0, Vendor: "GenuineIntel", Model: modelA, Microcode: "0x1", Capabilities: strings.Fields(flagsA),
				TotalCores: 3, TotalHardwareThreads: 3,
				Cores: []ProcessorCore{
					{ID: 0, TotalHardwareThreads: 1, LogicalProcessors: []int{0}},
					{ID: 1, TotalHardwareThreads: 1, LogicalProcessors: []int{1}},
					{ID: 2, TotalHardwareThreads: 1, LogicalProcessors: []int{2}},
				},
			}},
			OfflineLogicalProcessors: []int{3},
		})
	})

	t.Run("VM with a socket per vCPU", func(t *testing.T) {
		h := cpuHost{}
		var blocks []string
		for n := range 4 {
			h.cpu(n, n, 0, fmt.Sprint(n))
			blocks = append(blocks, x86Block(n, modelA, flagsA, ""))
		}
		h.cpuinfo(blocks...)
		// Room for hot-added vCPUs: possible but not present, so not
		// offline either.
		h.file(cpuSysDir+"/possible", "0-7\n")
		h.file(cpuSysDir+"/present", "0-3\n")
		h.file(cpuSysDir+"/online", "0-3\n")
		got := probeCPUHost(t, fstest.MapFS(h))
		want := &CPU{TotalCores: 4, TotalHardwareThreads: 4}
		for n := range 4 {
			want.Processors = append(want.Processors, Processor{
				ID: n, Vendor: "GenuineIntel", Model: modelA, Capabilities: strings.Fields(flagsA),
				TotalCores: 1, TotalHardwareThreads: 1,
				Cores: []ProcessorCore{{ID: 0, TotalHardwareThreads: 1, LogicalProcessors: []int{n}}},
			})
		}
		checkCPU(t, got, want)
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(`"microcode"`)) || bytes.Contains(b, []byte(`"offline_logical_processors"`)) {
			t.Errorf("payload has microcode or offline_logical_processors: %s", b)
		}
	})

	t.Run("multi-die package", func(t *testing.T) {
		// core_id is unique only within a die: two dies of one package
		// each have a core 0 and a core 1.
		h := cpuHost{}
		for n, core := range []int{0, 1, 0, 1} {
			h.cpu(n, 0, core, fmt.Sprint(n))
		}
		got := probeCPUHost(t, fstest.MapFS(h))
		checkCPU(t, got, &CPU{
			TotalCores: 4, TotalHardwareThreads: 4,
			Processors: []Processor{{
				ID: 0, TotalCores: 4, TotalHardwareThreads: 4,
				Cores: []ProcessorCore{
					{ID: 0, TotalHardwareThreads: 1, LogicalProcessors: []int{0}},
					{ID: 0, TotalHardwareThreads: 1, LogicalProcessors: []int{2}},
					{ID: 1, TotalHardwareThreads: 1, LogicalProcessors: []int{1}},
					{ID: 1, TotalHardwareThreads: 1, LogicalProcessors: []int{3}},
				},
			}},
		})
	})

	armHost := func(part string) cpuHost {
		h := cpuHost{}
		var blocks []string
		for n := range 2 {
			h.cpu(n, 0, n, fmt.Sprint(n))
			blocks = append(blocks, cpuinfoBlock(n,
				"BogoMIPS", "50.00", "Features", "fp asimd evtstrm aes pmull", "CPU implementer", "0x41",
				"CPU architecture", "8", "CPU variant", "0x3", "CPU part", part, "CPU revision", "1"))
		}
		h.cpuinfo(blocks...)
		return h
	}
	armWant := func(model string) *CPU {
		return &CPU{
			TotalCores: 2, TotalHardwareThreads: 2,
			Processors: []Processor{{
				ID: 0, Vendor: "ARM", Model: model, Capabilities: []string{"fp", "asimd", "evtstrm", "aes", "pmull"},
				TotalCores: 2, TotalHardwareThreads: 2,
				Cores: []ProcessorCore{
					{ID: 0, TotalHardwareThreads: 1, LogicalProcessors: []int{0}},
					{ID: 1, TotalHardwareThreads: 1, LogicalProcessors: []int{1}},
				},
			}},
		}
	}

	t.Run("arm64", func(t *testing.T) {
		checkCPU(t, probeCPUHost(t, fstest.MapFS(armHost("0xd0c"))), armWant("Neoverse-N1"))
	})

	t.Run("arm64, unknown part", func(t *testing.T) {
		checkCPU(t, probeCPUHost(t, fstest.MapFS(armHost("0xd0b"))), armWant("0x41:0xd0b"))
	})

	t.Run("cpuinfo missing", func(t *testing.T) {
		h := commaSiblingsHost()
		delete(h, "proc/cpuinfo")
		got := probeCPUHost(t, fstest.MapFS(h))
		want := twoSocketWant(
			[]ProcessorCore{{ID: 0, TotalHardwareThreads: 2, LogicalProcessors: []int{0, 2}}, {ID: 1, TotalHardwareThreads: 2, LogicalProcessors: []int{1, 3}}},
			[]ProcessorCore{{ID: 0, TotalHardwareThreads: 2, LogicalProcessors: []int{4, 6}}, {ID: 1, TotalHardwareThreads: 2, LogicalProcessors: []int{5, 7}}},
		)
		for i := range want.Processors {
			p := &want.Processors[i]
			p.Vendor, p.Model, p.Microcode, p.Capabilities = "", "", "", nil
		}
		checkCPU(t, got, want)
	})

	t.Run("cpuinfo unreadable", func(t *testing.T) {
		fsys := deniedFS{FS: fstest.MapFS(commaSiblingsHost()), denied: map[string]bool{"proc/cpuinfo": true}}
		got := probeCPUHost(t, fsys)
		want := twoSocketWant(
			[]ProcessorCore{{ID: 0, TotalHardwareThreads: 2, LogicalProcessors: []int{0, 2}}, {ID: 1, TotalHardwareThreads: 2, LogicalProcessors: []int{1, 3}}},
			[]ProcessorCore{{ID: 0, TotalHardwareThreads: 2, LogicalProcessors: []int{4, 6}}, {ID: 1, TotalHardwareThreads: 2, LogicalProcessors: []int{5, 7}}},
		)
		for i := range want.Processors {
			p := &want.Processors[i]
			p.Vendor, p.Model, p.Microcode, p.Capabilities = "unknown", "unknown", "", nil
		}
		checkCPU(t, got, want)
	})

	t.Run("package fields come from the lowest CPU that has a block", func(t *testing.T) {
		h := commaSiblingsHost()
		var blocks []string
		for n := range 8 {
			// Package 0 keeps only cpu1's block; package 1 keeps cpus 5-7.
			if n != 0 && n != 4 {
				blocks = append(blocks, x86Block(n, map[bool]string{true: modelA, false: modelB}[n < 4], map[bool]string{true: flagsA, false: flagsB}[n < 4], "0x1"))
			}
		}
		h.cpuinfo(blocks...)
		got := probeCPUHost(t, fstest.MapFS(h))
		if p := got.Processors[0]; p.Model != modelA || p.Microcode != "0x1" || !slices.Equal(p.Capabilities, strings.Fields(flagsA)) {
			t.Errorf("processor 0 = %+v, want cpu1's fields", p)
		}
		if p := got.Processors[1]; p.Model != modelB || !slices.Equal(p.Capabilities, strings.Fields(flagsB)) {
			t.Errorf("processor 1 = %+v, want its own CPUs' fields", p)
		}
	})

	t.Run("package without any cpuinfo block", func(t *testing.T) {
		h := commaSiblingsHost()
		var blocks []string
		for n := range 4 {
			blocks = append(blocks, x86Block(n, modelA, flagsA, "0x1"))
		}
		h.cpuinfo(blocks...)
		got := probeCPUHost(t, fstest.MapFS(h))
		if p := got.Processors[1]; p.Vendor != "" || p.Model != "" || p.Microcode != "" || p.Capabilities != nil {
			t.Errorf("processor 1 = %+v, want no cpuinfo fields: none of its CPUs has a block", p)
		}
		if p := got.Processors[0]; p.Model != modelA {
			t.Errorf("processor 0 model = %q, want %q", p.Model, modelA)
		}
	})

	t.Run("CPU whose topology vanishes mid-probe", func(t *testing.T) {
		h := cpuHost{}
		h.cpu(0, 0, 0, "0")
		h.cpu(1, 0, 1, "1")
		h.file(cpuSysDir+"/cpu3/online", "1\n")
		h.cpuinfo(x86Block(0, modelA, flagsA, "0x1"), x86Block(1, modelA, flagsA, "0x1"))
		got := probeCPUHost(t, fstest.MapFS(h))
		checkCPU(t, got, &CPU{
			TotalCores: 2, TotalHardwareThreads: 2,
			Processors: []Processor{{
				ID: 0, Vendor: "GenuineIntel", Model: modelA, Microcode: "0x1", Capabilities: strings.Fields(flagsA),
				TotalCores: 2, TotalHardwareThreads: 2,
				Cores: []ProcessorCore{
					{ID: 0, TotalHardwareThreads: 1, LogicalProcessors: []int{0}},
					{ID: 1, TotalHardwareThreads: 1, LogicalProcessors: []int{1}},
				},
			}},
		})
	})

	t.Run("unreadable or unparseable topology", func(t *testing.T) {
		h := cpuHost{}
		h.cpu(0, 0, 0, "0")
		h.cpu(1, 0, 1, "1")
		h.cpu(2, 0, 2, "2")
		h.file(cpuSysDir+"/cpu2/topology/core_id", "garbage\n")
		h.cpuinfo(x86Block(0, modelA, flagsA, "0x1"), x86Block(1, modelB, flagsB, "0x2"), x86Block(2, modelA, flagsA, "0x1"))
		fsys := deniedFS{FS: fstest.MapFS(h), denied: map[string]bool{cpuSysDir + "/cpu1/topology/physical_package_id": true}}
		got := probeCPUHost(t, fsys)
		checkCPU(t, got, &CPU{
			TotalCores: 3, TotalHardwareThreads: 3,
			Processors: []Processor{
				{
					ID: -1, Vendor: "GenuineIntel", Model: modelB, Microcode: "0x2", Capabilities: strings.Fields(flagsB),
					TotalCores: 1, TotalHardwareThreads: 1,
					Cores: []ProcessorCore{{ID: 1, TotalHardwareThreads: 1, LogicalProcessors: []int{1}}},
				},
				{
					ID: 0, Vendor: "GenuineIntel", Model: modelA, Microcode: "0x1", Capabilities: strings.Fields(flagsA),
					TotalCores: 2, TotalHardwareThreads: 2,
					Cores: []ProcessorCore{
						{ID: -1, TotalHardwareThreads: 1, LogicalProcessors: []int{2}},
						{ID: 0, TotalHardwareThreads: 1, LogicalProcessors: []int{0}},
					},
				},
			},
		})
	})

	t.Run("sibling list fallbacks", func(t *testing.T) {
		h := cpuHost{}
		h.cpu(0, 0, 0, "0-1")
		h.cpu(1, 0, 0, "0-1")
		h.cpu(2, 0, 1, "2-3")
		h.cpu(3, 0, 1, "2-3")
		// cpu1 has only the newer name of the list; cpu2 has none, and
		// cpu3 one the kernel would never write, so each of those is a
		// core of its own.
		delete(h, cpuSysDir+"/cpu1/topology/thread_siblings_list")
		h.file(cpuSysDir+"/cpu1/topology/core_cpus_list", "0-1\n")
		delete(h, cpuSysDir+"/cpu2/topology/thread_siblings_list")
		h.file(cpuSysDir+"/cpu3/topology/thread_siblings_list", "2-\n")
		got := probeCPUHost(t, fstest.MapFS(h))
		checkCPU(t, got, &CPU{
			TotalCores: 3, TotalHardwareThreads: 4,
			Processors: []Processor{{
				ID: 0, TotalCores: 3, TotalHardwareThreads: 4,
				Cores: []ProcessorCore{
					{ID: 0, TotalHardwareThreads: 2, LogicalProcessors: []int{0, 1}},
					{ID: 1, TotalHardwareThreads: 1, LogicalProcessors: []int{2}},
					{ID: 1, TotalHardwareThreads: 1, LogicalProcessors: []int{3}},
				},
			}},
		})
	})

	t.Run("offline list unreadable", func(t *testing.T) {
		h := cpuHost{}
		h.cpu(0, 0, 0, "0")
		h.file(cpuSysDir+"/present", "0-1\n")
		h.file(cpuSysDir+"/online", "0\n")
		fsys := deniedFS{FS: fstest.MapFS(h), denied: map[string]bool{cpuSysDir + "/present": true}}
		if got := probeCPUHost(t, fsys).OfflineLogicalProcessors; got != nil {
			t.Errorf("OfflineLogicalProcessors = %v, want none", got)
		}
		h.file(cpuSysDir+"/online", "0-\n")
		if got := probeCPUHost(t, fstest.MapFS(h)).OfflineLogicalProcessors; got != nil {
			t.Errorf("OfflineLogicalProcessors with a malformed online list = %v, want none", got)
		}
	})
}

func TestCPUProberFails(t *testing.T) {
	t.Run("cpu directory missing", func(t *testing.T) {
		h := cpuHost{}
		h.cpuinfo(x86Block(0, modelA, flagsA, "0x1"))
		if _, err := NewCPUProber(fstest.MapFS(h)).Probe(context.Background()); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Probe error = %v, want a not-exist error", err)
		}
	})

	t.Run("cpu directory unreadable", func(t *testing.T) {
		fsys := deniedFS{FS: fstest.MapFS(commaSiblingsHost()), denied: map[string]bool{cpuSysDir: true}}
		if _, err := NewCPUProber(fsys).Probe(context.Background()); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("Probe error = %v, want a permission error", err)
		}
	})

	t.Run("no online CPU", func(t *testing.T) {
		h := cpuHost{}
		h.file(cpuSysDir+"/online", "\n")
		h.file(cpuSysDir+"/cpufreq/boost", "1\n")
		if _, err := NewCPUProber(fstest.MapFS(h)).Probe(context.Background()); err == nil {
			t.Error("Probe succeeded on a host without CPUs")
		}
	})
}

// cpuRange returns the CPUs lo to hi, inclusive.
func cpuRange(lo, hi int) []int {
	var cpus []int
	for n := lo; n <= hi; n++ {
		cpus = append(cpus, n)
	}
	return cpus
}

func TestCPUParseList(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    []int
		wantErr bool
	}{
		{in: "", want: []int{}},
		{in: "\n", want: []int{}},
		{in: "5", want: []int{5}},
		{in: "0-3,8-11\n", want: []int{0, 1, 2, 3, 8, 9, 10, 11}},
		{in: "0,4", want: []int{0, 4}},
		{in: "0-3,", wantErr: true},
		{in: "3-1", wantErr: true},
		{in: "x", wantErr: true},
		{in: "-1", wantErr: true},
		// A corrupt list must not exhaust memory: ranges wider than 65536
		// CPUs, numbers above 1<<20 and lists longer than any real one are
		// malformed.
		{in: "0-999999999", wantErr: true},
		{in: "0-65536", wantErr: true},
		{in: "1048577", wantErr: true},
		{in: "99999999999999999999", wantErr: true},
		{in: strings.Repeat("0-65535,", 17) + "0", wantErr: true},
		{in: "0-8191", want: cpuRange(0, 8191)},
		{in: "1048576", want: []int{1 << 20}},
	} {
		got, err := parseCPUList(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseCPUList(%q) error = %v, want error %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && !slices.Equal(got, tc.want) {
			t.Errorf("parseCPUList(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCPUARMNames(t *testing.T) {
	for _, tc := range []struct{ implementer, part, vendor, model string }{
		{"0x41", "0xd40", "ARM", "Neoverse-V1"},
		{"0x41", "0xd4f", "ARM", "Neoverse-V2"},
		{"0xc0", "0xac3", "Ampere", "Ampere-1"},
		{"0x48", "0xd01", "HiSilicon", "Kunpeng-920"},
		{"0x4e", "0x004", "NVIDIA", "0x4e:0x004"},
		{"0x99", "0x001", "0x99", "0x99:0x001"},
	} {
		if got := armVendor(tc.implementer); got != tc.vendor {
			t.Errorf("armVendor(%q) = %q, want %q", tc.implementer, got, tc.vendor)
		}
		if got := armModel(tc.implementer, tc.part); got != tc.model {
			t.Errorf("armModel(%q, %q) = %q, want %q", tc.implementer, tc.part, got, tc.model)
		}
	}
}

func TestCPUJSON(t *testing.T) {
	marshal := func(t *testing.T, v any) []byte {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	t.Run("field order", func(t *testing.T) {
		core := ProcessorCore{ID: 1, TotalHardwareThreads: 1, LogicalProcessors: []int{1}}
		p := Processor{
			ID: 1, Vendor: "x", Model: "x", Microcode: "x", Capabilities: []string{"x"}, TotalCores: 1,
			TotalHardwareThreads: 1, Cores: []ProcessorCore{core},
		}
		c := CPU{TotalCores: 1, TotalHardwareThreads: 1, Processors: []Processor{p}, OfflineLogicalProcessors: []int{2}}
		for _, tc := range []struct {
			v    any
			want []string
		}{
			{c, []string{"total_cores", "total_hardware_threads", "processors", "offline_logical_processors"}},
			{p, []string{"id", "vendor", "model", "microcode", "capabilities", "total_cores", "total_hardware_threads", "cores"}},
			{core, []string{"id", "total_hardware_threads", "logical_processors"}},
			{Processor{}, []string{"id", "total_cores", "total_hardware_threads", "cores"}},
		} {
			if got := jsonKeys(t, marshal(t, tc.v)); !slices.Equal(got, tc.want) {
				t.Errorf("%T keys = %v\nwant %v", tc.v, got, tc.want)
			}
		}
	})

	t.Run("two-socket payload", func(t *testing.T) {
		fsys := fstest.MapFS(commaSiblingsHost())
		first := marshal(t, probeCPUHost(t, fsys))
		if got, want := jsonKeys(t, first), []string{"total_cores", "total_hardware_threads", "processors"}; !slices.Equal(got, want) {
			t.Errorf("keys = %v, want %v", got, want)
		}
		var c struct {
			Processors []json.RawMessage `json:"processors"`
		}
		if err := json.Unmarshal(first, &c); err != nil {
			t.Fatal(err)
		}
		wantProc := []string{"id", "vendor", "model", "microcode", "capabilities", "total_cores", "total_hardware_threads", "cores"}
		for _, p := range c.Processors {
			if got := jsonKeys(t, p); !slices.Equal(got, wantProc) {
				t.Errorf("processor keys = %v\nwant %v", got, wantProc)
			}
			var cores struct {
				Cores []json.RawMessage `json:"cores"`
			}
			if err := json.Unmarshal(p, &cores); err != nil {
				t.Fatal(err)
			}
			for _, core := range cores.Cores {
				if got, want := jsonKeys(t, core), []string{"id", "total_hardware_threads", "logical_processors"}; !slices.Equal(got, want) {
					t.Errorf("core keys = %v, want %v", got, want)
				}
			}
		}
		for range 10 {
			if again := marshal(t, probeCPUHost(t, fsys)); !bytes.Equal(again, first) {
				t.Fatalf("payload changed between probes:\n%s\n%s", first, again)
			}
		}
	})
}

// Only x86's cpuinfo flags can carry a hypervisor bit. The answer comes
// from the host's cpuinfo, not the agent's GOARCH: under user-mode
// emulation an amd64 agent can read an arm64 host's Features.
func TestReportsHypervisorFlag(t *testing.T) {
	x86 := cpuHost{}
	x86.cpuinfo(x86Block(0, "Intel(R) Xeon(R)", "fpu sse2", "0x2b000603"))
	arm := cpuHost{}
	arm.cpuinfo(cpuinfoBlock(0, "Features", "fp asimd aes", "CPU implementer", "0x41"))

	for _, tc := range []struct {
		name string
		root fs.FS
		want bool
	}{
		{"x86 flags", fstest.MapFS(x86), true},
		{"arm64 Features", fstest.MapFS(arm), false},
		{"no cpuinfo", fstest.MapFS{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReportsHypervisorFlag(tc.root); got != tc.want {
				t.Errorf("ReportsHypervisorFlag = %v, want %v", got, tc.want)
			}
		})
	}
}

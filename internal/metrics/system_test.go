package metrics

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func TestRuntimeEmitsConventionalNames(t *testing.T) {
	var buf bytes.Buffer
	writeRuntime(&buf)
	got := buf.String()
	for _, want := range []string{
		"go_goroutines",
		"go_memstats_heap_alloc_bytes",
		"go_memstats_heap_inuse_bytes",
		"go_memstats_sys_bytes",
		"go_gc_cycles_total",
		"go_gc_pauses_seconds",
		"go_info{",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("runtime exposition is missing %q:\n%s", want, got)
		}
	}
}

// TestRuntimeSeparatesHeapAndStackMemory guards against a heap-mapping
// defect: reporting goroutine stack memory (/memory/classes/heap/stacks) as
// go_memstats_heap_inuse_bytes, which is conventionally
// runtime.MemStats.HeapInuse and is off by roughly an order of magnitude
// when fed stack bytes instead. Stacks must be exposed under their own
// honest name, and heap-inuse must be objects+unused.
func TestRuntimeSeparatesHeapAndStackMemory(t *testing.T) {
	var buf bytes.Buffer
	writeRuntime(&buf)
	got := buf.String()
	for _, want := range []string{
		"go_memstats_heap_alloc_bytes",
		"go_memstats_heap_inuse_bytes",
		"go_memstats_stack_inuse_bytes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("runtime exposition is missing %q:\n%s", want, got)
		}
	}

	alloc, ok := parseBareGaugeValue(got, "go_memstats_heap_alloc_bytes")
	if !ok {
		t.Fatal("could not parse go_memstats_heap_alloc_bytes")
	}
	inuse, ok := parseBareGaugeValue(got, "go_memstats_heap_inuse_bytes")
	if !ok {
		t.Fatal("could not parse go_memstats_heap_inuse_bytes")
	}
	// heap_inuse = objects (= heap_alloc) + unused, and unused can never
	// be negative, so heap_inuse must never be less than heap_alloc. The
	// old, wrong mapping (stacks folded into heap_inuse) could violate
	// this on a host where stack usage was smaller than unused span space.
	if inuse < alloc {
		t.Errorf("go_memstats_heap_inuse_bytes (%g) < go_memstats_heap_alloc_bytes (%g); "+
			"heap_inuse must be objects+unused, never less than objects alone", inuse, alloc)
	}
}

// parseBareGaugeValue extracts the value of an unlabeled sample line
// ("name value") from a rendered exposition.
func parseBareGaugeValue(exposition, name string) (float64, bool) {
	for _, line := range strings.Split(exposition, "\n") {
		rest, ok := strings.CutPrefix(line, name+" ")
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

func TestProcessReadsProcfs(t *testing.T) {
	// utime 100 + stime 200 ticks at 100 Hz = 3 seconds.
	stat := "1 (hwi-api) S 0 1 1 0 -1 4194304 100 0 0 0 100 200 0 0 20 0 8 0 900 " +
		"1073741824 2048 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 1 0 0 0 0 0\n"
	fsys := fstest.MapFS{
		"proc/self/stat":   {Data: []byte(stat)},
		"proc/self/status": {Data: []byte("Name:\thwi-api\nVmRSS:\t   8192 kB\n")},
		"proc/stat":        {Data: []byte("btime 1758441600\n")},
	}

	var buf bytes.Buffer
	writeProcess(&buf, fsys)
	got := buf.String()
	for _, want := range []string{
		"process_cpu_seconds_total 3",
		"process_resident_memory_bytes 8.388608e+06",
		"process_virtual_memory_bytes 1.073741824e+09",
		"process_start_time_seconds",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("process exposition is missing %q:\n%s", want, got)
		}
	}
}

// TestProcessMissingFilesEmitsNothing guards the "a missing /proc
// file emits nothing rather than failing the scrape" requirement, which is
// what lets this package build and test on darwin, where these files never
// exist. An empty fs.FS stands in for that.
func TestProcessMissingFilesEmitsNothing(t *testing.T) {
	var buf bytes.Buffer
	writeProcess(&buf, fstest.MapFS{})
	got := buf.String()
	for _, unwanted := range []string{
		"process_cpu_seconds_total",
		"process_resident_memory_bytes",
		"process_virtual_memory_bytes",
		"process_start_time_seconds",
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("exposition unexpectedly contains %q with no procfs available:\n%s", unwanted, got)
		}
	}
}

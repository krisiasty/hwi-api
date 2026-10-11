package metrics

import (
	"bytes"
	"fmt"
	"io/fs"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
)

// runtimeSampleNames are read fresh on every scrape and mapped onto the
// conventional client_golang names so existing dashboards keep working.
//
// Index order matches the idx* constants below; keep them in sync.
var runtimeSampleNames = []string{
	"/sched/goroutines:goroutines",
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/heap/unused:bytes",
	"/memory/classes/heap/stacks:bytes",
	"/memory/classes/total:bytes",
	"/gc/cycles/total:gc-cycles",
	"/gc/pauses:seconds",
}

const (
	idxGoroutines = iota
	idxHeapObjects
	idxHeapUnused
	idxHeapStacks
	idxMemTotal
	idxGCCyclesTotal
	idxGCPauses
)

// writeRuntime writes a handful of runtime/metrics samples under the
// conventional go_* names client_golang's Go collector uses, plus go_info,
// read as the process is when scraped.
func writeRuntime(w *bytes.Buffer) {
	samples := make([]metrics.Sample, len(runtimeSampleNames))
	for i, name := range runtimeSampleNames {
		samples[i].Name = name
	}
	metrics.Read(samples)

	if v, ok := sampleUint64(samples[idxGoroutines]); ok {
		writeGaugeLine(w, "go_goroutines", "Number of goroutines that currently exist.", v)
	}

	objects, okObjects := sampleUint64(samples[idxHeapObjects])
	if okObjects {
		// Measured equal to runtime.MemStats.HeapAlloc exactly: leave
		// this one alone.
		writeGaugeLine(w, "go_memstats_heap_alloc_bytes", "Number of heap bytes allocated and still in use.", objects)
	}

	if unused, okUnused := sampleUint64(samples[idxHeapUnused]); okObjects && okUnused {
		// go_memstats_heap_inuse_bytes is conventionally
		// runtime.MemStats.HeapInuse, which is heap span space either
		// holding a live object or reserved (but not yet used) for
		// more objects of that size class: objects + unused, not
		// objects alone. Mapping /memory/classes/heap/stacks here
		// instead would report goroutine stack memory as heap in use,
		// about an order of magnitude too high on a typical process. Do not
		// simplify this back to a single runtime/metrics class.
		writeGaugeLine(w, "go_memstats_heap_inuse_bytes",
			"Number of heap bytes that are in use (allocated objects plus span space reserved for their size class; objects+unused).",
			objects+unused)
	}

	if v, ok := sampleUint64(samples[idxHeapStacks]); ok {
		// This is runtime.MemStats.StackInuse, not heap memory: goroutine
		// stack spans, exposed under their own honest, conventional
		// client_golang name rather than folded into heap_inuse.
		writeGaugeLine(w, "go_memstats_stack_inuse_bytes", "Number of bytes in stack spans currently in use by goroutines.", v)
	}

	if v, ok := sampleUint64(samples[idxMemTotal]); ok {
		writeGaugeLine(w, "go_memstats_sys_bytes", "Number of bytes obtained from the system.", v)
	}
	if v, ok := sampleUint64(samples[idxGCCyclesTotal]); ok {
		writeCounterLine(w, "go_gc_cycles_total", "Count of completed GC cycles.", v)
	}
	writeRuntimeHistogram(w, "go_gc_pauses_seconds",
		"Distribution of individual GC-related stop-the-world pause latencies. "+
			"runtime/metrics only reports bucketed counts, not individual pause "+
			"samples, so _sum here is an approximation (each bucket's count "+
			"weighted by the midpoint of its bounds); _count and every _bucket "+
			"value are exact.",
		samples[idxGCPauses])

	fmt.Fprintf(w, "# HELP go_info Information about the Go environment.\n# TYPE go_info gauge\ngo_info{version=\"%s\"} 1\n", escapeLabelValue(runtime.Version()))
}

// sampleUint64 reads a uint64-kind sample, reporting 0, false if the kind
// does not match: a metric this package no longer recognizes (a Go version
// skew) must emit nothing, not panic.
func sampleUint64(s metrics.Sample) (float64, bool) {
	if s.Value.Kind() != metrics.KindUint64 {
		return 0, false
	}
	return float64(s.Value.Uint64()), true
}

func writeGaugeLine(w *bytes.Buffer, name, help string, v float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", name, help, name, name, formatFloat(v))
}

func writeCounterLine(w *bytes.Buffer, name, help string, v float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %s\n", name, help, name, name, formatFloat(v))
}

// writeRuntimeHistogram renders a runtime/metrics Float64Histogram sample
// as a Prometheus histogram. The runtime only gives us per-bucket counts
// and boundaries, not the individual observations, so _sum is necessarily
// an approximation: each bucket's count is weighted by the midpoint of its
// (finite) bounds, or by whichever bound is finite when the other is
// infinite. _count and the _bucket values themselves are exact.
func writeRuntimeHistogram(w *bytes.Buffer, name, help string, s metrics.Sample) {
	if s.Value.Kind() != metrics.KindFloat64Histogram {
		return
	}
	h := s.Value.Float64Histogram()
	if h == nil || len(h.Buckets) < 2 {
		return
	}

	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)

	var cumulative uint64
	var sum float64
	for i, count := range h.Counts {
		lower, upper := h.Buckets[i], h.Buckets[i+1]
		cumulative += count
		sum += float64(count) * bucketWeight(lower, upper)

		le := "+Inf"
		if !isInf(upper) {
			le = formatFloat(upper)
		}
		fmt.Fprintf(w, "%s_bucket{le=\"%s\"} %d\n", name, le, cumulative)
	}
	fmt.Fprintf(w, "%s_sum %s\n", name, formatFloat(sum))
	fmt.Fprintf(w, "%s_count %d\n", name, cumulative)
}

func bucketWeight(lower, upper float64) float64 {
	switch {
	case isInf(lower) && isInf(upper):
		return 0
	case isInf(lower):
		return upper
	case isInf(upper):
		return lower
	default:
		return (lower + upper) / 2
	}
}

func isInf(v float64) bool {
	return v > maxFinite || v < -maxFinite
}

// maxFinite is larger than any bucket boundary runtime/metrics produces in
// practice; it exists only so isInf need not import math for a single
// comparison against +/-Inf.
const maxFinite = 1e300

// writeProcess writes process_cpu_seconds_total,
// process_virtual_memory_bytes, process_resident_memory_bytes and
// process_start_time_seconds from the /proc files in fsys. A file that
// cannot be read, as on darwin, where this package must still build and
// test, leaves its metrics out of the scrape rather than failing it.
func writeProcess(w *bytes.Buffer, fsys fs.FS) {
	stat, statErr := fs.ReadFile(fsys, "proc/self/stat")
	fields, haveStat := parseProcSelfStat(stat)
	if statErr != nil {
		haveStat = false
	}

	if haveStat {
		cpuSeconds := float64(fields.utime+fields.stime) / 100
		writeCounterLine(w, "process_cpu_seconds_total", "Total user and system CPU time spent in seconds.", cpuSeconds)
		writeGaugeLine(w, "process_virtual_memory_bytes", "Virtual memory size in bytes.", float64(fields.vsize))
	}

	if status, err := fs.ReadFile(fsys, "proc/self/status"); err == nil {
		if rss, ok := parseVmRSSBytes(status); ok {
			writeGaugeLine(w, "process_resident_memory_bytes", "Resident memory size in bytes.", float64(rss))
		}
	}

	if haveStat {
		if procStat, err := fs.ReadFile(fsys, "proc/stat"); err == nil {
			if btime, ok := parseBtime(procStat); ok {
				start := float64(btime) + float64(fields.starttime)/100
				writeGaugeLine(w, "process_start_time_seconds", "Start time of the process since unix epoch in seconds.", start)
			}
		}
	}
}

// statFields is the subset of /proc/self/stat this package needs, using
// the field numbers from proc(5) (1-based, counting pid and comm).
type statFields struct {
	utime, stime, starttime, vsize uint64
}

// parseProcSelfStat extracts utime (field 14), stime (field 15), starttime
// (field 22) and vsize (field 23) from the contents of /proc/self/stat.
//
// The comm field (field 2) is the executable's basename in parentheses,
// and the kernel allows it to contain spaces or even its own parentheses,
// so this takes everything after the *last* ") " rather than splitting on
// whitespace from the start: doing the latter would shift every field
// after comm by however many extra tokens the name introduced.
func parseProcSelfStat(data []byte) (statFields, bool) {
	if len(data) == 0 {
		return statFields{}, false
	}
	s := string(data)
	idx := strings.LastIndex(s, ") ")
	if idx < 0 {
		return statFields{}, false
	}
	// Fields here are numbered from state (proc(5) field 3) as index 0.
	rest := strings.Fields(s[idx+2:])
	const (
		fUtime     = 14
		fStime     = 15
		fStarttime = 22
		fVsize     = 23
	)
	if len(rest) <= fVsize-3 {
		return statFields{}, false
	}

	var f statFields
	var ok bool
	if f.utime, ok = parseUint(rest[fUtime-3]); !ok {
		return statFields{}, false
	}
	if f.stime, ok = parseUint(rest[fStime-3]); !ok {
		return statFields{}, false
	}
	if f.starttime, ok = parseUint(rest[fStarttime-3]); !ok {
		return statFields{}, false
	}
	if f.vsize, ok = parseUint(rest[fVsize-3]); !ok {
		return statFields{}, false
	}
	return f, true
}

// parseVmRSSBytes extracts VmRSS from /proc/self/status, in bytes: the
// kernel reports it in KiB, so this multiplies by 1024.
func parseVmRSSBytes(data []byte) (uint64, bool) {
	for line := range strings.SplitSeq(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "VmRSS:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		kb, ok := parseUint(fields[0])
		if !ok {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

// parseBtime extracts the "btime" (boot time, seconds since the epoch)
// line from /proc/stat.
func parseBtime(data []byte) (int64, bool) {
	for line := range strings.SplitSeq(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, "btime ")
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

func parseUint(s string) (uint64, bool) {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

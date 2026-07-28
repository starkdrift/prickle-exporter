// SPDX-License-Identifier: Apache-2.0

package container

import (
	"errors"
	"path/filepath"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// memoryStatFields are the memory.stat keys that get their own metric.
//
// The captured file has 40 keys. Emitting all of them would cost 40 series per
// container — on the captured node, more series than the entire Phase 1 host
// collector produces — for numbers most of which answer a kernel developer's
// question rather than an operator's. This is the subset that says where a
// container's memory went and whether it can be reclaimed. SPEC.md §Metrics
// contract: each addition multiplies series count, so additions are deliberate.
var memoryStatFields = []struct{ key, name, help string }{
	{"anon", "memory_anon_bytes", "Anonymous memory: heap and stack, not backed by a file, reclaimable only to swap."},
	{"file", "memory_file_bytes", "Page cache."},
	{"inactive_file", "memory_inactive_file_bytes", "Page cache on the inactive LRU list, the first thing reclaimed under pressure. Subtract it from memory_usage_bytes for the working set."},
	{"kernel_stack", "memory_kernel_stack_bytes", "Kernel stacks of the container's threads."},
	{"pagetables", "memory_page_tables_bytes", "Page tables mapping the container's address spaces."},
	{"slab", "memory_slab_bytes", "Kernel data structures allocated on the container's behalf."},
	{"sock", "memory_socket_bytes", "Memory held in network socket buffers."},
	{"shmem", "memory_shmem_bytes", "Shared memory and tmpfs, charged to the container that faulted it in."},
	{"unevictable", "memory_unevictable_bytes", "Memory that cannot be reclaimed at all, such as mlock'd pages."},
}

// memoryLimits are the cgroup v2 memory control files, each reported only when
// it is actually set.
var memoryLimits = []struct{ file, name, help string }{
	{"memory.max", "memory_limit_bytes", "Hard memory limit: the container is OOM-killed above it. Absent when unlimited."},
	{"memory.high", "memory_high_bytes", "Throttling threshold: allocation is slowed and reclaim forced above it. Absent when unset."},
	{"memory.min", "memory_min_bytes", "Guaranteed memory never reclaimed under pressure. Absent when unset."},
	{"memory.low", "memory_low_bytes", "Best-effort memory protected from reclaim. Absent when unset."},
}

// collectMemory parses memory.current, memory.stat and the four limit files.
func (c *Collector) collectMemory(out *exposition.Set, cg cgroup, labels []exposition.Label) error {
	var errs []error

	path := filepath.Join(cg.dir, "memory.current")
	if text, err := readFile(path); err != nil {
		errs = append(errs, skipMissing(err))
	} else if v, err := parseUint(path, text); err != nil {
		errs = append(errs, err)
	} else {
		out.Gauge(prefix+"memory_usage_bytes",
			"Total memory charged to the container, including page cache.").
			Add(float64(v), labels...)
	}

	for _, l := range memoryLimits {
		path := filepath.Join(cg.dir, l.file)
		v, set, err := readLimit(path)
		if err != nil {
			errs = append(errs, skipMissing(err))
			continue
		}
		// memory.min and memory.low read 0, not "max", when unset — and a zero
		// reservation is the same statement as no reservation.
		if !set || v == 0 {
			continue
		}
		out.Gauge(prefix+l.name, l.help).Add(float64(v), labels...)
	}

	path = filepath.Join(cg.dir, "memory.stat")
	stat, err := readFlatKeyed(path)
	if err != nil {
		errs = append(errs, skipMissing(err))
	}
	for _, f := range memoryStatFields {
		if v, ok := stat[f.key]; ok {
			out.Gauge(prefix+f.name, f.help).Add(float64(v), labels...)
		}
	}
	if v, ok := stat["pgfault"]; ok {
		out.Counter(prefix+"memory_page_faults_total",
			"Page faults taken by the container, major and minor together.").
			Add(float64(v), labels...)
	}
	if v, ok := stat["pgmajfault"]; ok {
		out.Counter(prefix+"memory_major_page_faults_total",
			"Page faults that required a disk read. A rising rate here is a container swapping or thrashing its page cache.").
			Add(float64(v), labels...)
	}

	return errors.Join(errs...)
}

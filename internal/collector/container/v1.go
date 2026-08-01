// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// v1Hierarchy reads the split hierarchies of cgroup v1.
//
// Brought into scope on 2026-08-01 (SPEC.md §Hard constraints #4) because RHEL
// 8 defaults to it and is supported into 2029, so treating it as exotic meant
// an ordinary enterprise host produced an empty scrape.
type v1Hierarchy struct{ c *Collector }

func (h v1Hierarchy) version() string { return "v1" }

func (h v1Hierarchy) sources(devices map[string]string) []source {
	// No pressure source. v1 has no PSI — it arrived with the unified
	// hierarchy — so prickle_container_pressure_stalled_seconds_total is
	// absent here rather than zero, which SPEC.md §Hard constraints #4 requires
	// for the same reason utilization_ratio vanishes under MIG: a zero would
	// read as "nothing is stalling" instead of "this kernel cannot say".
	return []source{
		h.collectCPU,
		h.collectMemory,
		h.collectIO(devices),
		h.collectPIDs,
	}
}

// v1DiscoveryController is the hierarchy walked to find containers.
//
// Any controller would do — every container appears under all of them — and
// memory is the one that is always mounted and always populated. Walking one
// and deriving the rest keeps a container from being discovered several times
// under different controllers, which is what walking them all would do.
const v1DiscoveryController = "memory"

// discover walks one controller hierarchy and records where each container sits
// relative to it, which is where it also sits under every other controller.
func (h v1Hierarchy) discover(ctx context.Context) ([]cgroup, error) {
	cgroupRoot := h.c.opts.Roots.CgroupPath()
	root := filepath.Join(cgroupRoot, v1DiscoveryController)
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var found []cgroup
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) || os.IsPermission(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// The directory-name shapes are the runtimes', not the hierarchy's, so
		// identify is shared: docker-<hex>.scope and kubepods-…pod<uid>.slice
		// look the same under /sys/fs/cgroup/memory as under a v2 root.
		cg, ok := identify(path, d.Name())
		if !ok {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		cg.root, cg.rel = cgroupRoot, rel
		found = append(found, cg)
		return fs.SkipDir
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// v1Unlimited is what a v1 limit file holds when no limit is set.
//
// PAGE_COUNTER_MAX rendered in bytes: the largest page count that fits in a
// signed long, times the page size. v2 replaced this with the literal string
// `max`, which is why the two hierarchies cannot share the check.
const v1Unlimited = 9223372036854771712

// collectCPU reads the v1 CPU controllers.
//
//	cpu.cfs_quota_us   25000        cpu.stat: nr_periods 341
//	cpu.cfs_period_us  100000                 nr_throttled 338
//	cpu.shares         1024                   throttled_time 24200774018
//	cpuacct.usage      8507920624
//
// Two unit traps, both of which would silently produce numbers a thousand
// times wrong. v1 counts CPU time in **nanoseconds** where v2's cpu.stat uses
// microseconds — that applies to cpuacct.usage and to throttled_time alike.
// And v1's quota is a pair of separate files rather than v2's single
// `<quota> <period>` line, with -1 rather than `max` for unset.
func (h v1Hierarchy) collectCPU(out *exposition.Set, cg cgroup, labels []exposition.Label) error {
	var errs []error

	if v, err := readV1Int(cg.ctrlPath("cpuacct.usage", "cpu,cpuacct", "cpuacct")); err == nil {
		out.Counter(prefix+"cpu_usage_seconds_total",
			"Total CPU time consumed. The sum of the user and system modes below, from the kernel's own accounting.").
			Add(float64(v)/1e9, labels...)
	} else {
		errs = append(errs, skipMissing(err))
	}

	// cpuacct.stat splits the total, in USER_HZ rather than either of the
	// units above. The kernel fixes USER_HZ at 100 for this file regardless of
	// the configured tick, which is why the divisor is a constant and not a
	// sysconf call.
	if stat, err := readFlatKeyed(cg.ctrlPath("cpuacct.stat", "cpu,cpuacct", "cpuacct")); err == nil {
		for _, m := range []struct{ key, mode string }{{"user", "user"}, {"system", "system"}} {
			if v, ok := stat[m.key]; ok {
				out.Counter(prefix+"cpu_seconds_total", "CPU time consumed, split by mode.").
					Add(float64(v)/100, with(labels, exposition.L("mode", m.mode))...)
			}
		}
	} else {
		errs = append(errs, skipMissing(err))
	}

	quota, qErr := readV1Int(cg.ctrlPath("cpu.cfs_quota_us", "cpu,cpuacct", "cpu"))
	period, pErr := readV1Int(cg.ctrlPath("cpu.cfs_period_us", "cpu,cpuacct", "cpu"))
	if qErr == nil && pErr == nil && quota > 0 && period > 0 {
		out.Gauge(prefix+"cpu_limit_cores",
			"CPU quota in cores. Absent when the container has no quota.").
			Add(float64(quota)/float64(period), labels...)
	}

	// cpu.shares is v1's relative weight. Its scale is not v2's: the default is
	// 1024 where cpu.weight defaults to 100. Converted rather than passed
	// through, so prickle_container_cpu_weight means one thing on both
	// hierarchies — SPEC.md §Hard constraints #4 forbids the contract forking.
	if v, err := readV1Int(cg.ctrlPath("cpu.shares", "cpu,cpuacct", "cpu")); err == nil {
		out.Gauge(prefix+"cpu_weight",
			"Relative CPU weight against sibling cgroups, on the cgroup v2 scale of 1 to 10000.").
			Add(math.Round(float64(v)*100/1024), labels...)
	}

	if stat, err := readFlatKeyed(cg.ctrlPath("cpu.stat", "cpu,cpuacct", "cpu")); err == nil {
		if v, ok := stat["nr_periods"]; ok {
			out.Counter(prefix+"cpu_periods_total",
				"Scheduling periods elapsed.").Add(float64(v), labels...)
		}
		if v, ok := stat["nr_throttled"]; ok {
			out.Counter(prefix+"cpu_throttled_periods_total",
				"Scheduling periods in which the container was throttled.").Add(float64(v), labels...)
		}
		if v, ok := stat["throttled_time"]; ok {
			out.Counter(prefix+"cpu_throttled_seconds_total",
				"Time the container spent throttled.").Add(float64(v)/1e9, labels...)
		}
	}
	return errors.Join(errs...)
}

// v1MemoryStat maps the v1 memory.stat field names onto the metrics the v2
// path emits from its own, differently-spelled fields.
//
// The names are not translations of each other. v1's `rss` counts anonymous
// pages, which is what v2 calls `anon`; v1's `cache` is v2's `file`. Where v1
// has no equivalent at all — v2's slab, sock, kernel_stack and page_tables —
// the family is simply absent on this hierarchy, which is honest: the kernel
// is not accounting it, so there is no number to report.
// The help text is v2's, verbatim: exposition rejects a family declared with
// two different help strings, and identical text is also the plainest statement
// that these are the same metric read from a different file.
//
// v1's `mapped_file` is deliberately not here. It has no counterpart in the v2
// set, and adding a family that exists only on one hierarchy is exactly the
// fork SPEC.md §Hard constraints #4 forbids — a dashboard built against it
// would go blank the day the host booted with the unified hierarchy.
var v1MemoryStat = []struct{ key, name, help string }{
	{"rss", "memory_anon_bytes", "Anonymous memory: heap and stack, not backed by a file, reclaimable only to swap."},
	{"cache", "memory_file_bytes", "Page cache."},
	{"inactive_file", "memory_inactive_file_bytes", "Page cache on the inactive LRU list, the first thing reclaimed under pressure. Subtract it from memory_usage_bytes for the working set."},
	{"shmem", "memory_shmem_bytes", "Shared memory and tmpfs, charged to the container that faulted it in."},
	{"unevictable", "memory_unevictable_bytes", "Memory that cannot be reclaimed at all, such as mlock'd pages."},
}

// collectMemory reads the v1 memory controller.
func (h v1Hierarchy) collectMemory(out *exposition.Set, cg cgroup, labels []exposition.Label) error {
	var errs []error

	if v, err := readV1Int(cg.ctrlPath("memory.usage_in_bytes", "memory")); err == nil {
		out.Gauge(prefix+"memory_usage_bytes", "Memory in use.").Add(float64(v), labels...)
	} else {
		errs = append(errs, skipMissing(err))
	}

	// Absent rather than a sentinel when unlimited, matching what the v2 path
	// does with `max`: a limit of nine exabytes on every unconstrained
	// container would dominate any capacity panel that sums it.
	if v, err := readV1Int(cg.ctrlPath("memory.limit_in_bytes", "memory")); err == nil && v < v1Unlimited {
		out.Gauge(prefix+"memory_limit_bytes",
			"Memory limit. Absent when the container is unlimited.").Add(float64(v), labels...)
	}

	stat, err := readFlatKeyed(cg.ctrlPath("memory.stat", "memory"))
	if err != nil {
		return errors.Join(append(errs, skipMissing(err))...)
	}
	for _, f := range v1MemoryStat {
		if v, ok := stat[f.key]; ok {
			out.Gauge(prefix+f.name, f.help).Add(float64(v), labels...)
		}
	}
	if v, ok := stat["pgfault"]; ok {
		out.Counter(prefix+"memory_page_faults_total", "Page faults.").Add(float64(v), labels...)
	}
	if v, ok := stat["pgmajfault"]; ok {
		out.Counter(prefix+"memory_major_page_faults_total", "Major page faults.").Add(float64(v), labels...)
	}
	return errors.Join(errs...)
}

// v1BlkioFiles maps a blkio file and its operation column onto a metric.
//
// v1 reports one line per device *per operation* — `252:0 Read 8704` — where
// v2 packs every counter for a device onto one line. The throttle.* files are
// the ones that count every I/O rather than only those the throttler acted on,
// which is what the v2 io.stat figures are.
var v1BlkioFiles = []struct{ file, op, name, help string }{
	{"blkio.throttle.io_service_bytes", "Read", "io_read_bytes_total", "Bytes read from block devices."},
	{"blkio.throttle.io_service_bytes", "Write", "io_written_bytes_total", "Bytes written to block devices."},
	{"blkio.throttle.io_serviced", "Read", "io_reads_completed_total", "Read operations completed."},
	{"blkio.throttle.io_serviced", "Write", "io_writes_completed_total", "Write operations completed."},
	{"blkio.throttle.io_service_bytes", "Discard", "io_discarded_bytes_total", "Bytes discarded."},
	{"blkio.throttle.io_serviced", "Discard", "io_discards_completed_total", "Discard operations completed."},
}

// collectIO reads the v1 blkio controller.
func (h v1Hierarchy) collectIO(devices map[string]string) source {
	return func(out *exposition.Set, cg cgroup, labels []exposition.Label) error {
		// Read each file once; several metrics come out of each.
		parsed := map[string]map[string]map[string]uint64{} // file -> dev -> op -> value
		var errs []error
		for _, file := range []string{"blkio.throttle.io_service_bytes", "blkio.throttle.io_serviced"} {
			lines, err := readLines(cg.ctrlPath(file, "blkio"))
			if err != nil {
				errs = append(errs, skipMissing(err))
				continue
			}
			byDev := map[string]map[string]uint64{}
			for _, line := range lines {
				f := strings.Fields(line)
				// The file ends with a bare `Total <n>` summing every device,
				// which has no device to label and would double every sum.
				if len(f) != 3 {
					continue
				}
				v, err := strconv.ParseUint(f[2], 10, 64)
				if err != nil {
					continue
				}
				if byDev[f[0]] == nil {
					byDev[f[0]] = map[string]uint64{}
				}
				byDev[f[0]][f[1]] = v
			}
			parsed[file] = byDev
		}

		for _, m := range v1BlkioFiles {
			for dev, ops := range parsed[m.file] {
				v, ok := ops[m.op]
				if !ok {
					continue
				}
				name := devices[dev]
				if name == "" {
					name = dev
				}
				out.Counter(prefix+m.name, m.help).
					Add(float64(v), with(labels, exposition.L("device", name))...)
			}
		}
		return errors.Join(errs...)
	}
}

// collectPIDs reads the v1 pids controller. The file names and the `max`
// sentinel are the same as v2's, which is why this is the shortest of them.
func (h v1Hierarchy) collectPIDs(out *exposition.Set, cg cgroup, labels []exposition.Label) error {
	if v, err := readV1Int(cg.ctrlPath("pids.current", "pids")); err == nil {
		out.Gauge(prefix+"processes", "Processes and threads in the container.").
			Add(float64(v), labels...)
	}
	if text, err := readFile(cg.ctrlPath("pids.max", "pids")); err == nil && text != unlimited {
		if v, err := strconv.ParseUint(text, 10, 64); err == nil {
			out.Gauge(prefix+"processes_limit",
				"Process limit. Absent when the container is unlimited.").
				Add(float64(v), labels...)
		}
	}
	return nil
}

// readV1Int reads a file holding a single integer.
func readV1Int(path string) (int64, error) {
	text, err := readFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(text, 10, 64)
}

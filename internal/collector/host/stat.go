// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"fmt"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// cpuModes names the per-mode columns of a /proc/stat cpu line, in file order.
//
// The captured fixture (Linux 5.15) has all ten. Older kernels stop earlier;
// the loop below walks whichever columns are present rather than requiring the
// full set.
var cpuModes = []string{
	"user", "nice", "system", "idle", "iowait",
	"irq", "softirq", "steal", "guest", "guest_nice",
}

// collectStat parses /proc/stat.
//
//	cpu  7699 378 4944 4840571 656 0 133 5 0 0
//	cpu0 247 0 357 201436 9 0 29 1 0 0
//	intr 5048544 15 9 0 ...
//	ctxt 8766266
//	btime 1785042511
//	processes 12744
//	procs_running 1
//	procs_blocked 0
//	softirq 740265 4 117952 ...
//
// The aggregate `cpu` line and the per-core `cpuN` lines go to two different
// families: mixing them would double-count every rate() and give the same
// family two label dimensions (SPEC.md §Collectors).
func (c *Collector) collectStat(out *exposition.Set) error {
	path := c.opts.Roots.ProcPath("stat")
	lines, err := readLines(path)
	if err != nil {
		return err
	}

	cpuTime := out.Counter(prefix+"cpu_seconds_total",
		"Seconds the CPUs spent in each mode, summed across all cores.")
	var coreTime *exposition.Family
	if c.opts.PerCoreCPU {
		coreTime = out.Counter(prefix+"cpu_core_seconds_total",
			"Seconds each individual core spent in each mode.")
	}

	var errs []error
	for _, line := range lines {
		key, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)

		switch {
		case key == "cpu":
			errs = append(errs, addCPUModes(path, cpuTime, rest)...)

		case strings.HasPrefix(key, "cpu"):
			if coreTime == nil {
				continue
			}
			core := exposition.L("cpu", strings.TrimPrefix(key, "cpu"))
			errs = append(errs, addCPUModes(path, coreTime, rest, core)...)

		case key == "intr", key == "softirq":
			// Both lines are a total followed by a per-vector breakdown. Only
			// the total is exposed: the vector list is hundreds of columns
			// wide, its indices are not stable across boots, and nothing joins
			// against them.
			total, _, _ := strings.Cut(rest, " ")
			name, help := prefix+"interrupts_total", "Interrupts serviced since boot, all vectors."
			if key == "softirq" {
				name, help = prefix+"softirqs_total", "Softirqs serviced since boot, all vectors."
			}
			v, err := parseUint(path, key, total)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			out.Counter(name, help).Add(float64(v))

		case key == "ctxt":
			errs = append(errs, addScalar(out.Counter(prefix+"context_switches_total",
				"Context switches since boot."), path, key, rest))

		case key == "processes":
			errs = append(errs, addScalar(out.Counter(prefix+"forks_total",
				"Processes and threads forked since boot."), path, key, rest))

		case key == "procs_running":
			errs = append(errs, addScalar(out.Gauge(prefix+"procs_running",
				"Processes in the runnable state."), path, key, rest))

		case key == "procs_blocked":
			errs = append(errs, addScalar(out.Gauge(prefix+"procs_blocked",
				"Processes blocked waiting for I/O."), path, key, rest))

		case key == "btime":
			errs = append(errs, addScalar(out.Gauge(prefix+"boot_time_seconds",
				"Unix timestamp at which the host booted."), path, key, rest))
		}
	}
	return errors.Join(errs...)
}

// addCPUModes splits a cpu line's columns across the mode label.
func addCPUModes(path string, f *exposition.Family, rest string, extra ...exposition.Label) []error {
	var errs []error
	for i, col := range strings.Fields(rest) {
		if i >= len(cpuModes) {
			// A kernel newer than the mode table. Ignore rather than guess:
			// SPEC.md §Testing rules — no fixture, no format.
			break
		}
		ticks, err := parseUint(path, "cpu."+cpuModes[i], col)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		labels := append(append([]exposition.Label{}, extra...), exposition.L("mode", cpuModes[i]))
		f.Add(float64(ticks)/userHZ, labels...)
	}
	return errs
}

// addScalar handles the single-value lines of /proc/stat.
func addScalar(f *exposition.Family, path, key, rest string) error {
	v, err := parseUint(path, key, strings.TrimSpace(rest))
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	f.Add(float64(v))
	return nil
}

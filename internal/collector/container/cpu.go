// SPDX-License-Identifier: Apache-2.0

package container

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// cpuModes are the two columns of cpu.stat that decompose the total, and the
// `mode` label value each carries.
//
// SPEC.md §Metrics contract names `mode` on CPU time as the example of a
// dimensional label: its values are enumerable from the file itself and mean
// nothing away from it.
var cpuModes = []struct{ key, mode string }{
	{"user_usec", "user"},
	{"system_usec", "system"},
}

// collectCPU parses cpu.stat, cpu.max and cpu.weight.
//
//	usage_usec 51099
//	user_usec 22710
//	system_usec 28388
//	nr_periods 0
//	nr_throttled 0
//	throttled_usec 0
//
// The captured tree has no container with a CPU quota, so nr_periods and
// nr_throttled are zero throughout it: cpu.max is `max 100000` everywhere. The
// counters are still emitted — a zero throttle count is the answer to "is this
// container being throttled", and a family that appears only once throttling
// starts cannot be alerted on.
func (c *Collector) collectCPU(out *exposition.Set, cg cgroup, labels []exposition.Label) error {
	var errs []error

	path := filepath.Join(cg.dir, "cpu.stat")
	stat, err := readFlatKeyed(path)
	if err != nil {
		errs = append(errs, skipMissing(err))
	}

	if v, ok := stat["usage_usec"]; ok {
		out.Counter(prefix+"cpu_usage_seconds_total",
			"Total CPU time consumed. The sum of the user and system modes below, from the kernel's own accounting.").
			Add(float64(v)/1e6, labels...)
	}
	for _, m := range cpuModes {
		v, ok := stat[m.key]
		if !ok {
			continue
		}
		out.Counter(prefix+"cpu_seconds_total",
			"CPU time consumed, split by mode.").
			Add(float64(v)/1e6, with(labels, exposition.L("mode", m.mode))...)
	}
	// divisor, not a multiplier by 1e-6: 1e6 is exactly representable in
	// float64 and 1e-6 is not, so dividing gives the correctly rounded second
	// count where multiplying can land a microsecond off the printed value.
	for _, f := range []struct {
		key, name, help string
		divisor         float64
	}{
		{"nr_periods", "cpu_periods_total", "Bandwidth-enforcement periods elapsed. Zero until a CPU quota is set.", 1},
		{"nr_throttled", "cpu_throttled_periods_total", "Periods in which the container exhausted its CPU quota and was throttled.", 1},
		{"throttled_usec", "cpu_throttled_seconds_total", "Seconds the container spent throttled against its CPU quota.", 1e6},
	} {
		if v, ok := stat[f.key]; ok {
			out.Counter(prefix+f.name, f.help).Add(float64(v)/f.divisor, labels...)
		}
	}

	if err := c.collectCPULimit(out, cg, labels); err != nil {
		errs = append(errs, err)
	}

	path = filepath.Join(cg.dir, "cpu.weight")
	if text, err := readFile(path); err != nil {
		errs = append(errs, skipMissing(err))
	} else if weight, err := parseUint(path, text); err != nil {
		errs = append(errs, err)
	} else {
		out.Gauge(prefix+"cpu_weight",
			"Relative share of CPU time under contention. The cgroup v2 cpu.weight, 1 to 10000, default 100.").
			Add(float64(weight), labels...)
	}

	return errors.Join(errs...)
}

// collectCPULimit parses cpu.max.
//
//	max 100000
//	400000 100000
//
// Two fields: the quota in microseconds per period, and the period itself. The
// limit is reported in cores — quota divided by period — because that is the
// number the operator set, the number `kubectl describe` shows, and the number
// that compares directly against a rate() of cpu_usage_seconds_total. An
// unlimited container gets no sample at all rather than a sentinel.
func (c *Collector) collectCPULimit(out *exposition.Set, cg cgroup, labels []exposition.Label) error {
	path := filepath.Join(cg.dir, "cpu.max")
	text, err := readFile(path)
	if err != nil {
		return skipMissing(err)
	}
	quota, period, ok := strings.Cut(text, " ")
	if !ok {
		return fmt.Errorf("%s: expected %q, got %q", path, "<quota|max> <period>", text)
	}
	if quota == unlimited {
		return nil
	}
	q, err := parseUint(path, quota)
	if err != nil {
		return err
	}
	p, err := parseUint(path, period)
	if err != nil {
		return err
	}
	if p == 0 {
		return fmt.Errorf("%s: period is zero", path)
	}
	out.Gauge(prefix+"cpu_limit_cores",
		"CPU quota in cores: the cgroup v2 cpu.max quota divided by its period. Absent when unlimited.").
		Add(float64(q)/float64(p), labels...)
	return nil
}

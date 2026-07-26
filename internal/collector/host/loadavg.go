// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"fmt"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// loadFields names the first three columns of /proc/loadavg.
var loadFields = []struct{ name, help string }{
	{"load1", "1-minute load average."},
	{"load5", "5-minute load average."},
	{"load15", "15-minute load average."},
}

// collectLoadavg parses /proc/loadavg.
//
//	0.07 0.05 0.00 1/788 12731
//
// Only the three averages are exposed. The fourth column is runnable/total
// processes, already available from /proc/stat's procs_running, and the fifth
// is the most recently created PID — SPEC.md §Metrics contract: a PID never
// appears as a label or a metric value anywhere.
func (c *Collector) collectLoadavg(out *exposition.Set) error {
	path := c.opts.Roots.ProcPath("loadavg")
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		return fmt.Errorf("%s: empty", path)
	}

	fields := strings.Fields(lines[0])
	if len(fields) < len(loadFields) {
		return fmt.Errorf("%s: expected at least %d fields, got %d", path, len(loadFields), len(fields))
	}

	var errs []error
	for i, lf := range loadFields {
		v, err := parseFloat(path, lf.name, fields[i])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out.Gauge(prefix+lf.name, lf.help).Add(v)
	}
	return errors.Join(errs...)
}

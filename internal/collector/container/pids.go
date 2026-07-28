// SPDX-License-Identifier: Apache-2.0

package container

import (
	"errors"
	"path/filepath"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// collectPIDs parses pids.current and pids.max.
//
// These are counts and a limit, never an identifier: SPEC.md §Metrics contract
// forbids a PID as a label or a value, and the file that would leak one —
// cgroup.procs — is deliberately never read (see the package comment).
func (c *Collector) collectPIDs(out *exposition.Set, cg cgroup, labels []exposition.Label) error {
	var errs []error

	path := filepath.Join(cg.dir, "pids.current")
	if text, err := readFile(path); err != nil {
		errs = append(errs, skipMissing(err))
	} else if v, err := parseUint(path, text); err != nil {
		errs = append(errs, err)
	} else {
		out.Gauge(prefix+"processes",
			"Processes and threads currently in the container's cgroup.").
			Add(float64(v), labels...)
	}

	path = filepath.Join(cg.dir, "pids.max")
	if v, set, err := readLimit(path); err != nil {
		errs = append(errs, skipMissing(err))
	} else if set {
		out.Gauge(prefix+"processes_limit",
			"Maximum processes and threads the container may create. Absent when unlimited.").
			Add(float64(v), labels...)
	}

	return errors.Join(errs...)
}

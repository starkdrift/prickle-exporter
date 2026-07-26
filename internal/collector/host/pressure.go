// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// psiResources are the files under /proc/pressure this collector reads.
var psiResources = []string{"cpu", "memory", "io"}

// collectPressure parses /proc/pressure/{cpu,memory,io}.
//
//	some avg10=0.01 avg60=0.02 avg300=0.00 total=4788821
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=0
//
// Only `total` is exposed, as a counter of seconds stalled. The avg10/60/300
// columns are the kernel's own moving averages of exactly that counter — a
// Prometheus deployment computes those with rate() over whatever window the
// query needs, and three fixed windows per resource per kind would be twelve
// redundant series per node.
//
// A kernel built without CONFIG_PSI has no /proc/pressure at all. That is a
// supported configuration, not an error: the directory's absence is reported as
// no samples rather than a failed collection.
func (c *Collector) collectPressure(out *exposition.Set) error {
	stalled := out.Counter(prefix+"pressure_stalled_seconds_total",
		"Seconds of stall from resource pressure. kind=some: at least one task stalled; kind=full: every task stalled.")

	var errs []error
	for _, resource := range psiResources {
		path := c.opts.Roots.ProcPath("pressure", resource)
		lines, err := readLines(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // CONFIG_PSI is off, or this resource has no file.
			}
			errs = append(errs, err)
			continue
		}

		for _, line := range lines {
			kind, rest, ok := strings.Cut(line, " ")
			if !ok || (kind != "some" && kind != "full") {
				continue
			}
			total, ok := psiTotal(rest)
			if !ok {
				errs = append(errs, fmt.Errorf("%s: %s: no total= field", path, kind))
				continue
			}
			micros, err := parseUint(path, kind+".total", total)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			stalled.Add(float64(micros)/1e6,
				exposition.L("resource", resource),
				exposition.L("kind", kind))
		}
	}
	return errors.Join(errs...)
}

// psiTotal pulls the total= field out of a PSI line. The field is searched for
// by name rather than taken by position: the kernel has added columns to these
// lines before.
func psiTotal(rest string) (string, bool) {
	for _, field := range strings.Fields(rest) {
		if v, ok := strings.CutPrefix(field, "total="); ok {
			return v, true
		}
	}
	return "", false
}

// SPDX-License-Identifier: Apache-2.0

package container

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// psiResources are the per-cgroup pressure files, named to match the `resource`
// label values the host collector emits for /proc/pressure/*.
//
// Only memory.pressure is present in the captured tree — the capture script
// collects that one file. cpu.pressure and io.pressure are the identical PSI
// format on the identical interface, and are read here because container-level
// CPU and I/O stall are among the most useful signals the cgroup offers; a
// cgroup that does not have them is skipped, exactly as a kernel without
// CONFIG_PSI is. testdata/README.md records that they are not fixture-covered.
var psiResources = []string{"cpu", "io", "memory"}

// collectPressure parses the per-cgroup PSI files.
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=0
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=0
//
// Only `total` is exposed, for the reason given in the host collector: the
// avg10/60/300 columns are the kernel's own moving averages of that same
// counter, and Prometheus computes those with rate() over whatever window the
// query wants.
func (c *Collector) collectPressure(out *exposition.Set, cg cgroup, labels []exposition.Label) error {
	var errs []error
	for _, resource := range psiResources {
		path := filepath.Join(cg.dir, resource+".pressure")
		lines, err := readLines(path)
		if err != nil {
			errs = append(errs, skipMissing(err))
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
			micros, err := parseUint(path, total)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			psi := with(labels, exposition.L("resource", resource))
			psi = with(psi, exposition.L("kind", kind))
			out.Counter(prefix+"pressure_stalled_seconds_total",
				"Seconds of stall from resource pressure. kind=some: at least one task stalled; kind=full: every task stalled.").
				Add(float64(micros)/1e6, psi...)
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

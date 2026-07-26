// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/starkdrift/prickle-exporter/internal/collector/host"
	"github.com/starkdrift/prickle-exporter/internal/exposition"
	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// diagnose reports what the exporter can read on this host and what it would
// emit, without starting a server.
//
// It is the first thing to run when a scrape is empty or a series is missing:
// the usual causes are a filesystem the process cannot read, a cgroup v1 host
// (SPEC.md §Hard constraints #4), or a collector filter excluding more than
// intended.
func diagnose(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("prickle diagnose", flag.ContinueOnError)
	var cfg config
	cfg.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	roots := cfg.roots()
	fmt.Fprintf(w, "prickle %s\n\n", version)

	fmt.Fprintln(w, "Filesystem roots")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  procfs\t%s\n", roots.Proc)
	fmt.Fprintf(tw, "  sysfs\t%s\n", roots.Sys)
	fmt.Fprintf(tw, "  cgroupfs\t%s\n", roots.Cgroup)
	tw.Flush()

	node, err := cfg.nodeName()
	if err != nil {
		fmt.Fprintf(w, "\nnode label: UNRESOLVED (%v)\n", err)
	} else {
		fmt.Fprintf(w, "\nnode label: %s\n", node)
	}

	fmt.Fprintln(w, "\ncgroup")
	fmt.Fprintf(w, "  %s\n", describeCgroup(roots))

	fmt.Fprintln(w, "\nPhase 1 sources")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range phase1Sources(roots) {
		fmt.Fprintf(tw, "  %s\t%s\n", p, describeReadable(p))
	}
	tw.Flush()

	fmt.Fprintln(w, "\nGPU")
	fmt.Fprintln(w, "  NVIDIA source selection is Phase 3 and not implemented yet.")

	hostOpts, err := cfg.hostOptions()
	if err != nil {
		return err
	}
	set := exposition.NewSet(exposition.L("node", node))
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	start := time.Now()
	collectErr := host.New(hostOpts).Collect(ctx, set)
	elapsed := time.Since(start)

	rendered := set.String()
	fmt.Fprintf(w, "\nhost collector: %d series in %s\n", countSeries(rendered), elapsed.Round(time.Microsecond))
	if collectErr != nil {
		fmt.Fprintln(w, "  errors:")
		for _, line := range strings.Split(collectErr.Error(), "\n") {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
	if err := set.Err(); err != nil {
		// A naming or duplicate-series problem is a bug in the exporter, not a
		// property of the host. Surface it separately from collector errors.
		fmt.Fprintln(w, "  exposition problems:")
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
	return nil
}

// phase1Sources lists the files the host collector reads, in SPEC.md
// §Collectors order.
func phase1Sources(r fsroot.Roots) []string {
	return []string{
		r.ProcPath("stat"),
		r.ProcPath("meminfo"),
		r.ProcPath("diskstats"),
		r.ProcPath("net", "dev"),
		r.ProcPath("loadavg"),
		r.ProcPath("pressure", "cpu"),
		r.ProcPath("pressure", "memory"),
		r.ProcPath("pressure", "io"),
		r.ProcPath("mounts"),
	}
}

// describeReadable reports whether a source can actually be read, by reading
// it. Stat is not enough: procfs files report size 0 and permission failures
// show up on open, not on stat.
func describeReadable(path string) string {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing"
		}
		return fmt.Sprintf("UNREADABLE (%v)", err)
	}
	defer f.Close()

	buf := make([]byte, 1)
	if _, err := f.Read(buf); err != nil && err != io.EOF {
		return fmt.Sprintf("UNREADABLE (%v)", err)
	}
	return "ok"
}

// describeCgroup reports the cgroup hierarchy version.
//
// SPEC.md §Hard constraints #4: v1 and hybrid hosts are out of scope, and this
// is where that is said plainly rather than left to an empty Phase 2 scrape.
// The answer comes from /proc/mounts rather than a statfs magic number so it
// works against a fixture tree too.
func describeCgroup(roots fsroot.Roots) string {
	b, err := os.ReadFile(roots.ProcPath("mounts"))
	if err != nil {
		return fmt.Sprintf("UNKNOWN: cannot read mounts (%v)", err)
	}

	var v1, v2 bool
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		switch f[2] {
		case "cgroup2":
			v2 = true
		case "cgroup":
			v1 = true
		}
	}

	switch {
	case v2 && v1:
		return "HYBRID v1+v2 — out of scope. Phase 2 container metrics will be " +
			"incomplete; boot with systemd.unified_cgroup_hierarchy=1."
	case v2:
		return "v2 (unified) — supported."
	case v1:
		return "v1 — OUT OF SCOPE. prickle is cgroup v2 only; Phase 2 container " +
			"metrics will be empty on this host."
	default:
		return "no cgroup mount found."
	}
}

// countSeries counts sample lines, ignoring HELP and TYPE.
func countSeries(rendered string) int {
	var n int
	for _, line := range strings.Split(rendered, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			n++
		}
	}
	return n
}

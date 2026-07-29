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

	"github.com/starkdrift/prickle-exporter/internal/collector/container"
	"github.com/starkdrift/prickle-exporter/internal/collector/gpu"
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

	fmt.Fprintln(w, "\nPhase 2 containers")
	if err := describeContainers(w, cfg); err != nil {
		return err
	}

	fmt.Fprintln(w, "\nPhase 3 GPU")
	describeGPUs(w, cfg)

	collectors, err := cfg.collectors()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	fmt.Fprintln(w)
	for _, c := range collectors {
		// A fresh Set per collector, so the series count below is that
		// collector's own and not a running total.
		one := exposition.NewSet(exposition.L("node", node))
		start := time.Now()
		collectErr := c.Collect(ctx, one)
		elapsed := time.Since(start)

		fmt.Fprintf(w, "%s collector: %d series in %s\n",
			c.Name(), countSeries(one.String()), elapsed.Round(time.Microsecond))
		if collectErr != nil {
			fmt.Fprintln(w, "  errors:")
			for _, line := range strings.Split(collectErr.Error(), "\n") {
				fmt.Fprintf(w, "    %s\n", line)
			}
		}
		if err := one.Err(); err != nil {
			// A naming or duplicate-series problem is a bug in the exporter,
			// not a property of the host. Surface it separately.
			fmt.Fprintln(w, "  exposition problems:")
			for _, line := range strings.Split(err.Error(), "\n") {
				fmt.Fprintf(w, "    %s\n", line)
			}
		}
	}
	return nil
}

// describeContainers reports what the cgroup walk finds, broken down the way
// the questions arrive: "why is my container missing" is almost always a
// runtime whose directory shape is not covered, a walk that cannot read the
// tree, or the collector being switched off.
func describeContainers(w io.Writer, cfg config) error {
	if !cfg.containers {
		fmt.Fprintln(w, "  disabled with -collector.container=false.")
		return nil
	}

	roots := cfg.roots()
	fmt.Fprintf(w, "  cgroup root: %s — %s\n", roots.CgroupPath(), describeWalkable(roots.CgroupPath()))

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	set := exposition.NewSet()
	if err := container.New(cfg.containerOptions()).Collect(ctx, set); err != nil {
		fmt.Fprintf(w, "  reported: %v\n", err)
	}

	// The _info gauge carries one sample per container found, with the runtime
	// it was identified by and the name enrichment did or did not supply.
	runtimes := map[string]int{}
	var total, named int
	for _, line := range strings.Split(set.String(), "\n") {
		if !strings.HasPrefix(line, "prickle_container_info{") {
			continue
		}
		total++
		if !strings.Contains(line, `name=""`) {
			named++
		}
		for _, r := range []string{"docker", "containerd", "crio"} {
			if strings.Contains(line, `runtime="`+r+`"`) {
				runtimes[r]++
			}
		}
	}

	if total == 0 {
		fmt.Fprintln(w, "  no containers found. On a host that is running some, this is")
		fmt.Fprintln(w, "  either cgroup v1 (see above), a cgroup tree this process cannot")
		fmt.Fprintln(w, "  read, or a runtime layout Phase 2 does not cover — see")
		fmt.Fprintln(w, "  internal/collector/container/testdata/README.md §Coverage gaps.")
		return nil
	}

	fmt.Fprintf(w, "  containers found: %d (docker %d, containerd %d, crio %d)\n",
		total, runtimes["docker"], runtimes["containerd"], runtimes["crio"])

	if cfg.dockerSocket == "" {
		fmt.Fprintln(w, "  Docker enrichment: off. Names and images are absent from")
		fmt.Fprintln(w, "  prickle_container_info; -collector.container.docker-socket turns it on.")
	} else {
		fmt.Fprintf(w, "  Docker enrichment: %s, %d of %d Docker containers named\n",
			cfg.dockerSocket, named, runtimes["docker"])
	}
	return nil
}

// describeGPUs reports which NVIDIA implementation is live and, when none is,
// why — SPEC.md §Collectors requires exactly that of `prickle diagnose`.
//
// "NVML failed to load" and "there is no GPU here" need different responses
// from an operator, and an empty GPU section distinguishes neither.
func describeGPUs(w io.Writer, cfg config) {
	if !cfg.gpus {
		fmt.Fprintln(w, "  disabled with -collector.gpu=false.")
		return
	}

	fmt.Fprintf(w, "  this binary: %s\n", gpuBuildDescription())

	c := gpu.New(cfg.gpuOptions())
	defer c.Close()

	if name := c.SourceName(); name != "" {
		fmt.Fprintf(w, "  live source: %s\n", name)
	} else {
		fmt.Fprintln(w, "  live source: none — no NVIDIA metrics will be exposed.")
		if err := c.SelectionError(); err != nil {
			for _, line := range strings.Split(err.Error(), "\n") {
				fmt.Fprintf(w, "    %s\n", line)
			}
		}
		// Only true of automatic selection. Saying it after an operator forced
		// a source that this build or host cannot provide reads as "you have no
		// GPU" on a machine that has one — which is the opposite of the answer
		// they came for.
		if cfg.nvidiaSource == gpu.SourceAuto {
			fmt.Fprintln(w, "  On a host with no NVIDIA GPU this is expected and not an error.")
		} else {
			fmt.Fprintf(w, "  -collector.gpu.nvidia-source=%s forced this source; %s selects one automatically.\n",
				cfg.nvidiaSource, gpu.SourceAuto)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	set := exposition.NewSet()
	if err := c.Collect(ctx, set); err != nil {
		fmt.Fprintf(w, "  reported: %v\n", err)
	}

	rendered := set.String()
	fmt.Fprintf(w, "  GPUs: %d, MIG instances: %d\n",
		countMatching(rendered, "prickle_gpu_info{"),
		countMatching(rendered, "prickle_gpu_mig_info{"))

	if !cfg.gpuPerProcess {
		fmt.Fprintln(w, "  per-process attribution: off (-collector.gpu.per-process turns it on).")
	}
	fmt.Fprintln(w, "  AMD is SPEC.md §Collectors scope but unimplemented: no capture")
	fmt.Fprintln(w, "  exists for it, so an AMD host reports nothing. Intel is out of")
	fmt.Fprintln(w, "  scope. See")
	fmt.Fprintln(w, "  internal/collector/gpu/testdata/README.md §Coverage gaps.")
}

// gpuBuildDescription names which of the two artifacts this is.
//
// SPEC.md §Distribution ships both, and "why is my source smi when I asked for
// nvml" is answered here rather than by the operator comparing file sizes.
func gpuBuildDescription() string {
	if gpu.NVMLBuilt {
		return "prickle-nvml — NVML available, nvidia-smi as fallback"
	}
	return "prickle (static) — nvidia-smi only; a static binary cannot dlopen NVML"
}

// countMatching counts sample lines starting with prefix.
func countMatching(rendered, prefix string) int {
	var n int
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
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

// describeWalkable reports whether a directory can be listed, by listing it.
//
// The container collector's input is a tree, not a file, so describeReadable's
// one-byte read is the wrong question — it reports "is a directory" for a
// perfectly good cgroup mount. What matters here is whether the walk can
// enumerate entries and how many it finds at the top level.
func describeWalkable(path string) string {
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing — no cgroup v2 mount here"
		}
		return fmt.Sprintf("UNREADABLE (%v)", err)
	}
	var dirs int
	for _, e := range entries {
		if e.IsDir() {
			dirs++
		}
	}
	return fmt.Sprintf("ok, %d top-level cgroups", dirs)
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

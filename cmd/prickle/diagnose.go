// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
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

	// "My metric disappeared" must have an answer that is not reading the
	// source. Printed before the collectors so it frames everything below it.
	fmt.Fprintln(w, "\nmetric selection")
	if sel, err := cfg.selector(); err != nil {
		fmt.Fprintf(w, "  INVALID: %v\n", err)
	} else {
		one := exposition.NewSet()
		one.Select(sel)
		if cs, err := cfg.collectors(); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
			for _, c := range cs {
				_ = c.Collect(ctx, one)
			}
			cancel()
		}
		switch cfg.metricsPreset {
		case exposition.PresetFull:
			fmt.Fprintln(w, "  -metrics.preset=full — every family the collectors produce.")
		case exposition.PresetCustom:
			fmt.Fprintf(w, "  -metrics.preset=custom — %s\n", cfg.metricsInclude)
		default:
			fmt.Fprintln(w, "  -metrics.preset=minimal (default) — what the shipped dashboards query.")
		}
		fmt.Fprintf(w, "  exposing %d families; %d withheld by the selection.\n",
			countFamilies(one.String()), one.Withheld())
		if one.Withheld() > 0 {
			fmt.Fprintln(w, "  -metrics.preset=full exposes them. Self-metrics are never withheld.")
		}
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

// knownRuntimes are the runtime label values a cgroup directory name can carry,
// in the order diagnose reports them. It must track scopePrefixes in the
// container package: a runtime missing here is counted as "unknown" and blamed
// on a cgroupfs tree that names no runtime, which is a confident wrong answer.
var knownRuntimes = []string{"docker", "containerd", "crio", "podman"}

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
	_, v2, _ := cgroupVersions(roots)
	fmt.Fprintf(w, "  cgroup root: %s — %s\n", roots.CgroupPath(), describeWalkable(roots.CgroupPath(), v2))

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	set := exposition.NewSet()
	if err := container.New(cfg.containerOptions()).Collect(ctx, set); err != nil {
		fmt.Fprintf(w, "  reported: %v\n", err)
	}

	// The _info gauge carries one sample per container found, with the runtime
	// it was identified by and the name enrichment did or did not supply.
	runtimes := map[string]int{}
	var total, named, inPod, podNamed int
	for _, line := range strings.Split(set.String(), "\n") {
		if !strings.HasPrefix(line, "prickle_container_info{") {
			continue
		}
		total++
		if !strings.Contains(line, `name=""`) {
			named++
		}
		// `pod` is the UID the cgroup walk produced; a container outside a pod
		// carries an empty one. `pod_name` exists on the series only while
		// -collector.container.pod-names is on, so an absent label and an
		// empty one both mean "not resolved" and both are counted the same.
		if !strings.Contains(line, `pod=""`) {
			inPod++
			if strings.Contains(line, `pod_name="`) && !strings.Contains(line, `pod_name=""`) {
				podNamed++
			}
		}
		for _, r := range knownRuntimes {
			if strings.Contains(line, `runtime="`+r+`"`) {
				runtimes[r]++
			}
		}
	}

	if total == 0 {
		fmt.Fprintln(w, "  no containers found. On a host that is running some, this is")
		fmt.Fprintln(w, "  either a cgroup tree this process cannot read, or a runtime")
		fmt.Fprintln(w, "  layout Phase 2 does not cover — see")
		fmt.Fprintln(w, "  internal/collector/container/testdata/README.md §Coverage gaps.")
		fmt.Fprintln(w, "  cgroup v1 is no longer a cause: both hierarchies are read.")
		return nil
	}

	counts := make([]string, 0, len(knownRuntimes))
	for _, r := range knownRuntimes {
		counts = append(counts, fmt.Sprintf("%s %d", r, runtimes[r]))
	}
	fmt.Fprintf(w, "  containers found: %d (%s)\n", total, strings.Join(counts, ", "))

	// Under the cgroupfs cgroup driver the directory names carry no runtime, so
	// every count above is zero while containers are plainly being found.
	// Without this line that reads as three failed identifications rather than
	// one attribute the layout does not publish.
	attributed := 0
	for _, r := range knownRuntimes {
		attributed += runtimes[r]
	}
	if unknown := total - attributed; unknown > 0 {
		fmt.Fprintf(w, "  %d of those sit in a cgroupfs-driver tree, whose directory names do\n", unknown)
		fmt.Fprintln(w, "  not name a runtime; they carry an empty `runtime` on")
		fmt.Fprintln(w, "  prickle_container_info. Not an error — the tree has nothing to read.")
	}

	if cfg.dockerSocket == "" {
		fmt.Fprintln(w, "  Docker enrichment: off. Names and images are absent from")
		fmt.Fprintln(w, "  prickle_container_info; -collector.container.docker-socket turns it on.")
	} else {
		fmt.Fprintf(w, "  Docker enrichment: %s, %d of %d Docker containers named\n",
			cfg.dockerSocket, named, runtimes["docker"])
	}

	describePodNames(w, cfg, readPodLogs(roots), inPod, podNamed)
	return nil
}

// podLogs is what diagnose could learn about the kubelet's pod log directory.
// Split from the reporting so the reporting is testable without a filesystem
// that can produce an EACCES — which a test running as root cannot arrange.
type podLogs struct {
	path string
	err  error
	dirs int
}

// readPodLogs lists the pod log directory, exactly as the collector does.
func readPodLogs(roots fsroot.Roots) podLogs {
	st := podLogs{path: roots.PodLogsPath()}
	entries, err := os.ReadDir(st.path)
	if err != nil {
		st.err = err
		return st
	}
	for _, e := range entries {
		if e.IsDir() {
			st.dirs++
		}
	}
	return st
}

// describePodNames reports whether pod-name resolution is on and, when it is
// on and producing nothing, why.
//
// This section exists because that failure is otherwise **completely silent**.
// An unreadable pod log directory is deliberately not a collection error — the
// container metrics are unaffected and only the names are missing, so raising
// prickle_collector_errors_total forever would be wrong — with the result that
// every container is still reported, every one of them identified by UID, and
// nothing anywhere says the privilege is missing. Measured on 2026-08-04:
// forcing the chart's pod to gid 65532 left 18 of 18 containers unnamed while
// this subcommand printed an entirely healthy report.
func describePodNames(w io.Writer, cfg config, st podLogs, inPod, podNamed int) {
	if !cfg.podNames {
		fmt.Fprintln(w, "  pod names: off. A container in a pod is identified by the pod's UID;")
		fmt.Fprintln(w, "  -collector.container.pod-names resolves the name and namespace.")
		return
	}

	switch {
	case errors.Is(st.err, fs.ErrPermission):
		fmt.Fprintf(w, "  pod names: ON, AND READING NOTHING — %s: permission denied.\n", st.path)
		fmt.Fprintf(w, "  Running as uid=%d gid=%d. That directory is 0750 root:root, so\n", os.Getuid(), os.Getgid())
		fmt.Fprintln(w, "  reading it needs uid 0, membership of group root (the Helm chart")
		fmt.Fprintln(w, "  sets runAsGroup: 0), or — under systemd only — ambient")
		fmt.Fprintln(w, "  CAP_DAC_READ_SEARCH. A capability added to a non-root uid on")
		fmt.Fprintln(w, "  Kubernetes lands in the bounding set alone and does nothing.")
		fmt.Fprintf(w, "  Every container is still reported: %d of them are in a pod and\n", inPod)
		fmt.Fprintln(w, "  carry a UID with no name.")
	case errors.Is(st.err, fs.ErrNotExist):
		fmt.Fprintf(w, "  pod names: on, but %s does not exist. Expected on a host that is\n", st.path)
		fmt.Fprintln(w, "  not a Kubernetes node — the kubelet is what creates it. Containers")
		fmt.Fprintln(w, "  started by hand are in no pod and have no name to resolve.")
	case st.err != nil:
		fmt.Fprintf(w, "  pod names: on, but %s could not be listed: %v\n", st.path, st.err)
	case inPod == 0:
		fmt.Fprintf(w, "  pod names: on, %s holds %d pod directories, and no container found\n", st.path, st.dirs)
		fmt.Fprintln(w, "  here is in a pod — so there is nothing to resolve.")
	case podNamed < inPod:
		fmt.Fprintf(w, "  pod names: on — %d of %d containers in a pod resolved, from %s.\n",
			podNamed, inPod, st.path)
		fmt.Fprintln(w, "  The rest are pods the kubelet has no log directory for, usually")
		fmt.Fprintln(w, "  because it was pruned after the pod exited.")
	default:
		fmt.Fprintf(w, "  pod names: on — %d of %d containers in a pod resolved, from %s.\n",
			podNamed, inPod, st.path)
	}
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
			describeNVIDIAPresence(w, cfg)
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
		describeSMITimeout(w, cfg, c.SourceName(), err)
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

// describeSMITimeout explains the one nvidia-smi failure that looks like a bug
// in this exporter and is not.
//
// With NVIDIA persistence mode disabled and nothing else holding the device
// open, the driver tears down its state whenever the last client exits — so
// every nvidia-smi this collector spawns pays the initialisation cost afresh.
// Measured on an idle H100 (driver 580.173.02): ~2.7-3.5 s per invocation, and
// the source makes three per pass, which overruns the default 5 s deadline and
// reports a killed subprocess on every scrape. Enabling persistence mode took
// the same pass from 5.4 s to 61 ms.
//
// The NVML path does not have this problem: it holds the library open across
// passes, which keeps the driver initialised for the same reason persistence
// mode does.
func describeSMITimeout(w io.Writer, cfg config, source string, err error) {
	if source != gpu.SourceSMI {
		return
	}
	// A deadline overrun surfaces either as the context error or as the signal
	// that killed the subprocess, depending on which side noticed first.
	msg := err.Error()
	if !strings.Contains(msg, context.DeadlineExceeded.Error()) &&
		!strings.Contains(msg, "signal: killed") {
		return
	}

	fmt.Fprintf(w, "  That is a deadline overrun, not a broken nvidia-smi. This source spawns\n")
	fmt.Fprintf(w, "  several nvidia-smi calls per pass, and with NVIDIA persistence mode off\n")
	fmt.Fprintf(w, "  each one re-initialises the driver — seconds apiece on an idle card,\n")
	fmt.Fprintf(w, "  against a %s deadline. Any one of these fixes it:\n", cfg.timeout)
	fmt.Fprintln(w, "    - enable persistence mode: `nvidia-smi -pm 1`, or run nvidia-persistenced")
	fmt.Fprintln(w, "    - deploy prickle-nvml, which holds NVML open and never spawns anything")
	fmt.Fprintln(w, "    - raise -collector.timeout, which hides the latency rather than removing it")
}

// describeNVIDIAPresence says whether the host has an NVIDIA card at all.
//
// Reached only when automatic selection found no source. Until this existed,
// that printed "On a host with no NVIDIA GPU this is expected and not an
// error" unconditionally — which on a machine with a card and no driver is
// precisely the wrong answer, and the answer an operator is least equipped to
// doubt. The PCI bus knows the difference even when nothing else does: it
// advertises the card whether or not a driver is bound to it.
func describeNVIDIAPresence(w io.Writer, cfg config) {
	n, err := gpu.CountNVIDIAGPUs(cfg.roots())
	switch {
	case err != nil:
		// Do not guess in either direction. An unreadable PCI bus is its own
		// state, and the two confident answers below would both be inventions.
		fmt.Fprintf(w, "  Whether this host has an NVIDIA card could not be determined: %v\n", err)
	case n == 0:
		fmt.Fprintln(w, "  The PCI bus advertises no NVIDIA display adapter either, so this")
		fmt.Fprintln(w, "  host has no NVIDIA GPU and the absence of a source is expected,")
		fmt.Fprintln(w, "  not an error.")
	default:
		fmt.Fprintf(w, "  The PCI bus advertises %s, so the hardware is\n",
			plural(n, "NVIDIA display adapter"))
		fmt.Fprintln(w, "  present and the driver is not. Install the NVIDIA driver; until")
		fmt.Fprintln(w, "  then neither source can report on a card that is physically here.")
	}
}

// plural renders "1 thing" or "3 things".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
func describeWalkable(path string, cgroup2 bool) string {
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
	if !cgroup2 {
		// On a v1 host /sys/fs/cgroup is a tmpfs holding one directory per
		// controller — blkio, cpu,cpuacct, memory and the rest. Calling those
		// "top-level cgroups" would misreport what they are, even though the
		// walk now reads straight through them.
		return fmt.Sprintf("ok, %d v1 controller hierarchies", dirs)
	}
	return fmt.Sprintf("ok, %d top-level cgroups", dirs)
}

// cgroupVersions reports which cgroup hierarchies are mounted.
//
// From /proc/mounts rather than a statfs magic number, so it works against a
// captured fixture tree too — the same reason describeCgroup has always read
// it that way. Both hierarchies can be present at once: that is a hybrid host.
func cgroupVersions(roots fsroot.Roots) (v1, v2 bool, err error) {
	b, err := os.ReadFile(roots.ProcPath("mounts"))
	if err != nil {
		return false, false, err
	}
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
	return v1, v2, nil
}

// describeCgroup reports the cgroup hierarchy version.
//
// SPEC.md §Hard constraints #4: v1 and hybrid hosts are out of scope, and this
// is where that is said plainly rather than left to an empty Phase 2 scrape.
// The answer comes from /proc/mounts rather than a statfs magic number so it
// works against a fixture tree too.
func describeCgroup(roots fsroot.Roots) string {
	v1, v2, err := cgroupVersions(roots)
	if err != nil {
		return fmt.Sprintf("UNKNOWN: cannot read mounts (%v)", err)
	}

	switch {
	case v2 && v1:
		return "hybrid v1+v2 — both supported. Containers are read from " +
			"whichever hierarchy the runtime put them in, v2 first."
	case v2:
		return "v2 (unified) — supported."
	case v1:
		return "v1 — supported. This hierarchy has no PSI, so " +
			"prickle_container_pressure_stalled_seconds_total is absent here " +
			"rather than zero; everything else is reported."
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

// countFamilies counts HELP lines, which is one per exposed family.
func countFamilies(rendered string) int {
	var n int
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, "# HELP ") {
			n++
		}
	}
	return n
}

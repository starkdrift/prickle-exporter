// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/starkdrift/prickle-exporter/internal/collector"
	"github.com/starkdrift/prickle-exporter/internal/collector/container"
	"github.com/starkdrift/prickle-exporter/internal/collector/gpu"
	"github.com/starkdrift/prickle-exporter/internal/collector/host"
	"github.com/starkdrift/prickle-exporter/internal/exposition"
	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

// DefaultMaxSeries is the per-collector cardinality cap.
//
// SPEC.md §Metrics contract requires a cap that never OOMs the scrape. This is
// sized as a backstop rather than a budget: the largest thing measured so far
// is a Kubernetes node at a few thousand series, and a node would have to be
// two orders of magnitude past that before the cap is what stops it. A default
// low enough to bind on a real host would silently truncate one, which is a
// worse failure than the one being prevented — a truncated scrape looks like a
// healthy one.
const DefaultMaxSeries = 100_000

// config is the flag set shared by `prickle` and `prickle diagnose`, so the
// diagnostic reports on exactly the configuration the exporter would run with.
type config struct {
	listenAddress  string
	telemetryPath  string
	interval       time.Duration
	timeout        time.Duration
	maxSeries      int
	metricsPreset  string
	metricsInclude string

	node string

	fixtureRoot string
	procPath    string
	sysPath     string
	cgroupPath  string

	perCoreCPU          bool
	ignoredDisks        string
	ignoredNetDevices   string
	excludedFSTypes     string
	excludedMountPoints string

	containers    bool
	podNames      bool
	dockerSocket  string
	dockerTimeout time.Duration

	gpus          bool
	nvidiaSource  string
	gpuPerProcess bool
	smiCommand    string

	logLevel    string
	showVersion bool
}

func (c *config) register(fs *flag.FlagSet) {
	fs.StringVar(&c.listenAddress, "web.listen-address", ":10047",
		"Address to serve metrics on. SPEC.md §Identity fixes the port at 10047.")
	fs.StringVar(&c.telemetryPath, "web.telemetry-path", "/metrics",
		"Path under which to expose metrics.")
	fs.DurationVar(&c.interval, "sample.interval", 10*time.Second,
		"How often to poll collectors. Scrapes are served from the last completed pass.")
	fs.DurationVar(&c.timeout, "collector.timeout", 5*time.Second,
		"Deadline for a single collector's pass.")
	fs.IntVar(&c.maxSeries, "collector.max-series", DefaultMaxSeries,
		"Cap on the series one collector may contribute in a single pass. Past "+
			"it the extra samples are dropped and counted on "+
			"prickle_collector_series_dropped_total. A backstop against a "+
			"runaway cardinality source taking the process down, not a tuning "+
			"knob: the default is far above any real host. 0 disables it.")

	fs.StringVar(&c.metricsPreset, "metrics.preset", exposition.PresetMinimal,
		"How much to expose: `minimal` (what the shipped Grafana dashboards "+
			"query), full (every family the collectors produce), or custom "+
			"(-metrics.include). A host emits about 156 families and the "+
			"dashboards use 35, so the default withholds roughly three "+
			"quarters — full is one flag away. Self-metrics are exposed under "+
			"every preset.")
	fs.StringVar(&c.metricsInclude, "metrics.include", "",
		"Comma-separated regexps of metric families to expose, with "+
			"-metrics.preset=custom. Anchor them (^…$) unless a substring "+
			"match is what you want.")

	fs.StringVar(&c.node, "node", "",
		"Value of the `node` identity label. Empty means the system hostname.")

	fs.StringVar(&c.fixtureRoot, "path.rootfs", "",
		"Prefix all of /proc, /sys and the cgroup mount with this directory. For "+
			"running against a captured fixture tree, or from inside a container "+
			"with the host filesystem bind-mounted.")
	fs.StringVar(&c.procPath, "path.procfs", fsroot.DefaultProc, "procfs mount point.")
	fs.StringVar(&c.sysPath, "path.sysfs", fsroot.DefaultSys, "sysfs mount point.")
	fs.StringVar(&c.cgroupPath, "path.cgroupfs", fsroot.DefaultCgroup, "cgroup v2 mount point.")

	fs.BoolVar(&c.perCoreCPU, "collector.cpu.per-core", false,
		"Also expose per-core CPU time. Costs one series per core per mode.")
	fs.StringVar(&c.ignoredDisks, "collector.diskstats.ignored-devices",
		host.DefaultIgnoredDisks.String(), "Regexp of block devices to skip.")
	fs.StringVar(&c.ignoredNetDevices, "collector.netdev.ignored-devices", "",
		"Regexp of network interfaces to skip. Empty means none; `^veth` is the "+
			"usual choice on a Kubernetes node.")
	fs.StringVar(&c.excludedFSTypes, "collector.filesystem.excluded-fs-types",
		host.DefaultExcludedFSTypes.String(), "Regexp of filesystem types to skip.")
	fs.StringVar(&c.excludedMountPoints, "collector.filesystem.excluded-mount-points",
		host.DefaultExcludedMountPoints.String(), "Regexp of mount points to skip.")

	fs.BoolVar(&c.containers, "collector.container", true,
		"Walk the cgroup v2 tree and expose per-container metrics.")
	fs.StringVar(&c.dockerSocket, "collector.container.docker-socket", "",
		"Path to the Docker socket, usually `/var/run/docker.sock`. Enables one "+
			"GET request per pass for container names and images, which land on "+
			"prickle_container_info and never on a hot series. Empty — the "+
			"default — opens no socket at all.")
	fs.BoolVar(&c.podNames, "collector.container.pod-names", false,
		"Resolve a pod's UID to its namespace and name by listing the kubelet's "+
			"pod log directory. Adds `pod_name` to prickle_container_info and "+
			"`namespace` to container series. OFF BY DEFAULT because "+
			"/var/log/pods is 0750 root:root: reading it needs uid 0, "+
			"membership of group root, or — under systemd only — ambient "+
			"CAP_DAC_READ_SEARCH, which for a read-only exporter is close to "+
			"the same grant as root. Without it a pod is identified by UID, "+
			"as before.")
	fs.DurationVar(&c.dockerTimeout, "collector.container.docker-timeout",
		container.DefaultDockerTimeout,
		"Deadline for that request. A wedged daemon costs the names, not the metrics.")

	fs.BoolVar(&c.gpus, "collector.gpu", true,
		"Expose GPU metrics. NVIDIA only; AMD is Phase 3 scope with no captured "+
			"fixtures yet and reports nothing, and Intel is out of scope.")
	fs.StringVar(&c.nvidiaSource, "collector.gpu.nvidia-source", gpu.SourceAuto,
		"Force an NVIDIA implementation: `auto`, nvml or smi. auto tries NVML and "+
			"falls back to nvidia-smi. A debugging flag, not a tuning knob.")
	fs.BoolVar(&c.gpuPerProcess, "collector.gpu.per-process", false,
		"Also expose per-process GPU memory, keyed on the `command` label taken "+
			"from the executable's basename. Never a PID. Opt-in: it is one series "+
			"per distinct command per GPU.")
	fs.StringVar(&c.smiCommand, "collector.gpu.nvidia-smi-command", gpu.DefaultSMICommand,
		"The nvidia-smi binary to spawn, for hosts that keep it outside PATH.")

	fs.StringVar(&c.logLevel, "log.level", "info", "One of debug, info, warn, error.")
	fs.BoolVar(&c.showVersion, "version", false, "Print the version and exit.")
}

// roots resolves the filesystem prefixes. -path.rootfs wins over the
// individual flags: it exists precisely so an operator does not have to set
// three consistent paths by hand.
func (c *config) roots() fsroot.Roots {
	if c.fixtureRoot != "" {
		return fsroot.At(c.fixtureRoot)
	}
	return fsroot.Roots{Proc: c.procPath, Sys: c.sysPath, Cgroup: c.cgroupPath}
}

// hostOptions builds the Phase 1 collector's configuration, compiling the
// filter regexps so a typo fails at startup rather than at the first scrape.
func (c *config) hostOptions() (host.Options, error) {
	opts := host.Options{Roots: c.roots(), PerCoreCPU: c.perCoreCPU}

	for _, f := range []struct {
		flag string
		expr string
		dst  **regexp.Regexp
	}{
		{"collector.diskstats.ignored-devices", c.ignoredDisks, &opts.IgnoredDisks},
		{"collector.netdev.ignored-devices", c.ignoredNetDevices, &opts.IgnoredNetDevices},
		{"collector.filesystem.excluded-fs-types", c.excludedFSTypes, &opts.ExcludedFSTypes},
		{"collector.filesystem.excluded-mount-points", c.excludedMountPoints, &opts.ExcludedMountPoints},
	} {
		if f.expr == "" {
			continue // match nothing
		}
		re, err := regexp.Compile(f.expr)
		if err != nil {
			return host.Options{}, fmt.Errorf("-%s: %w", f.flag, err)
		}
		*f.dst = re
	}
	return opts, nil
}

// containerOptions builds the Phase 2 collector's configuration.
func (c *config) containerOptions() container.Options {
	return container.Options{
		Roots:         c.roots(),
		DockerSocket:  c.dockerSocket,
		DockerTimeout: c.dockerTimeout,
		PodNames:      c.podNames,
	}
}

// collectors builds the set the sampler polls, in the order SPEC.md §Collectors
// lists the phases.
func (c *config) collectors() ([]collector.Collector, error) {
	hostOpts, err := c.hostOptions()
	if err != nil {
		return nil, err
	}
	collectors := []collector.Collector{host.New(hostOpts)}
	if c.containers {
		collectors = append(collectors, container.New(c.containerOptions()))
	}
	if c.gpus {
		collectors = append(collectors, gpu.New(c.gpuOptions()))
	}
	return collectors, nil
}

// gpuOptions builds the Phase 3 collector's configuration.
func (c *config) gpuOptions() gpu.Options {
	return gpu.Options{
		Roots:        c.roots(),
		NVIDIASource: c.nvidiaSource,
		PerProcess:   c.gpuPerProcess,
		SMICommand:   c.smiCommand,
	}
}

// nodeName resolves the `node` identity label.
//
// The hostname is the default rather than a required flag so that a bare
// `prickle` on a laptop produces correct output; on a Kubernetes node the unit
// or manifest should pass -node explicitly, since the pod's view of the
// hostname is not the node's name.
func (c *config) nodeName() (string, error) {
	if c.node != "" {
		return c.node, nil
	}
	name, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("resolving hostname for the node label: %w", err)
	}
	return name, nil
}

func (c *config) logger() *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.logLevel)); err != nil {
		level = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// selector builds the metric selection from the flags, failing at startup
// rather than at the first scrape: a typo in a regexp should stop the process,
// not quietly withhold everything it was meant to match.
func (c *config) selector() (*exposition.Selector, error) {
	var include []string
	for _, p := range strings.Split(c.metricsInclude, ",") {
		if p = strings.TrimSpace(p); p != "" {
			include = append(include, p)
		}
	}
	return exposition.NewSelector(c.metricsPreset, include)
}

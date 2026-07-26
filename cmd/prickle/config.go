// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"time"

	"github.com/starkdrift/prickle-exporter/internal/collector/host"
	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

// config is the flag set shared by `prickle` and `prickle diagnose`, so the
// diagnostic reports on exactly the configuration the exporter would run with.
type config struct {
	listenAddress string
	telemetryPath string
	interval      time.Duration
	timeout       time.Duration

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

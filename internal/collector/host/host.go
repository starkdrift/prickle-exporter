// SPDX-License-Identifier: Apache-2.0

// Package host implements the Phase 1 host collector: /proc/stat, meminfo,
// diskstats, net/dev, loadavg, pressure/{cpu,memory,io}, and mounts + Statfs.
//
// Every parser in this package was developed against the captured fixture tree
// in testdata/ (SPEC.md §Testing rules). Nothing here guesses at a file format;
// where a field is absent on the captured kernel the code says so rather than
// inventing a shape for it.
//
// All paths are built through fsroot.Roots — no absolute /proc or /sys literal
// appears below (SPEC.md §Hard constraints #3).
package host

import (
	"context"
	"errors"
	"regexp"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// Metric name prefix for every family in this package.
const prefix = "prickle_host_"

// userHZ is the kernel's clock tick, the unit of the counters in /proc/stat.
//
// It is a compile-time constant of the kernel ABI (USER_HZ), fixed at 100 on
// every Linux architecture the exporter targets. Reading it properly means
// sysconf(_SC_CLK_TCK), which needs cgo — unavailable in the default
// CGO_ENABLED=0 build (SPEC.md §Distribution).
const userHZ = 100

// sectorSize is the unit of the sector counters in /proc/diskstats. It is
// always 512 there regardless of the device's real logical block size — the
// kernel normalises before writing the file.
const sectorSize = 512

// Default exclusion patterns. Each is a flag on the command line; the defaults
// aim at "everything an operator would plot", not at completeness.
var (
	// Loop, ramdisk and floppy devices carry no interesting I/O.
	DefaultIgnoredDisks = regexp.MustCompile(`^(ram|loop|fd|sr)\d+$`)

	// Nothing by default: veth churn on a Kubernetes node is real, but
	// silently hiding container networking is worse than the cardinality.
	// Phase 4's caps are the intended answer.
	DefaultIgnoredNetDevices *regexp.Regexp

	// Pseudo-filesystems with no meaningful capacity, plus overlay and squashfs
	// whose numbers duplicate the backing filesystem.
	DefaultExcludedFSTypes = regexp.MustCompile(`^(autofs|binfmt_misc|bpf|cgroup2?|configfs|debugfs|devpts|devtmpfs|fuse\.lxcfs|fusectl|hugetlbfs|iso9660|mqueue|nsfs|overlay|proc|procfs|pstore|ramfs|rpc_pipefs|securityfs|selinuxfs|squashfs|sysfs|tracefs)$`)

	// Per-container and per-pod mounts: one series per container lifetime,
	// all reporting the same backing filesystem's numbers.
	DefaultExcludedMountPoints = regexp.MustCompile(`^/(dev|proc|sys|run/credentials/.+|run/docker/.+|run/k3s/.+|run/snapd/.+|var/lib/docker/.+|var/lib/kubelet/.+|var/lib/rancher/.+)($|/)`)
)

// Options configures the host collector. The zero value is usable: it reads a
// live host with the defaults above and the real Statfs syscall.
type Options struct {
	// Roots is the set of filesystem prefixes to read through.
	Roots fsroot.Roots

	// PerCoreCPU adds the prickle_host_cpu_core_seconds_total family.
	// SPEC.md §Collectors: opt-in, because it costs 10 series per core.
	PerCoreCPU bool

	// Statfs is the filesystem-capacity source. Nil means the real syscall.
	// SPEC.md §Collectors: behind an interface, because a syscall cannot be
	// captured as a fixture file.
	Statfs Statfser

	IgnoredDisks        *regexp.Regexp
	IgnoredNetDevices   *regexp.Regexp
	ExcludedFSTypes     *regexp.Regexp
	ExcludedMountPoints *regexp.Regexp
}

// Collector reads the Phase 1 host sources.
type Collector struct {
	opts Options
}

// New returns a host collector. Unset Options fields take their defaults.
func New(opts Options) *Collector {
	if opts.IgnoredDisks == nil {
		opts.IgnoredDisks = DefaultIgnoredDisks
	}
	if opts.ExcludedFSTypes == nil {
		opts.ExcludedFSTypes = DefaultExcludedFSTypes
	}
	if opts.ExcludedMountPoints == nil {
		opts.ExcludedMountPoints = DefaultExcludedMountPoints
	}
	if opts.Statfs == nil {
		opts.Statfs = SyscallStatfs{}
	}
	return &Collector{opts: opts}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "host" }

// Collect implements collector.Collector.
//
// Each source is independent: a failure in one is recorded and the rest still
// run, so a kernel without PSI still reports CPU, memory and disk.
func (c *Collector) Collect(ctx context.Context, out *exposition.Set) error {
	sources := []struct {
		name string
		fn   func(*exposition.Set) error
	}{
		{"stat", c.collectStat},
		{"meminfo", c.collectMeminfo},
		{"diskstats", c.collectDiskstats},
		{"netdev", c.collectNetDev},
		{"loadavg", c.collectLoadavg},
		{"pressure", c.collectPressure},
		{"filesystem", c.collectFilesystem},
	}

	var errs []error
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := src.fn(out); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

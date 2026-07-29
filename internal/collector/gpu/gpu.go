// SPDX-License-Identifier: Apache-2.0

// Package gpu implements the Phase 3 GPU collector.
//
// SPEC.md §Collectors gives NVIDIA two interchangeable implementations behind
// one nvidiaSource interface: NVML via dlopen (preferred, needs cgo and a
// dynamically linked binary, so it lives behind //go:build nvml) and an
// nvidia-smi CSV subprocess (the fallback, always built). Selection is
// automatic — attempt the NVML load once at startup, fall back silently — and
// the live source is recorded on prickle_gpu_nvidia_source_info.
//
// The interface exists so the two must agree: a fake source drives the metric
// emission in tests, and the same emission code renders whichever real source
// loaded. NVML itself is a C call rather than a file read, so SPEC.md §Testing
// rules verifies it only on hardware; its captured nvidia-smi fixtures remain
// the reference for what it must report.
//
// **PID handling.** nvidia-smi's --query-compute-apps returns a pid column and
// this package parses it, because the captured fixture is that exact query and
// SPEC.md §Testing rules forbids inventing a different one. The value is
// discarded at the parse boundary and never reaches a snapshot field: there is
// nowhere in this package's data model for a PID to live, which is what makes
// "PID never appears" (SPEC.md §Metrics contract) structural rather than a
// promise. Per-process attribution is keyed on `command`, the basename of the
// process path, and is opt-in.
//
// AMD and Intel are specified in SPEC.md §Collectors but not implemented: the
// captured host has neither, and testdata/README.md records that gap rather
// than this package guessing at a sysfs layout it has never seen.
package gpu

import (
	"context"
	"errors"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// Metric name prefix for every family in this package.
const prefix = "prickle_gpu_"

// mebibyte converts nvidia-smi's `nounits` memory columns, which are MiB.
const mebibyte = 1 << 20

// snapshot is one pass over the NVIDIA devices, as any source reports it.
//
// There is deliberately no PID field anywhere below.
type snapshot struct {
	devices   []device
	processes []process
}

// device is one physical GPU.
type device struct {
	Index int
	UUID  string
	Name  string

	// Utilization is the SM busy fraction, 0 to 1. Absent rather than zero
	// when the driver reports [N/A], which it does for the whole card whenever
	// MIG is enabled (SPEC.md §Collectors, verified on H200 / driver 580).
	Utilization      float64
	HasUtilization   bool
	MemoryUsedBytes  uint64
	MemoryTotalBytes uint64
	TemperatureC     float64
	HasTemperature   bool
	PowerWatts       float64
	HasPower         bool

	// MIGEnabled is true when the card is partitioned. MIG holds one entry per
	// instance; a card in Default mode has none.
	MIGEnabled bool
	MIG        []migDevice
}

// migDevice is one MIG instance of a partitioned GPU.
//
// Memory and utilization are absent from the nvidia-smi source — it publishes
// them only in the human-readable table, not in any CSV query — so they carry
// a Has* flag and the NVML source is what fills them in. SPEC.md §Collectors
// calls NVML "the only reliable source of MIG topology" for exactly this
// reason.
// The GPU-instance and compute-instance IDs are deliberately not carried. The
// nvidia-smi source can list them (`mig -lgi`) and can list MIG UUIDs (`-L`)
// but publishes no output joining the two, so pairing them would mean assuming
// the two listings are in the same order — a guess, and SPEC.md §Testing rules
// forbids guessing. A label NVML could fill and nvidia-smi could not would also
// break the requirement there that both sources emit identical output.
type migDevice struct {
	UUID        string
	Profile     string // "1g.18gb"
	DeviceIndex int    // the driver's MIG device number, from `nvidia-smi -L`

	MemoryUsedBytes  uint64
	MemoryTotalBytes uint64
	HasMemory        bool
	Utilization      float64
	HasUtilization   bool
}

// process is one compute process holding GPU memory.
type process struct {
	// GPUUUID is the device the process was attributed to. The nvidia-smi
	// source reports the *parent* GPU UUID for a MIG-resident process, a
	// verified limitation recorded in SPEC.md §Collectors; MIGUUID is empty
	// there and only NVML can fill it.
	GPUUUID string
	MIGUUID string

	// Command is the basename of the process's executable path — never comm,
	// which is truncated to 15 characters and forgeable (SPEC.md §Metrics
	// contract).
	Command string

	MemoryBytes uint64
}

// nvidiaSource is one way of reading the NVIDIA devices.
type nvidiaSource interface {
	// Name is the value reported on prickle_gpu_nvidia_source_info.
	Name() string

	// Read takes one snapshot.
	Read(ctx context.Context) (snapshot, error)

	// Close releases whatever the source holds open. Safe on a nil-ish source
	// and safe to call more than once.
	Close() error
}

// Options configures the GPU collector. The zero value is usable: it selects a
// source automatically and leaves per-process attribution off.
type Options struct {
	// Roots is carried for the AMD and Intel collectors, which read sysfs and
	// DRM fdinfo. Unused until those exist (testdata/README.md §Coverage gaps).
	Roots fsroot.Roots

	// NVIDIASource forces one implementation: "auto" (default), "nvml" or
	// "smi". SPEC.md §Collectors: a debugging flag, not a tuning knob.
	NVIDIASource string

	// PerProcess adds prickle_gpu_process_memory_bytes, keyed on `command`.
	// SPEC.md §Metrics contract makes per-process attribution opt-in: it is one
	// series per distinct command per GPU, and on a shared node that is
	// unbounded in a way the device series are not.
	PerProcess bool

	// SMICommand overrides the nvidia-smi binary name. For tests and for hosts
	// that keep it outside PATH.
	SMICommand string

	// runner executes nvidia-smi. Nil means the real subprocess; tests inject
	// a fake that replays captured output, because a subprocess is not a file
	// and cannot be pointed at a fixture tree (the same reason Statfser exists
	// in the host collector).
	runner commandRunner

	// newSources overrides source construction in tests.
	newSources func(Options) []nvidiaSource
}

// Collector reads the GPUs in the host.
type Collector struct {
	opts   Options
	source nvidiaSource

	// selectErr is why no source loaded, kept for `prickle diagnose` to
	// explain rather than leaving an empty GPU section.
	selectErr error
}

// New returns a GPU collector with a source already selected.
//
// Selection happens once, here, rather than per scrape: SPEC.md §Collectors
// says to attempt the NVML load once at startup and fall back silently, and a
// per-scrape retry would spawn a process or a dlopen on a host that has neither.
func New(opts Options) *Collector {
	if opts.NVIDIASource == "" {
		opts.NVIDIASource = SourceAuto
	}
	c := &Collector{opts: opts}
	c.source, c.selectErr = selectSource(opts)
	return c
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "gpu" }

// Close releases the selected source.
func (c *Collector) Close() error {
	if c.source == nil {
		return nil
	}
	return c.source.Close()
}

// Collect implements collector.Collector.
//
// A host with no NVIDIA GPU produces no samples and no error. That is the
// common case on the nodes this exporter also watches for host and container
// metrics, and it is not a failure — `prickle diagnose` is where the absence is
// explained.
func (c *Collector) Collect(ctx context.Context, out *exposition.Set) error {
	if c.source == nil {
		return nil
	}

	out.Gauge(prefix+"nvidia_source_info",
		"Which NVIDIA implementation is live: constant 1, labelled with the source that loaded.").
		Add(1, exposition.L("source", c.source.Name()))

	snap, err := c.source.Read(ctx)
	// A partial read still renders: SPEC.md's partial-collection contract in
	// internal/collector applies here as much as to a missing /proc file.
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}

	c.emitDevices(out, snap.devices)
	if c.opts.PerProcess {
		c.emitProcesses(out, snap.processes)
	}
	return errors.Join(errs...)
}

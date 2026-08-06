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
// AMD is specified in SPEC.md §Collectors but not implemented: no captured
// host has one, and testdata/README.md records that gap rather than this
// package guessing at a sysfs layout it has never seen. Intel is out of scope
// there, for the same want of a capture.
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

	// Vendor is the driver stack the card was read through: "nvidia" or "amd".
	// It rides prickle_gpu_info as a descriptive attribute, which is where
	// SPEC.md §Metrics contract puts anything that is not identity.
	//
	// A mixed host is why it is there at all. `name` would otherwise be the
	// only thing distinguishing the vendors, and AMD sysfs publishes no
	// marketing name for a card — see amdMarketNames.
	Vendor string

	// ComputePartition and MemoryPartition are AMD's partitioning mode, "SPX"
	// and "NPS1" on an unpartitioned MI300X. They are the nearest thing AMD has
	// to MIG topology, and they are emitted as their own _info gauge rather
	// than folded into the MIG families: MIG is an NVIDIA feature with an
	// NVIDIA-specific data model, and a prickle_gpu_mig_enabled on an AMD card
	// would be a false statement in either direction. Empty on NVIDIA.
	ComputePartition string
	MemoryPartition  string

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

	// Container is the ID of the container the process runs in, or "" for one
	// running directly on the host. It comes from the process's own
	// /proc/<pid>/cgroup, so the PID dies here exactly as it does for Command —
	// SPEC.md §Metrics contract forbids a PID as a label or a value, not as a
	// transient lookup key, which is the same allowance Command already relies
	// on.
	//
	// `container` is already in the closed hot-series identity set, so this
	// costs no new label key: it makes a GPU process joinable to
	// prickle_container_info, and through it to a pod name and namespace, which
	// is the question "who is using this card" actually reduces to on a
	// Kubernetes node.
	Container string

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
	// Roots is carried for the AMD collector, which reads sysfs and DRM fdinfo.
	// Unused until it exists (testdata/README.md §Coverage gaps).
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

	// nvidiaCandidates overrides which NVIDIA implementations selectSource
	// tries. Nil means the real order in candidates().
	//
	// The AMD tests need it to return nothing: their fixture host has no NVIDIA
	// card, and without this a developer machine with nvidia-smi installed would
	// have the real binary spawned underneath a sysfs fixture test and produce a
	// different result from CI.
	nvidiaCandidates func(Options) []sourceCandidate
}

// Collector reads the GPUs in the host.
type Collector struct {
	opts   Options
	source nvidiaSource

	// amd reads AMD cards from sysfs. It is always present and always consulted:
	// unlike the NVIDIA sources there is nothing to select between and nothing
	// to load, so "does this host have an AMD GPU" is answered by the same
	// readdir that reads them. A host with none costs one failed directory
	// listing per scrape.
	//
	// The two vendors are read independently and both are rendered, because a
	// host may have both and because neither can stand in for the other.
	amd *amdSource

	// selectErr is why no source loaded, kept for `prickle diagnose` to
	// explain rather than leaving an empty GPU section.
	selectErr error

	// declined is why the sources ahead of the live one refused, kept for the
	// same reason and for the more common case: something did load, and the
	// operator wants to know why it was not the preferred one.
	declined []error
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
	c := &Collector{opts: opts, amd: &amdSource{roots: opts.Roots}}
	c.source, c.declined, c.selectErr = selectSource(opts)
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
	// A partial read still renders: SPEC.md's partial-collection contract in
	// internal/collector applies here as much as to a missing /proc file. It
	// also applies across vendors — an NVIDIA driver that has stopped
	// answering must not cost an AMD card in the same box its metrics.
	var errs []error
	var devices []device
	var processes []process

	if c.source != nil {
		out.Gauge(prefix+"nvidia_source_info",
			"Which NVIDIA implementation is live: constant 1, labelled with the source that loaded.").
			Add(1, exposition.L("source", c.source.Name()))

		snap, err := c.source.Read(ctx)
		if err != nil {
			errs = append(errs, err)
		}
		// The NVIDIA sources predate the vendor label and do not set it, so it
		// is stamped here rather than in each of them: there is one place that
		// knows a snapshot came from the NVIDIA path, and this is it.
		for i := range snap.devices {
			snap.devices[i].Vendor = vendorNVIDIA
		}
		devices = append(devices, snap.devices...)
		processes = append(processes, snap.processes...)
	}

	if c.amd != nil {
		snap, err := c.amd.read(c.opts.PerProcess)
		if err != nil {
			errs = append(errs, err)
		}
		devices = append(devices, snap.devices...)
		processes = append(processes, snap.processes...)
	}

	c.emitDevices(out, devices)
	if c.opts.PerProcess {
		c.emitProcesses(out, processes)
	}
	return errors.Join(errs...)
}

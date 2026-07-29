// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"strconv"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// emitDevices renders one pass of physical GPUs and their MIG instances.
//
// MIG instances get their own families rather than an extra label on the
// device families. A MIG instance's memory is a partition of its parent's, so
// one family holding both would make sum(prickle_gpu_memory_used_bytes) count
// the same bytes twice — the same double-counting trap the container collector
// avoids by sampling only leaf cgroups. A card's total is the device series; a
// partition's is the mig series.
func (c *Collector) emitDevices(out *exposition.Set, devices []device) {
	for _, d := range devices {
		gpu := exposition.L("gpu_uuid", d.UUID)

		out.Gauge(prefix+"info",
			"GPU identity: constant 1, carrying the model name and the driver's device index.").
			Add(1, gpu,
				exposition.L("name", d.Name),
				exposition.L("index", strconv.Itoa(d.Index)))

		// Absent, not zero: the driver reports [N/A] for the whole card
		// whenever MIG is enabled, and a zero would read as an idle GPU.
		if d.HasUtilization {
			out.Gauge(prefix+"utilization_ratio",
				"Fraction of time the GPU's SMs were busy, 0 to 1. Absent when the driver reports it as unavailable, which it does for the whole card while MIG is enabled.").
				Add(d.Utilization, gpu)
		}

		out.Gauge(prefix+"memory_used_bytes", "GPU memory in use.").
			Add(float64(d.MemoryUsedBytes), gpu)
		out.Gauge(prefix+"memory_total_bytes", "GPU memory installed.").
			Add(float64(d.MemoryTotalBytes), gpu)

		if d.HasTemperature {
			out.Gauge(prefix+"temperature_celsius", "GPU core temperature.").
				Add(d.TemperatureC, gpu)
		}
		if d.HasPower {
			out.Gauge(prefix+"power_watts", "Power the GPU is drawing.").
				Add(d.PowerWatts, gpu)
		}

		migEnabled := 0.0
		if d.MIGEnabled {
			migEnabled = 1
		}
		out.Gauge(prefix+"mig_enabled",
			"1 when the GPU is partitioned into MIG instances, 0 when it is in Default mode.").
			Add(migEnabled, gpu)

		c.emitMIG(out, gpu, d.MIG)
	}
}

// emitMIG renders one GPU's MIG instances.
//
// The topology gauge is always emitted when an instance exists; memory and
// utilization only when the source could supply them. The nvidia-smi source
// cannot — it publishes per-instance figures solely in the human-readable
// table, and SPEC.md §Testing rules is not satisfied by parsing box-drawing
// characters that no captured CSV pins.
func (c *Collector) emitMIG(out *exposition.Set, gpu exposition.Label, instances []migDevice) {
	for _, m := range instances {
		labels := []exposition.Label{gpu, exposition.L("mig_uuid", m.UUID)}

		out.Gauge(prefix+"mig_info",
			"MIG instance identity: constant 1, carrying the profile and the driver's MIG device index.").
			Add(1, gpu,
				exposition.L("mig_uuid", m.UUID),
				exposition.L("profile", m.Profile),
				exposition.L("device_index", strconv.Itoa(m.DeviceIndex)))

		if m.HasMemory {
			out.Gauge(prefix+"mig_memory_used_bytes",
				"Memory in use on a MIG instance. Absent from the nvidia-smi source, which does not publish it in any CSV query.").
				Add(float64(m.MemoryUsedBytes), labels...)
			out.Gauge(prefix+"mig_memory_total_bytes",
				"Memory allocated to a MIG instance. Absent from the nvidia-smi source, which does not publish it in any CSV query.").
				Add(float64(m.MemoryTotalBytes), labels...)
		}
		if m.HasUtilization {
			out.Gauge(prefix+"mig_utilization_ratio",
				"Fraction of time a MIG instance's SMs were busy, 0 to 1. Absent from the nvidia-smi source.").
				Add(m.Utilization, labels...)
		}
	}
}

// emitProcesses renders per-process GPU memory, keyed on `command`.
//
// SPEC.md §Metrics contract: the label is the basename of the executable path
// and never comm, and no PID appears. Two processes running the same binary on
// the same GPU are one series with their memory summed — which is what an
// operator asking "how much is PyTorch holding" wants, and is also the only
// aggregation available once the PID is gone.
func (c *Collector) emitProcesses(out *exposition.Set, processes []process) {
	type key struct{ gpu, mig, command string }
	totals := make(map[key]uint64, len(processes))
	order := make([]key, 0, len(processes))

	for _, p := range processes {
		k := key{p.GPUUUID, p.MIGUUID, p.Command}
		if _, seen := totals[k]; !seen {
			order = append(order, k)
		}
		totals[k] += p.MemoryBytes
	}

	for _, k := range order {
		labels := []exposition.Label{exposition.L("gpu_uuid", k.gpu)}
		if k.mig != "" {
			labels = append(labels, exposition.L("mig_uuid", k.mig))
		}
		labels = append(labels, exposition.L("command", k.command))

		out.Gauge(prefix+"process_memory_bytes",
			"GPU memory held by processes running one command, summed. Opt-in behind -collector.gpu.per-process.").
			Add(float64(totals[k]), labels...)
	}
}

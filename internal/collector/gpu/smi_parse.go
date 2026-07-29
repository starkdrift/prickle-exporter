// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// absent reports whether a CSV token is one of the driver's bracketed
// placeholders rather than a value.
//
// SPEC.md §Collectors, verified on H200 / driver 580: utilization.gpu returns
// [N/A] for the whole card whenever MIG is enabled, and the CSV parser must
// treat bracketed tokens as absent values, never as errors. The check is on the
// bracket shape rather than a list of known strings, because the driver also
// emits [Not Supported] and [Unknown Error] in the same position and a parser
// that knows only [N/A] fails on a card it has never seen.
func absent(token string) bool {
	return strings.HasPrefix(token, "[") && strings.HasSuffix(token, "]")
}

// splitCSV splits one `--format=csv,noheader` row.
//
// nvidia-smi writes ", " between fields. Splitting on the comma and trimming
// tolerates both that and a bare comma, which some driver versions emit.
func splitCSV(line string) []string {
	fields := strings.Split(line, ",")
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
	}
	return fields
}

// parseQueryGPU parses --query-gpu=queryGPUFields --format=csv,noheader,nounits.
//
//	0, GPU-2af1c335-11fb-c9d3-4de3-27a25697fc35, NVIDIA H200, [N/A], 639, 143771, 38, 123.31
//
// Eight columns: index, uuid, name, utilization %, memory used MiB, memory
// total MiB, temperature C, power W. `nounits` strips the suffixes, so the
// memory columns are MiB and are scaled to bytes here — a metric named _bytes
// must hold bytes (SPEC.md §Metrics contract).
//
// A row whose optional columns are absent still produces a device: a card that
// cannot report power is still a card, and its memory is still worth having.
func parseQueryGPU(out string) ([]device, error) {
	var devices []device
	var errs []error

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := splitCSV(line)
		if len(f) < 8 {
			errs = append(errs, fmt.Errorf("--query-gpu: expected 8 fields, got %d in %q", len(f), line))
			continue
		}

		d := device{UUID: f[1], Name: f[2]}
		if d.UUID == "" {
			errs = append(errs, fmt.Errorf("--query-gpu: row with no UUID: %q", line))
			continue
		}
		if index, err := strconv.Atoi(f[0]); err == nil {
			d.Index = index
		} else if !absent(f[0]) {
			errs = append(errs, fmt.Errorf("--query-gpu: index %q: %w", f[0], err))
		}

		// Utilization is a percentage in the CSV and a ratio in the metric.
		if v, ok, err := optionalFloat("utilization.gpu", f[3]); err != nil {
			errs = append(errs, err)
		} else if ok {
			d.Utilization, d.HasUtilization = v/100, true
		}

		for _, m := range []struct {
			name  string
			token string
			dst   *uint64
		}{
			{"memory.used", f[4], &d.MemoryUsedBytes},
			{"memory.total", f[5], &d.MemoryTotalBytes},
		} {
			if v, ok, err := optionalFloat(m.name, m.token); err != nil {
				errs = append(errs, err)
			} else if ok {
				*m.dst = uint64(v) * mebibyte
			}
		}

		if v, ok, err := optionalFloat("temperature.gpu", f[6]); err != nil {
			errs = append(errs, err)
		} else if ok {
			d.TemperatureC, d.HasTemperature = v, true
		}
		if v, ok, err := optionalFloat("power.draw", f[7]); err != nil {
			errs = append(errs, err)
		} else if ok {
			d.PowerWatts, d.HasPower = v, true
		}

		devices = append(devices, d)
	}
	return devices, errors.Join(errs...)
}

// optionalFloat parses a CSV numeric token, reporting a bracketed placeholder
// as absent rather than as a failure.
func optionalFloat(field, token string) (float64, bool, error) {
	if token == "" || absent(token) {
		return 0, false, nil
	}
	v, err := strconv.ParseFloat(token, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%s: %q is not a number", field, token)
	}
	return v, true, nil
}

// attachMIG parses `nvidia-smi -L` and fills in each device's MIG instances.
//
//	GPU 0: NVIDIA H200 (UUID: GPU-2af1c335-11fb-c9d3-4de3-27a25697fc35)
//	  MIG 1g.18gb     Device  0: (UUID: MIG-30366fdf-6105-5648-968d-679e250aa830)
//	  MIG 1g.18gb     Device  1: (UUID: MIG-7138b2c5-cb05-5700-b65a-5fdec910f0f4)
//
// Indented MIG lines belong to the GPU line above them. A card in Default mode
// has no MIG lines at all, which is how MIGEnabled is decided — the captured
// --query-gpu field set carries no mig.mode column, and this is the output that
// answers the question without inventing a query.
//
// Devices are matched by UUID rather than by position, so a -L listing in a
// different order than --query-gpu cannot silently attach one card's partitions
// to another.
func attachMIG(devices []device, out string) error {
	byUUID := make(map[string]*device, len(devices))
	for i := range devices {
		byUUID[devices[i].UUID] = &devices[i]
	}

	var current *device
	var errs []error

	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indented := line != strings.TrimLeft(line, " \t")
		trimmed := strings.TrimSpace(line)

		if !indented && strings.HasPrefix(trimmed, "GPU ") {
			uuid, ok := uuidFromListing(trimmed)
			if !ok {
				errs = append(errs, fmt.Errorf("-L: no UUID in %q", trimmed))
				current = nil
				continue
			}
			// A GPU in -L that --query-gpu did not report is not an error
			// worth failing over; there is simply nothing to attach to.
			current = byUUID[uuid]
			continue
		}

		if !strings.HasPrefix(trimmed, "MIG ") {
			continue
		}
		if current == nil {
			errs = append(errs, fmt.Errorf("-L: MIG line with no preceding GPU: %q", trimmed))
			continue
		}
		m, err := parseMIGListing(trimmed)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		current.MIGEnabled = true
		current.MIG = append(current.MIG, m)
	}
	return errors.Join(errs...)
}

// uuidFromListing pulls the identifier out of a `(UUID: ...)` suffix.
func uuidFromListing(line string) (string, bool) {
	_, rest, ok := strings.Cut(line, "(UUID: ")
	if !ok {
		return "", false
	}
	uuid, _, ok := strings.Cut(rest, ")")
	if !ok || uuid == "" {
		return "", false
	}
	return uuid, true
}

// parseMIGListing parses one indented MIG line of `nvidia-smi -L`.
//
//	MIG 1g.18gb     Device  0: (UUID: MIG-30366fdf-6105-5648-968d-679e250aa830)
//
// The profile is the token after "MIG"; the device index is the token after
// "Device", with its trailing colon removed.
func parseMIGListing(line string) (migDevice, error) {
	uuid, ok := uuidFromListing(line)
	if !ok {
		return migDevice{}, fmt.Errorf("-L: no UUID in MIG line %q", line)
	}

	fields := strings.Fields(line)
	if len(fields) < 4 || fields[0] != "MIG" {
		return migDevice{}, fmt.Errorf("-L: unrecognised MIG line %q", line)
	}
	m := migDevice{UUID: uuid, Profile: fields[1], DeviceIndex: -1}

	for i, f := range fields {
		if f != "Device" || i+1 >= len(fields) {
			continue
		}
		if index, err := strconv.Atoi(strings.TrimSuffix(fields[i+1], ":")); err == nil {
			m.DeviceIndex = index
		}
		break
	}
	if m.DeviceIndex < 0 {
		return migDevice{}, fmt.Errorf("-L: no device index in MIG line %q", line)
	}
	return m, nil
}

// parseComputeApps parses --query-compute-apps=queryProcessFields.
//
//	GPU-2af1c335-11fb-c9d3-4de3-27a25697fc35, 12648, /tmp/prickle-gpu-spin, 602
//
// Four columns: gpu_uuid, pid, process_name, used_gpu_memory MiB.
//
// **The pid column is read and thrown away.** It is requested because the
// captured fixture requested it and SPEC.md §Testing rules makes the capture
// the record of the format; it is dropped here, at the boundary, so that no
// PID exists anywhere downstream to leak into a label or a value (SPEC.md
// §Metrics contract). `command` is the basename of process_name — the path the
// driver reports, which is the exe, not the truncated and forgeable comm.
//
// The gpu_uuid column holds the *parent* GPU's UUID for a MIG-resident
// process: a verified limitation of this source recorded in SPEC.md
// §Collectors, so MIGUUID is left empty and only NVML can fill it. Attributing
// such a process to the physical card is correct-but-coarse, not wrong.
func parseComputeApps(out string) ([]process, error) {
	var processes []process
	var errs []error

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// No compute processes at all is the common case, and nvidia-smi says
		// so in prose rather than with an empty document.
		if strings.Contains(line, "No running processes found") {
			continue
		}
		f := splitCSV(line)
		if len(f) < 4 {
			errs = append(errs, fmt.Errorf("--query-compute-apps: expected 4 fields, got %d in %q", len(f), line))
			continue
		}

		p := process{GPUUUID: f[0], Command: path.Base(f[2])}
		if p.GPUUUID == "" {
			errs = append(errs, fmt.Errorf("--query-compute-apps: row with no GPU UUID: %q", line))
			continue
		}
		if f[2] == "" || absent(f[2]) {
			// Without a command there is no label to key the series on, and
			// the PID that would identify it is exactly what must not be used.
			continue
		}
		if v, ok, err := optionalFloat("used_gpu_memory", f[3]); err != nil {
			errs = append(errs, err)
			continue
		} else if ok {
			p.MemoryBytes = uint64(v) * mebibyte
		}

		processes = append(processes, p)
	}
	return processes, errors.Join(errs...)
}

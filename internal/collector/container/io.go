// SPDX-License-Identifier: Apache-2.0

package container

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// ioFields are the key=value pairs of an io.stat line.
//
// The names mirror the host collector's diskstats families, so a container's
// read bytes and its node's read bytes carry the same word for the same thing.
var ioFields = []struct{ key, name, help string }{
	{"rbytes", "io_read_bytes_total", "Bytes read from block devices."},
	{"wbytes", "io_written_bytes_total", "Bytes written to block devices."},
	{"rios", "io_reads_completed_total", "Read operations completed."},
	{"wios", "io_writes_completed_total", "Write operations completed."},
	{"dbytes", "io_discarded_bytes_total", "Bytes discarded."},
	{"dios", "io_discards_completed_total", "Discard operations completed."},
}

// collectIO returns the io.stat collector, closing over the major:minor to
// device-name map so it is resolved once per pass rather than once per
// container.
//
//	252:0 rbytes=0 wbytes=12288 rios=0 wios=3 dbytes=0 dios=0
//
// One line per block device the container has touched; a container that has
// done no block I/O has an empty file, and gets no samples. The `device` label
// is dimensional in the sense SPEC.md §Metrics contract allows: its values are
// enumerable from the file itself and name a part of this container's I/O, not
// an entity of their own.
func (c *Collector) collectIO(devices map[string]string) func(*exposition.Set, cgroup, []exposition.Label) error {
	return func(out *exposition.Set, cg cgroup, labels []exposition.Label) error {
		path := filepath.Join(cg.dir, "io.stat")
		lines, err := readLines(path)
		if err != nil {
			return skipMissing(err)
		}

		var errs []error
		for _, line := range lines {
			devnum, rest, ok := strings.Cut(line, " ")
			if !ok {
				continue
			}
			name, known := devices[devnum]
			if !known {
				// No /proc/diskstats row for this major:minor. The number is
				// still the device's identity, so the series is emitted under
				// it rather than dropped.
				name = devnum
			}
			device := with(labels, exposition.L("device", name))

			values := make(map[string]uint64, len(ioFields))
			for _, pair := range strings.Fields(rest) {
				key, value, ok := strings.Cut(pair, "=")
				if !ok {
					continue
				}
				v, err := parseUint(path, value)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				values[key] = v
			}
			for _, f := range ioFields {
				if v, ok := values[f.key]; ok {
					out.Counter(prefix+f.name, f.help).Add(float64(v), device...)
				}
			}
		}
		return errors.Join(errs...)
	}
}

// blockDevices maps a major:minor pair to the device name.
//
// io.stat identifies devices by number; the host collector's disk series are
// keyed by name. Resolving here is what lets a container's I/O join to its
// node's without a translation table in every query. /proc/diskstats is the
// source because it is one file with both, is already part of the captured
// fixture tree, and is read through fsroot like everything else.
//
// A host without a readable /proc/diskstats is not an error worth failing the
// collector over: the label falls back to the major:minor pair.
func (c *Collector) blockDevices() (map[string]string, error) {
	path := c.opts.Roots.ProcPath("diskstats")
	lines, err := readLines(path)
	if err != nil {
		return nil, skipMissing(err)
	}
	devices := make(map[string]string, len(lines))
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		devices[f[0]+":"+f[1]] = f[2]
	}
	return devices, nil
}

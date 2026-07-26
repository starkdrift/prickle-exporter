// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"fmt"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// diskField describes one column of /proc/diskstats after the device name.
type diskField struct {
	name  string
	help  string
	kind  exposition.Kind
	scale float64
}

// diskFields is the column layout of /proc/diskstats, in file order.
//
// Columns 1-11 have been stable since 2.6. Columns 12-15 (discards) arrived in
// 4.18 and 16-17 (flush) in 5.5, so a line is parsed for as many columns as it
// actually has — the captured host (5.15) has all 17.
//
// Sector counts are always in 512-byte units here, whatever the device's real
// block size, so they are scaled to bytes at parse time and named accordingly.
var diskFields = []diskField{
	{"disk_reads_completed_total", "Reads completed successfully.", exposition.Counter, 1},
	{"disk_reads_merged_total", "Reads merged with an adjacent request before being issued.", exposition.Counter, 1},
	{"disk_read_bytes_total", "Bytes read.", exposition.Counter, sectorSize},
	{"disk_read_seconds_total", "Seconds spent reading.", exposition.Counter, 1e-3},
	{"disk_writes_completed_total", "Writes completed successfully.", exposition.Counter, 1},
	{"disk_writes_merged_total", "Writes merged with an adjacent request before being issued.", exposition.Counter, 1},
	{"disk_written_bytes_total", "Bytes written.", exposition.Counter, sectorSize},
	{"disk_write_seconds_total", "Seconds spent writing.", exposition.Counter, 1e-3},
	{"disk_io_now", "Requests currently in flight.", exposition.Gauge, 1},
	{"disk_io_time_seconds_total", "Seconds during which the device had I/O in flight.", exposition.Counter, 1e-3},
	{"disk_io_time_weighted_seconds_total", "Seconds spent on I/O, weighted by the queue depth at the time.", exposition.Counter, 1e-3},
	{"disk_discards_completed_total", "Discards completed successfully.", exposition.Counter, 1},
	{"disk_discards_merged_total", "Discards merged with an adjacent request before being issued.", exposition.Counter, 1},
	{"disk_discarded_bytes_total", "Bytes discarded.", exposition.Counter, sectorSize},
	{"disk_discard_seconds_total", "Seconds spent discarding.", exposition.Counter, 1e-3},
	{"disk_flush_requests_total", "Flush requests completed successfully.", exposition.Counter, 1},
	{"disk_flush_seconds_total", "Seconds spent flushing.", exposition.Counter, 1e-3},
}

// collectDiskstats parses /proc/diskstats.
//
//	252       0 vda 25188 3354 1884114 4515 29661 56042 50588588 37904 0 32716 42915 732 0 7262488 63 5216 431
//
// The device name keys the hot series — it is the disk's identity and nothing
// shorter exists. Major and minor numbers are descriptive, so they ride on the
// _info gauge (SPEC.md §Metrics contract).
func (c *Collector) collectDiskstats(out *exposition.Set) error {
	path := c.opts.Roots.ProcPath("diskstats")
	lines, err := readLines(path)
	if err != nil {
		return err
	}

	info := out.Gauge(prefix+"disk_info",
		"Block device identity: constant 1, carrying the device's major and minor numbers.")

	var errs []error
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		major, minor, device := f[0], f[1], f[2]
		if c.opts.IgnoredDisks != nil && c.opts.IgnoredDisks.MatchString(device) {
			continue
		}
		dev := exposition.L("device", device)
		info.Add(1, dev, exposition.L("major", major), exposition.L("minor", minor))

		for i, col := range f[3:] {
			if i >= len(diskFields) {
				// Columns beyond the documented layout. No fixture, no format.
				break
			}
			df := diskFields[i]
			v, err := parseUint(path, df.name, col)
			if err != nil {
				errs = append(errs, fmt.Errorf("device %s: %w", device, err))
				continue
			}
			df.family(out).Add(float64(v)*df.scale, dev)
		}
	}
	return errors.Join(errs...)
}

// family resolves a diskField to its family in out, creating it on first use.
func (d diskField) family(out *exposition.Set) *exposition.Family {
	if d.kind == exposition.Counter {
		return out.Counter(prefix+d.name, d.help)
	}
	return out.Gauge(prefix+d.name, d.help)
}

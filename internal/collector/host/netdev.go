// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"fmt"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// netDevFields is the column layout of /proc/net/dev after the interface name:
// eight receive columns then eight transmit columns, in file order.
//
// The `errs`, `colls` and `fifo` spellings are the kernel's own, kept rather
// than expanded, so the metric name and the file agree.
var netDevFields = []struct{ name, help string }{
	{"network_receive_bytes_total", "Bytes received."},
	{"network_receive_packets_total", "Packets received."},
	{"network_receive_errs_total", "Receive errors."},
	{"network_receive_drop_total", "Packets dropped on receive."},
	{"network_receive_fifo_total", "Receive FIFO overruns."},
	{"network_receive_frame_total", "Receive framing errors."},
	{"network_receive_compressed_total", "Compressed packets received."},
	{"network_receive_multicast_total", "Multicast packets received."},
	{"network_transmit_bytes_total", "Bytes transmitted."},
	{"network_transmit_packets_total", "Packets transmitted."},
	{"network_transmit_errs_total", "Transmit errors."},
	{"network_transmit_drop_total", "Packets dropped on transmit."},
	{"network_transmit_fifo_total", "Transmit FIFO overruns."},
	{"network_transmit_colls_total", "Transmit collisions."},
	{"network_transmit_carrier_total", "Transmit carrier losses."},
	{"network_transmit_compressed_total", "Compressed packets transmitted."},
}

// collectNetDev parses /proc/net/dev.
//
//	Inter-|   Receive                    |  Transmit
//	 face |bytes    packets errs drop ...|bytes    packets errs drop ...
//	    lo: 33503626   40008    0    0 ...
//	vethec7b279:     126       3    0    0 ...
//
// The two header lines are the ones without a colon. The colon is the only
// reliable separator: an interface name long enough to touch it leaves no space
// before the first count, so splitting on whitespace loses the name.
func (c *Collector) collectNetDev(out *exposition.Set) error {
	path := c.opts.Roots.ProcPath("net", "dev")
	lines, err := readLines(path)
	if err != nil {
		return err
	}

	var errs []error
	for _, line := range lines {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue // header
		}
		iface := strings.TrimSpace(name)
		if iface == "" {
			continue
		}
		if c.opts.IgnoredNetDevices != nil && c.opts.IgnoredNetDevices.MatchString(iface) {
			continue
		}

		label := exposition.L("interface", iface)
		for i, col := range strings.Fields(rest) {
			if i >= len(netDevFields) {
				break
			}
			v, err := parseUint(path, netDevFields[i].name, col)
			if err != nil {
				errs = append(errs, fmt.Errorf("interface %s: %w", iface, err))
				continue
			}
			out.Counter(prefix+netDevFields[i].name, netDevFields[i].help).Add(float64(v), label)
		}
	}
	return errors.Join(errs...)
}

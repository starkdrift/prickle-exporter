// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"fmt"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// meminfoAliases spell out kernel field names that the generic converter turns
// into something promlint rejects as an abbreviated unit.
//
// `SReclaimable` becomes `s_reclaimable` and `SecPageTables` becomes
// `sec_page_tables`; promlint reads a bare `s` and `sec` as abbreviations for
// seconds, failing `promtool check metrics` — which SPEC.md §Metrics contract
// requires to pass at all times. `KReclaimable` is expanded alongside its
// siblings for consistency.
//
// The expansions are the kernel's own descriptions of the fields
// (Documentation/filesystems/proc.rst): S is slab, K is kernel, and
// SecPageTables is memory consumed by secondary page tables.
//
// This table is not the safety net — exposition.validMetricName rejects an
// abbreviated unit outright, so an unaliased field on a future kernel is
// dropped with a logged error rather than breaking lint for the whole endpoint.
// The table is what turns such a field back into a metric, and it is the right
// fix each time one appears.
var meminfoAliases = map[string]string{
	"SReclaimable":  "slab_reclaimable",
	"SUnreclaim":    "slab_unreclaim",
	"KReclaimable":  "kernel_reclaimable",
	"SecPageTables": "secondary_page_tables",
}

// metricSegment converts a kernel field name to its metric name segment,
// applying an alias if one exists.
func metricSegment(name string) string {
	if alias, ok := meminfoAliases[name]; ok {
		return alias
	}
	return snakeCase(name)
}

// collectMeminfo parses /proc/meminfo.
//
//	MemTotal:       247405756 kB
//	Active(anon):        2164 kB
//	HugePages_Total:        0
//
// Every field is exposed, not a curated subset: the file is ~50 lines, the
// names are a stable kernel ABI, and which of Committed_AS or KReclaimable
// matters is the operator's call, not this collector's.
//
// Values carrying the kB unit become a _bytes gauge (the kernel's "kB" is
// really KiB — 1024, not 1000). The unitless HugePages_* fields are counts, so
// they share one family with a state label; naming them individually would
// produce prickle_host_memory_huge_pages_total, a gauge with a counter's
// suffix.
func (c *Collector) collectMeminfo(out *exposition.Set) error {
	path := c.opts.Roots.ProcPath("meminfo")
	lines, err := readLines(path)
	if err != nil {
		return err
	}

	var errs []error
	for _, line := range lines {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := parseUint(path, name, fields[0])
		if err != nil {
			errs = append(errs, err)
			continue
		}

		switch {
		case len(fields) > 1 && fields[1] == "kB":
			out.Gauge(prefix+"memory_"+metricSegment(name)+"_bytes",
				"Kernel meminfo field "+name+", in bytes.").Add(float64(v) * 1024)

		case strings.HasPrefix(name, "HugePages_"):
			state := snakeCase(strings.TrimPrefix(name, "HugePages_"))
			out.Gauge(prefix+"memory_huge_pages",
				"Huge pages by state, as counts of pages (see prickle_host_memory_hugepagesize_bytes).").
				Add(float64(v), exposition.L("state", state))

		case len(fields) == 1:
			out.Gauge(prefix+"memory_"+metricSegment(name),
				"Kernel meminfo field "+name+".").Add(float64(v))

		default:
			// A unit other than kB. No fixture shows one; refuse to guess at
			// its scale rather than publish a wrong number.
			errs = append(errs, fmt.Errorf("%s: %s: unknown unit %q", path, name, fields[1]))
		}
	}
	return errors.Join(errs...)
}

// snakeCase converts a kernel field name to a Prometheus metric name segment.
//
//	MemTotal      -> mem_total
//	SReclaimable  -> s_reclaimable
//	Active(anon)  -> active_anon
//	NFS_Unstable  -> nfs_unstable
//	HugePages_Rsvd-> huge_pages_rsvd
//	DirectMap2M   -> direct_map2m
//
// SPEC.md §Metrics contract requires snake_case: promlint rejects camelCase, so
// node_exporter's habit of passing MemTotal through verbatim is not available.
//
// Two boundary rules, and no others. A word starts where a lower-case letter is
// followed by an upper-case one (Mem|Total), and where a run of capitals is
// followed by a lower-case one (S|Reclaimable, KR is K + Reclaimable). A digit
// before a capital is deliberately not a boundary, so DirectMap2M stays
// direct_map2m rather than becoming direct_map2_m.
func snakeCase(name string) string {
	var b strings.Builder
	b.Grow(len(name) + 4)

	writeSep := func() {
		if b.Len() > 0 && b.String()[b.Len()-1] != '_' {
			b.WriteByte('_')
		}
	}

	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '(', c == ' ':
			writeSep()
		case c == ')':
			// Closing paren carries no information once the opening one has
			// become a separator.
		case isUpper(c):
			prevLower := i > 0 && isLower(name[i-1])
			nextLower := i+1 < len(name) && isLower(name[i+1])
			prevUpper := i > 0 && isUpper(name[i-1])
			if prevLower || (prevUpper && nextLower) {
				writeSep()
			}
			b.WriteByte(c - 'A' + 'a')
		default:
			b.WriteByte(c)
		}
	}
	return strings.Trim(b.String(), "_")
}

func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
func isLower(c byte) bool { return c >= 'a' && c <= 'z' }

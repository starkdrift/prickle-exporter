// SPDX-License-Identifier: Apache-2.0

package host

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// newSet returns a Set with the node label the parser tests assert against.
func newSet() *exposition.Set {
	return exposition.NewSet(exposition.L("node", "test"))
}

// requireLines fails unless every want appears verbatim in rendered.
func requireLines(t *testing.T, rendered string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(rendered, w) {
			t.Errorf("missing series:\n  %s", w)
		}
	}
	if t.Failed() {
		t.Logf("full output:\n%s", rendered)
	}
}

// TestCollectStat checks the /proc/stat parse against the captured file,
// including the USER_HZ conversion that turns ticks into seconds.
func TestCollectStat(t *testing.T) {
	set := newSet()
	if err := newFixtureCollector(t).collectStat(set); err != nil {
		t.Fatal(err)
	}

	requireLines(t, set.String(),
		// cpu  7699 378 4944 4840571 656 0 133 5 0 0
		`prickle_host_cpu_seconds_total{node="test",mode="user"} 76.99`,
		`prickle_host_cpu_seconds_total{node="test",mode="nice"} 3.78`,
		`prickle_host_cpu_seconds_total{node="test",mode="system"} 49.44`,
		`prickle_host_cpu_seconds_total{node="test",mode="idle"} 48405.71`,
		`prickle_host_cpu_seconds_total{node="test",mode="iowait"} 6.56`,
		`prickle_host_cpu_seconds_total{node="test",mode="irq"} 0`,
		`prickle_host_cpu_seconds_total{node="test",mode="softirq"} 1.33`,
		`prickle_host_cpu_seconds_total{node="test",mode="steal"} 0.05`,
		`prickle_host_cpu_seconds_total{node="test",mode="guest"} 0`,
		`prickle_host_cpu_seconds_total{node="test",mode="guest_nice"} 0`,

		`prickle_host_context_switches_total{node="test"} 8766266`,
		`prickle_host_forks_total{node="test"} 12744`,
		`prickle_host_procs_running{node="test"} 1`,
		`prickle_host_procs_blocked{node="test"} 0`,
		`prickle_host_boot_time_seconds{node="test"} 1785042511`,

		// Only the leading total of the intr/softirq vector lists.
		`prickle_host_interrupts_total{node="test"} 5048544`,
		`prickle_host_softirqs_total{node="test"} 740265`,
	)
}

// TestPerCoreCPUValues spot-checks a single core against the fixture line
// "cpu17 241 5 1002 200998 6 0 4 0 0 0" — the one core with an unusual system
// time, which makes a mis-indexed column obvious.
func TestPerCoreCPUValues(t *testing.T) {
	set := newSet()
	c := newFixtureCollector(t, func(o *Options) { o.PerCoreCPU = true })
	if err := c.collectStat(set); err != nil {
		t.Fatal(err)
	}

	requireLines(t, set.String(),
		`prickle_host_cpu_core_seconds_total{node="test",cpu="17",mode="user"} 2.41`,
		`prickle_host_cpu_core_seconds_total{node="test",cpu="17",mode="nice"} 0.05`,
		`prickle_host_cpu_core_seconds_total{node="test",cpu="17",mode="system"} 10.02`,
		`prickle_host_cpu_core_seconds_total{node="test",cpu="17",mode="idle"} 2009.98`,
		`prickle_host_cpu_core_seconds_total{node="test",cpu="0",mode="softirq"} 0.29`,
	)
}

// TestCollectMeminfo checks the kB→bytes conversion, the snake_case names, and
// the unitless HugePages_* fields folded into one family.
func TestCollectMeminfo(t *testing.T) {
	set := newSet()
	if err := newFixtureCollector(t).collectMeminfo(set); err != nil {
		t.Fatal(err)
	}

	requireLines(t, set.String(),
		// MemTotal: 247405756 kB. The kernel's "kB" is KiB.
		`prickle_host_memory_mem_total_bytes{node="test"} 253343494144`,
		`prickle_host_memory_mem_free_bytes{node="test"} 248480178176`,
		`prickle_host_memory_mem_available_bytes{node="test"} 249700446208`,
		`prickle_host_memory_swap_total_bytes{node="test"} 0`,
		// Active(anon): 2164 kB
		`prickle_host_memory_active_anon_bytes{node="test"} 2215936`,
		// SReclaimable / SUnreclaim / KReclaimable: capital runs.
		`prickle_host_memory_slab_reclaimable_bytes{node="test"} 173404160`,
		`prickle_host_memory_slab_unreclaim_bytes{node="test"} 139051008`,
		`prickle_host_memory_kernel_reclaimable_bytes{node="test"} 173404160`,
		// NFS_Unstable and Committed_AS: existing underscores kept.
		`prickle_host_memory_nfs_unstable_bytes{node="test"} 0`,
		`prickle_host_memory_committed_as_bytes{node="test"} 3386413056`,
		// DirectMap2M: no boundary between a digit and a capital.
		`prickle_host_memory_direct_map2m_bytes{node="test"} 4857004032`,
		`prickle_host_memory_direct_map1g_bytes{node="test"} 254476812288`,
		`prickle_host_memory_hugepagesize_bytes{node="test"} 2097152`,
		// Unitless counts share one family with a state label.
		`prickle_host_memory_huge_pages{node="test",state="total"} 0`,
		`prickle_host_memory_huge_pages{node="test",state="free"} 0`,
		`prickle_host_memory_huge_pages{node="test",state="rsvd"} 0`,
		`prickle_host_memory_huge_pages{node="test",state="surp"} 0`,
	)
}

// TestSnakeCase pins the converter's two boundary rules.
func TestSnakeCase(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"MemTotal", "mem_total"},
		{"MemFree", "mem_free"},
		{"Buffers", "buffers"},
		{"SwapCached", "swap_cached"},
		{"Active(anon)", "active_anon"},
		{"Inactive(file)", "inactive_file"},
		{"SReclaimable", "s_reclaimable"},
		{"SUnreclaim", "s_unreclaim"},
		{"KReclaimable", "k_reclaimable"},
		{"NFS_Unstable", "nfs_unstable"},
		{"Committed_AS", "committed_as"},
		{"HugePages_Total", "huge_pages_total"},
		{"Hugepagesize", "hugepagesize"},
		{"AnonHugePages", "anon_huge_pages"},
		{"ShmemPmdMapped", "shmem_pmd_mapped"},
		{"VmallocChunk", "vmalloc_chunk"},
		{"Percpu", "percpu"},
		{"HardwareCorrupted", "hardware_corrupted"},
		{"WritebackTmp", "writeback_tmp"},
		{"DirectMap4k", "direct_map4k"},
		{"DirectMap2M", "direct_map2m"},
		{"DirectMap1G", "direct_map1g"},
	} {
		if got := snakeCase(tc.in); got != tc.want {
			t.Errorf("snakeCase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMeminfoNamesAreUnique guards the converter's one real hazard: two
// distinct kernel fields collapsing to the same metric name, which would
// silently produce a duplicate series.
func TestMeminfoNamesAreUnique(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(fixtureDir, "proc", "meminfo"))
	if err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		converted := metricSegment(name)
		if prev, dup := seen[converted]; dup {
			t.Errorf("%q and %q both convert to %q", prev, name, converted)
		}
		seen[converted] = name
	}
	if len(seen) != 51 {
		t.Errorf("converted %d field names, want 51 — did the fixture change?", len(seen))
	}
}

// TestCollectDiskstats checks the sector→byte and millisecond→second scaling,
// the _info split, and the default device filter.
func TestCollectDiskstats(t *testing.T) {
	set := newSet()
	if err := newFixtureCollector(t).collectDiskstats(set); err != nil {
		t.Fatal(err)
	}
	out := set.String()

	requireLines(t, out,
		// 252 0 vda 25188 3354 1884114 4515 29661 56042 50588588 37904 0 32716 42915 732 0 7262488 63 5216 431
		`prickle_host_disk_reads_completed_total{node="test",device="vda"} 25188`,
		`prickle_host_disk_reads_merged_total{node="test",device="vda"} 3354`,
		`prickle_host_disk_read_bytes_total{node="test",device="vda"} 964666368`,   // 1884114 × 512
		`prickle_host_disk_read_seconds_total{node="test",device="vda"} 4.515`,     // 4515 ms
		`prickle_host_disk_writes_completed_total{node="test",device="vda"} 29661`, //
		`prickle_host_disk_written_bytes_total{node="test",device="vda"} 25901357056`,
		`prickle_host_disk_write_seconds_total{node="test",device="vda"} 37.904`,
		`prickle_host_disk_io_now{node="test",device="vda"} 0`,
		`prickle_host_disk_io_time_seconds_total{node="test",device="vda"} 32.716`,
		`prickle_host_disk_io_time_weighted_seconds_total{node="test",device="vda"} 42.915`,
		// Discard columns (4.18+) and flush columns (5.5+) are both present.
		`prickle_host_disk_discards_completed_total{node="test",device="vda"} 732`,
		`prickle_host_disk_discarded_bytes_total{node="test",device="vda"} 3718393856`,
		`prickle_host_disk_discard_seconds_total{node="test",device="vda"} 0.063`,
		`prickle_host_disk_flush_requests_total{node="test",device="vda"} 5216`,
		`prickle_host_disk_flush_seconds_total{node="test",device="vda"} 0.431`,
		// Major and minor are descriptive: _info, not the hot series.
		`prickle_host_disk_info{node="test",device="vda",major="252",minor="0"} 1`,
		`prickle_host_disk_info{node="test",device="vda1",major="252",minor="1"} 1`,
	)

	if strings.Contains(out, `device="loop0"`) {
		t.Error("loop0 survived the default device filter")
	}
	if !strings.Contains(out, `device="vda1"`) {
		t.Error("partitions should be kept by the default device filter")
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "prickle_host_disk_") &&
			!strings.HasPrefix(line, "prickle_host_disk_info") &&
			(strings.Contains(line, "major=") || strings.Contains(line, "minor=")) {
			t.Errorf("descriptive label on a hot series: %s", line)
		}
	}
}

// TestShortDiskstatsLine checks a pre-4.18 kernel: eleven columns, no discard
// or flush. The absent columns must produce no series rather than zeros.
func TestShortDiskstatsLine(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "proc", "diskstats"),
		"   8       0 sda 100 10 2000 50 200 20 4000 80 0 130 130\n")

	set := newSet()
	if err := New(Options{Roots: fsroot.At(dir)}).collectDiskstats(set); err != nil {
		t.Fatal(err)
	}

	out := set.String()
	requireLines(t, out,
		`prickle_host_disk_reads_completed_total{node="test",device="sda"} 100`,
		`prickle_host_disk_io_time_weighted_seconds_total{node="test",device="sda"} 0.13`,
	)
	for _, absent := range []string{"discards_completed", "discarded_bytes", "flush_requests"} {
		if strings.Contains(out, absent) {
			t.Errorf("%s reported for a kernel that does not have the column", absent)
		}
	}
}

// TestCollectNetDev checks the colon split, which is the whole difficulty of
// this file: a long interface name leaves no space before the first count.
func TestCollectNetDev(t *testing.T) {
	set := newSet()
	if err := newFixtureCollector(t).collectNetDev(set); err != nil {
		t.Fatal(err)
	}

	requireLines(t, set.String(),
		//     eth0: 467416665   34157 ... 2212454   18024
		`prickle_host_network_receive_bytes_total{node="test",interface="eth0"} 467416665`,
		`prickle_host_network_receive_packets_total{node="test",interface="eth0"} 34157`,
		`prickle_host_network_transmit_bytes_total{node="test",interface="eth0"} 2212454`,
		`prickle_host_network_transmit_packets_total{node="test",interface="eth0"} 18024`,
		// "vethec7b279:     126" — name touching the colon.
		`prickle_host_network_receive_bytes_total{node="test",interface="vethec7b279"} 126`,
		`prickle_host_network_transmit_bytes_total{node="test",interface="vethec7b279"} 1854`,
		// "  cni0: 7341310   17365 ... 89 ..." — the only non-zero multicast.
		`prickle_host_network_receive_multicast_total{node="test",interface="cni0"} 89`,
		// flannel.1 is all zeroes but for a transmit drop; a dot in the name.
		`prickle_host_network_transmit_drop_total{node="test",interface="flannel.1"} 5`,
		`prickle_host_network_receive_bytes_total{node="test",interface="lo"} 33503626`,
	)

	if n := countMatching(set.String(), "prickle_host_network_receive_bytes_total{"); n != 15 {
		t.Errorf("interfaces = %d, want 15 (the two header lines must be skipped)", n)
	}
}

// TestNetDevIgnoreRegexp checks the opt-in filter an operator reaches for on a
// Kubernetes node.
func TestNetDevIgnoreRegexp(t *testing.T) {
	set := newSet()
	c := newFixtureCollector(t, func(o *Options) {
		o.IgnoredNetDevices = regexp.MustCompile(`^veth`)
	})
	if err := c.collectNetDev(set); err != nil {
		t.Fatal(err)
	}

	out := set.String()
	if strings.Contains(out, `interface="veth`) {
		t.Error("veth interfaces survived the ignore regexp")
	}
	if n := countMatching(out, "prickle_host_network_receive_bytes_total{"); n != 6 {
		t.Errorf("interfaces = %d, want 6 after dropping the 9 veths", n)
	}
}

// TestCollectLoadavg checks the three averages, and that nothing else is
// exposed: SPEC.md §Collectors, fields 4-5 stay unread.
func TestCollectLoadavg(t *testing.T) {
	set := newSet()
	if err := newFixtureCollector(t).collectLoadavg(set); err != nil {
		t.Fatal(err)
	}

	out := set.String()
	requireLines(t, out,
		`prickle_host_load1{node="test"} 0.07`,
		`prickle_host_load5{node="test"} 0.05`,
		`prickle_host_load15{node="test"} 0`,
	)
	if n := countSeries(out); n != 3 {
		t.Errorf("loadavg produced %d series, want exactly 3:\n%s", n, out)
	}
}

// TestCollectPressure checks the microsecond→second conversion and that only
// total= is exposed, not the kernel's three moving averages.
func TestCollectPressure(t *testing.T) {
	set := newSet()
	if err := newFixtureCollector(t).collectPressure(set); err != nil {
		t.Fatal(err)
	}

	out := set.String()
	requireLines(t, out,
		// cpu: some total=4788821, full total=0
		`prickle_host_pressure_stalled_seconds_total{node="test",resource="cpu",kind="some"} 4.788821`,
		`prickle_host_pressure_stalled_seconds_total{node="test",resource="cpu",kind="full"} 0`,
		`prickle_host_pressure_stalled_seconds_total{node="test",resource="memory",kind="some"} 0`,
		// io: some total=1167390, full total=1145566
		`prickle_host_pressure_stalled_seconds_total{node="test",resource="io",kind="some"} 1.16739`,
		`prickle_host_pressure_stalled_seconds_total{node="test",resource="io",kind="full"} 1.145566`,
	)
	if n := countSeries(out); n != 6 {
		t.Errorf("pressure produced %d series, want 6 (3 resources × 2 kinds):\n%s", n, out)
	}
	if strings.Contains(out, "avg10") || strings.Contains(out, "avg300") {
		t.Error("the kernel's moving averages should not be exposed")
	}
}

// TestMissingPressureIsNotAnError checks that a kernel without CONFIG_PSI is
// treated as a supported configuration, not a failure.
func TestMissingPressureIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}

	set := newSet()
	if err := New(Options{Roots: fsroot.At(dir)}).collectPressure(set); err != nil {
		t.Fatalf("absent /proc/pressure reported as an error: %v", err)
	}
	if n := countSeries(set.String()); n != 0 {
		t.Errorf("got %d series with no /proc/pressure, want 0", n)
	}
}

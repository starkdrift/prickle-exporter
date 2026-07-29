// SPDX-License-Identifier: Apache-2.0

//go:build nvml

// This file is the hardware verification SPEC.md §Testing rules asks for:
// "the two sources must produce identical metric output for the same GPU, and
// a hardware test asserts that". It cannot run anywhere else — NVML is a C
// call, not a file read — so every test here skips unless the library actually
// loads and nvidia-smi is on PATH.
//
// It is not a substitute for the fixture tests. Those pin the parse of a
// captured format; this pins the agreement of two live implementations, which
// is the only thing a fixture cannot express.
package gpu

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// migOnlyFamilies are the families NVML publishes and nvidia-smi cannot.
//
// SPEC.md §Collectors calls NVML "the only reliable source of MIG topology"
// for exactly this: per-instance memory and utilization appear in no
// nvidia-smi CSV query, only in a box-drawing table whose column widths shift
// with driver version (internal/collector/gpu/testdata/README.md §Deliberately
// not parsed).
//
// So the sources are identical up to this list, and the list is asserted
// rather than described: a future divergence that is not one of these fails
// the test instead of being discovered on a dashboard.
var migOnlyFamilies = []string{
	prefix + "mig_memory_used_bytes",
	prefix + "mig_memory_total_bytes",
	prefix + "mig_utilization_ratio",
}

// tolerances are how far a shared series may drift between two reads taken
// milliseconds apart, in the metric's own units.
//
// Three separate reasons appear here and are worth keeping distinct:
//
//   - nvidia-smi's --format=nounits prints memory in MiB, so a byte-exact
//     comparison would fail on rounding alone.
//   - temperature and power are physical measurements of a loaded card and
//     move between the two reads.
//   - utilization is a sampled percentage that swings the full range under a
//     bursty workload. Its *presence* is what the sources must agree on, so it
//     is compared as "any value" rather than dropped from the comparison —
//     which keeps the series identity itself asserted.
var tolerances = map[string]float64{
	prefix + "memory_used_bytes":      32 << 20,
	prefix + "memory_total_bytes":     1 << 20,
	prefix + "mig_memory_used_bytes":  32 << 20,
	prefix + "mig_memory_total_bytes": 1 << 20,
	prefix + "temperature_celsius":    5,
	prefix + "power_watts":            60,
	prefix + "utilization_ratio":      math.Inf(1),
	prefix + "mig_utilization_ratio":  math.Inf(1),
	prefix + "process_memory_bytes":   64 << 20,
}

// renderHardware collects one source into a rendered exposition set.
func renderHardware(t *testing.T, source string) string {
	t.Helper()

	c := New(Options{NVIDIASource: source, PerProcess: true})
	if c.SourceName() != source {
		t.Skipf("%s source unavailable on this host: %v", source, c.SelectionError())
	}
	t.Cleanup(func() { c.Close() })

	set := exposition.NewSet(exposition.L("node", "hardware"))
	if err := c.Collect(context.Background(), set); err != nil {
		t.Fatalf("%s source: %v", source, err)
	}
	if err := set.Err(); err != nil {
		t.Fatalf("%s source: exposition problems: %v", source, err)
	}
	return set.String()
}

// samples parses rendered output into series identity -> value. The identity
// is the whole `name{labels}` string, so a label *value* that differs between
// the sources — a MIG profile spelled two ways, say — shows up as a missing
// series rather than as a value mismatch.
func samples(t *testing.T, rendered string) map[string]float64 {
	t.Helper()

	out := map[string]float64{}
	for _, line := range strings.Split(rendered, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndex(line, " ")
		if i < 0 {
			t.Fatalf("unparseable sample line: %q", line)
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("unparseable value in %q: %v", line, err)
		}
		out[line[:i]] = v
	}
	return out
}

// familyOf returns the metric name of a series identity.
func familyOf(series string) string {
	if i := strings.IndexByte(series, '{'); i >= 0 {
		return series[:i]
	}
	return series
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestSourcesAgreeOnHardware is the assertion SPEC.md §Testing rules requires.
//
// Both sources read the same card in the same second and must produce the same
// series — same families, same label keys, same label values — up to three
// documented exceptions: the source gauge that names which implementation ran,
// the MIG families only NVML can fill, and per-process series for processes
// whose executable NVML could not resolve (see below).
//
// Verified on an H100 80GB, driver 580.173.02, in both Default and MIG mode,
// 2026-07-29. It found three real disagreements, each fixed rather than
// tolerated: NVML reporting driver-reserved memory inside memory_used_bytes,
// NVML spelling a MIG profile "10gb" where nvidia-smi spells it "1g.10gb", and
// a second GPU collector in one process being handed an already-closed NVML
// source.
func TestSourcesAgreeOnHardware(t *testing.T) {
	nvml := samples(t, renderHardware(t, SourceNVML))
	smi := samples(t, renderHardware(t, SourceSMI))

	// The source gauge is the one series that must differ: it names the
	// implementation that produced the scrape.
	for _, set := range []map[string]float64{nvml, smi} {
		for series := range set {
			if familyOf(series) == prefix+"nvidia_source_info" {
				delete(set, series)
			}
		}
	}

	// NVML-only families are lifted out and checked separately, so what remains
	// is the set both sources claim to serve.
	migOnly := map[string]float64{}
	for series, v := range nvml {
		for _, family := range migOnlyFamilies {
			if familyOf(series) == family {
				migOnly[series] = v
				delete(nvml, series)
			}
		}
	}
	for series := range smi {
		for _, family := range migOnlyFamilies {
			if familyOf(series) == family {
				t.Errorf("the nvidia-smi source emitted %s, which no CSV query publishes", series)
			}
		}
	}

	// A process whose exe symlink NVML could not read is dropped by that source
	// rather than keyed on a PID (see readProcesses). nvidia-smi gets the name
	// from the driver instead, so it can report processes NVML cannot: NVML's
	// process series must be a subset of nvidia-smi's, not equal to it.
	for series := range nvml {
		if familyOf(series) != prefix+"process_memory_bytes" {
			continue
		}
		if _, ok := smi[series]; !ok {
			t.Errorf("NVML reported a process series nvidia-smi did not: %s", series)
		}
		delete(nvml, series)
		delete(smi, series)
	}
	for series := range smi {
		if familyOf(series) == prefix+"process_memory_bytes" {
			t.Logf("nvidia-smi saw a process NVML could not name (unreadable exe): %s", series)
			delete(smi, series)
		}
	}

	for _, series := range sortedKeys(nvml) {
		want, ok := smi[series]
		if !ok {
			t.Errorf("NVML emitted a series nvidia-smi did not:\n  %s", series)
			continue
		}
		got := nvml[series]
		tolerance, known := tolerances[familyOf(series)]
		if !known {
			// An _info or a mode gauge: a constant, and equality is the point.
			tolerance = 0
		}
		if math.Abs(got-want) > tolerance {
			t.Errorf("%s\n  nvml: %v\n   smi: %v\n  differ by more than %v",
				series, got, want, tolerance)
		}
	}
	for _, series := range sortedKeys(smi) {
		if _, ok := nvml[series]; !ok {
			t.Errorf("nvidia-smi emitted a series NVML did not:\n  %s", series)
		}
	}

	// The MIG-only families are NVML's reason to exist, so on a partitioned
	// card their absence is a failure, not an option.
	partitioned := false
	for series, v := range smi {
		if familyOf(series) == prefix+"mig_enabled" && v == 1 {
			partitioned = true
		}
	}
	switch {
	case partitioned && len(migOnly) == 0:
		t.Error("the card is partitioned but NVML published no per-MIG memory; " +
			"that is the data SPEC.md §Collectors says only NVML can provide")
	case !partitioned && len(migOnly) > 0:
		t.Errorf("per-MIG series on a card that is not partitioned: %v", sortedKeys(migOnly))
	case partitioned:
		t.Logf("MIG mode: %d NVML-only series", len(migOnly))
	}
}

// TestMIGProfileSpellingAgrees pins the specific label value that hardware
// caught disagreeing. Both sources put a profile on prickle_gpu_mig_info, and
// nvidia-smi -L spells it "1g.10gb"; NVML has no entry point returning that
// string, so migProfile assembles it from the instance's slice count and
// memory. A test on the whole series identity would catch this too, but not
// name it, and this is the one that says what broke.
func TestMIGProfileSpellingAgrees(t *testing.T) {
	nvml := profilesByMIGUUID(t, renderHardware(t, SourceNVML))
	smi := profilesByMIGUUID(t, renderHardware(t, SourceSMI))

	if len(smi) == 0 {
		t.Skip("this card is not partitioned; nothing to compare")
	}
	for uuid, want := range smi {
		got, ok := nvml[uuid]
		if !ok {
			t.Errorf("NVML did not report MIG instance %s", uuid)
			continue
		}
		if got != want {
			t.Errorf("MIG %s profile: nvml %q, smi %q — a label value that "+
				"differs between the two artifacts for the same card", uuid, got, want)
		}
	}
}

// profilesByMIGUUID extracts mig_uuid -> profile from rendered output.
func profilesByMIGUUID(t *testing.T, rendered string) map[string]string {
	t.Helper()

	out := map[string]string{}
	for _, line := range strings.Split(rendered, "\n") {
		if !strings.HasPrefix(line, prefix+"mig_info{") {
			continue
		}
		uuid, profile := labelValue(line, "mig_uuid"), labelValue(line, "profile")
		if uuid == "" {
			t.Fatalf("mig_info without a mig_uuid: %q", line)
		}
		out[uuid] = profile
	}
	return out
}

// labelValue reads one label out of a sample line.
func labelValue(line, key string) string {
	i := strings.Index(line, key+`="`)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key)+2:]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// TestNVMLSourceSurvivesAnEarlierClose is the regression guard for what
// `prickle diagnose` hit on hardware: it builds a GPU collector to describe the
// live source, closes it, and then builds the real one from cfg.collectors().
// The NVML library handle is process-global, and a single shared source handed
// out after its own Close reported "NVML source is closed" on a host where NVML
// was working perfectly — an error message that pointed at the hardware rather
// than at the exporter.
func TestNVMLSourceSurvivesAnEarlierClose(t *testing.T) {
	first := New(Options{NVIDIASource: SourceNVML})
	if first.SourceName() != SourceNVML {
		t.Skipf("NVML unavailable on this host: %v", first.SelectionError())
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := New(Options{NVIDIASource: SourceNVML})
	defer second.Close()
	if second.SourceName() != SourceNVML {
		t.Fatalf("a second collector could not load NVML: %v", second.SelectionError())
	}

	set := exposition.NewSet()
	if err := second.Collect(context.Background(), set); err != nil {
		t.Fatalf("a collector built after an earlier one was closed: %v", err)
	}
	if countMatching(set.String(), prefix+"info{") == 0 {
		t.Error("the second collector reported no devices")
	}
}

// TestNVMLReadsNoReservedMemory is the regression guard for the accounting
// difference hardware found. nvmlDeviceGetMemoryInfo folds driver-reserved
// memory into `used` — 480 MiB of it on the H100 this was measured on — while
// nvidia-smi reports the _v2 number that excludes it. Half a gigabyte per card
// is not rounding, and it would have shifted every memory panel and every
// capacity alert depending on which artifact was deployed.
func TestNVMLReadsNoReservedMemory(t *testing.T) {
	nvml := samples(t, renderHardware(t, SourceNVML))
	smi := samples(t, renderHardware(t, SourceSMI))

	for series, got := range nvml {
		if familyOf(series) != prefix+"memory_used_bytes" {
			continue
		}
		want, ok := smi[series]
		if !ok {
			t.Errorf("no nvidia-smi counterpart for %s", series)
			continue
		}
		// One MiB is nvidia-smi's own print granularity. Reserved memory is
		// three orders of magnitude above it.
		if diff := math.Abs(got - want); diff > 32<<20 {
			t.Errorf("%s: nvml %v, smi %v, differ by %.0f MiB — reserved memory "+
				"is being counted as used", series, got, want, diff/(1<<20))
		}
	}
}

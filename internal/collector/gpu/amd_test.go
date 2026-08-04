// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// The AMD capture: 2× MI300X under a HIP kernel, one process on the host and
// one in a Docker container, each holding VRAM on both cards. See
// testdata/README.md §AMD.
const amdFixture = "mi300x-2gpu-20260804"

// The two cards' identities, as sysfs `unique_id` gives them. Every assertion
// below keys on these rather than on card0/card8: which DRM node a card lands
// on is enumeration order, and the point of reading unique_id is that it is not.
const (
	amdCard0UUID = "594afe08e1ab3ae6"
	amdCard8UUID = "25a594c05f2eb594"
)

// noNVIDIA forces the NVIDIA half of the collector to find nothing.
//
// Without it this test would spawn a real nvidia-smi on any developer machine
// that has one, underneath a fixture that is a sysfs tree — and produce a
// different result there than in CI. The AMD capture host has no NVIDIA card,
// so declining is also what the fixture actually describes.
func noNVIDIA(o *Options) {
	o.nvidiaCandidates = func(Options) []sourceCandidate {
		return []sourceCandidate{{
			name:  "none",
			build: func(Options) (nvidiaSource, error) { return nil, ErrUnavailable },
		}}
	}
}

// collectAMD renders one pass over a captured sysfs tree.
func collectAMD(t *testing.T, tree string, mutate ...func(*Options)) string {
	t.Helper()

	opts := Options{Roots: fsroot.At(filepath.Join("testdata", tree))}
	noNVIDIA(&opts)
	for _, m := range mutate {
		m(&opts)
	}

	c := New(opts)
	set := exposition.NewSet(exposition.L("node", "fixture"))
	if err := c.Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := set.Err(); err != nil {
		t.Fatalf("exposition problems: %v", err)
	}
	return set.String()
}

// TestAMDGolden pins the entire AMD output. A diff here is a metric contract
// change and should be read as one.
func TestAMDGolden(t *testing.T) {
	got := collectAMD(t, amdFixture, func(o *Options) { o.PerProcess = true })
	path := filepath.Join("testdata", "golden", "gpu-amd-mi300x.prom")

	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (regenerate with -update-golden)", err)
	}
	if got != string(want) {
		t.Errorf("output differs from golden; first difference:\n%s", firstDiff(string(want), got))
	}
}

// TestAMDIdentityComesFromUniqueID is the one assertion the whole AMD path
// rests on. sysfs offers nothing else that is both stable and per-card: the two
// captured MI300X share a device ID, a subsystem ID and a vbios version, and
// their DRM node numbers are enumeration order. If gpu_uuid ever came from one
// of those instead, two cards would collapse into one series and the collapse
// would look like a working exporter.
func TestAMDIdentityComesFromUniqueID(t *testing.T) {
	out := collectAMD(t, amdFixture)

	for _, uuid := range []string{amdCard0UUID, amdCard8UUID} {
		if !strings.Contains(out, `gpu_uuid="`+uuid+`"`) {
			t.Errorf("no series for card %s; the capture has two distinct unique_id values", uuid)
		}
	}
	if amdCard0UUID == amdCard8UUID {
		t.Fatal("fixture is broken: the two cards must not share an identity")
	}

	// Both cards report the same VRAM total, so a metric keyed on anything
	// non-unique would still render — with one card's readings silently lost.
	if n := strings.Count(out, prefix+"memory_total_bytes{"); n != 2 {
		t.Errorf("memory_total_bytes has %d series, want 2 — one per captured card", n)
	}
}

// TestAMDReadsUtilizationMemoryAndSensors checks the four device readings
// against the captured files, including the two unit conversions that have no
// second chance of being noticed: hwmon is millidegrees and microwatts, so a
// missed divide reports a 71°C card as 71000°C and a 660 W one as 660 MW.
func TestAMDReadsUtilizationMemoryAndSensors(t *testing.T) {
	out := collectAMD(t, amdFixture)

	want := []string{
		// gpu_busy_percent is whole percent; the contract is a 0-to-1 ratio.
		prefix + `utilization_ratio{node="fixture",gpu_uuid="` + amdCard0UUID + `"} 1`,
		prefix + `utilization_ratio{node="fixture",gpu_uuid="` + amdCard8UUID + `"} 0.98`,
		// Plain byte counts, unlike nvidia-smi's MiB.
		prefix + `memory_used_bytes{node="fixture",gpu_uuid="` + amdCard0UUID + `"} 2956627968`,
		prefix + `memory_total_bytes{node="fixture",gpu_uuid="` + amdCard0UUID + `"} 205822885888`,
		// temp2_input = 70000 millidegrees, and temp2 is labelled "junction".
		prefix + `temperature_celsius{node="fixture",gpu_uuid="` + amdCard0UUID + `"} 70`,
		prefix + `temperature_celsius{node="fixture",gpu_uuid="` + amdCard8UUID + `"} 70`,
		// power1_input = 641000000 and 659000000 microwatts.
		prefix + `power_watts{node="fixture",gpu_uuid="` + amdCard0UUID + `"} 641`,
		prefix + `power_watts{node="fixture",gpu_uuid="` + amdCard8UUID + `"} 659`,
	}
	for _, line := range want {
		if !strings.Contains(out, line) {
			t.Errorf("missing series:\n  %s", line)
		}
	}
}

// TestAMDFindsSensorsByLabelNotIndex is the specific trap this card sprang on
// the capture script, which asked for temp1_input and power1_average by name
// and got neither.
//
// An MI300X has no temp1 at all: it publishes temp2 (junction) and temp3 (mem),
// named by sibling *_label files. Indexing the sensors instead of reading the
// labels yields no temperature on this card and the *memory* temperature on one
// numbered the other way round — a plausible reading that is simply the wrong
// sensor, which is the failure a hardcoded index actually produces.
func TestAMDFindsSensorsByLabelNotIndex(t *testing.T) {
	dir := filepath.Join("testdata", amdFixture, "sys", "class", "drm", "card0", "device", "hwmon", "hwmon0")

	if _, err := os.Stat(filepath.Join(dir, "temp1_input")); !os.IsNotExist(err) {
		t.Fatal("fixture no longer lacks temp1_input; this test's premise is gone")
	}

	labels, err := hwmonLabels(dir)
	if err != nil {
		t.Fatalf("hwmonLabels: %v", err)
	}
	if got := labels["junction"]; got != "temp2" {
		t.Errorf("junction resolved to %q, want temp2", got)
	}
	if got := labels["mem"]; got != "temp3" {
		t.Errorf("mem resolved to %q, want temp3", got)
	}

	// 70°C is junction (temp2). 42°C is mem (temp3), and reporting it would be
	// a believable number from the wrong sensor.
	out := collectAMD(t, amdFixture)
	if strings.Contains(out, prefix+`temperature_celsius{node="fixture",gpu_uuid="`+amdCard0UUID+`"} 42`) {
		t.Error("temperature came from temp3 (mem) rather than the junction sensor")
	}
}

// TestAMDProcessesJoinThroughPCIAddress covers the structural difference
// between per-process attribution on the two vendors: DRM fdinfo names a GPU by
// `drm-pdev` and carries no UUID, so every process has to be joined back to a
// card through its PCI address.
//
// The captured workload is the case that makes a mistake visible — one process
// on the host and one in a container, each holding memory on *both* cards. A
// join that dropped the address and took the first card would report four
// series as two, all on card0, and the totals would still look plausible.
func TestAMDProcessesJoinThroughPCIAddress(t *testing.T) {
	out := collectAMD(t, amdFixture, func(o *Options) { o.PerProcess = true })

	// 1196292 KiB and 1196288 KiB for the host process; 671992 and 671996 KiB
	// for the containerised one. Different per card, so a swap is detectable.
	want := []string{
		prefix + `process_memory_bytes{node="fixture",gpu_uuid="` + amdCard0UUID + `",command="gpu-spin",container=""} 1225003008`,
		prefix + `process_memory_bytes{node="fixture",gpu_uuid="` + amdCard8UUID + `",command="gpu-spin",container=""} 1224998912`,
	}
	for _, line := range want {
		if !strings.Contains(out, line) {
			t.Errorf("missing host-process series:\n  %s", line)
		}
	}

	// The containerised process shares its command with the host one, so the
	// two are only distinguishable by `container` — which is the label that
	// makes a GPU process joinable to a pod.
	const containerID = "ea20f94c6cfe0d9b9cc735a683b4a674d8ef7772a8df040eed47c06cf62e54b9"
	if !strings.Contains(out, `command="gpu-spin",container="`+containerID+`"`) {
		t.Error("the containerised GPU process was not attributed to its container")
	}
	if n := strings.Count(out, prefix+"process_memory_bytes{"); n != 4 {
		t.Errorf("got %d process series, want 4 — two processes on two cards each", n)
	}
}

// TestAMDPerProcessIsOptIn keeps AMD on the same footing as NVIDIA: SPEC.md
// §Metrics contract makes per-process attribution opt-in, and reading every
// process's fdinfo is exactly the cost that decision is about.
func TestAMDPerProcessIsOptIn(t *testing.T) {
	if out := collectAMD(t, amdFixture); strings.Contains(out, prefix+"process_memory_bytes") {
		t.Error("process_memory_bytes emitted without -collector.gpu.per-process")
	}
}

// TestAMDEmitsNoMIGFamilies holds the line SPEC.md draws for cgroup v1 and PSI:
// where a platform has no such concept the family is **absent**, not zero.
//
// prickle_gpu_mig_enabled 0 on an AMD card would be a specific false claim —
// that this is an NVIDIA card in Default mode — and it is the kind that survives
// review, because 0 is what an unpartitioned card reports.
func TestAMDEmitsNoMIGFamilies(t *testing.T) {
	out := collectAMD(t, amdFixture, func(o *Options) { o.PerProcess = true })

	for _, family := range []string{"mig_enabled", "mig_info", "mig_memory_used_bytes", "nvidia_source_info"} {
		if strings.Contains(out, prefix+family) {
			t.Errorf("%s emitted for an AMD card", prefix+family)
		}
	}
	if !strings.Contains(out, prefix+`amd_partition_info{node="fixture",gpu_uuid="`+amdCard0UUID+`",compute_partition="SPX",memory_partition="NPS1"}`) {
		t.Error("amd_partition_info missing; SPX/NPS1 is what the capture records")
	}
}

// TestAMDVendorLabel checks the one label added to an existing series. On a
// mixed host it is the only thing on prickle_gpu_info that says which stack a
// card was read through, because AMD sysfs publishes no marketing name.
func TestAMDVendorLabel(t *testing.T) {
	out := collectAMD(t, amdFixture)

	if !strings.Contains(out, `vendor="amd"`) {
		t.Error(`prickle_gpu_info carries no vendor="amd"`)
	}
	// The PCI ID is the documented fallback, and this card is in the table.
	if !strings.Contains(out, `name="AMD Instinct MI300X VF"`) {
		t.Error("the captured PCI ID did not resolve to its market name")
	}
}

// TestAMDAbsentWithoutAnAMDCard is the negative. The driverless-H100 tree is a
// mirrored sysfs layout with no amdgpu card in it, so the AMD reader must find
// nothing and say nothing — a host with one vendor's GPU should not acquire a
// second vendor's empty series.
func TestAMDAbsentWithoutAnAMDCard(t *testing.T) {
	out := collectAMD(t, "h100-nodriver-20260801", func(o *Options) { o.PerProcess = true })

	if strings.Contains(out, prefix+"info") {
		t.Errorf("emitted GPU series on a host with no amdgpu card:\n%s", out)
	}
}

// TestAMDDriverTestIsNotThePCIClass records why the card test reads uevent's
// DRIVER= rather than reusing pci.go's display-class match.
//
// An MI300X reports PCI class 0x120000 — a processing accelerator — and no
// device on the captured host reports a 0x03xx display class at all. A class
// test inherited from the NVIDIA path would find zero AMD GPUs on a machine
// with two.
func TestAMDDriverTestIsNotThePCIClass(t *testing.T) {
	device := filepath.Join("testdata", amdFixture, "sys", "class", "drm", "card0", "device")

	class, err := readTrimmed(filepath.Join(device, "class"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(class, displayBaseClass) {
		t.Fatalf("card class is %s, which the NVIDIA display-class test would match; "+
			"this test's premise is gone", class)
	}

	uevent, err := parseUevent(filepath.Join(device, "uevent"))
	if err != nil {
		t.Fatal(err)
	}
	if uevent["DRIVER"] != amdDriver {
		t.Errorf("uevent DRIVER is %q, want %q", uevent["DRIVER"], amdDriver)
	}
	if uevent["PCI_SLOT_NAME"] == "" {
		t.Error("uevent carries no PCI_SLOT_NAME; per-process attribution has nothing to join on")
	}
}

// TestParseDRMSize covers the unit suffix. The kernel writes these values as a
// number and a unit, and a missed KiB multiplier under-reports a process's VRAM
// by 1024× — small enough to look like an idle process rather than a bug.
func TestParseDRMSize(t *testing.T) {
	cases := []struct {
		in    string
		want  uint64
		valid bool
	}{
		{"1196292 KiB", 1196292 * 1024, true},
		{"512 MiB", 512 * 1024 * 1024, true},
		{"2 GiB", 2 * 1024 * 1024 * 1024, true},
		{"4096", 4096, true},
		{"0", 0, true},
		{"", 0, false},
		{"nonsense", 0, false},
		// An unrecognised unit is dropped rather than assumed to be bytes: a
		// wrong multiplier is a wrong metric and there is no safe default.
		{"12 TiB", 0, false},
	}
	for _, c := range cases {
		got, ok := parseDRMSize(c.in)
		if ok != c.valid || got != c.want {
			t.Errorf("parseDRMSize(%q) = %d, %v; want %d, %v", c.in, got, ok, c.want, c.valid)
		}
	}
}

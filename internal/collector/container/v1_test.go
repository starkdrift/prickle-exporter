// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// v1Dir is a pure cgroup v1 host: Rocky Linux 8.10, kernel 4.18, Docker 26.1.3
// on the cgroupfs driver, captured 2026-08-01.
//
// RHEL 8 defaults to v1 and is supported into 2029, which is what reversed
// SPEC.md §Hard constraints #4 — running diagnose here and being told the host
// was out of scope made the old decision hard to defend.
//
// Three containers, deliberately the same spread as the v2 Docker capture so
// the two hierarchies can be diffed rather than merely each checked.
const v1Dir = "testdata/docker-cgroupv1-20260801"

const (
	v1Containers = 3
	// The busy loop against a 0.25-CPU quota, and the one with a 64 MiB limit.
	v1ThrottledID = "118a4f0b17290689be61e988d9789e4ec7bf40e7054a581882dbab33c33b794e"
)

func v1Collect(t *testing.T) string {
	t.Helper()
	set := newFixtureSet()
	if err := New(Options{Roots: fsroot.At(v1Dir)}).Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := set.Err(); err != nil {
		t.Fatalf("exposition problems: %v", err)
	}
	return set.String()
}

// sampleFor returns one metric's value for one container.
func sampleFor(t *testing.T, rendered, metric, container string) (float64, bool) {
	t.Helper()
	for _, line := range strings.Split(rendered, "\n") {
		if !strings.HasPrefix(line, metric+"{") || !strings.Contains(line, `container="`+container+`"`) {
			continue
		}
		fields := strings.Fields(line)
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("parsing %q: %v", line, err)
		}
		return v, true
	}
	return 0, false
}

// TestV1IsReadAtAll is the headline: this tree produced nothing before.
func TestV1IsReadAtAll(t *testing.T) {
	rendered := v1Collect(t)
	if got := strings.Count(rendered, "prickle_container_info{"); got != v1Containers {
		t.Fatalf("found %d containers, want %d", got, v1Containers)
	}
	if !strings.Contains(rendered, `runtime="docker"`) {
		t.Error(`no container reports runtime="docker"`)
	}
}

// TestV1UnitsAreConverted is the test most likely to catch a real defect.
//
// v1 counts CPU time in nanoseconds where v2's cpu.stat uses microseconds, and
// splits user/system in USER_HZ. Getting either wrong yields a number wrong by
// a factor of 1000 or 100 that still looks entirely plausible on a graph, so
// each is checked against the raw file rather than against a remembered value.
func TestV1UnitsAreConverted(t *testing.T) {
	rendered := v1Collect(t)
	cpuDir := filepath.Join(v1Dir, "sys", "fs", "cgroup", "cpu,cpuacct", "docker", v1ThrottledID)

	raw := func(file string) uint64 {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(cpuDir, file))
		if err != nil {
			t.Fatal(err)
		}
		v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	// cpuacct.usage is nanoseconds.
	got, ok := sampleFor(t, rendered, "prickle_container_cpu_usage_seconds_total", v1ThrottledID)
	if !ok {
		t.Fatal("no cpu_usage_seconds_total for the throttled container")
	}
	if want := float64(raw("cpuacct.usage")) / 1e9; got != want {
		t.Errorf("cpu_usage_seconds_total = %v, want %v (cpuacct.usage is nanoseconds)", got, want)
	}

	// cpu.stat's throttled_time is nanoseconds too — v2 spells the same idea
	// throttled_usec, in microseconds.
	stat, err := readFlatKeyed(filepath.Join(cpuDir, "cpu.stat"))
	if err != nil {
		t.Fatal(err)
	}
	got, ok = sampleFor(t, rendered, "prickle_container_cpu_throttled_seconds_total", v1ThrottledID)
	if !ok {
		t.Fatal("no cpu_throttled_seconds_total for the throttled container")
	}
	if want := float64(stat["throttled_time"]) / 1e9; got != want {
		t.Errorf("cpu_throttled_seconds_total = %v, want %v (throttled_time is nanoseconds)", got, want)
	}
}

// TestV1QuotaBecomesCores: v1 splits v2's single `cpu.max` line across two
// files, and uses -1 rather than `max` for "no quota".
func TestV1QuotaBecomesCores(t *testing.T) {
	rendered := v1Collect(t)

	got, ok := sampleFor(t, rendered, "prickle_container_cpu_limit_cores", v1ThrottledID)
	if !ok {
		t.Fatal("no cpu_limit_cores for a container created with --cpus=0.25")
	}
	if got != 0.25 {
		t.Errorf("cpu_limit_cores = %v, want 0.25", got)
	}

	// The unlimited container must emit no limit series at all, exactly as on
	// v2 — a quota of -1 must not become a limit of -1 or of 0.
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, "prickle_container_cpu_limit_cores{") {
			if v := line[strings.LastIndex(line, " ")+1:]; v == "-1" || v == "0" {
				t.Errorf("a container reports cpu_limit_cores %s; unset must be absent", v)
			}
		}
	}
}

// TestV1WeightIsOnTheV2Scale pins the contract not forking.
//
// cpu.shares defaults to 1024 and cpu.weight to 100. Reporting the raw v1
// number under the same metric name would make prickle_container_cpu_weight
// mean two different things depending on the host's kernel configuration,
// which SPEC.md §Hard constraints #4 explicitly forbids.
func TestV1WeightIsOnTheV2Scale(t *testing.T) {
	got, ok := sampleFor(t, v1Collect(t), "prickle_container_cpu_weight", v1ThrottledID)
	if !ok {
		t.Fatal("no cpu_weight for the throttled container")
	}
	if got != 100 {
		t.Errorf("cpu_weight = %v, want 100 — the default cpu.shares of 1024 on the v2 scale", got)
	}
}

// TestV1UnlimitedMemoryIsAbsent: v1 spells "no limit" as a near-max integer
// rather than the string `max`. Emitting it would put nine exabytes on every
// unconstrained container and swamp any panel that sums the limit.
func TestV1UnlimitedMemoryIsAbsent(t *testing.T) {
	rendered := v1Collect(t)
	for _, line := range strings.Split(rendered, "\n") {
		if !strings.HasPrefix(line, "prickle_container_memory_limit_bytes{") {
			continue
		}
		v, err := strconv.ParseFloat(line[strings.LastIndex(line, " ")+1:], 64)
		if err != nil {
			t.Fatal(err)
		}
		if v >= v1Unlimited {
			t.Errorf("memory_limit_bytes = %v; the unlimited sentinel must be absent, not reported", v)
		}
	}
	// The 64 MiB container still reports its real limit.
	if !strings.Contains(rendered, "prickle_container_memory_limit_bytes") {
		t.Error("no memory_limit_bytes at all; the limited container should have one")
	}
}

// TestV1HasNoPressureFamily: PSI arrived with the unified hierarchy, so v1
// cannot answer. SPEC.md §Hard constraints #4 requires absence rather than
// zero — a zero reads as "nothing is stalling" instead of "cannot say".
func TestV1HasNoPressureFamily(t *testing.T) {
	if strings.Contains(v1Collect(t), "pressure_stalled") {
		t.Error("v1 emitted a pressure family; this hierarchy has no PSI to read")
	}
}

// TestV1DeviceLabelsAreNames checks the io labels match the v2 path's, which
// resolve major:minor through /proc/diskstats. A bare "252:0" here and a "vda"
// on v2 would be the contract forking in a way no metric name reveals.
func TestV1DeviceLabelsAreNames(t *testing.T) {
	rendered := v1Collect(t)
	if !strings.Contains(rendered, `device="vda"`) {
		t.Error(`no io series carries device="vda"; major:minor was not resolved`)
	}
	if strings.Contains(rendered, `device="252:0"`) {
		t.Error(`an io series still carries the raw major:minor`)
	}
}

// TestV1EmitsNoMetricV2Lacks is the contract check in the strict direction: v1
// must not invent a family name of its own. Every name it emits has to be one
// the v2 path also emits, or the two hierarchies have forked.
func TestV1EmitsNoMetricV2Lacks(t *testing.T) {
	names := func(rendered string) map[string]bool {
		out := map[string]bool{}
		for _, line := range strings.Split(rendered, "\n") {
			if name, _, ok := strings.Cut(line, "{"); ok && strings.HasPrefix(name, prefix) {
				out[name] = true
			}
		}
		return out
	}

	// Against every v2 golden, not one fixture. A single capture is missing
	// whatever its host happened not to do — the first version of this test
	// compared against a tree where no container had a CPU quota and duly
	// accused cpu_limit_cores of being v1-only.
	goldens, err := filepath.Glob(filepath.Join("testdata", "golden", "container*.prom"))
	if err != nil {
		t.Fatal(err)
	}
	v2Names := map[string]bool{}
	for _, path := range goldens {
		if strings.Contains(path, "cgroupv1") {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for name := range names(string(b)) {
			v2Names[name] = true
		}
	}
	if len(v2Names) == 0 {
		t.Fatal("no v2 golden files found; this test would pass vacuously")
	}

	v1Names := names(v1Collect(t))
	if len(v1Names) == 0 {
		t.Fatal("v1 emitted no families at all")
	}
	for name := range v1Names {
		if !v2Names[name] {
			t.Errorf("v1 emits %s, which no v2 golden contains", name)
		}
	}
}

func TestV1Golden(t *testing.T) {
	got := v1Collect(t)
	path := filepath.Join("testdata", "golden", "container-cgroupv1.prom")

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

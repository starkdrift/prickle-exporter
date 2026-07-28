// SPDX-License-Identifier: Apache-2.0

package container

import (
	"bufio"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/golden/*.prom")

// fixtureDir is the captured tree every parser test reads through.
const fixtureDir = "testdata/h200-ubuntu2204-20260726"

// Counts of what the capture contains, asserted rather than assumed: a fixture
// silently losing half its tree would otherwise make every test below pass on
// less than it claims to cover.
const (
	fixtureContainers = 16
	fixtureDocker     = 3
	fixtureKubernetes = 13
	fixturePods       = 6
)

// newFixtureSet returns an empty Set with the constant label the golden file
// and every assertion below are written against.
func newFixtureSet() *exposition.Set {
	return exposition.NewSet(exposition.L("node", "fixture"))
}

// collectFixture runs the collector over the fixture tree and returns the
// rendered document. A collector error fails the test: every file in the tree
// is present and well-formed, so any error is a parser bug.
func collectFixture(t *testing.T, mutate ...func(*Options)) string {
	t.Helper()
	opts := Options{Roots: fsroot.At(fixtureDir)}
	for _, m := range mutate {
		m(&opts)
	}

	set := newFixtureSet()
	if err := New(opts).Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := set.Err(); err != nil {
		t.Fatalf("exposition problems: %v", err)
	}
	return set.String()
}

// TestGolden pins the entire Phase 2 output. It is the review surface for every
// metric name, label, unit and value in the phase — a diff here means a metric
// contract change, and should be read as one.
func TestGolden(t *testing.T) {
	got := collectFixture(t)
	path := filepath.Join("testdata", "golden", "container.prom")

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
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

// TestDiscoversEveryContainer checks the walk against the capture's own shape,
// and that it found containers from both runtimes in the tree.
func TestDiscoversEveryContainer(t *testing.T) {
	found, err := New(Options{Roots: fsroot.At(fixtureDir)}).discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != fixtureContainers {
		t.Errorf("discovered %d containers, want %d", len(found), fixtureContainers)
	}

	runtimes := map[string]int{}
	pods := map[string]bool{}
	for _, cg := range found {
		runtimes[cg.runtime]++
		if cg.pod != "" {
			pods[cg.pod] = true
		}
	}
	if runtimes["docker"] != fixtureDocker {
		t.Errorf("docker containers = %d, want %d", runtimes["docker"], fixtureDocker)
	}
	if runtimes["containerd"] != fixtureKubernetes {
		t.Errorf("containerd containers = %d, want %d", runtimes["containerd"], fixtureKubernetes)
	}
	if len(pods) != fixturePods {
		t.Errorf("distinct pods = %d, want %d", len(pods), fixturePods)
	}
}

// TestPodSlicesAreNotEmitted is the double-counting guard from the package
// comment: only leaf container cgroups produce series, so
// sum(prickle_container_memory_usage_bytes) is the node's containers once, not
// once per level of the kubepods hierarchy.
func TestPodSlicesAreNotEmitted(t *testing.T) {
	out := collectFixture(t)
	if n := countMatching(out, "prickle_container_memory_usage_bytes{"); n != fixtureContainers {
		t.Errorf("memory_usage_bytes series = %d, want %d (one per container leaf)", n, fixtureContainers)
	}
	// A pod, QoS or root slice would have to appear without a container label.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "prickle_container_") && !strings.Contains(line, `container="`) &&
			!strings.HasPrefix(line, "#") {
			t.Errorf("series with no container label: %s", line)
		}
	}
}

// TestNoPIDAnywhere enforces SPEC.md §Metrics contract. cgroup.procs is the one
// file in a container's cgroup that holds PIDs, and this package never reads
// it — the check is that no code path started to.
func TestNoPIDAnywhere(t *testing.T) {
	out := collectFixture(t)
	for _, label := range []string{"pid=", "PID=", "process_id=", "cgroup_procs"} {
		if strings.Contains(out, label) {
			t.Errorf("output contains %q", label)
		}
	}

	// cgroup.procs is the one file in a container's cgroup that holds PIDs. The
	// package comment says it is deliberately never read; this is what makes
	// that a check rather than a promise. A filepath.Join to it would appear in
	// the source as a quoted string.
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range sources {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		body, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), `"cgroup.procs"`) {
			t.Errorf("%s reads cgroup.procs; SPEC.md forbids PIDs everywhere", src)
		}
	}
}

// TestIdentityLabelsAreClosed enforces SPEC.md §Metrics contract: the hot
// series carry only labels from the closed identity set plus the dimensional
// labels this collector declares. Descriptive attributes belong on _info.
func TestIdentityLabelsAreClosed(t *testing.T) {
	allowed := map[string]bool{
		// Identity (SPEC.md §Metrics contract).
		"node": true, "container": true, "pod": true,
		// Dimensional, declared in this package.
		"mode": true, "device": true, "resource": true, "kind": true,
	}

	for _, line := range strings.Split(collectFixture(t), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels := splitSeries(line)
		if name == "prickle_container_info" {
			continue // The _info gauge is where descriptive attributes live.
		}
		for _, l := range labels {
			if !allowed[l] {
				t.Errorf("%s carries the non-identity label %q", name, l)
			}
		}
	}
}

// TestInfoCarriesTheDescriptiveAttributes checks the companion-gauge half of
// the same rule, and that QoS survives the walk up to the pod slice.
func TestInfoCarriesTheDescriptiveAttributes(t *testing.T) {
	out := collectFixture(t)
	if n := countMatching(out, "prickle_container_info{"); n != fixtureContainers {
		t.Errorf("info series = %d, want %d", n, fixtureContainers)
	}
	for _, want := range []string{
		`runtime="docker"`,
		`runtime="containerd"`,
		`qos="besteffort"`,
		`qos="burstable"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no info series carries %s", want)
		}
	}
}

// TestUnlimitedIsAbsentNotSentinel checks that "max" produces no sample. A
// limit family that reported the kernel's sentinel would put a 9.2-exabyte
// memory limit on every unconstrained container and break every
// usage/limit ratio on a dashboard.
func TestUnlimitedIsAbsentNotSentinel(t *testing.T) {
	out := collectFixture(t)

	// One container in the capture has a real memory limit; the rest are "max".
	if n := countMatching(out, "prickle_container_memory_limit_bytes{"); n != 1 {
		t.Errorf("memory_limit_bytes series = %d, want 1 (only one limited container in the capture)", n)
	}
	if !strings.Contains(out, "prickle_container_memory_limit_bytes{node=\"fixture\",container=\"9973c4b071d3315ccfff4511c511e01756e000000fbf887c420d63d213faa359\",pod=\"6eb5044d-ef2e-49d1-a9cc-28f4e3fe88a3\"} 178257920") {
		t.Error("the one limited container's memory_limit_bytes is missing or wrong")
	}
	// No container in the capture has a CPU quota: cpu.max is "max 100000".
	if strings.Contains(out, "prickle_container_cpu_limit_cores") {
		t.Error("cpu_limit_cores emitted, but no container in the capture has a quota")
	}
	// memory.min and memory.low read 0 everywhere, which is no reservation.
	for _, name := range []string{"memory_min_bytes", "memory_low_bytes", "memory_high_bytes"} {
		if strings.Contains(out, prefix+name) {
			t.Errorf("%s emitted, but the capture has it unset everywhere", name)
		}
	}
}

// TestBlockDevicesResolveToNames checks the io.stat major:minor to
// /proc/diskstats name mapping, which is what lets a container's I/O join to
// its node's disk series without a translation table in the query.
func TestBlockDevicesResolveToNames(t *testing.T) {
	out := collectFixture(t)
	if !strings.Contains(out, `device="vda"`) {
		t.Error(`no io series resolved 252:0 to device="vda"`)
	}
	if strings.Contains(out, `device="252:0"`) {
		t.Error("an io series fell back to the major:minor pair despite a readable /proc/diskstats")
	}
}

// TestNoCgroupMountIsNotAnError checks the cgroup v1 path from SPEC.md §Hard
// constraints #4: a host with no v2 mount reports nothing, quietly. `prickle
// diagnose` is where that is explained, not a failed collection on every
// scrape.
func TestNoCgroupMountIsNotAnError(t *testing.T) {
	set := exposition.NewSet()
	c := New(Options{Roots: fsroot.At(t.TempDir())})
	if err := c.Collect(context.Background(), set); err != nil {
		t.Errorf("Collect on a host with no cgroup v2 mount: %v", err)
	}
	if n := countSeries(set.String()); n != 0 {
		t.Errorf("collected %d series with no cgroup mount, want 0", n)
	}
}

// TestPartialCgroupIsPartial checks the contract from internal/collector: a
// cgroup missing a controller costs its own metrics and nothing else.
func TestPartialCgroupIsPartial(t *testing.T) {
	dir := t.TempDir()
	scope := filepath.Join(dir, "sys", "fs", "cgroup", "system.slice",
		"docker-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd.scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	// Only memory.current exists — no cpu controller, no io, no pids, no PSI.
	if err := os.WriteFile(filepath.Join(scope, "memory.current"), []byte("4096\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	set := exposition.NewSet()
	if err := New(Options{Roots: fsroot.At(dir)}).Collect(context.Background(), set); err != nil {
		t.Fatalf("a cgroup with no controllers enabled is a supported host, not an error: %v", err)
	}
	out := set.String()
	if !strings.Contains(out, "} 4096") {
		t.Errorf("the one readable file did not produce its metric:\n%s", out)
	}
	if strings.Contains(out, "cpu_usage_seconds_total") {
		t.Error("emitted CPU metrics for a cgroup with no cpu.stat")
	}
}

// TestCollectIsReadOnly is a standing check on SPEC.md §Hard constraints #2.
func TestCollectIsReadOnly(t *testing.T) {
	before := treeSnapshot(t, fixtureDir)
	collectFixture(t)
	after := treeSnapshot(t, fixtureDir)

	if before != after {
		t.Error("Collect modified the fixture tree; the exporter must never write to /proc, /sys or cgroups")
	}
}

// TestContextCancellation checks that a cancelled context stops the pass rather
// than being ignored: the sampler's per-collector deadline is what keeps a
// stalled filesystem from blocking every later collector.
func TestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	set := exposition.NewSet()
	err := New(Options{Roots: fsroot.At(fixtureDir)}).Collect(ctx, set)
	if err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
	if n := countSeries(set.String()); n != 0 {
		t.Errorf("collected %d series after cancellation, want 0", n)
	}
}

// splitSeries breaks a sample line into its metric name and label names.
func splitSeries(line string) (name string, labels []string) {
	name, rest, ok := strings.Cut(line, "{")
	if !ok {
		name, _, _ = strings.Cut(line, " ")
		return name, nil
	}
	rest, _, _ = strings.Cut(rest, "}")
	for _, pair := range strings.Split(rest, `",`) {
		if key, _, ok := strings.Cut(pair, "="); ok {
			labels = append(labels, strings.TrimSpace(key))
		}
	}
	return name, labels
}

// treeSnapshot renders a directory tree's paths, sizes and modification times
// as a comparable string.
func treeSnapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		b.WriteString(path)
		b.WriteByte(' ')
		b.WriteString(strconv.FormatInt(info.Size(), 10))
		b.WriteByte(' ')
		b.WriteString(info.ModTime().String())
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func countSeries(rendered string) int {
	var n int
	for _, line := range strings.Split(rendered, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			n++
		}
	}
	return n
}

func countMatching(rendered, prefix string) int {
	var n int
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// firstDiff reports the first differing line, with its number.
func firstDiff(want, got string) string {
	w := bufio.NewScanner(strings.NewReader(want))
	g := bufio.NewScanner(strings.NewReader(got))
	for line := 1; ; line++ {
		wOK, gOK := w.Scan(), g.Scan()
		switch {
		case !wOK && !gOK:
			return "(no line differs; trailing newline?)"
		case w.Text() != g.Text():
			return "line " + strconv.Itoa(line) + "\n want: " + w.Text() + "\n  got: " + g.Text()
		}
	}
}

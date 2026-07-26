// SPDX-License-Identifier: Apache-2.0

package host

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

// newFixtureCollector returns a collector pointed at the captured tree, with
// the Statfs fake standing in for the syscall.
func newFixtureCollector(t *testing.T, mutate ...func(*Options)) *Collector {
	t.Helper()
	opts := Options{
		Roots:  fsroot.At(fixtureDir),
		Statfs: newFakeStatfs(t),
	}
	for _, m := range mutate {
		m(&opts)
	}
	return New(opts)
}

// collectFixture runs the collector over the fixture tree and returns the
// rendered document. A collector error fails the test: every source in the
// tree is present and well-formed, so any error is a parser bug.
func collectFixture(t *testing.T, mutate ...func(*Options)) string {
	t.Helper()
	set := exposition.NewSet(exposition.L("node", "fixture"))
	if err := newFixtureCollector(t, mutate...).Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := set.Err(); err != nil {
		t.Fatalf("exposition problems: %v", err)
	}
	return set.String()
}

// TestGolden pins the entire Phase 1 output. It is the review surface for
// every metric name, label, unit and value in the phase — a diff here means a
// metric contract change, and should be read as one.
func TestGolden(t *testing.T) {
	got := collectFixture(t)
	path := filepath.Join("testdata", "golden", "host.prom")

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

// TestCollectIsReadOnly is a standing check on SPEC.md §Hard constraints #2.
// The fixture tree is made read-only for the duration of a collection, so any
// attempt to create, truncate or unlink inside it fails loudly.
func TestCollectIsReadOnly(t *testing.T) {
	before := treeSnapshot(t, fixtureDir)
	collectFixture(t)
	after := treeSnapshot(t, fixtureDir)

	if before != after {
		t.Error("Collect modified the fixture tree; the exporter must never write to /proc or /sys")
	}
}

// TestContextCancellation checks that a cancelled context stops the pass
// rather than being ignored: the sampler's per-collector deadline is what
// keeps a stalled filesystem from blocking every later collector.
func TestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	set := exposition.NewSet()
	err := newFixtureCollector(t).Collect(ctx, set)
	if err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
	if n := countSeries(set.String()); n != 0 {
		t.Errorf("collected %d series after cancellation, want 0", n)
	}
}

// TestMissingSourcesArePartial checks the partial-collection contract from
// internal/collector: an unreadable source costs its own metrics and nothing
// else. A host with no /proc/pressure still reports CPU and memory.
func TestMissingSourcesArePartial(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Only /proc/loadavg exists. Everything else is missing.
	if err := os.WriteFile(filepath.Join(dir, "proc", "loadavg"), []byte("0.50 0.25 0.10 1/788 12731\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	set := exposition.NewSet()
	c := New(Options{Roots: fsroot.At(dir), Statfs: newFakeStatfs(t)})
	if err := c.Collect(context.Background(), set); err == nil {
		t.Fatal("expected errors for the missing sources")
	}

	out := set.String()
	if !strings.Contains(out, "prickle_host_load1 0.5") {
		t.Errorf("the one readable source did not produce its metric:\n%s", out)
	}
}

// TestPerCoreCPUIsOptIn checks SPEC.md §Collectors: per-core series are a
// separate family, absent unless asked for.
func TestPerCoreCPUIsOptIn(t *testing.T) {
	off := collectFixture(t)
	if strings.Contains(off, "prickle_host_cpu_core_seconds_total") {
		t.Error("per-core series present without --collector.cpu.per-core")
	}

	on := collectFixture(t, func(o *Options) { o.PerCoreCPU = true })
	if !strings.Contains(on, "prickle_host_cpu_core_seconds_total") {
		t.Fatal("per-core series absent with PerCoreCPU set")
	}
	// 24 cores in the fixture × 10 modes.
	if n := countMatching(on, "prickle_host_cpu_core_seconds_total{"); n != 240 {
		t.Errorf("per-core series = %d, want 240 (24 cores × 10 modes)", n)
	}
	// The aggregate must not gain a cpu label or a duplicate.
	if n := countMatching(on, "prickle_host_cpu_seconds_total{"); n != 10 {
		t.Errorf("aggregate series = %d, want 10", n)
	}
}

// TestNoPIDAnywhere enforces SPEC.md §Metrics contract: a PID never appears as
// a label or a metric value. /proc/loadavg's fifth field is the most recently
// created PID and is the one place Phase 1 could leak one.
//
// The check is on exact values and label names rather than a substring sweep:
// "788" occurs inside plenty of legitimate byte counts.
func TestNoPIDAnywhere(t *testing.T) {
	out := collectFixture(t)

	// The fixture's /proc/loadavg is "0.07 0.05 0.00 1/788 12731". Neither the
	// runnable/total pair nor the last PID may appear as a value.
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		_, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if !strings.HasPrefix(line, "prickle_host_load") {
			continue
		}
		switch value {
		case "12731", "788", "1":
			t.Errorf("load metric carries a /proc/loadavg field 4-5 value: %s", line)
		}
	}

	for _, label := range []string{"pid=", "PID=", "process_id="} {
		if strings.Contains(out, label) {
			t.Errorf("output contains a %q label", label)
		}
	}
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

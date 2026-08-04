// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/golden/*.prom")

// fixtureDir is the captured NVIDIA output the tests in this file read
// through: an H200 with MIG enabled. The second capture, an H100 in Default
// mode, drives smi_default_mode_test.go through the same runner.
const fixtureDir = "testdata/h200-mig-20260726"

// Facts about the capture, asserted rather than assumed.
const (
	fixtureGPUUUID = "GPU-2af1c335-11fb-c9d3-4de3-27a25697fc35"
	fixtureMIG0    = "MIG-30366fdf-6105-5648-968d-679e250aa830"
	fixtureMIG1    = "MIG-7138b2c5-cb05-5700-b65a-5fdec910f0f4"
	fixturePID     = "12648" // present in the captured CSV, forbidden in output
)

// fixtureRunner replays captured nvidia-smi output.
//
// A subprocess is not a file, so it cannot be pointed at a fixture tree the way
// fsroot points the host and container collectors at theirs (SPEC.md
// §Collectors puts it behind an interface for the same reason as Statfser).
// This is that interface, replaying the exact output the captured host gave for
// the exact queries smi.go issues.
type fixtureRunner struct {
	t *testing.T

	// dir is the capture being replayed. There is more than one, and which
	// host a test runs against is part of what it asserts.
	dir string

	// calls records what was asked for, so a test can assert on the queries
	// themselves — the field set is part of what the fixture pins.
	calls [][]string

	// fail makes the named query return an error, for the partial-read tests.
	fail map[string]error

	// override replaces one query's output.
	override map[string]string
}

func newFixtureRunner(t *testing.T) *fixtureRunner {
	t.Helper()
	return newFixtureRunnerAt(t, fixtureDir)
}

// newFixtureRunnerAt replays a named capture.
func newFixtureRunnerAt(t *testing.T, dir string) *fixtureRunner {
	t.Helper()
	return &fixtureRunner{t: t, dir: dir, fail: map[string]error{}, override: map[string]string{}}
}

// queryKind names the three calls smi.go makes.
func queryKind(args []string) string {
	for _, a := range args {
		switch {
		case a == "-L":
			return "-L"
		case strings.HasPrefix(a, "--query-gpu="):
			return "--query-gpu"
		case strings.HasPrefix(a, "--query-compute-apps="):
			return "--query-compute-apps"
		}
	}
	return strings.Join(args, " ")
}

func (r *fixtureRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, args)
	kind := queryKind(args)

	if err, ok := r.fail[kind]; ok {
		return nil, err
	}
	if body, ok := r.override[kind]; ok {
		return []byte(body), nil
	}

	var name string
	switch kind {
	case "--query-gpu":
		name = "query-gpu.csv"
	case "-L":
		name = "gpus.txt"
	case "--query-compute-apps":
		name = "query-compute-apps.csv"
	default:
		return nil, fmt.Errorf("fixtureRunner: unexpected query %q", kind)
	}

	body, err := os.ReadFile(filepath.Join(r.dir, "nvidia", name))
	if err != nil {
		r.t.Fatal(err)
	}
	return body, nil
}

// hermeticRoots points a collector at an empty tree, so it reads the fixture
// under test and nothing else.
//
// Every nvidia-smi fixture test needs this, and needs it for a reason that is
// not tidiness. The zero fsroot.Roots resolves to the live /sys and /proc, and
// the AMD reader takes its cards from there — so on a host that actually has an
// AMD GPU, these NVIDIA tests collected it. The real cards joined the golden
// comparison and the real GPU processes joined the per-command summing, which
// is how TestSameCommandIsOneSummedSeries came to count four series for a
// fixture describing two.
//
// It was invisible everywhere it had ever been run. No CI runner has a GPU, and
// no developer machine here has an AMD one; it surfaced the first time the suite
// was run on the 2× MI300X capture host. An empty tree makes these tests
// describe their fixture whatever the machine underneath them has plugged in.
func hermeticRoots(t *testing.T) fsroot.Roots {
	t.Helper()
	return fsroot.At(t.TempDir())
}

// newFixtureCollector returns a collector wired to the captured output.
func newFixtureCollector(t *testing.T, mutate ...func(*Options)) (*Collector, *fixtureRunner) {
	t.Helper()
	runner := newFixtureRunner(t)
	opts := Options{
		NVIDIASource: SourceSMI,
		runner:       runner,
		Roots:        hermeticRoots(t),
	}
	for _, m := range mutate {
		m(&opts)
	}
	return New(opts), runner
}

// collectFixture renders one pass over the captured output.
func collectFixture(t *testing.T, mutate ...func(*Options)) string {
	t.Helper()
	c, _ := newFixtureCollector(t, mutate...)

	set := exposition.NewSet(exposition.L("node", "fixture"))
	if err := c.Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := set.Err(); err != nil {
		t.Fatalf("exposition problems: %v", err)
	}
	return set.String()
}

// TestGolden pins the entire Phase 3 nvidia-smi output. A diff here is a
// metric contract change and should be read as one.
func TestGolden(t *testing.T) {
	got := collectFixture(t, func(o *Options) { o.PerProcess = true })
	path := filepath.Join("testdata", "golden", "gpu.prom")

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

// TestUtilizationIsAbsentUnderMIG covers the first of the two limitations
// SPEC.md §Collectors records for this source, verified on H200 / driver 580:
// utilization.gpu returns [N/A] once MIG is enabled, and a bracketed token is
// an absent value, never an error.
//
// A zero here would be worse than nothing: it reads as an idle GPU, and an
// idle-GPU alert firing across a fleet of MIG nodes is exactly the failure this
// prevents.
func TestUtilizationIsAbsentUnderMIG(t *testing.T) {
	out := collectFixture(t)

	if strings.Contains(out, prefix+"utilization_ratio") {
		t.Error("utilization_ratio emitted, but the captured card reports [N/A] under MIG")
	}
	// The rest of the row parsed, so [N/A] cost its own field and nothing else.
	for _, want := range []string{
		prefix + `memory_used_bytes{node="fixture",gpu_uuid="` + fixtureGPUUUID + `"} 670040064`,
		prefix + `temperature_celsius{node="fixture",gpu_uuid="` + fixtureGPUUUID + `"} 38`,
		prefix + `power_watts{node="fixture",gpu_uuid="` + fixtureGPUUUID + `"} 123.31`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("[N/A] in one column cost another: missing %s", want)
		}
	}
}

// TestBracketedTokensAreAbsentNotErrors generalises that rule. The driver also
// emits [Not Supported] and [Unknown Error] in the same position, and a parser
// that knows only [N/A] fails on a card it has never seen.
func TestBracketedTokensAreAbsentNotErrors(t *testing.T) {
	c, runner := newFixtureCollector(t)
	runner.override["--query-gpu"] =
		"0, " + fixtureGPUUUID + ", NVIDIA H200, [Not Supported], 639, 143771, [Unknown Error], [N/A]\n"

	set := exposition.NewSet()
	if err := c.Collect(context.Background(), set); err != nil {
		t.Fatalf("bracketed tokens must not be errors: %v", err)
	}
	out := set.String()

	for _, absent := range []string{"utilization_ratio", "temperature_celsius", "power_watts"} {
		if strings.Contains(out, prefix+absent) {
			t.Errorf("%s emitted for a bracketed token", absent)
		}
	}
	if !strings.Contains(out, prefix+"memory_used_bytes") {
		t.Error("the columns that did parse were dropped alongside the ones that did not")
	}
}

// TestMIGProcessIsAttributedToTheParent covers the second recorded limitation:
// --query-compute-apps reports the *parent* GPU UUID for a MIG-resident
// process, so this source cannot do per-process MIG attribution.
//
// The captured process runs on MIG device 0, and the CSV names the physical
// card. Attributing it there is correct-but-coarse; inventing a mig_uuid for it
// would be wrong.
func TestMIGProcessIsAttributedToTheParent(t *testing.T) {
	out := collectFixture(t, func(o *Options) { o.PerProcess = true })

	want := prefix + `process_memory_bytes{node="fixture",gpu_uuid="` + fixtureGPUUUID + `",command="prickle-gpu-spin",container=""} 631242752`
	if !strings.Contains(out, want) {
		t.Errorf("missing %s in:\n%s", want, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix+"process_memory_bytes") && strings.Contains(line, "mig_uuid=") {
			t.Errorf("this source cannot attribute a process to a MIG instance, but did: %s", line)
		}
	}
}

// TestNoPIDAnywhere enforces SPEC.md §Metrics contract. The captured CSV
// contains PID 12648 in a column this code parses, which makes it the one
// place in Phase 3 a PID could escape.
func TestNoPIDAnywhere(t *testing.T) {
	out := collectFixture(t, func(o *Options) { o.PerProcess = true })

	if strings.Contains(out, fixturePID) {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, fixturePID) {
				t.Errorf("the captured PID reached the output: %s", line)
			}
		}
	}
	for _, label := range []string{"pid=", "PID=", "process_id="} {
		if strings.Contains(out, label) {
			t.Errorf("output contains a %q label", label)
		}
	}

	// Structural, not incidental: there is nowhere in the data model to put a
	// PID, so no future edit can route one through without adding a field.
	for _, field := range []string{"PID", "Pid"} {
		if strings.Contains(fmt.Sprintf("%+v", process{}), field) {
			t.Errorf("the process struct has a %s field", field)
		}
	}
}

// TestCommandIsExeBasename checks the label's provenance. SPEC.md §Metrics
// contract: the basename of the executable path, never comm, which the kernel
// truncates to 15 characters — the captured host's comm for this process is
// "prickle-gpu-spi", one character short of the real name.
func TestCommandIsExeBasename(t *testing.T) {
	out := collectFixture(t, func(o *Options) { o.PerProcess = true })

	if !strings.Contains(out, `command="prickle-gpu-spin"`) {
		t.Error(`missing command="prickle-gpu-spin"`)
	}
	if strings.Contains(out, "prickle-gpu-spi\"") {
		t.Error("the label carries the truncated comm, not the exe basename")
	}
	if strings.Contains(out, `command="/tmp/`) {
		t.Error("the label carries a full path rather than a basename")
	}
}

// TestPerProcessIsOptIn checks SPEC.md §Metrics contract: per-process
// attribution is off unless asked for. It is one series per distinct command
// per GPU, which on a shared node is unbounded in a way the device series are
// not.
func TestPerProcessIsOptIn(t *testing.T) {
	if off := collectFixture(t); strings.Contains(off, prefix+"process_memory_bytes") {
		t.Error("per-process series present without -collector.gpu.per-process")
	}
	if on := collectFixture(t, func(o *Options) { o.PerProcess = true }); !strings.Contains(on, prefix+"process_memory_bytes") {
		t.Error("per-process series absent with PerProcess set")
	}
}

// TestMIGTopology checks what this source can say about a partitioned card:
// both instances, by UUID and profile, and no per-instance memory, which no
// CSV query publishes.
func TestMIGTopology(t *testing.T) {
	out := collectFixture(t)

	if !strings.Contains(out, prefix+`mig_enabled{node="fixture",gpu_uuid="`+fixtureGPUUUID+`"} 1`) {
		t.Error("mig_enabled is not 1 for the captured partitioned card")
	}
	for i, uuid := range []string{fixtureMIG0, fixtureMIG1} {
		want := fmt.Sprintf(`mig_uuid="%s",profile="1g.18gb",device_index="%d"`, uuid, i)
		if !strings.Contains(out, want) {
			t.Errorf("missing MIG instance %s", want)
		}
	}
	for _, absent := range []string{"mig_memory_used_bytes", "mig_memory_total_bytes", "mig_utilization_ratio"} {
		if strings.Contains(out, prefix+absent) {
			t.Errorf("%s emitted, but no nvidia-smi CSV query publishes it", absent)
		}
	}
}

// TestMIGAndDeviceMemoryAreSeparateFamilies is the double-counting guard. A MIG
// instance's memory is a partition of its parent's, so one family holding both
// would make a sum over it count the same bytes twice.
func TestMIGAndDeviceMemoryAreSeparateFamilies(t *testing.T) {
	out := collectFixture(t)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix+"memory_used_bytes") && strings.Contains(line, "mig_uuid=") {
			t.Errorf("a MIG instance appears in the device memory family: %s", line)
		}
	}
}

// TestDefaultModeCardHasNoMIG checks the other branch: a card that is not
// partitioned reports mig_enabled 0 and no instances. `nvidia-smi -L` is what
// decides, because the captured --query-gpu field set has no mig.mode column.
func TestDefaultModeCardHasNoMIG(t *testing.T) {
	c, runner := newFixtureCollector(t)
	runner.override["-L"] = "GPU 0: NVIDIA H200 (UUID: " + fixtureGPUUUID + ")\n"

	set := exposition.NewSet()
	if err := c.Collect(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	out := set.String()

	if !strings.Contains(out, prefix+`mig_enabled{gpu_uuid="`+fixtureGPUUUID+`"} 0`) {
		t.Errorf("mig_enabled is not 0 for an unpartitioned card:\n%s", out)
	}
	if strings.Contains(out, prefix+"mig_info") {
		t.Error("mig_info emitted for a card with no MIG devices in -L")
	}
}

// TestSourceIsRecorded checks SPEC.md §Collectors: the live source is recorded
// on an _info gauge, so a scrape says which implementation produced it.
func TestSourceIsRecorded(t *testing.T) {
	out := collectFixture(t)
	if !strings.Contains(out, prefix+`nvidia_source_info{node="fixture",source="smi"} 1`) {
		t.Errorf("the live source is not recorded:\n%s", out)
	}
}

// TestQueriesMatchTheCapture pins the field sets. The fixture is only a record
// of the format if the code asks the captured host's question; a changed field
// set would leave it describing output nothing reads (SPEC.md §Testing rules).
func TestQueriesMatchTheCapture(t *testing.T) {
	c, runner := newFixtureCollector(t)
	if err := c.Collect(context.Background(), exposition.NewSet()); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"--query-gpu=" + queryGPUFields: false,
		"-L":                            false,
		"--query-compute-apps=" + queryProcessFields: false,
	}
	for _, call := range runner.calls {
		for _, arg := range call {
			if _, ok := want[arg]; ok {
				want[arg] = true
			}
		}
	}
	for query, seen := range want {
		if !seen {
			t.Errorf("the collector never issued %q; the capture no longer describes what it reads", query)
		}
	}
}

// TestPartialReadStillRenders checks the partial-collection contract: a failing
// MIG or process query costs its own metrics and leaves the device series
// standing, because a host whose driver refuses one query still has memory and
// power worth reporting.
func TestPartialReadStillRenders(t *testing.T) {
	for _, failing := range []string{"-L", "--query-compute-apps"} {
		t.Run(failing, func(t *testing.T) {
			c, runner := newFixtureCollector(t, func(o *Options) { o.PerProcess = true })
			runner.fail[failing] = fmt.Errorf("simulated failure")

			set := exposition.NewSet()
			if err := c.Collect(context.Background(), set); err == nil {
				t.Error("expected the failure to be reported, so prickle_collector_errors_total rises")
			}
			if out := set.String(); !strings.Contains(out, prefix+"memory_used_bytes") {
				t.Errorf("a failing %s query cost the device metrics:\n%s", failing, out)
			}
		})
	}
}

// TestNoDeviceQueryIsFatal is the other side: without the device list there is
// nothing to attach anything to, so that one failure ends the pass.
func TestNoDeviceQueryIsFatal(t *testing.T) {
	c, runner := newFixtureCollector(t)
	runner.fail["--query-gpu"] = fmt.Errorf("nvidia-smi: no devices were found")

	set := exposition.NewSet()
	if err := c.Collect(context.Background(), set); err == nil {
		t.Fatal("expected an error when the device query fails")
	}
	// The source gauge still renders: knowing which source failed is the point
	// of recording it.
	if n := countMatching(set.String(), prefix+"nvidia_source_info"); n != 1 {
		t.Error("the source gauge should still say which implementation was live")
	}
}

// TestNoGPUIsNotAnError checks the common case on the nodes this exporter also
// watches for host and container metrics: no GPU hardware at all, which must
// produce no samples and no error.
//
// "No GPU" now means neither vendor, so the empty tree is what makes the case
// real rather than incidental — without it the AMD reader walks the live /sys
// and the test asserts the absence of a card on whatever host is running it.
func TestNoGPUIsNotAnError(t *testing.T) {
	c := New(Options{
		NVIDIASource: SourceSMI,
		SMICommand:   "prickle-nvidia-smi-that-does-not-exist",
		Roots:        hermeticRoots(t),
	})

	if c.SourceName() != "" {
		t.Errorf("a source loaded without nvidia-smi present: %q", c.SourceName())
	}
	if c.SelectionError() == nil {
		t.Error("the selection error should say why, for prickle diagnose")
	}

	set := exposition.NewSet()
	if err := c.Collect(context.Background(), set); err != nil {
		t.Errorf("a host with no GPU is not an error: %v", err)
	}
	if n := countSeries(set.String()); n != 0 {
		t.Errorf("collected %d series with no GPU, want 0", n)
	}
}

// TestUnknownSourceIsRejected checks the flag's validation.
func TestUnknownSourceIsRejected(t *testing.T) {
	c := New(Options{NVIDIASource: "cuda"})
	if c.SelectionError() == nil {
		t.Fatal("expected an error for an unknown source")
	}
	if !strings.Contains(c.SelectionError().Error(), "unknown NVIDIA source") {
		t.Errorf("unhelpful error: %v", c.SelectionError())
	}
}

// TestNVMLIsAbsentFromTheDefaultBuild checks SPEC.md §Distribution: the static
// artifact cannot dlopen, so it must not claim NVML. Forcing the source is how
// an operator finds out they have the wrong binary.
func TestNVMLIsAbsentFromTheDefaultBuild(t *testing.T) {
	if NVMLBuilt {
		t.Skip("built with -tags nvml; this asserts the default build's behaviour")
	}
	c := New(Options{NVIDIASource: SourceNVML})
	err := c.SelectionError()
	if err == nil {
		t.Fatal("the default build must not provide an NVML source")
	}
	if !strings.Contains(err.Error(), "nvml tag") {
		t.Errorf("the error should name the missing build tag: %v", err)
	}
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

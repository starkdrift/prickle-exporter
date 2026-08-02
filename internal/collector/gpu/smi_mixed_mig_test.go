// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// mixedMIGDir is the fourth capture: the same H100 again, partitioned three
// ways — one `3g.40gb` GPU instance subdivided into a single `1c` compute
// instance, and two whole `1g.10gb` instances — with three compute processes
// resident, two of which run the same binary and one of which has had its
// binary deleted out from under it.
//
// Every one of those is a parser behaviour nothing else in testdata/ pins:
//
//   - `nvidia-smi -L` spells a subdivided instance `1c.3g.40gb`. Both other MIG
//     captures are `Ng.Mgb`, so nothing proved the -L parser passes a profile
//     through rather than expecting that shape. It is also the spelling that
//     caught the NVML source assembling profile names from too few fields.
//   - Three instances rather than two, with *different* profiles on one card.
//     Two captures of two identical instances cannot show a parser attaching
//     the wrong profile to the wrong UUID.
//   - Two processes running one command, which must land on one summed series.
//   - A process whose binary was deleted while it ran, which is where the
//     kernel starts appending " (deleted)" to the exe link.
const mixedMIGDir = "testdata/h100-mig-mixed-20260729"

// Facts about that capture.
const (
	mixedMIGCompute = "MIG-5ff0d535-9a82-5af6-80df-768b9d3ac092" // the 1c.3g.40gb device
	mixedMIG1       = "MIG-8443519f-66f7-5962-8955-60dba0866f3a"
	mixedMIG2       = "MIG-ee6996fb-86fb-5405-83f0-b20887da1bb3"

	// 4188 MiB, the memory each of the three processes held.
	mixedProcessBytes = 4188 << 20
)

// collectMixedMIG renders one pass over the mixed-profile capture.
func collectMixedMIG(t *testing.T, mutate ...func(*Options)) string {
	t.Helper()

	opts := Options{
		NVIDIASource: SourceSMI,
		runner:       newFixtureRunnerAt(t, mixedMIGDir),
	}
	for _, m := range mutate {
		m(&opts)
	}

	set := exposition.NewSet(exposition.L("node", "fixture"))
	if err := New(opts).Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := set.Err(); err != nil {
		t.Fatalf("exposition problems: %v", err)
	}
	return set.String()
}

// TestMixedMIGGolden pins the fourth capture's rendering.
func TestMixedMIGGolden(t *testing.T) {
	got := collectMixedMIG(t, func(o *Options) { o.PerProcess = true })
	path := filepath.Join("testdata", "golden", "gpu-h100-mig-mixed.prom")

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

// TestComputeInstanceProfileIsPassedThrough checks the `1c.3g.40gb` spelling.
//
// A compute instance smaller than its GPU instance is a supported MIG layout,
// and `nvidia-smi -L` names it with a leading slice count the other captures
// never produce. The label is whatever the driver printed; this source's job is
// not to understand it.
func TestComputeInstanceProfileIsPassedThrough(t *testing.T) {
	out := collectMixedMIG(t)

	want := `mig_uuid="` + mixedMIGCompute + `",profile="1c.3g.40gb",device_index="0"`
	if !strings.Contains(out, want) {
		t.Errorf("missing %s in:\n%s", want, out)
	}
	// The plain spelling of the same instance would mean the "1c." was parsed
	// off, which is what the NVML source did before it was taught otherwise.
	if strings.Contains(out, `profile="3g.40gb"`) {
		t.Error(`the compute-instance prefix was dropped: profile="3g.40gb"`)
	}
}

// TestProfilesFollowTheirOwnInstances checks that three instances with two
// different profiles on one card keep them straight. Two captures of two
// identical instances cannot catch a parser that pairs profiles with UUIDs
// positionally and gets away with it.
func TestProfilesFollowTheirOwnInstances(t *testing.T) {
	out := collectMixedMIG(t)

	for _, want := range []string{
		`mig_uuid="` + mixedMIGCompute + `",profile="1c.3g.40gb",device_index="0"`,
		`mig_uuid="` + mixedMIG1 + `",profile="1g.10gb",device_index="1"`,
		`mig_uuid="` + mixedMIG2 + `",profile="1g.10gb",device_index="2"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	if n := countMatching(out, prefix+"mig_info{"); n != 3 {
		t.Errorf("got %d MIG instances, want 3", n)
	}
}

// TestSameCommandIsOneSummedSeries checks what the help text promises:
// "GPU memory held by processes running one command, summed". Two processes
// ran the same binary here, and per-command keying is the whole reason the
// series does not carry a PID (SPEC.md §Metrics contract) — so two processes
// must collapse to one series carrying both, not to one that silently drops
// the second.
func TestSameCommandIsOneSummedSeries(t *testing.T) {
	out := collectMixedMIG(t, func(o *Options) { o.PerProcess = true })

	if n := countMatching(out, prefix+"process_memory_bytes"); n != 2 {
		t.Errorf("got %d process series for three processes running two commands, want 2", n)
	}
	summed := prefix + `process_memory_bytes{node="fixture",gpu_uuid="` + h100UUID +
		`",command="loadgen",container=""} 8782872576`
	if !strings.Contains(out, summed) {
		t.Errorf("the two loadgen processes were not summed; want:\n  %s\ngot:\n%s", summed, out)
	}
}

// TestDeletedBinaryKeepsItsName covers the process whose binary was removed
// while it ran. The kernel appends " (deleted)" to /proc/<pid>/exe from that
// moment, and the NVML source strips it so a redeployed workload does not fork
// its series in two. This source reads the name nvidia-smi reports instead, so
// the fixture is what shows the two agree: one `ghost`, no suffix, no path.
func TestDeletedBinaryKeepsItsName(t *testing.T) {
	out := collectMixedMIG(t, func(o *Options) { o.PerProcess = true })

	want := prefix + `process_memory_bytes{node="fixture",gpu_uuid="` + h100UUID +
		`",command="ghost",container=""} 4391436288`
	if !strings.Contains(out, want) {
		t.Errorf("missing %s in:\n%s", want, out)
	}
	for _, wrong := range []string{"deleted", `command="/tmp/`} {
		if strings.Contains(out, wrong) {
			t.Errorf("output contains %q", wrong)
		}
	}
}

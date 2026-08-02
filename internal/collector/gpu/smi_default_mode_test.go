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

// defaultModeDir is the second capture: an H100 in Default mode — MIG off —
// under a real CUDA load.
//
// It exists because the first capture could not answer what an unpartitioned
// card looks like. The H200 rental was in MIG mode for its whole life, so
// TestDefaultModeCardHasNoMIG had to hand-write an `nvidia-smi -L` line, and a
// hand-written line proves only that the parser handles what the test author
// imagined. This tree is the real thing.
const defaultModeDir = "testdata/h100-default-20260729"

// Facts about the Default-mode capture, asserted rather than assumed.
const (
	h100UUID = "GPU-0d0831da-0c98-2717-6446-8acd91d39b65"
	h100PID  = "4559" // present in the captured CSV, forbidden in output
)

// collectDefaultMode renders one pass over the H100 capture.
func collectDefaultMode(t *testing.T, mutate ...func(*Options)) string {
	t.Helper()

	opts := Options{
		NVIDIASource: SourceSMI,
		runner:       newFixtureRunnerAt(t, defaultModeDir),
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

// TestDefaultModeGolden pins the whole Default-mode rendering, the way
// TestGolden pins the MIG one. Two goldens rather than one because the two
// captures differ in what the driver will answer, not merely in their numbers.
func TestDefaultModeGolden(t *testing.T) {
	got := collectDefaultMode(t, func(o *Options) { o.PerProcess = true })
	path := filepath.Join("testdata", "golden", "gpu-default-mode.prom")

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

// TestDefaultModeUtilizationIsANumber is the other half of
// TestUtilizationIsAbsentUnderMIG, and the reason this capture was worth a
// second rental.
//
// The MIG capture pins that a bracketed [N/A] must not become a number. It
// cannot pin the converse — that a real reading is *not* dropped — because
// that card never produced one. Nor can it distinguish "absent" from "zero":
// the failure mode the other test guards against, defaulting [N/A] to 0, would
// have looked identical against a fixture captured on an idle card.
//
// This card was pinned at 100% by a CUDA kernel when it was captured, so the
// value is a number, and it is neither absent nor zero.
func TestDefaultModeUtilizationIsANumber(t *testing.T) {
	out := collectDefaultMode(t)

	want := prefix + `utilization_ratio{node="fixture",gpu_uuid="` + h100UUID + `"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("missing %s in:\n%s", want, out)
	}
}

// TestDefaultModeCardReportsNoInstances is TestDefaultModeCardHasNoMIG against
// a real capture instead of a hand-written -L line: a MIG-capable H100 that is
// simply not partitioned reports mig_enabled 0 and no instances at all.
func TestDefaultModeCardReportsNoInstances(t *testing.T) {
	out := collectDefaultMode(t)

	if !strings.Contains(out, prefix+`mig_enabled{node="fixture",gpu_uuid="`+h100UUID+`"} 0`) {
		t.Errorf("mig_enabled is not 0 for the captured unpartitioned card:\n%s", out)
	}
	for _, absent := range []string{"mig_info", "mig_memory_used_bytes", "mig_memory_total_bytes"} {
		if strings.Contains(out, prefix+absent) {
			t.Errorf("%s emitted for a card with no MIG devices", absent)
		}
	}
	if strings.Contains(out, "mig_uuid=") {
		t.Error("a mig_uuid label on an unpartitioned card")
	}
}

// TestDefaultModeProcessIsAttributedToTheCard checks that a compute process on
// an unpartitioned card lands on the card — the same code path that must
// resist inventing a mig_uuid under MIG.
func TestDefaultModeProcessIsAttributedToTheCard(t *testing.T) {
	out := collectDefaultMode(t, func(o *Options) { o.PerProcess = true })

	want := prefix + `process_memory_bytes{node="fixture",gpu_uuid="` + h100UUID + `",command="loadgen",container=""} 4842323968`
	if !strings.Contains(out, want) {
		t.Errorf("missing %s in:\n%s", want, out)
	}
}

// TestDefaultModeHasNoPID is TestNoPIDAnywhere against the second capture. The
// rule is per-output, not per-fixture: a PID this capture carries must not
// reach the output either.
func TestDefaultModeHasNoPID(t *testing.T) {
	out := collectDefaultMode(t, func(o *Options) { o.PerProcess = true })

	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, h100PID) {
			t.Errorf("the captured PID reached the output: %s", line)
		}
	}
}

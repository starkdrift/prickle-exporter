// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// secondMIGDir is the third capture: the same H100 as
// h100-default-20260729, partitioned into two `1g.10gb` instances for the
// hardware verification run and captured before it was torn down.
//
// It is here because the numbers in testdata/README.md §Hardware verification
// are read off it. A README that cites a capture nobody kept is prose, and the
// claims it makes cannot be rechecked — which matters, because rechecking this
// one is what found the ordering claim it originally made to be wrong.
//
// It also differs from the H200 tree in the two ways that could hide a parser
// assumption: a different profile string, and a partition count that is not
// the same as the H200's by coincidence of both being two.
const secondMIGDir = "testdata/h100-mig-20260729"

// Facts about that capture.
const (
	h100MIG0 = "MIG-8443519f-66f7-5962-8955-60dba0866f3a" // -L device 0, GI 11
	h100MIG1 = "MIG-ee6996fb-86fb-5405-83f0-b20887da1bb3" // -L device 1, GI 13
)

// collectSecondMIG renders one pass over the H100 MIG capture.
func collectSecondMIG(t *testing.T, mutate ...func(*Options)) string {
	t.Helper()

	opts := Options{
		NVIDIASource: SourceSMI,
		runner:       newFixtureRunnerAt(t, secondMIGDir),
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

// TestSecondMIGGolden pins the third capture's rendering.
func TestSecondMIGGolden(t *testing.T) {
	got := collectSecondMIG(t, func(o *Options) { o.PerProcess = true })
	path := filepath.Join("testdata", "golden", "gpu-h100-mig.prom")

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

// TestSecondMIGProfileIsNotTheFirstCapturesProfile checks that the profile
// label is read from the capture rather than settled on by a parser that has
// only ever seen one card. The H200 tree's instances are all `1g.18gb`; this
// card's are `1g.10gb`, on a different GPU with a different memory size.
//
// The same string is what the NVML source had to be taught to spell — it said
// `10gb` until nvmlDeviceGetAttributes_v2 supplied the slice count — so this
// is also the fixture side of TestMIGProfileSpellingAgrees.
func TestSecondMIGProfileIsNotTheFirstCapturesProfile(t *testing.T) {
	out := collectSecondMIG(t)

	for i, uuid := range []string{h100MIG0, h100MIG1} {
		want := fmt.Sprintf(`mig_uuid="%s",profile="1g.10gb",device_index="%d"`, uuid, i)
		if !strings.Contains(out, want) {
			t.Errorf("missing MIG instance %s in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "1g.18gb") {
		t.Error("the H200 capture's profile leaked into this one")
	}
}

// TestSecondMIGRepeatsTheDriverLimitations checks that both limitations
// SPEC.md §Collectors records for the nvidia-smi source hold on a second card
// and a second driver install, rather than being a quirk of the H200 rental:
// no card-level utilization under MIG, and a MIG-resident process attributed
// to its parent.
func TestSecondMIGRepeatsTheDriverLimitations(t *testing.T) {
	out := collectSecondMIG(t, func(o *Options) { o.PerProcess = true })

	if strings.Contains(out, prefix+"utilization_ratio") {
		t.Error("utilization_ratio emitted, but this card also reports [N/A] under MIG")
	}
	want := prefix + `process_memory_bytes{node="fixture",gpu_uuid="` + h100UUID + `",command="loadgen"} 4391436288`
	if !strings.Contains(out, want) {
		t.Errorf("missing %s in:\n%s", want, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix+"process_memory_bytes") && strings.Contains(line, "mig_uuid=") {
			t.Errorf("this source cannot attribute a process to a MIG instance, but did: %s", line)
		}
	}
}

// TestSameCardBothModes uses what no other pair of captures can offer: the
// same physical GPU, captured partitioned and unpartitioned within an hour.
//
// The UUID is identical across both, so anything that differs is the mode and
// not the hardware — which makes this the tightest available check that
// mig_enabled and the instance families follow the card's configuration rather
// than something incidental to a particular host.
func TestSameCardBothModes(t *testing.T) {
	partitioned := collectSecondMIG(t)
	plain := collectDefaultMode(t)

	if !strings.Contains(partitioned, prefix+`mig_enabled{node="fixture",gpu_uuid="`+h100UUID+`"} 1`) {
		t.Error("mig_enabled is not 1 for the partitioned capture of the card")
	}
	if !strings.Contains(plain, prefix+`mig_enabled{node="fixture",gpu_uuid="`+h100UUID+`"} 0`) {
		t.Error("mig_enabled is not 0 for the Default-mode capture of the same card")
	}
	if strings.Contains(plain, "mig_uuid=") {
		t.Error("the same card reports MIG instances in Default mode")
	}

	// Utilization is the field the two modes disagree about, and the disagreement
	// is the driver's, not the parser's: a number in Default mode, [N/A] under
	// MIG. One card, one driver, one hour apart.
	if !strings.Contains(plain, prefix+"utilization_ratio") {
		t.Error("no utilization_ratio in Default mode")
	}
	if strings.Contains(partitioned, prefix+"utilization_ratio") {
		t.Error("utilization_ratio survived MIG being enabled on the same card")
	}
}

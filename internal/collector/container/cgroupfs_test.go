// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// cgroupfsDir is the second capture: a managed Kubernetes node whose kubelet
// runs the **cgroupfs** cgroup driver rather than systemd.
//
// Every directory name in it differs from the systemd tree — no `.scope`
// suffix, no runtime prefix, QoS as its own level, the UID unescaped — so
// before identifyPodChild existed the walk returned nothing at all here. The
// numbers below are what the capture holds; see testdata/README.md for which
// pod is which.
const cgroupfsDir = "testdata/doks-cgroupfs-20260801"

const (
	cgroupfsContainers = 14
	cgroupfsPods       = 5
	cgroupfsBurstable  = 7 // containers, not pods
	cgroupfsBestEffort = 7
)

// collectCgroupfs renders one pass over the cgroupfs capture.
func collectCgroupfs(t *testing.T) string {
	t.Helper()
	set := newFixtureSet()
	if err := New(Options{Roots: fsroot.At(cgroupfsDir)}).Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := set.Err(); err != nil {
		t.Fatalf("exposition problems: %v", err)
	}
	return set.String()
}

// TestCgroupfsGolden pins the whole output for this driver, the same way
// TestGolden does for the systemd one. A second golden rather than an addition
// to the first: the two trees are different hosts, and merging them would hide
// which driver produced which series.
func TestCgroupfsGolden(t *testing.T) {
	got := collectCgroupfs(t)
	path := filepath.Join("testdata", "golden", "container-cgroupfs.prom")

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

// TestCgroupfsDiscoversEveryContainer is the assertion that would have caught
// the gap: the capture holds fourteen container cgroups and the walk must find
// all of them, not the zero it found before.
func TestCgroupfsDiscoversEveryContainer(t *testing.T) {
	found, err := New(Options{Roots: fsroot.At(cgroupfsDir)}).discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != cgroupfsContainers {
		t.Fatalf("discovered %d containers, want %d", len(found), cgroupfsContainers)
	}

	pods := map[string]struct{}{}
	qos := map[string]int{}
	for _, cg := range found {
		if cg.pod == "" {
			t.Errorf("container %s has no pod UID; every cgroup in this tree is under one", cg.id)
		}
		pods[cg.pod] = struct{}{}
		qos[cg.qos]++
	}
	if len(pods) != cgroupfsPods {
		t.Errorf("found %d distinct pods, want %d", len(pods), cgroupfsPods)
	}
	if qos["burstable"] != cgroupfsBurstable {
		t.Errorf("burstable containers = %d, want %d", qos["burstable"], cgroupfsBurstable)
	}
	if qos["besteffort"] != cgroupfsBestEffort {
		t.Errorf("besteffort containers = %d, want %d", qos["besteffort"], cgroupfsBestEffort)
	}
}

// TestCgroupfsPodUIDsAreUnescaped checks the UID reaches the label the way
// Kubernetes spells it. The systemd driver escapes a hyphen to an underscore
// and podIdentity converts it back; this driver never escapes it, and a
// conversion applied twice would be just as wrong as one never applied.
func TestCgroupfsPodUIDsAreUnescaped(t *testing.T) {
	found, err := New(Options{Roots: fsroot.At(cgroupfsDir)}).discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, cg := range found {
		if strings.Contains(cg.pod, "_") {
			t.Errorf("pod UID %q contains an underscore; this driver does not escape them", cg.pod)
		}
		if strings.Count(cg.pod, "-") != 4 {
			t.Errorf("pod UID %q is not in 8-4-4-4-12 form", cg.pod)
		}
	}
}

// TestCgroupfsRuntimeIsEmpty pins the deliberate omission.
//
// These directory names carry no runtime — that is a property of the layout,
// not a parse failure — so prickle_container_info reports an empty one. The
// test exists so that filling it in later is a visible decision rather than a
// quiet drift, and so that nobody "fixes" it by guessing containerd.
func TestCgroupfsRuntimeIsEmpty(t *testing.T) {
	found, err := New(Options{Roots: fsroot.At(cgroupfsDir)}).discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, cg := range found {
		if cg.runtime != "" {
			t.Errorf("container %s reports runtime %q; this layout does not name one",
				cg.id, cg.runtime)
		}
	}

	rendered := collectCgroupfs(t)
	if !strings.Contains(rendered, `runtime=""`) {
		t.Error(`no prickle_container_info sample carries runtime=""`)
	}
}

// TestCgroupfsSystemdTreeStillParses guards the other direction: the systemd
// layout must keep working now that a second shape is accepted. The bare-hex
// branch is reached only when the .scope branch declines, and a container ID
// is hex either way — so a mistake here would silently reclassify every
// systemd container.
func TestCgroupfsSystemdTreeStillParses(t *testing.T) {
	found, err := New(Options{Roots: fsroot.At(fixtureDir)}).discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != fixtureContainers {
		t.Fatalf("systemd tree discovered %d containers, want %d", len(found), fixtureContainers)
	}
	for _, cg := range found {
		if cg.runtime == "" {
			t.Errorf("systemd container %s lost its runtime", cg.id)
		}
	}
}

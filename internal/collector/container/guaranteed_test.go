// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// guaranteedDir is a kubeadm node running the **cgroupfs** cgroup driver with
// all three QoS classes present, captured 2026-08-02.
//
// It exists for one directory shape. Under the cgroupfs driver a Guaranteed pod
// has no QoS level at all — its cgroup is `kubepods/pod<uid>/<hex>`, a level
// shallower than Burstable's `kubepods/burstable/pod<uid>/<hex>`. Until this
// capture that shape was parsed on the directory name alone in TestIdentify,
// because neither cluster available had ever run a Guaranteed pod: QoS follows
// from requests versus limits, and Guaranteed needs `requests == limits` for
// cpu *and* memory on every container in the pod, which nothing does by
// accident. It had to be arranged deliberately.
//
// The systemd-driver spelling of the same class is in
// kubeadm-systemd-20260801; this is the other one.
const guaranteedDir = "testdata/kubeadm-cgroupfs-20260802"

const (
	guaranteedContainers  = 32
	guaranteedGuaranteed  = 2 // containers in the one Guaranteed pod
	guaranteedBurstable   = 22
	guaranteedBestEffort  = 8
	guaranteedPodUID      = "cc3fc5a5-a01f-4b3b-b846-2e5686dbb650"
	guaranteedPodQoSLevel = "guaranteed"
)

func guaranteedDiscover(t *testing.T) []cgroup {
	t.Helper()
	found, err := New(Options{Roots: fsroot.At(guaranteedDir)}).discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestGuaranteedPodHasNoQoSLevel is the assertion the fixture was captured for.
//
// The QoS class of a Guaranteed pod is not written anywhere in its path. It is
// implied by the *absence* of a level: the pod directory sits directly under
// kubepods. So qosFromDir has to read the parent of the pod directory and treat
// "kubepods" itself as meaning guaranteed — a rule that reads like an
// off-by-one until you see the tree. This checks it against a tree a kubelet
// actually produced rather than against the rule's own restatement.
func TestGuaranteedPodHasNoQoSLevel(t *testing.T) {
	var seen int
	for _, cg := range guaranteedDiscover(t) {
		if cg.pod != guaranteedPodUID {
			continue
		}
		seen++
		if cg.qos != guaranteedPodQoSLevel {
			t.Errorf("container %.12s in the Guaranteed pod: qos = %q, want %q",
				cg.id, cg.qos, guaranteedPodQoSLevel)
		}
	}
	if seen != guaranteedGuaranteed {
		t.Errorf("found %d containers in the Guaranteed pod, want %d", seen, guaranteedGuaranteed)
	}
}

// TestGuaranteedTreeQoSSpread checks the other two classes came through the
// same walk, because a parser that special-cased the shallow layout could
// easily stop seeing the deeper one.
func TestGuaranteedTreeQoSSpread(t *testing.T) {
	found := guaranteedDiscover(t)
	if len(found) != guaranteedContainers {
		t.Fatalf("discovered %d containers, want %d", len(found), guaranteedContainers)
	}

	qos := map[string]int{}
	for _, cg := range found {
		if cg.pod == "" {
			t.Errorf("container %.12s has no pod UID; every cgroup in this tree is under one", cg.id)
		}
		qos[cg.qos]++
	}
	for _, tc := range []struct {
		class string
		want  int
	}{
		{"guaranteed", guaranteedGuaranteed},
		{"burstable", guaranteedBurstable},
		{"besteffort", guaranteedBestEffort},
	} {
		if qos[tc.class] != tc.want {
			t.Errorf("%s containers = %d, want %d", tc.class, qos[tc.class], tc.want)
		}
	}
	// Anything left over means a fourth value appeared, which under this
	// layout can only be an unrecognised parent directory read as a QoS class.
	if total := qos["guaranteed"] + qos["burstable"] + qos["besteffort"]; total != len(found) {
		t.Errorf("%d containers carry a QoS class outside the three real ones", len(found)-total)
	}
}

func TestGuaranteedGolden(t *testing.T) {
	set := newFixtureSet()
	if err := New(Options{Roots: fsroot.At(guaranteedDir)}).Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := set.Err(); err != nil {
		t.Fatalf("exposition problems: %v", err)
	}
	got := set.String()
	path := filepath.Join("testdata", "golden", "container-kubeadm-cgroupfs.prom")

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

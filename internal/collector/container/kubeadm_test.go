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

// kubeadmDir is a kubeadm node — Kubernetes 1.34.10, containerd 2.2.2, kernel
// 7.0.0 — with the kubelet on the **systemd** cgroup driver.
//
// It exists for one directory name. `kubepods-pod<uid>.slice`, the Guaranteed
// pod shape with no QoS component, had been parse-only since Phase 2 because
// no captured cluster ever ran a Guaranteed pod: DigitalOcean's managed cluster
// had none, and the original rental had none. Three pods were created here on
// purpose, one per QoS class, so all three shapes sit in one tree.
const kubeadmDir = "testdata/kubeadm-systemd-20260801"

// Pod UIDs as Kubernetes reported them — with hyphens, not the underscores
// systemd writes into the slice name. That difference is the point of
// TestKubeadmGuaranteedPod.
const (
	kubeadmGuaranteedPod = "54af9685-6b23-4dc9-aaf3-85520df7a05e"
	kubeadmBurstablePod  = "1e387c5f-7402-44de-b881-2fc0f8aeccf2"
	kubeadmBestEffortPod = "59ae0a77-5850-4cf4-9cd3-43df8caecf38"
)

const kubeadmContainers = 10

func kubeadmDiscover(t *testing.T) []cgroup {
	t.Helper()
	found, err := New(Options{Roots: fsroot.At(kubeadmDir)}).discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != kubeadmContainers {
		t.Fatalf("discovered %d containers, want %d", len(found), kubeadmContainers)
	}
	return found
}

// TestKubeadmGuaranteedPod is the assertion this whole capture was built for.
//
// A Guaranteed pod sits directly under kubepods.slice with no QoS component in
// its name, so podSlicePattern has to make that component optional and default
// the class to "guaranteed". That branch was written from the systemd naming
// rules and never executed against a real one until now.
func TestKubeadmGuaranteedPod(t *testing.T) {
	var got []cgroup
	for _, cg := range kubeadmDiscover(t) {
		if cg.pod == kubeadmGuaranteedPod {
			got = append(got, cg)
		}
	}
	if len(got) == 0 {
		t.Fatalf("no container found for the Guaranteed pod %s", kubeadmGuaranteedPod)
	}
	for _, cg := range got {
		if cg.qos != "guaranteed" {
			t.Errorf("container %s: qos = %q, want guaranteed", cg.id, cg.qos)
		}
		if cg.runtime != "containerd" {
			t.Errorf("container %s: runtime = %q, want containerd", cg.id, cg.runtime)
		}
	}
}

// TestKubeadmAllThreeQoSClasses: one tree, three shapes. Before this capture no
// fixture held more than two, so nothing checked that the QoS-bearing and
// QoS-less spellings coexist without one swallowing the other.
func TestKubeadmAllThreeQoSClasses(t *testing.T) {
	want := map[string]string{
		kubeadmGuaranteedPod: "guaranteed",
		kubeadmBurstablePod:  "burstable",
		kubeadmBestEffortPod: "besteffort",
	}
	seen := map[string]string{}
	for _, cg := range kubeadmDiscover(t) {
		if class, ok := want[cg.pod]; ok {
			if cg.qos != class {
				t.Errorf("pod %s: qos = %q, want %q", cg.pod, cg.qos, class)
			}
			seen[cg.pod] = cg.qos
		}
	}
	for uid := range want {
		if _, ok := seen[uid]; !ok {
			t.Errorf("pod %s is missing from the walk entirely", uid)
		}
	}
}

// TestKubeadmPodUIDsAreUnescaped: systemd escapes a hyphen to an underscore
// because the hyphen is its own slice path separator. The label has to carry
// what `kubectl get pod -o jsonpath='{.metadata.uid}'` reports, or a join
// against Kubernetes metadata silently matches nothing.
func TestKubeadmPodUIDsAreUnescaped(t *testing.T) {
	for _, cg := range kubeadmDiscover(t) {
		if strings.Contains(cg.pod, "_") {
			t.Errorf("pod UID %q still carries systemd's underscore escaping", cg.pod)
		}
	}
}

func TestKubeadmGolden(t *testing.T) {
	set := newFixtureSet()
	if err := New(Options{Roots: fsroot.At(kubeadmDir)}).Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := set.Err(); err != nil {
		t.Fatalf("exposition problems: %v", err)
	}
	got := set.String()
	path := filepath.Join("testdata", "golden", "container-kubeadm-systemd.prom")

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

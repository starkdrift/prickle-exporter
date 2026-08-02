// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// podNamesDir is a kubeadm node captured 2026-08-01 with a pod deliberately
// named `web-frontend` holding containers `nginx` and `sidecar`, so the join
// can be checked against names somebody chose rather than generated ones.
//
// Only the directory structure was captured. /var/log/pods holds workload log
// output and the names are entirely in the directory names, so nothing inside
// is read and nothing inside is committed.
const podNamesDir = "testdata/kubeadm-podnames-20260801"

func TestParsePodLogDir(t *testing.T) {
	tests := []struct {
		dir      string
		ns, name string
		uid      string
		ok       bool
	}{
		{"default_web-frontend_537209ed-f2d7-423a-8e0a-ec05d6280092",
			"default", "web-frontend", "537209ed-f2d7-423a-8e0a-ec05d6280092", true},
		// A static pod's UID is bare hex with no hyphens.
		{"kube-system_etcd-k1_6bc9b3cf5aaa8164b7e9fa5d637f956e",
			"kube-system", "etcd-k1", "6bc9b3cf5aaa8164b7e9fa5d637f956e", true},
		// Hyphens are everywhere in real names and must not confuse the split.
		{"kube-flannel_kube-flannel-ds-l8wfp_e26d05fe-52a6-4f86-aa9a-eb3966040734",
			"kube-flannel", "kube-flannel-ds-l8wfp", "e26d05fe-52a6-4f86-aa9a-eb3966040734", true},
		{"not-a-pod-directory", "", "", "", false},
		{"only_two", "", "", "", false},
		{"a_b_c_d", "", "", "", false},
		{"_empty_ns", "", "", "", false},
	}
	for _, tt := range tests {
		meta, uid, ok := parsePodLogDir(tt.dir)
		if ok != tt.ok {
			t.Errorf("parsePodLogDir(%q) ok = %v, want %v", tt.dir, ok, tt.ok)
			continue
		}
		if !ok {
			continue
		}
		if meta.namespace != tt.ns || meta.name != tt.name || uid != tt.uid {
			t.Errorf("parsePodLogDir(%q) = %q/%q uid %q, want %q/%q uid %q",
				tt.dir, meta.namespace, meta.name, uid, tt.ns, tt.name, tt.uid)
		}
	}
}

// TestPodNamesOffByDefault is the guard on the privilege trade. The reader must
// not touch /var/log/pods unless asked: that path is root-only, and an exporter
// that reached for it unprompted would make every unprivileged deployment log a
// permission error on every pass.
func TestPodNamesOffByDefault(t *testing.T) {
	c := New(Options{Roots: fsroot.At(podNamesDir)})
	pods, err := c.podNames()
	if err != nil {
		t.Fatalf("podNames: %v", err)
	}
	if pods != nil {
		t.Errorf("podNames returned %d entries with the flag off; want none", len(pods))
	}
}

func TestPodNamesResolvesUIDs(t *testing.T) {
	c := New(Options{Roots: fsroot.At(podNamesDir), PodNames: true})
	pods, err := c.podNames()
	if err != nil {
		t.Fatalf("podNames: %v", err)
	}
	got, ok := pods["537209ed-f2d7-423a-8e0a-ec05d6280092"]
	if !ok {
		t.Fatalf("web-frontend's UID not resolved; got %d pods", len(pods))
	}
	if got.namespace != "default" || got.name != "web-frontend" {
		t.Errorf("resolved %q/%q, want default/web-frontend", got.namespace, got.name)
	}
}

// TestPodNamesReachTheOutput is the end-to-end join: the UID comes from the
// cgroup tree, the name from the kubelet's directory, and they meet on the
// _info gauge.
func TestPodNamesReachTheOutput(t *testing.T) {
	set := newFixtureSet()
	c := New(Options{Roots: fsroot.At(podNamesDir), PodNames: true})
	if err := c.Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := set.String()

	if !strings.Contains(out, `pod_name="web-frontend"`) {
		t.Error(`no info series carries pod_name="web-frontend"`)
	}
	if !strings.Contains(out, `namespace="default"`) {
		t.Error(`no series carries namespace="default"`)
	}
	// `pod` must still be the UID: it is the key every existing rule joins on,
	// and repurposing it would break them silently.
	if !strings.Contains(out, `pod="537209ed-f2d7-423a-8e0a-ec05d6280092"`) {
		t.Error("pod no longer carries the UID")
	}
}

// TestPodNamesAbsentWithoutTheFlag: the same tree, flag off, must produce the
// UID and no name — the unprivileged behaviour an operator keeps by default.
func TestPodNamesAbsentWithoutTheFlag(t *testing.T) {
	set := newFixtureSet()
	c := New(Options{Roots: fsroot.At(podNamesDir)})
	if err := c.Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := set.String()

	if strings.Contains(out, `pod_name="web-frontend"`) {
		t.Error("a pod name appeared without -collector.container.pod-names")
	}
	// Specifically the *hot* series. prickle_container_info carries the key
	// unconditionally, empty, exactly as it does for pod_name, name and image —
	// a companion gauge with a varying label set would be worse to query than
	// one with an empty value. What must not happen without the flag is a hot
	// series gaining a key, because SPEC.md §Versioning counts that as a major
	// for breaking every aggregation written without `by`.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) &&
			!strings.HasPrefix(line, prefix+"info{") &&
			strings.Contains(line, "namespace=") {
			t.Errorf("a hot series gained a namespace key without the flag: %s", line)
		}
	}
	if !strings.Contains(out, `pod="537209ed-f2d7-423a-8e0a-ec05d6280092"`) {
		t.Error("the pod UID should still be reported")
	}
}

// TestPodNamesUnreadableIsNotAnError: an operator who enables the flag without
// granting the privilege gets UIDs and a note, not a collector that fails every
// pass and raises prickle_collector_errors_total forever.
func TestPodNamesUnreadableIsNotAnError(t *testing.T) {
	c := New(Options{Roots: fsroot.At(t.TempDir()), PodNames: true})
	pods, err := c.podNames()
	if err != nil {
		t.Errorf("a missing pod log directory reported an error: %v", err)
	}
	if len(pods) != 0 {
		t.Errorf("got %d pods from an empty tree", len(pods))
	}
}

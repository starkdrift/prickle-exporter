// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// TestIdentify covers the directory-name shapes SPEC.md §Collectors lists.
//
// The docker- and cri-containerd- cases are the ones the captured tree
// exercises end to end; crio- and the Guaranteed pod slice are here because
// they are named in SPEC.md but absent from the capture (testdata/README.md
// §Coverage gaps), and a unit test on the name parse is the most this package
// can honestly assert about them.
func TestIdentify(t *testing.T) {
	const id = "48c2b913843a172adff5fb81b2e0a0d1e4916f03c3b0c47d6b746875465c9d74"

	tests := []struct {
		name    string
		path    string
		want    cgroup
		wantNot bool
	}{{
		name: "docker scope under system.slice",
		path: "/system.slice/docker-" + id + ".scope",
		want: cgroup{id: id, runtime: "docker"},
	}, {
		name: "containerd scope in a besteffort pod",
		path: "/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod07fc7cef_656b_48a7_929d_2734c2b4498e.slice/cri-containerd-" + id + ".scope",
		want: cgroup{id: id, runtime: "containerd", pod: "07fc7cef-656b-48a7-929d-2734c2b4498e", qos: "besteffort"},
	}, {
		name: "containerd scope in a burstable pod",
		path: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod6eb5044d_ef2e_49d1_a9cc_28f4e3fe88a3.slice/cri-containerd-" + id + ".scope",
		want: cgroup{id: id, runtime: "containerd", pod: "6eb5044d-ef2e-49d1-a9cc-28f4e3fe88a3", qos: "burstable"},
	}, {
		name: "crio scope in a guaranteed pod",
		path: "/kubepods.slice/kubepods-pod6eb5044d_ef2e_49d1_a9cc_28f4e3fe88a3.slice/crio-" + id + ".scope",
		want: cgroup{id: id, runtime: "crio", pod: "6eb5044d-ef2e-49d1-a9cc-28f4e3fe88a3", qos: "guaranteed"},
	}, {
		// The cgroupfs driver. No .scope suffix, no runtime prefix, QoS as its
		// own directory level, and the UID unescaped. Covered end to end by
		// testdata/doks-cgroupfs-20260801; these pin the name parse alone.
		name: "cgroupfs burstable pod",
		path: "/kubepods/burstable/pod4d521664-aa00-4570-9841-ce67a3756762/" + id,
		want: cgroup{id: id, pod: "4d521664-aa00-4570-9841-ce67a3756762", qos: "burstable"},
	}, {
		name: "cgroupfs besteffort pod",
		path: "/kubepods/besteffort/pode7aa4094-2f07-4a8a-b4b1-fb1f38d6c2dd/" + id,
		want: cgroup{id: id, pod: "e7aa4094-2f07-4a8a-b4b1-fb1f38d6c2dd", qos: "besteffort"},
	}, {
		// Guaranteed has no QoS level under either driver. Uncaptured in both
		// layouts — the clusters ran none — so this is parse-only, exactly as
		// the systemd Guaranteed case above is.
		name: "cgroupfs guaranteed pod",
		path: "/kubepods/pod6eb5044d-ef2e-49d1-a9cc-28f4e3fe88a3/" + id,
		want: cgroup{id: id, pod: "6eb5044d-ef2e-49d1-a9cc-28f4e3fe88a3", qos: "guaranteed"},
	}, {
		// Docker with native.cgroupdriver=cgroupfs. Unlike the kubepods case
		// below, the parent directory does name the runtime.
		name: "cgroupfs docker container",
		path: "/docker/" + id,
		want: cgroup{id: id, runtime: "docker"},
	}, {
		name:    "a docker directory entry that is not an ID",
		path:    "/docker/buildkit",
		wantNot: true,
	}, {
		name:    "the cgroupfs pod directory itself is not a container",
		path:    "/kubepods/burstable/pod4d521664-aa00-4570-9841-ce67a3756762",
		wantNot: true,
	}, {
		// A bare hex directory is only a container because of where it sits.
		// Without the pod parent this would match any hex-named cgroup a
		// process happened to create.
		name:    "a bare hex directory outside a pod",
		path:    "/system.slice/" + id,
		wantNot: true,
	}, {
		name:    "the pod slice itself is not a container",
		path:    "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod6eb5044d_ef2e_49d1_a9cc_28f4e3fe88a3.slice",
		wantNot: true,
	}, {
		name:    "a system unit is not a container",
		path:    "/system.slice/containerd.service",
		wantNot: true,
	}, {
		name:    "a scope that only looks like one",
		path:    "/system.slice/docker-shim.scope",
		wantNot: true,
	}, {
		name:    "an unknown runtime prefix",
		path:    "/system.slice/podman-" + id + ".scope",
		wantNot: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := identify(tt.path, filepath.Base(tt.path))
			if tt.wantNot {
				if ok {
					t.Fatalf("identified %s as container %q", tt.path, got.id)
				}
				return
			}
			if !ok {
				t.Fatalf("did not identify %s as a container", tt.path)
			}
			tt.want.dir = tt.path
			if got != tt.want {
				t.Errorf("identity = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestNestedCgroupsAreNotDoubleCounted checks that the walk stops at a
// container leaf. A container running its own init or a nested runtime creates
// child cgroups whose counters are already included in the parent's.
func TestNestedCgroupsAreNotDoubleCounted(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd01"
	const nested = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

	dir := t.TempDir()
	outer := filepath.Join(dir, "sys", "fs", "cgroup", "system.slice", "docker-"+id+".scope")
	inner := filepath.Join(outer, "docker-"+nested+".scope")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{outer, inner} {
		if err := os.WriteFile(filepath.Join(d, "memory.current"), []byte("4096\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	found, err := New(Options{Roots: fsroot.At(dir)}).discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("discovered %d containers, want 1 (the walk must not descend into a container)", len(found))
	}
	if found[0].id != id {
		t.Errorf("discovered %q, want the outer container %q", found[0].id, id)
	}
}

// TestPerCgroupPressure covers the two PSI files the capture does not contain.
//
// cpu.pressure and io.pressure are the same format as memory.pressure and as
// the /proc/pressure/* files the host collector's fixtures do cover, so the
// tree here is written in that captured format rather than invented — see
// testdata/README.md §Coverage gaps.
func TestPerCgroupPressure(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd01"

	dir := t.TempDir()
	scope := filepath.Join(dir, "sys", "fs", "cgroup", "system.slice", "docker-"+id+".scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ name, body string }{
		{"cpu.pressure", "some avg10=0.00 avg60=0.11 avg300=0.05 total=1500000\n"},
		{"io.pressure", "some avg10=0.00 avg60=0.00 avg300=0.00 total=250000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=125000\n"},
	} {
		if err := os.WriteFile(filepath.Join(scope, f.name), []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	set := exposition.NewSet()
	if err := New(Options{Roots: fsroot.At(dir)}).Collect(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	out := set.String()

	for _, want := range []string{
		`resource="cpu",kind="some"} 1.5`,
		`resource="io",kind="some"} 0.25`,
		`resource="io",kind="full"} 0.125`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing pressure series %s in:\n%s", want, out)
		}
	}
	// cpu.pressure has no `full` line on many kernels, and this one does not.
	if strings.Contains(out, `resource="cpu",kind="full"`) {
		t.Error("emitted a cpu full-pressure series the file does not contain")
	}
}

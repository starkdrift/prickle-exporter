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

// nerdctlDir is containerd driven directly rather than through the CRI:
// AlmaLinux 9.8, containerd with the systemd cgroup driver, nerdctl 2.3.5,
// captured 2026-08-01. Its scopes are nerdctl-<hex>.scope under system.slice.
const nerdctlDir = "testdata/containerd-nerdctl-20260801"

const nerdctlContainers = 2

// TestNerdctlIsContainerd pins the contract decision as much as the parse: a
// container started by nerdctl and one started by a kubelet are the same
// runtime, so they carry the same label value. Splitting them would put one
// runtime under two names for no reason a query could use.
func TestNerdctlIsContainerd(t *testing.T) {
	found, err := New(Options{Roots: fsroot.At(nerdctlDir)}).discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != nerdctlContainers {
		t.Fatalf("discovered %d containers, want %d", len(found), nerdctlContainers)
	}
	for _, cg := range found {
		if cg.runtime != "containerd" {
			t.Errorf("container %s: runtime = %q, want containerd", cg.id, cg.runtime)
		}
		if cg.pod != "" || cg.qos != "" {
			t.Errorf("container %s: pod=%q qos=%q, want both empty under system.slice",
				cg.id, cg.pod, cg.qos)
		}
	}
}

// TestBothContainerdSpellingsAgree reads a CRI scope and a nerdctl scope through
// identify and checks they come out identical but for the ID.
func TestBothContainerdSpellingsAgree(t *testing.T) {
	const id = "5990b0f7b874c5c4272ee2e6463a90eb7b2239e9b42e2e7cdf9c9e6c88002006"

	cri, ok := identify("/system.slice/cri-containerd-"+id+".scope", "cri-containerd-"+id+".scope")
	if !ok {
		t.Fatal("cri-containerd scope not identified")
	}
	nerd, ok := identify("/system.slice/nerdctl-"+id+".scope", "nerdctl-"+id+".scope")
	if !ok {
		t.Fatal("nerdctl scope not identified")
	}
	if cri.runtime != nerd.runtime {
		t.Errorf("runtime differs: cri=%q nerdctl=%q", cri.runtime, nerd.runtime)
	}
	if cri.id != nerd.id {
		t.Errorf("id differs: cri=%q nerdctl=%q", cri.id, nerd.id)
	}
}

func TestNerdctlGolden(t *testing.T) {
	set := newFixtureSet()
	if err := New(Options{Roots: fsroot.At(nerdctlDir)}).Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := set.Err(); err != nil {
		t.Fatalf("exposition problems: %v", err)
	}
	got := set.String()
	path := filepath.Join("testdata", "golden", "container-containerd-nerdctl.prom")

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
	if !strings.Contains(got, `runtime="containerd"`) {
		t.Error(`no sample carries runtime="containerd"`)
	}
}

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

// podmanDir is AlmaLinux 9.8, kernel 5.14, podman 5.8.2 with the systemd cgroup
// manager on cgroup v2, captured 2026-08-01.
//
// Podman is the default container runtime on RHEL and Fedora, and before
// libpod- was added to scopePrefixes a host running it reported no containers
// at all. Its scopes live under machine.slice rather than a pod slice, so there
// is no pod or QoS identity to read.
const podmanDir = "testdata/podman-alma9-20260801"

const podmanContainers = 3

func podmanDiscover(t *testing.T) []cgroup {
	t.Helper()
	found, err := New(Options{Roots: fsroot.At(podmanDir)}).discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestPodmanScopesAreFound(t *testing.T) {
	found := podmanDiscover(t)
	if len(found) != podmanContainers {
		t.Fatalf("discovered %d containers, want %d", len(found), podmanContainers)
	}
	for _, cg := range found {
		if cg.runtime != "podman" {
			t.Errorf("container %s: runtime = %q, want podman", cg.id, cg.runtime)
		}
		// machine.slice is not a pod slice; inventing identity from it would be
		// worse than leaving it empty.
		if cg.pod != "" || cg.qos != "" {
			t.Errorf("container %s: pod=%q qos=%q, want both empty under machine.slice",
				cg.id, cg.pod, cg.qos)
		}
	}
}

// TestMonitorScopesAreNotContainers is the one that would break quietly.
//
// podman and CRI-O both pair each container with a conmon monitor scope —
// libpod-conmon-<hex>.scope, crio-conmon-<hex>.scope. Nothing rejects them
// explicitly: stripping the runtime prefix leaves "conmon-<hex>", and hexID
// declines it because `conmon-` is not hex. Two independent rules happening to
// compose. If either is ever loosened, every container on such a host doubles,
// with the monitor's near-zero usage reported under an ID that looks like a
// container's, so the accident is pinned here deliberately.
func TestMonitorScopesAreNotContainers(t *testing.T) {
	root := filepath.Join(podmanDir, "sys", "fs", "cgroup", "machine.slice")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	var monitors int
	for _, e := range entries {
		if !strings.Contains(e.Name(), "conmon-") {
			continue
		}
		monitors++
		if cg, ok := identify(filepath.Join(root, e.Name()), e.Name()); ok {
			t.Errorf("%s was identified as container %q", e.Name(), cg.id)
		}
	}
	// If the capture stops containing monitor scopes this test proves nothing.
	if monitors != podmanContainers {
		t.Fatalf("fixture holds %d conmon scopes, want %d — the pairing this test rests on is gone",
			monitors, podmanContainers)
	}

	// And the same rule for CRI-O, spelled differently.
	const crioConmon = "crio-conmon-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd.scope"
	if _, ok := identify("/system.slice/"+crioConmon, crioConmon); ok {
		t.Error("a crio-conmon scope was identified as a container")
	}
}

// TestPodmanQuotaAndLimit: the same three containers as every other capture,
// so the v2 readers are exercised on a runtime that had never been read.
func TestPodmanQuotaAndLimit(t *testing.T) {
	set := newFixtureSet()
	if err := New(Options{Roots: fsroot.At(podmanDir)}).Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	rendered := set.String()

	for _, want := range []string{
		`prickle_container_cpu_limit_cores{`,
		`prickle_container_memory_limit_bytes{`,
		`runtime="podman"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestPodmanGolden(t *testing.T) {
	set := newFixtureSet()
	if err := New(Options{Roots: fsroot.At(podmanDir)}).Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := set.Err(); err != nil {
		t.Fatalf("exposition problems: %v", err)
	}
	got := set.String()
	path := filepath.Join("testdata", "golden", "container-podman.prom")

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

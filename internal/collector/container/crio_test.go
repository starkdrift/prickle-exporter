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

// crioDir is a CRI-O node: the same kubeadm cluster as kubeadm-systemd-20260801
// with the worker's runtime switched to CRI-O 1.32.1 and the node rebooted, so
// nothing containerd wrote is left in the tree.
//
// `crio-<hex>.scope` had been parse-only since Phase 2 — SPEC.md §Collectors
// names CRI-O, but no CRI-O host had ever been captured, so the prefix was
// taken on faith from the runtime's documentation.
const crioDir = "testdata/crio-systemd-20260801"

const crioContainers = 5

func crioDiscover(t *testing.T) []cgroup {
	t.Helper()
	found, err := New(Options{Roots: fsroot.At(crioDir)}).discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestCrioScopesAreFound confirms the prefix against a real node.
func TestCrioScopesAreFound(t *testing.T) {
	found := crioDiscover(t)
	if len(found) != crioContainers {
		t.Fatalf("discovered %d containers, want %d", len(found), crioContainers)
	}
	for _, cg := range found {
		if cg.runtime != "crio" {
			t.Errorf("container %s: runtime = %q, want crio", cg.id, cg.runtime)
		}
		if cg.pod == "" {
			t.Errorf("container %s has no pod UID", cg.id)
		}
	}
}

// TestCrioSuffixlessSiblingsAreSkipped is the reason this capture is worth
// more than a prefix check.
//
// CRI-O writes *two* directories per pod: `crio-<hex>.scope`, the container,
// and `crio-<hex>` with no suffix, which is empty — zero processes, zero bytes.
// Nothing in the code was written with that sibling in mind; it is skipped
// because identifyScope requires the `.scope` suffix and identifyBareID
// requires a bare hex name, and `crio-<hex>` is neither. That is the right
// outcome reached by accident, so it gets a test: counting them would double
// this node's container count and emit five containers whose every value is
// zero, which reads as five idle containers rather than as an artefact.
func TestCrioSuffixlessSiblingsAreSkipped(t *testing.T) {
	root := filepath.Join(crioDir, "sys", "fs", "cgroup", "kubepods.slice")

	var scoped, suffixless []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		switch name := d.Name(); {
		case strings.HasPrefix(name, "crio-") && strings.HasSuffix(name, ".scope"):
			scoped = append(scoped, path)
		case strings.HasPrefix(name, "crio-"):
			suffixless = append(suffixless, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// If the capture ever stops containing the pairing, this test proves
	// nothing and should say so rather than passing quietly.
	if len(suffixless) != crioContainers {
		t.Fatalf("fixture has %d suffixless crio directories, want %d — the pairing this test rests on is gone",
			len(suffixless), crioContainers)
	}
	if len(scoped) != crioContainers {
		t.Fatalf("fixture has %d crio scopes, want %d", len(scoped), crioContainers)
	}

	// Every suffixless sibling is empty; that is why dropping them loses
	// nothing. Asserted rather than assumed, because if CRI-O ever starts
	// putting the sandbox's processes here, skipping them stops being free.
	for _, dir := range suffixless {
		for _, file := range []string{"pids.current", "memory.current"} {
			b, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				continue
			}
			if got := strings.TrimSpace(string(b)); got != "0" {
				t.Errorf("%s/%s = %q, want 0 — a skipped cgroup is holding something",
					filepath.Base(dir), file, got)
			}
		}
	}

	// And the walk agrees: the scopes, not the siblings.
	if got := len(crioDiscover(t)); got != len(scoped) {
		t.Errorf("walk found %d containers, want %d (the scopes only)", got, len(scoped))
	}
}

// TestCrioGuaranteedPod: the QoS rules are the kubelet's, not the runtime's, so
// switching CRI-O in must not disturb them. Same cluster and same three pods as
// kubeadm-systemd-20260801, so this is a genuine A/B on the runtime alone.
func TestCrioGuaranteedPod(t *testing.T) {
	seen := map[string]bool{}
	for _, cg := range crioDiscover(t) {
		seen[cg.qos] = true
	}
	for _, class := range []string{"guaranteed", "burstable", "besteffort"} {
		if !seen[class] {
			t.Errorf("no container with qos=%q; the CRI-O tree should hold all three", class)
		}
	}
}

func TestCrioGolden(t *testing.T) {
	set := newFixtureSet()
	if err := New(Options{Roots: fsroot.At(crioDir)}).Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := set.Err(); err != nil {
		t.Fatalf("exposition problems: %v", err)
	}
	got := set.String()
	path := filepath.Join("testdata", "golden", "container-crio.prom")

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

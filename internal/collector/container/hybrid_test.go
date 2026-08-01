// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// hybridDir is a host with both hierarchies mounted: the twelve v1 controllers
// under a tmpfs /sys/fs/cgroup, and cgroup2 at /sys/fs/cgroup/unified. Captured
// 2026-08-01 from the same Rocky 8 box as docker-cgroupv1-20260801, put into
// hybrid mode on purpose.
//
// It exists because the hybrid branch shipped without ever having run. It had
// a bug (see TestHybridPrefersTheHierarchyHoldingTheContainers) that no
// v2-only or v1-only tree could expose.
const hybridDir = "testdata/docker-hybrid-20260801"

// TestHybridMountDetection is the unit under the bug.
//
// A cgroup2 mount existing somewhere is not the same as the cgroup root being
// cgroup2. On this host it is not: /sys/fs/cgroup is a tmpfs and the cgroup2
// mount is a directory inside it.
func TestHybridMountDetection(t *testing.T) {
	c := New(Options{Roots: fsroot.At(hybridDir)})
	v1, v2 := c.mountedVersions()

	if !v1 {
		t.Error("v1 not detected; the capture has twelve cgroup mounts")
	}
	if v2 {
		t.Error("v2 detected, but the cgroup2 mount is /sys/fs/cgroup/unified, " +
			"not the cgroup root — this is the mistake that made the v2 walker " +
			"read v1 files")
	}
}

// TestHybridPrefersTheHierarchyHoldingTheContainers is the regression test.
//
// Before the fix the v2 reader was chosen on this host, walked the v1 tree,
// matched the same container directory names, and then read v2 filenames that
// do not exist there. It did not fail: v1 and v2 spell a handful of fields
// identically — nr_periods, nr_throttled, inactive_file, shmem, unevictable —
// so those parsed, and everything else silently vanished. 27 series where the
// v1 reader produces 54, with no error and no missing container, which is the
// hardest kind of wrong to notice on a dashboard.
func TestHybridPrefersTheHierarchyHoldingTheContainers(t *testing.T) {
	set := newFixtureSet()
	if err := New(Options{Roots: fsroot.At(hybridDir)}).Collect(context.Background(), set); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	rendered := set.String()

	// The families that disappeared. Each is read from a v1 file whose name has
	// no v2 counterpart, which is exactly why the v2 reader lost them.
	for _, family := range []string{
		"prickle_container_cpu_usage_seconds_total",     // cpuacct.usage
		"prickle_container_cpu_throttled_seconds_total", // cpu.stat throttled_time
		"prickle_container_memory_usage_bytes",          // memory.usage_in_bytes
		"prickle_container_memory_anon_bytes",           // memory.stat rss
		"prickle_container_cpu_weight",                  // cpu.shares
	} {
		if !strings.Contains(rendered, family+"{") {
			t.Errorf("%s is missing; the v2 reader was chosen on a hybrid host again", family)
		}
	}

	if got := strings.Count(rendered, "prickle_container_info{"); got != 3 {
		t.Errorf("found %d containers, want 3", got)
	}
}

// TestHybridMatchesThePureV1Host: the same three containers on the same box,
// captured before and after the cgroup2 mount was added. Adding a hierarchy the
// containers are not in must not change what is reported about them, so the two
// trees must produce the same set of family names.
func TestHybridMatchesThePureV1Host(t *testing.T) {
	families := func(dir string) map[string]bool {
		set := newFixtureSet()
		if err := New(Options{Roots: fsroot.At(dir)}).Collect(context.Background(), set); err != nil {
			t.Fatalf("Collect(%s): %v", dir, err)
		}
		out := map[string]bool{}
		for _, line := range strings.Split(set.String(), "\n") {
			if name, _, ok := strings.Cut(line, "{"); ok && strings.HasPrefix(name, prefix) {
				out[name] = true
			}
		}
		return out
	}

	pure, hybrid := families(v1Dir), families(hybridDir)
	for name := range pure {
		// io_* depends on the containers having done I/O, which they had not
		// after the restart the hybrid capture followed. Everything else must
		// match.
		if strings.Contains(name, "_io_") {
			continue
		}
		if !hybrid[name] {
			t.Errorf("%s is reported on the pure v1 host but not on the hybrid one", name)
		}
	}
}

func TestPathHasSuffix(t *testing.T) {
	tests := []struct {
		path, suffix string
		want         bool
	}{
		{"/sys/fs/cgroup", "/sys/fs/cgroup", true},
		{"/tmp/fixture/sys/fs/cgroup", "/sys/fs/cgroup", true},
		// The hybrid case: the mount is below the root, not at it.
		{"/sys/fs/cgroup", "/sys/fs/cgroup/unified", false},
		// Implies a fixture prefix of /sys/fs, which this model permits. No
		// host mounts cgroup2 at /cgroup while rooting at /sys/fs/cgroup.
		{"/sys/fs/cgroup", "/cgroup", true},
		// A partial component is still not a match: "mycgroup" does not end
		// with "/cgroup".
		{"/var/lib/mycgroup", "/cgroup", false},
	}
	for _, tt := range tests {
		if got := pathHasSuffix(tt.path, tt.suffix); got != tt.want {
			t.Errorf("pathHasSuffix(%q, %q) = %v, want %v", tt.path, tt.suffix, got, tt.want)
		}
	}
}

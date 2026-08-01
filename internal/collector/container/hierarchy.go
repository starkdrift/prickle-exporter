// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"os"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// source reads one group of metrics for one container.
type source func(*exposition.Set, cgroup, []exposition.Label) error

// hierarchy is one cgroup layout: where containers live, and how to read them.
//
// SPEC.md §Hard constraints #4 requires v1 to be a separate implementation
// behind this interface rather than branches inside the v2 path. The two are
// different data models — one leaf directory per container against one
// directory per controller, `memory.current` against `memory.usage_in_bytes`,
// microseconds against nanoseconds — and interleaving them would put four
// conditionals in every reader to save one type.
//
// What the interface does *not* allow is a fork in the metrics contract. Both
// implementations emit the same names with the same units and the same labels;
// the hierarchy decides where a value is read from, never what it is called.
type hierarchy interface {
	// version is "v1" or "v2", for diagnose and for error messages.
	version() string
	// discover finds every container leaf in this hierarchy.
	discover(ctx context.Context) ([]cgroup, error)
	// sources are the per-container readers, in a fixed order so the rendered
	// document stays byte-stable.
	sources(devices map[string]string) []source
}

// hierarchies returns the layouts to try, in preference order.
//
// v2 first: SPEC.md §Hard constraints #4 calls it the primary hierarchy, and a
// host running both — a hybrid — has its containers in whichever one the
// runtime chose. Trying v2 and falling back when it finds nothing covers pure
// v2, pure v1 and hybrid without having to identify which is which, and
// without a flag an operator would have to know to set.
//
// A host with neither mounted yields an empty list, which produces no samples
// and no error. That is not a silent failure: `prickle diagnose` reports the
// hierarchy plainly, and a collector that errored on every scrape of a machine
// with no cgroups would be noise rather than news.
func (c *Collector) hierarchies() []hierarchy {
	v1Mounted, v2Mounted := c.mountedVersions()

	var hs []hierarchy
	if v2Mounted {
		hs = append(hs, v2Hierarchy{c})
	}
	if v1Mounted {
		hs = append(hs, v1Hierarchy{c})
	}
	if len(hs) == 0 {
		// No /proc/mounts to read, or nothing recognisable in it. Assume v2:
		// it is the default everywhere current, and a fixture tree that
		// captured cgroups without capturing mounts should still parse.
		hs = append(hs, v2Hierarchy{c})
	}
	return hs
}

// mountedVersions reads which hierarchies /proc/mounts advertises.
//
// From the mount table rather than a statfs magic number so it works against a
// captured fixture tree, which is the same reason `prickle diagnose` reads it
// that way.
//
// v2 is reported only when the cgroup2 mount **is** the configured cgroup root,
// not merely when one exists somewhere. That distinction is the whole of this
// function's difficulty, and getting it wrong is not a near miss: on a hybrid
// host systemd mounts cgroup2 at /sys/fs/cgroup/unified while /sys/fs/cgroup
// itself stays a tmpfs holding the v1 controllers. Answering "yes, v2" there
// sends the v2 walker into the v1 tree, where it matches the same container
// directory names and then reads v2 filenames that do not exist. Measured on a
// hybrid Rocky 8 host, that produced 27 series where the v1 reader produces 54
// — with no error, because the handful of fields v1 and v2 happen to spell
// identically parsed successfully. Silent partial data that looks healthy.
func (c *Collector) mountedVersions() (v1, v2 bool) {
	b, err := os.ReadFile(c.opts.Roots.ProcPath("mounts"))
	if err != nil {
		return false, false
	}
	root := c.opts.Roots.CgroupPath()
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		switch f[2] {
		case "cgroup2":
			// Compared as a path suffix because a fixture tree is the same
			// mount table rebased under a prefix: the file says
			// /sys/fs/cgroup, the configured root is <fixture>/sys/fs/cgroup.
			if pathHasSuffix(root, f[1]) {
				v2 = true
			}
		case "cgroup":
			v1 = true
		}
	}
	return v1, v2
}

// pathHasSuffix reports whether a configured root is the mount point from
// /proc/mounts, allowing for a fixture tree's prefix.
//
// A plain suffix test is the whole of it, and is already component-aligned: a
// mount point is absolute, so it begins with a separator, and any match
// therefore starts at one. There is no partial-component case to exclude.
//
// It cannot distinguish a root of /sys/fs/cgroup from a mount at /cgroup —
// that would imply a fixture prefix of /sys/fs, which is a shape this model
// permits. Nothing better is available without knowing the prefix, and a
// cgroup2 mount at /cgroup on a host whose root is /sys/fs/cgroup is not a
// configuration anyone has.
func pathHasSuffix(path, suffix string) bool {
	return path == suffix || strings.HasSuffix(path, suffix)
}

// v2Hierarchy is the unified hierarchy: one directory per container, holding
// every controller's files together.
type v2Hierarchy struct{ c *Collector }

func (h v2Hierarchy) version() string { return "v2" }

func (h v2Hierarchy) discover(ctx context.Context) ([]cgroup, error) {
	return h.c.discover(ctx)
}

func (h v2Hierarchy) sources(devices map[string]string) []source {
	return []source{
		h.c.collectCPU,
		h.c.collectMemory,
		h.c.collectIO(devices),
		h.c.collectPIDs,
		h.c.collectPressure,
	}
}

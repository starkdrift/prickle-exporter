// SPDX-License-Identifier: Apache-2.0

// Package fsroot centralises every filesystem prefix the exporter reads from.
//
// SPEC.md §Hard constraints #3: no collector may hardcode an absolute /proc,
// /sys or /sys/fs/cgroup path. Collectors take a Roots and build paths through
// it, so a test can point the whole exporter at a captured fixture tree. The
// rule is enforced by TestNoHardcodedRoots in internal/collector.
//
// Nothing here opens a file for writing. SPEC.md §Hard constraints #2: the
// exporter is strictly read-only.
package fsroot

import "path/filepath"

// Default filesystem prefixes on a live Linux host.
const (
	DefaultProc   = "/proc"
	DefaultSys    = "/sys"
	DefaultCgroup = "/sys/fs/cgroup"

	// DefaultPodLogs is where the kubelet — not the container runtime — creates
	// one directory per pod, named <namespace>_<pod>_<uid>. It is the only
	// place on a node that carries a pod's name without asking the API.
	DefaultPodLogs = "/var/log/pods"
)

// Roots holds the three prefixes every collector reads through.
//
// Cgroup is kept separate from Sys rather than derived as Sys + "/fs/cgroup":
// a captured fixture tree may carry one without the other, and on a live host
// the cgroup2 mount point is configurable independently of /sys.
type Roots struct {
	Proc   string
	Sys    string
	Cgroup string

	// PodLogs is read only when -collector.container.pod-names is on. It is
	// root-only on every node, which is why that flag defaults to off.
	PodLogs string
}

// Default returns the prefixes for a live host.
func Default() Roots {
	return Roots{
		Proc:    DefaultProc,
		Sys:     DefaultSys,
		Cgroup:  DefaultCgroup,
		PodLogs: DefaultPodLogs,
	}
}

// At returns Roots rebased under a single directory, laid out mirroring real
// paths: dir/proc, dir/sys, dir/sys/fs/cgroup. This is the shape
// scripts/capture-fixtures.sh produces, so tests can point straight at an
// unpacked capture.
func At(dir string) Roots {
	return Roots{
		Proc:    filepath.Join(dir, "proc"),
		Sys:     filepath.Join(dir, "sys"),
		Cgroup:  filepath.Join(dir, "sys", "fs", "cgroup"),
		PodLogs: filepath.Join(dir, "var", "log", "pods"),
	}
}

// ProcPath joins elem onto the /proc prefix.
func (r Roots) ProcPath(elem ...string) string {
	return join(r.Proc, DefaultProc, elem)
}

// SysPath joins elem onto the /sys prefix.
func (r Roots) SysPath(elem ...string) string {
	return join(r.Sys, DefaultSys, elem)
}

// CgroupPath joins elem onto the cgroup v2 prefix.
func (r Roots) CgroupPath(elem ...string) string {
	return join(r.Cgroup, DefaultCgroup, elem)
}

// PodLogsPath joins elem onto the kubelet pod-log prefix.
func (r Roots) PodLogsPath(elem ...string) string {
	return join(r.PodLogs, DefaultPodLogs, elem)
}

// join falls back to the live-host prefix when the field is empty, so a
// zero-value Roots behaves as Default() rather than resolving to a relative
// path against the process working directory.
func join(root, fallback string, elem []string) string {
	if root == "" {
		root = fallback
	}
	return filepath.Join(append([]string{root}, elem...)...)
}

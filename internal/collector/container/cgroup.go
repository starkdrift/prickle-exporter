// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// cgroup is one container's leaf cgroup: where it lives and who it is.
type cgroup struct {
	dir     string // absolute path to the cgroup directory
	id      string // container ID, as the runtime wrote it into the directory name
	pod     string // Kubernetes pod UID, empty outside kubepods
	runtime string // docker, containerd or crio
	qos     string // guaranteed, burstable or besteffort; empty outside kubepods
}

// labels returns the identity labels for this container's hot series.
//
// SPEC.md §Metrics contract fixes the closed set; `container` and `pod` are the
// two members a cgroup walk can fill. `namespace` is not derivable from the
// cgroup tree — the kernel only ever sees the pod's UID, never its name or
// namespace — so it is left off rather than filled with a guess.
//
// A fresh slice per call: the io collector appends a device label to it, and
// appending into a shared backing array would rewrite another container's
// labels.
func (c cgroup) labels() []exposition.Label {
	if c.pod == "" {
		return []exposition.Label{exposition.L("container", c.id)}
	}
	return []exposition.Label{
		exposition.L("container", c.id),
		exposition.L("pod", c.pod),
	}
}

// scopePrefixes maps the directory-name prefix each runtime writes to the
// runtime name reported on prickle_container_info.
//
// These are the shapes SPEC.md §Collectors lists. docker- and cri-containerd-
// are both present in the captured tree; crio- is not, and testdata/README.md
// records that gap rather than letting it pass unremarked.
var scopePrefixes = []struct{ prefix, runtime string }{
	{"docker-", "docker"},
	{"cri-containerd-", "containerd"},
	{"crio-", "crio"},
}

// podSlicePattern matches a systemd pod slice under kubepods.slice.
//
//	kubepods-besteffort-pod07fc7cef_656b_48a7_929d_2734c2b4498e.slice
//	kubepods-burstable-pod6eb5044d_ef2e_49d1_a9cc_28f4e3fe88a3.slice
//	kubepods-pod<uid>.slice                                       (guaranteed)
//
// The QoS component is absent for Guaranteed pods, which sit directly under
// kubepods.slice. The captured tree has besteffort and burstable pods; the
// Guaranteed shape is the same name minus that component and is handled by the
// same expression, but it is not fixture-covered (see testdata/README.md).
var podSlicePattern = regexp.MustCompile(`^kubepods-(?:(besteffort|burstable)-)?pod([0-9a-fA-F_-]+)\.slice$`)

// hexID matches a container ID as a runtime writes it into a directory name.
// Long enough that a stray directory called "docker-shim.scope" cannot pass.
var hexID = regexp.MustCompile(`^[0-9a-f]{12,}$`)

// podDirPattern matches a pod directory under the **cgroupfs** cgroup driver,
// where the kubelet writes plain directories instead of systemd slices:
//
//	kubepods/burstable/pod4d521664-aa00-4570-9841-ce67a3756762
//	kubepods/besteffort/pode7aa4094-2f07-4a8a-b4b1-fb1f38d6c2dd
//	kubepods/pod<uid>                                       (guaranteed)
//
// No `.slice` suffix and no systemd escaping — the UID is spelled the way
// Kubernetes spells it. The character class matches podSlicePattern's so the
// two drivers accept the same UIDs; an underscore cannot actually occur here,
// but rejecting one would be a difference without a reason.
var podDirPattern = regexp.MustCompile(`^pod([0-9a-fA-F_-]+)$`)

// discover walks the cgroup v2 tree and returns every container leaf, in the
// walk's lexical order so the rendered document is byte-stable.
//
// The walk does not descend below a match: a container that runs its own nested
// cgroups (Docker-in-Docker, a systemd init inside the container) would
// otherwise contribute a second set of series under the same identity.
//
// An unreadable subdirectory is skipped rather than failing the walk. On a live
// node the tree changes while it is being read — a container exiting mid-walk
// is normal, not an error.
func (c *Collector) discover(ctx context.Context) ([]cgroup, error) {
	root := c.opts.Roots.CgroupPath()
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			// No cgroup v2 mount. SPEC.md §Hard constraints #4.
			return nil, nil
		}
		return nil, err
	}

	var found []cgroup
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) || os.IsPermission(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cg, ok := identify(path, d.Name())
		if !ok {
			return nil
		}
		found = append(found, cg)
		return fs.SkipDir
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// identify decides whether a directory is a container leaf and, if so, who it
// belongs to.
//
// The container ID comes from this directory's own name and the pod identity
// from its parent's, which is the whole of what the cgroup tree knows: the
// kernel stores no container name, no image, and no pod name or namespace.
//
// Two layouts, because the kubelet's cgroup driver decides the shape of the
// whole tree and both are ordinary. See identifyScope and identifyPodChild.
func identify(path, name string) (cgroup, bool) {
	if cg, ok := identifyScope(path, name); ok {
		return cg, true
	}
	return identifyPodChild(path, name)
}

// identifyScope reads the systemd-driver layout, where the directory name
// carries the runtime and the container ID together:
//
//	kubepods.slice/kubepods-burstable-pod<uid>.slice/cri-containerd-<hex>.scope
//	system.slice/docker-<hex>.scope
func identifyScope(path, name string) (cgroup, bool) {
	scope, ok := strings.CutSuffix(name, ".scope")
	if !ok {
		return cgroup{}, false
	}
	for _, sp := range scopePrefixes {
		id, ok := strings.CutPrefix(scope, sp.prefix)
		if !ok || !hexID.MatchString(id) {
			continue
		}
		cg := cgroup{dir: path, id: id, runtime: sp.runtime}
		cg.pod, cg.qos = podIdentity(filepath.Base(filepath.Dir(path)))
		return cg, true
	}
	return cgroup{}, false
}

// identifyPodChild reads the **cgroupfs**-driver layout, where the container
// directory is a bare ID under a pod directory:
//
//	kubepods/burstable/pod<uid>/<hex>
//	kubepods/pod<uid>/<hex>        (guaranteed)
//
// This is what a managed Kubernetes cluster gives you by default, not an
// exotic configuration — testdata/doks-cgroupfs-20260801 is a capture of one.
// Until it was handled, such a node reported zero containers while running
// nine, because the .scope test above rejected every directory in the tree.
//
// A bare hex name is not on its own enough to call something a container: the
// parent must be a pod directory. That is what keeps this from matching some
// unrelated hex-named cgroup elsewhere in the tree.
//
// The runtime is left empty, deliberately. These names do not encode it — that
// is a property of the layout, not an oversight — so this tree cannot say
// whether containerd or CRI-O is underneath. An empty attribute on the _info
// gauge says "not known from here", which is true; naming a runtime would be a
// guess, and the collector already declines the same way over `namespace`.
func identifyPodChild(path, name string) (cgroup, bool) {
	if !hexID.MatchString(name) {
		return cgroup{}, false
	}
	parentPath := filepath.Dir(path)
	m := podDirPattern.FindStringSubmatch(filepath.Base(parentPath))
	if m == nil {
		return cgroup{}, false
	}
	return cgroup{
		dir: path,
		id:  name,
		pod: strings.ReplaceAll(m[1], "_", "-"),
		qos: qosFromDir(filepath.Base(filepath.Dir(parentPath))),
	}, true
}

// qosFromDir reads the QoS class from the level above a cgroupfs pod
// directory. Guaranteed pods have no QoS level of their own and sit directly
// under kubepods, exactly as they sit directly under kubepods.slice in the
// systemd layout.
//
// An unrecognised parent yields an empty class rather than a wrong one: the
// pod directory may have been reparented by a kubelet setting this does not
// model, and "" reads as unknown where "guaranteed" would read as a fact.
func qosFromDir(name string) string {
	switch name {
	case "burstable", "besteffort":
		return name
	case "kubepods":
		return "guaranteed"
	}
	return ""
}

// podIdentity reads a pod UID and QoS class out of the parent slice's name.
//
// systemd escapes a hyphen in a unit name as an underscore, because the hyphen
// is its own path separator for slices. The UID is unescaped back so the label
// carries the value `kubectl get pod -o jsonpath='{.metadata.uid}'` reports,
// rather than a systemd-internal spelling of it.
//
// The `pod` label therefore holds the pod's **UID**, not its name: the cgroup
// tree has no name to offer. Resolving one needs a Kubernetes-aware source,
// which the cgroup walk is not.
func podIdentity(parent string) (uid, qos string) {
	m := podSlicePattern.FindStringSubmatch(parent)
	if m == nil {
		return "", ""
	}
	qos = m[1]
	if qos == "" {
		// No QoS component: a Guaranteed pod, directly under kubepods.slice.
		qos = "guaranteed"
	}
	return strings.ReplaceAll(m[2], "_", "-"), qos
}

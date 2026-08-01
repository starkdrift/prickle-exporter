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

	// root and rel are set only on cgroup v1, where one container's files are
	// spread across a directory per controller — /sys/fs/cgroup/memory/…,
	// /sys/fs/cgroup/cpu,cpuacct/… — rather than gathered in one leaf. rel is
	// the path below a controller's root, which is identical across
	// controllers, so the two together locate any of them. Unused on v2, where
	// dir alone is the answer.
	root string
	rel  string
}

// ctrlPath builds the path to one file under one cgroup v1 controller.
//
// Callers pass the controller alternatives in preference order because the
// kernel co-mounts some of them: on RHEL 8 `cpu` and `cpuacct` are symlinks to
// a single `cpu,cpuacct` directory, while elsewhere they are separate mounts.
// The first that exists wins; the last is returned unconditionally so a caller
// that finds nothing reports a missing file rather than an empty path.
func (c cgroup) ctrlPath(file string, controllers ...string) string {
	var path string
	for _, ctrl := range controllers {
		path = filepath.Join(c.root, ctrl, c.rel, file)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return path
}

// labels returns the identity labels for this container's hot series.
//
// SPEC.md §Metrics contract fixes the closed set. `container` and `pod` are the
// two members a cgroup walk can fill on its own: the kernel only ever sees the
// pod's UID, never its name or namespace. `namespace` is passed in, resolved
// from the kubelet's pod log directory when
// -collector.container.pod-names is on, and omitted entirely when it is not —
// an empty label key on every series would be worse than no label at all.
//
// A fresh slice per call: the io collector appends a device label to it, and
// appending into a shared backing array would rewrite another container's
// labels.
func (c cgroup) labels(namespace string) []exposition.Label {
	if c.pod == "" {
		return []exposition.Label{exposition.L("container", c.id)}
	}
	out := []exposition.Label{
		exposition.L("container", c.id),
		exposition.L("pod", c.pod),
	}
	// Only when it is actually known. Adding an always-empty label key to
	// every container series would be a contract change that bought nothing,
	// and SPEC.md §Versioning counts adding a key to an existing series as a
	// major precisely because it breaks aggregations written without `by`.
	if namespace != "" {
		out = append(out, exposition.L("namespace", namespace))
	}
	return out
}

// scopePrefixes maps the directory-name prefix each runtime writes to the
// runtime name reported on prickle_container_info.
//
// These are the shapes SPEC.md §Collectors lists, and every one of them now has
// a captured tree behind it — see testdata/README.md.
//
// Both crio- and libpod- pair each container with a monitor scope,
// crio-conmon-<hex>.scope and libpod-conmon-<hex>.scope, which are not
// containers and must not be counted. Nothing here excludes them explicitly:
// stripping the prefix leaves "conmon-<hex>", and hexID rejects it because
// `conmon-` is not hex. That is a happy accident of two independent rules, so
// TestMonitorScopesAreNotContainers pins it rather than trusting it to hold.
//
// cri-containerd- and nerdctl- deliberately map to the same runtime name. They
// are one runtime reached two ways — through the CRI on a Kubernetes node, and
// through nerdctl on a plain host — and the label names the runtime, not the
// client that started the container.
var scopePrefixes = []struct{ prefix, runtime string }{
	{"docker-", "docker"},
	{"cri-containerd-", "containerd"},
	{"nerdctl-", "containerd"},
	{"crio-", "crio"},
	{"libpod-", "podman"},
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
			// No cgroup2 mount here. The v1 hierarchy is tried separately;
			// see hierarchy.go.
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
	return identifyBareID(path, name)
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

// dockerCgroupfsDir is where Docker puts containers when it is configured with
// the cgroupfs driver rather than systemd: /sys/fs/cgroup/docker/<hex>.
const dockerCgroupfsDir = "docker"

// identifyBareID reads the **cgroupfs**-driver layouts, where the container
// directory is a bare ID and its parent says what it belongs to:
//
//	docker/<hex>                   Docker with native.cgroupdriver=cgroupfs
//	kubepods/burstable/pod<uid>/<hex>
//	kubepods/pod<uid>/<hex>        (guaranteed)
//
// Neither is exotic — the Kubernetes one is what a managed cluster gives you by
// default. Until they were handled, such a host reported zero containers while
// running many, because the .scope test above rejected every directory.
//
// A bare hex name is never enough on its own: the parent has to identify it.
// That is what keeps this from matching an unrelated hex-named cgroup that some
// process happened to create elsewhere in the tree.
//
// The two differ in one respect worth keeping straight. Docker's parent
// directory names the runtime, so that case reports `docker`. The Kubernetes
// one does not — no part of `kubepods/burstable/pod<uid>/<hex>` says whether
// containerd or CRI-O is underneath — so it reports an empty runtime. That is a
// property of the layout, not a parse failure: an empty attribute on the _info
// gauge says "not known from here", where naming one would be a guess. The
// collector already declines the same way over `namespace`.
func identifyBareID(path, name string) (cgroup, bool) {
	if !hexID.MatchString(name) {
		return cgroup{}, false
	}
	parentPath := filepath.Dir(path)
	parent := filepath.Base(parentPath)

	if parent == dockerCgroupfsDir {
		return cgroup{dir: path, id: name, runtime: "docker"}, true
	}

	m := podDirPattern.FindStringSubmatch(parent)
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

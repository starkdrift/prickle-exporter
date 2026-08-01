// SPDX-License-Identifier: Apache-2.0

package container

import (
	"os"
	"strings"
)

// podMeta is what the kubelet knows about a pod that the cgroup tree does not.
type podMeta struct {
	namespace string
	name      string
}

// podNames maps pod UID to namespace and name by listing the kubelet's pod log
// directory (SPEC.md §Collectors).
//
// The kubelet creates /var/log/pods/<namespace>_<pod>_<uid>/<container>/ before
// it starts a container, and it does so on every CRI runtime because the
// kubelet and not the runtime owns that path — so this works identically under
// containerd and CRI-O. Nothing inside is read: the directory *names* carry
// everything, which keeps this to one readdir and keeps workload log content
// entirely out of reach.
//
// The UID here is the same one the cgroup walk already produces, so no other
// join is needed. containerd's per-container state directory carries the same
// facts and only for containerd, and the CRI socket would need gRPC and
// protobuf, which SPEC.md §Hard constraints #1 rules out.
//
// **This path is root:root 0750 on every node**, which is why the flag that
// reaches it is off by default. An unreadable directory is not an error: an
// operator who has not granted the privilege gets pod UIDs, exactly as before.
func (c *Collector) podNames() (map[string]podMeta, error) {
	if !c.opts.PodNames {
		return nil, nil
	}

	entries, err := os.ReadDir(c.opts.Roots.PodLogsPath())
	if err != nil {
		// Absent on a host that runs no kubelet; unreadable when the privilege
		// was not granted. Neither is a collection failure — the container
		// metrics are unaffected and only the names are missing — so this is
		// reported rather than returned as an error that would raise
		// prickle_collector_errors_total on every pass forever.
		return nil, skipMissing(err)
	}

	out := make(map[string]podMeta, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, uid, ok := parsePodLogDir(e.Name())
		if !ok {
			continue
		}
		out[uid] = meta
	}
	return out, nil
}

// parsePodLogDir splits <namespace>_<pod>_<uid>.
//
// Exactly two separators, and splitting is unambiguous because a namespace and
// a pod name are both RFC 1123 labels: lowercase alphanumerics and hyphens, no
// underscore possible. The UID is the only field that could vary in shape — a
// normal pod has a hyphenated UUID and a static pod's is bare hex — and neither
// contains an underscore either.
func parsePodLogDir(name string) (podMeta, string, bool) {
	parts := strings.Split(name, "_")
	if len(parts) != 3 {
		return podMeta{}, "", false
	}
	ns, pod, uid := parts[0], parts[1], parts[2]
	if ns == "" || pod == "" || uid == "" {
		return podMeta{}, "", false
	}
	return podMeta{namespace: ns, name: pod}, uid, true
}

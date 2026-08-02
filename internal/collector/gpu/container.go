// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"os"
	"strconv"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/collector/container"
	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// containerOfPID resolves a process to the container it runs in, or "" for one
// running directly on the host.
//
// This is the second and last place a PID is touched (the first is the exe
// symlink that names the command), and it dies here too: the return value is a
// container ID, and no caller ever sees the number. SPEC.md §Metrics contract
// forbids a PID as a label or a value, which is what makes a transient lookup
// key like this permissible — the same allowance `command` already depends on.
//
// The parse is delegated to the container collector rather than repeated.
// Runtime prefixes and the two cgroup-driver layouts have one definition
// between them; a second copy would drift, and would drift silently, giving GPU
// processes an empty container on exactly the hosts a new prefix was added for.
//
// A process that cannot be read — it exited between the NVML call and this one,
// or the exporter is not permitted to look — yields "", which reports as a
// process not in a container. That is the same shape as the honest answer and
// is deliberate: the alternative is dropping the series, which would lose the
// GPU memory reading as well as the attribution.
func containerOfPID(roots fsroot.Roots, pid uint32) string {
	body, err := os.ReadFile(roots.ProcPath(strconv.FormatUint(uint64(pid), 10), "cgroup"))
	if err != nil {
		return ""
	}
	return containerFromCgroupFile(string(body))
}

// containerFromCgroupFile reads the container ID out of a /proc/<pid>/cgroup
// body.
//
// Both hierarchies appear here and the file is ordered differently on each:
//
//	cgroup v2   0::/kubepods.slice/.../cri-containerd-<hex>.scope
//	cgroup v1   12:memory:/docker/<hex>
//
// Every line of a v1 file names the same container through a different
// controller, so the first line that resolves is as good as any. A v2 file has
// exactly one line. Scanning until something resolves handles both without
// asking which hierarchy this is, which the GPU collector has no other reason
// to know.
func containerFromCgroupFile(body string) string {
	for _, line := range strings.Split(body, "\n") {
		// hierarchy-ID:controller-list:cgroup-path — and the path may itself
		// contain colons, so split off exactly the first two fields.
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		if id := container.IDFromCgroupPath(parts[2]); id != "" {
			return id
		}
	}
	return ""
}

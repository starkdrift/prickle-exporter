// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// TestContainerFromCgroupFile covers both hierarchies, because the file is
// shaped differently on each and the GPU collector has no other reason to know
// which one it is looking at.
func TestContainerFromCgroupFile(t *testing.T) {
	const id = "1a54d33cd263e75821ac36425faead122e0f1982ce7e730b53d46508f8e4d56a"

	for _, tc := range []struct {
		name, body, want string
	}{
		{
			// cgroup v2: one line, no controller list.
			name: "v2 kubernetes systemd driver",
			body: "0::/kubepods.slice/kubepods-burstable-pod1234_5678.slice/cri-containerd-" + id + ".scope\n",
			want: id,
		},
		{
			name: "v2 kubernetes cgroupfs driver",
			body: "0::/kubepods/burstable/pod1234-5678/" + id + "\n",
			want: id,
		},
		{
			name: "v2 docker",
			body: "0::/system.slice/docker-" + id + ".scope\n",
			want: id,
		},
		{
			// cgroup v1: every line names the same container through a
			// different controller, so the first that resolves will do.
			name: "v1 many controllers",
			body: "12:pids:/docker/" + id + "\n" +
				"11:memory:/docker/" + id + "\n" +
				"10:cpu,cpuacct:/docker/" + id + "\n",
			want: id,
		},
		{
			// The controller field is absent on v2 and the path may itself
			// contain colons, so the split has to keep exactly two fields.
			name: "path containing a colon",
			body: "0::/system.slice/docker-" + id + ".scope\n",
			want: id,
		},
		{
			name: "host process, not in a container",
			body: "0::/user.slice/user-1000.slice/session-3.scope\n",
			want: "",
		},
		{
			name: "init",
			body: "0::/\n",
			want: "",
		},
		{
			// The monitor scope must not be mistaken for the container it
			// watches, or a GPU process would be attributed to a sibling that
			// is not running it.
			name: "podman conmon monitor",
			body: "0::/machine.slice/libpod-conmon-" + id + ".scope\n",
			want: "",
		},
		{
			name: "empty file",
			body: "",
			want: "",
		},
		{
			name: "malformed",
			body: "not a cgroup line at all\n",
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := containerFromCgroupFile(tc.body); got != tc.want {
				t.Errorf("containerFromCgroupFile() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestContainerOfMissingPIDIsEmpty pins the degraded case. A process that
// exited between the NVML call and this one, or one the exporter may not
// inspect, must yield "" rather than an error: the GPU memory reading is still
// worth reporting, and dropping the series would lose it along with the
// attribution.
func TestContainerOfMissingPIDIsEmpty(t *testing.T) {
	if got := containerOfPID(fsroot.At(t.TempDir()), 4294967294); got != "" {
		t.Errorf("containerOfPID(unreadable) = %q, want empty", got)
	}
}

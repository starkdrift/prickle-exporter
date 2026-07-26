// SPDX-License-Identifier: Apache-2.0

package fsroot

import "testing"

func TestDefault(t *testing.T) {
	r := Default()
	for _, tc := range []struct{ got, want string }{
		{r.ProcPath("stat"), "/proc/stat"},
		{r.ProcPath("net", "dev"), "/proc/net/dev"},
		{r.ProcPath("pressure", "cpu"), "/proc/pressure/cpu"},
		{r.SysPath("class", "drm"), "/sys/class/drm"},
		{r.CgroupPath("system.slice"), "/sys/fs/cgroup/system.slice"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestAt(t *testing.T) {
	r := At("testdata/capture")
	for _, tc := range []struct{ got, want string }{
		{r.ProcPath("stat"), "testdata/capture/proc/stat"},
		{r.SysPath("class"), "testdata/capture/sys/class"},
		{r.CgroupPath("system.slice"), "testdata/capture/sys/fs/cgroup/system.slice"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// TestZeroValueIsLive checks that an unset field resolves to the live-host
// prefix. Falling through to a relative path would silently read from the
// process working directory — an empty scrape rather than a loud failure.
func TestZeroValueIsLive(t *testing.T) {
	var r Roots
	for _, tc := range []struct{ got, want string }{
		{r.ProcPath("stat"), "/proc/stat"},
		{r.SysPath("class"), "/sys/class"},
		{r.CgroupPath("x"), "/sys/fs/cgroup/x"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// SPDX-License-Identifier: Apache-2.0

package container

import "testing"

func TestIDFromCgroupPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/kubepods.slice/kubepods-burstable-pod1234_5678.slice/cri-containerd-" + hex64 + ".scope", hex64},
		{"/system.slice/docker-" + hex64 + ".scope", hex64},
		{"/machine.slice/libpod-" + hex64 + ".scope", hex64},
		{"/docker/" + hex64, hex64},
		{"/kubepods/burstable/pod1234-5678/" + hex64, hex64},
		{"/kubepods/pod1234-5678/" + hex64, hex64},
		// Not containers.
		{"/machine.slice/libpod-conmon-" + hex64 + ".scope", ""},
		{"/user.slice/user-1000.slice", ""},
		{"/", ""},
		{"", ""},
		{"/kubepods.slice", ""},
	} {
		if got := IDFromCgroupPath(tc.in); got != tc.want {
			t.Errorf("IDFromCgroupPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

const hex64 = "1a54d33cd263e75821ac36425faead122e0f1982ce7e730b53d46508f8e4d56a"

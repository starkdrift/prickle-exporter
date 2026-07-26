// SPDX-License-Identifier: Apache-2.0

//go:build linux

package host

import "syscall"

// SyscallStatfs is the live-host Statfser: a direct statfs(2).
//
// It is a read-only syscall, permitted by SPEC.md §Hard constraints #2. It can
// block indefinitely on an unresponsive network mount, which is why the sampler
// runs collectors under a per-collector deadline and serves the previous render
// rather than waiting.
type SyscallStatfs struct{}

// Statfs implements Statfser.
func (SyscallStatfs) Statfs(path string) (FSStats, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSStats{}, err
	}
	return FSStats{
		BlockSize:   int64(st.Bsize),
		Blocks:      st.Blocks,
		BlocksFree:  st.Bfree,
		BlocksAvail: st.Bavail,
		Files:       st.Files,
		FilesFree:   st.Ffree,
	}, nil
}

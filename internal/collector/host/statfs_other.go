// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package host

import "errors"

// errNotLinux is returned by every Statfs call off Linux.
//
// prickle is a Linux exporter — SPEC.md §Hard constraints #4 alone (cgroup v2)
// settles that. This stub exists so the package still builds and its parser
// tests still run on a developer's macOS machine; it is never reached in a
// shipped binary.
var errNotLinux = errors.New("statfs is only implemented on linux")

// SyscallStatfs is the non-Linux stub Statfser.
type SyscallStatfs struct{}

// Statfs implements Statfser.
func (SyscallStatfs) Statfs(string) (FSStats, error) { return FSStats{}, errNotLinux }

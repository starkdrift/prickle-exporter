// SPDX-License-Identifier: Apache-2.0

//go:build !nvml

package gpu

import "fmt"

// NVMLBuilt reports whether this binary contains the NVML implementation.
// False here: this is the default CGO_ENABLED=0 static build.
const NVMLBuilt = false

// newNVMLSource always fails in the default build.
//
// SPEC.md §Collectors: dlopen needs cgo *and* a dynamically linked binary, and
// a fully static binary cannot dlopen at all. Rather than ship a code path that
// would fail at runtime in a way an operator has to decode, the default build
// says plainly that this artifact is not the one with NVML in it — `prickle`
// uses nvidia-smi, `prickle-nvml` uses NVML.
func newNVMLSource(Options) (nvidiaSource, error) {
	return nil, fmt.Errorf("%w: this binary was built without the nvml tag; "+
		"use the prickle-nvml artifact for the NVML path", ErrUnavailable)
}

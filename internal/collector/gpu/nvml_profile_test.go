// SPDX-License-Identifier: Apache-2.0

//go:build nvml

package gpu

import "testing"

// TestMIGProfileSpelling pins how the profile label is assembled, against the
// spellings nvidia-smi -L printed for the same instances.
//
// Unlike the rest of the nvml-tagged tests this one needs no hardware: it is
// the assembly, extracted, so `ci/check.sh` re-checks it on every run rather
// than only when someone has a GPU to hand. Every `want` below was read off
// `nvidia-smi -L` while the corresponding instance existed — on an H100
// (driver 580.173.02) for the live cases, and from the committed
// testdata/h200-mig-20260726 capture for the H200 one. None is what the rule
// "ought" to produce.
func TestMIGProfileSpelling(t *testing.T) {
	// Framebuffer sizes, which are *not* what the names are derived from. Both
	// are read off their captures: 9984 MiB and 40448 MiB on the H100, 16384
	// MiB on the H200 — whose profile is nonetheless called 1g.18gb.
	const (
		h100Small = 9984 << 20
		h100Large = 40448 << 20
		h200Small = 16384 << 20
	)

	for _, tc := range []struct {
		name                     string
		driverName               string
		gpuSlices, computeSlices uint64
		bytes                    uint64
		want                     string
	}{
		{"one slice", "1g.10gb", 1, 1, h100Small, "1g.10gb"},
		{"three slices", "3g.40gb", 3, 3, h100Large, "3g.40gb"},

		// The case that made the driver's name non-negotiable. Deriving from
		// the framebuffer gives "16" here, and nvidia-smi says 18.
		{"H200, name is not the framebuffer", "1g.18gb", 1, 1, h200Small, "1g.18gb"},

		// A subdivided GPU instance. The driver's compute-instance name is
		// already the whole spelling; adding the slice counts on top of it is
		// what produced "1c.1c.3g.40gb" on hardware.
		{"subdivided, named", "1c.3g.40gb", 3, 1, h100Large, "1c.3g.40gb"},

		// A GPU instance whose compute instance carries fewer engines. The
		// GPU-instance profile is "1g.10gb+me"; what -L prints, and what the
		// compute instance is called, is the plain name.
		{"media-engine instance", "1g.10gb", 1, 1, h100Small, "1g.10gb"},

		// Fallbacks, for a driver that declines the profile lookup. Coarse
		// rather than confidently wrong, and the hardware test fails on any
		// card where the result disagrees with nvidia-smi.
		{"no name, slice count only", "", 1, 1, h100Small, "1g.10gb"},
		{"no name, no slice counts", "", 0, 0, h100Small, "10gb"},
		{"no name, subdivided", "", 3, 1, h100Large, "1c.3g.40gb"},
		{"no name, undivided", "", 3, 3, h100Large, "3g.40gb"},

		// Nothing to report at all: no name and no memory means the instance
		// answered neither question, and a label invented from slice counts
		// alone would be a claim this never had.
		{"nothing known", "", 3, 1, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := migProfile(tc.driverName, tc.gpuSlices, tc.computeSlices, tc.bytes)
			if got != tc.want {
				t.Errorf("migProfile(%q, %d, %d, %d) = %q, want %q",
					tc.driverName, tc.gpuSlices, tc.computeSlices, tc.bytes, got, tc.want)
			}
		})
	}
}

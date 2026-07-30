// SPDX-License-Identifier: Apache-2.0

//go:build nvml

package gpu

import "testing"

// Framebuffer sizes, which are emphatically *not* what profile names are
// derived from. Every one was read off the card that carries that profile:
// H100 (driver 580.173.02) on 2026-07-29, H200 (same driver) on 2026-07-30.
//
// The H200 numbers are the reason this function asks the driver for a name
// instead of computing one. Not one of its profiles is named after its own
// framebuffer, and the gap is not a rounding artefact: 16.00 GiB is called
// 18gb, 32.50 GiB is called 35gb, 69.75 GiB is called 71gb. NVIDIA names the
// profile after a share of the card's *advertised* 141 GB, which NVML never
// reports.
const (
	h100Small = 9984 << 20  // 1g.10gb
	h100Large = 40448 << 20 // 3g.40gb
	h200Small = 16384 << 20 // 1g.18gb
	h200Mid   = 33280 << 20 // 1g.35gb
	h200Large = 71424 << 20 // 3g.71gb
)

// TestMIGProfileSpelling pins how the profile label is assembled, against the
// spellings nvidia-smi -L printed for the same instances.
//
// Unlike the rest of the nvml-tagged tests this one needs no hardware: it is
// the assembly, extracted, so `ci/check.sh` re-checks it on every run rather
// than only when someone has a GPU to hand. Every `want` below was read off
// `nvidia-smi -L` while the corresponding instance existed. None is what the
// rule "ought" to produce.
func TestMIGProfileSpelling(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		driverName               string
		gpuSlices, computeSlices uint64
		bytes                    uint64
		want                     string
	}{
		{"H100 one slice", "1g.10gb", 1, 1, h100Small, "1g.10gb"},
		{"H100 three slices", "3g.40gb", 3, 3, h100Large, "3g.40gb"},
		{"H200 one slice", "1g.18gb", 1, 1, h200Small, "1g.18gb"},
		{"H200 whole card", "7g.141gb", 7, 7, 150109880320, "7g.141gb"},

		// A subdivided GPU instance. The driver's compute-instance name is
		// already the whole spelling; adding the slice counts on top of it is
		// what produced "1c.1c.3g.40gb" on hardware.
		{"subdivided, named", "1c.3g.40gb", 3, 1, h100Large, "1c.3g.40gb"},

		// A GPU instance whose compute instance carries fewer engines. The
		// GPU-instance profile is "1g.10gb+me"; what -L prints, and what the
		// compute instance is called, is the plain name.
		{"media-engine instance", "1g.10gb", 1, 1, h100Small, "1g.10gb"},

		// The fallback, for a driver that declines the lookup. On the H100 it
		// happens to be right, which is exactly how it survived three rounds
		// of review.
		{"no name, H100", "", 1, 1, h100Small, "1g.10gb"},
		{"no name, no slice counts", "", 0, 0, h100Small, "10gb"},
		{"no name, subdivided", "", 3, 1, h100Large, "1c.3g.40gb"},

		// On the H200 the same fallback is wrong for every profile the card
		// offers. These three `want`s are not predictions: they were observed
		// on hardware, by building with the name lookup disabled and scraping
		// the card. They are kept as tests because they are the evidence for
		// why the lookup is not optional, and because a future edit that
		// "simplifies" the derivation back into the primary path will produce
		// exactly these strings.
		{"no name, H200 1g.18gb — wrong", "", 1, 1, h200Small, "1g.16gb"},
		{"no name, H200 1g.35gb — wrong", "", 1, 1, h200Mid, "1g.33gb"},
		{"no name, H200 3g.71gb — wrong", "", 3, 3, h200Large, "3g.70gb"},

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

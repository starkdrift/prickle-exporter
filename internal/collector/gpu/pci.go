// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// nvidiaVendorID is NVIDIA's PCI vendor ID, as sysfs spells it in
// bus/pci/devices/*/vendor.
const nvidiaVendorID = "0x10de"

// displayBaseClass is the PCI base class for display controllers. sysfs writes
// the full 24-bit class, so this is a prefix test rather than an equality one:
// 0x030000 is a VGA controller and 0x030200 a 3D controller, which is what a
// datacenter card with no display output reports.
const displayBaseClass = "0x03"

// CountNVIDIAGPUs reports how many NVIDIA display adapters the PCI bus
// advertises, whether or not a driver is bound to them.
//
// This is deliberately not a source of metrics, and nothing in a scrape calls
// it. It answers the one question `prickle diagnose` otherwise cannot: when
// neither NVML nor nvidia-smi is available, is that because the host has no
// NVIDIA card, or because it has one and the driver is missing? Those need
// opposite responses from an operator, and every other signal the exporter has
// — the dlopen failure, the absent binary — reads identically in both cases.
//
// Matching is on vendor *and* base class, because either alone is wrong.
// Vendor alone counts the HDMI audio function that consumer NVIDIA cards
// expose as a second card. Base class alone counts any display adapter: the
// fixture this was written against is a virtual machine whose console is a
// virtio VGA device at class 0x030000, sitting on the same bus as the H100.
func CountNVIDIAGPUs(roots fsroot.Roots) (int, error) {
	dir := roots.SysPath("bus", "pci", "devices")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("reading the PCI device list: %w", err)
	}

	count := 0
	for _, e := range entries {
		// A device whose attributes will not read is skipped rather than
		// failed on. The question here is "is a card present", and one
		// unreadable entry out of fourteen should not turn a confident no
		// into an error.
		if vendor, err := readPCIAttr(dir, e.Name(), "vendor"); err != nil || vendor != nvidiaVendorID {
			continue
		}
		class, err := readPCIAttr(dir, e.Name(), "class")
		if err != nil || !strings.HasPrefix(class, displayBaseClass) {
			continue
		}
		count++
	}
	return count, nil
}

// readPCIAttr reads one sysfs attribute of one PCI device.
//
// sysfs attributes report size 0, so they are read whole rather than stat'ed
// and streamed — the same reason scripts/capture-fixtures.sh uses `cat` and
// not `cp`.
func readPCIAttr(dir, device, attr string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, device, attr))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

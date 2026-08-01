// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// noDriverDir is a capture from a host with an H100 SXM5 on the bus and no
// NVIDIA driver installed at all — the case that motivated CountNVIDIAGPUs.
//
// Its value is what sits beside the H100: the machine's console is a virtio
// VGA adapter, so the tree contains a display-class device that is not NVIDIA
// and an NVIDIA device that is not VGA-class. A match on either attribute
// alone gets a different answer here than a match on both.
const noDriverDir = "testdata/h100-nodriver-20260801"

func TestCountNVIDIAGPUsOnADriverlessHost(t *testing.T) {
	n, err := CountNVIDIAGPUs(fsroot.At(noDriverDir))
	if err != nil {
		t.Fatalf("CountNVIDIAGPUs: %v", err)
	}
	if n != 1 {
		t.Errorf("counted %d NVIDIA GPUs, want 1", n)
	}
}

// TestCountNVIDIAGPUsIgnoresNonNVIDIADisplayAdapters pins the reason the class
// test is not the whole test. The capture's 0000:00:02.0 is a virtio VGA
// controller at class 0x030000 — a display adapter by every measure except the
// one that matters.
func TestCountNVIDIAGPUsIgnoresNonNVIDIADisplayAdapters(t *testing.T) {
	const virtioVGA = "0000:00:02.0"

	dir := filepath.Join(noDriverDir, "sys", "bus", "pci", "devices", virtioVGA)
	vendor, err := readPCIAttr(filepath.Dir(dir), virtioVGA, "vendor")
	if err != nil {
		t.Fatalf("reading the fixture's %s vendor: %v", virtioVGA, err)
	}
	class, err := readPCIAttr(filepath.Dir(dir), virtioVGA, "class")
	if err != nil {
		t.Fatalf("reading the fixture's %s class: %v", virtioVGA, err)
	}

	// If the capture ever stops containing this pairing, the test above stops
	// proving anything and this says so directly.
	if vendor == nvidiaVendorID {
		t.Fatalf("fixture device %s is NVIDIA (%s); it is meant to be the non-NVIDIA display adapter",
			virtioVGA, vendor)
	}
	if got, want := class[:len(displayBaseClass)], displayBaseClass; got != want {
		t.Fatalf("fixture device %s has class %s, want a display-class device to make this test meaningful",
			virtioVGA, class)
	}
}

// TestCountNVIDIAGPUsWithoutAPCIBus covers a container whose /sys is not
// mounted. Diagnose must say it could not tell, not that there is no card, so
// this has to surface as an error rather than a confident zero.
func TestCountNVIDIAGPUsWithoutAPCIBus(t *testing.T) {
	if _, err := CountNVIDIAGPUs(fsroot.At(t.TempDir())); err == nil {
		t.Fatal("CountNVIDIAGPUs succeeded with no PCI bus present; want an error")
	}
}

// TestCountNVIDIAGPUsOnAnEmptyBus is the other half: a bus that reads fine and
// holds no NVIDIA card is a confident zero, not an error.
func TestCountNVIDIAGPUsOnAnEmptyBus(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sys", "bus", "pci", "devices"), 0o755); err != nil {
		t.Fatal(err)
	}

	n, err := CountNVIDIAGPUs(fsroot.At(root))
	if err != nil {
		t.Fatalf("CountNVIDIAGPUs: %v", err)
	}
	if n != 0 {
		t.Errorf("counted %d NVIDIA GPUs on an empty bus, want 0", n)
	}
}

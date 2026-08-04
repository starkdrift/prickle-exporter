// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// AMD support, per SPEC.md §Collectors: "AMD via sysfs + DRM fdinfo".
//
// Nothing here spawns a process. SPEC.md §Hard constraints #2 permits exactly
// one subprocess, `nvidia-smi`, so `rocm-smi` and `amd-smi` are not sources
// however convenient they would be — they appear in the fixture tree as
// reference output to check this reader against, and are never executed. That
// restriction costs one thing, recorded on prickle_gpu_info below: sysfs
// publishes no marketing name for a card.
//
// Everything read here is a file, so unlike NVML this path is fully
// fixture-testable, and it is developed against
// testdata/mi300x-2gpu-20260804 — 2× MI300X under load, one process on the
// host and one in a container holding memory on both cards.

const (
	// amdDriver is what a card's uevent DRIVER= carries. This is the test for
	// "is this an AMD GPU", in preference to the vendor ID or the PCI class.
	//
	// The class would be wrong: an MI300X reports 0x120000, a processing
	// accelerator, and not the 0x03xx display class pci.go matches NVIDIA on —
	// so a card with no display output is invisible to a class test. The vendor
	// ID alone would be right here but says nothing about which driver is bound,
	// and this reader can only read a card that amdgpu is driving.
	amdDriver = "amdgpu"

	// Vendor values on prickle_gpu_info. A mixed host is the reason this label
	// exists: with `name` unavailable from AMD sysfs there would otherwise be
	// nothing on the series saying which stack a card was read through.
	vendorNVIDIA = "nvidia"
	vendorAMD    = "amd"
)

// amdMarketNames maps a uevent PCI_ID to the name the hardware calls itself.
//
// Deliberately tiny, and it grows only by capture. SPEC.md §Testing rules
// forbids developing against a layout nobody has captured, and the same
// standard is applied to a constant: the single entry here is verified against
// amd-smi's MARKET_NAME in
// testdata/mi300x-2gpu-20260804/amd/amd-smi-static.txt. Filling the table out
// from a public device-ID list would be a table of guesses, and a wrong `name`
// on an _info gauge is worse than an honest PCI ID.
//
// `VF` is not a typo: 0x74b5 is the MI300X SR-IOV *virtual function*, which is
// what a guest sees and what was captured. A bare-metal MI300X presents a
// different device ID and would need its own entry.
var amdMarketNames = map[string]string{
	"1002:74b5": "AMD Instinct MI300X VF",
}

// amdSource reads the AMD GPUs in the host from sysfs and DRM fdinfo.
//
// It holds nothing open and has no construction cost, so unlike the NVIDIA
// sources it is not selected once at startup: the card list is re-read on every
// scrape. That is a readdir and a handful of small sysfs reads, and it means a
// card whose driver loaded after the exporter started is picked up rather than
// missed until a restart.
type amdSource struct {
	roots fsroot.Roots
}

// amdCard is one card as sysfs names it, before it becomes a device.
type amdCard struct {
	node  string // "card0"
	index int    // 0, parsed from the node name
	pdev  string // "0000:fd:00.0", from uevent PCI_SLOT_NAME
	pciID string // "1002:74b5", from uevent PCI_ID, lowercased
	uuid  string // from unique_id
}

// read takes one pass over the AMD GPUs.
//
// A host with no AMD card yields an empty snapshot and no error — the common
// case, and not a failure. Errors from individual cards are collected rather
// than returned early, for the same reason the rest of this package renders a
// partial read: one card whose hwmon has gone missing should not cost the
// other seven their memory readings.
func (s *amdSource) read(perProcess bool) (snapshot, error) {
	cards, err := s.cards()
	if err != nil || len(cards) == 0 {
		return snapshot{}, err
	}

	var snap snapshot
	var errs []error
	for _, c := range cards {
		d, err := s.device(c)
		if err != nil {
			errs = append(errs, err)
		}
		snap.devices = append(snap.devices, d)
	}

	if perProcess {
		procs, err := s.processes(cards)
		if err != nil {
			errs = append(errs, err)
		}
		snap.processes = procs
	}
	return snap, errors.Join(errs...)
}

// cards enumerates the amdgpu-driven DRM cards, in node order.
//
// The render nodes (renderD*) under the same directory are deliberately not
// enumerated: a card and its render node are one GPU, and counting both would
// double every device series.
func (s *amdSource) cards() ([]amdCard, error) {
	dir := s.roots.SysPath("class", "drm")
	entries, err := os.ReadDir(dir)
	if err != nil {
		// No /sys/class/drm at all is a host without DRM, not an error worth
		// failing a scrape over.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the DRM class directory: %w", err)
	}

	var cards []amdCard
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "card") {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(name, "card"))
		if err != nil {
			continue
		}

		device := filepath.Join(dir, name, "device")
		uevent, err := parseUevent(filepath.Join(device, "uevent"))
		if err != nil || uevent["DRIVER"] != amdDriver {
			continue
		}

		// unique_id is the only stable per-card identity sysfs publishes, and it
		// matches amd-smi's ASIC_SERIAL. Everything else on the card is shared
		// with its siblings: the two MI300X captured have the same device ID,
		// subsystem ID and vbios version, and card0/card8 is enumeration order.
		//
		// Where it is absent — it is not guaranteed on every generation — the
		// PCI address stands in. That is stable for as long as the card stays in
		// its slot, which is weaker than a serial and is the honest best
		// available; the alternative is dropping a card from the scrape
		// entirely, which loses real readings to protect an identifier.
		uuid, err := readTrimmed(filepath.Join(device, "unique_id"))
		if err != nil || uuid == "" {
			uuid = uevent["PCI_SLOT_NAME"]
		}
		if uuid == "" {
			continue
		}

		cards = append(cards, amdCard{
			node:  name,
			index: index,
			pdev:  uevent["PCI_SLOT_NAME"],
			pciID: strings.ToLower(uevent["PCI_ID"]),
			uuid:  uuid,
		})
	}

	// ReadDir is lexical, which orders card10 before card8. Sorting on the
	// parsed number keeps the emitted order matching the driver's.
	sort.Slice(cards, func(i, j int) bool { return cards[i].index < cards[j].index })
	return cards, nil
}

// device reads one card's metrics.
func (s *amdSource) device(c amdCard) (device, error) {
	dir := s.roots.SysPath("class", "drm", c.node, "device")
	var errs []error

	d := device{
		Index:  c.index,
		UUID:   c.uuid,
		Vendor: vendorAMD,
		Name:   amdMarketNames[c.pciID],
	}
	// No table entry and no name in sysfs, so the PCI ID is what is left. It is
	// at least resolvable by whoever reads the dashboard, which an empty string
	// is not.
	if d.Name == "" {
		d.Name = c.pciID
	}

	// gpu_busy_percent is whole percent, 0 to 100, and is the same quantity
	// NVIDIA reports as utilization.gpu. Unlike NVIDIA it has no "unavailable"
	// spelling — there is no AMD equivalent of [N/A] under MIG — so a card whose
	// file reads is always HasUtilization.
	if busy, err := readUint(filepath.Join(dir, "gpu_busy_percent")); err != nil {
		errs = append(errs, err)
	} else {
		d.Utilization = float64(busy) / 100
		d.HasUtilization = true
	}

	// Both are plain byte counts, not the MiB nvidia-smi's nounits reports.
	if used, err := readUint(filepath.Join(dir, "mem_info_vram_used")); err != nil {
		errs = append(errs, err)
	} else {
		d.MemoryUsedBytes = used
	}
	if total, err := readUint(filepath.Join(dir, "mem_info_vram_total")); err != nil {
		errs = append(errs, err)
	} else {
		d.MemoryTotalBytes = total
	}

	d.ComputePartition, _ = readTrimmed(filepath.Join(dir, "current_compute_partition"))
	d.MemoryPartition, _ = readTrimmed(filepath.Join(dir, "current_memory_partition"))

	if err := s.readHwmon(dir, &d); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return d, fmt.Errorf("%s: %w", c.node, errors.Join(errs...))
	}
	return d, nil
}

// amdTempPreference is which hwmon temperature stands in for "the GPU's
// temperature", most analogous to NVIDIA's temperature.gpu first.
//
// The sensors are named by sibling *_label files rather than numbered
// predictably, which is the whole reason this is a search and not a read of
// temp1_input: an MI300X has no temp1 at all. It publishes temp2 (junction) and
// temp3 (mem); a consumer Radeon publishes temp1 (edge). Preferring edge and
// falling back to junction reports the same thing NVIDIA does wherever the card
// offers it, and the hotspot — which is what rocm-smi shows — where it does not.
var amdTempPreference = []string{"edge", "junction", "mem"}

// readHwmon fills in temperature and power from the card's hwmon directory.
//
// Both are absent rather than zero when the card does not publish them, for the
// same reason utilization is absent under MIG: a zero reads as a cold, idle card.
func (s *amdSource) readHwmon(deviceDir string, d *device) error {
	matches, err := filepath.Glob(filepath.Join(deviceDir, "hwmon", "hwmon*"))
	if err != nil || len(matches) == 0 {
		// A card with no hwmon still reports memory and utilization.
		return nil
	}
	dir := matches[0]

	labels, err := hwmonLabels(dir)
	if err != nil {
		return err
	}

	for _, want := range amdTempPreference {
		sensor, ok := labels[want]
		if !ok || !strings.HasPrefix(sensor, "temp") {
			continue
		}
		// Millidegrees Celsius.
		if milli, err := readUint(filepath.Join(dir, sensor+"_input")); err == nil {
			d.TemperatureC = float64(milli) / 1000
			d.HasTemperature = true
			break
		}
	}

	// power1_input is the instantaneous draw and power1_average the averaged
	// one; a card publishes one or the other and the MI300X publishes _input.
	// Microwatts in both spellings.
	for _, file := range []string{"power1_input", "power1_average"} {
		micro, err := readUint(filepath.Join(dir, file))
		if err != nil {
			continue
		}
		d.PowerWatts = float64(micro) / 1e6
		d.HasPower = true
		break
	}
	return nil
}

// hwmonLabels maps a sensor's label to its file prefix — "junction" to "temp2".
func hwmonLabels(dir string) (map[string]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*_label"))
	if err != nil {
		return nil, err
	}
	labels := make(map[string]string, len(matches))
	for _, path := range matches {
		value, err := readTrimmed(path)
		if err != nil || value == "" {
			continue
		}
		prefix := strings.TrimSuffix(filepath.Base(path), "_label")
		// First one wins: two sensors sharing a label would otherwise depend on
		// glob order, and the lower-numbered sensor is the card's primary.
		if _, seen := labels[value]; !seen {
			labels[value] = prefix
		}
	}
	return labels, nil
}

// processes attributes per-process VRAM through DRM fdinfo.
//
// This is the AMD half of what NVML and nvidia-smi do for NVIDIA, and it works
// differently in one way that matters: fdinfo identifies a GPU by `drm-pdev`,
// its PCI address, and carries no UUID at all. So the card list is indexed by
// address and every process is joined back through it.
//
// The PID is touched here exactly as it is elsewhere in this package — to read
// the process's own exe symlink and cgroup — and dies at this boundary. No
// caller sees it, which is what makes SPEC.md §Metrics contract's "PID never
// appears" structural rather than a promise.
func (s *amdSource) processes(cards []amdCard) ([]process, error) {
	byPdev := make(map[string]string, len(cards))
	for _, c := range cards {
		if c.pdev != "" {
			byPdev[c.pdev] = c.uuid
		}
	}
	if len(byPdev) == 0 {
		return nil, nil
	}

	entries, err := os.ReadDir(s.roots.ProcPath())
	if err != nil {
		return nil, fmt.Errorf("reading /proc: %w", err)
	}

	var procs []process
	for _, e := range entries {
		pid, err := strconv.ParseUint(e.Name(), 10, 32)
		if err != nil {
			continue
		}
		procs = append(procs, s.processFDs(uint32(pid), byPdev)...)
	}
	return procs, nil
}

// processFDs reads one process's DRM fdinfo files.
//
// A process holding fds on several cards produces one entry per card, which is
// what the fixture's workload does: it allocates on both GPUs and shows up
// under both. Two fds onto the *same* card are summed instead — each is a
// separate amdgpu client with its own allocation, so they are two contributions
// to one total rather than one number counted twice.
func (s *amdSource) processFDs(pid uint32, byPdev map[string]string) []process {
	pidStr := strconv.FormatUint(uint64(pid), 10)
	dir := s.roots.ProcPath(pidStr, "fdinfo")
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Almost always a process that exited between the readdir and here, or
		// one belonging to another user. Neither is worth an error.
		return nil
	}

	// Resolved lazily: most processes on a host hold no GPU fd, and neither
	// lookup should be paid for them.
	var command, container string
	resolved := false

	byUUID := make(map[string]uint64)
	var order []string

	for _, e := range entries {
		fields, err := parseFdinfo(filepath.Join(dir, e.Name()))
		if err != nil || fields["drm-driver"] != amdDriver {
			continue
		}
		uuid, ok := byPdev[fields["drm-pdev"]]
		if !ok {
			continue
		}

		// drm-total-vram is what the process has allocated in VRAM, which is
		// the quantity nvidia-smi reports as used_gpu_memory. drm-resident-vram
		// would be what is physically resident right now — a different and
		// smaller question once a card starts evicting, and not the one the
		// NVIDIA sources answer.
		bytes, ok := parseDRMSize(fields["drm-total-vram"])
		if !ok {
			continue
		}

		if !resolved {
			command, container = s.identify(pidStr)
			resolved = true
		}
		if command == "" {
			return nil
		}
		if _, seen := byUUID[uuid]; !seen {
			order = append(order, uuid)
		}
		byUUID[uuid] += bytes
	}

	procs := make([]process, 0, len(order))
	for _, uuid := range order {
		procs = append(procs, process{
			GPUUUID:     uuid,
			Command:     command,
			Container:   container,
			MemoryBytes: byUUID[uuid],
		})
	}
	return procs
}

// identify resolves a PID to the command that names it and the container it
// runs in. Both are the same lookups the NVIDIA sources make, and both are
// deliberately keyed on the process's own files rather than on anything the
// GPU driver reports.
func (s *amdSource) identify(pid string) (command, container string) {
	// SPEC.md §Metrics contract: the basename of the exe symlink, never comm,
	// which is truncated to 15 characters and forgeable. An unreadable link
	// means the process is gone or not ours, and the caller drops it — a series
	// labelled with a command nobody can name is not worth the cardinality.
	target, err := os.Readlink(s.roots.ProcPath(pid, "exe"))
	if err != nil {
		return "", ""
	}
	// The kernel appends " (deleted)" once the binary is unlinked underneath a
	// still-running process, which is ordinary during an upgrade.
	command = filepath.Base(strings.TrimSuffix(target, " (deleted)"))

	body, err := os.ReadFile(s.roots.ProcPath(pid, "cgroup"))
	if err != nil {
		// Empty is the honest answer and the same shape as "not in a container".
		return command, ""
	}
	return command, containerFromCgroupFile(string(body))
}

// parseUevent reads a sysfs uevent file into its KEY=VALUE pairs.
func parseUevent(path string) (map[string]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fields := make(map[string]string, 8)
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			fields[key] = value
		}
	}
	return fields, nil
}

// parseFdinfo reads a /proc/<pid>/fdinfo/<fd> file into its key:value pairs.
//
// The DRM keys are tab-separated after the colon and the plain kernel ones
// ("pos", "flags") are not, so the value is trimmed rather than split on a
// fixed separator.
func parseFdinfo(path string) (map[string]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fields := make(map[string]string, 48)
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return fields, nil
}

// parseDRMSize converts a DRM fdinfo memory value to bytes.
//
// The kernel writes these as a number and a unit — "1196292 KiB" — and the
// unit is part of the documented format rather than incidental, so it is read
// rather than assumed. A bare number is bytes.
func parseDRMSize(value string) (uint64, bool) {
	if value == "" {
		return 0, false
	}
	number, unit, _ := strings.Cut(value, " ")
	n, err := strconv.ParseUint(strings.TrimSpace(number), 10, 64)
	if err != nil {
		return 0, false
	}

	switch strings.TrimSpace(unit) {
	case "":
		return n, true
	case "KiB":
		return n * 1024, true
	case "MiB":
		return n * 1024 * 1024, true
	case "GiB":
		return n * 1024 * 1024 * 1024, true
	default:
		// An unrecognised unit is dropped rather than guessed at: a wrong
		// multiplier is a wrong metric, and there is no safe default.
		return 0, false
	}
}

// readTrimmed reads a sysfs attribute whole.
//
// sysfs files report size 0, so they are read entire rather than stat'ed and
// streamed — the same reason scripts/capture-fixtures.sh uses `cat` and not `cp`.
func readTrimmed(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

// readUint reads a sysfs attribute holding a single unsigned number.
func readUint(path string) (uint64, error) {
	text, err := readTrimmed(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", path, err)
	}
	return n, nil
}

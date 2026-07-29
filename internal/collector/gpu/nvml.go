// SPDX-License-Identifier: Apache-2.0

//go:build nvml

// This file is the NVML implementation of nvidiaSource, and the only cgo in
// the tree. It is built solely into the prickle-nvml artifact (SPEC.md
// §Distribution); the default CGO_ENABLED=0 binary gets nvml_disabled.go and
// cannot contain any of this.
//
// # Why dlopen rather than linking
//
// SPEC.md §Hard constraints #1: zero third-party dependencies. Linking
// libnvidia-ml at build time would make the NVIDIA driver a build dependency
// and the binary unrunnable without it. dlopen keeps the dependency at runtime
// and optional — which is also what lets one artifact serve hosts with and
// without a driver. It does require a dynamically linked binary: a fully static
// one cannot dlopen at all, which is why this is a second artifact rather than
// a build flag on the first.
//
// # Why only versioned symbols
//
// Every entry point resolved below carries NVIDIA's _v2/_v3 suffix where one
// exists. Those suffixes are the ABI contract: NVIDIA introduces a new suffix
// precisely when a signature or a struct layout changes, and never mutates one
// already published. Binding to nvmlDeviceGetComputeRunningProcesses_v3 rather
// than to the unsuffixed alias is what makes the struct layouts declared here
// safe against a driver update. An unsuffixed symbol is resolved only as a
// fallback for the handful that have never been versioned.
//
// # Read-only
//
// SPEC.md §Hard constraints #2 forbids any NVML call that mutates device
// state. Every symbol here is a Get: no nvmlDeviceSetMigMode, no clock or
// persistence changes, no ECC toggles, nothing from the nvmlDeviceSet* or
// nvmlDeviceClear* families. nvmlInit_v2 and nvmlShutdown manage this
// process's own handle to the library and touch no device.
//
// # Verification
//
// SPEC.md §Testing rules: this path cannot be fixture-tested, because it is a
// C call rather than a file read. It is exercised on hardware, against the
// captured nvidia-smi fixtures as the reference for what it must report.
package gpu

/*
#cgo LDFLAGS: -ldl

#include <dlfcn.h>
#include <stdlib.h>
#include <string.h>

// --- NVML types, from nvml.h ------------------------------------------------
//
// Only the shapes the versioned entry points below take. Each is annotated
// with the symbol that fixes its layout.

typedef int nvmlReturn_t;
typedef void *nvmlDevice_t;

// nvmlUtilization_t, as taken by nvmlDeviceGetUtilizationRates.
typedef struct {
  unsigned int gpu;
  unsigned int memory;
} prickle_utilization_t;

// nvmlMemory_t, as taken by nvmlDeviceGetMemoryInfo. The _v2 variant of that
// call uses a larger struct with a version field; this binds the original,
// whose three-field layout has never changed.
typedef struct {
  unsigned long long total;
  unsigned long long free;
  unsigned long long used;
} prickle_memory_t;

// nvmlProcessInfo_t, as taken by nvmlDeviceGetComputeRunningProcesses_v3.
// The _v3 suffix is what fixes these four fields in this order.
typedef struct {
  unsigned int pid;
  unsigned long long usedGpuMemory;
  unsigned int gpuInstanceId;
  unsigned int computeInstanceId;
} prickle_process_t;

// --- Resolved entry points --------------------------------------------------

static void *nvml_handle;

static nvmlReturn_t (*p_init)(void);
static nvmlReturn_t (*p_shutdown)(void);
static const char *(*p_error_string)(nvmlReturn_t);
static nvmlReturn_t (*p_driver_version)(char *, unsigned int);
static nvmlReturn_t (*p_device_count)(unsigned int *);
static nvmlReturn_t (*p_device_by_index)(unsigned int, nvmlDevice_t *);
static nvmlReturn_t (*p_device_uuid)(nvmlDevice_t, char *, unsigned int);
static nvmlReturn_t (*p_device_name)(nvmlDevice_t, char *, unsigned int);
static nvmlReturn_t (*p_device_utilization)(nvmlDevice_t, prickle_utilization_t *);
static nvmlReturn_t (*p_device_memory)(nvmlDevice_t, prickle_memory_t *);
static nvmlReturn_t (*p_device_temperature)(nvmlDevice_t, int, unsigned int *);
static nvmlReturn_t (*p_device_power)(nvmlDevice_t, unsigned int *);
static nvmlReturn_t (*p_device_mig_mode)(nvmlDevice_t, unsigned int *, unsigned int *);
static nvmlReturn_t (*p_device_max_mig_count)(nvmlDevice_t, unsigned int *);
static nvmlReturn_t (*p_device_mig_by_index)(nvmlDevice_t, unsigned int, nvmlDevice_t *);
static nvmlReturn_t (*p_device_compute_procs)(nvmlDevice_t, unsigned int *, prickle_process_t *);

// prickle_sym resolves name, then name without its version suffix. Returns
// NULL when neither exists.
static void *prickle_sym(const char *versioned, const char *plain) {
  void *fn = dlsym(nvml_handle, versioned);
  if (fn == NULL && plain != NULL) fn = dlsym(nvml_handle, plain);
  return fn;
}

// prickle_open dlopens the library and resolves every entry point. Returns 0
// on success, or a negative code identifying what was missing.
static int prickle_open(void) {
  if (nvml_handle != NULL) return 0;

  nvml_handle = dlopen("libnvidia-ml.so.1", RTLD_LAZY | RTLD_LOCAL);
  if (nvml_handle == NULL) return -1;

  p_init            = prickle_sym("nvmlInit_v2", "nvmlInit");
  p_shutdown        = prickle_sym("nvmlShutdown", NULL);
  p_error_string    = prickle_sym("nvmlErrorString", NULL);
  p_driver_version  = prickle_sym("nvmlSystemGetDriverVersion", NULL);
  p_device_count    = prickle_sym("nvmlDeviceGetCount_v2", "nvmlDeviceGetCount");
  p_device_by_index = prickle_sym("nvmlDeviceGetHandleByIndex_v2", "nvmlDeviceGetHandleByIndex");
  p_device_uuid     = prickle_sym("nvmlDeviceGetUUID", NULL);
  p_device_name     = prickle_sym("nvmlDeviceGetName", NULL);
  p_device_utilization = prickle_sym("nvmlDeviceGetUtilizationRates", NULL);
  p_device_memory   = prickle_sym("nvmlDeviceGetMemoryInfo", NULL);
  p_device_temperature = prickle_sym("nvmlDeviceGetTemperature", NULL);
  p_device_power    = prickle_sym("nvmlDeviceGetPowerUsage", NULL);
  p_device_mig_mode = prickle_sym("nvmlDeviceGetMigMode", NULL);
  p_device_max_mig_count = prickle_sym("nvmlDeviceGetMaxMigDeviceCount", NULL);
  p_device_mig_by_index  = prickle_sym("nvmlDeviceGetMigDeviceHandleByIndex", NULL);
  p_device_compute_procs = prickle_sym("nvmlDeviceGetComputeRunningProcesses_v3", NULL);

  // The minimum set without which there is nothing to report. The optional
  // ones (MIG, temperature, power) are checked at their call sites, so a
  // consumer card missing MIG entry points still reports memory.
  if (p_init == NULL || p_shutdown == NULL || p_device_count == NULL ||
      p_device_by_index == NULL || p_device_uuid == NULL ||
      p_device_memory == NULL) {
    dlclose(nvml_handle);
    nvml_handle = NULL;
    return -2;
  }
  return 0;
}

static void prickle_close(void) {
  if (nvml_handle == NULL) return;
  if (p_shutdown != NULL) p_shutdown();
  dlclose(nvml_handle);
  nvml_handle = NULL;
}

static const char *prickle_dlerror(void) { return dlerror(); }

static nvmlReturn_t prickle_init(void) { return p_init(); }

static const char *prickle_error_string(nvmlReturn_t r) {
  if (p_error_string == NULL) return "unknown NVML error";
  return p_error_string(r);
}

static nvmlReturn_t prickle_driver_version(char *buf, unsigned int len) {
  if (p_driver_version == NULL) return -1;
  return p_driver_version(buf, len);
}

static nvmlReturn_t prickle_device_count(unsigned int *n) { return p_device_count(n); }

static nvmlReturn_t prickle_device_by_index(unsigned int i, nvmlDevice_t *d) {
  return p_device_by_index(i, d);
}

static nvmlReturn_t prickle_device_uuid(nvmlDevice_t d, char *buf, unsigned int len) {
  return p_device_uuid(d, buf, len);
}

static nvmlReturn_t prickle_device_name(nvmlDevice_t d, char *buf, unsigned int len) {
  if (p_device_name == NULL) { buf[0] = '\0'; return 0; }
  return p_device_name(d, buf, len);
}

static nvmlReturn_t prickle_device_utilization(nvmlDevice_t d, unsigned int *gpu) {
  if (p_device_utilization == NULL) return -1;
  prickle_utilization_t u;
  memset(&u, 0, sizeof u);
  nvmlReturn_t r = p_device_utilization(d, &u);
  if (r == 0) *gpu = u.gpu;
  return r;
}

static nvmlReturn_t prickle_device_memory(nvmlDevice_t d, unsigned long long *used,
                                          unsigned long long *total) {
  prickle_memory_t m;
  memset(&m, 0, sizeof m);
  nvmlReturn_t r = p_device_memory(d, &m);
  if (r == 0) { *used = m.used; *total = m.total; }
  return r;
}

// Sensor 0 is NVML_TEMPERATURE_GPU, the only member of that enum.
static nvmlReturn_t prickle_device_temperature(nvmlDevice_t d, unsigned int *c) {
  if (p_device_temperature == NULL) return -1;
  return p_device_temperature(d, 0, c);
}

static nvmlReturn_t prickle_device_power_milliwatts(nvmlDevice_t d, unsigned int *mw) {
  if (p_device_power == NULL) return -1;
  return p_device_power(d, mw);
}

// Reports 1 when MIG is currently enabled. Query only: the pending-mode
// out-parameter is read and ignored, and nothing is set.
static nvmlReturn_t prickle_device_mig_enabled(nvmlDevice_t d, unsigned int *enabled) {
  if (p_device_mig_mode == NULL) return -1;
  unsigned int current = 0, pending = 0;
  nvmlReturn_t r = p_device_mig_mode(d, &current, &pending);
  if (r == 0) *enabled = (current == 1) ? 1 : 0;
  return r;
}

static nvmlReturn_t prickle_device_max_mig_count(nvmlDevice_t d, unsigned int *n) {
  if (p_device_max_mig_count == NULL) return -1;
  return p_device_max_mig_count(d, n);
}

static nvmlReturn_t prickle_device_mig_by_index(nvmlDevice_t d, unsigned int i, nvmlDevice_t *m) {
  if (p_device_mig_by_index == NULL) return -1;
  return p_device_mig_by_index(d, i, m);
}

// Two-call convention: pass count=0 to learn the length, then call again with
// a buffer. The caller sizes the buffer from the first call's answer.
static nvmlReturn_t prickle_device_compute_proc_count(nvmlDevice_t d, unsigned int *n) {
  if (p_device_compute_procs == NULL) return -1;
  *n = 0;
  return p_device_compute_procs(d, n, NULL);
}

static nvmlReturn_t prickle_device_compute_procs(nvmlDevice_t d, unsigned int *n,
                                                 prickle_process_t *buf) {
  if (p_device_compute_procs == NULL) return -1;
  return p_device_compute_procs(d, n, buf);
}

static unsigned long long prickle_proc_memory(prickle_process_t *buf, unsigned int i) {
  return buf[i].usedGpuMemory;
}

// The PID is read for exactly one purpose: resolving /proc/<pid>/exe to a
// command name, which SPEC.md §Metrics contract names as the source of the
// `command` label. It is consumed by that lookup and never returned upward.
static unsigned int prickle_proc_pid(prickle_process_t *buf, unsigned int i) {
  return buf[i].pid;
}
*/
import "C"

import (
	"context"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// NVMLBuilt reports whether this binary contains the NVML implementation.
const NVMLBuilt = true

// Buffer sizes from nvml.h's NVML_DEVICE_UUID_V2_BUFFER_SIZE and friends,
// rounded up. NVML truncates rather than overflowing, so generous is safe.
const (
	uuidBufferSize   = 96
	nameBufferSize   = 96
	driverBufferSize = 96
)

// nvmlNotSupported is NVML_ERROR_NOT_SUPPORTED. A consumer card returns it for
// MIG, and a passively cooled one for power, and neither is a failure worth
// reporting — the sample is simply absent.
const nvmlNotSupported = 3

// nvmlMemoryUnavailable is what nvmlDeviceGetComputeRunningProcesses reports
// for a process whose memory cannot be attributed.
const nvmlMemoryUnavailable = ^C.ulonglong(0)

// nvmlSource reads the devices through libnvidia-ml.so.1.
//
// The library is process-global: nvmlInit_v2 and nvmlShutdown act on a
// per-process handle, so exactly one source may own it. openOnce enforces that,
// and the mutex serialises reads because NVML's own thread safety varies by
// entry point and the sampler gives us no reason to need concurrency here.
type nvmlSource struct {
	mu     sync.Mutex
	closed bool

	// roots resolves /proc for the exe-symlink lookup that names a process.
	// SPEC.md §Hard constraints #3: no absolute /proc literal, here either.
	roots fsroot.Roots
}

var (
	openOnce   sync.Once
	openErr    error
	openSource *nvmlSource
)

// newNVMLSource loads the library and initialises NVML, once per process.
//
// SPEC.md §Collectors: attempted once at startup and fallen back from silently.
// The error is descriptive because `prickle diagnose` reports it when asked why
// NVML did not load — "cannot open shared object file" and "driver/library
// version mismatch" need different fixes.
func newNVMLSource(opts Options) (nvidiaSource, error) {
	openOnce.Do(func() {
		if rc := C.prickle_open(); rc != 0 {
			switch rc {
			case -1:
				openErr = fmt.Errorf("%w: dlopen libnvidia-ml.so.1: %s",
					ErrUnavailable, C.GoString(C.prickle_dlerror()))
			default:
				openErr = fmt.Errorf("%w: libnvidia-ml.so.1 is missing entry points this build requires",
					ErrUnavailable)
			}
			return
		}
		if r := C.prickle_init(); r != 0 {
			C.prickle_close()
			openErr = fmt.Errorf("%w: nvmlInit: %s", ErrUnavailable, nvmlError(r))
			return
		}
		openSource = &nvmlSource{roots: opts.Roots}
	})

	if openErr != nil {
		return nil, openErr
	}
	return openSource, nil
}

// nvmlError renders an NVML return code.
func nvmlError(r C.nvmlReturn_t) string {
	return fmt.Sprintf("%s (%d)", C.GoString(C.prickle_error_string(r)), int(r))
}

// Name implements nvidiaSource.
func (s *nvmlSource) Name() string { return SourceNVML }

// Close implements nvidiaSource, shutting NVML down once.
func (s *nvmlSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	C.prickle_close()
	return nil
}

// DriverVersion reports the driver version string, for `prickle diagnose`.
func DriverVersion() (string, error) {
	buf := (*C.char)(C.malloc(driverBufferSize))
	defer C.free(unsafe.Pointer(buf))
	if r := C.prickle_driver_version(buf, driverBufferSize); r != 0 {
		return "", fmt.Errorf("nvmlSystemGetDriverVersion: %s", nvmlError(r))
	}
	return C.GoString(buf), nil
}

// Read implements nvidiaSource.
//
// Every per-device call that can legitimately fail on some card — utilization
// while MIG is on, power on a passively cooled board, MIG on a consumer chip —
// leaves its field absent rather than failing the pass. That mirrors what the
// nvidia-smi source does with a bracketed [N/A], which is the same fact
// arriving by a different route: SPEC.md §Testing rules requires the two
// sources to agree, and agreeing about absence is part of that.
func (s *nvmlSource) Read(ctx context.Context) (snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return snapshot{}, fmt.Errorf("NVML source is closed")
	}

	var count C.uint
	if r := C.prickle_device_count(&count); r != 0 {
		return snapshot{}, fmt.Errorf("nvmlDeviceGetCount: %s", nvmlError(r))
	}

	var snap snapshot
	for i := C.uint(0); i < count; i++ {
		if err := ctx.Err(); err != nil {
			return snap, err
		}
		var handle C.nvmlDevice_t
		if r := C.prickle_device_by_index(i, &handle); r != 0 {
			return snap, fmt.Errorf("nvmlDeviceGetHandleByIndex(%d): %s", int(i), nvmlError(r))
		}

		d := device{Index: int(i)}
		uuid, err := deviceString(handle, fieldUUID, uuidBufferSize)
		if err != nil {
			return snap, fmt.Errorf("nvmlDeviceGetUUID(%d): %w", int(i), err)
		}
		d.UUID = uuid
		d.Name, _ = deviceString(handle, fieldName, nameBufferSize)

		var used, total C.ulonglong
		if r := C.prickle_device_memory(handle, &used, &total); r == 0 {
			d.MemoryUsedBytes, d.MemoryTotalBytes = uint64(used), uint64(total)
		}

		var util C.uint
		if r := C.prickle_device_utilization(handle, &util); r == 0 {
			d.Utilization, d.HasUtilization = float64(util)/100, true
		}
		var celsius C.uint
		if r := C.prickle_device_temperature(handle, &celsius); r == 0 {
			d.TemperatureC, d.HasTemperature = float64(celsius), true
		}
		var milliwatts C.uint
		if r := C.prickle_device_power_milliwatts(handle, &milliwatts); r == 0 {
			d.PowerWatts, d.HasPower = float64(milliwatts)/1000, true
		}

		var migEnabled C.uint
		if r := C.prickle_device_mig_enabled(handle, &migEnabled); r == 0 && migEnabled == 1 {
			d.MIGEnabled = true
			d.MIG = readMIG(handle)
		}

		snap.processes = append(snap.processes, s.readProcesses(handle, d)...)
		snap.devices = append(snap.devices, d)
	}
	return snap, nil
}

// stringField selects which char-buffer getter deviceString calls.
//
// cgo exposes a C function as an unsafe.Pointer rather than as a Go func
// value, so the getter cannot be passed in as a parameter; a selector and a
// switch is the idiomatic way to share the buffer handling between the two.
type stringField int

const (
	fieldUUID stringField = iota
	fieldName
)

// deviceString reads a NUL-terminated device attribute into a Go string.
func deviceString(handle C.nvmlDevice_t, which stringField, size C.uint) (string, error) {
	buf := (*C.char)(C.malloc(C.size_t(size)))
	defer C.free(unsafe.Pointer(buf))

	var r C.nvmlReturn_t
	switch which {
	case fieldUUID:
		r = C.prickle_device_uuid(handle, buf, size)
	case fieldName:
		r = C.prickle_device_name(handle, buf, size)
	}
	if r != 0 {
		return "", fmt.Errorf("%s", nvmlError(r))
	}
	return C.GoString(buf), nil
}

// readMIG enumerates a partitioned card's instances.
//
// This is the data SPEC.md §Collectors calls NVML the only reliable source of:
// each instance's own UUID and its own memory, neither of which any nvidia-smi
// CSV query publishes. The profile name is derived from the instance's memory
// rather than queried, because the profile-name entry point is not among the
// versioned symbols this build binds.
func readMIG(parent C.nvmlDevice_t) []migDevice {
	var max C.uint
	if r := C.prickle_device_max_mig_count(parent, &max); r != 0 {
		return nil
	}

	var instances []migDevice
	for i := C.uint(0); i < max; i++ {
		var handle C.nvmlDevice_t
		// A gap in the index range is normal: instances are not dense.
		if r := C.prickle_device_mig_by_index(parent, i, &handle); r != 0 {
			continue
		}
		uuid, err := deviceString(handle, fieldUUID, uuidBufferSize)
		if err != nil || uuid == "" {
			continue
		}
		m := migDevice{UUID: uuid, DeviceIndex: int(i)}

		var used, total C.ulonglong
		if r := C.prickle_device_memory(handle, &used, &total); r == 0 {
			m.MemoryUsedBytes, m.MemoryTotalBytes = uint64(used), uint64(total)
			m.HasMemory = true
			m.Profile = migProfile(uint64(total))
		}
		var util C.uint
		if r := C.prickle_device_utilization(handle, &util); r == 0 {
			m.Utilization, m.HasUtilization = float64(util)/100, true
		}
		instances = append(instances, m)
	}
	return instances
}

// migProfile renders a profile name from an instance's memory size, matching
// the "1g.18gb" spelling nvidia-smi -L uses so both sources agree.
//
// The slice count is not available from the versioned symbols bound here, so
// the leading count is omitted rather than guessed: "18gb" not "1g.18gb". A
// profile label that disagreed between the two sources would be worse than one
// that is visibly coarser in the fallback direction.
func migProfile(totalBytes uint64) string {
	if totalBytes == 0 {
		return ""
	}
	gb := (totalBytes + (1 << 29)) >> 30 // nearest GiB
	return strconv.FormatUint(gb, 10) + "gb"
}

// readProcesses lists the compute processes on a device.
//
// The PID that NVML returns in every entry is never read out of the struct:
// prickle_proc_memory reaches past it to the memory field, and no Go code here
// touches buf[i].pid. SPEC.md §Metrics contract forbids a PID as a label or a
// value, and the command comes from the process's own exe symlink, which is
// also the only place a name can be got — NVML has no process-name entry point
// among its versioned symbols.
func (s *nvmlSource) readProcesses(handle C.nvmlDevice_t, d device) []process {
	var count C.uint
	// Sizing call. NVML answers INSUFFICIENT_SIZE with the count it needs, so
	// a non-zero return here is expected and only the count matters.
	C.prickle_device_compute_proc_count(handle, &count)
	if count == 0 {
		return nil
	}

	buf := (*C.prickle_process_t)(C.malloc(C.size_t(count) * C.sizeof_prickle_process_t))
	defer C.free(unsafe.Pointer(buf))

	if r := C.prickle_device_compute_procs(handle, &count, buf); r != 0 {
		return nil
	}

	var processes []process
	for i := C.uint(0); i < count; i++ {
		memory := C.prickle_proc_memory(buf, i)
		if memory == nvmlMemoryUnavailable {
			continue
		}
		command := s.commandOf(uint32(C.prickle_proc_pid(buf, i)))
		if command == "" {
			// No readable exe link — a process that exited between the two
			// NVML calls, or one this process may not inspect. Dropping it
			// loses a few megabytes of attribution; the alternative would be
			// keying the series on the PID, which SPEC.md forbids outright.
			continue
		}
		processes = append(processes, process{
			GPUUUID:     d.UUID,
			Command:     command,
			MemoryBytes: uint64(memory),
		})
	}
	return processes
}

// commandOf resolves a PID to the basename of its executable, and is where the
// PID's life ends.
//
// SPEC.md §Metrics contract: the label comes from the `exe` symlink, never from
// comm, which the kernel truncates to 15 characters and which a process can
// rewrite to anything. The returned string is a basename, so the series does
// not carry a full filesystem path either.
//
// A deleted binary leaves the kernel appending " (deleted)" to the link target;
// that suffix is stripped so a redeployed workload does not fork its series.
func (s *nvmlSource) commandOf(pid uint32) string {
	target, err := os.Readlink(s.roots.ProcPath(strconv.FormatUint(uint64(pid), 10), "exe"))
	if err != nil {
		return ""
	}
	return path.Base(strings.TrimSuffix(target, " (deleted)"))
}

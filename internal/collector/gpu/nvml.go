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

// nvmlMemory_t, as taken by nvmlDeviceGetMemoryInfo. Its three-field layout
// has never changed. It is the fallback: see prickle_device_memory.
typedef struct {
  unsigned long long total;
  unsigned long long free;
  unsigned long long used;
} prickle_memory_t;

// nvmlMemory_v2_t, as taken by nvmlDeviceGetMemoryInfo_v2, which is what the
// two accounting schemes differ in. The original call has no `reserved` field
// and folds driver-reserved memory into `used` (it reports total - free); _v2
// breaks the two apart and reports only what a process actually allocated.
// nvidia-smi 580 prints the _v2 number, so binding the original made this
// source over-report used memory by the reserved amount — 480 MiB on the H100
// this was measured on — against the same card's nvidia-smi source. Verified
// on hardware 2026-07-29.
//
// The leading `version` field is NVIDIA's NVML_STRUCT_VERSION: the struct's
// own size ORed with the version number. The call rejects a struct whose
// version it does not recognise, which is what makes this declared layout
// self-checking rather than a hope about padding.
typedef struct {
  unsigned int version;
  unsigned long long total;
  unsigned long long reserved;
  unsigned long long free;
  unsigned long long used;
} prickle_memory_v2_t;

// nvmlDeviceAttributes_t, as taken by nvmlDeviceGetAttributes_v2. The _v2
// suffix fixes these nine fields in this order. Only gpuInstanceSliceCount is
// read: it is the leading "1g" of a MIG profile name, and without it this
// source spells the same partition "10gb" while nvidia-smi spells it
// "1g.10gb" — a label value that disagreed between the two artifacts for the
// same card. Verified on hardware 2026-07-29.
typedef struct {
  unsigned int multiprocessorCount;
  unsigned int sharedCopyEngineCount;
  unsigned int sharedDecoderCount;
  unsigned int sharedEncoderCount;
  unsigned int sharedJpegCount;
  unsigned int sharedOfaCount;
  unsigned int gpuInstanceSliceCount;
  unsigned int computeInstanceSliceCount;
  unsigned long long memorySizeMB;
} prickle_attributes_t;

// nvmlGpuInstance_t, an opaque handle to one GPU instance. Distinct from
// nvmlDevice_t in the API even though both are pointers.
typedef void *nvmlGpuInstance_t;

// nvmlGpuInstancePlacement_t and nvmlGpuInstanceInfo_t, as taken by
// nvmlGpuInstanceGetInfo. Only profileId is read; the placement is here
// because it is part of the layout, not because anything wants it.
typedef struct {
  unsigned int start;
  unsigned int size;
} prickle_gi_placement_t;

typedef struct {
  nvmlDevice_t device;
  unsigned int id;
  unsigned int profileId;
  prickle_gi_placement_t placement;
} prickle_gi_info_t;

// nvmlGpuInstanceProfileInfo_v2_t, as taken by
// nvmlDeviceGetGpuInstanceProfileInfoV. Its `name` is the driver's own
// spelling of the profile — "1g.10gb" — and is the only trustworthy source of
// it.
//
// Deriving the name from the instance's memory size instead is what this code
// did until an H200 fixture disproved it: that card's `1g.18gb` profile has a
// 16.00 GiB framebuffer, so no arithmetic over the framebuffer produces "18".
// It happened to work on an H100, where 9.75 GiB rounds to the "10" in
// "1g.10gb" and 39.50 GiB rounds to the "40" in "3g.40gb" — two coincidences
// that would have shipped a `prickle_gpu_mig_info` label reading `1g.16gb`
// from prickle-nvml and `1g.18gb` from prickle on the same H200.
//
// NVML_DEVICE_NAME_V2_BUFFER_SIZE is 96 (nvml.h, CUDA 13.1).
typedef struct {
  unsigned int version;
  unsigned int id;
  unsigned int isP2pSupported;
  unsigned int sliceCount;
  unsigned int instanceCount;
  unsigned int multiprocessorCount;
  unsigned int copyEngineCount;
  unsigned int decoderCount;
  unsigned int encoderCount;
  unsigned int jpegCount;
  unsigned int ofaCount;
  unsigned long long memorySizeMB;
  char name[96];
} prickle_gi_profile_info_t;

// nvmlComputeInstance_t, opaque, and nvmlComputeInstanceInfo_t as taken by
// nvmlComputeInstanceGetInfo_v2. Only profileId is read.
typedef void *nvmlComputeInstance_t;

typedef struct {
  unsigned int start;
  unsigned int size;
} prickle_ci_placement_t;

typedef struct {
  nvmlDevice_t device;
  nvmlGpuInstance_t gpuInstance;
  unsigned int id;
  unsigned int profileId;
  prickle_ci_placement_t placement;
} prickle_ci_info_t;

// nvmlComputeInstanceProfileInfo_v2_t, as taken by
// nvmlGpuInstanceGetComputeInstanceProfileInfoV. Its `name` is what
// `nvidia-smi -L` prints for a MIG device, which is the string this source has
// to reproduce.
typedef struct {
  unsigned int version;
  unsigned int id;
  unsigned int sliceCount;
  unsigned int instanceCount;
  unsigned int multiprocessorCount;
  unsigned int sharedCopyEngineCount;
  unsigned int sharedDecoderCount;
  unsigned int sharedEncoderCount;
  unsigned int sharedJpegCount;
  unsigned int sharedOfaCount;
  char name[96];
} prickle_ci_profile_info_t;

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
static nvmlReturn_t (*p_device_memory_v2)(nvmlDevice_t, prickle_memory_v2_t *);
static nvmlReturn_t (*p_device_temperature)(nvmlDevice_t, int, unsigned int *);
static nvmlReturn_t (*p_device_power)(nvmlDevice_t, unsigned int *);
static nvmlReturn_t (*p_device_mig_mode)(nvmlDevice_t, unsigned int *, unsigned int *);
static nvmlReturn_t (*p_device_max_mig_count)(nvmlDevice_t, unsigned int *);
static nvmlReturn_t (*p_device_mig_by_index)(nvmlDevice_t, unsigned int, nvmlDevice_t *);
static nvmlReturn_t (*p_device_attributes)(nvmlDevice_t, prickle_attributes_t *);
static nvmlReturn_t (*p_device_gi_id)(nvmlDevice_t, unsigned int *);
static nvmlReturn_t (*p_device_parent_of_mig)(nvmlDevice_t, nvmlDevice_t *);
static nvmlReturn_t (*p_device_gi_by_id)(nvmlDevice_t, unsigned int, nvmlGpuInstance_t *);
static nvmlReturn_t (*p_gi_info)(nvmlGpuInstance_t, prickle_gi_info_t *);
static nvmlReturn_t (*p_device_gi_profile_info)(nvmlDevice_t, unsigned int, prickle_gi_profile_info_t *);
static nvmlReturn_t (*p_device_ci_id)(nvmlDevice_t, unsigned int *);
static nvmlReturn_t (*p_gi_ci_by_id)(nvmlGpuInstance_t, unsigned int, nvmlComputeInstance_t *);
static nvmlReturn_t (*p_ci_info)(nvmlComputeInstance_t, prickle_ci_info_t *);
static nvmlReturn_t (*p_gi_ci_profile_info)(nvmlGpuInstance_t, unsigned int, unsigned int, prickle_ci_profile_info_t *);
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
  p_device_memory_v2 = prickle_sym("nvmlDeviceGetMemoryInfo_v2", NULL);
  p_device_temperature = prickle_sym("nvmlDeviceGetTemperature", NULL);
  p_device_power    = prickle_sym("nvmlDeviceGetPowerUsage", NULL);
  p_device_mig_mode = prickle_sym("nvmlDeviceGetMigMode", NULL);
  p_device_max_mig_count = prickle_sym("nvmlDeviceGetMaxMigDeviceCount", NULL);
  p_device_mig_by_index  = prickle_sym("nvmlDeviceGetMigDeviceHandleByIndex", NULL);
  p_device_attributes    = prickle_sym("nvmlDeviceGetAttributes_v2", NULL);
  p_device_gi_id         = prickle_sym("nvmlDeviceGetGpuInstanceId", NULL);
  p_device_parent_of_mig = prickle_sym("nvmlDeviceGetDeviceHandleFromMigDeviceHandle", NULL);
  p_device_gi_by_id      = prickle_sym("nvmlDeviceGetGpuInstanceById", NULL);
  p_gi_info              = prickle_sym("nvmlGpuInstanceGetInfo", NULL);
  p_device_gi_profile_info = prickle_sym("nvmlDeviceGetGpuInstanceProfileInfoV", NULL);
  p_device_ci_id         = prickle_sym("nvmlDeviceGetComputeInstanceId", NULL);
  p_gi_ci_by_id          = prickle_sym("nvmlGpuInstanceGetComputeInstanceById", NULL);
  p_ci_info              = prickle_sym("nvmlComputeInstanceGetInfo_v2", NULL);
  p_gi_ci_profile_info   = prickle_sym("nvmlGpuInstanceGetComputeInstanceProfileInfoV", NULL);
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
  // Every pointer into the library must go with it. dlclose unmaps the code
  // they point at, but leaves the pointers themselves non-NULL, so the NULL
  // guards in the wrappers below all still pass and the call jumps into
  // unmapped memory. That is not theoretical: it segfaulted prickle-nvml on a
  // real H100 the first time nvmlInit failed there.
  p_init = NULL;
  p_shutdown = NULL;
  p_error_string = NULL;
  p_driver_version = NULL;
  p_device_count = NULL;
  p_device_by_index = NULL;
  p_device_uuid = NULL;
  p_device_name = NULL;
  p_device_utilization = NULL;
  p_device_memory = NULL;
  p_device_memory_v2 = NULL;
  p_device_temperature = NULL;
  p_device_power = NULL;
  p_device_mig_mode = NULL;
  p_device_max_mig_count = NULL;
  p_device_mig_by_index = NULL;
  p_device_attributes = NULL;
  p_device_gi_id = NULL;
  p_device_parent_of_mig = NULL;
  p_device_gi_by_id = NULL;
  p_gi_info = NULL;
  p_device_gi_profile_info = NULL;
  p_device_ci_id = NULL;
  p_gi_ci_by_id = NULL;
  p_ci_info = NULL;
  p_gi_ci_profile_info = NULL;
  p_device_compute_procs = NULL;
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

// prickle_device_memory reports used and total bytes, preferring the _v2 call.
//
// The two calls disagree, and not by rounding: the original folds
// driver-reserved memory into `used`, _v2 does not. nvidia-smi reports the _v2
// number, and SPEC.md §Testing rules requires both sources to emit identical
// output for the same GPU, so _v2 is what this binds wherever the driver
// publishes it.
//
// The fallback is for drivers old enough not to publish the symbol at all —
// and those are exactly the drivers whose nvidia-smi also reports the original
// accounting, so the two sources still agree there. A driver that publishes
// _v2 but rejects the call is not fallen back for: its error is returned, and
// the sample goes absent rather than silently reverting to a number that would
// disagree with the other source by half a gigabyte.
static nvmlReturn_t prickle_device_memory(nvmlDevice_t d, unsigned long long *used,
                                          unsigned long long *total) {
  if (p_device_memory_v2 != NULL) {
    prickle_memory_v2_t m2;
    memset(&m2, 0, sizeof m2);
    m2.version = (unsigned int)(sizeof m2 | (2u << 24));
    nvmlReturn_t r2 = p_device_memory_v2(d, &m2);
    if (r2 == 0) { *used = m2.used; *total = m2.total; }
    return r2;
  }
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

// Reports the two slice counts a MIG profile name is built from: the "3g" of
// "3g.40gb", and the "1c" that appears in front of it when the GPU instance is
// subdivided into smaller compute instances. Only meaningful on a MIG device
// handle; the parent card answers with its own attributes, where both are zero.
static nvmlReturn_t prickle_device_slice_counts(nvmlDevice_t d, unsigned int *gpu_slices,
                                                unsigned int *compute_slices) {
  if (p_device_attributes == NULL) return -1;
  prickle_attributes_t a;
  memset(&a, 0, sizeof a);
  nvmlReturn_t r = p_device_attributes(d, &a);
  if (r == 0) {
    *gpu_slices = a.gpuInstanceSliceCount;
    *compute_slices = a.computeInstanceSliceCount;
  }
  return r;
}

// Profile enum bounds from nvml.h (CUDA 13.1). Both lookups below are indexed
// by an enum that is *not* the profile id an instance reports — see
// prickle_mig_profile_name.
#define PRICKLE_GI_PROFILE_COUNT 0x11
#define PRICKLE_CI_PROFILE_COUNT 0x8
#define PRICKLE_CI_ENGINE_PROFILE_COUNT 0x1

// prickle_mig_profile_name copies the driver's own name for a MIG device into
// buf — the string `nvidia-smi -L` prints for it. Returns 0 on success.
//
// NVML offers no call from a MIG device handle to that name, so this walks
// there: the MIG device knows its GPU-instance and compute-instance ids and its
// parent card; the parent turns the GPU-instance id into a handle; that handle
// turns the compute-instance id into a handle; and the compute instance
// carries a profile id whose name the GPU instance can describe.
//
// **The name belongs to the compute instance, not the GPU instance**, and
// hardware is what settled that. A GPU instance created from the `1g.10gb+me`
// profile — the one that carries the media engines — holds a compute instance
// that `-L` calls plain `1g.10gb`; reporting the GPU instance's name put a
// `+me` on a label the other source spelled without one. The compute-instance
// name is also already `1c.3g.40gb` for a subdivided instance, so it needs no
// slice-count arithmetic on top.
//
// The id matching is the other thing hardware sprang. An instance's profileId
// is the driver's device-unique id — 9 for 3g.40gb on an H100, the number
// `nvidia-smi mig -lgip` prints — while the lookup's `profile` parameter is an
// unrelated enum in which 9 means 1_SLICE_REV2. Passing one as the other
// returns a real profile with a plausible name for a different partition: it
// reported "1g.20gb" for a 3g.40gb instance until the cross-source test
// rejected it. So the id is matched, never indexed.
static nvmlReturn_t prickle_mig_profile_name(nvmlDevice_t mig_device, char *buf,
                                             unsigned int len) {
  if (p_device_gi_id == NULL || p_device_ci_id == NULL ||
      p_device_parent_of_mig == NULL || p_device_gi_by_id == NULL ||
      p_gi_ci_by_id == NULL || p_ci_info == NULL || p_gi_ci_profile_info == NULL)
    return -1;

  unsigned int gi_id = 0, ci_id = 0;
  nvmlReturn_t r = p_device_gi_id(mig_device, &gi_id);
  if (r != 0) return r;
  r = p_device_ci_id(mig_device, &ci_id);
  if (r != 0) return r;

  nvmlDevice_t parent = NULL;
  r = p_device_parent_of_mig(mig_device, &parent);
  if (r != 0) return r;

  nvmlGpuInstance_t gi = NULL;
  r = p_device_gi_by_id(parent, gi_id, &gi);
  if (r != 0) return r;

  nvmlComputeInstance_t ci = NULL;
  r = p_gi_ci_by_id(gi, ci_id, &ci);
  if (r != 0) return r;

  prickle_ci_info_t info;
  memset(&info, 0, sizeof info);
  r = p_ci_info(ci, &info);
  if (r != 0) return r;

  for (unsigned int prof = 0; prof < PRICKLE_CI_PROFILE_COUNT; prof++) {
    for (unsigned int eng = 0; eng < PRICKLE_CI_ENGINE_PROFILE_COUNT; eng++) {
      prickle_ci_profile_info_t profile;
      memset(&profile, 0, sizeof profile);
      profile.version = (unsigned int)(sizeof profile | (2u << 24));

      // A profile this GPU instance does not offer answers NOT_SUPPORTED.
      // Skipping is correct: the enum spans every card NVML knows about.
      if (p_gi_ci_profile_info(gi, prof, eng, &profile) != 0) continue;
      if (profile.id != info.profileId) continue;

      profile.name[sizeof profile.name - 1] = '\0';
      strncpy(buf, profile.name, len - 1);
      buf[len - 1] = '\0';
      return 0;
    }
  }
  return -1;
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
// per-process handle rather than on anything a source could own privately. So
// the load is shared and reference-counted, and nvmlMu below both guards that
// bookkeeping and serialises every NVML call — NVML's own thread safety varies
// by entry point and the sampler gives us no reason to need concurrency here.
type nvmlSource struct {
	// closed makes Close idempotent per source, so one source releasing the
	// shared load twice cannot drop the reference count below the number of
	// sources actually using it.
	closed bool

	// roots resolves /proc for the exe-symlink lookup that names a process.
	// SPEC.md §Hard constraints #3: no absolute /proc literal, here either.
	roots fsroot.Roots
}

var (
	// nvmlMu guards the three fields below and serialises every C call.
	nvmlMu sync.Mutex

	// nvmlLoaded is whether the library is open and nvmlInit_v2 has run.
	nvmlLoaded bool

	// nvmlRefs counts the live sources holding it open.
	nvmlRefs int

	// nvmlErr is sticky by design. SPEC.md §Collectors says to attempt the
	// load once and fall back silently, so a host without the driver libraries
	// pays for one dlopen, not one per collector.
	nvmlErr error
)

// newNVMLSource loads the library and initialises NVML.
//
// SPEC.md §Collectors: attempted once at startup and fallen back from silently.
// The error is descriptive because `prickle diagnose` reports it when asked why
// NVML did not load — "cannot open shared object file" and "driver/library
// version mismatch" need different fixes.
//
// "Once" governs *failure*: a load that failed is never retried. A load that
// succeeded and was then fully released is re-established here, because the
// alternative is what hardware found — `prickle diagnose` builds a GPU
// collector to describe it, closes it, and then builds the real one, and a
// single shared source handed out after its own Close reported "NVML source is
// closed" on a host where NVML was working perfectly.
func newNVMLSource(opts Options) (nvidiaSource, error) {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()

	if nvmlErr != nil {
		return nil, nvmlErr
	}
	if !nvmlLoaded {
		if rc := C.prickle_open(); rc != 0 {
			switch rc {
			case -1:
				nvmlErr = fmt.Errorf("%w: dlopen libnvidia-ml.so.1: %s",
					ErrUnavailable, C.GoString(C.prickle_dlerror()))
			default:
				nvmlErr = fmt.Errorf("%w: libnvidia-ml.so.1 is missing entry points this build requires",
					ErrUnavailable)
			}
			return nil, nvmlErr
		}
		if r := C.prickle_init(); r != 0 {
			// Describe the failure BEFORE closing. nvmlError reaches back into
			// the library for the message, and prickle_close dlcloses it.
			msg := nvmlError(r)
			C.prickle_close()
			nvmlErr = fmt.Errorf("%w: nvmlInit: %s", ErrUnavailable, msg)
			return nil, nvmlErr
		}
		nvmlLoaded = true
	}

	nvmlRefs++
	return &nvmlSource{roots: opts.Roots}, nil
}

// nvmlError renders an NVML return code.
func nvmlError(r C.nvmlReturn_t) string {
	return fmt.Sprintf("%s (%d)", C.GoString(C.prickle_error_string(r)), int(r))
}

// Name implements nvidiaSource.
func (s *nvmlSource) Name() string { return SourceNVML }

// Close implements nvidiaSource, releasing this source's reference and shutting
// NVML down when it was the last one.
func (s *nvmlSource) Close() error {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	nvmlRefs--
	if nvmlRefs <= 0 {
		nvmlRefs = 0
		if nvmlLoaded {
			C.prickle_close()
			nvmlLoaded = false
		}
	}
	return nil
}

// DriverVersion reports the driver version string, for `prickle diagnose`.
func DriverVersion() (string, error) {
	nvmlMu.Lock()
	defer nvmlMu.Unlock()
	if !nvmlLoaded {
		return "", fmt.Errorf("%w: NVML is not loaded", ErrUnavailable)
	}

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
	nvmlMu.Lock()
	defer nvmlMu.Unlock()
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
// CSV query publishes. The profile name is assembled from the instance's slice
// count and memory rather than read as a string, because NVML publishes no
// versioned entry point returning the name nvidia-smi prints.
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

			var gpuSlices, computeSlices C.uint
			C.prickle_device_slice_counts(handle, &gpuSlices, &computeSlices)
			m.Profile = migProfile(profileName(handle),
				uint64(gpuSlices), uint64(computeSlices), uint64(total))
		}
		var util C.uint
		if r := C.prickle_device_utilization(handle, &util); r == 0 {
			m.Utilization, m.HasUtilization = float64(util)/100, true
		}
		instances = append(instances, m)
	}
	return instances
}

// profileName asks the driver what a MIG device's GPU-instance profile is
// called. Empty when any step of the lookup declines.
//
// NVML returns the name with a "MIG " prefix — "MIG 1g.10gb" — which is how
// nvidia-smi's tables print it but not how `nvidia-smi -L` spells it, and the
// label has to match `-L` because that is what the other source parses. The
// prefix is dropped here, at the boundary where the driver's string arrives,
// so nothing downstream has to know it was ever there.
func profileName(handle C.nvmlDevice_t) string {
	buf := (*C.char)(C.malloc(nameBufferSize))
	defer C.free(unsafe.Pointer(buf))

	if r := C.prickle_mig_profile_name(handle, buf, nameBufferSize); r != 0 {
		return ""
	}
	return strings.TrimPrefix(C.GoString(buf), "MIG ")
}

// migProfile renders the profile label for one MIG instance, matching the
// spelling nvidia-smi -L uses so both sources put the same value on
// prickle_gpu_mig_info.
//
// When the driver names the compute instance, that name is used whole. It is
// already the complete spelling — `1c.3g.40gb` for a subdivided instance, not
// `3g.40gb` needing a prefix — and adding anything to it produced the
// `1c.1c.3g.40gb` that hardware rejected.
//
// It has to come from the driver, because the name is not a function of the
// instance's memory. An H200's `1g.18gb` profile has a 16.00 GiB framebuffer,
// so no arithmetic over that number yields "18": the committed
// testdata/h200-mig-20260726 capture is the proof, and it is why deriving the
// name was replaced with asking for it.
//
// The rest is the fallback, for a driver that declines the lookup: memory
// rounded to GiB, with the GPU-instance slice count in front and the
// compute-instance count in front of that when the two differ, which is how
// nvidia-smi spells a subdivided instance. It is a last resort — known to be
// coarse-to-wrong on cards whose profile name is not their framebuffer size —
// and the hardware test fails on any card where it disagrees with nvidia-smi.
func migProfile(driverName string, gpuSlices, computeSlices, totalBytes uint64) string {
	if driverName != "" {
		return driverName
	}
	if totalBytes == 0 {
		return ""
	}

	name := strconv.FormatUint((totalBytes+(1<<29))>>30, 10) + "gb" // nearest GiB
	if gpuSlices == 0 {
		return name
	}
	name = strconv.FormatUint(gpuSlices, 10) + "g." + name
	if computeSlices != 0 && computeSlices != gpuSlices {
		name = strconv.FormatUint(computeSlices, 10) + "c." + name
	}
	return name
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
			Container:   containerOfPID(s.roots, uint32(C.prickle_proc_pid(buf, i))),
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

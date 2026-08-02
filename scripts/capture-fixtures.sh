#!/usr/bin/env bash
#
# capture-fixtures.sh (v2) — prickle-exporter (Starkdrift) test fixture capture
#
# v2 adds everything the first H200 rental run was missing:
#   * `check`   — preflight: reports exactly which fixtures a capture would
#                 produce and which would come back empty. Run this FIRST.
#   * `prep`    — DISPOSABLE CAPTURE HOSTS ONLY. Installs Docker + k3s, starts
#                 test containers and a pod, enables MIG + creates instances
#                 (NVIDIA datacenter cards), and starts a GPU workload so
#                 query-compute-apps and per-process fdinfo are non-empty.
#                 This MUTATES the host. Never run it on a machine you keep.
#   * `capture` — (default) the read-only capture, now including MIG topology,
#                 rocm-smi output, and a gap report so an incomplete capture
#                 is impossible to miss before you destroy the rental.
#
# Usage:
#   sudo ./capture-fixtures.sh check
#   sudo ./capture-fixtures.sh prep      # throwaway hosts only
#   sudo ./capture-fixtures.sh           # capture (add OUT_DIR=... to override)
#
set -uo pipefail    # intentionally no -e: partial capture beats no capture

HOST=$(hostname -s 2>/dev/null || echo unknown)
STAMP=$(date +%Y%m%d)
OUT_DIR=${OUT_DIR:-./prickle-fixtures-${HOST}-${STAMP}}
CAPTURED=0; SKIPPED=0; GAPS=()

say()  { printf '\n==> %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
warn() { printf '    [skip] %s\n' "$*"; SKIPPED=$((SKIPPED+1)); }
gap()  { printf '    [GAP]  %s\n' "$*"; GAPS+=("$*"); }
ok()   { printf '    [ok]   %s\n' "$*"; }

have() { command -v "$1" >/dev/null 2>&1; }

# cat, not cp: /proc and /sys files report size 0 and cp mangles them
grab() {
  local src=$1 dst
  [[ -r $src ]] || { warn "unreadable: $src"; return 1; }
  dst="$OUT_DIR$src"
  mkdir -p "$(dirname "$dst")"
  if cat "$src" > "$dst" 2>/dev/null; then CAPTURED=$((CAPTURED+1)); else
    rm -f "$dst"; warn "read failed: $src"; return 1; fi
}

# cgroup.procs is deliberately absent. It is the one file in a cgroup that
# contains PIDs, SPEC.md §Metrics contract forbids a PID appearing anywhere, and
# the collector never reads it. Capturing it and stripping it by hand before
# committing — which is what this script used to require — puts a SPEC violation
# one forgotten step away from the repository, in exchange for a file nothing
# reads. ci/check.sh fails if one ever appears under testdata/.
#
# cpu.pressure and io.pressure are here because the collector reads both; before
# this they were covered only by a hand-written tree, so the format was asserted
# against itself rather than against a kernel.
CG_FILES="cgroup.type cpu.stat cpu.max cpu.weight cpu.pressure
          memory.current memory.max memory.min memory.low memory.high
          memory.stat memory.pressure io.stat io.pressure
          pids.current pids.max"
grab_cgroup_dir() { local d=$1 f; for f in $CG_FILES; do [[ -e $d/$f ]] && grab "$d/$f"; done; }

require_root() {
  if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    echo "This command needs root (sudo): fdinfo/cgroup access, MIG, installs." >&2
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# Environment probes (shared by check / prep / capture)
# ---------------------------------------------------------------------------
docker_running_count() {
  have docker || { echo 0; return; }
  docker ps -q 2>/dev/null | wc -l
}
# The kubelet writes one of two trees depending on its cgroup driver, and they
# differ in the directory name as well as the layout:
#
#   systemd   /sys/fs/cgroup/kubepods.slice/kubepods-<qos>.slice/kubepods-<qos>-pod<uid>.slice/
#   cgroupfs  /sys/fs/cgroup/kubepods/<qos>/pod<uid>/
#
# Only the first was known here, so on a cgroupfs node this script captured no
# pod cgroups at all and reported "no kubepods.slice pods", which reads as "you
# forgot to run something" on a host that was running nine.
kubepods_root() {
  local d
  for d in /sys/fs/cgroup/kubepods.slice /sys/fs/cgroup/kubepods; do
    [[ -d $d ]] && { printf '%s' "$d"; return 0; }
  done
  return 1
}
kubepods_pod_count() {
  local root; root=$(kubepods_root) || { echo 0; return; }
  # pod<uid> under cgroupfs, kubepods-...pod<uid>.slice under systemd.
  find "$root" -maxdepth 3 -type d \( -name 'pod*' -o -name '*pod*.slice' \) \
       2>/dev/null | wc -l
}
nvidia_present()      { have nvidia-smi; }
nvidia_mig_capable()  { nvidia-smi mig -lgip >/dev/null 2>&1; }
nvidia_mig_enabled()  { nvidia-smi -L 2>/dev/null | grep -q 'MIG '; }
nvidia_compute_apps() {
  nvidia-smi --query-compute-apps=pid --format=csv,noheader 2>/dev/null | grep -c .
}
# Echoes the first card's utilization as the driver prints it: a number, or
# [N/A] whenever MIG is enabled.
nvidia_utilization() {
  nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits 2>/dev/null \
    | head -1 | tr -d ' '
}
amd_present() { compgen -G '/sys/class/drm/card*/device/gpu_busy_percent' >/dev/null; }

# ---------------------------------------------------------------------------
# GPU workload: needed so query-compute-apps and per-process fdinfo have rows.
# Fallback chain: dcgmproftester -> torch -> nvcc-built kernel -> self-built
# spinner (gcc + libcuda driver API via dlopen + embedded PTX — needs NO CUDA
# toolkit, no downloads).
#
# nvcc comes before the PTX spinner because the spinner's JIT is the fragile
# link: on an H100 with driver 580 / CUDA 13.0 its kernel launch failed with
# CUDA_ERROR_CONTEXT_IS_DESTROYED and it fell back to holding a context, which
# is a *silent* degradation — query-compute-apps still has its row, so `check`
# passes, and the capture goes home with utilization.gpu reading 0. A fixture
# that says 0 cannot be told apart from one where the parser wrongly turned an
# absent [N/A] into a zero, which is the single most important thing the GPU
# fixtures exist to distinguish. NVIDIA's own images ship a toolkit; use it.
# ---------------------------------------------------------------------------
emit_spinner_cu() {
  cat > "$1" <<'CUEOF'
/* prickle-gpu-spin (nvcc): a real kernel, so utilization.gpu reads 100 rather
 * than 0. Preferred over the PTX spinner whenever a CUDA toolkit is present. */
#include <cstdio>
#include <cstdlib>
#include <ctime>

__global__ void spin(float *out, unsigned long long iters) {
  float f = 1.0000001f;
  for (unsigned long long i = 0; i < iters; i++) f = fmaf(f, f, f);
  out[blockIdx.x * blockDim.x + threadIdx.x] = f;
}

int main(int argc, char **argv) {
  long duration = argc > 1 ? atol(argv[1]) : 600;
  float *out = NULL; void *ballast = NULL;
  cudaMalloc(&out, 1024 * 1024 * sizeof(float));
  cudaMalloc(&ballast, 4ULL << 30);        /* best effort: visible memory use */
  setvbuf(stderr, NULL, _IONBF, 0);

  time_t end = time(NULL) + duration;
  long n = 0;
  while (time(NULL) < end) {
    spin<<<1024, 256>>>(out, 2000000ULL);
    cudaError_t e = cudaDeviceSynchronize();
    if (e != cudaSuccess) {
      fprintf(stderr, "cudaDeviceSynchronize: %s\n", cudaGetErrorString(e));
      return 1;
    }
    if (++n == 1) fprintf(stderr, "first kernel iteration OK\n");
  }
  fprintf(stderr, "done: %ld kernel iterations\n", n);
  return 0;
}
CUEOF
}

# nvcc_path echoes a usable nvcc, or nothing.
nvcc_path() {
  if have nvcc; then command -v nvcc; return; fi
  local c
  for c in /usr/local/cuda/bin/nvcc /usr/local/cuda-*/bin/nvcc; do
    [[ -x $c ]] && { echo "$c"; return; }
  done
}

emit_spinner_c() {
  cat > "$1" <<'CEOF'
/* prickle-gpu-spin: minimal CUDA driver-API load generator.
 * Builds with plain gcc on any host with the NVIDIA driver (dlopen libcuda,
 * JIT-compiled PTX) — no CUDA toolkit required. If kernel execution fails
 * for any reason, it degrades to holding a context + memory allocation so
 * the process still appears in query-compute-apps (utilization reads 0). */
#include <stdio.h>
#include <stdlib.h>
#include <dlfcn.h>
#include <time.h>
#include <unistd.h>

typedef int CUresult; typedef int CUdevice;
typedef void *CUcontext, *CUmodule, *CUfunction;
typedef unsigned long long CUdeviceptr;

static const char *ptx =
".version 7.0\n.target sm_70\n.address_size 64\n"
".visible .entry spin(.param .u64 out_ptr, .param .u64 iters)\n{\n"
"  .reg .pred %p1;\n  .reg .u64 %rd<5>;\n  .reg .f32 %f<2>;\n"
"  ld.param.u64 %rd1, [out_ptr];\n  ld.param.u64 %rd2, [iters];\n"
"  cvta.to.global.u64 %rd4, %rd1;\n"
"  mov.u64 %rd3, 0;\n  mov.f32 %f1, 0f3F800001;\n"
"$L:\n  fma.rn.f32 %f1, %f1, %f1, %f1;\n  add.u64 %rd3, %rd3, 1;\n"
"  setp.lt.u64 %p1, %rd3, %rd2;\n  @%p1 bra $L;\n"
"  st.global.f32 [%rd4], %f1;\n  ret;\n}\n";

static CUresult (*cuInit)(unsigned);
static CUresult (*cuDeviceGet)(CUdevice*, int);
static CUresult (*cuDeviceGetName)(char*, int, CUdevice);
static CUresult (*cuDevicePrimaryCtxRetain)(CUcontext*, CUdevice);
static CUresult (*cuCtxSetCurrent)(CUcontext);
static CUresult (*cuModuleLoadData)(CUmodule*, const void*);
static CUresult (*cuModuleGetFunction)(CUfunction*, CUmodule, const char*);
static CUresult (*cuMemAlloc)(CUdeviceptr*, unsigned long);
static CUresult (*cuLaunchKernel)(CUfunction, unsigned,unsigned,unsigned,
    unsigned,unsigned,unsigned, unsigned, void*, void**, void**);
static CUresult (*cuCtxSynchronize)(void);
static CUresult (*cuGetErrorName)(CUresult, const char**);

#define GET(sym) do { \
  *(void**)(&sym) = dlsym(h, #sym "_v2"); \
  if (!sym) *(void**)(&sym) = dlsym(h, #sym); \
  if (!sym) { fprintf(stderr, "missing symbol %s\n", #sym); return 1; } \
} while (0)

static const char *errname(CUresult r) {
  const char *s = "?";
  if (cuGetErrorName) cuGetErrorName(r, &s);
  return s;
}
static CUresult rc;
#define TRY(call) \
  ((rc = (call)) ? (fprintf(stderr, "%s -> %d (%s)\n", #call, rc, errname(rc)), 1) : 0)

int main(int argc, char **argv) {
  setvbuf(stderr, NULL, _IONBF, 0);
  long duration = argc > 1 ? atol(argv[1]) : 600;
  void *h = dlopen("libcuda.so.1", RTLD_NOW);
  if (!h) { fprintf(stderr, "dlopen libcuda.so.1: %s\n", dlerror()); return 1; }
  GET(cuInit); GET(cuDeviceGet); GET(cuDevicePrimaryCtxRetain);
  GET(cuCtxSetCurrent); GET(cuModuleLoadData); GET(cuModuleGetFunction);
  GET(cuMemAlloc); GET(cuLaunchKernel); GET(cuCtxSynchronize);
  *(void**)(&cuGetErrorName)  = dlsym(h, "cuGetErrorName");
  *(void**)(&cuDeviceGetName) = dlsym(h, "cuDeviceGetName");

  CUdevice dev; CUcontext ctx;
  if (TRY(cuInit(0)) || TRY(cuDeviceGet(&dev, 0)) ||
      TRY(cuDevicePrimaryCtxRetain(&ctx, dev)) || TRY(cuCtxSetCurrent(ctx)))
    return 1;
  char name[128] = "unknown";
  if (cuDeviceGetName) cuDeviceGetName(name, sizeof name, dev);
  fprintf(stderr, "prickle-gpu-spin: context up on device 0 (%s), %lds\n",
          name, duration);

  CUdeviceptr out = 0, ballast = 0;
  cuMemAlloc(&ballast, 512ULL << 20);           /* best effort */
  CUmodule mod; CUfunction fn; int kernel_ok = 1;
  if (TRY(cuMemAlloc(&out, 8)) || TRY(cuModuleLoadData(&mod, ptx)) ||
      TRY(cuModuleGetFunction(&fn, mod, "spin"))) kernel_ok = 0;

  unsigned long long iters = 5000000ULL;
  void *params[2] = { &out, &iters };
  long n = 0;
  time_t end = time(NULL) + duration;
  while (time(NULL) < end) {
    if (kernel_ok) {
      if (TRY(cuLaunchKernel(fn, 256,1,1, 256,1,1, 0, NULL, params, NULL)) ||
          TRY(cuCtxSynchronize())) {
        kernel_ok = 0;
        fprintf(stderr, "kernel path failed after %ld iterations; holding "
                "context only (process stays visible, utilization reads 0)\n", n);
      } else if (++n == 1) {
        fprintf(stderr, "first kernel iteration OK\n");
      }
    } else {
      sleep(2);
    }
  }
  fprintf(stderr, "done: %ld kernel iterations\n", n);
  return 0;
}
CEOF
}

start_nvidia_workload() {
  # $1 = MIG UUID to pin to, or empty for the whole GPU
  local uuid=$1 runner=() dur=600
  [[ -n $uuid ]] && runner=(env "CUDA_VISIBLE_DEVICES=$uuid")
  if have dcgmproftester12 || have dcgmproftester; then
    local dp; dp=$(have dcgmproftester12 && echo dcgmproftester12 || echo dcgmproftester)
    nohup "${runner[@]}" "$dp" --no-dcgm-validation -t 1004 -d $dur \
      >/tmp/gpu-workload.log 2>&1 &
    note "$dp running (${dur}s)${uuid:+ on $uuid}"
  elif have python3 && python3 -c 'import torch' 2>/dev/null; then
    nohup "${runner[@]}" python3 -c '
import torch
a = torch.rand(8192, 8192, device="cuda")
while True: a = a @ a' >/tmp/gpu-workload.log 2>&1 &
    note "torch matmul loop running${uuid:+ on $uuid}"
  elif [[ -n $(nvcc_path) ]]; then
    local nvcc; nvcc=$(nvcc_path)
    note "no dcgmproftester/torch — building prickle-gpu-spin with $nvcc"
    emit_spinner_cu /tmp/prickle-gpu-spin.cu
    # sm_90 is Hopper (H100/H200); -arch=native lets older and newer cards
    # build the same source. Try native first, since it needs no table here.
    if "$nvcc" -O2 -arch=native -o /tmp/prickle-gpu-spin /tmp/prickle-gpu-spin.cu \
         >/tmp/gpu-workload.log 2>&1 ||
       "$nvcc" -O2 -o /tmp/prickle-gpu-spin /tmp/prickle-gpu-spin.cu \
         >>/tmp/gpu-workload.log 2>&1; then
      nohup "${runner[@]}" /tmp/prickle-gpu-spin $dur >>/tmp/gpu-workload.log 2>&1 &
      note "prickle-gpu-spin running (${dur}s)${uuid:+ on $uuid}"
    else
      warn "nvcc build failed — see /tmp/gpu-workload.log"
    fi
  elif have gcc; then
    note "no dcgmproftester/torch/nvcc — building prickle-gpu-spin (gcc + libcuda, no toolkit needed)"
    note "NOTE: this path JIT-compiles PTX and has been seen to fail on CUDA 13"
    note "      drivers, degrading to a context-only load with 0% utilization."
    note "      Check the captured query-gpu.csv before destroying the host."
    emit_spinner_c /tmp/prickle-gpu-spin.c
    if gcc -O2 -o /tmp/prickle-gpu-spin /tmp/prickle-gpu-spin.c -ldl 2>/tmp/gpu-workload.log; then
      nohup "${runner[@]}" /tmp/prickle-gpu-spin $dur >>/tmp/gpu-workload.log 2>&1 &
      note "prickle-gpu-spin running (${dur}s)${uuid:+ on $uuid}"
    else
      warn "spinner build failed — see /tmp/gpu-workload.log"
    fi
  else
    warn "no dcgmproftester, torch, or gcc — start a GPU workload manually"
  fi
}
fdinfo_has_drm_keys() {
  local p f
  for p in /proc/[0-9]*; do
    for f in "$p"/fd/*; do
      case $(readlink "$f" 2>/dev/null) in
        /dev/dri/*) grep -qs 'drm-' "$p/fdinfo/${f##*/}" && return 0 ;;
      esac
    done
  done
  return 1
}

# ---------------------------------------------------------------------------
# check — preflight report: what would this capture actually contain?
# ---------------------------------------------------------------------------
cmd_check() {
  say "Preflight: what a capture on this host would contain"

  [[ -r /proc/stat ]] && ok "host /proc metrics" || gap "/proc unreadable"
  local fstype; fstype=$(stat -fc %T /sys/fs/cgroup/ 2>/dev/null || echo unknown)
  if [[ $fstype == cgroup2fs ]]; then ok "cgroup v2 ($fstype)"
  else gap "cgroup fstype is '$fstype' — prickle is v2-only; fixtures unusable"; fi

  local n; n=$(docker_running_count)
  if [[ $n -gt 0 ]]; then ok "Docker: $n running container(s)"
  else gap "no running Docker containers — Phase 2 docker scopes will be EMPTY"; fi

  n=$(kubepods_pod_count)
  if [[ $n -gt 0 ]]; then ok "kubepods: $n pod slice(s)"
  else gap "no kubepods pods under either driver's layout — Phase 2 kubernetes fixtures will be EMPTY"; fi

  if nvidia_present; then
    ok "nvidia-smi present (driver $(nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null | head -1))"
    if nvidia_mig_capable; then
      if nvidia_mig_enabled; then ok "MIG enabled — MIG UUIDs will be captured"
      else gap "MIG-capable GPU but MIG is DISABLED — the mig_uuid fixtures will be EMPTY (run prep, or enable manually)"; fi
    else
      note "GPU is not MIG-capable (consumer/workstation card) — MIG fixtures not expected here"
    fi
    n=$(nvidia_compute_apps)
    if [[ $n -gt 0 ]]; then
      ok "GPU compute apps running: $n — query-compute-apps will have rows"
      # A process can hold a CUDA context without running a single kernel, and
      # that capture is worse than useless: utilization.gpu reads 0, which is
      # indistinguishable from a parser wrongly turning an absent [N/A] into a
      # zero. [N/A] itself is fine and expected under MIG — only a numeric 0 is
      # the problem.
      if [[ $(nvidia_utilization) == 0 ]]; then
        gap "a GPU process is resident but utilization.gpu is 0 — the workload holds a context without running kernels, and a 0 in the fixture cannot be told from a mis-parsed [N/A]"
      fi
    else gap "no GPU compute processes — query-compute-apps.csv will be EMPTY"; fi
  else
    note "no nvidia-smi (fine on AMD hosts)"
  fi

  if amd_present; then
    ok "AMD GPU sysfs present"
    if fdinfo_has_drm_keys; then ok "DRM fdinfo has drm-* keys (workload running)"
    else gap "AMD present but no drm-* fdinfo keys — start a ROCm/GPU workload for per-process fixtures"; fi
  else
    note "no AMD GPU sysfs (fine on NVIDIA hosts)"
  fi

  echo
  if [[ ${#GAPS[@]} -eq 0 ]]; then
    echo "READY: a capture now would be complete for this host class."
  else
    echo "INCOMPLETE: ${#GAPS[@]} gap(s). Fix them (or run 'prep' on a disposable"
    echo "host) BEFORE capturing — do not destroy a rental with gaps open:"
    printf '  - %s\n' "${GAPS[@]}"
    return 2
  fi
}

# ---------------------------------------------------------------------------
# prep — DISPOSABLE HOSTS ONLY: mutate the box until check passes
# ---------------------------------------------------------------------------
cmd_prep() {
  require_root
  echo "*** prep MUTATES this host (installs Docker/k3s, toggles MIG, starts"
  echo "*** workloads). It is for disposable capture rentals ONLY."
  echo "*** Ctrl-C within 5s to abort."
  sleep 5

  say "Docker"
  if ! have docker; then
    note "installing docker.io"
    apt-get update -qq && apt-get install -y -qq docker.io >/dev/null || warn "docker install failed"
  fi
  if have docker; then
    for c in fixture-nginx fixture-redis fixture-sleeper; do
      docker inspect "$c" >/dev/null 2>&1 && continue
      case $c in
        fixture-nginx)   docker run -d --name "$c" nginx:alpine >/dev/null ;;
        fixture-redis)   docker run -d --name "$c" redis:alpine >/dev/null ;;
        fixture-sleeper) docker run -d --name "$c" busybox sleep 86400 >/dev/null ;;
      esac && note "started $c"
    done
  fi

  say "k3s (single node) + one pod"
  if ! have k3s; then
    note "installing k3s"
    curl -sfL https://get.k3s.io | sh -s - --write-kubeconfig-mode 644 >/dev/null 2>&1 \
      || warn "k3s install failed"
  fi
  if have k3s; then
    k3s kubectl get pod fixture-pod >/dev/null 2>&1 \
      || k3s kubectl run fixture-pod --image=nginx:alpine >/dev/null 2>&1
    note "waiting for fixture-pod"
    k3s kubectl wait --for=condition=Ready pod/fixture-pod --timeout=120s >/dev/null 2>&1 \
      || warn "fixture-pod not ready — kubepods capture may be partial"
  fi

  if nvidia_present && nvidia_mig_capable && ! nvidia_mig_enabled; then
    say "Enabling MIG and creating 2 instances"
    systemctl stop dcgm-exporter nvidia-persistenced >/dev/null 2>&1
    nvidia-smi -mig 1 >/dev/null 2>&1 || warn "MIG enable failed (may need reboot)"
    # smallest GPU-instance profile, twice; 19 is the 1g profile on A100/H100/H200
    local prof
    prof=$(nvidia-smi mig -lgip 2>/dev/null | grep -oP 'MIG\s+\S+\s+\K[0-9]+' | head -1)
    nvidia-smi mig -cgi "${prof:-19},${prof:-19}" -C >/dev/null 2>&1 \
      || warn "MIG instance creation failed — check 'nvidia-smi mig -lgip'"
    systemctl start nvidia-persistenced >/dev/null 2>&1
  fi

  if nvidia_present && [[ $(nvidia_compute_apps) -eq 0 ]]; then
    say "Starting a GPU workload"
    local mig_uuid
    mig_uuid=$(nvidia-smi -L | grep -oP 'MIG-[0-9a-f-]+' | head -1)
    start_nvidia_workload "${mig_uuid:-}"
    sleep 12
    if [[ $(nvidia_compute_apps) -gt 0 ]]; then
      note "workload visible in query-compute-apps"
    else
      warn "workload started but not yet visible — check /tmp/gpu-workload.log"
    fi
  fi

  echo; say "Re-running preflight"; GAPS=(); cmd_check
}

# ---------------------------------------------------------------------------
# capture — read-only, with gap report
# ---------------------------------------------------------------------------
cmd_capture() {
  if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    echo "WARNING: not root — other processes' fdinfo/cgroup capture will be" >&2
    echo "incomplete. Re-run with sudo for a full capture." >&2
  fi
  mkdir -p "$OUT_DIR/meta"

  say "Host metadata"
  { echo "capture_date: $(date -Is)"; echo "hostname: $HOST"
    echo "kernel: $(uname -r)"; echo "uname: $(uname -a)"
    echo "cgroup_fstype: $(stat -fc %T /sys/fs/cgroup/ 2>/dev/null || echo unknown)"
  } > "$OUT_DIR/meta/host.txt"
  [[ -r /etc/os-release ]] && cat /etc/os-release > "$OUT_DIR/meta/os-release.txt"
  have docker && docker version > "$OUT_DIR/meta/docker-version.txt" 2>&1
  nvidia_present && nvidia-smi --query-gpu=driver_version --format=csv,noheader \
    > "$OUT_DIR/meta/nvidia-driver.txt" 2>&1
  ls -l /sys/class/drm > "$OUT_DIR/meta/drm-listing.txt" 2>&1 || true

  say "/proc host metrics"
  for f in stat meminfo diskstats loadavg uptime mounts; do grab "/proc/$f"; done
  grab /proc/net/dev
  for f in cpu memory io; do grab "/proc/pressure/$f"; done
  df -B1 --output=target,fstype,size,used,avail 2>/dev/null \
    > "$OUT_DIR/meta/statfs-reference.txt" || true

  # cgroup v1 controller hierarchies. A v1 host spreads one container across
  # /sys/fs/cgroup/<controller>/, so the per-controller files are captured
  # separately from the v2 walk below; on a v2-only host this finds nothing and
  # costs a few stats. proc/mounts decides which hierarchy the reader uses, and
  # is already captured above.
  say "cgroup v1 controllers"
  local v1=0 ctrl d
  for ctrl in memory "cpu,cpuacct" cpu cpuacct blkio pids; do
    [[ -d /sys/fs/cgroup/$ctrl ]] || continue
    while IFS= read -r d; do
      for f in memory.usage_in_bytes memory.limit_in_bytes memory.stat \
               cpu.cfs_quota_us cpu.cfs_period_us cpu.shares cpu.stat \
               cpuacct.usage cpuacct.stat \
               blkio.throttle.io_service_bytes blkio.throttle.io_serviced \
               pids.current pids.max; do
        [[ -e $d/$f ]] && grab "$d/$f" && v1=$((v1+1))
      done
    done < <(find "/sys/fs/cgroup/$ctrl" -mindepth 1 -type d 2>/dev/null)
  done
  [[ $v1 -eq 0 ]] && note "no cgroup v1 controllers (fine on a v2 host)"

  say "Docker cgroup scopes"
  local found=0 d
  while IFS= read -r d; do grab_cgroup_dir "$d"; found=1; done \
    < <(find /sys/fs/cgroup/system.slice -maxdepth 1 -type d -name 'docker-*.scope' 2>/dev/null)
  while IFS= read -r d; do grab_cgroup_dir "$d"; found=1; done \
    < <(find /sys/fs/cgroup/docker -mindepth 1 -maxdepth 1 -type d 2>/dev/null)
  [[ $found -eq 0 ]] && gap "no Docker container cgroups captured"

  say "kubepods cgroup tree"
  if kp_root=$(kubepods_root); then
    say "  driver layout: $kp_root"
    while IFS= read -r d; do grab_cgroup_dir "$d"; done \
      < <(find "$kp_root" -type d 2>/dev/null)
  else gap "no kubepods tree under either /sys/fs/cgroup/kubepods.slice or /sys/fs/cgroup/kubepods"; fi

  say "Docker API"
  if [[ -S /var/run/docker.sock ]] && have curl; then
    mkdir -p "$OUT_DIR/docker-api"
    curl -sf --unix-socket /var/run/docker.sock http://localhost/containers/json \
      > "$OUT_DIR/docker-api/containers.json" && CAPTURED=$((CAPTURED+1))
    [[ $(cat "$OUT_DIR/docker-api/containers.json" 2>/dev/null) == "[]" ]] \
      && gap "docker-api/containers.json is an empty list"
  else warn "no Docker socket or curl missing"; fi

  # Captured on every host, not only GPU ones. The non-NVIDIA devices are what
  # give the fixture its value: a vendor test and a class test disagree about a
  # virtio VGA console, and only a tree containing one can show that. It is also
  # the only GPU signal that survives a missing driver, which is what lets
  # `prickle diagnose` tell "no NVIDIA card here" apart from "a card whose
  # driver is not installed" — identical in every other signal the exporter has.
  say "PCI bus"
  local pci=0 dev f
  for dev in /sys/bus/pci/devices/*/; do
    [[ -d $dev ]] || continue
    for f in vendor device class; do [[ -r "$dev$f" ]] && grab "$dev$f"; done
    pci=$((pci+1))
  done
  [[ $pci -eq 0 ]] && gap "no PCI devices captured"

  say "NVIDIA"
  if nvidia_present; then
    mkdir -p "$OUT_DIR/nvidia"
    nvidia-smi -L > "$OUT_DIR/nvidia/gpus.txt" 2>&1
    nvidia-smi --query-gpu=index,uuid,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw \
      --format=csv,noheader,nounits > "$OUT_DIR/nvidia/query-gpu.csv" 2>&1
    nvidia-smi --query-compute-apps=gpu_uuid,pid,process_name,used_gpu_memory \
      --format=csv,noheader,nounits > "$OUT_DIR/nvidia/query-compute-apps.csv" 2>&1
    nvidia-smi > "$OUT_DIR/nvidia/smi.txt" 2>&1
    nvidia-smi mig -lgip > "$OUT_DIR/nvidia/mig-profiles.txt" 2>&1
    nvidia-smi mig -lgi  > "$OUT_DIR/nvidia/mig-gi.txt" 2>&1
    nvidia-smi mig -lci  > "$OUT_DIR/nvidia/mig-ci.txt" 2>&1
    CAPTURED=$((CAPTURED+6))
    nvidia_mig_capable && ! nvidia_mig_enabled \
      && gap "MIG-capable GPU captured with MIG disabled"
    [[ ! -s $OUT_DIR/nvidia/query-compute-apps.csv ]] \
      && gap "query-compute-apps.csv is empty (no GPU workload was running)"
    [[ -s $OUT_DIR/nvidia/query-compute-apps.csv && $(nvidia_utilization) == 0 ]] \
      && gap "utilization.gpu captured as 0 with a process resident — see 'check'"
  else note "no nvidia-smi"; fi

  say "AMD sysfs + rocm-smi"
  local card dev hw f found_amd=0
  for card in /sys/class/drm/card[0-9]*; do
    dev="$card/device"; [[ -e $dev/gpu_busy_percent ]] || continue
    found_amd=1
    for f in gpu_busy_percent mem_info_vram_used mem_info_vram_total; do grab "$dev/$f"; done
    for hw in "$dev"/hwmon/hwmon*; do
      for f in temp1_input power1_average power1_cap name; do
        [[ -e $hw/$f ]] && grab "$hw/$f"; done; done
  done
  if [[ $found_amd -eq 1 ]]; then
    have rocm-smi && rocm-smi --showall > "$OUT_DIR/meta/rocm-smi.txt" 2>&1
  else note "no AMD GPUs (fine on NVIDIA hosts)"; fi

  say "Per-process GPU fdinfo"
  local pid_dir pid fd_link tgt hit gpu_pids=0 drm_keys=0
  for pid_dir in /proc/[0-9]*; do
    pid=${pid_dir##*/}; hit=0
    for fd_link in "$pid_dir"/fd/*; do
      tgt=$(readlink "$fd_link" 2>/dev/null) || continue
      case $tgt in
        /dev/dri/*|/dev/nvidia*)
          grab "$pid_dir/fdinfo/${fd_link##*/}"
          grep -qs 'drm-' "$pid_dir/fdinfo/${fd_link##*/}" && drm_keys=1
          hit=1 ;;
      esac
    done
    if [[ $hit -eq 1 ]]; then
      gpu_pids=$((gpu_pids+1)); grab "$pid_dir/cgroup"; grab "$pid_dir/comm"
      mkdir -p "$OUT_DIR$pid_dir"
      readlink "$pid_dir/exe" 2>/dev/null > "$OUT_DIR$pid_dir/exe.link" \
        || echo '<unreadable>' > "$OUT_DIR$pid_dir/exe.link"
    fi
  done
  note "processes with GPU fds: $gpu_pids (drm-* keys present: $drm_keys)"
  [[ $gpu_pids -eq 0 ]] && gap "no processes held GPU device fds"
  amd_present && [[ $drm_keys -eq 0 ]] \
    && gap "AMD host but no drm-* fdinfo keys (no workload during capture)"

  say "Done"
  note "files captured: $CAPTURED   skipped: $SKIPPED"
  note "output: $OUT_DIR"
  local tarball="${OUT_DIR%/}.tar.gz"
  tar -czf "$tarball" -C "$(dirname "$OUT_DIR")" "$(basename "$OUT_DIR")" 2>/dev/null \
    && note "tarball: $tarball"
  echo
  if [[ ${#GAPS[@]} -gt 0 ]]; then
    echo "############################################################"
    echo "# CAPTURE IS INCOMPLETE — ${#GAPS[@]} gap(s). Do NOT destroy this"
    echo "# host yet. Fix the gaps (or run 'prep') and capture again:"
    printf '#   - %s\n' "${GAPS[@]}"
    echo "############################################################"
    return 2
  fi
  echo "Capture complete with no gaps. Review for hostnames/image names"
  echo "before committing to a public repository."
}

case "${1:-capture}" in
  check)   cmd_check ;;
  prep)    cmd_prep ;;
  capture) cmd_capture ;;
  *) echo "usage: $0 [check|prep|capture]" >&2; exit 1 ;;
esac
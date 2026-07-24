#!/usr/bin/env bash
#
# capture-fixtures.sh — prickle-exporter (Starkdrift) test fixture capture
#
# Captures /proc, cgroup v2, Docker API, and NVIDIA fixture data from a live
# host into a directory tree that mirrors real filesystem layout, so Go tests
# can point their configurable fsroot ("/proc", "/sys", "/sys/fs/cgroup"
# prefixes) directly at the capture.
#
# Usage:
#   sudo ./capture-fixtures.sh              # capture into ./prickle-fixtures-<host>-<date>/
#   sudo OUT_DIR=/tmp/fix ./capture-fixtures.sh
#
# Run as root: fdinfo/cgroup files of other processes are not readable
# otherwise. The script never writes outside OUT_DIR and never modifies
# system state (read-only, like prickle itself).
#
# NOTE ON kind vs k3s: prefer k3s for kubepods captures. kind runs its node
# inside a Docker container with a nested cgroup namespace, so the captured
# tree will NOT look like a real Kubernetes host. k3s installs directly on
# the host and produces the genuine kubepods.slice layout.
#
# BEFORE COMMITTING to a public repo: review the capture. It contains
# hostnames, container/image names, process names, and PIDs from this host.

set -uo pipefail    # intentionally no -e: partial capture is better than none

# ---------------------------------------------------------------------------
# Setup
# ---------------------------------------------------------------------------

HOST=$(hostname -s 2>/dev/null || echo unknown)
STAMP=$(date +%Y%m%d)
OUT_DIR=${OUT_DIR:-./prickle-fixtures-${HOST}-${STAMP}}
CAPTURED=0
SKIPPED=0

say()  { printf '\n==> %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
warn() { printf '    [skip] %s\n' "$*"; SKIPPED=$((SKIPPED+1)); }

# Copy one file, preserving its absolute path under $OUT_DIR.
# Uses cat, not cp: /proc and /sys files report size 0 and cp mangles them.
grab() {
  local src=$1 dst
  [[ -r $src ]] || { warn "unreadable: $src"; return 1; }
  dst="$OUT_DIR$src"
  mkdir -p "$(dirname "$dst")"
  if cat "$src" > "$dst" 2>/dev/null; then
    CAPTURED=$((CAPTURED+1))
  else
    rm -f "$dst"
    warn "read failed: $src"
    return 1
  fi
}

# Capture a fixed set of cgroup files from one cgroup directory.
CG_FILES="cgroup.type cgroup.procs cpu.stat cpu.max cpu.weight
          memory.current memory.max memory.min memory.low memory.high
          memory.stat memory.pressure io.stat pids.current pids.max"
grab_cgroup_dir() {
  local dir=$1 f
  for f in $CG_FILES; do
    [[ -e $dir/$f ]] && grab "$dir/$f"
  done
}

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "WARNING: not running as root — fdinfo/cgroup capture for other" >&2
  echo "processes will be incomplete. Re-run with sudo for a full capture." >&2
fi

mkdir -p "$OUT_DIR/meta"

# ---------------------------------------------------------------------------
# Metadata — record everything needed to label the fixture set
# ---------------------------------------------------------------------------
say "Capturing host metadata"
{
  echo "capture_date: $(date -Is)"
  echo "hostname: $HOST"
  echo "kernel: $(uname -r)"
  echo "uname: $(uname -a)"
  echo "cgroup_fstype: $(stat -fc %T /sys/fs/cgroup/ 2>/dev/null || echo unknown)"
} > "$OUT_DIR/meta/host.txt"
[[ -r /etc/os-release ]] && cat /etc/os-release > "$OUT_DIR/meta/os-release.txt"
command -v docker >/dev/null 2>&1 && docker version > "$OUT_DIR/meta/docker-version.txt" 2>&1
command -v nvidia-smi >/dev/null 2>&1 && \
  nvidia-smi --query-gpu=driver_version --format=csv,noheader > "$OUT_DIR/meta/nvidia-driver.txt" 2>&1
ls -l /sys/class/drm > "$OUT_DIR/meta/drm-listing.txt" 2>&1 || true
note "cgroup fstype: $(stat -fc %T /sys/fs/cgroup/ 2>/dev/null || echo unknown) (cgroup2fs = v2, tmpfs = v1/hybrid)"

# ---------------------------------------------------------------------------
# Phase 1 fixtures — host /proc
# ---------------------------------------------------------------------------
say "Capturing /proc host metrics"
for f in stat meminfo diskstats loadavg uptime mounts; do
  grab "/proc/$f"
done
grab /proc/net/dev
for f in cpu memory io; do
  grab "/proc/pressure/$f"
done
# Statfs values can't be file fixtures; record them for reference so the
# Statfs interface stub in tests can use realistic numbers.
df -B1 --output=target,fstype,size,used,avail 2>/dev/null \
  > "$OUT_DIR/meta/statfs-reference.txt" || true

# ---------------------------------------------------------------------------
# Phase 2 fixtures — cgroup v2 trees (Docker, kubepods/containerd, CRI-O)
# Directory names are preserved: they ARE the identity-extraction test data.
# ---------------------------------------------------------------------------
say "Capturing Docker cgroup scopes"
found_docker=0
# systemd cgroup driver layout
while IFS= read -r d; do
  grab_cgroup_dir "$d"; found_docker=1
done < <(find /sys/fs/cgroup/system.slice -maxdepth 1 -type d -name 'docker-*.scope' 2>/dev/null)
# cgroupfs driver layout (older/non-systemd setups)
while IFS= read -r d; do
  grab_cgroup_dir "$d"; found_docker=1
done < <(find /sys/fs/cgroup/docker -mindepth 1 -maxdepth 1 -type d 2>/dev/null)
[[ $found_docker -eq 0 ]] && warn "no Docker container cgroups found — start 2-3 containers first"

say "Capturing kubepods cgroup tree (k3s / containerd / CRI-O)"
if [[ -d /sys/fs/cgroup/kubepods.slice ]]; then
  # Capture every level — the directory names ARE the identity test data:
  #   kubepods.slice/.../pod<uid>, cri-containerd-<hex>.scope, crio-<hex>.scope
  while IFS= read -r d; do
    grab_cgroup_dir "$d"
  done < <(find /sys/fs/cgroup/kubepods.slice -type d 2>/dev/null)
else
  warn "no kubepods.slice — install k3s and run a pod first (see header note re: kind)"
fi

say "Capturing Docker API response"
if [[ -S /var/run/docker.sock ]] && command -v curl >/dev/null 2>&1; then
  mkdir -p "$OUT_DIR/docker-api"
  if curl -sf --unix-socket /var/run/docker.sock \
       http://localhost/containers/json > "$OUT_DIR/docker-api/containers.json"; then
    CAPTURED=$((CAPTURED+1))
  else
    warn "Docker socket query failed"
  fi
else
  warn "no Docker socket or curl missing"
fi

# ---------------------------------------------------------------------------
# Phase 3 fixtures — NVIDIA (nvidia-smi CSV) and per-process GPU fdinfo.
# These nvidia-smi CSVs are the reference the NVML path must reproduce
# identically (SPEC: the two sources must emit the same metric output for the
# same GPU). NVML itself is a C library call, not a file read, so it cannot be
# captured here — it is verified only on real hardware against these fixtures.
# ---------------------------------------------------------------------------
say "Capturing nvidia-smi output"
if command -v nvidia-smi >/dev/null 2>&1; then
  mkdir -p "$OUT_DIR/nvidia"
  # -L lists both whole-GPU and MIG instance UUIDs — the mig_uuid source.
  nvidia-smi -L > "$OUT_DIR/nvidia/gpus.txt" 2>&1
  nvidia-smi \
    --query-gpu=index,uuid,name,mig.mode.current,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw \
    --format=csv,noheader,nounits > "$OUT_DIR/nvidia/query-gpu.csv" 2>&1
  # On MIG-partitioned cards gpu_uuid here is the MIG instance UUID (mig_uuid).
  nvidia-smi \
    --query-compute-apps=gpu_uuid,pid,process_name,used_gpu_memory \
    --format=csv,noheader,nounits > "$OUT_DIR/nvidia/query-compute-apps.csv" 2>&1
  CAPTURED=$((CAPTURED+3))
  # MIG topology (datacenter cards only): GPU-instance / compute-instance layout.
  if nvidia-smi mig -lgi >/dev/null 2>&1; then
    nvidia-smi mig -lgi > "$OUT_DIR/nvidia/mig-gi.txt" 2>&1
    nvidia-smi mig -lci > "$OUT_DIR/nvidia/mig-ci.txt" 2>&1
    CAPTURED=$((CAPTURED+2))
    note "MIG partitioning detected — captured GI/CI topology"
  else
    note "no MIG partitioning on this host (expected on consumer cards)"
  fi
  if [[ ! -s $OUT_DIR/nvidia/query-compute-apps.csv ]]; then
    note "query-compute-apps.csv is empty — start a GPU workload and re-run"
    note "for a fixture with real per-process rows"
  fi
else
  warn "nvidia-smi not found"
fi

# /dev/dri/* fdinfo is the Intel DRM source and the AMD per-process source
# (AMD also needs the sysfs capture below); /dev/nvidia* covers NVIDIA.
say "Capturing per-process GPU fdinfo (/dev/dri/*, /dev/nvidia*)"
gpu_pids=0
for pid_dir in /proc/[0-9]*; do
  pid=${pid_dir##*/}
  hit=0
  for fd_link in "$pid_dir"/fd/*; do
    tgt=$(readlink "$fd_link" 2>/dev/null) || continue
    case $tgt in
      /dev/dri/*|/dev/nvidia*)
        grab "$pid_dir/fdinfo/${fd_link##*/}"
        hit=1
        ;;
    esac
  done
  if [[ $hit -eq 1 ]]; then
    gpu_pids=$((gpu_pids+1))
    grab "$pid_dir/cgroup"          # maps the process to its container cgroup
    # SPEC: the per-process `command` label is sourced from the exe symlink
    # basename, never comm (truncated, forgeable), and PID is never emitted.
    # Capture exe as text (tests stub Readlink); comm is deliberately NOT
    # captured — no parser consumes it.
    exe=$(readlink "$pid_dir/exe" 2>/dev/null) || exe="<unreadable>"
    mkdir -p "$OUT_DIR$pid_dir"
    printf '%s\n' "$exe" > "$OUT_DIR$pid_dir/exe.link"
  fi
done
[[ $gpu_pids -eq 0 ]] && warn "no processes with GPU device fds found — start a GPU workload"
note "processes with GPU fds captured: $gpu_pids"

# ---------------------------------------------------------------------------
# AMD sysfs — no-op on this host, ready for a rented MI300 hour
# ---------------------------------------------------------------------------
say "Capturing AMD sysfs (if present)"
found_amd=0
for card in /sys/class/drm/card[0-9]*; do
  dev="$card/device"
  [[ -e $dev/gpu_busy_percent ]] || continue
  found_amd=1
  for f in gpu_busy_percent mem_info_vram_used mem_info_vram_total; do
    grab "$dev/$f"
  done
  for hw in "$dev"/hwmon/hwmon*; do
    for f in temp1_input power1_average power1_cap name; do
      [[ -e $hw/$f ]] && grab "$hw/$f"
    done
  done
done
[[ $found_amd -eq 0 ]] && note "no AMD GPUs on this host (expected)"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
say "Done"
note "files captured: $CAPTURED   skipped: $SKIPPED"
note "output: $OUT_DIR"
tarball="${OUT_DIR%/}.tar.gz"
if tar -czf "$tarball" -C "$(dirname "$OUT_DIR")" "$(basename "$OUT_DIR")" 2>/dev/null; then
  note "tarball: $tarball"
fi
echo
echo "Reminder: review the capture for hostnames, container/image names, and"
echo "process names before committing fixtures to a public repository."
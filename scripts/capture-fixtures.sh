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

CG_FILES="cgroup.type cgroup.procs cpu.stat cpu.max cpu.weight
          memory.current memory.max memory.min memory.low memory.high
          memory.stat memory.pressure io.stat pids.current pids.max"
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
kubepods_pod_count() {
  find /sys/fs/cgroup/kubepods.slice -maxdepth 3 -type d -name 'kubepods-pod*' \
       -o -maxdepth 3 -type d -name '*pod*.slice' 2>/dev/null | wc -l
}
nvidia_present()      { have nvidia-smi; }
nvidia_mig_capable()  { nvidia-smi mig -lgip >/dev/null 2>&1; }
nvidia_mig_enabled()  { nvidia-smi -L 2>/dev/null | grep -q 'MIG '; }
nvidia_compute_apps() {
  nvidia-smi --query-compute-apps=pid --format=csv,noheader 2>/dev/null | grep -c .
}
amd_present() { compgen -G '/sys/class/drm/card*/device/gpu_busy_percent' >/dev/null; }
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
  else gap "no kubepods.slice pods — Phase 2 kubernetes fixtures will be EMPTY"; fi

  if nvidia_present; then
    ok "nvidia-smi present (driver $(nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null | head -1))"
    if nvidia_mig_capable; then
      if nvidia_mig_enabled; then ok "MIG enabled — MIG UUIDs will be captured"
      else gap "MIG-capable GPU but MIG is DISABLED — the mig_uuid fixtures will be EMPTY (run prep, or enable manually)"; fi
    else
      note "GPU is not MIG-capable (consumer/workstation card) — MIG fixtures not expected here"
    fi
    n=$(nvidia_compute_apps)
    if [[ $n -gt 0 ]]; then ok "GPU compute apps running: $n — query-compute-apps will have rows"
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
    prof=$(nvidia-smi mig -lgip 2>/dev/null | awk '/MIG [0-9]+g/{print $NF; exit}')
    nvidia-smi mig -cgi "${prof:-19},${prof:-19}" -C >/dev/null 2>&1 \
      || warn "MIG instance creation failed — check 'nvidia-smi mig -lgip'"
    systemctl start nvidia-persistenced >/dev/null 2>&1
  fi

  if nvidia_present && [[ $(nvidia_compute_apps) -eq 0 ]]; then
    say "Starting a GPU workload"
    local mig_uuid
    mig_uuid=$(nvidia-smi -L | grep -oP 'MIG-[0-9a-f-]+' | head -1)
    if have dcgmproftester12 || have dcgmproftester; then
      local dp; dp=$(have dcgmproftester12 && echo dcgmproftester12 || echo dcgmproftester)
      CUDA_VISIBLE_DEVICES=${mig_uuid:-0} nohup "$dp" --no-dcgm-validation -t 1004 -d 600 \
        >/tmp/gpu-workload.log 2>&1 &
      note "$dp running (10 min)${mig_uuid:+ on $mig_uuid}"
    elif have python3 && python3 -c 'import torch' 2>/dev/null; then
      CUDA_VISIBLE_DEVICES=${mig_uuid:-0} nohup python3 -c '
import torch
a = torch.rand(8192, 8192, device="cuda")
while True: a = a @ a' >/tmp/gpu-workload.log 2>&1 &
      note "torch matmul loop running${mig_uuid:+ on $mig_uuid}"
    else
      warn "no dcgmproftester or torch — start a GPU workload manually (e.g. gpu-burn)"
    fi
    sleep 10
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

  say "Docker cgroup scopes"
  local found=0 d
  while IFS= read -r d; do grab_cgroup_dir "$d"; found=1; done \
    < <(find /sys/fs/cgroup/system.slice -maxdepth 1 -type d -name 'docker-*.scope' 2>/dev/null)
  while IFS= read -r d; do grab_cgroup_dir "$d"; found=1; done \
    < <(find /sys/fs/cgroup/docker -mindepth 1 -maxdepth 1 -type d 2>/dev/null)
  [[ $found -eq 0 ]] && gap "no Docker container cgroups captured"

  say "kubepods cgroup tree"
  if [[ -d /sys/fs/cgroup/kubepods.slice ]]; then
    while IFS= read -r d; do grab_cgroup_dir "$d"; done \
      < <(find /sys/fs/cgroup/kubepods.slice -type d 2>/dev/null)
  else gap "no kubepods.slice captured"; fi

  say "Docker API"
  if [[ -S /var/run/docker.sock ]] && have curl; then
    mkdir -p "$OUT_DIR/docker-api"
    curl -sf --unix-socket /var/run/docker.sock http://localhost/containers/json \
      > "$OUT_DIR/docker-api/containers.json" && CAPTURED=$((CAPTURED+1))
    [[ $(cat "$OUT_DIR/docker-api/containers.json" 2>/dev/null) == "[]" ]] \
      && gap "docker-api/containers.json is an empty list"
  else warn "no Docker socket or curl missing"; fi

  say "NVIDIA"
  if nvidia_present; then
    mkdir -p "$OUT_DIR/nvidia"
    nvidia-smi -L > "$OUT_DIR/nvidia/gpus.txt" 2>&1
    nvidia-smi --query-gpu=index,uuid,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw \
      --format=csv,noheader,nounits > "$OUT_DIR/nvidia/query-gpu.csv" 2>&1
    nvidia-smi --query-compute-apps=gpu_uuid,pid,process_name,used_gpu_memory \
      --format=csv,noheader,nounits > "$OUT_DIR/nvidia/query-compute-apps.csv" 2>&1
    nvidia-smi mig -lgip > "$OUT_DIR/nvidia/mig-profiles.txt" 2>&1
    nvidia-smi mig -lgi  > "$OUT_DIR/nvidia/mig-gi.txt" 2>&1
    nvidia-smi mig -lci  > "$OUT_DIR/nvidia/mig-ci.txt" 2>&1
    CAPTURED=$((CAPTURED+6))
    nvidia_mig_capable && ! nvidia_mig_enabled \
      && gap "MIG-capable GPU captured with MIG disabled"
    [[ ! -s $OUT_DIR/nvidia/query-compute-apps.csv ]] \
      && gap "query-compute-apps.csv is empty (no GPU workload was running)"
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
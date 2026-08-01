<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/prickle-logo-dark.svg">
    <img src="assets/prickle-logo.svg" alt="prickle-exporter" width="440">
  </picture>
</p>

<p align="center">
  <em>A Prometheus exporter for host, container and GPU metrics.<br>
  One Go binary. Standard library only. Strictly read-only.</em>
</p>

<p align="center">
  <a href="https://github.com/starkdrift/prickle-exporter/actions/workflows/ci.yml"><img src="https://github.com/starkdrift/prickle-exporter/actions/workflows/ci.yml/badge.svg" alt="ci"></a>
  <img src="https://img.shields.io/badge/license-Apache--2.0-2B3044" alt="Apache-2.0">
  <img src="https://img.shields.io/badge/go-1.26-2B3044" alt="Go 1.26">
  <img src="https://img.shields.io/badge/dependencies-0-2B3044" alt="Zero dependencies">
  <img src="https://img.shields.io/badge/status-phase%204-F0A202" alt="Phase 4">
</p>

---

## What it is

`prickle-exporter` builds one binary, `prickle`, that exposes Prometheus metrics
for a Linux host, the containers on it, and the GPUs in it — the three layers you
need together to answer "which tenant is using that accelerator, and is the node
underneath it healthy?"

It exists because that answer normally takes three exporters with three
different label conventions. `prickle` uses one closed set of identity labels
across all three layers, so a GPU series joins to a container series joins to a
node series without relabeling.

Three properties are non-negotiable, and they are why the code looks the way it
does:

- **Zero third-party dependencies.** Including `prometheus/client_golang` — the
  text exposition is hand-written in [internal/exposition/](internal/exposition/)
  and gated on `promtool check metrics`. `go.sum` is empty and CI fails if it
  stops being empty.
- **Strictly read-only.** The exporter never writes to `/proc`, `/sys` or
  cgroups, and never calls an NVML function that mutates device state — no MIG
  reconfiguration, no clock or persistence-mode changes, no ECC toggles.
  Remediation belongs to something else.
- **A slow collector can never stall a scrape.** A sampler goroutine polls on an
  interval and swaps a fully rendered buffer under a mutex; `/metrics` serves the
  last completed render.

The full contract is [SPEC.md](SPEC.md). It is frozen: code follows the spec, and
changing a decision means editing SPEC.md first, in its own commit.

## Status

**Phase 4.** Host, container and NVIDIA GPU collectors are implemented and
tested against captured fixture trees, with per-collector timeouts, cardinality
caps and self-instrumentation. The container collector reads both cgroup
drivers and all three CRI runtimes it names. AMD is specified but not written —
no capture exists for it. Intel is out of scope (SPEC §Collectors). Multi-GPU
hosts remain unverified.

| Phase | Scope | State |
|---|---|---|
| 1 | Host — CPU, memory, disks, network, load, PSI, filesystems | **shipped** |
| 2 | Containers — cgroup v2 and v1, Docker/containerd/CRI-O/Kubernetes identity | **shipped** — with [coverage gaps](#coverage-gaps) worth reading before you deploy it |
| 3 | GPU — NVIDIA (NVML + `nvidia-smi`), AMD sysfs + DRM fdinfo | **NVIDIA shipped**; [AMD unimplemented](#coverage-gaps-1), Intel out of scope |
| 4 | Per-collector timeouts, cardinality caps, self-instrumentation | **shipped** |
| 5 | Distribution — systemd units, Helm chart, Docker, four Grafana dashboards | planned |

Linux only. **cgroup v2 and v1** are both read — v2 is the primary hierarchy and
`prickle diagnose` says so plainly rather than leaving you with an empty scrape.
The package still builds on macOS so parser tests run there; `Statfs` is stubbed
out and no shipped binary reaches it.

## Quick start

Requires Go 1.26. There is nothing to fetch — the module has no dependencies.

```sh
git clone https://github.com/starkdrift/prickle-exporter
cd prickle-exporter
CGO_ENABLED=0 go build -o prickle ./cmd/prickle
./prickle
```

Then scrape it:

```sh
curl -s localhost:10047/metrics | head
```

Port **10047** is fixed by [SPEC.md §Identity](SPEC.md#identity). `-web.listen-address`
exists for when something else on your workstation already holds it — don't
change it in anything that ships.

Stamp a version into the binary with:

```sh
go build -ldflags "-X main.version=$(git describe --tags --always)" -o prickle ./cmd/prickle
```

### Working from source

[scripts/dev-run.sh](scripts/dev-run.sh) wraps the common loops with
dev-friendly defaults — debug logging, a 2s sample interval, no root needed:

```sh
./scripts/dev-run.sh              # serve on :10047 until Ctrl-C
./scripts/dev-run.sh fixture      # same, but read a captured fixture tree
./scripts/dev-run.sh diagnose     # what this host can and cannot be read from
./scripts/dev-run.sh scrape       # start, scrape once, print, promtool, stop
```

See [scripts/README.md](scripts/README.md) for the details, including why
`fixture` mode still reports *your* filesystems.

## `prickle diagnose`

Run this first when a scrape comes back empty or a family is missing. It reports
what the exporter can actually read on this host — by reading it, not by
stat'ing it, because procfs files report size 0 and permission failures surface
on open.

```
$ prickle diagnose
prickle dev

Filesystem roots
  procfs    /proc
  sysfs     /sys
  cgroupfs  /sys/fs/cgroup

node label: fedora

cgroup
  v2 (unified) — supported.

Phase 1 sources
  /proc/stat             ok
  /proc/meminfo          ok
  /proc/diskstats        ok
  /proc/net/dev          ok
  /proc/loadavg          ok
  /proc/pressure/cpu     ok
  /proc/pressure/memory  ok
  /proc/pressure/io      ok
  /proc/mounts           ok

Phase 2 containers
  cgroup root: /sys/fs/cgroup — ok, 11 top-level cgroups
  containers found: 16 (docker 3, containerd 13, crio 0)
  Docker enrichment: off. Names and images are absent from
  prickle_container_info; -collector.container.docker-socket turns it on.

Phase 3 GPU
  this binary: prickle (static) — nvidia-smi only; a static binary cannot dlopen NVML
  live source: smi
  GPUs: 1, MIG instances: 2
  per-process attribution: off (-collector.gpu.per-process turns it on).
  AMD is SPEC.md §Collectors scope but unimplemented: no capture
  exists for it, so an AMD host reports nothing. Intel is out of scope.

host collector: 278 series in 672µs
container collector: 403 series in 1.4ms
gpu collector: 12 series in 41ms
```

On a host with no NVIDIA GPU the GPU section says so, and says why each source
declined — "NVML failed to load" and "there is no GPU here" need different
responses from you, and an empty section distinguishes neither.

When it reports no containers on a host that is running some, it says which
cause to check: a tree this process cannot read, or a runtime layout Phase 2
does not cover. cgroup v1 used to head that list and no longer does — both
hierarchies are read.

It takes the same flags as the exporter, so it diagnoses exactly the
configuration you would run with — including `-path.rootfs`.

## Metrics

Every metric is prefixed `prickle_`, never abbreviated.

### Host — Phase 1

~280 series on a plain host, from these families:

| Source | Families |
|---|---|
| `/proc/stat` | `prickle_host_cpu_seconds_total`, `boot_time_seconds`, `context_switches_total`, `interrupts_total`, `softirqs_total`, `forks_total`, `procs_running`, `procs_blocked` |
| `/proc/meminfo` | `prickle_host_memory_*_bytes` — one gauge per kernel field, `snake_case`d, plus `memory_huge_pages{state=…}` |
| `/proc/diskstats` | `prickle_host_disk_{reads,writes,discards}_*`, `*_bytes_total`, `*_seconds_total`, `io_now`, `io_time_*`, `disk_info` |
| `/proc/net/dev` | `prickle_host_network_{receive,transmit}_*_total` |
| `/proc/loadavg` | `prickle_host_load1`, `load5`, `load15` |
| `/proc/pressure/*` | `prickle_host_pressure_stalled_seconds_total{resource,kind}` |
| `/proc/mounts` + `statfs` | `prickle_host_filesystem_{size,free,avail}_bytes`, `files`, `files_free`, `readonly`, `error`, `filesystem_info` |
| the exporter itself | `prickle_build_info`, `prickle_render_timestamp_seconds`, `prickle_collector_duration_seconds`, `prickle_collector_errors_total`, `prickle_collector_success` |

The full rendered output for the captured H200 fixture is the golden file
[internal/collector/host/testdata/golden/host.prom](internal/collector/host/testdata/golden/host.prom).

### Containers — Phase 2

A walk of the cgroup v2 tree, about 25 series per container. Identity comes out
of the directory names the runtimes create: `docker-<hex>.scope`,
`cri-containerd-<hex>.scope`, `crio-<hex>.scope`, and the pod slices under
`kubepods.slice`.

| Source | Families |
|---|---|
| `cpu.stat` | `prickle_container_cpu_usage_seconds_total`, `cpu_seconds_total{mode}`, `cpu_periods_total`, `cpu_throttled_periods_total`, `cpu_throttled_seconds_total` |
| `cpu.max`, `cpu.weight` | `prickle_container_cpu_limit_cores`, `cpu_weight` |
| `memory.current`, `memory.{max,high,min,low}` | `prickle_container_memory_usage_bytes`, `memory_limit_bytes`, `memory_high_bytes`, `memory_min_bytes`, `memory_low_bytes` |
| `memory.stat` | `prickle_container_memory_{anon,file,inactive_file,kernel_stack,page_tables,slab,socket,shmem,unevictable}_bytes`, `memory_page_faults_total`, `memory_major_page_faults_total` |
| `io.stat` | `prickle_container_io_{read,written,discarded}_bytes_total`, `io_{reads,writes,discards}_completed_total`, all `{device}` |
| `pids.current`, `pids.max` | `prickle_container_processes`, `processes_limit` |
| `{cpu,io,memory}.pressure` | `prickle_container_pressure_stalled_seconds_total{resource,kind}` |
| identity | `prickle_container_info` |

Three things about this collector are worth knowing before you query it:

- **Only leaf containers are emitted.** The pod, QoS and root slices above them
  are walked for identity and never sampled — emitting them alongside their
  children would make `sum(prickle_container_memory_usage_bytes)` count every
  byte two or three times. A pod total is `sum by (pod)`.
- **An unset limit is an absent series, not a sentinel.** `memory.max` reading
  `max` produces no `memory_limit_bytes` sample at all, rather than the kernel's
  internal 9.2-exabyte sentinel on every unconstrained container.
- **`pod` holds the pod's UID, not its name,** and `namespace` is not emitted at
  all. The cgroup tree is all the kernel knows, and it stores neither — see
  [the label contract](#the-label-contract) below.

The full rendered output for the captured fixture tree is
[internal/collector/container/testdata/golden/container.prom](internal/collector/container/testdata/golden/container.prom).

#### Coverage gaps

SPEC §Testing rules forbids inventing a file format or a path shape: where no
capture exists, the code says so instead of guessing. These are the shapes
Phase 2 does not cover, and the two that can leave you with an empty scrape are
first.

| Gap | Effect | What closes it |
|---|---|---|
| **Docker with the cgroupfs driver** — `/sys/fs/cgroup/docker/<hex>/` | **No containers reported at all** on such a host. | A capture from a host running `"exec-opts": ["native.cgroupdriver=cgroupfs"]`. |
| **Kubelet with the cgroupfs driver** — `kubepods/besteffort/pod<uid>/<hex>` | **No containers reported** on such a node. | A capture from a node with `cgroupDriver: cgroupfs`. |
| CRI-O — `crio-<hex>.scope` | Directory-name parse is unit-tested; nothing beyond it is. | A capture from a CRI-O host. |
| Guaranteed pods — `kubepods-pod<uid>.slice`, no QoS component | Same: name parse only. The rental ran none. | A pod with equal requests and limits during capture. |
| A container with a CPU quota | Fixtures show none — every `cpu.max` in the capture is `max 100000` — so `cpu_limit_cores` and the throttling counters are covered by hand-written trees in `cpu_test.go` instead. Their values are asserted; only their provenance is synthetic. | `docker run --cpus=2`, or a pod with a CPU limit, during capture. |
| `cpu.pressure`, `io.pressure` | **Implemented and emitted.** The capture script collects only `memory.pressure`, but the format is byte-identical to it and to the `/proc/pressure/*` files Phase 1 does capture, so a hand-written tree covers them in `TestPerCgroupPressure`. | Adding the two files to `capture-fixtures.sh`. |

If `prickle diagnose` reports no containers on a host that is running some, the
first two rows are the likeliest reason after cgroup v1 and a permissions
problem — it says as much. The authoritative list, with what each row is tested
by, lives beside the fixtures in
[internal/collector/container/testdata/README.md](internal/collector/container/testdata/README.md#coverage-gaps).

```promql
# The containers closest to their memory limit. The limit series is absent for
# unlimited containers, so the division drops them rather than dividing by a
# sentinel — which is the point of emitting nothing.
topk(10,
  prickle_container_memory_usage_bytes / on (node, container) prickle_container_memory_limit_bytes
)

# CPU throttling: the fraction of enforcement periods in which a container hit
# its quota. Anything sustained above zero is a container that needs more CPU
# than it is allowed.
rate(prickle_container_cpu_throttled_periods_total[5m])
  / rate(prickle_container_cpu_periods_total[5m])

# Container memory with the name and image grafted on for display. Both live
# on the _info gauge because they are descriptive attributes, and never on a
# hot series — this is the group_left join that puts them back together.
prickle_container_memory_usage_bytes
  * on (node, container) group_left (name, image, runtime) prickle_container_info
```

### GPUs — Phase 3 (NVIDIA only)

About 12 series for one MIG-partitioned card. NVIDIA is served by two
interchangeable implementations behind one interface; **AMD is specified but
not implemented and Intel is out of scope** — see [Coverage gaps](#coverage-gaps-1).

| Source | Families |
|---|---|
| `--query-gpu` | `prickle_gpu_utilization_ratio`, `memory_used_bytes`, `memory_total_bytes`, `temperature_celsius`, `power_watts` |
| `nvidia-smi -L` | `prickle_gpu_mig_enabled`, `mig_info` |
| NVML only | `prickle_gpu_mig_memory_used_bytes`, `mig_memory_total_bytes`, `mig_utilization_ratio` |
| `--query-compute-apps` | `prickle_gpu_process_memory_bytes{command}` — opt-in |
| identity | `prickle_gpu_info`, `prickle_gpu_nvidia_source_info` |

Four things worth knowing:

- **An absent metric means the driver would not say.** `utilization_ratio`
  disappears for the whole card once MIG is enabled — the driver reports `[N/A]`,
  verified on H200 / driver 580. Emitting zero there would read as an idle GPU
  and fire idle-capacity alerts across a MIG fleet. The same rule covers
  `[Not Supported]` and `[Unknown Error]`.
- **MIG instances live in their own families.** A MIG instance's memory is a
  partition of its parent's, so one family holding both would double-count under
  `sum()`. A card's total is `prickle_gpu_memory_used_bytes`; a partition's is
  `prickle_gpu_mig_memory_used_bytes`.
- **The `nvidia-smi` source cannot attribute a process to a MIG instance.**
  `--query-compute-apps` reports the *parent* GPU's UUID for a MIG-resident
  process, so those series carry `gpu_uuid` and no `mig_uuid`. That is coarse,
  not wrong; NVML can do better.
- **No PID, anywhere.** The CSV carries one and the parser discards it at the
  boundary — there is no field in the data model for a PID to live in. Process
  series are keyed on `command`, the basename of the executable path, never the
  truncated and forgeable `comm`.

```promql
# GPU memory pressure per card, with the model name for display.
prickle_gpu_memory_used_bytes / prickle_gpu_memory_total_bytes
  * on (node, gpu_uuid) group_left (name) prickle_gpu_info

# Which command is holding a card. Needs -collector.gpu.per-process.
topk(5, prickle_gpu_process_memory_bytes)

# Whether a scrape came from NVML or the nvidia-smi fallback.
prickle_gpu_nvidia_source_info
```

#### Coverage gaps

| Gap | Effect | What closes it |
|---|---|---|
| **AMD — sysfs + DRM fdinfo** | **No AMD metrics at all.** A third of what SPEC §Collectors assigns to Phase 3. | A capture from an AMD host with a ROCm workload running. `capture-fixtures.sh check` already reports whether one would produce usable `drm-*` fdinfo keys. |
| **Intel — DRM fdinfo** | **Out of scope** as of SPEC §Collectors: no capture host is obtainable, so listing it would be scope on paper and an empty scrape in practice. | A capture. Intel rides the same DRM fdinfo path AMD needs, so reopening it costs a fixture tree, not a redesign. |
| ~~**NVML — the entire path**~~ | **Closed.** Verified on an H100 80GB / driver 580.173.02, Default and MIG mode, 2026-07-29; the hardware test that asserts the two sources agree ships in the package. Still not fixture-testable — a C call is not a file read — so it re-verifies only where a GPU is present. | — |
| Per-MIG memory and utilization from `nvidia-smi` | Absent from that source. No CSV query publishes them, and the human-readable table is not parsed. | Nothing — this is a real limitation of the fallback. NVML supplies them. |
| GPU-instance / compute-instance IDs | Not exposed by either source. Nothing captured joins a MIG UUID to a GI/CI ID, and pairing the two listings would assume they are in the same order. | A capture that joins them, or an NVML-only label — the latter would break the identical-output requirement. |
| A multi-GPU host | Single card captured. Parsers key on UUID rather than position so a second card cannot attach its partitions to the first, but nothing proves it. | A capture from a multi-GPU host. |

### The label contract

This is the part worth reading before you build dashboards on it.

**Identity labels — and only these:** `node`, `namespace`, `pod`, `container`,
`gpu_uuid`, `mig_uuid`. That closed set says *which entity* a sample belongs to,
and it is the same set at every layer, which is what makes a GPU series join to
a pod series.

**Dimensional labels are separate and permitted:** `mode` on CPU time, `cpu` on
per-core series, `device` on disks, `interface` on links, `mountpoint` on
filesystems, `resource`/`kind` on PSI. A dimensional label partitions one metric
across the parts of one entity; it never names an entity another metric could
also be about.

**`pod` carries the pod's UID, and `namespace` is not emitted yet.** This is the
one place the closed set is currently filled with less than you would want, and
it is a limit of the source rather than a choice: a cgroup directory name is
`kubepods-burstable-pod6eb5044d_ef2e_49d1_a9cc_28f4e3fe88a3.slice`, and the
kernel stores no pod name and no namespace anywhere in the tree. The UID is
unescaped back to the form `kubectl get pod -o jsonpath='{.metadata.uid}'`
reports, so it joins to anything that knows about pods. Resolving the name needs
a Kubernetes-aware source, which a cgroup walk is not.

**Descriptive attributes live on `_info` gauges,** never on hot series — join
them with `group_left`. Where a hot series already has a natural key, the
human-readable extras go to the companion: `prickle_host_filesystem_size_bytes`
carries `mountpoint`, and `prickle_host_filesystem_info` carries the `device` and
`fstype` for it.

```promql
# Filesystem fill, with the backing device and fstype grafted on for display.
(
  1 - prickle_host_filesystem_avail_bytes / prickle_host_filesystem_size_bytes
)
* on (node, mountpoint) group_left (device, fstype) prickle_host_filesystem_info
```

**PID never appears** — not as a label, not as a value. That is also why
`/proc/loadavg`'s fifth field is not exposed. Per-process GPU attribution in
Phase 3 is opt-in and keyed on a `command` label taken from the `exe` symlink
basename, never `comm`, which is truncated and forgeable.

A few more example queries:

```promql
# Node CPU utilization. The guest modes are excluded from the denominator, not
# the numerator: /proc/stat already counts guest inside user and guest_nice
# inside nice, so summing all ten modes would double-count them.
1 - sum without (mode) (rate(prickle_host_cpu_seconds_total{mode="idle"}[5m]))
      / sum without (mode) (rate(prickle_host_cpu_seconds_total{mode!~"guest.*"}[5m]))

# Fraction of wall time in which every task was stalled on memory — the
# signal that arrives before the OOM killer does.
rate(prickle_host_pressure_stalled_seconds_total{resource="memory",kind="full"}[5m])

# Data age. A scrape serves the last completed render, so this is how stale
# the sample is, not how slow the request was.
time() - prickle_render_timestamp_seconds
```

## Configuration

All flags, with defaults:

| Flag | Default | Notes |
|---|---|---|
| `-web.listen-address` | `:10047` | Port fixed by SPEC §Identity. |
| `-web.telemetry-path` | `/metrics` | |
| `-sample.interval` | `10s` | How often collectors are polled. Scrapes are served from the last completed pass. |
| `-collector.timeout` | `5s` | Deadline for one collector's pass. |
| `-node` | system hostname | The `node` identity label. **Set this explicitly on Kubernetes** — a pod's view of the hostname is not the node's name. |
| `-path.rootfs` | — | Prefix `/proc`, `/sys` and the cgroup mount with this directory. For fixture trees, or for running in a container with the host filesystem bind-mounted. Wins over the three flags below. |
| `-path.procfs` | `/proc` | |
| `-path.sysfs` | `/sys` | |
| `-path.cgroupfs` | `/sys/fs/cgroup` | cgroup v2 mount point. |
| `-collector.cpu.per-core` | `false` | Opt-in. Costs one series per core per mode — deliberately off so default cardinality doesn't scale with core count on a large GPU node. Exposed as a separate family from the aggregate, which is always on. |
| `-collector.diskstats.ignored-devices` | `^(ram\|loop\|fd\|sr)\d+$` | Regexp. |
| `-collector.netdev.ignored-devices` | *(none)* | Regexp. `^veth` is the usual choice on a Kubernetes node. |
| `-collector.filesystem.excluded-fs-types` | pseudo-filesystems, `overlay`, `squashfs` | Regexp. |
| `-collector.filesystem.excluded-mount-points` | `/dev`, `/proc`, `/sys`, per-container and per-pod mounts | Regexp. |
| `-collector.container` | `true` | Walk the cgroup v2 tree. Set false on a host where you only want node metrics. |
| `-collector.container.docker-socket` | *(none)* | Path to the Docker socket, usually `/var/run/docker.sock`. Enables one GET request per pass for container names and images, which land on `prickle_container_info` and never on a hot series. Empty — the default — opens no socket at all. |
| `-collector.container.docker-timeout` | `2s` | Deadline for that request. A wedged daemon costs the names, not the metrics. |
| `-collector.gpu` | `true` | Expose GPU metrics. NVIDIA only today. |
| `-collector.gpu.nvidia-source` | `auto` | `auto`, `nvml` or `smi`. `auto` tries NVML and falls back to `nvidia-smi`. A debugging flag, not a tuning knob. |
| `-collector.gpu.per-process` | `false` | Also expose per-process GPU memory, keyed on `command` from the executable's basename — never a PID. Opt-in: one series per distinct command per GPU. |
| `-collector.gpu.nvidia-smi-command` | `nvidia-smi` | The binary to spawn, for hosts that keep it outside PATH. |
| `-log.level` | `info` | `debug`, `info`, `warn`, `error`. |
| `-version` | | Print version and exit. |

Regexps are compiled at startup, so a typo fails immediately rather than at the
first scrape.

## NVIDIA: two builds

`dlopen` needs cgo *and* a dynamically linked binary — a fully static binary
cannot `dlopen` at all. So NVIDIA support ships as two artifacts:

| Artifact | Build | NVIDIA source |
|---|---|---|
| `prickle` | `CGO_ENABLED=0`, pure Go, static | `nvidia-smi` CSV subprocess |
| `prickle-nvml` | `-tags nvml`, cgo, dynamically linked | `dlopen` of `libnvidia-ml.so.1` — the preferred path |

NVML is preferred: richer data, no per-scrape process spawn, no CSV parsing, and
the only reliable source of MIG topology. The `nvidia-smi` path is a supported
fallback, not a deprecated one — it is what works inside slim containers that
lack the driver libraries, and it stays tested. Selection is automatic (attempt
the NVML load once at startup, fall back silently, record the live source on an
`_info` gauge); `-nvidia-source={auto,nvml,smi}` forces one for debugging.

Neither build links NVIDIA libraries at compile time, so the zero-dependency
rule holds for both. Both sources sit behind one `nvidiaSource` interface and
**must emit identical metric output for the same GPU** — a hardware test asserts
it.

### Enable persistence mode if you deploy the `nvidia-smi` path

Measured on an idle H100, driver 580.173.02, with **persistence mode disabled**:
a single `nvidia-smi` query costs **2.7–3.5 s**, because the driver tears down
its state when the last client exits and rebuilds it for the next one. This
source spawns several queries per pass, so it overruns the default 5 s
`-collector.timeout` and reports a killed subprocess **on every scrape**.

Turning persistence mode on took the same pass from **5.4 s to 61 ms**:

```sh
nvidia-smi -pm 1          # or run nvidia-persistenced
```

`prickle diagnose` says this when it sees the overrun rather than leaving you to
find it. `prickle-nvml` is not affected — it holds NVML open across passes,
which keeps the driver initialised for the same reason persistence mode does.
Raising `-collector.timeout` also stops the errors, but hides the latency
instead of removing it.

Both are published per architecture from `v0.3.0` on. Dynamic linking costs
`prickle-nvml` a glibc floor — it is built on glibc 2.39 and needs at least that
on the host — which is the one reason to prefer `prickle` on a machine that does
have a GPU. Installing `prickle-nvml` where NVML cannot load is otherwise
harmless: it falls back to the `nvidia-smi` path by itself, and `prickle
diagnose` names the live source and why the other declined.

The hardware test lives in
[internal/collector/gpu/nvml_hardware_test.go](internal/collector/gpu/nvml_hardware_test.go)
and skips itself wherever NVML does not load, so `go test -tags nvml` stays
green on a laptop. It was run on an **H100 80GB** (2026-07-29) and an **H200 141GB**
(2026-07-30), driver 580.173.02, in both Default and MIG mode, and it found
three disagreements that no fixture could have:

| Found | Effect had it shipped |
|---|---|
| NVML counted driver-reserved memory as used | `prickle_gpu_memory_used_bytes` 480 MiB higher per card from `prickle-nvml` than from `prickle` — every memory panel and capacity alert shifting with the artifact deployed |
| NVML *derived* a MIG profile name instead of reading it — three plausible rules, each wrong on some card | The `profile` label on `prickle_gpu_mig_info` differing between the two binaries for the same card, for **every profile an H200 offers** |
| A second GPU collector in one process got an already-closed NVML handle | `prickle diagnose` reporting `NVML source is closed` on a host where NVML worked perfectly |

All three are fixed, and each has a test that fails if it comes back. Two card
classes is not redundancy: the C struct layouts the NVML build declares are
only known not to be H100-shaped by coincidence because a second card agrees
with them.

Both are read-only. Every NVML symbol bound is a `Get`, resolved by its
versioned name (`nvmlDeviceGetComputeRunningProcesses_v3`) so the struct layouts
are fixed by NVIDIA's own ABI contract rather than by hope. Nothing from the
`nvmlDeviceSet*` or `nvmlDeviceClear*` families is resolved at all.

## Development

One command is the whole pre-commit checklist, and CI runs the same script:

```sh
./ci/check.sh
```

It runs `gofmt -l`, `go vet`, `go test -race ./...`, compiles and vets the
`-tags nvml` build (which no other step sees), then gates on: an empty
`go.sum` and no `require` block, an SPDX header in every `.go` file, `promtool
check metrics` on every golden file, and greps for denied names and abbreviated
metric prefixes.

The test run is under the race detector because the sampler swaps a fully
rendered buffer under a mutex while `net/http` serves the previous one from
another goroutine — that non-blocking swap is the architecture's load-bearing
claim, and nothing else in the checklist can falsify it. The detector needs a C
toolchain, which is the one thing `ci/check.sh` wants that the release build
(`CGO_ENABLED=0`, static) deliberately does not: the gate tests the source, the
release builds the artifact.

`promtool` is required, not optional. Get the same pinned version CI uses:

```sh
./ci/install-promtool.sh          # checksum-verified, into ./bin
export PATH="$PWD/bin:$PATH"
```

> **Known gap:** `ci/denied-names.txt` is currently empty, so that gate reports
> `VACUOUS` and protects nothing until the names discarded during naming are
> filled in.

`ci/check.sh` is hermetic and stays that way. The one check that needs the
network lives on its own:

```sh
./ci/check-port-registration.sh
```

It confirms port 10047 is still registered to this project on the [Prometheus
default-port wiki](https://github.com/prometheus/prometheus/wiki/Default-port-allocations)
— a page anyone can edit, in someone else's repository, that sends no
notification if our row is reassigned or dropped. The port is read out of
SPEC.md rather than hardcoded, so the check can't silently drift from the spec.
It exits `0` registered, `2` missing or reassigned, and `1` for *couldn't tell*
(no network, page moved, payload that isn't the allocation table) — so a flaky
fetch never reads as a lost registration. Run it on a schedule, not before a
commit.

### Fixtures

Every parser is developed against a captured fixture tree under `testdata/`,
laid out mirroring real paths so [internal/fsroot](internal/fsroot/) points
straight at it. Exposition output is checked against golden files.

**File formats and path shapes are never invented.** If no fixture exists for a
case, stop and capture one with
[scripts/capture-fixtures.sh](scripts/capture-fixtures.sh) — it dumps the exact
`/proc`, `/sys`, cgroup, Docker-API and GPU vendor-tool output a real host
exposes. Capture hosts are usually rented by the hour, so the script has `check`
and `prep` subcommands to make sure the interesting state is live *before* you
capture and destroy the box. Read [scripts/README.md](scripts/README.md) before
renting anything.

Synthetic fixtures are allowed only where hardware access is pending, and must
be marked synthetic in a README beside them.

NVML is the one path that cannot be fixture-tested — it's a C library call, not
a file read. Unit tests exercise a fake `nvidiaSource`; its captured
`nvidia-smi` fixtures remain the reference for what the NVML path must report.

### Layout

```
cmd/prickle/          main, flags, diagnose
internal/collector/   the Collector interface
  host/               Phase 1: /proc parsers + fixtures + golden file
internal/exposition/  hand-written Prometheus text format
internal/fsroot/      the /proc, /sys, cgroup prefixes every path goes through
internal/sampler/     poll loop, buffer swap, self-metrics, http.Handler
ci/check.sh           the pre-commit gate
scripts/              dev-run.sh, capture-fixtures.sh
assets/               logo and mark, light and dark
```

## Releases and versioning

Everything is under `internal/`, so there is no importable Go API. SemVer here
applies to the two surfaces that actually exist: the **metrics contract** and
the **command line**. [SPEC.md §Versioning](SPEC.md#versioning) is the full
policy; the short version:

- **major** — a metric renamed or removed, a label key added to or removed from
  an existing series, a flag removed. (Adding a label key is a major, not a
  minor: it breaks every rule that aggregates without `by`.)
- **minor** — a new metric family, collector, flag, or label value.
- **patch** — a wrong value corrected, a parser fix, docs.

Pre-1.0 the minor tracks the roadmap phase, so the version says what is
implemented: `0.1.0` host, `0.2.0` containers, `0.3.0` GPU. **`1.0.0` means the
metrics contract is frozen.** Changes are recorded by hand in
[CHANGELOG.md](CHANGELOG.md).

Git tags are the only source of truth for the version — there is no VERSION
file. Releases carry SLSA build provenance:

```sh
sha256sum -c SHA256SUMS
gh attestation verify prickle_v0.1.0_linux_amd64.tar.gz --repo starkdrift/prickle-exporter
```

## Contributing

Read [SPEC.md](SPEC.md) first, in full — it is short, and it settles most of the
questions a patch would otherwise raise. [CLAUDE.md](CLAUDE.md) is the fast index
plus the traps that are easy to trip on. Then make `./ci/check.sh` pass.

## License

Apache-2.0 — see [LICENSE](LICENSE). Every source file carries an
`SPDX-License-Identifier: Apache-2.0` header, and CI enforces it.

---

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/prickle-mark-dark.svg">
    <img src="assets/prickle-mark.svg" alt="" width="52">
  </picture>
  <br>
  <sub>A <a href="https://github.com/starkdrift">Starkdrift</a> project.</sub>
</p>

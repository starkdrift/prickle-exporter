# Metrics reference

Every family `prickle` can emit, what it is read from, and how much of it
you get by default.

[← README](../README.md)

Every metric is prefixed `prickle_`, never abbreviated.

### How much is exposed

`-metrics.preset` controls the size of the payload. A host emits about **156
families** with everything on, and the four shipped dashboards query **35** of
them, so the default is not "everything the collectors can see":

| Preset | What it exposes |
|---|---|
| `minimal` *(default)* | The families the shipped Grafana dashboards query |
| `full` | Every family the collectors produce |
| `custom` | Whatever `-metrics.include` matches — comma-separated regexps |

```sh
prickle                                     # minimal
prickle -metrics.preset=full                # everything
prickle -metrics.preset=custom \
        -metrics.include='^prickle_gpu_,^prickle_host_load'
```

**Self-metrics are exposed under every preset** — `prickle_collector_*`,
`prickle_build_info`, `prickle_render_timestamp_seconds`. A scrape that has been
reduced must still be able to say so, which is the same reason the cardinality
cap does not apply to them.

Misusing the flags fails at **startup**, not at the first scrape: an unknown
preset, `-metrics.include` without `custom`, `custom` without patterns, or a
regexp that does not compile. An ignored filter would make you believe a metric
was missing from the host rather than from the flag.

`prickle diagnose` reports the active preset, how many families it is exposing
and how many it is withholding — so "my metric disappeared" has an answer that
is not reading the source.

The minimal set is an **explicit list in the code**, not "whatever the
dashboards reference". A test asserts the dashboards are a *subset* of it, so
editing a panel can never silently change what a fleet records.

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
[internal/collector/host/testdata/golden/host.prom](../internal/collector/host/testdata/golden/host.prom).

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
[internal/collector/container/testdata/golden/container.prom](../internal/collector/container/testdata/golden/container.prom).

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
[internal/collector/container/testdata/README.md](../internal/collector/container/testdata/README.md#coverage-gaps).

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

### GPUs — Phase 3 (NVIDIA and AMD)

About 12 series for one MIG-partitioned card, and about 9 for an AMD card.
NVIDIA is served by two interchangeable implementations behind one interface;
AMD is read from sysfs and DRM fdinfo. **Intel is out of scope** — see
[Coverage gaps](#coverage-gaps-1).

The two vendors do not fork the contract: the same families, units and labels
on either, with `vendor` on `prickle_gpu_info` saying which stack a card came
from. What differs is only what a platform genuinely does not have.

| Source | Families |
|---|---|
| `--query-gpu` | `prickle_gpu_utilization_ratio`, `memory_used_bytes`, `memory_total_bytes`, `temperature_celsius`, `power_watts` |
| `nvidia-smi -L` | `prickle_gpu_mig_enabled`, `mig_info` |
| NVML only | `prickle_gpu_mig_memory_used_bytes`, `mig_memory_total_bytes`, `mig_utilization_ratio` |
| `--query-compute-apps` | `prickle_gpu_process_memory_bytes{command,container}` — opt-in |
| AMD sysfs | the same five `--query-gpu` families, plus `prickle_gpu_amd_partition_info` |
| AMD DRM fdinfo | `prickle_gpu_process_memory_bytes{command,container}` — opt-in |
| identity | `prickle_gpu_info{vendor,name,index}`, `prickle_gpu_nvidia_source_info` |

**The MIG families are absent on an AMD card, not zero**, and
`prickle_gpu_amd_partition_info` is absent on an NVIDIA one. A
`prickle_gpu_mig_enabled 0` would claim the card is an unpartitioned NVIDIA
card, which is a specific and wrong statement rather than a missing one — the
same rule that keeps the pressure family off cgroup v1.

Two AMD-only caveats:

- **`name` is a lookup, not a reading.** AMD sysfs publishes no marketing name,
  and SPEC.md §Hard constraints #2 does not permit spawning `amd-smi` to ask.
  Known PCI IDs resolve to a name; anything else reports the PCI ID itself.
- **Per-process attribution needs to be able to read other users' `fdinfo`.**
  An unprivileged exporter sees its own processes and silently omits the rest,
  which on a Kubernetes node means the containers are the ones missing. This is
  the same trade as `-collector.container.pod-names` and is why per-process is
  opt-in.

Four things worth knowing:

- **`container` on a GPU process is empty for a process on the host**, not
  missing. The value comes from the process's own `/proc/<pid>/cgroup`, so it is
  a statement about that process rather than about the exporter, and the key is
  always present so a query written without `by` keeps working either way.
  Joining it to `prickle_container_info` gives the pod, the namespace and the
  image — which is how "who is using this card" gets answered on Kubernetes.

  On Kubernetes this needs **`CAP_SYS_PTRACE`** as well as the flag. Naming a
  process means reading its `exe` link, a `PTRACE_MODE_READ` operation, and
  Yama `ptrace_scope=1` — the default on Debian and Ubuntu — permits that only
  for a process's own descendants. Without it every GPU process resolves to an
  empty command and is dropped, so the family is absent with nothing logged.
  The chart adds the capability when `collectors.perProcess` is set.

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

# Which POD is holding a card. The GPU series carries the container; the pod
# name comes from prickle_container_info, which is where descriptive attributes
# live. Aggregate the right-hand side: during a DaemonSet rollout two exporters
# report the same container and an unaggregated join fails the whole query.
prickle_gpu_process_memory_bytes
  * on (container) group_left (pod_name)
    max by (container, pod_name) (prickle_container_info)

# Whether a scrape came from NVML or the nvidia-smi fallback.
prickle_gpu_nvidia_source_info
```

#### Coverage gaps

| Gap | Effect | What closes it |
|---|---|---|
| ~~**AMD — sysfs + DRM fdinfo**~~ | **Closed 2026-08-04.** Captured and implemented against 2× MI300X, and verified live on that host: the fixture-derived golden output and the real scrape agree series for series. | — |
| A **bare-metal** AMD host | The captured cards are SR-IOV virtual functions, so `current_compute_partition` is read-only in the guest and has only ever been observed at `SPX`. AMD's CPX/DPX partitioning is unexercised — the analogue of the MIG-on/MIG-off pair the NVIDIA fixtures have. | A capture from a bare-metal AMD host. |
| A host with **both vendors' cards** | Both paths run in one pass and are rendered into one Set, but no machine has ever had both to prove it. | A capture, or a host with an NVIDIA and an AMD card in it. |
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
  AMD: no amdgpu card on this host. The collector is implemented,
  so this is an absence rather than a gap.
  Intel is out of scope.

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


# Changelog

All notable changes to `prickle-exporter` are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning follows [SPEC.md §Versioning](SPEC.md#versioning): every package is
under `internal/`, so there is no Go API to version — SemVer applies to the
**metrics contract** and the **command line**. Pre-1.0, the minor tracks the
roadmap phase; `1.0.0` is where the metrics contract freezes.

This file is written by hand. A metric change needs prose telling an operator
what to do about it, which no commit-log generator writes.

## [Unreleased]

Phase 3: the GPU collector, **NVIDIA only**. Nothing in Phase 1 or 2 output
changes. AMD and Intel are in SPEC.md §Collectors' Phase 3 scope and are *not*
implemented — see Notes.

### Added

- **NVIDIA GPU collector** reporting per-card utilization, memory, temperature
  and power; MIG topology; and optional per-process memory. Two interchangeable
  implementations behind one `nvidiaSource` interface, selected once at startup:
  NVML via `dlopen` where available, `nvidia-smi` otherwise. About 12 series for
  one MIG-partitioned card.
- `prickle_gpu_nvidia_source_info`, recording which implementation is live, so a
  scrape says whether it came from NVML or the fallback.
- **`-collector.gpu.per-process`**, adding `prickle_gpu_process_memory_bytes`
  keyed on `command` — the basename of the executable path. Opt-in, because it
  is one series per distinct command per GPU.
- **`-collector.gpu.nvidia-source={auto,nvml,smi}`** to force one path for
  debugging, plus `-collector.gpu` and `-collector.gpu.nvidia-smi-command`.
- `prickle diagnose` gained a Phase 3 section: which of the two artifacts this
  binary is, which source is live, how many GPUs and MIG instances were found,
  and — when nothing loaded — why each candidate declined.
- `ci/check.sh` now compiles, vets and tests the `-tags nvml` build. That source
  is invisible to every other step, so an edit to shared GPU code could break
  the second shipped artifact silently.
- **A hardware test asserting the two sources agree**, which SPEC.md §Testing
  rules requires and nothing implemented. It reads the same card through both
  implementations and compares whole series identities — name, label keys *and*
  label values — so a divergence in a label shows up as a missing series rather
  than as a value that looks close enough. It skips wherever NVML does not
  load, so `go test -tags nvml` stays green off hardware.
- A second fixture capture, `h100-default-20260729`: an H100 in **Default
  mode** under a real CUDA kernel, with its own golden file. The first capture
  was MIG-partitioned for its whole life, so "what does an unpartitioned card
  report" was answered by a hand-written `nvidia-smi -L` line, and "does a real
  utilization reading survive the parser" could not be answered at all — an
  idle card reads `0`, which is indistinguishable from a parser wrongly turning
  an absent `[N/A]` into a zero. This card was pinned at 100%.

### Fixed

Three defects in the NVML path, all found by its **first execution on hardware**
(H100 80GB, driver 580.173.02, Default and MIG mode, 2026-07-29). None was
reachable from a fixture; each now has a test that fails if it returns.

- **`prickle_gpu_memory_used_bytes` was 480 MiB too high from `prickle-nvml`.**
  It bound `nvmlDeviceGetMemoryInfo`, whose `used` is `total - free` and so
  includes memory the driver reserves; `nvidia-smi` reports the `_v2` number,
  which excludes it. The two artifacts therefore disagreed about the same card
  by half a gigabyte — enough to move every memory panel and capacity alert
  depending on which binary was deployed. Now binds
  `nvmlDeviceGetMemoryInfo_v2`, falling back to the original only on drivers too
  old to publish it, which are the drivers whose `nvidia-smi` reports the old
  accounting anyway.
- **The `profile` label on `prickle_gpu_mig_info` differed between the two
  sources.** NVML has no entry point returning the profile name, and the label
  was derived from the instance's memory alone: `10gb` where `nvidia-smi -L`
  spells it `1g.10gb`. The leading slice count now comes from
  `nvmlDeviceGetAttributes_v2`. **Operator impact:** a dashboard or recording
  rule filtering on `profile` matched only one of the two artifacts.
- **`prickle diagnose` reported `NVML source is closed` on hosts where NVML
  worked.** The library handle is process-global; diagnose builds a GPU
  collector to describe the live source, closes it, and then builds the real
  one, which was handed the same already-closed source. The load is now
  reference-counted and re-established after a full release. A failed load is
  still never retried, which is what SPEC.md §Collectors' "attempt once at
  startup" protects.

Also fixed, in the same pass:

- `prickle diagnose` no longer says "On a host with no NVIDIA GPU this is
  expected" after an operator *forced* an unavailable source with
  `-collector.gpu.nvidia-source`. On a host that plainly has a GPU, that line
  answered a question nobody asked.
- `scripts/capture-fixtures.sh` prefers an `nvcc`-built kernel over its
  `gcc` + embedded-PTX spinner, whose JIT fails on CUDA 13 drivers and degrades
  *silently* to a context-only load. The old chain produced a capture that
  looked complete with `utilization.gpu` reading `0` — the one value a GPU
  fixture must never be ambiguous about. `check` and `capture` now report a
  numeric `0` alongside a resident compute process as a gap; a literal `[N/A]`
  is not flagged, because under MIG it is the correct answer.

### Notes

- **AMD and Intel are not implemented.** They are Phase 3 scope, but the
  captured host is NVIDIA-only: there is no `gpu_busy_percent`, no
  `mem_info_vram_*`, no `hwmon` tree and no `drm-*` fdinfo to develop against,
  and SPEC.md §Testing rules forbids inventing a sysfs layout. An AMD or Intel
  host reports no GPU metrics at all. Closing this needs a capture from such a
  host with a workload running.
- **The NVML path has now run on hardware** — an H100 80GB, driver 580.173.02,
  in Default and MIG mode — and its output was diffed against the same card's
  `nvidia-smi` source. That is what SPEC.md §Testing rules means by the two
  sources having to agree, and it took three fixes to be true (see Fixed). It
  remains un-fixture-testable: a C call is not a file read, so the assertion
  re-runs only where a GPU is present. What is still unproven for both sources
  is a **multi-GPU host** — every capture so far is a single card.
- **An absent metric means the driver would not say.** `utilization_ratio`
  vanishes for the whole card once MIG is enabled — the driver reports `[N/A]`,
  verified on H200 / driver 580 and present in the fixture. Reporting zero there
  would read as an idle GPU and fire idle-capacity alerts across a MIG fleet.
- **MIG instances are their own families.** `prickle_gpu_mig_memory_used_bytes`
  rather than a `mig_uuid` label on the card's family, because an instance's
  memory is a partition of its parent's and one family holding both would
  double-count under `sum()`.
- **The `nvidia-smi` source cannot attribute a process to a MIG instance.**
  `--query-compute-apps` returns the parent GPU's UUID for a MIG-resident
  process, so those series carry `gpu_uuid` and no `mig_uuid`. Coarse, not
  wrong. NVML can do better; nothing else can.
- **No PID reaches the output.** The compute-apps CSV carries one, and it is
  discarded at the parse boundary — the snapshot type has no field for it, so
  the guarantee is structural rather than a promise. Process series key on
  `command`, never on the truncated and forgeable `comm`.
- Per-MIG memory and utilization are absent from the `nvidia-smi` source: no CSV
  query publishes them, and the human-readable table is not parsed. GPU-instance
  and compute-instance IDs are exposed by neither source, because nothing
  captured joins a MIG UUID to those IDs and a label only NVML could fill would
  break the identical-output requirement.

## [0.2.0] — 2026-07-28

Phase 2: the container collector. Nothing in Phase 1's output changes — no
metric was renamed, no label added to an existing series — so a Prometheus
already scraping 0.1.x keeps every rule and dashboard it has. What arrives is a
new `prickle_container_*` namespace and three new flags.

### Added

- **Container collector**, walking the cgroup v2 tree and reporting CPU (with
  quota and throttling), memory (usage, the four limit files, nine `memory.stat`
  fields, page faults), block I/O per device, process counts, and per-cgroup
  PSI. About 25 series per container. Identity comes from the directory names
  the runtimes write — `docker-<hex>.scope`, `cri-containerd-<hex>.scope`,
  `crio-<hex>.scope`, and the pod slices under `kubepods.slice`.
- `prickle_container_info`, the companion gauge carrying the runtime, the
  Kubernetes QoS class, and — with Docker enrichment on — the container's name
  and image. Join it with `group_left`; none of those attributes appears on a
  hot series.
- **`-collector.container.docker-socket`**, the optional enrichment path from
  SPEC.md §Collectors: one GET request per pass for names and images. Off by
  default — the exporter opens no socket nobody asked it to open — and bounded
  by `-collector.container.docker-timeout` (2s), because a wedged daemon must
  cost the names and not the metrics.
- **`-collector.container`** (default `true`) to switch the whole walk off.
- `prickle diagnose` gained a Phase 2 section: whether the cgroup root can be
  walked, how many containers were found and under which runtimes, and whether
  Docker enrichment is on and working. When it finds none, it names the three
  causes worth checking rather than leaving you with an empty scrape.

### Notes

- **`pod` carries the pod's UID, not its name, and `namespace` is not emitted.**
  A cgroup directory name holds a UID and nothing else — the kernel stores no
  pod name or namespace anywhere in the tree — so those are the honest values.
  The UID is unescaped from systemd's spelling
  (`6eb5044d_ef2e_49d1_a9cc_28f4e3fe88a3`) back to the one `kubectl` reports.
  Resolving a name needs a Kubernetes-aware source, which a cgroup walk is not;
  if that lands, `pod` changing meaning would be a major bump, and the label
  would gain a companion rather than change under you.
- **Only leaf containers are sampled.** The pod, QoS and root slices above them
  are walked for identity and never emitted: a `sum` over the family is the
  node's containers once, not once per level of the hierarchy. Pod totals are
  `sum by (pod)`.
- **An unset limit is an absent series.** `memory.max` reading `max` produces no
  `prickle_container_memory_limit_bytes` sample rather than the kernel's
  9.2-exabyte sentinel, so a usage/limit ratio silently drops unconstrained
  containers instead of reporting them as 0% full.
- `cgroup.procs` is never read. It is the one file in a container's cgroup that
  contains PIDs, SPEC.md §Metrics contract forbids those everywhere, and a test
  fails if any source file starts to read it.
- Known coverage gaps, recorded in full in
  [internal/collector/container/testdata/README.md](internal/collector/container/testdata/README.md#coverage-gaps):
  CRI-O and Guaranteed-pod directory names are unit-tested on the name parse
  only, and the **cgroupfs-driver layouts** (`/sys/fs/cgroup/docker/<hex>/` and
  `kubepods/besteffort/pod<uid>/<hex>`) are **not implemented** — a host using
  them reports no containers. Closing those needs a capture from a host
  configured that way; no format was guessed at in the meantime.

## [0.1.1] — 2026-07-27

### Fixed

- Release binaries after the first architecture were built from a working tree
  that the build's own staging directory had made dirty, so Go stamped them
  `vcs.modified=true` and `vX.Y.Z+dirty`. In 0.1.0 the amd64 and arm64
  artifacts therefore disagreed about whether they came from the tag. **No
  change to the exporter, its behavior, or its output** — the 0.1.0 binaries
  run identically and their checksums and attestations are valid. Only the
  recorded provenance was wrong. If you took the 0.1.0 arm64 artifact and care
  about provenance, re-download.

  The release workflow now asserts the property directly, per architecture,
  and fails on a missing stamp as well as a dirty one.

## [0.1.0] — 2026-07-27

Phase 1: the host collector, and the machinery underneath it.

### Added

- **Host collector** reading `/proc/stat`, `meminfo`, `diskstats`, `net/dev`,
  `loadavg`, `pressure/{cpu,memory,io}`, and `mounts` + `statfs`. Roughly 280
  series on a plain host. Every parser was developed against a captured H200
  fixture tree, and the rendered output is checked against a golden file.
- **Hand-written Prometheus text exposition** (`internal/exposition`), with no
  `prometheus/client_golang`. Output is gated on `promtool check metrics`.
- **Sampler** that polls collectors on an interval and swaps a fully rendered
  buffer under a mutex, so `/metrics` serves the last completed render and a
  slow collector can never stall a scrape.
- **Self-instrumentation**: `prickle_build_info`,
  `prickle_render_timestamp_seconds`, `prickle_collector_duration_seconds`,
  `prickle_collector_errors_total`, `prickle_collector_success`.
- **`prickle diagnose`**, reporting what the exporter can actually read on a
  host — by reading it, since procfs files report size 0 and permission
  failures surface on open, not on stat. Detects cgroup v1 and says so.
- **Configurable filesystem roots** (`internal/fsroot`) behind `-path.rootfs`
  and friends, so tests and containers point at a tree rather than at `/`.
- Aggregate CPU time is always exposed; per-core series are opt-in behind
  `-collector.cpu.per-core` as a separate family, so default cardinality does
  not scale with core count.
- `ci/check.sh`, the entire pre-commit checklist in one command, run unchanged
  by CI.
- `ci/check-port-registration.sh`, a watchdog on the upstream Prometheus
  default-port registration for `:10047`.

### Notes

- Linux only, and **cgroup v2 only**. v1 and hybrid hosts are out of scope.
- Containers (Phase 2) and GPUs (Phase 3) are specified but not implemented.
  `prickle diagnose` says so rather than reporting an empty GPU section as
  though it were a healthy one.
- `prickle-nvml` (SPEC.md §Distribution) is not released yet: no `//go:build
  nvml` source exists until Phase 3, so the artifact would be an identical copy
  of `prickle` under a name promising NVML support.
- `/proc/loadavg`'s fourth and fifth fields are not exposed. The fifth is a
  PID, and SPEC.md §Metrics contract forbids PIDs everywhere.

[Unreleased]: https://github.com/starkdrift/prickle-exporter/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/starkdrift/prickle-exporter/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/starkdrift/prickle-exporter/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/starkdrift/prickle-exporter/releases/tag/v0.1.0

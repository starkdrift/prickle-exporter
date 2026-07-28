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

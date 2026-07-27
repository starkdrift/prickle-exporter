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

[Unreleased]: https://github.com/starkdrift/prickle-exporter/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/starkdrift/prickle-exporter/releases/tag/v0.1.0

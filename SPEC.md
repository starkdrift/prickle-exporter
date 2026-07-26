# prickle-exporter — SPEC

**Read this file in full before generating or modifying any code.** It is the
contract that keeps generation sessions consistent. Decisions recorded here are
frozen; do not reopen them mid-session. If a session genuinely needs to change
one, the change happens in this file first, in its own commit.

## Identity

| Item | Value |
|---|---|
| Organisation | Starkdrift |
| Repository / Go module | `github.com/starkdrift/prickle-exporter` |
| Binary and CLI command | `prickle` |
| Diagnostic subcommand | `prickle diagnose` |
| Metric prefix | `prickle_` — always the full word |
| Listen address | `:10047` — next free slot on the Prometheus default-port wiki; register it there as soon as the repo is public |
| License | Apache-2.0 — `LICENSE` file at repo root, SPDX header `Apache-2.0` in every source file |

The names above are the only ones used anywhere in the tree — code, comments,
docs, fixtures, chart names, dashboard JSON. Earlier candidate names were
discarded during naming and must not reappear; the deny-list lives in
`ci/denied-names.txt` and is enforced by a CI grep that excludes itself and
this file. Never abbreviate the metric prefix.

## Hard constraints

1. **Go standard library only.** Zero third-party dependencies, including
   `prometheus/client_golang`. Exposition is hand-written (`internal/exposition`).
2. **Strictly read-only.** The exporter never writes to `/proc`, `/sys`, or
   cgroups and takes no corrective actions — those belong to the separate
   closed agent. Only permitted external interactions: read-only filesystem
   access, GET-only requests on the Docker socket (optional), `dlopen` of
   `libnvidia-ml.so.1` with query-only NVML calls, and spawning `nvidia-smi`
   as a fallback subprocess. No NVML call that mutates device state (MIG
   configuration, clocks, persistence mode, ECC) may ever be used.
3. **Configurable filesystem roots.** All access to `/proc`, `/sys`, and
   `/sys/fs/cgroup` goes through `internal/fsroot` prefixes so tests can point
   at fixture trees. No collector may hardcode an absolute path.
4. **cgroup v2 only.** v1/hybrid hosts are out of scope; `prickle diagnose`
   detects v1 and says so plainly.

## Metrics contract

- Identity labels on hot series, and only these: `node`, `namespace`, `pod`,
  `container`, `gpu_uuid`, `mig_uuid`.
- Descriptive attributes (names, models, versions, images) live on companion
  `_info` gauges, joined in queries via `group_left`. Never put them on hot series.
- Per-process GPU attribution is **opt-in** via a `command` label sourced from
  the `exe` symlink basename — never `comm` (truncated, forgeable).
- **PID never appears** as a label or metric value anywhere.
- Output is the Prometheus text format with correct `# HELP` / `# TYPE` lines
  and must pass `promtool check metrics` at all times.
- Cardinality caps per collector (Phase 4): on breach, drop and count via
  self-instrumentation; never OOM the scrape.

## Architecture

One binary. `internal/collector` defines the `Collector` interface; a sampler
goroutine polls collectors on an interval, renders into a buffer, and swaps it
under a mutex; `net/http` serves the last completed render so slow collectors
can never stall a scrape. Per-collector timeouts and self-metrics
(`prickle_collector_duration_seconds`, `prickle_collector_errors_total`) are
mandatory from Phase 4.

## Collectors

- **Host (Phase 1):** `/proc/stat`, `meminfo`, `diskstats`, `net/dev`,
  `loadavg`, `pressure/{cpu,memory,io}`, `mounts` + `Statfs` (behind an
  interface — not fixture-able as a file).
- **Containers (Phase 2):** walk the cgroup v2 tree; identity extracted from
  directory names: `docker-<hex>.scope`, `cri-containerd-<hex>.scope`,
  `crio-<hex>.scope`, `kubepods.slice/.../pod<uid>`. Docker socket is an
  optional enrichment path for human-readable names only.
- **GPU (Phase 3):** AMD via sysfs + DRM fdinfo; Intel via DRM fdinfo.
  NVIDIA is served by two interchangeable implementations behind one
  `nvidiaSource` interface, selected at runtime in this order:
  1. **NVML via `dlopen` of `libnvidia-ml.so.1` — the preferred path.** Richer
     data, no per-scrape process spawn, no CSV parsing, and the only reliable
     source of MIG topology. Requires cgo (see build note below).
  2. **`nvidia-smi` CSV subprocess — fallback only.** Used when the NVML build
     is unavailable or the library cannot be loaded at runtime (common in slim
     containers that lack the driver libraries). Kept working and tested; it is
     not deprecated. Verified limitations (H200, driver 580):
     `--query-compute-apps` reports the parent GPU UUID for MIG-resident
     processes, so per-process MIG attribution is unavailable in this source
     (processes are attributed to the physical GPU); and `utilization.gpu`
     returns `[N/A]` when MIG is enabled — the CSV parser must treat bracketed
     `[N/A]` tokens as absent values, never as errors.

  Selection is automatic: attempt the NVML load once at startup, fall back
  silently, and record the active source on an `_info` gauge.
  `prickle diagnose` states which source is live and, when NVML failed to load,
  why. A `--nvidia-source={auto,nvml,smi}` flag forces one path for debugging.
  Consumer and MIG-partitioned datacenter cards are both supported; MIG
  instances carry `mig_uuid`.

  **Build note.** `dlopen` requires cgo *and* a dynamically linked binary — a
  fully static binary cannot `dlopen` at all. NVML therefore lives behind
  `//go:build nvml` and produces a second, dynamically linked artifact. The
  default `CGO_ENABLED=0` build remains pure-Go and static, and uses the
  `nvidia-smi` path. Ship both (see Distribution); neither build links against
  NVIDIA libraries at compile time, so the zero-dependency rule holds in both.

## Testing rules

Every parser is developed against captured fixture trees under `testdata/`,
laid out mirroring real paths so `fsroot` points directly at them. Fixtures
come from `capture-fixtures.sh`; synthetic (hand-built) fixtures are permitted
only where hardware access is pending and must be marked synthetic in a README
beside them. **Never invent file formats or path shapes** — if no fixture
exists for a case, stop and request a capture. Exposition output is verified
against golden files.

NVML is the one path that cannot be fixture-tested: it is a C library call, not
a file read. Both NVIDIA implementations sit behind the `nvidiaSource`
interface, so unit tests exercise a fake source, and the NVML implementation
itself is verified only on real hardware. Its captured `nvidia-smi` fixtures
remain the reference for what the NVML path must report — the two sources must
produce identical metric output for the same GPU, and a hardware test asserts
that.

## Distribution (Phase 5)

Two release artifacts per platform: `prickle` (static, `CGO_ENABLED=0`,
nvidia-smi path) and `prickle-nvml` (dynamically linked, `-tags nvml`,
preferred on NVIDIA hosts). Both ship with a systemd unit using a strict
sandbox (`ProtectSystem=strict`, read-only binds, no capabilities beyond what
reading requires) — the NVML unit additionally needs read access to the driver
library path and `/dev/nvidiactl`. Docker one-liner; Helm chart
`prickle-exporter` with ServiceMonitor; `docker compose`
quickstart bundling prickle, Prometheus, and pre-provisioned Grafana. Four
dashboards ship with it — GPU Tenancy, Node Overview, Container Resources,
Fleet Health — using paired textbox + chained-dropdown template variables per
identity label, wrapping input as `.*<input>.*` for contains-search, filtering
on `command` (never PID). Dashboards carry Starkdrift branding.

## Session checklist

Before writing code: read this file, read the target package's existing code,
and confirm the fixture tree for the collector exists. Before committing: `go
vet`, tests green, `promtool check metrics` on sample output, and the forbidden-
string grep.

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
4. **cgroup v2 is the primary hierarchy; v1 and hybrid are supported.**
   Reversed on 2026-08-01, after `prickle diagnose` was run on a real v1 host
   (Rocky Linux 8.10, kernel 4.18) and correctly reported that it would produce
   nothing. Correctly, and uselessly: RHEL 8 defaults to v1 and is supported
   into 2029, so "out of scope" meant an ordinary enterprise host got an empty
   scrape. The detection was working as designed; the design was the problem.

   v1 is a different data model, not a spelling variant — one hierarchy per
   controller (`/sys/fs/cgroup/memory/…`, `/sys/fs/cgroup/cpu,cpuacct/…`),
   different file names, and different units. It therefore gets its own reader
   behind the same interface, never a set of `if v1` branches in the v2 path.

   **The metrics contract does not fork.** A container reports the same metric
   names with the same units and the same label set on either hierarchy; the
   hierarchy decides where a value is read from, never what it is called.
   Where v1 genuinely cannot answer — it has no PSI, so there is no
   `prickle_container_pressure_stalled_seconds_total` — the family is **absent**
   rather than zero, for the same reason `utilization_ratio` vanishes under MIG.
   `prickle diagnose` states which hierarchy is live and what it costs.

## Metrics contract

- Identity labels on hot series, and only these: `node`, `namespace`, `pod`,
  `container`, `gpu_uuid`, `mig_uuid`.
- That closed set governs **identity** — which entity a sample belongs to. It
  does not forbid *dimensional* labels that partition a single metric across
  the parts of one entity: `mode` on CPU time, `cpu` on per-core series,
  `device` on disks, `interface` on links, `mountpoint` on filesystems,
  `resource`/`kind` on PSI. A dimensional label is one whose values are
  enumerable from the metric's own source and which is meaningless without the
  metric; it never names an entity that another metric could also be about. New
  dimensional labels are added deliberately, not by reflex — each one multiplies
  series count.
- Descriptive attributes (names, models, versions, images) live on companion
  `_info` gauges, joined in queries via `group_left`. Never put them on hot series.
  This applies to dimensional labels too: where a hot series has a natural key
  (`mountpoint`), the human-readable extras (`device`, `fstype`) belong on the
  `_info` gauge.
- Metric names are `snake_case` throughout — `promtool check metrics` lints
  camelCase as an error, so kernel field names (`MemTotal`, `Active(anon)`) are
  converted, not passed through.
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
  interface — not fixture-able as a file). Aggregate CPU time is always
  exposed; per-core series are opt-in behind `--collector.cpu.per-core`, as a
  separate family, so default cardinality does not scale with core count on
  large GPU nodes. `/proc/loadavg`'s fourth and fifth fields are not exposed:
  the fifth is a PID.
- **Containers (Phase 2):** walk the cgroup v2 tree; identity extracted from
  directory names. The kubelet's and Docker's **cgroup driver** decides the
  shape of the whole tree, and both drivers are in scope:
  - *systemd driver* — `docker-<hex>.scope`, `cri-containerd-<hex>.scope`,
    `crio-<hex>.scope`, under `kubepods.slice/.../kubepods-…pod<uid>.slice`
    with the UID systemd-escaped. Guaranteed pods carry no QoS component.
  - *cgroupfs driver* — `docker/<hex>` and `kubepods/<qos>/pod<uid>/<hex>`,
    with no suffix, no runtime prefix, and the UID unescaped. This is the
    default on managed Kubernetes, so treating it as exotic means an ordinary
    node reports nothing.

  A bare hex directory is a container only because of its parent; the parent is
  what distinguishes one from any other hex-named cgroup. Where the layout does
  not name the runtime — which is every cgroupfs kubepods tree — `runtime` on
  the `_info` gauge is **empty** rather than inferred. Docker socket is an
  optional enrichment path for human-readable names only.
- **GPU (Phase 3):** AMD via sysfs + DRM fdinfo. **Intel is out of scope.** No
  capture host is obtainable, and §Testing rules forbids developing a parser
  against a layout nobody has captured — so listing it would be scope on paper
  and an empty scrape in practice, which is the worse of the two failures. This
  is a decision about what ships, not a door welded shut: Intel is read through
  the same DRM fdinfo path AMD needs anyway, so a capture is all that stands
  between this line and reopening it.
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

## Versioning

Every package is under `internal/`, so there is no importable Go API and no Go
API compatibility obligation. SemVer here governs the only two public surfaces
the project has: the **metrics contract** (names, label sets, types, units) and
the **command line** (flag names and their meanings).

| Bump | Trigger |
|---|---|
| major | A metric is renamed or removed; a label key is added to, removed from, or changed in meaning on an existing series; a flag is removed; a default changes such that output changes |
| minor | A new metric family, collector, flag, or label *value* |
| patch | A wrong value corrected, a parser fix, performance, documentation |

Adding a label *key* to an existing hot series is a major, not a minor: it
breaks every recording rule and dashboard that aggregates without `by`.

Releases are tagged `vX.Y.Z`. **Git tags are the sole source of truth for the
version** — there is no VERSION file and no version constant to forget to bump.
The build injects it with `-ldflags "-X main.version=..."`, and it is exposed to
operators on `prickle_build_info`.

Pre-1.0, the minor tracks the roadmap phase, so the version states what is
implemented: `0.1.0` host, `0.2.0` containers, `0.3.0` GPU, `0.4.0` caps and
timeouts. Until 1.0.0 a minor may break the contract — Phases 2 and 3 will
teach the label set things Phase 1 cannot.

**`1.0.0` means the metrics contract is frozen.** It is Phase 5, and a
deliberate promise rather than something to drift into.

From 1.0.0 on, a metric that must change is emitted under both the old and new
names for one full minor, with the old name marked deprecated in its `# HELP`
text, and removed at the next major.

`CHANGELOG.md` is written by hand, not generated from commits: a metric change
needs prose that says what to do about it, which no generator writes.

## Session checklist

Before writing code: read this file, read the target package's existing code,
and confirm the fixture tree for the collector exists. Before committing: `go
vet`, tests green, `promtool check metrics` on sample output, and the forbidden-
string grep.

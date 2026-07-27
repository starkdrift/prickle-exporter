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
  <img src="https://img.shields.io/badge/status-phase%201-F0A202" alt="Phase 1">
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

**Phase 1.** The host collector is implemented and tested against a captured
fixture tree. Containers and GPUs are specified but not yet written.

| Phase | Scope | State |
|---|---|---|
| 1 | Host — CPU, memory, disks, network, load, PSI, filesystems | **shipped** |
| 2 | Containers — cgroup v2 walk, Docker/containerd/CRI-O/Kubernetes identity | planned |
| 3 | GPU — NVIDIA (NVML + `nvidia-smi`), AMD sysfs + DRM fdinfo, Intel DRM fdinfo | planned |
| 4 | Per-collector timeouts, cardinality caps, self-instrumentation | partial — self-metrics exist, caps do not |
| 5 | Distribution — systemd units, Helm chart, Docker, four Grafana dashboards | planned |

Linux only, and **cgroup v2 only** — v1 and hybrid hosts are out of scope, and
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

GPU
  NVIDIA source selection is Phase 3 and not implemented yet.

host collector: 278 series in 672µs
```

It takes the same flags as the exporter, so it diagnoses exactly the
configuration you would run with — including `-path.rootfs`.

## Metrics

Every metric is prefixed `prickle_`, never abbreviated. Phase 1 emits ~280
series on a plain host, from these families:

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
| `-log.level` | `info` | `debug`, `info`, `warn`, `error`. |
| `-version` | | Print version and exit. |

Regexps are compiled at startup, so a typo fails immediately rather than at the
first scrape.

## NVIDIA: two builds (Phase 3)

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

## Development

One command is the whole pre-commit checklist, and CI runs the same script:

```sh
./ci/check.sh
```

It runs `gofmt -l`, `go vet`, `go test ./...`, then gates on: an empty `go.sum`
and no `require` block, an SPDX header in every `.go` file, `promtool check
metrics` on every golden file, and greps for denied names and abbreviated metric
prefixes.

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

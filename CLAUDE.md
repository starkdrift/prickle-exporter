# CLAUDE.md

## Read SPEC.md first — every session, in full

[SPEC.md](SPEC.md) is the frozen contract for this project. **Read it before
generating or modifying any code.** Decisions in it are not to be reopened
mid-session; if one genuinely must change, edit SPEC.md first in its own commit,
then write code to match. Nothing below replaces SPEC.md — this file is a fast
index plus the traps that are easy to trip on.

## What this is

`prickle-exporter` is a Prometheus exporter for host, container, and GPU metrics.
One Go binary, `prickle`. Read-only. Standard-library only. See
[SPEC.md](SPEC.md) for the full identity table, architecture, and roadmap.

- Module: `github.com/starkdrift/prickle-exporter`
- Binary / CLI: `prickle` (diagnostics: `prickle diagnose`)
- Listen address: `:10047`
- License: Apache-2.0 — `SPDX-License-Identifier: Apache-2.0` header in **every**
  source file.

## Non-negotiable constraints (full text in SPEC.md §Hard constraints)

1. **Standard library only.** Zero third-party deps — including
   `prometheus/client_golang`. Exposition is hand-written in
   `internal/exposition` and must pass `promtool check metrics`.
2. **Strictly read-only.** Never write to `/proc`, `/sys`, or cgroups; never call
   any NVML function that mutates device state. Permitted external interactions
   are exactly the list in SPEC.md §Hard constraints #2 — nothing else.
3. **All fs access goes through `internal/fsroot` prefixes.** No collector may
   hardcode an absolute `/proc`, `/sys`, or `/sys/fs/cgroup` path — tests point
   these at fixture trees.
4. **cgroup v2 and v1.** v2 is the primary hierarchy; v1 and hybrid hosts are
   supported through a separate reader (`internal/collector/container/v1.go`),
   never `if v1` branches in the v2 path. The metrics contract does not fork —
   same names, units and labels on either. v1 has no PSI, so the pressure
   family is absent there rather than zero.

## Metrics contract (full text in SPEC.md §Metrics contract)

- Hot-series identity labels — **only** these: `node`, `namespace`, `pod`,
  `container`, `gpu_uuid`, `mig_uuid`.
- Descriptive attributes (names, models, versions, images) go on companion
  `_info` gauges, never on hot series.
- **PID never appears** anywhere — not as a label, not as a value. Per-process
  GPU attribution is opt-in via a `command` label from the `exe` symlink
  basename, never `comm`.

## Architecture in one breath

One binary. `internal/collector` defines the `Collector` interface. A sampler
goroutine polls collectors on an interval, renders into a buffer, and swaps it
under a mutex; `net/http` serves the last completed render so a slow collector
can never stall a scrape. Per-collector timeouts + self-metrics
(`prickle_collector_duration_seconds`, `prickle_collector_errors_total`) are
mandatory from Phase 4.

## Fixtures — do not invent file formats

Every parser is developed against captured fixture trees under `testdata/`,
mirroring real paths so `fsroot` points straight at them. **If no fixture exists
for a case, stop and request a capture** ([scripts/capture-fixtures.sh](scripts/capture-fixtures.sh),
usage in [scripts/README.md](scripts/README.md)) rather than guessing a format
or path shape. Synthetic fixtures are allowed only where hardware access is
pending and must be marked synthetic in a README beside them.
Exposition output is checked against golden files. NVML is the one path that
can't be fixture-tested (C call, not a file read) — both NVIDIA sources sit
behind the `nvidiaSource` interface and unit tests use a fake source.

## NVIDIA: two builds (full text in SPEC.md §Collectors, §Distribution)

- Default build: `CGO_ENABLED=0`, pure-Go, static → `nvidia-smi` CSV subprocess.
- `//go:build nvml` build: cgo + dynamically linked → `dlopen` of
  `libnvidia-ml.so.1`, the preferred path. A static binary cannot `dlopen`.
- Neither build links NVIDIA libs at compile time, so the zero-dep rule holds
  for both. `nvidia-smi` is a supported fallback, not deprecated — keep it
  tested. The two sources must emit identical metric output for the same GPU.

## Naming discipline

The names in SPEC.md §Identity are the only ones used anywhere — code, comments,
docs, fixtures, charts, dashboard JSON. Never abbreviate the metric prefix
(`prickle_`, always the full word). Discarded candidate names live in
`ci/denied-names.txt` and are grep-enforced in CI.

## Before you commit (SPEC.md §Session checklist)

Run [ci/check.sh](ci/check.sh) — it is the whole checklist in one command, and
CI runs the same script:

- `gofmt -l`, `go vet`, `go test ./...`
- zero third-party deps (empty `go.sum`, no `require` block)
- an SPDX header in every `.go` file
- `promtool check metrics` on every `testdata/golden/*.prom`
- the denied-names and metric-prefix greps

`ci/denied-names.txt` is currently **empty**, so that gate reports VACUOUS and
protects nothing until the discarded candidate names are filled in.

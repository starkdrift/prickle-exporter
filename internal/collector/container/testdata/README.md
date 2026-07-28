# Phase 2 container fixtures

`h200-ubuntu2204-20260726/` is a curated subset of the same capture the Phase 1
host fixtures come from, made by
[scripts/capture-fixtures.sh](../../../../scripts/capture-fixtures.sh) and kept
in the mirrored path shape so `fsroot.At("testdata/h200-ubuntu2204-20260726")`
resolves straight into it (SPEC.md §Testing rules).

| | |
|---|---|
| Host | single-tenant H200 rental, since destroyed. Its hostname is deliberately not recorded here; the directory name is what identifies the capture. |
| Captured | 2026-07-26T05:42:16Z |
| OS | Ubuntu 22.04.5 LTS, kernel 5.15.0-185-generic, x86_64 |
| cgroup | `cgroup2fs` — pure v2 |
| Runtimes | Docker 29.1.3, API 1.52 (three containers) and k3s / containerd (thirteen containers in six pods) |

Everything under `h200-ubuntu2204-20260726/` is **captured, unmodified** kernel
and daemon output. Only the files the Phase 2 parsers read were copied over from
the full capture — see [Not copied](#not-copied) for what was left behind and
why.

## What this tree exercises

- **Two runtimes, two directory shapes.** `docker-<hex>.scope` under
  `system.slice`, and `cri-containerd-<hex>.scope` inside the systemd
  `kubepods.slice` hierarchy. Both are the shapes SPEC.md §Collectors names.
- **Both non-Guaranteed QoS classes** — four `kubepods-besteffort-pod*.slice`
  pods and two `kubepods-burstable-pod*.slice` ones — with the systemd escaping
  the parser has to undo: the pod UID appears as
  `07fc7cef_656b_48a7_929d_2734c2b4498e` in the directory name and must be
  reported as `07fc7cef-656b-48a7-929d-2734c2b4498e`, which is what `kubectl`
  shows.
- **Multi-container pods.** Five of the six pods hold two containers and one
  holds three, so a `sum by (pod)` in a dashboard has something to sum.
- **Set and unset limits.** Exactly one container carries a real
  `memory.max` (178257920). Every other `memory.max`, every `memory.high` and
  every `cpu.max` quota reads `max`, and `memory.min` / `memory.low` read `0`.
  That mix is what pins the "unlimited is an absent series, not a sentinel"
  behaviour in `TestUnlimitedIsAbsentNotSentinel`.
- **An empty `io.stat`.** Containers that have touched no block device have a
  zero-length file, which must produce no samples rather than a parse error.
- **`memory.stat` with 40 keys**, of which the collector selects nine. The
  fixture is the record of which keys the captured kernel actually offers.
- **`proc/diskstats`** is here for one reason: `io.stat` names devices by
  `major:minor` and the host collector names them `vda`. This file is what the
  container collector resolves `252:0` through, so a container's I/O joins to
  its node's disk series.
- **`docker-api/containers.json`** is the captured `GET /containers/json`
  response, served over a unix socket in `docker_test.go` to exercise the
  optional enrichment path. Its container IDs are the IDs of the three Docker
  scopes in the cgroup tree, so the join under test is a real one.

## Coverage gaps

These are shapes SPEC.md names or real hosts have that this capture does
**not** contain. None of them is invented in code that pretends otherwise; each
is either unit-tested on the name parse alone, or not implemented at all.

| Gap | Status |
|---|---|
| `crio-<hex>.scope` (CRI-O) | Directory-name parse only, in `TestIdentify`. No CRI-O host was captured. |
| Guaranteed pods — `kubepods-pod<uid>.slice`, with no QoS component | Directory-name parse only, in `TestIdentify`. The rental ran no Guaranteed pod. |
| `cpu.pressure`, `io.pressure` | Read by the collector, covered by a hand-written tree in `TestPerCgroupPressure`. The capture script collects only `memory.pressure`; the format is identical to it and to the `/proc/pressure/*` files the Phase 1 fixtures do capture. |
| A container with a CPU quota (`cpu.max` = `<quota> <period>`) | Parsed, unit-tested, never seen. Every cgroup on the captured host reads `max 100000`, so `nr_periods` and `nr_throttled` are zero throughout the golden file. |
| cgroupfs-driver Docker — `/sys/fs/cgroup/docker/<hex>/` | **Not implemented.** `capture-fixtures.sh` looks for it, but the captured host used the systemd driver, so nothing confirms the shape. Docker on a cgroupfs-driver host reports no containers until a capture exists. |
| Non-systemd kubelet — `kubepods/besteffort/pod<uid>/<hex>` | **Not implemented**, same reason. |

Closing the last two needs a capture from a host configured that way; the first
four need a CRI-O host, a Guaranteed pod, and a container with a CPU limit,
which `capture-fixtures.sh prep` could arrange on a disposable rental.

## Not copied

The full capture holds more per-cgroup files than are here. Two kinds were left
out:

- **`cgroup.procs`** — the only file in a cgroup that contains PIDs. SPEC.md
  §Metrics contract forbids a PID anywhere, this collector never reads the file,
  and `TestNoPIDAnywhere` checks that no source file has started to. There is no
  reason to carry a list of a dead host's PIDs in this repository.
- **The pod, QoS and root slices' own files.** The collector reports leaf
  containers only — emitting the levels above them would triple-count every
  byte — so those files would sit here unread. The slice *directories* are
  present because the pod UID and QoS class are read out of their names.

## Golden output

`golden/container.prom` is the exposition document this tree renders to, with
`node="fixture"`. It is checked byte-for-byte and verified with `promtool check
metrics`. Regenerate after an intentional change with:

```sh
go test ./internal/collector/container/ -update-golden
```

Read the diff before committing it — the golden is the review surface for every
metric name, label and unit in Phase 2.

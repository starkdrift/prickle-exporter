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
| `crio-<hex>.scope` (CRI-O) | **Captured**, in `crio-systemd-20260801/`. |
| Guaranteed pods — `kubepods-pod<uid>.slice`, with no QoS component | **Captured**, in `kubeadm-systemd-20260801/`. |
| `cpu.pressure`, `io.pressure` | Read by the collector, covered by a hand-written tree in `TestPerCgroupPressure`. The capture script collects only `memory.pressure`; the format is identical to it and to the `/proc/pressure/*` files the Phase 1 fixtures do capture. |
| A container with a CPU quota (`cpu.max` = `<quota> <period>`) | **Captured**, in `docker-cgroupfs-20260801/`. The hand-written trees in `cpu_test.go` stay — they pin the arithmetic in isolation — but the quota, the throttle counters and the conversion to cores are now also checked against values a kernel actually produced. |
| cgroupfs-driver Docker — `/sys/fs/cgroup/docker/<hex>/` | **Implemented and captured**, in `docker-cgroupfs-20260801/`. Unlike the kubepods layout, this one *does* name its runtime: the parent directory is literally `docker`. |
| Non-systemd kubelet — `kubepods/<qos>/pod<uid>/<hex>` | **Implemented and captured.** `doks-cgroupfs-20260801/` is that host, with its own golden file; see below. The runtime is not recoverable from it — that is a gap in the layout, not in the parser. |
| Guaranteed pods under the cgroupfs driver — `kubepods/pod<uid>/<hex>` | Directory-name parse only, in `TestIdentify`. Neither cluster ran a Guaranteed pod, in either layout. |

Closing cgroupfs-driver Docker needs a capture from a host configured that way;
the rest need a CRI-O host, a Guaranteed pod, and a container with a CPU limit,
which `capture-fixtures.sh prep` could arrange on a disposable rental.

## The cgroupfs-driver tree: `doks-cgroupfs-20260801/`

A second capture, from a DigitalOcean managed Kubernetes node — Debian 13,
kernel 6.12.96, containerd 2.2.3, Kubernetes 1.36.3 — taken on 2026-08-01. It
is the shape the table above calls "non-systemd kubelet", and it is what a
managed cluster gives you by default rather than an exotic configuration.

The difference is total, not cosmetic:

| | systemd driver (`h200-ubuntu2204-20260726`) | cgroupfs driver (this tree) |
|---|---|---|
| Pod directory | `kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod4d52_1664_….slice` | `kubepods/burstable/pod4d521664-aa00-4570-9841-ce67a3756762` |
| Container directory | `cri-containerd-<hex>.scope` | `<hex>` — bare, no prefix and **no `.scope` suffix** |
| UID spelling | systemd-escaped, `_` for `-` | the UID as Kubernetes writes it |
| QoS | in the slice name | a directory level of its own |

`identify` in [cgroup.go](../cgroup.go) used to require the `.scope` suffix
before looking at anything else, so on this tree it rejected every directory
and the collector reported **zero containers where fourteen cgroups exist**.
`identifyBareID` now reads this shape: fourteen containers across five pods,
353 series, pinned by `container-cgroupfs.prom`.

A bare hex name is not by itself enough to call a directory a container — its
parent has to be a pod directory. That is what stops the new branch matching
some unrelated hex-named cgroup, and it is why the systemd tree still parses
exactly as before.

The five pods captured, so the tree can be read against what was running:

| Pod UID | Pod | QoS |
|---|---|---|
| `4d521664-aa00-4570-9841-ce67a3756762` | `kube-system/cilium-fc58t` | Burstable |
| `b3e47cc2-5076-4270-b881-6a55451c2f64` | `kube-system/do-node-agent-nvidia-dcgm-exporter-s5j92` | Burstable |
| `b023f31c-7dbb-42b5-919e-4fd7cb3ed9cc` | `kube-system/csi-do-node-h86ft` | BestEffort |
| `e7aa4094-2f07-4a8a-b4b1-fb1f38d6c2dd` | `kube-system/doks-telemetry-config-reloader-gjwsv` | BestEffort |
| `1168c1cf-27d9-4a96-8ba5-4a58d4acb6e8` | `kube-system/k8s-nvidia-device-plugin-5kddt` | BestEffort |

Still no Guaranteed pod: the cluster ran none, in either QoS layout. Under this
driver a Guaranteed pod sits at `kubepods/pod<uid>/` with no QoS level at all,
which is a third shape and remains uncaptured.

**The runtime is not in these names.** The systemd layout spells it —
`cri-containerd-`, `docker-`, `crio-` — and this one does not, so a parser
reading this tree can know a container is there and not what runs it. That is a
gap in the source, not in the parser, and the honest reporting is an empty
`runtime` on `prickle_container_info` rather than an inference from the node's
`containerRuntime` field, which the cgroup walk cannot see and which SPEC.md
§Metrics contract would not let onto a hot series anyway.

The capture came through an already-running pod — the node's `cilium-agent`
bind-mounts the host cgroup2 root at `/run/cilium/cgroupv2` — so nothing was
scheduled and no image was pulled to obtain it. The same thirteen per-cgroup
files as the systemd tree, and `cgroup.procs` excluded for the same reason.

## The Guaranteed-pod tree: `kubeadm-systemd-20260801/`

A kubeadm node — Kubernetes 1.34.10, containerd 2.2.2, kernel 7.0.0, kubelet on
the **systemd** cgroup driver — captured 2026-08-01 from a two-node cluster
stood up for this purpose.

It exists for one directory name. `kubepods-pod<uid>.slice`, the Guaranteed
shape with no QoS component, had been parse-only since Phase 2 for a reason
that never resolved itself: no cluster anyone captured was running a Guaranteed
pod. Managed DigitalOcean had none, the original rental had none. So three pods
were created deliberately, one per QoS class, and all three shapes now sit in
one tree:

| Pod | QoS | Slice |
|---|---|---|
| `qos-guaranteed` | Guaranteed | `kubepods.slice/kubepods-pod54af9685_6b23_4dc9_aaf3_85520df7a05e.slice` |
| `qos-burstable` | Burstable | `kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1e387c5f_…slice` |
| `qos-besteffort` | BestEffort | `kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod59ae0a77_…slice` |

`podSlicePattern` made the QoS component optional and defaulted the missing
case to `guaranteed`, which was correct — the capture confirms the rule rather
than correcting it. What it adds is that the rule has now been run against a
kernel's output instead of against the systemd naming documentation, and that
all three spellings coexist in one walk without one swallowing another.

The Guaranteed pod's `cpu.max` reads `25000 100000`: for a Guaranteed pod the
limit equals the request, which is why the quota exists at all.

Ten container scopes, the same thirteen per-cgroup files as the other trees,
and `cgroup.procs` stripped for the usual reason.

## The CRI-O tree: `crio-systemd-20260801/`

The same kubeadm cluster and the same three QoS pods as
`kubeadm-systemd-20260801`, with the worker's runtime switched to **CRI-O
1.32.1** and the node rebooted so nothing containerd wrote survives. Two
captures differing in the runtime and nothing else, which is what makes them
comparable.

`crio-<hex>.scope` had been parse-only since Phase 2: SPEC.md §Collectors names
CRI-O, but the prefix came from the runtime's documentation rather than from a
host. It is right.

**What the documentation did not say** is that CRI-O writes *two* directories
per container:

| Directory | Contents |
|---|---|
| `crio-<hex>.scope` | The container. Real processes and memory — 1 to 20 pids, 0.4 to 72 MB across this capture |
| `crio-<hex>` (no suffix) | **Empty.** Zero processes, zero bytes, five of them |

Nothing was written with that sibling in mind. It is skipped because
`identifyScope` requires the `.scope` suffix and `identifyBareID` requires a
bare hex name, and `crio-<hex>` is neither — the right outcome reached by
accident. `TestCrioSuffixlessSiblingsAreSkipped` pins it, and asserts the
siblings are empty rather than assuming it: skipping them is free only while
they hold nothing, and counting them would double this node's container count
with five containers reading zero everywhere, which looks like five idle
containers rather than an artefact.

**One cross-runtime difference worth knowing before comparing counts.** On this
node containerd produced two scopes per pod and CRI-O produces one, so the same
five pods yield ten containers under containerd and five under CRI-O. Neither
is wrong — they are different statements about what a container is — but a
dashboard counting `prickle_container_info` will show a step change if a node's
runtime is switched underneath it.

## The Docker cgroupfs tree: `docker-cgroupfs-20260801/`

Ubuntu 24.04, Docker 29.1.3 configured with
`"exec-opts": ["native.cgroupdriver=cgroupfs"]`, captured 2026-08-01. Docker
then places containers at `/sys/fs/cgroup/docker/<hex>/` rather than
`system.slice/docker-<hex>.scope`, which is the shape this file listed as
unconfirmed for two phases.

Three containers, chosen for their `cpu.max` states rather than their
workloads — that is the part no previous capture could supply:

| Container | `cpu.max` | `memory.max` | What it pins |
|---|---|---|---|
| `d38f2cabd19d…` | `25000 100000` | `max` | A quota **being hit**: a busy loop against 0.25 CPU, throttled in 385 of 386 periods, 28.797198 s stalled |
| `f628cec04bf1…` | `150000 100000` | `67108864` | A quota **never hit**, and a real 64 MiB memory limit |
| `db66742d610a…` | `max 100000` | `max` | No quota at all — `cpu_limit_cores` must not be emitted, and every counter stays zero |

Until this tree existed, every cgroup in every capture read `max 100000`, so
`nr_periods` and `nr_throttled` were zero throughout `container.prom` and the
quota arithmetic was pinned only by hand-written trees. Those tests remain —
they isolate the arithmetic — but `0.25` and `1.5` cores are now derived from
numbers a kernel wrote, and the throttled seconds from time a kernel actually
withheld.

`cgroup.procs` was captured by the script and stripped before committing, for
the reason in **Not copied** below.

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

# Where this has been tested

Evidence rather than instructions. What was run, on what, and what it found —
so a claim in the README can be checked rather than taken on trust.

[← README](../README.md)

## Base images

Every DigitalOcean base image, swept on 2026-08-01: rebuilt from the stock
image, podman installed from the distro's own repository, one container started
with a CPU quota and a memory limit, then `prickle diagnose` and a real scrape.

| Image | Kernel | cgroup | PSI | podman | Found | Host series | Container series |
|---|---|---|---|---|---|---|---|
| `almalinux-8` | 4.18.0-553 | **v1** | absent | 4.9.4 | 1 | 310 | 20 |
| `almalinux-9` | 5.14.0-687 | v2 | absent | 5.8.2 | 1 | 328 | 24 |
| `almalinux-10` | 6.12.0-211 | v2 | absent | 5.8.2 | 1 | 329 | 24 |
| `centos-stream-9` | 5.14.0-710 | v2 | absent | 5.8.5 | 1 | 276 | 24 |
| `centos-stream-10` | 6.12.0-233 | v2 | absent | 6.0.2 | 1 | 277 | 24 |
| `debian-13` | 6.12.94 | v2 | present | 5.4.2 | 1 | 359 | 30 |
| `fedora-43` | 7.0.12 | v2 | present | 5.8.2 | 1 | 414 | 30 |
| `fedora-44` | 7.0.12 | v2 | present | 5.8.3 | 1 | 396 | 30 |
| `rockylinux-8` | 4.18.0-553 | **v1** | absent | 4.9.4 | 1 | 326 | 20 |
| `rockylinux-9` | 5.14.0-687 | v2 | absent | 5.8.2 | 1 | 336 | 24 |
| `rockylinux-10` | 6.12.0-211 | v2 | absent | 5.8.2 | 1 | 366 | 24 |
| `ubuntu-22-04` | 5.15.0-185 | v2 | present | 3.4.4 | 1 | 310 | 28 |
| `ubuntu-24-04` | 6.8.0-124 | v2 | present | 4.9.3 | 1 | 340 | 30 |
| `ubuntu-26-04` | 7.0.0-27 | v2 | present | 5.7.0 | 1 | 380 | 30 |

**14 of 14 found their container, with zero exposition problems** across
kernels 4.18 to 7.0, both cgroup hierarchies, and six years of podman versions
(3.4.4 to 6.0.2).

Two things in that table are worth reading before you deploy:

- **PSI is absent on the entire RHEL family** — Alma, Rocky and CentOS Stream,
  at 8, 9 and 10 alike. The kernels have it compiled in; RHEL ships it *off*
  unless the host is booted with `psi=1`. So `prickle_host_pressure_*` and
  `prickle_container_pressure_stalled_seconds_total` are simply not there, and
  any dashboard panel or alert built on saturation will be blank. This is a
  property of the distribution, not of the exporter, and `prickle diagnose`
  reports each `/proc/pressure/*` file as `missing` rather than failing.
- **The container series count is explained entirely by those two columns.**
  20 on cgroup v1 (no PSI, and v1's `memory.stat` has no counterpart for four
  of the v2 fields), 24 on RHEL v2 (PSI off), 28–30 elsewhere. A host reporting
  fewer series than its neighbour is usually this, not a fault.


## NVIDIA: two builds

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

### Enable persistence mode if you deploy the `nvidia-smi` path

Measured on an idle H100, driver 580.173.02, with **persistence mode disabled**:
a single `nvidia-smi` query costs **2.7–3.5 s**, because the driver tears down
its state when the last client exits and rebuilds it for the next one. This
source spawns several queries per pass, so it overruns the default 5 s
`-collector.timeout` and reports a killed subprocess **on every scrape**.

Turning persistence mode on took the same pass from **5.4 s to 61 ms**:

```sh
nvidia-smi -pm 1          # or run nvidia-persistenced
```

`prickle diagnose` says this when it sees the overrun rather than leaving you to
find it. `prickle-nvml` is not affected — it holds NVML open across passes,
which keeps the driver initialised for the same reason persistence mode does.
Raising `-collector.timeout` also stops the errors, but hides the latency
instead of removing it.

Both are published per architecture from `v0.3.0` on. Dynamic linking costs
`prickle-nvml` a glibc floor — it is built on glibc 2.39 and needs at least that
on the host — which is the one reason to prefer `prickle` on a machine that does
have a GPU. Installing `prickle-nvml` where NVML cannot load is otherwise
harmless: it falls back to the `nvidia-smi` path by itself, and `prickle
diagnose` names the live source and why the other declined.

The hardware test lives in
[internal/collector/gpu/nvml_hardware_test.go](../internal/collector/gpu/nvml_hardware_test.go)
and skips itself wherever NVML does not load, so `go test -tags nvml` stays
green on a laptop. It was run on an **H100 80GB** (2026-07-29) and an **H200 141GB**
(2026-07-30), driver 580.173.02, in both Default and MIG mode, and it found
three disagreements that no fixture could have:

| Found | Effect had it shipped |
|---|---|
| NVML counted driver-reserved memory as used | `prickle_gpu_memory_used_bytes` 480 MiB higher per card from `prickle-nvml` than from `prickle` — every memory panel and capacity alert shifting with the artifact deployed |
| NVML *derived* a MIG profile name instead of reading it — three plausible rules, each wrong on some card | The `profile` label on `prickle_gpu_mig_info` differing between the two binaries for the same card, for **every profile an H200 offers** |
| A second GPU collector in one process got an already-closed NVML handle | `prickle diagnose` reporting `NVML source is closed` on a host where NVML worked perfectly |

All three are fixed, and each has a test that fails if it comes back. Two card
classes is not redundancy: the C struct layouts the NVML build declares are
only known not to be H100-shaped by coincidence because a second card agrees
with them.

Both are read-only. Every NVML symbol bound is a `Get`, resolved by its
versioned name (`nvmlDeviceGetComputeRunningProcesses_v3`) so the struct layouts
are fixed by NVIDIA's own ABI contract rather than by hope. Nothing from the
`nvmlDeviceSet*` or `nvmlDeviceClear*` families is resolved at all.


## Fixture coverage

Every parser is developed against a captured tree, never an invented one
(SPEC.md §Testing rules). Each capture records what it covers and — more
usefully — what it does not:

- [Container fixtures](../internal/collector/container/testdata/README.md) —
  both cgroup hierarchies, both cgroup drivers, four runtimes, a Guaranteed
  pod in each driver's layout, and a CPU quota actually being hit. As of
  2026-08-02 its coverage-gap table has no open rows.
- [GPU fixtures](../internal/collector/gpu/testdata/README.md) — four NVIDIA
  captures across two card classes, the hardware verification log, and the
  three defects that only running on hardware could find.
- [Host fixtures](../internal/collector/host/testdata/README.md).

## Decisions and their reasons

This file records *where* things were tested. The reasoning behind what was
built lives in two places by design:

- **[SPEC.md](../SPEC.md)** — the frozen contract. Every decision, and for the
  reversed ones, what changed the mind.
- **[CHANGELOG.md](../CHANGELOG.md)** — written by hand, per release, because a
  metric change needs prose telling an operator what to do about it.

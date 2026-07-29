# Phase 3 GPU fixtures

`h200-mig-20260726/nvidia/` is the NVIDIA half of the same capture Phases 1 and
2 come from, made by
[scripts/capture-fixtures.sh](../../../../scripts/capture-fixtures.sh).

Unlike the other two phases, this tree is **not** a mirrored filesystem layout
and `fsroot` does not point at it. `nvidia-smi` is a subprocess, not a file, so
SPEC.md §Collectors puts it behind an interface — the same reason `Statfser`
exists in the host collector — and the tests replay these files through a fake
`commandRunner`. What the layout mirrors is the *capture*, not the host.

| | |
|---|---|
| Host | single-tenant H200 rental, since destroyed |
| Captured | 2026-07-26T05:42:16Z |
| Hardware | 1× NVIDIA H200 141 GB, **MIG enabled** with two `1g.18gb` instances |
| Driver | 580.173.02, CUDA 13.0 |

Everything here is **captured, unmodified** `nvidia-smi` output.

## What this tree exercises

The capture is unusually valuable because the rental was deliberately put into
the awkward state: MIG on, with a real compute process resident on a MIG
instance. Both of the limitations SPEC.md §Collectors records for the
`nvidia-smi` source are therefore *present in the fixture* rather than described
in prose, and a parser that mishandles either fails a test.

- **`query-gpu.csv`** — the eight-column device row, whose `utilization.gpu`
  reads `[N/A]`. That is the first recorded limitation: the driver reports no
  card-level utilization at all while MIG is enabled. A parser that treats the
  bracketed token as a number fails; one that treats it as an error loses the
  seven columns that did parse; one that defaults it to zero reports an idle
  H200 to an alerting rule. The metric is simply absent.
- **`query-compute-apps.csv`** — one process, `/tmp/prickle-gpu-spin`, running
  on MIG device 0 and reported against the **parent** GPU's UUID. That is the
  second recorded limitation: per-process MIG attribution is unavailable from
  this source. The fixture is what stops anyone "fixing" that by inventing a
  `mig_uuid`.
- The same row carries **PID 12648** in a column the parser reads and discards.
  SPEC.md §Metrics contract forbids a PID as a label or a value anywhere, and
  `TestNoPIDAnywhere` asserts that this specific number never reaches the
  output — the one place in Phase 3 it could.
- **`gpus.txt`** (`nvidia-smi -L`) — the GPU line plus two indented MIG lines
  with their own UUIDs. This is the only captured output carrying MIG UUIDs, and
  the only one that answers "is MIG on" — the captured `--query-gpu` field set
  has no `mig.mode` column, and adding one would mean parsing output no capture
  records.
- **`smi.txt`, `mig-gi.txt`, `mig-ci.txt`, `mig-profiles.txt`** — captured for
  reference and **not parsed**. See below.

## Deliberately not parsed

`smi.txt` holds per-MIG memory (`624MiB / 16384MiB`) that no CSV query
publishes, and `mig-gi.txt` / `mig-ci.txt` hold the GPU-instance and
compute-instance IDs. All three are box-drawing tables meant for humans. They
are kept because they are the reference for what the NVML path must report, and
they are not parsed because:

- **Per-MIG memory** would mean parsing an ASCII table whose column widths shift
  with driver version. `prickle_gpu_mig_memory_used_bytes` is therefore absent
  from the `nvidia-smi` source and present from NVML, which has a direct API for
  it — SPEC.md §Collectors calls NVML "the only reliable source of MIG topology"
  for exactly this reason.
- **GI and CI IDs** are listed in `mig-gi.txt` and the MIG UUIDs in `gpus.txt`,
  but *nothing captured joins the two*. Pairing them would mean assuming both
  listings are in the same order. They are not exposed at all, by either source:
  a label NVML could fill and `nvidia-smi` could not would break the requirement
  in SPEC.md §Testing rules that both sources emit identical output.

## Coverage gaps

| Gap | Status |
|---|---|
| **AMD — sysfs + DRM fdinfo** | **Not implemented.** SPEC.md §Collectors scope; the captured host is NVIDIA-only, so there is no `gpu_busy_percent`, no `mem_info_vram_*` and no `hwmon` tree to develop against. An AMD host reports nothing. |
| **Intel — DRM fdinfo** | **Not implemented**, same reason. |
| NVML — the whole path | Cannot be fixture-tested: a C call, not a file read (SPEC.md §Testing rules). Unit tests drive the shared emission code through a fake source; the NVML implementation itself is **unverified until it runs on hardware**. |
| A card in Default mode (MIG off) | Covered by a hand-written `-L` override in `TestDefaultModeCardHasNoMIG`. The rental had MIG on for the whole capture. |
| A multi-GPU host | Single card. The parsers key on UUID rather than position specifically so a second card cannot silently attach its partitions to the first, but nothing captured proves it. |
| `[Not Supported]` / `[Unknown Error]` tokens | Only `[N/A]` appears in the capture. The others are handled by the same bracket-shape rule and covered in `TestBracketedTokensAreAbsentNotErrors`. |

The AMD gap is the significant one: it is a third of what SPEC.md §Collectors
assigns to Phase 3, and closing it needs a capture from an AMD host with a ROCm
workload running — `capture-fixtures.sh check` already reports whether a host
would produce usable `drm-*` fdinfo keys.

## The DRM fdinfo negative

The full capture contains `/proc/<pid>/fdinfo/<fd>` for every process holding an
NVIDIA device, and **none of them carry `drm-*` keys** — only `pos`, `flags`,
`mnt_id` and `ino`. That is a wanted *negative* result, not a failed capture:
it is the evidence that per-process GPU attribution on the NVIDIA proprietary
driver must come from NVML or `nvidia-smi` and can never come from DRM fdinfo.
Those files are not copied here because nothing in this package reads them; the
finding is recorded in [scripts/README.md](../../../../scripts/README.md)
§Platform notes.

## Golden output

`golden/gpu.prom` is what this tree renders to through the `nvidia-smi` source,
with `node="fixture"` and per-process attribution on. It is checked
byte-for-byte and verified with `promtool check metrics`. Regenerate after an
intentional change with:

```sh
go test ./internal/collector/gpu/ -update-golden
```

Read the diff before committing it — the golden is the review surface for every
metric name, label and unit in Phase 3.

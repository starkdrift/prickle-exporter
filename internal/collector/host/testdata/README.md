# Phase 1 host fixtures

`h200-ubuntu2204-20260726/` is a curated subset of a capture made by
[scripts/capture-fixtures.sh](../../../../scripts/capture-fixtures.sh), kept in
the mirrored path shape so `fsroot.At("testdata/h200-ubuntu2204-20260726")`
resolves straight into it (SPEC.md §Testing rules).

| | |
|---|---|
| Host | single-tenant H200 rental, since destroyed. Its hostname is deliberately not recorded here; the directory name is what identifies the capture. |
| Captured | 2026-07-26T05:42:16Z |
| OS | Ubuntu 22.04.5 LTS, kernel 5.15.0-185-generic, x86_64 |
| Hardware | 24 cores, 236 GiB RAM, 1× H200 141 GB |
| cgroup | `cgroup2fs` — pure v2 |

Everything under `h200-ubuntu2204-20260726/proc/` is **captured, unmodified**
kernel output. Only the files the Phase 1 parsers read were copied over from the
full capture; the rest of that tree belongs to Phases 2 and 3.

Nothing in the tree carries the host's name — none of these `/proc` files
contain one. The container and pod IDs in `proc/mounts` are ephemeral
identifiers from a machine that no longer exists.

## What this tree exercises

- **`proc/stat`** — 24 per-core lines plus the aggregate, all ten mode columns,
  and the wide `intr` / `softirq` vector lists the parser deliberately truncates
  to their first field.
- **`proc/meminfo`** — 51 fields, including every awkward name shape the
  snake_case converter has to handle: `Active(anon)`, `SReclaimable`,
  `NFS_Unstable`, `Committed_AS`, `DirectMap2M`, and the unitless `HugePages_*`
  counts.
- **`proc/diskstats`** — a 5.15 kernel, so all 17 columns are present including
  discard (4.18+) and flush (5.5+). Contains `loop0`–`loop7`, which the default
  ignore regexp drops, and partitions (`vda1`, `vda15`) which it keeps.
- **`proc/net/dev`** — 15 interfaces, including `vethec7b279:` and
  `veth3d5d7dc0:` whose names run up against the colon with no separating
  space. That is the case that breaks a whitespace split, and the reason the
  parser cuts on the colon.
- **`proc/pressure/{cpu,memory,io}`** — PSI present, with a `full` line for cpu
  (which older kernels omit) and a non-zero `total=` on cpu and io.
- **`proc/mounts`** — 60 mounts: the five an operator wants, plus overlay,
  nsfs, squashfs, k3s sandbox and per-pod kubelet mounts. This is the tree the
  filesystem exclusion defaults were tuned against.

## `statfs-reference.txt` — the one synthetic edge

`statfs(2)` is a syscall, not a file, so SPEC.md §Collectors puts it behind the
`Statfser` interface and there is nothing to capture. `statfs-reference.txt` is
the capture's `df -B1` output, and `fakeStatfs` in [../filesystem_test.go](../filesystem_test.go)
is seeded from it: block size 1, blocks = the `1B-blocks` column, available =
`Avail`, free = `1B-blocks` − `Used`.

**`df` does not report inode counts, so the inode numbers in that fake are
synthetic** — a documented per-mount table in the test, not captured data. They
exist to exercise the `filesystem_files` / `filesystem_files_free` code path.
No other value in this tree is invented.

## Golden output

`golden/host.prom` is the exposition document this tree renders to, with
`node="fixture"`. It is checked byte-for-byte and verified with
`promtool check metrics`. Regenerate after an intentional change with:

```sh
go test ./internal/collector/host/ -update-golden
```

Read the diff before committing it — the golden is the review surface for every
metric name, label and unit in Phase 1.

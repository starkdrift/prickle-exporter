# scripts/

| Script | Mutates anything? | What it is for |
|---|---|---|
| [dev-run.sh](dev-run.sh) | no | Start the exporter from source on your own machine. |
| [capture-fixtures.sh](capture-fixtures.sh) | `prep` does, heavily | Dump a real host's `/proc`, `/sys`, cgroup and GPU output into a fixture tree. |

The pre-commit gate is not here — it is [ci/check.sh](../ci/check.sh).

## Running the exporter — dev-run.sh

Starts `prickle` from source with dev-friendly defaults (debug logging, a 2s
sample interval instead of 10s). No root: every `/proc` file the Phase 1 host
collector reads is world readable.

```sh
./scripts/dev-run.sh              # live host, serve on :10047 until Ctrl-C
./scripts/dev-run.sh fixture      # same, but reading a captured fixture tree
./scripts/dev-run.sh diagnose     # what this host can and cannot be read from
./scripts/dev-run.sh scrape       # start, scrape once, print, promtool, stop
```

Anything after the subcommand goes straight to the binary, after the script's
own flags, so it wins — Go's `flag` keeps the last occurrence:

```sh
./scripts/dev-run.sh run -collector.cpu.per-core -log.level=info
./scripts/dev-run.sh scrape -path.rootfs=internal/collector/host/testdata/h200-ubuntu2204-20260726
```

`ADDR`, `TELEMETRY_PATH`, `INTERVAL`, `LOG_LEVEL`, `NODE` and `GO` override the
defaults from the environment. `ADDR` exists for when something else on your
workstation already holds 10047 — SPEC §Identity fixes the port, so don't
change it in anything that ships.

Notes on the two less obvious modes:

- **`fixture`** finds the tree itself by looking for a `proc/` directory under
  `internal/collector/*/testdata/*/`, so a new capture doesn't leave the script
  pointing at the old one; pass a path as the first argument when there is more
  than one. There is more than one as of Phase 2: each collector's `testdata/`
  holds the subset of the capture that *its* parsers read, so the host tree has
  no cgroups and the container tree has only the one `/proc` file it needs.
  Neither exercises every phase at once, and the script says so rather than
  picking for you. It sets `-node=prickle-fixture` so output doesn't carry your
  hostname. `Statfs` is a syscall rather than a file (SPEC §Collectors), so the
  filesystem series still describe *your* machine, and fixture mount points that
  don't exist locally come back as `prickle_filesystem_error 1`. That is the
  expected result, not a broken fixture.
- **`scrape`** builds to `bin/` rather than using `go run`, because `go run`
  execs the binary as a child and killing it would leave the exporter holding
  the port. Output lands in `bin/dev-scrape.prom` (gitignored) and is linted
  with `promtool check metrics` — the same gate `ci/check.sh` runs on the golden
  files, here against live output, where the host rather than the fixture
  decides what gets named.

## Fixture capture — capture-fixtures.sh

[SPEC.md](../SPEC.md) §Testing rules: every parser is developed against a
captured fixture tree under `testdata/`, and **file formats and path shapes are
never invented**. [capture-fixtures.sh](capture-fixtures.sh) is how those trees
get made — it dumps the exact `/proc`, `/sys`, cgroup, Docker-API and GPU
vendor-tool output a real host exposes, laid out mirroring real paths so
`internal/fsroot` can be pointed straight at the tree.

A capture is only useful if the interesting state was *live* at capture time. An
idle host with no containers and no GPU workload produces a tree full of empty
files that looks complete and isn't. The script's `check` and `prep` commands
exist to prevent exactly that, because it already happened once (see
[Known-good and known-empty](#known-good-and-known-empty)).

### The rental workflow

Capture hosts are usually rented by the hour and destroyed afterwards, so the
order matters:

```sh
sudo ./scripts/capture-fixtures.sh check    # what would a capture contain?
sudo ./scripts/capture-fixtures.sh prep     # disposable hosts ONLY — mutates the box
sudo ./scripts/capture-fixtures.sh check    # confirm the gaps closed
sudo ./scripts/capture-fixtures.sh          # capture (read-only)
```

Then copy the tarball off the host, verify it locally, and **only then** destroy
the machine. If the capture ends with the `CAPTURE IS INCOMPLETE` banner, the
host still has something you can't get back once it's gone.

### Commands

| Command | Mutates host? | Root | Exit | What it does |
|---|---|---|---|---|
| `check` | no | recommended | `0` clean, `2` gaps found | Preflight. Reports which fixtures would come back empty: MIG disabled on a MIG-capable card, no running containers, no `kubepods.slice`, no GPU compute apps, no `drm-*` fdinfo keys on an AMD host. Gate scripts on the exit code. |
| `prep` | **yes, heavily** | required | passthrough from `check` | Disposable hosts only. Installs Docker and starts three named containers, installs k3s and waits for one pod, stops `dcgm-exporter`/`nvidia-persistenced` and enables MIG with two instances on datacenter cards, then starts a GPU workload pinned to the first MIG UUID. Prints a 5-second abort warning first. |
| `capture` (default) | no | required for a full capture | `0` clean, `2` gaps in the output | The actual read-only dump, ending with a gap report and a tarball. |

`prep` deliberately violates the project's read-only rule — that is why it is a
separate subcommand that `capture` never calls, and why it refuses to be
subtle about what it's doing. Never run it on a machine you intend to keep.

`capture` runs without root but warns: other processes' `fdinfo` and `cgroup`
files won't be readable, which silently guts the Phase 2 and Phase 3 material.
Use `sudo`.

The script runs `set -uo pipefail` **without** `-e`, on purpose: a partial
capture from a host you're about to lose beats no capture at all. Individual
failures are reported as `[skip]` and the run continues.

### What comes back, by phase

| Phase | Fixtures | Source |
|---|---|---|
| 1 — Host | `/proc/{stat,meminfo,diskstats,loadavg,uptime,mounts}`, `/proc/net/dev`, `/proc/pressure/{cpu,memory,io}` | direct read |
| 2 — Containers | `docker-*.scope` and `/sys/fs/cgroup/docker/*` cgroup files, the full `kubepods.slice` tree, `GET /containers/json` off the Docker socket | cgroup v2 tree + Docker API |
| 3 — GPU | `nvidia-smi -L`, `--query-gpu`, `--query-compute-apps`, `mig -lgip/-lgi/-lci`; AMD `gpu_busy_percent`, `mem_info_vram_{used,total}`, `hwmon/*`; `rocm-smi --showall`; per-process DRM/NVIDIA `fdinfo` plus that process's `cgroup`, `comm` and `exe` symlink target | vendor tools + sysfs + `/proc/<pid>/fdinfo` |

Per-cgroup files captured: `cgroup.type`, `cgroup.procs`, `cpu.stat`, `cpu.max`,
`cpu.weight`, `memory.{current,max,min,low,high,stat,pressure}`, `io.stat`,
`pids.{current,max}`.

`Statfs` (SPEC §Collectors, Phase 1) can't be captured as a file — it's a
syscall behind an interface. The script writes `meta/statfs-reference.txt` from
`df -B1` instead, as a cross-check for hand-built values.

### What the next capture should add

The captured H200 answered Phase 1 and most of Phase 2. What it did not contain
is the reason for the coverage gaps recorded in
[internal/collector/container/testdata/README.md](../internal/collector/container/testdata/README.md#coverage-gaps),
and `check` does not currently flag any of it. If you are renting a host anyway,
these are cheap to arrange and each one closes a gap:

| Arrange | Closes |
|---|---|
| Docker configured with `"exec-opts": ["native.cgroupdriver=cgroupfs"]`, or a second capture from such a host | Docker's cgroupfs-driver layout, `/sys/fs/cgroup/docker/<hex>/` — **unimplemented today, so those hosts report no containers at all** |
| A kubelet with `cgroupDriver: cgroupfs` | The matching `kubepods/besteffort/pod<uid>/<hex>` layout — same, unimplemented |
| A CRI-O host | `crio-<hex>.scope`, currently unit-tested on the name parse only |
| One pod with equal requests and limits | Guaranteed pods — `kubepods-pod<uid>.slice`, with no QoS component in the name |
| `docker run --cpus=2`, or a pod with a CPU limit | A non-`max` `cpu.max`, so `cpu_limit_cores` and the throttling counters are seen with real values rather than parsed in the dark |
| Adding `cpu.pressure` and `io.pressure` to `CG_FILES` | Two files the collector already reads, covered today by a hand-written tree rather than a capture |

The first two rows are the ones that cost a real operator metrics, so they are
worth a capture on their own. Everything else can ride along with the Phase 3
GPU capture.

### Output layout

```
prickle-fixtures-<host>-<YYYYMMDD>/
  meta/            host.txt, os-release.txt, docker-version.txt,
                   nvidia-driver.txt, drm-listing.txt, rocm-smi.txt,
                   statfs-reference.txt
  proc/            mirrored /proc paths, including proc/<pid>/fdinfo/<fd>,
                   proc/<pid>/{cgroup,comm,exe.link}
  sys/             mirrored /sys and /sys/fs/cgroup paths
  docker-api/      containers.json
  nvidia/          gpus.txt, query-gpu.csv, query-compute-apps.csv,
                   mig-profiles.txt, mig-gi.txt, mig-ci.txt
prickle-fixtures-<host>-<YYYYMMDD>.tar.gz
```

Override the directory with `OUT_DIR=/path ./capture-fixtures.sh`.

Files are read with `cat`, not `cp`: `/proc` and `/sys` files report size 0 and
`cp` mangles them. `exe` is stored as `exe.link` (the resolved target as text),
since a symlink into a fixture tree would dangle.

### Platform notes

#### NVIDIA

- **MIG is off by default** on every cloud rental seen so far. Enabling it
  requires no process to hold the GPU, which on NVIDIA's own images means
  stopping `dcgm-exporter` and `nvidia-persistenced` first — `prep` does this.
  If `nvidia-smi -mig 1` still fails, the host needs a reboot before MIG
  activates; capture after the reboot.
- **A compute workload must be running** or `query-compute-apps.csv` is empty
  and there is nothing to test per-process attribution against. `prep` uses
  `dcgmproftester` when present, falling back to a torch matmul loop, pinned via
  `CUDA_VISIBLE_DEVICES` to the first MIG UUID so the CSV rows are
  MIG-attributed — the exact row shape the parser most needs.
- **The proprietary driver emits no `drm-*` keys in fdinfo.** Processes holding
  `/dev/nvidia*` fds show only `pos`/`flags`/`mnt_id`/`ino`. This is a wanted
  *negative* fixture, not a failed capture: it's the evidence that per-process
  GPU attribution on NVIDIA must come from NVML or `nvidia-smi`, never from DRM
  fdinfo. Keep it, and mark it as such in the `testdata/` README.

#### AMD

- Prefer bare metal or a full VM. A container or a paravirtualised slice can
  give you a partial or synthetic `amdgpu` sysfs tree, which is worse than none.
- The `amdgpu` driver *does* emit `drm-*` fdinfo keys, so a ROCm workload must be
  running during capture — the NVIDIA-empty / AMD-populated pair is precisely
  the contrast the Phase 3 parser tests are built on. `prep` handles Docker and
  k3s here but **cannot** start a ROCm workload for you; any ROCm PyTorch loop
  works. `check` will tell you if the keys are missing.
- No script changes are needed for AMD — the sysfs and `rocm-smi` sections
  engage automatically when `gpu_busy_percent` is present.

#### Intel

Intel GPUs are read through DRM fdinfo, same path as AMD, with no vendor-tool
section. The `/dev/dri/*` fdinfo capture already covers them; a dedicated Intel
run is worth doing once Phase 3 has an Intel code path.

### After the capture

1. **Review before committing.** The tree contains hostnames, container and
   image names, process names and command paths. Scrub anything you don't want
   in a public repo.
2. **Curate, don't dump.** `.gitignore` excludes `prickle-fixtures-*/` and its
   tarball so a raw capture can never be committed by accident. Copy only the
   files a test actually reads into `testdata/`, keeping the mirrored path shape
   so `fsroot` still resolves.
3. **PIDs in fixture paths are fine.** SPEC's "PID never appears" rule is about
   exported metrics — labels and values. A fixture tree mirrors real `/proc`, so
   `proc/1234/fdinfo/7` is the correct path and must stay.
4. **Mark synthetic material.** Hand-built fixtures are allowed only where
   hardware access is pending, and SPEC requires them flagged as synthetic in a
   README beside them. Don't mix them into a captured tree unlabelled.

### Known-good and known-empty

The first H200 rental (Ubuntu 5.15, driver 580.173.02) produced complete and
correct Phase 1 material, `cgroup2fs` confirming pure v2, and the NVIDIA
empty-fdinfo negative fixture above. It missed everything else: MIG was
disabled, no GPU workload was running, and the host had no containers at all —
so `containers.json` was `[]` and there was no `sys/` tree. That run is why
`check` and `prep` exist. Run `check` before you capture, and read the banner
before you destroy the host.

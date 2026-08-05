# scripts/

| Script | Mutates anything? | What it is for |
|---|---|---|
| [dev-run.sh](dev-run.sh) | no | Start the exporter from source on your own machine. |
| [capture-fixtures.sh](capture-fixtures.sh) | `prep` does, heavily | Dump a real host's `/proc`, `/sys`, cgroup and GPU output into a fixture tree. |
| [capture-dashboard.sh](capture-dashboard.sh) | no | Render a running Grafana dashboard to a README-sized PNG. |
| [gpu-load/](gpu-load/) | runs pods on a cluster | Two GPU tenants at a *target* utilisation, so a capture is not a flat line. |

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
| 3 — GPU | `nvidia-smi -L`, `--query-gpu`, `--query-compute-apps`, `mig -lgip/-lgi/-lci`; AMD identity (`unique_id`, `board_info`, `vbios_version`), utilisation, the three memory pools, partition mode, and **every** file in `hwmon/hwmon*` — sensor numbering differs per card, so the `*_label` files are what name them; `amd/drm-map.txt`; `rocm-smi --showall`, `amd-smi static`/`metric`; per-process DRM/NVIDIA `fdinfo` plus that process's `cgroup`, `comm` and `exe` symlink target | vendor tools + sysfs + `/proc/<pid>/fdinfo` |

Per-cgroup files captured: `cgroup.type`, `cpu.stat`, `cpu.max`,
`cpu.weight`, `cpu.pressure`, `memory.{current,max,min,low,high,stat,pressure}`,
`io.stat`, `io.pressure`, `pids.{current,max}`.

**`cgroup.procs` is not captured**, and must never be. It is the only file in a
cgroup holding PIDs, SPEC.md §Metrics contract forbids a PID appearing
anywhere, and the collector does not read it. It used to be collected and
stripped by hand before committing; `ci/check.sh` now fails if one appears
under any `testdata/` tree, so the rule is enforced rather than remembered.

`Statfs` (SPEC §Collectors, Phase 1) can't be captured as a file — it's a
syscall behind an interface. The script writes `meta/statfs-reference.txt` from
`df -B1` instead, as a cross-check for hand-built values.

### What the next capture should add

Nothing, for the container collector. Eleven captures now cover both cgroup
hierarchies, both cgroup drivers and four runtimes, and the coverage-gap table
in
[internal/collector/container/testdata/README.md](../internal/collector/container/testdata/README.md#coverage-gaps)
has no open rows. Both hardware gaps that remained — an AMD GPU, and a host with
more than one card — were closed together on 2026-08-04 by a 2× MI300X capture
(`internal/collector/gpu/testdata/mi300x-2gpu-20260804`). What is still missing
is a **bare-metal** AMD host: those cards are SR-IOV virtual functions, so
compute partitioning is fixed at `SPX` in the guest and AMD's analogue of a
MIG-on/MIG-off fixture pair cannot be taken there.

Two things to know if you are arranging a capture anyway.

**A Guaranteed pod has to be created deliberately.** QoS follows from requests
versus limits, and Guaranteed needs `requests == limits` for cpu *and* memory on
*every* container in the pod. No cluster observed for this project has ever run
one by accident, which is why that layout went uncaptured until 2026-08-02.

**Switching a node's cgroup driver requires a reboot before capturing.** The old
driver's directories survive a kubelet restart and `rmdir` will not clear them,
so the capture ends up holding both layouts at once — which is worse than not
capturing, because it looks complete.

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
  `dcgmproftester` when present, falling back to a torch matmul loop, then to an
  `nvcc`-built kernel, then to a `gcc` + `dlopen(libcuda)` spinner that
  JIT-compiles embedded PTX. All are pinned via `CUDA_VISIBLE_DEVICES` to the
  first MIG UUID when there is one, so the CSV rows are MIG-attributed — the
  exact row shape the parser most needs.
- **A workload that holds a context but runs no kernels is worse than none.**
  The PTX spinner's JIT is the fragile link in that chain: on an H100 with
  driver 580 / CUDA 13.0 its kernel launch failed with
  `CUDA_ERROR_CONTEXT_IS_DESTROYED` and it degraded to holding a context. The
  degradation is silent — `query-compute-apps` still has its row, so the old
  `check` passed — and the capture goes home with `utilization.gpu` reading `0`.
  **A `0` in a fixture cannot be told apart from a parser that turned an absent
  `[N/A]` into a zero**, which is the single most important thing the GPU
  fixtures exist to distinguish. `nvcc` is now tried before that path (NVIDIA's
  own images ship a toolkit at `/usr/local/cuda`), and both `check` and
  `capture` now report a numeric `0` alongside a resident process as a gap. A
  literal `[N/A]` is not flagged: under MIG it is the correct answer.
- **The proprietary driver emits no `drm-*` keys in fdinfo.** Processes holding
  `/dev/nvidia*` fds show only `pos`/`flags`/`mnt_id`/`ino`. This is a wanted
  *negative* fixture, not a failed capture: it's the evidence that per-process
  GPU attribution on NVIDIA must come from NVML or `nvidia-smi`, never from DRM
  fdinfo. Keep it, and mark it as such in the `testdata/` README.

#### AMD

Confirmed against a real host on 2026-08-04; the notes below are what that run
established rather than what was expected of it.

- Prefer bare metal or a full VM. A container or a paravirtualised slice can
  give you a partial or synthetic `amdgpu` sysfs tree, which is worse than none.
  **An SR-IOV virtual function is the case to watch for**: `amd-smi static`
  names it (`MI300X VF`), the sysfs tree is otherwise complete, and the one
  thing it costs is partition mode — `current_compute_partition` is `0444` in a
  guest, so the CPX/SPX pair that would mirror the MIG fixtures is unobtainable.
- The `amdgpu` driver *does* emit `drm-*` fdinfo keys, so a ROCm workload must be
  running during capture — the NVIDIA-empty / AMD-populated pair is precisely
  the contrast the Phase 3 parser tests are built on. `prep` handles Docker and
  k3s here but **cannot** start a ROCm workload for you; any ROCm PyTorch loop
  works, as does a dozen-line HIP kernel built with the `hipcc` a ROCm host
  already has. `check` will tell you if the keys are missing.
  Run one copy on the host and one in a container: the container copy is the
  only thing that exercises attributing a GPU process to a `container` label.
- The fdinfo keys identify a card by `drm-pdev` and carry no UUID, so the
  capture also writes `amd/drm-map.txt` — card ↔ PCI address ↔ render node.
  `grab` flattens the `card<N>/device` symlink that would otherwise carry the
  address, and without that map the per-process files cannot be tied to a card.
- **Do not trust `/usr/bin/nvidia-smi` on an AMD box.** The capture host shipped
  a two-line shell script by that name which prints `nvidia-smi not found. This
  is AMD country.` and exits **0**. Every exit-code probe in this script
  believed it: the preflight reported an NVIDIA driver, a MIG-capable card with
  MIG disabled, and one running compute app, on a host with no NVIDIA hardware.
  The probes now test the *output* — a real `--query-gpu=uuid` prints
  `GPU-<uuid>` — and three of those four gaps disappeared.

#### Intel

**Out of scope** as of SPEC §Collectors — no capture host is obtainable, and a
parser developed against a layout nobody has captured is exactly what §Testing
rules forbids. The script still grabs `/dev/dri/*` fdinfo, because that is the
same path AMD needs and costs nothing to keep: an Intel host that wandered past
would produce a usable tree, which is all reopening the decision would take.

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

**H200, 2026-07-26** (Ubuntu 5.15, driver 580.173.02). The first rental produced
complete and correct Phase 1 material, `cgroup2fs` confirming pure v2, and the
NVIDIA empty-fdinfo negative fixture above. It missed everything else: MIG was
disabled, no GPU workload was running, and the host had no containers at all —
so `containers.json` was `[]` and there was no `sys/` tree. That run is why
`check` and `prep` exist. A second pass on the same host, after `prep`, is what
[internal/collector/gpu/testdata/h200-mig-20260726](../internal/collector/gpu/testdata/h200-mig-20260726)
holds.

**H100 80GB, 2026-07-29** (Ubuntu 22.04, driver 580.173.02, CUDA 13.1 toolkit
present). Captured twice on purpose: once in **Default mode** under a real
kernel — that tree is
[h100-default-20260729](../internal/collector/gpu/testdata/h100-default-20260729),
and it closes the "no unpartitioned card" gap the H200 left open — and once
with MIG enabled, to run the NVML hardware test in both modes before the host
was released. Docker and k3s were deliberately *not* installed: Phase 1 and
Phase 2 were already covered by the H200, and this rental was for the GPU.

Two things this rental taught, both now handled by the script:

- The PTX spinner degraded silently to a 0%-utilization load (see
  [NVIDIA](#nvidia) above). The first capture attempt looked complete and would
  have been ambiguous.
- `pkill -f <pattern>` inside an `ssh` one-liner matches the remote shell's own
  command line and kills the session. Use `pkill -x <name>`.

Run `check` before you capture, and read the banner before you destroy the host.

## Dashboard captures — capture-dashboard.sh

The README's hero image is the GPU Tenancy dashboard, because the claim it
makes — one `gpu_uuid` joins to a pod without relabeling — is otherwise
something a reader takes on faith. Re-capturing it means standing up a cluster
that has something worth photographing, and that is most of the work; the
screenshot itself is one command.

```sh
kubectl -n prickle-demo port-forward svc/grafana 3000:3000 &
scripts/capture-dashboard.sh http://localhost:3000 prickle-gpu-tenancy \
  assets/dashboards/gpu-tenancy-nvidia.png
```

The script handles `kiosk` (drops Grafana's nav, keeps the `contains`
textboxes), `theme=dark` on the URL rather than the per-user preference, the
tall-render-then-crop that a viewport-sized headless screenshot needs, and
palette-quantising to about 60 KB. Its header documents the options.

### What has to be true before you press the button

**A GPU node, and containers on it.** The compose quickstart on a laptop
photographs an empty dashboard. What was used for the shipped capture was a
two-node kubeadm cluster: a driverless control plane and one H100 worker.

**Two prickle releases, not one.** On a mixed cluster the stock image reports no
GPU at all and the NVML image cannot start on a driverless node, so a single
release leaves GPU Tenancy empty — see
[packaging/kubernetes-demo/README.md](../packaging/kubernetes-demo/README.md).

**`collectors.podNames.enabled=true`**, or every pod is a bare UID and the
image stops making its argument.

**`collectors.perProcess=true`**, or the two per-process panels are empty.

### The GPU workload is the part that surprises people

`nbody -benchmark`, the obvious stand-in, **cannot produce anything but a flat
line at 100%**. Measured on an H100: 2048 bodies and 524288 bodies both report
100%, because NVML's utilisation is the fraction of time *any* kernel was
resident, not how much of the card is busy — and nbody queues kernels
back-to-back. Gating it from outside does not work either: under `SIGSTOP` the
already-queued work keeps running and the card stays at 100%.

[gpu-load/](gpu-load/) exists for that reason. It holds a *target* utilisation
by leaving the card genuinely idle, alternating at ~120 ms — which has to stay
well under NVML's ~1 s sampling window, or each sample lands inside one busy or
idle phase and the series becomes a square wave between 0 and 100 instead of a
curve. Duty follows a bounded random walk so it drifts like a real tenant.

```sh
kubectl create ns team-alpha; kubectl create ns team-beta
kubectl -n team-alpha create configmap gpu-load-src \
  --from-file=gpu-load.cu=scripts/gpu-load/gpu-load.cu
kubectl apply -f scripts/gpu-load/gpu-tenants.yaml
```

A Job compiles the source onto the node with a CUDA devel image, and two pods
run it from there with different profiles — a training-shaped tenant at 45-92%
and a bursty inference-shaped one at 6-48%. **Apply and wait for the Job before
the pods**: a pod scheduled before the binary exists fails its hostPath mount
and has to be recreated. The `-gencode` line is `sm_90`; change it for another
card.

Two tenants rather than one is deliberate. The second shares the card through
`NVIDIA_VISIBLE_DEVICES` instead of `resources.limits."nvidia.com/gpu"`, so it
does not consume the only allocation — and "one card, two pods, two namespaces"
is the whole point of the dashboard.

#### On AMD

[gpu-load.hip](gpu-load/gpu-load.hip) and
[gpu-tenants-amd.yaml](gpu-load/gpu-tenants-amd.yaml) are the same two tenants
with the same profiles and the same memory figures, so the two vendors' captures
are comparable frame for frame:

```sh
kubectl label node <node> amd.com/gpu.present=true
kubectl create ns team-alpha; kubectl create ns team-beta
kubectl -n team-alpha create configmap gpu-load-src \
  --from-file=gpu-load.hip=scripts/gpu-load/gpu-load.hip
kubectl apply -f scripts/gpu-load/gpu-tenants-amd.yaml
```

**The duty window is 30 ms there, not 120 ms.** amdgpu's `gpu_busy_percent`
averages over a shorter window than NVML does, so the coarser alternation
survives into the reading as noise. Measured on an MI300X against a 45–55%
target, eight scrapes apiece: 120 ms read **34–63%**, 30 ms read **46–53%**.
The mean is right either way — it is the spread that decides whether a captured
curve means anything.

Three smaller differences, each a property of the platform: the build Job runs
`hipcc` from the node's own `/opt/rocm` rather than pulling tens of gigabytes of
devel image; `--offload-arch=gfx942` replaces `-gencode` (`rocminfo | grep gfx`
names another card); and the tenants are **privileged**, because AMD has no
counterpart to `NVIDIA_VISIBLE_DEVICES` and the ROCm device plugin allocates
whole cards, which would stop the second tenant sharing the first's. That is a
throwaway capture cluster, not a pattern to carry anywhere.

prickle itself needs none of it — it reads sysfs and `fdinfo` — but it does need
`collectors.appArmorUnconfined`, on by default since 0.8.0, or the per-process
panels come back **empty with nothing logged**. That failure looks exactly like
a broken exporter and is the first thing to check on AMD.

### Verify the panels before you capture, not after

Run every panel's `expr` as a **`query_range`**, not an instant query. A
one-to-many join failure during a DaemonSet rollout breaks a panel for the
whole dashboard window while an instant query at any quiet moment looks fine,
and a rollout is exactly what just happened. Substitute the `$*_search`
variables with the empty string, and assert both no error and a plausible
series count.

Expect the two MIG panels to be **empty** on an unpartitioned card. That is
correct, not a failed capture. Everything else should have series.

### Framing

Let the workload run past the window you intend to show — `--from now-5m` with
four minutes of history captures the ramp, which reads as a broken exporter.
Leave the `contains` boxes empty: empty means everything, which is the state a
new reader sees. Keep the result under a few hundred KB, and re-run the capture
if the dashboard changes shape — the image is a claim about what the dashboard
looks like today.

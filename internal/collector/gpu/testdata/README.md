# Phase 3 GPU fixtures

Four `nvidia-smi` captures from two rentals, one sysfs capture of a third host
that has a card and no driver, and one sysfs + DRM fdinfo capture of an **AMD**
host — the vendor SPEC.md §Collectors names and no capture had ever covered.
The AMD tree is documented in [its own section](#amd-mi300x-2gpu-20260804); the
NVIDIA trees follow. The first two differ in the one way that matters — whether
the card is partitioned — and the last two are that same second card again,
repartitioned:

| Tree | Host | State | What only it can answer |
|---|---|---|---|
| `h200-mig-20260726/` | H200 141 GB, driver 580.173.02 | **MIG on**, two `1g.18gb` instances, a compute process on one | What a partitioned card reports, and both limitations of the `nvidia-smi` source |
| `h100-default-20260729/` | H100 80 GB, driver 580.173.02 | **MIG off**, a CUDA kernel holding the card at 100% | What an unpartitioned card reports, and that a real utilization reading survives the parser |
| `h100-mig-20260729/` | the same H100, 40 minutes later | **MIG on**, two `1g.10gb` instances | That the mode and not the host is what changes the output, and a profile string the parser has not seen |
| `h100-mig-mixed-20260729/` | the same H100 again | **MIG on**, three instances of two profiles, one of them subdivided; three processes, two sharing a command, one with its binary deleted | A compute-instance profile spelling, profiles staying with their own UUIDs, per-command summing, and a deleted binary |

Both rentals have since been destroyed. Each tree is the NVIDIA half of a
[scripts/capture-fixtures.sh](../../../../scripts/capture-fixtures.sh) run; the
H200 capture is also where the Phase 1 and Phase 2 trees come from. The H100's
`/proc` was captured too and deliberately **not** kept: same Ubuntu image, same
disk names, identical `meminfo` field set and fewer interfaces than the H200
tree, so it would have been a second copy of covered ground rather than
coverage.

Unlike the other two phases, none of these is a mirrored filesystem layout and
`fsroot` does not point at them. `nvidia-smi` is a subprocess, not a file, so
SPEC.md §Collectors puts it behind an interface — the same reason `Statfser`
exists in the host collector — and the tests replay these files through a fake
`commandRunner`. What the layout mirrors is the *capture*, not the host.

| | `h200-mig-20260726` | `h100-default-20260729` | `h100-mig-20260729` |
|---|---|---|---|
| Captured | 2026-07-26T05:42:16Z | 2026-07-29T18:05:46Z | 2026-07-29T18:45:31Z |
| Hardware | 1× NVIDIA H200 141 GB | 1× NVIDIA H100 80 GB HBM3 | the same card |
| MIG | **enabled**, two `1g.18gb` instances | **disabled** (Default mode) | **enabled**, two `1g.10gb` instances |
| Driver | 580.173.02, CUDA 13.0 | 580.173.02, CUDA 13.0 | 580.173.02, CUDA 13.0 |
| Load at capture | context-only spinner, `utilization.gpu` = `[N/A]` under MIG | `nvcc`-built kernel, `utilization.gpu` = `100` | the same kernel pinned to one instance, `[N/A]` under MIG |

`h100-mig-mixed-20260729` is the same card once more, in the state the last
hour of the rental was spent in: a `3g.40gb` GPU instance subdivided into a
single `1c` compute instance, two whole `1g.10gb` instances, and three compute
processes. What each part of it pins is in
[smi_mixed_mig_test.go](../smi_mixed_mig_test.go).

Everything in all four is **captured, unmodified** `nvidia-smi` output.

## The driverless host: `h100-nodriver-20260801/`

The one tree here that *is* a mirrored filesystem layout, and the only one
`fsroot` points at. It holds `sys/bus/pci/devices/*/{vendor,device,class}`
captured on 2026-08-01 from an Ubuntu 26.04 VM with an **H100 SXM5 80 GB on the
bus and no NVIDIA driver installed** — no `libnvidia-ml.so.1`, no `nvidia-smi`,
no `nvidia` kernel module.

That host is not an edge case, it is the common one: a GPU instance before its
driver is provisioned. Both artifacts behave correctly on it — each declines
and says why — but `prickle diagnose` used to follow that with *"On a host with
no NVIDIA GPU this is expected and not an error"*, which is the wrong answer
and the one an operator is least equipped to doubt. `CountNVIDIAGPUs` reads
this tree to tell the two cases apart; see [pci.go](../pci.go).

Fourteen devices were captured, not one, because the negatives carry the
weight:

| Device | Vendor | Class | Why it is in the tree |
|---|---|---|---|
| `0000:00:09.0` | `0x10de` NVIDIA | `0x030200` 3D controller | The H100. Not VGA-class — a datacenter card drives no display |
| `0000:00:02.0` | `0x1af4` Red Hat | `0x030000` VGA controller | The VM console. Display-class and **not** NVIDIA |
| the other twelve | Intel, Red Hat | bridges, storage, network | Ordinary bus traffic to walk past |

Matching on vendor alone or class alone gets a different answer on this tree
than matching on both, which is what makes it a fixture rather than a sample.
The `device` attribute is captured but unread: `0x2330` identifies the H100
specifically, and nothing needs to know that yet.

## What the H200 MIG tree exercises

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

## What the Default-mode tree exercises

The H200 rental was partitioned for its whole life, which left one question it
could not answer: what an unpartitioned card looks like. `TestDefaultModeCardHasNoMIG`
had to hand-write an `nvidia-smi -L` line to ask it, and a hand-written line
proves only that the parser handles what the test author imagined.

- **`query-gpu.csv`** — the same eight columns, with `utilization.gpu` reading
  `100`. That is the converse of the H200's `[N/A]`, and it is the case the MIG
  capture structurally cannot cover: a fixture captured on an *idle* card would
  read `0`, which is indistinguishable from a parser that turned an absent
  `[N/A]` into a zero. The card was held at 100% by a real CUDA kernel for
  exactly this reason, so `TestDefaultModeUtilizationIsANumber` asserts a value
  that is neither absent nor zero.
- **`gpus.txt`** — one GPU line and no indented MIG lines, so `mig_enabled` is
  0 and no instance series exist. The real answer to what the hand-written
  override was standing in for.
- **`mig-gi.txt` / `mig-ci.txt`** — `No MIG-enabled devices found.` on a card
  that is fully MIG-*capable*: `mig-profiles.txt` lists all seven H100 profiles.
  Capability and configuration are different questions, and only `-L` answers
  the second.
- **`query-compute-apps.csv`** — one process, `/tmp/loadgen`, attributed to the
  card itself. Its PID (4559) is parsed and discarded exactly as the H200's is;
  `TestDefaultModeHasNoPID` asserts that per capture, not per fixture.

## What the second MIG tree exercises

`h100-mig-20260729/` is the H100 forty minutes later, partitioned into two
`1g.10gb` instances for the hardware verification run. Its value is not that it
is a second MIG capture — it is that it is the *same card* as the Default-mode
tree, with the same UUID, so anything that differs between the two is the mode
and not the host. `TestSameCardBothModes` is that comparison, and it is the
tightest available check that `mig_enabled` and the instance families follow
the card's configuration rather than something incidental to a machine.

- A **profile string the parser had not seen**: `1g.10gb`, against the H200's
  `1g.18gb`. `TestSecondMIGProfileIsNotTheFirstCapturesProfile` also asserts
  the H200's spelling does not appear, which is what would happen if the
  profile were ever settled on rather than read.
- **Both `nvidia-smi` limitations, on a second card and a second driver
  install**: `utilization.gpu` is `[N/A]` again, and the process pinned to MIG
  device 0 is again reported against the parent GPU's UUID. They are properties
  of the tool, not of one rental.
- The **numbers quoted in [Hardware verification](#hardware-verification)** are
  read off this tree, so those claims can be rechecked. That matters: doing
  exactly that is what caught the ordering claim this file originally made.

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

## AMD: `mi300x-2gpu-20260804/`

The first AMD capture, and the first multi-GPU capture of any vendor. **2×
AMD Instinct MI300X**, 192 GB each, on Ubuntu 24.04 / kernel 6.8.0-124, taken
2026-08-04T03:13:21Z with both cards under a HIP kernel — `gpu_busy_percent`
reads 100 and 98, not zero.

Like `h100-nodriver-20260801` and unlike the four `nvidia-smi` trees, this
**is** a mirrored filesystem layout and `fsroot` points straight at it. AMD is
read from sysfs and `/proc/<pid>/fdinfo`, which are files, so nothing here
needs a subprocess or an interface to stand in for one.

| Part | What only it can answer |
|---|---|
| `sys/class/drm/card{0,8}/device/` | What an AMD card publishes: `gpu_busy_percent`, `mem_busy_percent`, the three memory pools (`vram`, `vis_vram`, `gtt`), and the identity below |
| `.../device/hwmon/hwmon{0,1}/` | Power and temperature, and that the sensors are **self-describing** rather than fixed-index — see below |
| `sys/bus/pci/devices/` | 30 devices, of which the two GPUs are the only `0x1002`. **No device on this host has a `0x03xx` display class at all** |
| `proc/{8038,9163}/` | Two processes on two cards each: one on the host, one in a Docker container. `fdinfo/{7,8}`, `cgroup`, `comm`, `exe.link` |
| `amd/drm-map.txt` | The card ↔ PCI-address ↔ render-node map that ties fdinfo to a card |
| `amd/{rocm-smi-showall,amd-smi-static,amd-smi-metric}.txt` | Reference output to check the sysfs reading against. **Not a source** — SPEC.md §Hard constraints #2 permits spawning `nvidia-smi` and nothing else, so `rocm-smi` is never run by the exporter |

Four things in it are load-bearing for a collector that does not exist yet, and
each contradicts an assumption that was reasonable before the capture:

- **`unique_id` is the identity, and it is the only one.** It matches amd-smi's
  `ASIC_SERIAL` exactly (`594afe08e1ab3ae6` ↔ `0x594AFE08E1AB3AE6`) and is
  readable from sysfs with no vendor tool. Nothing else in the tree is stable
  and per-card: the two GPUs share a `device`, a `subsystem_device`, and a
  `vbios_version`, and `card0`/`card8` are kernel enumeration order. This is
  what `gpu_uuid` must come from.
- **DRM fdinfo names a GPU by `drm-pdev` and by nothing else.** There is no
  UUID in fdinfo, so per-process attribution is a join through the PCI address
  — `0000:fd:00.0` → `card0` → `unique_id`. `card<N>/device` is a symlink whose
  basename is that address, and a captured tree flattens symlinks, which is why
  `amd/drm-map.txt` exists at all. Without it the fdinfo files are unusable.
- **hwmon sensors are labelled, not numbered.** This card has no `temp1_input`
  and no `power1_average` — it publishes `temp2_input` (`junction`),
  `temp3_input` (`mem`) and `power1_input` (`PPT`), each named by a sibling
  `*_label` file. The capture script asked for the first two by name and got
  neither; it now takes every regular file in the directory. A collector must
  read the labels rather than index the sensors.
- **The PCI class is `0x120000`, "processing accelerator", not a display
  class.** `pci.go`'s `displayBaseClass = "0x03"` is correct for NVIDIA — an
  H100 reports `0x030200` — and would find zero GPUs here. An AMD presence
  check needs its own class test, not a reuse of that one.

### What this capture does not cover

- **A second partition mode.** These cards are `SPX` / `NPS1`, and AMD's
  compute partitioning (`CPX`, `DPX`, …) is the analogue of MIG — the one
  variation the NVIDIA fixtures deliberately capture twice, on the grounds that
  "the mode and not the host is what changes the output". It could not be done
  here: `amd-smi` reports these as **MI300X VF**, SR-IOV virtual functions, and
  `current_compute_partition` is mode `0444` in the guest. Partitioning is the
  hypervisor's to set. A bare-metal MI300X would make this capturable.
- **Per-process utilization.** The `amdgpu` driver on this kernel emits no
  `drm-engine-*` keys, only memory ones, so fdinfo can say how much VRAM a
  process holds and not how busy it kept the card.
- **A consumer Radeon.** Everything above is an OAM datacenter module
  (`board_info: type : oam`). A desktop card is likely to differ in exactly the
  hwmon indices this capture warns about.

## Coverage gaps

| Gap | Status |
|---|---|
| ~~**AMD — sysfs + DRM fdinfo**~~ | **Closed.** Captured *and* implemented, on the same day and against the same host. [amd.go](../amd.go) is developed against this tree and [amd_test.go](../amd_test.go) pins it, including a golden file. It was then run on the capture host itself and the two agree series for series — see [Hardware verification](#hardware-verification). |
| **Intel — DRM fdinfo** | **Out of scope** as of SPEC.md §Collectors: no capture host is obtainable, and a parser developed against a layout nobody has captured is forbidden by §Testing rules. Reopening it costs a capture, not a redesign — Intel rides the same DRM fdinfo path AMD needs. |
| NVML — the whole path | Still not fixture-testable: a C call, not a file read (SPEC.md §Testing rules). **No longer unverified** — see [Hardware verification](#hardware-verification) below. Unit tests drive the shared emission code through a fake source; `nvml_hardware_test.go` re-checks the real one wherever a GPU is present. |
| ~~A card in Default mode (MIG off)~~ | **Closed** by `h100-default-20260729`. The hand-written `-L` override in `TestDefaultModeCardHasNoMIG` is kept: it is now the *unit* of that behaviour, with the capture as the integration case. |
| A multi-GPU host | **Half closed.** `mi300x-2gpu-20260804` is two cards, with one process holding memory on both at once — but it is AMD, so it proves nothing about the NVIDIA parsers. Those still key on UUID rather than position specifically so a second card cannot silently attach its partitions to the first, and no NVIDIA capture demonstrates it. |
| `[Not Supported]` / `[Unknown Error]` tokens | Only `[N/A]` appears in the capture. The others are handled by the same bracket-shape rule and covered in `TestBracketedTokensAreAbsentNotErrors`. |

The AMD gap is closed in both halves: captured, and written against the
capture. Intel remains where it was — the same DRM fdinfo path, still with no
host, and now with a working reader of that format sitting next to it.

What replaces it are two narrower rows above, and they are worth reading as a
pair. Neither is a design question; both are one capture away, and both are
about a *combination* no single machine has offered yet — a bare-metal AMD card
that can actually be repartitioned, and a host holding cards from both vendors
at once.

## Hardware verification

### AMD, 2026-08-04

The AMD collector was run on the host its fixtures came from, while the fixtures
were still true of it — the static `CGO_ENABLED=0` build, on the 2× MI300X,
with the same HIP kernel still loading both cards.

`prickle diagnose` reported both cards, and a full scrape produced the same
series as the golden file, including the four per-process values to the byte:
`1225003008` and `1224998912` for the host process, `688119808` and `688123904`
for the containerised one. That is the strongest check available to this
collector and it is weaker than it sounds — the fixture and the live host are
the same machine minutes apart, so it proves the reader agrees with sysfs, not
that sysfs looks like this anywhere else. The `nvidia_hardware_test.go`
equivalent, two independent implementations agreeing, has no AMD analogue:
there is only one way to read these files.

**Running the suite itself on the host found a defect no CI run could.** The
NVIDIA fixture tests built their `Options` without `Roots`, and a zero
`fsroot.Roots` resolves to the live `/sys` and `/proc` — which mattered not at
all while the only collector was NVIDIA and its source was a fake
`commandRunner`, and started mattering the moment a collector read sysfs
directly. On this host the NVIDIA tests collected the two real MI300X: the
cards joined the golden comparison, the real GPU processes joined the
per-command summing, and `TestSameCommandIsOneSummedSeries` counted four series
for a fixture describing two. Six tests failed on hardware and none had ever
failed anywhere else, because no CI runner has a GPU and no developer machine
here has an AMD one. They now take `hermeticRoots(t)`, an empty tree, so they
describe their fixture whatever is plugged into the machine underneath.

One thing it did establish that no fixture could. **Per-process attribution is
silently partial when the exporter cannot read other users' `fdinfo`.** Run as
an ordinary user, the scrape showed only the host process and omitted the
containerised one entirely — no error, no gap, just two series where there
should be four. Run under `sudo`, all four appeared. On a Kubernetes node the
processes that go missing are exactly the ones in containers, which is the ones
worth measuring.

### NVIDIA

SPEC.md §Testing rules requires that the two NVIDIA sources emit identical
output for the same GPU, and that **a hardware test asserts it**. That test is
[../nvml_hardware_test.go](../nvml_hardware_test.go). It skips wherever NVML
does not load, and it was run on the same H100 the Default-mode tree was
captured from — driver 580.173.02, in **both** Default and MIG mode, on
2026-07-29, with the MIG state created for the run and torn down afterwards.

**Re-run on a third card and a new OS, 2026-08-01.** An H100 80GB on **Ubuntu
26.04, kernel 7.0.0**, driver 580.173.02, acting as a Kubernetes worker.
`TestSourcesAgreeOnHardware`, `TestNVMLSourceSurvivesAnEarlierClose` and
`TestNVMLReadsNoReservedMemory` all pass, and the full suite passes under
`-race` on both builds — the first time any of this has run on a 7.x kernel or
on 26.04. The two sources' output differed in exactly one series,
`prickle_gpu_nvidia_source_info{source=…}`, which is the one that is supposed
to differ.

That run is also the first time the **GPU and container collectors have
sampled the same host in one pass**: 452 host series, 157 container series from
five CRI-O containers, and 8 GPU series through NVML. Nothing in the fixtures
can express that, because each fixture tree is one collector's world.

It is not a substitute for these fixtures. They pin the parse of a captured
format; it pins the agreement of two live implementations, which is the one
thing a fixture cannot express. It found three disagreements on its first run:

| Disagreement | Why no fixture could have caught it |
|---|---|
| NVML's `used` memory included the 480 MiB the driver reserves; `nvidia-smi` reports the `_v2` number that excludes it | Both numbers are internally consistent. Only reading the same card through both APIs in the same second shows the gap. |
| NVML spelled a partition `10gb`, `nvidia-smi -L` spells it `1g.10gb` | NVML publishes no entry point returning that string; the slice count comes from `nvmlDeviceGetAttributes_v2` and had to be assembled. |
| A second GPU collector in one process was handed an already-closed NVML handle | The library handle is process-global. `prickle diagnose` builds a collector, closes it, then builds the real one — a sequence no fixture test performs. |
| The MIG `profile` label was *derived* rather than read, and three ways wrong | Only a second card class shows it. See below. |

### The profile label, which took four hardware rounds

`prickle_gpu_mig_info` carries the profile a MIG instance was cut from, and the
`nvidia-smi` source reads it straight out of `nvidia-smi -L`. NVML has no
equivalent one-call answer, so this source derived it — and every derivation
was wrong in a way only hardware could show:

1. **Memory alone** gave `10gb` where `-L` says `1g.10gb`.
2. **Memory plus the GPU-instance slice count** gave `1g.10gb` and `3g.40gb`
   correctly on an H100 — and is wrong for **every profile an H200 offers**.
   That was first inferred from `h200-mig-20260726/smi.txt`, which shows a
   16.00 GiB framebuffer against a profile called `1g.18gb`, and then measured
   on a live H200 (driver 580.173.02, 2026-07-30) by building with the name
   lookup disabled and scraping the card:

   | `-L` says | the derivation gave | framebuffer |
   |---|---|---|
   | `1g.18gb` | `1g.16gb` | 16.00 GiB |
   | `1g.35gb` | `1g.33gb` | 32.50 GiB |
   | `3g.71gb` | `3g.70gb` | 69.75 GiB |

   NVIDIA names a profile after a share of the card's *advertised* 141 GB,
   which NVML never reports. The name is not a function of the memory on any
   card; the H100 simply hid that by coincidence.
3. **The GPU instance's own profile name**, fetched from the driver, gave
   `1g.10gb+me` for an instance created from the media-engine profile, where
   `-L` says plain `1g.10gb`. `-L` names the *compute* instance, not the GPU
   instance.
4. **The compute instance's profile name** matches `-L` in every configuration
   tested — because it is the same string nvidia-smi prints. Verified on an
   H100 (plain, media-engine, `1c`- and `2c`-subdivided) and on an H200
   (`1g.18gb`, `1g.35gb`, `3g.71gb` together on one card, and a single
   `7g.141gb` instance spanning the whole GPU).

Two traps on the way there, both worth knowing before touching that code:

- An instance's `profileId` is the driver's device-unique id (`9` for
  `3g.40gb`, the number `mig -lgip` prints). The profile lookup's parameter is
  an unrelated enum in which `9` means `1_SLICE_REV2`. Passing one as the other
  returns a real profile with a plausible name for the *wrong* partition —
  `1g.20gb` for a `3g.40gb` instance. The id is matched, never indexed.
- The compute-instance name is already complete: `1c.3g.40gb`, not `3g.40gb`
  wanting a prefix. Adding the slice counts on top produced `1c.1c.3g.40gb`.

The whole suite was re-run on a **second card class** — an H200, driver
580.173.02, 2026-07-30 — which is what confirms the declared C struct layouts
are not H100-shaped by accident. It also produced the sharpest demonstration
of why MIG memory is a separate family: a single `7g.141gb` instance spanning
the whole card reports `prickle_gpu_mig_memory_used_bytes` and
`prickle_gpu_memory_used_bytes` as *the same 4846780416 bytes*. One family
holding both would have doubled a card's usage on a `sum()`, at 100% overlap.

Both sources' MIG numbers were cross-checked against `nvidia-smi`'s own
human-readable table on both hosts, and `h100-mig-20260729/` is one such
capture:
per-instance `4210MiB / 9984MiB` and `15MiB / 9984MiB` match NVML's byte values
exactly, and the instance whose `-L` device index is 0 is the one the table
attributes the process to.

**What that capture says about the GI/CI ordering question is the opposite of
what it first appeared to say, and it is worth stating plainly.** The two
listings are in the *same* order there: `mig -lgi` reports GPU instance 11 then
13, and `-L` device 0 then 1 map to exactly those. Pairing them positionally
would have worked on this host. What ran in a different order was the
*creation* — instance 13 was created first — so the listing order is not
creation order but placement order (`Start:Size` 4:1 before 6:1), an emergent
property of the driver that NVIDIA documents nowhere.

So this capture is not evidence *against* positional pairing. It is the more
uncomfortable thing: a sample of one where the guess holds, which is how a
pairing that is right by coincidence gets committed. The decision above stands
on its original ground — nothing captured joins a MIG UUID to a GI ID — and
this tree is the reason that ground is stated rather than the ordering.

What the two sources still differ in, by design and asserted as such:

- `prickle_gpu_nvidia_source_info` — names which implementation ran.
- The MIG-only families — NVML's reason to exist. On a partitioned card the
  test *requires* them from NVML and *forbids* them from `nvidia-smi`.
- Per-process series for a process whose `exe` symlink NVML could not read.
  NVML gets the name from `/proc`, `nvidia-smi` from the driver, so NVML's set
  must be a subset — never a superset.

## The DRM fdinfo negative

Both full captures contain `/proc/<pid>/fdinfo/<fd>` for every process holding
an NVIDIA device, and **none of them carry `drm-*` keys** — only `pos`,
`flags`, `mnt_id` and `ino`. Two hosts, two cards, same answer. That is a
wanted *negative* result, not a failed capture:
it is the evidence that per-process GPU attribution on the NVIDIA proprietary
driver must come from NVML or `nvidia-smi` and can never come from DRM fdinfo.
Those files are not copied here because nothing in this package reads them; the
finding is recorded in [scripts/README.md](../../../../scripts/README.md)
§Platform notes.

## Golden output

`golden/gpu.prom`, `golden/gpu-default-mode.prom`, `golden/gpu-h100-mig.prom`
and `golden/gpu-h100-mig-mixed.prom` are what the four trees render to through the `nvidia-smi` source, with
`node="fixture"` and per-process attribution on. One golden per tree, because
the captures differ in what the driver will answer, not merely in their
numbers. All are checked byte-for-byte and verified with `promtool check
metrics`. Regenerate after an intentional change with:

```sh
go test ./internal/collector/gpu/ -update-golden
```

Read the diff before committing it — the golden is the review surface for every
metric name, label and unit in Phase 3.

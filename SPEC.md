# prickle-exporter — SPEC

**Read this file in full before generating or modifying any code.** It is the
contract that keeps generation sessions consistent. Decisions recorded here are
frozen; do not reopen them mid-session. If a session genuinely needs to change
one, the change happens in this file first, in its own commit.

## Identity

| Item | Value |
|---|---|
| Organisation | Starkdrift |
| Repository / Go module | `github.com/starkdrift/prickle-exporter` |
| Binary and CLI command | `prickle` |
| Diagnostic subcommand | `prickle diagnose` |
| Metric prefix | `prickle_` — always the full word |
| Listen address | `:10047` — **registered** on the Prometheus default-port wiki against this repository; the repo is public, so the number is now a public claim and cannot drift |
| License | Apache-2.0 — `LICENSE` file at repo root, SPDX header `Apache-2.0` in every source file |

The names above are the only ones used anywhere in the tree — code, comments,
docs, fixtures, chart names, dashboard JSON. Earlier candidate names were
discarded during naming and must not reappear.

Two checks enforce this, because a deny-list alone could not. The names
discarded during the naming pass were never written down anywhere in the
repository, so `ci/denied-names.txt` has been empty since it was created and
the gate reported itself vacuous on every run — reassurance standing where a
check should be. The enforcing check is therefore **positive**: the only
exporter-shaped identifiers permitted in the tree are `prickle-exporter` and
the third-party exporters the documentation legitimately names, and anything
else fails CI whether or not anyone remembers rejecting it. The deny-list
remains and still runs, for names somebody does supply; the grep excludes
itself, the list, and this file. Never abbreviate the metric prefix.

## Hard constraints

1. **Go standard library only.** Zero third-party dependencies, including
   `prometheus/client_golang`. Exposition is hand-written (`internal/exposition`).
2. **Strictly read-only.** The exporter never writes to `/proc`, `/sys`, or
   cgroups and takes no corrective actions — those belong to the separate
   closed agent. Only permitted external interactions: read-only filesystem
   access, GET-only requests on the Docker socket (optional), `dlopen` of
   `libnvidia-ml.so.1` with query-only NVML calls, and spawning `nvidia-smi`
   as a fallback subprocess. No NVML call that mutates device state (MIG
   configuration, clocks, persistence mode, ECC) may ever be used.
3. **Configurable filesystem roots.** All access to `/proc`, `/sys`,
   `/sys/fs/cgroup` and `/var/log/pods` goes through `internal/fsroot` prefixes
   so tests can point at fixture trees. No collector may hardcode an absolute
   path.
4. **cgroup v2 is the primary hierarchy; v1 and hybrid are supported.**
   Reversed on 2026-08-01, after `prickle diagnose` was run on a real v1 host
   (Rocky Linux 8.10, kernel 4.18) and correctly reported that it would produce
   nothing. Correctly, and uselessly: RHEL 8 defaults to v1 and is supported
   into 2029, so "out of scope" meant an ordinary enterprise host got an empty
   scrape. The detection was working as designed; the design was the problem.

   v1 is a different data model, not a spelling variant — one hierarchy per
   controller (`/sys/fs/cgroup/memory/…`, `/sys/fs/cgroup/cpu,cpuacct/…`),
   different file names, and different units. It therefore gets its own reader
   behind the same interface, never a set of `if v1` branches in the v2 path.

   **The metrics contract does not fork.** A container reports the same metric
   names with the same units and the same label set on either hierarchy; the
   hierarchy decides where a value is read from, never what it is called.
   Where v1 genuinely cannot answer — it has no PSI, so there is no
   `prickle_container_pressure_stalled_seconds_total` — the family is **absent**
   rather than zero, for the same reason `utilization_ratio` vanishes under MIG.
   `prickle diagnose` states which hierarchy is live and what it costs.

## Metrics contract

- Identity labels on hot series, and only these: `node`, `namespace`, `pod`,
  `container`, `gpu_uuid`, `mig_uuid`.
- That closed set governs **identity** — which entity a sample belongs to. It
  does not forbid *dimensional* labels that partition a single metric across
  the parts of one entity: `mode` on CPU time, `cpu` on per-core series,
  `device` on disks, `interface` on links, `mountpoint` on filesystems,
  `resource`/`kind` on PSI. A dimensional label is one whose values are
  enumerable from the metric's own source and which is meaningless without the
  metric; it never names an entity that another metric could also be about. New
  dimensional labels are added deliberately, not by reflex — each one multiplies
  series count.
- Descriptive attributes (names, models, versions, images) live on companion
  `_info` gauges, joined in queries via `group_left`. Never put them on hot series.
  This applies to dimensional labels too: where a hot series has a natural key
  (`mountpoint`), the human-readable extras (`device`, `fstype`) belong on the
  `_info` gauge.
- Metric names are `snake_case` throughout — `promtool check metrics` lints
  camelCase as an error, so kernel field names (`MemTotal`, `Active(anon)`) are
  converted, not passed through.
- Per-process GPU attribution is **opt-in** via a `command` label sourced from
  the `exe` symlink basename — never `comm` (truncated, forgeable).
- **PID never appears** as a label or metric value anywhere.
- Output is the Prometheus text format with correct `# HELP` / `# TYPE` lines
  and must pass `promtool check metrics` at all times.
- Cardinality caps per collector (Phase 4): on breach, drop and count via
  self-instrumentation; never OOM the scrape.
- **Metric selection.** `-metrics.preset` chooses how much is exposed:
  `minimal` (the default), `full`, or `custom` with `-metrics.include` taking
  regexes. A host emits 156 families with everything on and the shipped
  dashboards use 35 of them, so the default withholds roughly three quarters of
  what the collectors can produce — a decision about what most deployments
  should pay for, not about what the exporter can measure.

  Three rules keep it from becoming a way to lose data quietly:

  1. **The minimal set is an explicit list in code, not "whatever the
     dashboards happen to reference".** Deriving it from the dashboards would
     let an edit to a panel silently change what every scrape returns. CI
     asserts the dashboards are a **subset** of the minimal set, so the two
     cannot drift apart without failing the build.
  2. **Self-metrics are never filtered.** `prickle_collector_*`,
     `prickle_build_info` and `prickle_render_timestamp_seconds` survive every
     preset, for the same reason the cardinality cap does not apply to them: a
     scrape that has been reduced must still be able to say so.
  3. **`prickle diagnose` reports the active preset and how many families it is
     withholding.** "My metric disappeared" has to have an answer that is not
     reading the source.

  Filtering happens at family creation, so a withheld family costs no
  allocation and no render, and `prickle_collector_series` counts what is
  actually served.

## Architecture

One binary. `internal/collector` defines the `Collector` interface; a sampler
goroutine polls collectors on an interval, renders into a buffer, and swaps it
under a mutex; `net/http` serves the last completed render so slow collectors
can never stall a scrape. Per-collector timeouts and self-metrics
(`prickle_collector_duration_seconds`, `prickle_collector_errors_total`) are
mandatory from Phase 4.

## Collectors

- **Host (Phase 1):** `/proc/stat`, `meminfo`, `diskstats`, `net/dev`,
  `loadavg`, `pressure/{cpu,memory,io}`, `mounts` + `Statfs` (behind an
  interface — not fixture-able as a file). Aggregate CPU time is always
  exposed; per-core series are opt-in behind `-collector.cpu.per-core`, as a
  separate family, so default cardinality does not scale with core count on
  large GPU nodes. `/proc/loadavg`'s fourth and fifth fields are not exposed:
  the fifth is a PID.
- **Containers (Phase 2):** walk the cgroup v2 tree; identity extracted from
  directory names. The kubelet's and Docker's **cgroup driver** decides the
  shape of the whole tree, and both drivers are in scope:
  - *systemd driver* — `docker-<hex>.scope`, `cri-containerd-<hex>.scope`,
    `nerdctl-<hex>.scope`, `crio-<hex>.scope`, `libpod-<hex>.scope`, under
    `kubepods.slice/.../kubepods-…pod<uid>.slice` with the UID
    systemd-escaped. Guaranteed pods carry no QoS component. Podman's scopes
    sit under `machine.slice` rather than a pod slice, and it pairs each
    container with a `libpod-conmon-<hex>.scope` monitor that is **not** a
    container; the hex-ID test excludes it without a special case.
    `cri-containerd-` and `nerdctl-` are the same runtime reached two ways —
    through the CRI and through nerdctl — and both report
    `runtime="containerd"`, because the label names the runtime and not the
    client that spoke to it.
  - *cgroupfs driver* — `docker/<hex>` and `kubepods/<qos>/pod<uid>/<hex>`,
    with no suffix, no runtime prefix, and the UID unescaped. This is the
    default on managed Kubernetes, so treating it as exotic means an ordinary
    node reports nothing.

  A bare hex directory is a container only because of its parent; the parent is
  what distinguishes one from any other hex-named cgroup. Where the layout does
  not name the runtime — which is every cgroupfs kubepods tree — `runtime` on
  the `_info` gauge is **empty** rather than inferred. Docker socket is an
  optional enrichment path for human-readable names only.

  **Pod names are opt-in, behind `-collector.container.pod-names`, and cost the
  exporter's unprivileged default.** The cgroup tree carries a pod's UID and
  never its name, but the kubelet does: it creates
  `/var/log/pods/<namespace>_<pod>_<uid>/<container>/` on every CRI runtime, so
  one directory listing maps the UID the cgroup walk already has to a namespace
  and a name. No API call, no second exporter, no gRPC — which matters, because
  the CRI socket would need protobuf and §Hard constraints #1 forbids the
  dependency.

  The price is that `/var/log/pods` is `root:root` `0750` and so are the two
  alternatives (`/var/lib/kubelet/pods`, containerd's per-container state
  directory). **What satisfies that mode differs by how the exporter is run,
  and the two costs are not equal** (measured 2026-08-04; this paragraph
  previously stated the capability was the mechanism in both):

  - **Under systemd**, `AmbientCapabilities=CAP_DAC_READ_SEARCH` works — the
    ambient set makes the capability real for a non-root `DynamicUser`. It is
    also the expensive route: for a process whose only capability is reading
    files, "read any file" is not meaningfully smaller than root, and it is this
    program's entire risk surface.
  - **On Kubernetes**, that capability is unusable and must not be relied on: a
    capability added to a non-root uid lands in the bounding set alone, leaving
    the permitted, effective and ambient sets empty. The chart instead runs as
    uid 65532 with **`runAsGroup: 0`** and reads the directory through its group
    bits, which reaches files owned by group root and nothing further.

  Either way it is **off by default** and enabling it is a deliberate trade an
  operator makes. Both routes assume the `0750`-with-group-root mode these hosts
  ship; where `/var/log/pods` is `0700`, only uid 0 can read it.

  When enabled: `namespace` joins the hot series, which the closed identity set
  already permits and which is what makes filtering by namespace work at all;
  and `pod_name` joins the `_info` gauge as a descriptive attribute. `pod`
  continues to hold the **UID**, unchanged, so existing joins keep working —
  renaming what that label means would break every rule built on it.
- **GPU (Phase 3):** AMD via sysfs + DRM fdinfo, captured and implemented
  2026-08-04 against 2× MI300X.

  Identity is `unique_id`, which is the only stable per-card value sysfs
  publishes and which matches amd-smi's `ASIC_SERIAL`; where a generation lacks
  it the PCI address stands in. A card is recognised by its uevent
  `DRIVER=amdgpu` and **not** by PCI class: an MI300X reports `0x120000`, a
  processing accelerator, so the display-class test the NVIDIA presence check
  uses would find nothing. hwmon sensors are located by their `*_label` files
  rather than by index, because the indices differ per card — an MI300X has no
  `temp1_input` at all.

  Per-process attribution reads `drm-total-vram` from DRM fdinfo. fdinfo names
  a GPU by `drm-pdev`, its PCI address, and carries no UUID, so processes are
  joined back to cards through that address.

  **`rocm-smi` and `amd-smi` are not sources and are never spawned.** §Hard
  constraints #2 permits exactly one subprocess and it is `nvidia-smi`. The
  cost is that AMD sysfs publishes no marketing name, so `prickle_gpu_info`
  carries a small capture-verified lookup keyed on the PCI ID and falls back to
  the ID itself. The two vendors' output does not fork: same families, same
  units, same labels. MIG is NVIDIA's and is **absent** on an AMD card rather
  than reported as 0 — the rule §Hard constraints #4 applies to PSI on cgroup
  v1 — while AMD's own partitioning gets `prickle_gpu_amd_partition_info`.

  **Intel is out of scope.** No
  capture host is obtainable, and §Testing rules forbids developing a parser
  against a layout nobody has captured — so listing it would be scope on paper
  and an empty scrape in practice, which is the worse of the two failures. This
  is a decision about what ships, not a door welded shut: Intel is read through
  the same DRM fdinfo path AMD needs anyway, so a capture is all that stands
  between this line and reopening it.
  NVIDIA is served by two interchangeable implementations behind one
  `nvidiaSource` interface, selected at runtime in this order:
  1. **NVML via `dlopen` of `libnvidia-ml.so.1` — the preferred path.** Richer
     data, no per-scrape process spawn, no CSV parsing, and the only reliable
     source of MIG topology. Requires cgo (see build note below).
  2. **`nvidia-smi` CSV subprocess — fallback only.** Used when the NVML build
     is unavailable or the library cannot be loaded at runtime (common in slim
     containers that lack the driver libraries). Kept working and tested; it is
     not deprecated. Verified limitations (H200, driver 580):
     `--query-compute-apps` reports the parent GPU UUID for MIG-resident
     processes, so per-process MIG attribution is unavailable in this source
     (processes are attributed to the physical GPU); and `utilization.gpu`
     returns `[N/A]` when MIG is enabled — the CSV parser must treat bracketed
     `[N/A]` tokens as absent values, never as errors.

  Selection is automatic: attempt the NVML load once at startup, fall back
  silently, and record the active source on an `_info` gauge.
  `prickle diagnose` states which source is live and, when NVML failed to load,
  why. `-collector.gpu.nvidia-source={auto,nvml,smi}` forces one path for
  debugging — this file called it `--nvidia-source` until 2026-08-06, a flag
  that has never existed, which is worth a note because §Versioning makes flag
  names one of the two surfaces SemVer governs here.
  Consumer and MIG-partitioned datacenter cards are both supported; MIG
  instances carry `mig_uuid`.

  **Build note.** `dlopen` requires cgo *and* a dynamically linked binary — a
  fully static binary cannot `dlopen` at all. NVML therefore lives behind
  `//go:build nvml` and produces a second, dynamically linked artifact. The
  default `CGO_ENABLED=0` build remains pure-Go and static, and uses the
  `nvidia-smi` path. Ship both (see Distribution); neither build links against
  NVIDIA libraries at compile time, so the zero-dependency rule holds in both.

## Testing rules

Every parser is developed against captured fixture trees under `testdata/`,
laid out mirroring real paths so `fsroot` points directly at them. Fixtures
come from `capture-fixtures.sh`; synthetic (hand-built) fixtures are permitted
only where hardware access is pending and must be marked synthetic in a README
beside them. **Never invent file formats or path shapes** — if no fixture
exists for a case, stop and request a capture. Exposition output is verified
against golden files.

NVML is the one path that cannot be fixture-tested: it is a C library call, not
a file read. Both NVIDIA implementations sit behind the `nvidiaSource`
interface, so unit tests exercise a fake source, and the NVML implementation
itself is verified only on real hardware. Its captured `nvidia-smi` fixtures
remain the reference for what the NVML path must report — the two sources must
produce identical metric output for the same GPU, and a hardware test asserts
that.

## Distribution (Phase 5)

Two release artifacts per platform: `prickle` (static, `CGO_ENABLED=0`,
nvidia-smi path) and `prickle-nvml` (dynamically linked, `-tags nvml`,
preferred on NVIDIA hosts).

**Container images are published to a registry alongside the tarballs**, and
the Helm chart pulls from there. Added 2026-08-01: the chart already defaulted
to a registry reference, but nothing published one, so a default `helm install`
could only ever have failed. It also makes the exporter installable in an
**air-gapped environment**, where a registry is often the single channel
available and building from source is not an option — so the images must be
multi-architecture, digest-addressable, and mirrorable with a plain
`skopeo copy` or `crane copy`. Both ship with a systemd unit using a strict
sandbox (`ProtectSystem=strict`, read-only binds, no capabilities beyond what
reading requires) — the NVML unit additionally needs read access to the driver
library path and `/dev/nvidiactl`. Docker one-liner; Helm chart
`prickle-exporter` with ServiceMonitor; `docker compose`
quickstart bundling prickle, Prometheus, and pre-provisioned Grafana. Four
dashboards ship with it — GPU Tenancy, Node Overview, Container Resources,
Fleet Health — wrapping input as `.*<input>.*` for contains-search, filtering
on `command` (never PID). Dashboards carry Starkdrift branding.

All four use **contains-search textbox** template variables per identity label
(amended 2026-08-03; they were paired textbox + chained dropdowns before). A
dashboard's only controls are the datasource and one `<label> contains` textbox
per identity label, and the typed string goes straight into the panels' PromQL.
Filtering is one action rather than two, and there is no second control holding
a stale selection.

Two consequences are accepted rather than worked around. Input is a **regex, not
a literal**: it reaches PromQL unescaped, so an unbalanced `[` makes the panel
error rather than match nothing — escaping it would also take `web-0[12]` away
from the operator who typed that deliberately. And **the values are no longer
enumerated**, so an operator who does not already know a fragment of a pod UID
has nothing to browse; the panel legends are where values are discovered now.

## Versioning

Every package is under `internal/`, so there is no importable Go API and no Go
API compatibility obligation. SemVer here governs the only two public surfaces
the project has: the **metrics contract** (names, label sets, types, units) and
the **command line** (flag names and their meanings).

| Bump | Trigger |
|---|---|
| major | A metric is renamed or removed; a label key is added to, removed from, or changed in meaning on an existing series; a flag is removed; a default changes such that output changes |
| minor | A new metric family, collector, flag, or label *value* |
| patch | A wrong value corrected, a parser fix, performance, documentation |

Adding a label *key* to an existing hot series is a major, not a minor: it
breaks every recording rule and dashboard that aggregates without `by`.

Releases are tagged `vX.Y.Z`. **Git tags are the sole source of truth for the
version** — there is no VERSION file and no version constant to forget to bump.
The build injects it with `-ldflags "-X main.version=..."`, and it is exposed to
operators on `prickle_build_info`.

Pre-1.0 the minor *tracked* the roadmap phase, so the version stated what was
implemented: `0.1.0` host, `0.2.0` containers, `0.3.0` GPU, `0.4.0` caps and
timeouts, `0.5.0` distribution. **All five phases had shipped by 2026-08-01,
and no minor since has corresponded to a phase** — `0.6.0` added
`-metrics.preset`, `0.7.0` pod-name resolution and the Kubernetes demo, `0.8.0`
the AMD collector. The roadmap ran out before the work did. From `0.6.0` on a
minor means exactly what the table above says and nothing more, which is the
rule the project has in fact been following; the phase numbers survive as
labels on the original design order, not as a version scheme. Until 1.0.0 a
minor may still break the contract.

`0.8.0` is also why a phase number is a poor progress report. AMD has been
assigned to Phase 3 since this file first listed it, and it shipped three
minors *after* Phase 5 — it was waiting on a capture host, not on the phases in
front of it. A phase records the order the work was designed in. It has never
reliably recorded the order the work lands in.

**`1.0.0` means the metrics contract is frozen, and it is no longer tied to a
phase.** It previously read "it is Phase 5", which would have made the freeze a
consequence of finishing a roadmap row rather than a judgement that the
contract is ready — the opposite of the "deliberate promise rather than
something to drift into" the same sentence claimed. Decoupled on 2026-08-01,
with Phase 5 shipping as `0.5.0`.

The freeze happens when the contract has stopped moving, and it has not: the
2026-08-01 session alone brought cgroup v1 into scope, added podman and
standalone containerd, and left `runtime` empty on layouts that do not encode
one.

**Two of the gaps that argued against freezing closed on 2026-08-04**: AMD is
captured and implemented, and a multi-GPU host has now been read — the same
one, 2× MI300X. That is progress toward a freeze rather than an argument for
it, because closing them moved the contract again: `prickle_gpu_info` gained a
`vendor` label and `prickle_gpu_amd_partition_info` is a new family. AMD was
then exercised on Kubernetes on 2026-08-06, two tenants sharing one MI300X.
Continue on `0.x` until the list below is empty; as of `0.8.0` it is not.

### Before 1.0.0

`1.0.0` freezes the metrics contract, so what stands in its way is not
unfinished features but **parts of the contract nothing has ever tested**. A
name, unit or label set that no real host has produced is a promise made on
the strength of a fixture. The list is deliberately short, and each entry says
what would close it.

1. **A bare-metal AMD host.** Every AMD card read so far has been an SR-IOV VF,
   which a hypervisor pins to `SPX`. `prickle_gpu_amd_partition_info` therefore
   has exactly one observed value, and CPX/DPX — the reason the family exists —
   have never been emitted. This is the AMD analogue of the MIG-on/MIG-off
   fixture pair the NVIDIA trees already have. Closes on one capture.
2. **A host with both vendors' cards.** Both collectors render into a single
   `Set`, and no machine has ever held an NVIDIA and an AMD card at once to
   prove the merged output is what the contract says. The failure this guards
   against is a label or family that only collides when both are present.
3. **NVIDIA source selection consults the PATH, not the bus.** `nvidia-smi`
   merely *being* on `PATH` selects the `smi` source; `CountNVIDIAGPUs` answers
   the question that should decide it — is there a card — and is consulted only
   by `prickle diagnose`. On a host with a leftover or stubbed `nvidia-smi` and
   no NVIDIA card, the result is a parse error every scrape and
   `prickle_gpu_nvidia_source_info{source="smi"}` asserting a source that
   cannot work. **A metric that states something false is a contract defect,
   not a cosmetic one**, so this closes before the freeze rather than after it.
   It was left alone deliberately during the AMD work — it is pre-existing, and
   fixing it touches §Collectors' "attempt the load, fall back silently", which
   is why it needs its own decision rather than a drive-by.
4. **`docs/verification.md` stops at the 0.7.1 acceptance sweep.** The 0.8.0
   sweep passed 14 of 14 base images on 2026-08-04 and that result is not in
   the repository. A frozen contract is a promise, and the record of what was
   actually run on real hardware is what makes it credible to anyone who did
   not run it.

**Intel is not on this list and is not a blocker** — §Collectors places it out
of scope for want of a capture host, and a freeze does not become less
defensible for excluding hardware nobody can obtain. Adding Intel later is a
new family on a new vendor, which the table above already calls a minor.

From 1.0.0 on, a metric that must change is emitted under both the old and new
names for one full minor, with the old name marked deprecated in its `# HELP`
text, and removed at the next major.

`CHANGELOG.md` is written by hand, not generated from commits: a metric change
needs prose that says what to do about it, which no generator writes.

## Session checklist

Before writing code: read this file, read the target package's existing code,
and confirm the fixture tree for the collector exists.

Before committing: **run `ci/check.sh`.** It is this checklist in one command
and CI runs the same script, so a green local run and a green CI run mean the
same thing. It stops at the first failure. The gates, each tied back to the
section it enforces:

`gofmt -l` · `go vet` · `go test -race` (§Architecture) · the `nvml` build
compiles and vets (§Distribution) · zero third-party dependencies
(§Hard constraints #1) · an SPDX header in every `.go` file (§Identity) ·
`promtool check metrics` on every golden file (§Metrics contract) ·
documentation links resolve · every fixture accounted for in its README
(§Testing rules) · the shipped Grafana dashboards match their generator
(§Distribution) · no PIDs in any fixture (§Metrics contract) · the released
version stated consistently (§Versioning) · naming discipline (§Identity) ·
the metric prefix never abbreviated (§Identity).

This list previously read "`go vet`, tests green, `promtool check metrics` on
sample output, and the forbidden-string grep" — four gates where there are
fourteen, and no mention that a script existed to run them. It is corrected
here rather than in `ci/check.sh` because the script was the accurate one.

# Changelog

All notable changes to `prickle-exporter` are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning follows [SPEC.md §Versioning](SPEC.md#versioning): every package is
under `internal/`, so there is no Go API to version — SemVer applies to the
**metrics contract** and the **command line**. Pre-1.0, the minor tracks the
roadmap phase; `1.0.0` is where the metrics contract freezes.

This file is written by hand. A metric change needs prose telling an operator
what to do about it, which no commit-log generator writes.

## [Unreleased]

### Added

- **`prickle_gpu_process_memory_bytes` carries a `container` label**, so a GPU
  process can be joined to `prickle_container_info` and through it to a pod
  name, a namespace and an image. "Which pod is holding this card" was
  previously unanswerable from this exporter: the series was keyed on `command`
  alone, and two pods running the same image were one series.

  The value comes from the process's own `/proc/<pid>/cgroup`. The PID dies
  there, exactly as it already does for `command` — SPEC.md §Metrics contract
  forbids a PID as a label or a value, not as a transient lookup key. `container`
  is already in the closed hot-series identity set, so this adds no new label
  key to the contract. It is empty for a process running on the host, which is
  a statement about the process rather than a failure to look.

  Adding a label key to an existing series is a **major** under SPEC.md
  §Versioning; pre-1.0 a minor may do it. Both sources emit it, so NVML and
  `nvidia-smi` still produce identical output for the same GPU.

  The cgroup parse is delegated to the container collector rather than repeated,
  so the runtime prefix table and the two cgroup-driver layouts keep one
  definition between them.

- **A "GPU memory by pod and container" panel**, labelled
  `<node>/<gpu-index>/<pod>/<container>`, and GPU series everywhere else are now
  identified as `<node>/<gpu-index>/<gpu_uuid>`. A UUID alone does not say which
  machine or which slot a card is in.

### Changed

- **All four dashboards drop their dropdowns and keep the `contains`
  textboxes.** A dashboard's controls are now the datasource and one `<label>
  contains` box per identity label. Type a fragment, press enter, and the panels
  filter — one action instead of typing into a box and then opening the dropdown
  it filtered. The boxes combine, so `namespace contains kube` with `container
  contains api` narrows to both. SPEC.md §Distribution was amended to match
  before the generator was touched; it specified the paired control.

  Two things this gives up, deliberately. **Nothing enumerates the values any
  more** — the chained dropdowns were also how you discovered that a namespace
  existed, and pod UIDs especially are not guessable, so a value now comes off a
  panel legend rather than out of a list. And the typed string reaches PromQL as
  `.*input.*` **unescaped**, which makes it a regex rather than a literal:
  `web-0[12]` is a working filter, an unbalanced `[` errors the panel instead of
  matching nothing. Escaping would have cost the first of those to prevent the
  second.

### Fixed

- **`prickle diagnose` was silent about pod names, including when they were
  all missing.** The subcommand exists to answer "what can this host be read
  from", and it said nothing about the one path that depends on a privilege: on
  a node where every container came back unnamed it printed a wholly healthy
  report. Found while verifying the chart change below, using the same forced
  misconfiguration.

  Phase 2 now always ends with a `pod names:` line. Off, it says so and names
  the flag. On and working, it reports how many of the containers in a pod
  resolved. On and reading nothing, it says so loudly and prints the running
  uid and gid, the directory's mode, and the three things that satisfy it —
  which is the fact that turns a puzzling empty label into a one-line fix.

  The silence was a consequence of a deliberate choice that stays: an unreadable
  pod log directory is **not** a collection error, because the container metrics
  are unaffected and only the names are lost, so raising
  `prickle_collector_errors_total` on every pass forever would be wrong. That
  makes `diagnose` the only place the failure can surface, and it now does.

- **The chart asked for `CAP_DAC_READ_SEARCH` and never used it.** Measured on
  2026-08-04: Kubernetes puts a capability added to a **non-root** uid in the
  bounding set alone, so `CapPrm`, `CapEff` and `CapAmb` were all zero and the
  grant was inert. Pod names resolved anyway — because `runAsGroup` was unset,
  the process inherited **gid 0**, and the group bits of `0750 root:root`
  `/var/log/pods` are what let it in. The feature worked by accident, and the
  accident was one hardening pass away from disappearing with nothing logged.

  The capability is gone and `runAsGroup: 0` is now set explicitly when
  `collectors.podNames.enabled` is on. This is a **reduction** in privilege, not
  a trade: group-root membership reaches files owned by group root, where the
  capability, had it ever worked, would have bypassed file-read checks host-wide.

  Measured one variable at a time, on `/var/log/pods`: uid 65532 + gid 0 reads
  it **with or without** the capability; uid 65532 + gid 65532 is **denied even
  with it**; uid 0 reads it with every capability dropped, via the owner bits.

  **The 0.7.0 changelog and the README were right about systemd and wrong about
  Kubernetes**, and both are corrected. Under systemd the documented drop-in
  sets `AmbientCapabilities=CAP_DAC_READ_SEARCH`, and ambient capabilities
  genuinely work for a non-root `DynamicUser` — verified: `CapPrm=CapEff=CapAmb`
  all `0x4` and the read succeeds, where the same user without the drop-in is
  denied. Kubernetes exposes no ambient-capability field, which is the whole of
  the difference.

  Operators on a node whose `/var/log/pods` is `0700` rather than `0750` need
  uid 0; the group-root route yields no names there, silently.

- **Per-process GPU attribution produced nothing in a container, silently.**
  Naming a process reads its `exe` link, a `PTRACE_MODE_READ` operation, and
  Yama `ptrace_scope=1` — the default on Debian and Ubuntu — permits that only
  for a process's own descendants. A GPU workload is never the exporter's
  descendant, so every process resolved to an empty command and was dropped by
  the guard that keeps PIDs out of labels: the family simply never appeared,
  with nothing logged. The chart now adds `CAP_SYS_PTRACE` when
  `collectors.perProcess` is set. Verified on an H100.

- **The chart granted GPU access with `hostPath` device mounts**, which make a
  device node visible without adding it to the container's device cgroup, so an
  unprivileged container was refused and `nvmlInit` returned 999. It now sets
  `NVIDIA_VISIBLE_DEVICES`, which the NVIDIA container runtime honours — and
  which, unlike requesting `nvidia.com/gpu: 1`, does not consume an allocatable
  GPU. On a single-GPU node that would have taken the only card away from the
  workload the exporter exists to measure.

- **`prickle-nvml` segfaulted whenever `nvmlInit` failed.** `prickle_close()`
  calls `dlclose()` but left all 27 function pointers dangling, so the `NULL`
  guards still passed and the call jumped into unmapped memory — while
  formatting the error that explains the failure. Guaranteed to crash in exactly
  the degraded case a clean message matters most for.

- **Container dashboard legends never rendered a pod name.** `pod_name` was
  named in a `sum by` over a hot series, where it does not exist, and the two
  `label_replace` calls were ordered so that the truncated UID overwrote the
  name whenever one was resolved. Both fixed; legends read
  `demo-idle/4407fe1bc9bc`.

- **Four container panels failed outright** with `found duplicate series for the
  match group` once that join existed. During a DaemonSet rollout two exporters
  report the same container, and a one-to-many join needs its right-hand side
  aggregated. An instant query at any quiet moment shows nothing wrong.

## [0.7.1] — 2026-08-02

A packaging release. No metric, label or flag changes — a 0.7.0 scrape and a
0.7.1 scrape are byte-identical on the same host.

**If you install with systemd, take this one.** The 0.7.0 tarball did not
contain `prickle.service`, so the README's own install instructions could not
be followed from the release artifact. It is in the tarball now, and the
release workflow fails if it ever goes missing again.

### Fixed

- **The release tarball now contains its systemd unit.** It shipped `prickle`,
  `LICENSE`, `README.md` and `CHANGELOG.md` — while the README told you to
  `install packaging/systemd/prickle.service`, a path that exists only in a git
  clone. Anyone deploying from the release artifact, which is how a binary
  normally reaches a server, had no unit to install. The README's systemd
  section is now written against the tarball, and the release workflow asserts
  the unit is present rather than assuming it.

- **The headline Helm command no longer sets `serviceMonitor.enabled=true`.**
  On a cluster without the Prometheus Operator's CRD that makes `helm install`
  fail outright — correct behaviour from the chart, since a silently ignored
  ServiceMonitor is a cluster that looks monitored and is not, but the wrong
  thing to put in a line people copy. It is documented in the flag table
  instead.

  Both of the above were found by running every documented install method on
  all fourteen DigitalOcean base images against the published 0.7.0 artifacts.
  Both are the same shape as the `nvml.enabled=true` fix earlier in this
  section: a copy-paste command carrying a flag or path that only works in some
  environments. `docs/verification.md` records the full matrix.

- **`capture-fixtures.sh` captured no Kubernetes cgroups at all on a
  cgroupfs-driver node.** Three places hardcoded
  `/sys/fs/cgroup/kubepods.slice`, which is the systemd driver's spelling; the
  cgroupfs driver writes `/sys/fs/cgroup/kubepods`. The script then reported
  `no kubepods.slice pods — Phase 2 kubernetes fixtures will be EMPTY`, which
  reads as "you forgot to start something" on a host that was running nine
  pods. 53 files captured where the fixed script takes 869. It now resolves
  either layout and prints which one it found.

- **`cgroup.procs` is no longer captured.** It is the only file in a cgroup
  that contains PIDs, SPEC.md §Metrics contract forbids a PID appearing
  anywhere, and the collector never reads it. Keeping it out of commits was a
  manual step in a checklist — a SPEC violation one forgotten edit away from
  the repository, in exchange for a file nothing reads. `ci/check.sh` now fails
  if one appears under any `testdata/` tree.

- **The contributor docs claimed shipped features were missing.**
  `scripts/README.md` said cgroupfs-driver Docker and the
  `kubepods/<qos>/pod<uid>/<hex>` layout were "unimplemented today, so those
  hosts report no containers at all", and that CRI-O was parse-tested only. All
  three shipped in 0.5.x with captured fixtures. `testdata/README.md`
  contradicted its own table three lines below it.

- **The README's Helm example set `nvml.enabled=true` in its headline
  command**, which cannot work on a node without an NVIDIA driver — the driver
  library and device nodes are `hostPath` mounts, so such a node leaves the pod
  in `ContainerCreating` indefinitely rather than `CrashLoopBackOff`. GPU
  metrics now get their own section with a `nodeSelector`, and the
  `--set-string` that command needs, since plain `--set` types the value as a
  boolean and the API rejects the DaemonSet.

### Added

- **A capture of the cgroupfs Guaranteed layout**,
  `kubeadm-cgroupfs-20260802/`. Under that driver a Guaranteed pod's cgroup is
  `kubepods/pod<uid>/<hex>` — one level shallower than Burstable's, with no QoS
  component — so the class is implied by an absent directory level. It had only
  ever been checked against a directory name, because Guaranteed requires
  `requests == limits` for cpu *and* memory on every container in a pod and
  nothing arrives there by accident. The container coverage-gap table now has
  no open rows.

- `cpu.pressure` and `io.pressure` to the capture script — read by the
  collector, previously covered only by a hand-written tree.

- **Tests for `cmd/prickle`**, which was at 3.5% coverage against 77–97%
  everywhere else despite holding flag parsing, `diagnose` and startup wiring.
  Now 69.4%. The notable one walks `packaging/` and `docs/` and asserts every
  flag they name is one the binary defines: those artifacts pass flags to a
  binary none of them can typecheck against.

- **Three `ci/check.sh` gates**, each verified by planting a failure: every
  fixture directory must be named in its README, the version must be stated
  consistently across the README and chart, and no `cgroup.procs` may exist in
  the tree.

### Changed

- The README has a nested Contents, Status moved down beside License, and the
  two Kubernetes installs — one that runs on every node, one node-selected for
  GPUs — are now distinguishable from the Contents alone.

## [0.7.0] — 2026-08-02

**If you deploy with the Helm chart, this is the release that makes
`collectors.podNames.enabled=true` work.** The flag it passes,
`-collector.container.pod-names`, does not exist in 0.6.0 — the container exits
immediately with `flag provided but not defined`.

Two label keys are added to `prickle_container_info`, which SPEC.md §Versioning
calls a **major**; pre-1.0 a minor is allowed to, and this is that. Nothing was
renamed, no label changed meaning, and every existing series is byte-identical.
A recording rule or dashboard written against 0.6.0 keeps working untouched.

### Added

- **`-collector.container.pod-names` resolves a pod's UID to its namespace and
  name**, off by default. The cgroup tree carries a pod's UID and never its
  name, which is why `namespace` has been in the closed identity set since
  Phase 1 and never populated. The kubelet does carry it:
  `/var/log/pods/<namespace>_<pod>_<uid>/` exists on every CRI runtime, so one
  directory listing supplies both. No API call, no second exporter, and nothing
  inside those directories is read — the names are entirely in the directory
  names, so workload log content stays out of reach.

  **It is off by default because of what it costs.** `/var/log/pods` is
  `root:root 0750`, as are the two alternatives. `CAP_DAC_READ_SEARCH` is
  enough and full root is not needed — but that capability bypasses file-read
  checks host-wide, and in the same test that proved it reads `/var/log/pods` it
  also read `/etc/shadow`. For a process whose only power is reading files, that
  is close to the whole of root. The shipped units and chart stay unprivileged;
  enabling this is a deliberate trade, and both document the drop-in that makes
  it.

  Enabling it adds `pod_name` to `prickle_container_info` and `namespace` to
  container hot series. **`pod` still carries the UID** — it is the key every
  existing rule joins on, and repurposing a label silently breaks them.

- `prickle_container_info` now always carries **`pod_name`** and **`namespace`**
  label keys, empty unless the above is enabled. Adding a label key to an
  existing series is a **major** under SPEC.md §Versioning; pre-1.0 a minor may
  do it. No value or series changed — the golden diffs are the two label keys
  and nothing else.

  `namespace` is on the gauge as well as on the hot series, which reads as
  duplication and is not. The dashboards fill their dropdowns with
  `label_values(prickle_container_info, …)`, so a label living only on hot
  series has nothing to populate a namespace picker from — found by standing the
  Kubernetes demo up and watching that dropdown stay empty while the hot series
  plainly carried the label. `pod` has been on this gauge for the same reason
  since Phase 2.

- **A Kubernetes demo** in `packaging/kubernetes-demo/`: prickle, Prometheus,
  Grafana with all four dashboards, and a deliberately throttled workload to
  draw. The Compose quickstart's equivalent on a cluster. Its dashboard
  ConfigMap is generated by the same run of `scripts/make-dashboards.py` that
  writes the JSON, and `ci/check.sh` fails if either copy is stale.

- The README has a **table of contents** and a **"Pod names, and what they
  cost"** section, and its Helm example now sets the flags most Kubernetes
  users want with a table of what each one trades away.

### Changed

- Container dashboard legends prefer the pod **name** where it is available and
  fall back to the truncated UID where it is not.

## [0.6.0] — 2026-08-01

**Read this before upgrading: the default output is smaller.**
`-metrics.preset` now defaults to `minimal`, which exposes the families the
shipped dashboards query and withholds the rest — on a typical host that is 25
families where 0.5.x served about 118. If you query anything the dashboards do
not, set `-metrics.preset=full` and nothing changes. `prickle diagnose` reports
the active preset and the number of families withheld.

SPEC.md §Versioning calls a default that changes output a **major**; pre-1.0 a
minor is allowed to do it, and this is that. No metric was renamed and no label
key changed meaning, so what you still receive is byte-identical to before.

### Changed

- **Container panels are legended `<pod>/<container>`**, truncated to 8 and 12
  characters. A plain Docker container with no pod keeps a bare
  `<container>` — the naive `{{pod}}/{{container}}` renders `/abc123` there,
  which is half the fleet on a mixed estate.

  Truncated because untruncated it is unreadable: a container ID is 64 hex
  characters, and **`pod` is the pod's UID, not its name**. The cgroup tree has
  no name to offer — the kernel only ever sees the UID — so joined in full the
  legend runs to about a hundred characters. 12 characters of container ID is
  Docker's own short-ID convention, so the value still matches what `docker ps`
  prints. Real pod *names* need kube-state-metrics; this exporter cannot know
  them.

  Dashboard-only: `pod` is already on every container series that has one, and
  a pre-joined label would put redundant bytes on every sample of every host.

### Added

- **`-metrics.preset` chooses how much is exposed**: `minimal` (the new
  default), `full`, or `custom` with `-metrics.include` regexps. A host emits
  about 156 families and the shipped dashboards query 35, so the default now
  withholds roughly three quarters of what the collectors produce.

  **This changes the default output, which SPEC.md §Versioning calls a major.**
  Pre-1.0 a minor is allowed to, but read this before upgrading: if you query a
  family the dashboards do not, it is gone until you set
  `-metrics.preset=full`. `prickle diagnose` reports the active preset and the
  number of families withheld, so the answer to "where did my metric go" is one
  command rather than a source dive.

  Self-metrics survive every preset — a scrape that has been reduced must still
  be able to report that it was, which is the same exemption the cardinality cap
  has. Misused flags fail at startup rather than at the first scrape, because a
  silently ignored filter looks exactly like a metric the host does not have.

  The minimal set is an explicit list in code and a test asserts the dashboards
  are a *subset* of it. Defining it as "the metrics the dashboards use" would
  have coupled every scrape in a fleet to whatever someone last edited in a
  panel.

### Fixed

- **The systemd install instructions were wrong on SELinux hosts.** A unit file
  copied into `/etc/systemd/system/` can keep the wrong SELinux label, and
  systemd then reports `Unit prickle.service not found` for a file that is
  plainly there in `ls` — no denial logged, SELinux never mentioned. The
  documented sequence now runs `restorecon` after the install, guarded so it is
  a no-op on distributions that do not have it.

  Found by sweeping the **released** 0.5.0 binary across all fourteen
  DigitalOcean base images, which is the point of sweeping released artifacts:
  AlmaLinux 8 labelled the copy `default_t` while Rocky 8 — equally SELinux
  Enforcing, same RHEL 8 base, same install command — labelled it
  `systemd_unit_file_t` and worked. Why the two differ is not established, so
  the guidance is to run `restorecon` rather than to reason about when it is
  needed. Documentation only; no binary changed.

## [0.5.0] — 2026-08-01

Phase 5: distribution. Also the release where the container collector stopped
being blind to whole runtimes and to an entire cgroup hierarchy.

**Upgrading.** No metric was renamed and no label key was added to an existing
series, so every recording rule and dashboard built on 0.4.x still holds. But
several classes of host that reported *nothing* now report containers — a
cgroup v1 host, a hybrid host, a podman host, a standalone containerd host. If
you scrape one, expect its series count to rise from zero; that is the fix
arriving, not a leak.

**Not 1.0.** SPEC.md §Versioning used to say 1.0.0 *was* Phase 5, which made
freezing the metrics contract a consequence of finishing a roadmap row rather
than a judgement that it was ready. It is not ready: cgroup v1, podman and
standalone containerd all arrived in this release, and AMD and multi-GPU are
still unproven. Decoupled, and the line continues at 0.5.x.

### Added

- **Container images**, `ghcr.io/starkdrift/prickle-exporter`, as
  multi-architecture manifest lists over amd64 and arm64. `X.Y.Z` is the static
  binary on `scratch` (3.0 MB); `X.Y.Z-nvml` is the NVML build on distroless
  (10.8 MB). The Helm chart has defaulted to this repository since it was
  written and nothing published it, so a default `helm install` could only ever
  have failed. Digest-addressable and mirrorable with `skopeo copy --all` for
  air-gapped installs, with provenance attested to the manifest-list digest and
  pushed to the registry so it survives the copy.
- **A Helm chart**, `packaging/helm/prickle-exporter`: a DaemonSet, a headless
  Service and an optional ServiceMonitor. No ServiceAccount and no RBAC — the
  exporter reads the node's filesystem and never the Kubernetes API, so there
  is nothing a token would be for.
- **Hardened systemd units** for both binaries. `DynamicUser`,
  `ProtectSystem=strict`, an empty `CapabilityBoundingSet` and a syscall
  allow-list; `systemd-analyze security` rates them 1.5 and 1.8.
- **Four Grafana dashboards** — GPU Tenancy, Node Overview, Container Resources,
  Fleet Health — with a textbox paired to a dropdown per identity label,
  contains-search, chained filtering, and `command` rather than any PID.
- **A compose quickstart** bundling prickle, Prometheus and a pre-provisioned
  Grafana.

### Notes

- **Swept across every DigitalOcean base image** on 2026-08-01 — fourteen of
  them, kernels 4.18 to 7.0, both cgroup hierarchies, podman 3.4.4 to 6.0.2.
  Each was rebuilt from stock, given podman from its own repository and one
  container with a CPU quota and memory limit, then diagnosed and scraped.
  **14 of 14 reported their container, with zero exposition problems.** The
  matrix is in README.md §Verified platforms.
- **PSI is absent on the entire RHEL family** — Alma, Rocky and CentOS Stream
  at 8, 9 and 10. The kernels have it compiled in; RHEL ships it off unless the
  host is booted with `psi=1`. So `prickle_host_pressure_*` and
  `prickle_container_pressure_stalled_seconds_total` do not exist there and any
  saturation panel is blank. A property of the distribution, not of this
  exporter, which reports each file as `missing` rather than failing — but it
  decides whether a dashboard is worth building, so it is written down.
- The per-container series count follows from those two facts: 20 on cgroup v1,
  24 on RHEL's v2 with PSI off, 28–30 elsewhere. A host reporting fewer series
  than its neighbour is usually this rather than a fault.

### Fixed

- **A hybrid host reported half its container metrics, silently.** Found by
  putting a real host into hybrid mode — v1 controllers under a tmpfs
  `/sys/fs/cgroup` with cgroup2 mounted at `/sys/fs/cgroup/unified` — which is
  what systemd's hybrid mode does and what the code had never been run against.
  Hierarchy selection treated "a cgroup2 mount exists" as "the cgroup root is
  cgroup2", so the v2 reader was chosen, walked the **v1** tree, matched the
  same container directory names, and read v2 filenames that are not there.

  It did not error, and no container went missing. v1 and v2 spell a few fields
  identically, so those parsed and the rest disappeared: **27 series where the
  correct reader produces 54**, with `cpu_usage_seconds_total`,
  `memory_usage_bytes` and `cpu_throttled_seconds_total` simply absent. A host
  that looks quiet rather than broken. Shipped in this same unreleased cycle, so
  no released version is affected.

### Added

- **Standalone containerd is now reported.** `cri-containerd-<hex>.scope` is
  containerd reached through the CRI — the Kubernetes path — and was the only
  spelling recognised. containerd driven directly writes
  `nerdctl-<hex>.scope` under `system.slice`, so a plain containerd host
  reported nothing. Both carry `runtime="containerd"`: they are one runtime
  reached two ways, and the label names the runtime rather than the client, so
  a query grouping by it need not know which tool started a container.

- **Podman containers are now reported.** `libpod-<hex>.scope` under
  `machine.slice` was in no runtime list, so a host running podman reported
  nothing — and podman is the default container runtime on RHEL and Fedora, so
  that is an ordinary host, not an edge case. `runtime="podman"` on
  `prickle_container_info`; there is no pod or QoS identity under
  `machine.slice`, so those stay empty as they do for plain Docker. Podman's
  paired `libpod-conmon-<hex>.scope` monitors are not counted — stripping the
  prefix leaves `conmon-<hex>`, which is not hex, so the existing ID test
  rejects them. `prickle diagnose` now counts podman in its runtime breakdown
  instead of reporting the containers as having no runtime at all.

- **cgroup v1 and hybrid hosts are now read.** SPEC.md §Hard constraints #4 said
  "v2 only" and was reversed on 2026-08-01; the SPEC change is its own commit,
  ahead of the code. What reversed it was running `prickle diagnose` on a real
  v1 host — Rocky Linux 8.10, kernel 4.18 — and watching it correctly announce
  that it would report nothing. RHEL 8 defaults to v1 and is supported into
  2029, so "out of scope" was quietly deciding that an ordinary enterprise host
  gets an empty scrape.

  **The metrics contract does not fork.** The same names, units and labels come
  out of either hierarchy; only the file a value is read from changes. Two
  conversions exist to keep that true: v1 counts CPU time in nanoseconds and
  USER_HZ where v2 uses microseconds, and `cpu.shares` (default 1024) is
  rescaled onto `cpu.weight`'s 1–10000 range (default 100) so
  `prickle_container_cpu_weight` does not mean two things depending on how a
  host booted. v1's `memory.limit_in_bytes` sentinel of `9223372036854771712`
  is treated as "unlimited" exactly as v2's `max` is, so no container reports a
  nine-exabyte limit.

  **On v1 there is no `prickle_container_pressure_stalled_seconds_total`** —
  PSI arrived with the unified hierarchy, so the family is *absent* rather than
  zero, for the same reason `prickle_gpu_utilization_ratio` vanishes under MIG.
  A zero would read as "nothing is stalling" instead of "this kernel cannot
  say". `prickle diagnose` states which hierarchy is live and names this cost.

  A hybrid host needs no configuration: v2 is tried first and v1 only if it
  found nothing.

- **`prickle diagnose` now explains the `nvidia-smi` deadline overrun**, which
  is the failure most likely to be mistaken for a bug in this exporter. With
  NVIDIA **persistence mode disabled** the driver tears down its state whenever
  the last client exits, so every `nvidia-smi` the fallback source spawns pays
  the initialisation cost again — measured at **2.7–3.5 s per invocation** on an
  idle H100, driver 580.173.02. The source makes several per pass, so it
  overruns the default 5 s `-collector.timeout` and reports a killed subprocess
  on **every scrape**, while still returning partial data.

  Enabling persistence mode took the same pass from **5.4 s to 61 ms** — an 88×
  difference between "errors constantly" and "fine". `diagnose` now names the
  three fixes (`nvidia-smi -pm 1`, deploy `prickle-nvml`, or raise the timeout)
  and says which of them hides the problem rather than removing it.
  `prickle-nvml` never had this failure: holding NVML open keeps the driver
  initialised for the same reason persistence mode does. Found by running the
  released artifacts on a GPU node rather than by reading the code.

## [0.4.0] — 2026-08-01

Phase 4: caps and timeouts. Also the release where the container collector
stopped being silent on whole classes of host — three runtime layouts it could
not read are now read, each against a captured tree rather than a guess.

**Upgrading:** no metric was renamed and no label key was added to an existing
series, so every recording rule and dashboard built on 0.3.x still holds. But a
node running the cgroupfs cgroup driver, or cgroupfs-driver Docker, reported
**no containers at all** before this and reports them now. If you scrape one,
expect its series count to rise from nothing to roughly 25 per container —
that is the fix arriving, not a leak. See the `runtime` label note below before
you join on it.

### Added

- **Per-collector cardinality caps**, the last item SPEC.md §Metrics contract
  assigns to Phase 4. Past `-collector.max-series` in one pass, a collector's
  further samples are dropped and counted rather than allowed to grow the
  render without bound. The budget is per collector and resets between them, so
  a runaway source costs its own tail and not the collectors after it.
- **`prickle_collector_series`** and **`prickle_collector_series_dropped_total`**,
  the self-instrumentation the cap reports through. The first is a useful
  cardinality gauge whether or not a cap is set; the second is the alert. Both
  are emitted outside every collector's budget on purpose — a breach that
  erased its own evidence would leave a truncated scrape looking like a healthy
  smaller one, which is the failure this whole mechanism exists to avoid.
- **`-collector.max-series`**, default 100000, `0` to disable. Sized as a
  backstop, not a budget: the largest thing measured so far is a Kubernetes
  node at a few thousand series, so a host would have to be two orders of
  magnitude past that before the cap binds. A default low enough to bind on a
  real host would silently truncate one, and a truncated scrape looks like a
  healthy one — the worse of the two failures.

- **Containers on a cgroupfs-driver node are now reported.** The kubelet's
  cgroup driver decides the shape of the whole tree, and only the systemd shape
  was read: `identify` tested for a `.scope` suffix before anything else, so on
  a node using the **cgroupfs** driver — `kubepods/<qos>/pod<uid>/<hex>`, no
  suffix, no runtime prefix, QoS as its own directory level, the UID unescaped
  — it rejected every directory and the collector reported nothing at all.
  **This is the default on a managed Kubernetes cluster**, not an exotic
  setting, so the blast radius was "an entire class of node reports zero
  containers", not "an edge case". A **minor**, not a patch: no existing series
  changes, but hosts that reported nothing now emit the full
  `prickle_container_*` namespace, and a Prometheus scraping one will see its
  series count rise.
- **Containers on a cgroupfs-driver Docker host are now reported**, from
  `/sys/fs/cgroup/docker/<hex>/`. Same defect, same cause, different runtime:
  the directory has no `.scope` suffix, so nothing matched it. `runtime` is
  `docker` here — unlike the Kubernetes layout, the parent directory names it.
  Also a **minor**, and for the same reason: a host that reported nothing now
  reports its containers.
- The `runtime` attribute on `prickle_container_info` is **empty** for
  containers found this way. Those directory names do not encode the runtime —
  a property of the layout, not a parse failure — so the tree cannot say
  whether containerd or CRI-O is underneath. An empty attribute says "not known
  from here"; naming one would be a guess, and the collector already declines
  the same way over `namespace`. If you `group_left` on `runtime`, expect the
  empty string rather than a missing series. `prickle diagnose` now says so
  explicitly instead of printing `docker 0, containerd 0, crio 0` under a
  non-zero total, which read as three failed identifications.

- **A CRI-O host**, captured at last. `crio-<hex>.scope` had been parse-only
  since Phase 2 — SPEC.md §Collectors names CRI-O, but the prefix came from the
  runtime's documentation rather than from a host. It is right. What the
  documentation did not mention is that CRI-O writes a second, **empty**
  directory per container, `crio-<hex>` with no suffix; it is skipped, and there
  is now a test saying so on purpose rather than by accident. **Worth knowing
  before comparing container counts across runtimes:** on the same five pods,
  containerd produces ten container cgroups and CRI-O five, so a node whose
  runtime is switched underneath a dashboard shows a step change in
  `prickle_container_info`. Neither count is wrong; they are different
  statements about what a container is.
- **A Guaranteed pod**, captured at last. `kubepods-pod<uid>.slice` — the
  systemd shape with no QoS component — had been parse-only since Phase 2
  because no cluster anyone captured ran one. A two-node kubeadm cluster was
  stood up with three pods, one per QoS class, so all three spellings now sit
  in one tree. The existing rule was **right**; this confirms it against a
  kernel's output rather than against systemd's naming documentation. No
  behaviour changed.
- **A CPU quota that is actually being hit**, captured rather than hand-built.
  Every cgroup in every previous capture read `cpu.max` = `max 100000`, so
  `prickle_container_cpu_throttled_periods_total` was zero throughout the
  golden file and the quota arithmetic was pinned only by trees written by
  hand. `docker-cgroupfs-20260801` holds a container throttled in 385 of 386
  periods for 28.797198 s, one with a quota it never reaches, and one with no
  quota at all — so `0.25` and `1.5` cores are now derived from numbers a
  kernel wrote. No behaviour changed; this is the evidence that the existing
  behaviour is right.

### Notes

- **Verified on a third card class and a new platform.** An H100 80GB on
  **Ubuntu 26.04, kernel 7.0.0**, driver 580.173.02, running as a Kubernetes
  worker. Both artifacts build, the full suite passes under `-race`, and the
  three NVML hardware assertions pass — including the two regression tests for
  the reserved-memory and closed-handle defects found on the earlier cards. The
  two NVIDIA sources differed in exactly one series,
  `prickle_gpu_nvidia_source_info{source=…}`, which exists to differ. This is
  the first run on a 7.x kernel.
- **The GPU and container collectors have now sampled one host in one pass** —
  452 host series, 157 container series across five CRI-O containers, and 8 GPU
  series through NVML. Every fixture tree is a single collector's world, so no
  fixture could have shown this; it needed a GPU node that was also running
  containers.
- **Multi-GPU is still unverified.** Every card reached so far has been a single
  GPU per host.

### Fixed

- **`prickle diagnose` told hosts that have a GPU they have no GPU.** When
  automatic selection found neither source, it closed with "On a host with no
  NVIDIA GPU this is expected and not an error" — unconditionally. On a machine
  whose driver simply is not installed yet, that is the wrong answer, and the
  one an operator is least equipped to doubt: every other signal the exporter
  has reads the same either way, because a `dlopen` that fails and an
  `nvidia-smi` that is absent look identical whether or not a card is in the
  slot. It now reads the PCI bus, which advertises the card with or without a
  driver, and says which of the two situations this is — or, when the bus
  itself cannot be read, says that instead of guessing. **No metric changes**;
  this is `diagnose` output only. The forced-source case was already fixed in
  0.3.0; this is the same defect on the path that is actually common — a GPU
  instance before its driver is provisioned.
- `scripts/capture-fixtures.sh` now captures
  `/sys/bus/pci/devices/*/{vendor,device,class}` on every host, and the
  `h100-nodriver-20260801` fixture is that capture from an Ubuntu 26.04 VM with
  an H100 SXM5 on the bus and no driver at all. Fourteen devices, not one: the
  VM's console is a virtio VGA adapter, so the tree holds a display-class
  device that is not NVIDIA beside an NVIDIA device that is not VGA-class.
  Matching on vendor alone, or on class alone, gets a different answer on that
  tree than matching on both — which is what makes it a fixture rather than a
  sample.

## [0.3.0] — 2026-08-01

Phase 3: the GPU collector, **NVIDIA only**. Nothing in Phase 1 or 2 output
changes. AMD is in SPEC.md §Collectors' Phase 3 scope and is *not* implemented;
Intel was dropped from that scope — see Notes.

### Added

- **`prickle-nvml`, the second release artifact** SPEC.md §Distribution has
  called for since 0.1.0. Every release until now shipped only the static
  `prickle`, because no `//go:build nvml` source existed and the second artifact
  would have been a byte-for-byte copy of the first under a name promising NVML
  support. It is dynamically linked, because a static binary cannot `dlopen` at
  all. **Which one to install:** `prickle-nvml` on NVIDIA hosts, for the richer
  data and the only per-process MIG attribution there is; `prickle` anywhere
  else, and anywhere a glibc floor is unwelcome — a scratch container, an older
  distribution. Neither links an NVIDIA library at compile time, and
  `prickle-nvml` falls back to the `nvidia-smi` path on its own when the driver
  library will not load, so installing it on a host without a GPU is a
  non-event rather than a startup failure.
- **NVIDIA GPU collector** reporting per-card utilization, memory, temperature
  and power; MIG topology; and optional per-process memory. Two interchangeable
  implementations behind one `nvidiaSource` interface, selected once at startup:
  NVML via `dlopen` where available, `nvidia-smi` otherwise. About 12 series for
  one MIG-partitioned card.
- `prickle_gpu_nvidia_source_info`, recording which implementation is live, so a
  scrape says whether it came from NVML or the fallback.
- **`-collector.gpu.per-process`**, adding `prickle_gpu_process_memory_bytes`
  keyed on `command` — the basename of the executable path. Opt-in, because it
  is one series per distinct command per GPU.
- **`-collector.gpu.nvidia-source={auto,nvml,smi}`** to force one path for
  debugging, plus `-collector.gpu` and `-collector.gpu.nvidia-smi-command`.
- `prickle diagnose` gained a Phase 3 section: which of the two artifacts this
  binary is, which source is live, how many GPUs and MIG instances were found,
  and — when nothing loaded — why each candidate declined.
- `ci/check.sh` now compiles, vets and tests the `-tags nvml` build. That source
  is invisible to every other step, so an edit to shared GPU code could break
  the second shipped artifact silently.
- **A hardware test asserting the two sources agree**, which SPEC.md §Testing
  rules requires and nothing implemented. It reads the same card through both
  implementations and compares whole series identities — name, label keys *and*
  label values — so a divergence in a label shows up as a missing series rather
  than as a value that looks close enough. It skips wherever NVML does not
  load, so `go test -tags nvml` stays green off hardware.
- Two more fixture captures, each with its own golden file. The first,
  `h100-default-20260729`, is an H100 in **Default mode** under a real CUDA
  kernel: the original capture was MIG-partitioned for its whole life, so "what
  does an unpartitioned card report" was answered by a hand-written
  `nvidia-smi -L` line, and "does a real utilization reading survive the
  parser" could not be answered at all — an idle card reads `0`, which is
  indistinguishable from a parser wrongly turning an absent `[N/A]` into a
  zero. This card was pinned at 100%. The second, `h100-mig-20260729`, is **the
  same card** forty minutes later with MIG on, which makes the mode the only
  variable between them, and carries a profile string (`1g.10gb`) the parser
  had not seen. A third, `h100-mig-mixed-20260729`, is that card partitioned
  three ways — two profiles, one GPU instance subdivided into a compute
  instance, three processes of which two share a command and one has had its
  binary deleted — which pins a compute-instance profile spelling, profiles
  staying with their own instance UUIDs, per-command summing, and the
  deleted-binary name. The H100's `/proc` was captured too and deliberately not kept:
  same image, same disks, identical `meminfo` fields, fewer interfaces than the
  H200 tree — a second copy of covered ground rather than coverage.

### Fixed

Three defects in the NVML path, all found by running it **on hardware** for the
first time — an H100 80GB on 2026-07-29 and an H200 141GB on 2026-07-30, driver
580.173.02, in Default and MIG mode. None was reachable from a fixture; each now
has a test that fails if it returns.

- **`prickle_gpu_memory_used_bytes` was 480 MiB too high from `prickle-nvml`.**
  It bound `nvmlDeviceGetMemoryInfo`, whose `used` is `total - free` and so
  includes memory the driver reserves; `nvidia-smi` reports the `_v2` number,
  which excludes it. The two artifacts therefore disagreed about the same card
  by half a gigabyte — enough to move every memory panel and capacity alert
  depending on which binary was deployed. Now binds
  `nvmlDeviceGetMemoryInfo_v2`, falling back to the original only on drivers too
  old to publish it, which are the drivers whose `nvidia-smi` reports the old
  accounting anyway.
- **The `profile` label on `prickle_gpu_mig_info` differed between the two
  sources**, and every attempt to derive it from what NVML reports about an
  instance was wrong on some card. It now comes from the driver: the *compute*
  instance's profile name, which is the same string `nvidia-smi -L` prints.
  **Operator impact:** a dashboard or recording rule filtering on `profile`
  matched only one of the two artifacts. Four rounds on hardware to get there,
  each ruling out a plausible rule:
    - memory alone gave `10gb` against `-L`'s `1g.10gb`;
    - memory plus the GPU-instance slice count was right on an H100 and wrong
      for **every profile an H200 offers** — measured on one, with the lookup
      disabled: `1g.16gb` for `1g.18gb`, `1g.33gb` for `1g.35gb`, `3g.70gb`
      for `3g.71gb`. NVIDIA names a profile after a share of the card's
      advertised 141 GB, which NVML never reports;
    - the GPU instance's own profile name gave `1g.10gb+me` where `-L` says
      `1g.10gb`, because `-L` names the compute instance, not the GPU instance;
    - the compute instance's profile name matches `-L` in every configuration
      tested: on an H100 plain, media-engine, and a `3g.40gb` subdivided into
      `1c` and `2c`; on an H200 three profiles at once and a single
      `7g.141gb` instance spanning the whole card.

  A memory-derived fallback remains for a driver that declines the lookup, and
  the hardware test fails on any card where it disagrees with `nvidia-smi`.
- **`prickle diagnose` reported `NVML source is closed` on hosts where NVML
  worked.** The library handle is process-global; diagnose builds a GPU
  collector to describe the live source, closes it, and then builds the real
  one, which was handed the same already-closed source. The load is now
  reference-counted and re-established after a full release. A failed load is
  still never retried, which is what SPEC.md §Collectors' "attempt once at
  startup" protects.

Also fixed, in the same pass:

- A claim in `internal/collector/gpu/testdata/README.md` that the `mig -lgi`
  and `nvidia-smi -L` listings came back in *opposite* orders on the H100. They
  did not: both list GPU instance 11 before 13. What ran in a different order
  was the instance *creation*. The decision that claim was offered in support
  of — never pair the two listings positionally — is unchanged and rests on its
  original ground, that nothing captured joins a MIG UUID to a GI ID. The
  capture is now committed as `h100-mig-20260729` so the corrected statement,
  and the per-instance memory figures quoted beside it, can be rechecked
  against the output they came from.

- `prickle diagnose` no longer says "On a host with no NVIDIA GPU this is
  expected" after an operator *forced* an unavailable source with
  `-collector.gpu.nvidia-source`. On a host that plainly has a GPU, that line
  answered a question nobody asked.
- `scripts/capture-fixtures.sh` prefers an `nvcc`-built kernel over its
  `gcc` + embedded-PTX spinner, whose JIT fails on CUDA 13 drivers and degrades
  *silently* to a context-only load. The old chain produced a capture that
  looked complete with `utilization.gpu` reading `0` — the one value a GPU
  fixture must never be ambiguous about. `check` and `capture` now report a
  numeric `0` alongside a resident compute process as a gap; a literal `[N/A]`
  is not flagged, because under MIG it is the correct answer.

### Notes

- **AMD is not implemented.** It is Phase 3 scope, but every captured host is
  NVIDIA-only: there is no `gpu_busy_percent`, no `mem_info_vram_*`, no `hwmon`
  tree and no `drm-*` fdinfo to develop against, and SPEC.md §Testing rules
  forbids inventing a sysfs layout. An AMD host reports no GPU metrics at all.
  Closing this needs a capture from such a host with a ROCm workload running.
- **Intel is no longer Phase 3 scope.** SPEC.md §Collectors dropped it: no
  capture host is obtainable, and scope that is never going to arrive is worse
  than a stated limit — it promises metrics an Intel host was always going to
  return none of. Nothing was deleted to do this. Intel reads through the same
  DRM fdinfo path the AMD collector needs anyway, and `capture-fixtures.sh`
  still captures it, so reopening the decision costs a fixture tree rather than
  a redesign.
- **The NVML path has now run on hardware** — an H100 80GB and an H200 141GB,
  driver 580.173.02, in Default and MIG mode — and its output was diffed
  against the same card's `nvidia-smi` source. Two card classes matter for more
  than coverage: the C struct layouts this build declares are only known not to
  be H100-shaped by accident because a second card agrees with them. That is what SPEC.md §Testing rules means by the two
  sources having to agree, and it took three fixes to be true (see Fixed). It
  remains un-fixture-testable: a C call is not a file read, so the assertion
  re-runs only where a GPU is present. What is still unproven for both sources
  is a **multi-GPU host** — every capture so far is a single card.
- **An absent metric means the driver would not say.** `utilization_ratio`
  vanishes for the whole card once MIG is enabled — the driver reports `[N/A]`,
  verified on H200 / driver 580 and present in the fixture. Reporting zero there
  would read as an idle GPU and fire idle-capacity alerts across a MIG fleet.
- **MIG instances are their own families.** `prickle_gpu_mig_memory_used_bytes`
  rather than a `mig_uuid` label on the card's family, because an instance's
  memory is a partition of its parent's and one family holding both would
  double-count under `sum()`.
- **The `nvidia-smi` source cannot attribute a process to a MIG instance.**
  `--query-compute-apps` returns the parent GPU's UUID for a MIG-resident
  process, so those series carry `gpu_uuid` and no `mig_uuid`. Coarse, not
  wrong. NVML can do better; nothing else can.
- **No PID reaches the output.** The compute-apps CSV carries one, and it is
  discarded at the parse boundary — the snapshot type has no field for it, so
  the guarantee is structural rather than a promise. Process series key on
  `command`, never on the truncated and forgeable `comm`.
- Per-MIG memory and utilization are absent from the `nvidia-smi` source: no CSV
  query publishes them, and the human-readable table is not parsed. GPU-instance
  and compute-instance IDs are exposed by neither source, because nothing
  captured joins a MIG UUID to those IDs and a label only NVML could fill would
  break the identical-output requirement.

## [0.2.0] — 2026-07-28

Phase 2: the container collector. Nothing in Phase 1's output changes — no
metric was renamed, no label added to an existing series — so a Prometheus
already scraping 0.1.x keeps every rule and dashboard it has. What arrives is a
new `prickle_container_*` namespace and three new flags.

### Added

- **Container collector**, walking the cgroup v2 tree and reporting CPU (with
  quota and throttling), memory (usage, the four limit files, nine `memory.stat`
  fields, page faults), block I/O per device, process counts, and per-cgroup
  PSI. About 25 series per container. Identity comes from the directory names
  the runtimes write — `docker-<hex>.scope`, `cri-containerd-<hex>.scope`,
  `crio-<hex>.scope`, and the pod slices under `kubepods.slice`.
- `prickle_container_info`, the companion gauge carrying the runtime, the
  Kubernetes QoS class, and — with Docker enrichment on — the container's name
  and image. Join it with `group_left`; none of those attributes appears on a
  hot series.
- **`-collector.container.docker-socket`**, the optional enrichment path from
  SPEC.md §Collectors: one GET request per pass for names and images. Off by
  default — the exporter opens no socket nobody asked it to open — and bounded
  by `-collector.container.docker-timeout` (2s), because a wedged daemon must
  cost the names and not the metrics.
- **`-collector.container`** (default `true`) to switch the whole walk off.
- `prickle diagnose` gained a Phase 2 section: whether the cgroup root can be
  walked, how many containers were found and under which runtimes, and whether
  Docker enrichment is on and working. When it finds none, it names the three
  causes worth checking rather than leaving you with an empty scrape.

### Notes

- **`pod` carries the pod's UID, not its name, and `namespace` is not emitted.**
  A cgroup directory name holds a UID and nothing else — the kernel stores no
  pod name or namespace anywhere in the tree — so those are the honest values.
  The UID is unescaped from systemd's spelling
  (`6eb5044d_ef2e_49d1_a9cc_28f4e3fe88a3`) back to the one `kubectl` reports.
  Resolving a name needs a Kubernetes-aware source, which a cgroup walk is not;
  if that lands, `pod` changing meaning would be a major bump, and the label
  would gain a companion rather than change under you.
- **Only leaf containers are sampled.** The pod, QoS and root slices above them
  are walked for identity and never emitted: a `sum` over the family is the
  node's containers once, not once per level of the hierarchy. Pod totals are
  `sum by (pod)`.
- **An unset limit is an absent series.** `memory.max` reading `max` produces no
  `prickle_container_memory_limit_bytes` sample rather than the kernel's
  9.2-exabyte sentinel, so a usage/limit ratio silently drops unconstrained
  containers instead of reporting them as 0% full.
- `cgroup.procs` is never read. It is the one file in a container's cgroup that
  contains PIDs, SPEC.md §Metrics contract forbids those everywhere, and a test
  fails if any source file starts to read it.
- Known coverage gaps, recorded in full in
  [internal/collector/container/testdata/README.md](internal/collector/container/testdata/README.md#coverage-gaps):
  CRI-O and Guaranteed-pod directory names are unit-tested on the name parse
  only, and the **cgroupfs-driver layouts** (`/sys/fs/cgroup/docker/<hex>/` and
  `kubepods/besteffort/pod<uid>/<hex>`) are **not implemented** — a host using
  them reports no containers. Closing those needs a capture from a host
  configured that way; no format was guessed at in the meantime.

## [0.1.1] — 2026-07-27

### Fixed

- Release binaries after the first architecture were built from a working tree
  that the build's own staging directory had made dirty, so Go stamped them
  `vcs.modified=true` and `vX.Y.Z+dirty`. In 0.1.0 the amd64 and arm64
  artifacts therefore disagreed about whether they came from the tag. **No
  change to the exporter, its behavior, or its output** — the 0.1.0 binaries
  run identically and their checksums and attestations are valid. Only the
  recorded provenance was wrong. If you took the 0.1.0 arm64 artifact and care
  about provenance, re-download.

  The release workflow now asserts the property directly, per architecture,
  and fails on a missing stamp as well as a dirty one.

## [0.1.0] — 2026-07-27

Phase 1: the host collector, and the machinery underneath it.

### Added

- **Host collector** reading `/proc/stat`, `meminfo`, `diskstats`, `net/dev`,
  `loadavg`, `pressure/{cpu,memory,io}`, and `mounts` + `statfs`. Roughly 280
  series on a plain host. Every parser was developed against a captured H200
  fixture tree, and the rendered output is checked against a golden file.
- **Hand-written Prometheus text exposition** (`internal/exposition`), with no
  `prometheus/client_golang`. Output is gated on `promtool check metrics`.
- **Sampler** that polls collectors on an interval and swaps a fully rendered
  buffer under a mutex, so `/metrics` serves the last completed render and a
  slow collector can never stall a scrape.
- **Self-instrumentation**: `prickle_build_info`,
  `prickle_render_timestamp_seconds`, `prickle_collector_duration_seconds`,
  `prickle_collector_errors_total`, `prickle_collector_success`.
- **`prickle diagnose`**, reporting what the exporter can actually read on a
  host — by reading it, since procfs files report size 0 and permission
  failures surface on open, not on stat. Detects cgroup v1 and says so.
- **Configurable filesystem roots** (`internal/fsroot`) behind `-path.rootfs`
  and friends, so tests and containers point at a tree rather than at `/`.
- Aggregate CPU time is always exposed; per-core series are opt-in behind
  `-collector.cpu.per-core` as a separate family, so default cardinality does
  not scale with core count.
- `ci/check.sh`, the entire pre-commit checklist in one command, run unchanged
  by CI.
- `ci/check-port-registration.sh`, a watchdog on the upstream Prometheus
  default-port registration for `:10047`.

### Notes

- Linux only, and **cgroup v2 only**. v1 and hybrid hosts are out of scope.
- Containers (Phase 2) and GPUs (Phase 3) are specified but not implemented.
  `prickle diagnose` says so rather than reporting an empty GPU section as
  though it were a healthy one.
- `prickle-nvml` (SPEC.md §Distribution) is not released yet: no `//go:build
  nvml` source exists until Phase 3, so the artifact would be an identical copy
  of `prickle` under a name promising NVML support.
- `/proc/loadavg`'s fourth and fifth fields are not exposed. The fifth is a
  PID, and SPEC.md §Metrics contract forbids PIDs everywhere.

[Unreleased]: https://github.com/starkdrift/prickle-exporter/compare/v0.7.1...HEAD
[0.7.1]: https://github.com/starkdrift/prickle-exporter/compare/v0.7.0...v0.7.1
[0.7.0]: https://github.com/starkdrift/prickle-exporter/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/starkdrift/prickle-exporter/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/starkdrift/prickle-exporter/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/starkdrift/prickle-exporter/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/starkdrift/prickle-exporter/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/starkdrift/prickle-exporter/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/starkdrift/prickle-exporter/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/starkdrift/prickle-exporter/releases/tag/v0.1.0

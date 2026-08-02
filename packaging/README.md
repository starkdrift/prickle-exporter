# Packaging

Everything SPEC.md §Distribution asks for, and what each piece was tested
against rather than assumed to work.

## systemd

`systemd/prickle.service` and `systemd/prickle-nvml.service`.

```sh
install -m 0755 prickle /usr/local/bin/prickle
install -m 0644 systemd/prickle.service /etc/systemd/system/
command -v restorecon >/dev/null && restorecon /etc/systemd/system/prickle.service
systemctl daemon-reload && systemctl enable --now prickle
```

**The `restorecon` line matters on SELinux hosts**, and skipping it fails
opaquely: a unit file that keeps the wrong label is invisible to systemd, which
says `Unit prickle.service not found` while the file sits there in `ls`. No
denial is logged and SELinux is never mentioned. Found by sweeping the released
binary across every base image — AlmaLinux 8 labelled the copied file
`default_t` instead of `systemd_unit_file_t`, while Rocky 8, equally Enforcing,
labelled the identical copy correctly. `restorecon` is a no-op where the label
is already right and does not exist on Debian or Ubuntu, hence the guard.

Both run under `DynamicUser`, an empty `CapabilityBoundingSet`,
`ProtectSystem=strict` and a syscall allow-list. Verified on live hosts: uid in
the 63xxx range, **`CapEff` `0000000000000000`**, exposure 1.5 (`prickle`) and
1.8 (`prickle-nvml`) from `systemd-analyze security`.

The two units differ in exactly one directive. `prickle-nvml` cannot use
`PrivateDevices`, because NVML opens `/dev/nvidiactl` and `/dev/nvidia<N>`
directly; those are allowed back read-write, named one per line so a card added
later is not automatically visible. **Nothing else is relaxed** — including
`MemoryDenyWriteExecute`, which an earlier draft disabled on the common
assumption that the NVIDIA driver needs W+X mappings. Forcing it back on and
restarting on an H100 with driver 580 still produced `source="nvml"` and a full
set of GPU series, so the assumption was wrong and the directive stays.

Two flags to know:

- `-collector.gpu.per-process` reads `/proc/<pid>/exe` for other users'
  processes, which `DynamicUser` cannot do. That flag needs `User=root` and
  `ProtectProc=default`; the shipped units are for the default configuration.
- `-collector.container.docker-socket` needs the socket's group, so add
  `SupplementaryGroups=docker`.

## Docker

A `scratch` image — 3.0 MB, no package manager, nothing to patch.

```sh
docker build -t prickle-exporter .
docker run -d --name prickle --network=host \
  -v /proc:/host/proc:ro -v /sys:/host/sys:ro \
  prickle-exporter -path.rootfs=/host
```

`-path.rootfs=/host` is not optional. Without it the exporter faithfully reports
the metrics of its own container.

`--pid=host` is needed only for `-collector.gpu.per-process`.

## Compose quickstart

`compose/` brings up prickle, Prometheus and a Grafana with the datasource and
dashboards already provisioned.

```sh
cd packaging/compose && docker compose up -d
# Grafana  http://localhost:3000   (anonymous admin, no login)
# Prometheus http://localhost:9090
```

Verified end to end on a VM: three containers running, 395 series scraped,
`up{job="prickle"} == 1` in Prometheus, and the datasource present in Grafana's
API without anyone importing anything.

It is a demonstration, not a deployment — Grafana runs with anonymous admin so
the quickstart has no password step. Do not put it on a network you do not own.

## CI trust model

Workflows run for **the maintainer and Dependabot only**. The guard is in
`ci.yml` and `codeql.yml` rather than in repository settings, because a
condition in a file can be read back and a setting cannot:

```yaml
if: >-
  github.event_name != 'pull_request' ||
  (github.event.pull_request.head.repo.full_name == github.repository &&
   (github.event.pull_request.author_association == 'OWNER' ||
    github.event.pull_request.user.login == 'dependabot[bot]'))
```

`author_association == 'OWNER'` rather than a literal username, because
usernames can change and a stale one would silently skip the maintainer's own
PRs. `pull_request.user.login` is the author; `github.actor` is whoever pushed
last, which on a `synchronize` event is somebody else.

The trigger is `pull_request`, **never `pull_request_target`**. The former runs
the PR's own code with a read-only token and no secrets; the latter would run it
with write access in the base repo's context, which is how most Actions
compromises happen. Nothing here needs a secret, so the safe trigger is also the
sufficient one.

**Two settings this cannot do, and a read-only token cannot verify:**

1. *Settings → Actions → General → Fork pull request workflows* →
   **"Require approval for all outside collaborators."** The repo is public with
   forking enabled, so this is the second layer under the guard above.
2. **Branch protection on `main` requiring the `check` status.** This one is
   load-bearing: the guard *skips* the job for an untrusted PR, and a skipped
   job reports no failure. The guard stops the code running; only branch
   protection stops an unverified PR being merged.

Dependabot PRs are verified but **not** auto-merged. They modify
`.github/workflows/**`, the highest-privilege files here, and a green check
proves the new action version builds — not that it is uncompromised. SHA pinning
plus a human reading the diff is the control; auto-merge would remove exactly
that.

## Grafana dashboards

Four, in `grafana/dashboards/`, provisioned into a **Prickle** folder by the
compose quickstart: **GPU Tenancy**, **Node Overview**, **Container Resources**
and **Fleet Health**.

They are generated by `scripts/make-dashboards.py`, which is the source of
truth; the JSON is checked in so a clone works without a build step, and
`ci/check.sh` re-runs the generator and fails if the two have drifted. Four
dashboards sharing twelve template variables is not something to maintain by
hand.

Every dashboard carries the same variables — a **textbox paired with a
dropdown** for each identity label in SPEC.md §Metrics contract's closed set:

- Type into `node contains` and the `node` dropdown filters to matches. Input is
  wrapped as `.*input.*`, so `gpu-` finds `node-gpu-04`. Empty means everything,
  so the dashboards are useful before anyone types.
- The dropdowns **chain**: choosing a node narrows the namespaces, which narrow
  the pods, which narrow the containers. A dropdown never offers a value that
  cannot exist given what is already selected.
- Per-process GPU panels filter on **`command`** — the basename of the exe
  symlink. There is no PID anywhere, in a query or a label, because SPEC.md
  §Metrics contract forbids one.

Verified against the running quickstart, not just linted: all four load into the
folder with no provisioning errors, and their queries return series.

Three panels say something a blank graph would not:

- **GPU utilization is absent, not zero, under MIG.** The driver reports
  `[N/A]`; reporting zero would fire idle-capacity alerts across a MIG fleet.
- **Host pressure is empty on the whole RHEL family.** Alma, Rocky and CentOS
  Stream ship PSI compiled in but disabled — it needs `psi=1` at boot.
- **`runtime` is empty on a cgroupfs-driver Kubernetes node.** Those directory
  names do not encode a runtime, so it is left unknown rather than guessed.

## Helm chart

`helm/prickle-exporter` — a DaemonSet, a headless Service, and an optional
ServiceMonitor.

```sh
helm install prickle packaging/helm/prickle-exporter -n monitoring --create-namespace
helm install prickle packaging/helm/prickle-exporter -n monitoring \
  --set nvml.enabled=true --set serviceMonitor.enabled=true
```

**No ServiceAccount and no RBAC anywhere in the chart.** prickle reads the
node's filesystem and never the Kubernetes API — pod identity comes out of
cgroup directory names — so nothing needs a token and
`automountServiceAccountToken: false`. A monitoring agent that cannot read the
API is a smaller thing to trust.

The container runs `runAsNonRoot`, `readOnlyRootFilesystem`, all capabilities
dropped, `seccompProfile: RuntimeDefault`. Only `collectors.perProcess=true`
relaxes that — it needs root and `hostPID` to read another process's `exe`
symlink, which is why it is off by default.

`nvml.enabled=true` selects the `-nvml` image and mounts the driver libraries
read-only. The Service is **headless on purpose**: every pod reports about a
different node, so a ClusterIP would hand a scraper one node at random and hide
the rest.

`serviceMonitor.enabled=true` **fails the install** with a clear message if the
`monitoring.coreos.com/v1` CRD is absent, rather than silently rendering nothing
and leaving you wondering why Prometheus never scraped.

Two things learned by installing it on a real two-node cluster rather than
`helm template`:

- **hostNetwork means the pod binds the node's own `:10047`.** If that node also
  runs prickle from the systemd unit, the pod CrashLoopBackOffs with `address
  already in use`. Run one or the other. The chart's NOTES says so.
- **`kubectl exec … -- /prickle diagnose` needs `-path.rootfs=/host`.** Exec
  starts a fresh process that does not inherit the DaemonSet's args, so without
  it the diagnostic reports on the container's own `/proc` and announces "no
  containers found" on a node running eighteen.

Verified: both pods Ready, each reporting its own node via the downward API
(`node="cp1"` / `node="gpu1"`, 873 and 502 series), and the container collector
finding 18 containerd containers through the host mount.

## Container images and air-gapped installs

Published to `ghcr.io/starkdrift/prickle-exporter` from `v0.5.0` on, as
multi-architecture manifest lists over `linux/amd64` and `linux/arm64`.

**`0.5.0` through `0.7.0` have been withdrawn.** They carried defects worth not
shipping, so their images no longer pull and their release binaries are no
longer attached, even though both were once published. That was a one-time
cleanup of early pre-1.0 builds rather than a retention policy: nothing is
removed on a schedule, and from `1.0` onward a published image or tarball stays
published. Before `1.0`, a release found to be defective may still be withdrawn
the same way.

Release pages and their notes are never deleted, and no git tag is ever removed,
so the source of any version remains buildable. Mirroring is still worth doing
if you depend on a particular version — that is what the `skopeo copy` below is
for.

| Tag | Contents | Base |
|---|---|---|
| `X.Y.Z`, `latest` | `prickle`, static, nvidia-smi path | `scratch`, 3.0 MB |
| `X.Y.Z-nvml` | `prickle-nvml`, NVML via `dlopen` | distroless/base, 10.8 MB |

`latest` follows the static image only, and never moves for a prerelease.

The nvml image **does not contain a driver library**, and cannot: `libnvidia-ml.so.1`
belongs to the host's driver and has to match it. Mount that one file in and the
image finds it through `LD_LIBRARY_PATH=/nvidia`, which is what `nvml.enabled=true`
does. Mount the *file*, never its directory — the directory holds 855 files on a
Debian host including `libc.so.6`, and mounting it replaces the container's own
libc to solve a one-file problem. Verified on an H100 with driver 580: that file
plus `/dev/nvidiactl` and `/dev/nvidia0` gives `live source: nvml`.

### Mirroring into an air-gapped registry

The images are digest-addressable manifest lists, so a plain copy works — no
rebuild, and the digest you mirror is the digest you verify:

```sh
skopeo copy --all \
  docker://ghcr.io/starkdrift/prickle-exporter:0.7.1 \
  docker://registry.internal/prickle-exporter:0.7.1
# or
crane copy ghcr.io/starkdrift/prickle-exporter:0.7.1 registry.internal/prickle-exporter:0.7.1
```

`--all` matters: without it you copy one architecture and the manifest list is
lost, so the mirror works on the machine you ran it from and fails on the other.

Then point the chart at the mirror:

```sh
helm install prickle packaging/helm/prickle-exporter -n monitoring \
  --set image.repository=registry.internal/prickle-exporter \
  --set image.tag=0.7.1@sha256:<digest>
```

Pinning by digest is the point of a mirror: it survives a tag being re-pushed
inside the perimeter, which is the failure an air-gapped deployment is usually
trying to rule out.

Provenance is attested to the manifest-list digest and pushed to the registry
alongside the image, so it can be verified after mirroring:

```sh
gh attestation verify oci://registry.internal/prickle-exporter:0.7.1 \
  --repo starkdrift/prickle-exporter
```

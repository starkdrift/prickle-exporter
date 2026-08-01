# Packaging

Everything SPEC.md §Distribution asks for, and what each piece was tested
against rather than assumed to work.

## systemd

`systemd/prickle.service` and `systemd/prickle-nvml.service`.

```sh
install -m 0755 prickle /usr/local/bin/prickle
install -m 0644 systemd/prickle.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now prickle
```

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

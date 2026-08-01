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

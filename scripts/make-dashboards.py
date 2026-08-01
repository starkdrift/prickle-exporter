#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Generate the four Grafana dashboards SPEC.md §Distribution ships.

This file is the source of truth; packaging/grafana/dashboards/*.json is
generated from it and checked in, because Grafana loads JSON and nobody wants a
build step between a clone and a working quickstart. ci/check.sh re-runs this
and fails if the tree differs, so the two cannot drift.

Four dashboards of hand-written JSON would be some thousands of lines with the
same eleven template variables copied into each. The variables are the part
SPEC.md is specific about, so they are defined once here and shared.

    python3 scripts/make-dashboards.py            # write
    python3 scripts/make-dashboards.py --check    # exit 1 if the tree differs
"""

import argparse
import json
import pathlib
import sys

OUT = pathlib.Path(__file__).resolve().parent.parent / "packaging" / "grafana" / "dashboards"

# SPEC.md §Metrics contract's closed set of identity labels, plus `command`.
# Each gets a textbox to type in and a dropdown that the textbox filters —
# "paired textbox + chained-dropdown template variables per identity label".
#
# The chain is deliberate: picking a node narrows the namespaces, which narrows
# the pods, which narrows the containers. Each level's query carries the levels
# above it, so a dropdown never offers a value that cannot exist given what is
# already selected.
IDENTITY = [
    # (name, label, metric the values come from, labels that constrain it)
    ("node",      "node",      "prickle_collector_series", []),
    ("namespace", "namespace", "prickle_container_info",   ["node"]),
    ("pod",       "pod",       "prickle_container_info",   ["node", "namespace"]),
    ("container", "container", "prickle_container_info",   ["node", "namespace", "pod"]),
    ("gpu",       "gpu_uuid",  "prickle_gpu_info",         ["node"]),
    ("mig",       "mig_uuid",  "prickle_gpu_mig_info",     ["node", "gpu_uuid"]),
]


def selector(constraints, extra=""):
    """Build a PromQL label selector from the chained variables."""
    parts = [f'{lbl}=~"${name}"' for name, lbl, _, _ in IDENTITY
             for c in [lbl] if c in constraints]
    if extra:
        parts.append(extra)
    return "{" + ",".join(parts) + "}" if parts else ""


def variables():
    """The templating list: a datasource, then a textbox+dropdown per label."""
    out = [{
        "name": "DS", "label": "Data source", "type": "datasource",
        "query": "prometheus", "current": {}, "hide": 0, "refresh": 1,
    }]
    for name, label, metric, constraints in IDENTITY:
        # The textbox. SPEC.md §Distribution: input is wrapped as .*<input>.*
        # so typing `gpu-` finds `node-gpu-04`. An empty box wraps to `.**`,
        # which matches everything — so the default state is "no filter"
        # rather than "nothing selected", and the dashboard is useful before
        # anyone types.
        out.append({
            "name": f"{name}_search", "label": f"{label} contains",
            "type": "textbox", "query": "", "hide": 0,
            "current": {"text": "", "value": ""},
            "description": f"Substring match on {label}. Wrapped as .*input.* — leave empty for all.",
        })
        # The dropdown, filtered by the textbox and chained to its parents.
        out.append({
            "name": name, "label": label, "type": "query",
            "datasource": {"type": "prometheus", "uid": "${DS}"},
            "query": {"query": f"label_values({metric}{selector(constraints)}, {label})",
                      "refId": f"{name}-values"},
            "regex": f'/.*${name}_search.*/',
            "multi": True, "includeAll": True, "allValue": ".*",
            "current": {"text": ["All"], "value": ["$__all"]},
            "refresh": 2, "sort": 1, "hide": 0,
        })
    return out


def pod_qualified(expr):
    """Label a container series `<pod>/<container>`, or just `<container>`
    when it is not in a pod.

    Presentation, so it lives here and not in the exporter: `pod` is already on
    every container series that has one, and adding a pre-joined label would put
    redundant bytes on every sample of every host to save a function call in
    four panels.

    label_join alone would render `/abc123` for a plain Docker container, whose
    pod label is empty. The outer label_replace strips that leading separator,
    and leaves the label untouched when the regexp does not match — which is
    exactly the Kubernetes case, where the value starts with the pod UID.
    """
    return (f'label_replace(label_join({expr}, "display", "/", "pod", "container"), '
            f'"display", "$1", "display", "^/(.+)$")')


def ts(title, exprs, gp, unit=None, desc=None, stack=False):
    """A timeseries panel. exprs is a list of (promql, legend)."""
    p = {
        "type": "timeseries", "title": title, "gridPos": gp,
        "datasource": {"type": "prometheus", "uid": "${DS}"},
        "targets": [{"expr": e, "legendFormat": l, "refId": chr(65 + i),
                     "datasource": {"type": "prometheus", "uid": "${DS}"}}
                    for i, (e, l) in enumerate(exprs)],
        "fieldConfig": {"defaults": {"custom": {
            "lineWidth": 1, "fillOpacity": 12 if not stack else 40,
            "showPoints": "never",
            "stacking": {"mode": "normal" if stack else "none"},
        }}, "overrides": []},
        "options": {"legend": {"displayMode": "table", "placement": "bottom",
                               "calcs": ["lastNotNull", "max"]},
                    "tooltip": {"mode": "multi", "sort": "desc"}},
    }
    if unit:
        p["fieldConfig"]["defaults"]["unit"] = unit
    if desc:
        p["description"] = desc
    return p


def stat(title, expr, gp, unit=None, desc=None, text=False):
    p = {
        "type": "stat", "title": title, "gridPos": gp,
        "datasource": {"type": "prometheus", "uid": "${DS}"},
        "targets": [{"expr": expr, "refId": "A", "instant": True,
                     "datasource": {"type": "prometheus", "uid": "${DS}"}}],
        "fieldConfig": {"defaults": {"unit": unit or "none"}, "overrides": []},
        "options": {"reduceOptions": {"calcs": ["lastNotNull"]},
                    "textMode": "value_and_name" if text else "auto",
                    "colorMode": "value", "graphMode": "area"},
    }
    if desc:
        p["description"] = desc
    return p


def table(title, expr, gp, desc=None):
    return {
        "type": "table", "title": title, "gridPos": gp,
        "description": desc,
        "datasource": {"type": "prometheus", "uid": "${DS}"},
        "targets": [{"expr": expr, "refId": "A", "instant": True, "format": "table",
                     "datasource": {"type": "prometheus", "uid": "${DS}"}}],
        "fieldConfig": {"defaults": {}, "overrides": []},
        "options": {"showHeader": True},
        "transformations": [{"id": "organize", "options": {
            "excludeByName": {"Time": True, "__name__": True, "job": True,
                              "instance": True, "Value": True}}}],
    }


def row(title, y):
    return {"type": "row", "title": title, "gridPos": {"h": 1, "w": 24, "x": 0, "y": y},
            "collapsed": False, "panels": []}


def dashboard(uid, title, description, panels):
    return {
        "uid": uid,
        "title": title,
        "description": description,
        "tags": ["prickle", "starkdrift"],
        "editable": True,
        "timezone": "browser",
        "schemaVersion": 39,
        "refresh": "30s",
        "time": {"from": "now-1h", "to": "now"},
        "templating": {"list": variables()},
        "panels": panels,
        "links": [{
            "title": "prickle-exporter", "type": "link", "icon": "external link",
            "url": "https://github.com/starkdrift/prickle-exporter",
            "targetBlank": True, "tooltip": "Starkdrift · prickle-exporter",
        }],
    }


# --------------------------------------------------------------------------
# 1. GPU Tenancy — which tenant is on which accelerator.
# --------------------------------------------------------------------------
def gpu_tenancy():
    g = selector(["node", "gpu_uuid"])
    mig = selector(["node", "gpu_uuid", "mig_uuid"])
    p = [
        row("Cards", 0),
        stat("GPUs", f"count(prickle_gpu_info{g})", {"h": 4, "w": 4, "x": 0, "y": 1},
             desc="Cards matching the current filters."),
        stat("MIG instances", f"count(prickle_gpu_mig_info{mig}) or vector(0)",
             {"h": 4, "w": 4, "x": 4, "y": 1},
             desc="Zero on unpartitioned cards. MIG instances are their own "
                  "families, never a label on the card's series, so a sum over "
                  "cards does not double-count them."),
        stat("Memory used", f"sum(prickle_gpu_memory_used_bytes{g})",
             {"h": 4, "w": 4, "x": 8, "y": 1}, unit="bytes"),
        stat("Power", f"sum(prickle_gpu_power_watts{g})",
             {"h": 4, "w": 4, "x": 12, "y": 1}, unit="watt"),
        stat("Hottest card", f"max(prickle_gpu_temperature_celsius{g})",
             {"h": 4, "w": 4, "x": 16, "y": 1}, unit="celsius"),
        stat("Live source", f"count by (source) (prickle_gpu_nvidia_source_info)",
             {"h": 4, "w": 4, "x": 20, "y": 1}, text=True,
             desc="nvml or smi. The two must report identically; this says "
                  "which one answered."),

        row("Utilization", 5),
        ts("GPU utilization",
           [(f"prickle_gpu_utilization_ratio{g} * 100", "{{gpu_uuid}}")],
           {"h": 8, "w": 12, "x": 0, "y": 6}, unit="percent",
           desc="ABSENT, not zero, once MIG is enabled — the driver reports "
                "[N/A] and reporting zero would fire idle-capacity alerts "
                "across a MIG fleet. A gap here on a MIG card is correct."),
        ts("Memory used per card",
           [(f"prickle_gpu_memory_used_bytes{g}", "{{gpu_uuid}}"),
            (f"prickle_gpu_memory_total_bytes{g}", "{{gpu_uuid}} total")],
           {"h": 8, "w": 12, "x": 12, "y": 6}, unit="bytes"),

        row("MIG partitions", 14),
        ts("MIG memory used",
           [(f"prickle_gpu_mig_memory_used_bytes{mig}", "{{mig_uuid}}")],
           {"h": 8, "w": 12, "x": 0, "y": 15}, unit="bytes",
           desc="A separate family from the card's memory: an instance's "
                "memory is a partition of its parent's, and one family holding "
                "both would double-count under sum()."),
        table("MIG topology",
              f"prickle_gpu_mig_info{mig}", {"h": 8, "w": 12, "x": 12, "y": 15},
              desc="profile comes from the driver's compute-instance name, the "
                   "same string nvidia-smi -L prints. Deriving it from memory "
                   "was wrong on every H200 profile."),

        row("Per-process (opt-in)", 23),
        ts("GPU memory by command",
           [(f"sum by (command, gpu_uuid) (prickle_gpu_process_memory_bytes{g})",
             "{{command}} @ {{gpu_uuid}}")],
           {"h": 8, "w": 24, "x": 0, "y": 24}, unit="bytes", stack=True,
           desc="Requires -collector.gpu.per-process. Keyed on `command`, the "
                "basename of the exe symlink — never a PID, which SPEC.md "
                "§Metrics contract forbids anywhere. Empty unless the flag is "
                "set. Under the nvidia-smi source these carry gpu_uuid only: "
                "that source cannot attribute a process to a MIG instance."),
    ]
    return dashboard("prickle-gpu-tenancy", "GPU Tenancy",
                     "Which workload is on which accelerator. Starkdrift · prickle-exporter.", p)


# --------------------------------------------------------------------------
# 2. Node Overview
# --------------------------------------------------------------------------
def node_overview():
    n = selector(["node"])
    p = [
        row("Load", 0),
        stat("Nodes", f"count(count by (node) (prickle_collector_series{n}))",
             {"h": 4, "w": 4, "x": 0, "y": 1}),
        stat("CPU busy", f"100 - (avg(rate(prickle_host_cpu_seconds_total{selector(['node'], 'mode=\"idle\"')}[5m])) * 100)",
             {"h": 4, "w": 5, "x": 4, "y": 1}, unit="percent"),
        stat("Memory used", f"sum(prickle_host_memory_mem_total_bytes{n} - prickle_host_memory_mem_available_bytes{n})",
             {"h": 4, "w": 5, "x": 9, "y": 1}, unit="bytes"),
        stat("Load 1m", f"max(prickle_host_load1{n})", {"h": 4, "w": 5, "x": 14, "y": 1}),
        stat("Scrape age", f"max(time() - prickle_render_timestamp_seconds{n})",
             {"h": 4, "w": 5, "x": 19, "y": 1}, unit="s",
             desc="A scrape serves the last completed render, so this is the "
                  "age of the data, not of the request. Rising means the "
                  "sampler is behind."),

        row("CPU and memory", 5),
        ts("CPU time by mode",
           [(f"sum by (mode) (rate(prickle_host_cpu_seconds_total{n}[5m]))", "{{mode}}")],
           {"h": 8, "w": 12, "x": 0, "y": 6}, unit="percentunit", stack=True,
           desc="Aggregate across cores. Per-core series are opt-in behind "
                "-collector.cpu.per-core as a separate family, so default "
                "cardinality does not scale with core count."),
        ts("Memory",
           [(f"prickle_host_memory_mem_total_bytes{n}", "total"),
            (f"prickle_host_memory_mem_available_bytes{n}", "available"),
            (f"prickle_host_memory_cached_bytes{n}", "cached")],
           {"h": 8, "w": 12, "x": 12, "y": 6}, unit="bytes"),

        row("Disk, network, pressure", 14),
        ts("Disk throughput",
           [(f"sum by (device) (rate(prickle_host_disk_read_bytes_total{n}[5m]))", "{{device}} read"),
            (f"sum by (device) (rate(prickle_host_disk_written_bytes_total{n}[5m]))", "{{device}} write")],
           {"h": 8, "w": 8, "x": 0, "y": 15}, unit="Bps"),
        ts("Network throughput",
           [(f"sum by (interface) (rate(prickle_host_network_receive_bytes_total{n}[5m]))", "{{interface}} rx"),
            (f"sum by (interface) (rate(prickle_host_network_transmit_bytes_total{n}[5m]))", "{{interface}} tx")],
           {"h": 8, "w": 8, "x": 8, "y": 15}, unit="Bps"),
        ts("Pressure stall",
           [(f"sum by (resource, kind) (rate(prickle_host_pressure_stalled_seconds_total{n}[5m]))",
             "{{resource}} {{kind}}")],
           {"h": 8, "w": 8, "x": 16, "y": 15}, unit="percentunit",
           desc="EMPTY ON THE WHOLE RHEL FAMILY. Alma, Rocky and CentOS Stream "
                "ship PSI compiled in but disabled; it needs psi=1 at boot. "
                "Also absent on kernels older than 4.20. Blank here is a "
                "property of the host, not a fault."),
    ]
    return dashboard("prickle-node-overview", "Node Overview",
                     "One node's CPU, memory, disks, network and pressure. Starkdrift · prickle-exporter.", p)


# --------------------------------------------------------------------------
# 3. Container Resources
# --------------------------------------------------------------------------
def container_resources():
    c = selector(["node", "namespace", "pod", "container"])
    p = [
        row("Overview", 0),
        stat("Containers", f"count(prickle_container_info{c})", {"h": 4, "w": 5, "x": 0, "y": 1}),
        stat("Memory used", f"sum(prickle_container_memory_usage_bytes{c})",
             {"h": 4, "w": 5, "x": 5, "y": 1}, unit="bytes"),
        stat("CPU used", f"sum(rate(prickle_container_cpu_usage_seconds_total{c}[5m]))",
             {"h": 4, "w": 5, "x": 10, "y": 1}, unit="percentunit"),
        stat("Throttled now", f"count(rate(prickle_container_cpu_throttled_periods_total{c}[5m]) > 0) or vector(0)",
             {"h": 4, "w": 4, "x": 15, "y": 1},
             desc="Containers throttled in the last 5 minutes. A container can "
                  "be throttled while well under its average limit."),
        stat("Runtimes", "count by (runtime) (prickle_container_info)",
             {"h": 4, "w": 5, "x": 19, "y": 1}, text=True,
             desc="docker, containerd, crio or podman. EMPTY is correct on a "
                  "cgroupfs-driver Kubernetes node: those directory names do "
                  "not encode a runtime, so it is reported unknown rather than "
                  "guessed."),

        row("CPU", 5),
        ts("CPU usage",
           [(pod_qualified(f"sum by (pod, container) (rate(prickle_container_cpu_usage_seconds_total{c}[5m]))"),
             "{{display}}")],
           {"h": 8, "w": 12, "x": 0, "y": 6}, unit="percentunit", stack=True),
        ts("Throttling",
           [(pod_qualified(f"sum by (pod, container) (rate(prickle_container_cpu_throttled_seconds_total{c}[5m]))"),
             "{{display}} stalled"),
            (pod_qualified(f"sum by (pod, container) (rate(prickle_container_cpu_throttled_periods_total{c}[5m]))"),
             "{{display}} periods")],
           {"h": 8, "w": 12, "x": 12, "y": 6},
           desc="Throttled seconds is time the kernel withheld, not a ratio. A "
                "container hitting its quota every period shows periods and "
                "stalled time rising together."),

        row("Memory and I/O", 14),
        ts("Memory usage against limit",
           [(pod_qualified(f"sum by (pod, container) (prickle_container_memory_usage_bytes{c})"),
             "{{display}}"),
            (pod_qualified(f"sum by (pod, container) (prickle_container_memory_limit_bytes{c})"),
             "{{display}} limit")],
           {"h": 8, "w": 12, "x": 0, "y": 15}, unit="bytes",
           desc="The limit series is ABSENT for an unlimited container rather "
                "than a sentinel, on either cgroup hierarchy — so a missing "
                "limit line means unlimited, not unknown."),
        ts("Block I/O",
           [(pod_qualified(f"sum by (pod, container, device) (rate(prickle_container_io_read_bytes_total{c}[5m]))"),
             "{{display}} {{device}} r"),
            (pod_qualified(f"sum by (pod, container, device) (rate(prickle_container_io_written_bytes_total{c}[5m]))"),
             "{{display}} {{device}} w")],
           {"h": 8, "w": 12, "x": 12, "y": 15}, unit="Bps"),

        row("Identity", 23),
        table("Containers",
              f"prickle_container_info{c}", {"h": 9, "w": 24, "x": 0, "y": 24},
              desc="The companion _info gauge. Descriptive attributes live "
                   "here and never on a hot series — join with group_left. "
                   "`pod` is the pod UID; the cgroup tree has no pod name to "
                   "offer, and `namespace` is not derivable from it at all."),
    ]
    return dashboard("prickle-container-resources", "Container Resources",
                     "Per-container CPU, memory, throttling and I/O. Starkdrift · prickle-exporter.", p)


# --------------------------------------------------------------------------
# 4. Fleet Health — the exporter watching itself.
# --------------------------------------------------------------------------
def fleet_health():
    n = selector(["node"])
    p = [
        row("Exporter", 0),
        stat("Nodes reporting", f"count(count by (node) (prickle_collector_series{n}))",
             {"h": 4, "w": 5, "x": 0, "y": 1}),
        stat("Collectors failing", f"count(prickle_collector_success{n} == 0) or vector(0)",
             {"h": 4, "w": 5, "x": 5, "y": 1},
             desc="A collector can fail and still emit: partial data plus a "
                  "raised error total beats an empty family."),
        stat("Series dropped", f"sum(increase(prickle_collector_series_dropped_total{n}[1h])) or vector(0)",
             {"h": 4, "w": 5, "x": 10, "y": 1},
             desc="Non-zero means a cardinality cap fired and a collector's "
                  "tail was discarded. The default cap is far above any real "
                  "host, so this should be flat zero."),
        stat("Slowest collector", f"max(prickle_collector_duration_seconds{n})",
             {"h": 4, "w": 4, "x": 15, "y": 1}, unit="s"),
        stat("Oldest scrape", f"max(time() - prickle_render_timestamp_seconds{n})",
             {"h": 4, "w": 5, "x": 19, "y": 1}, unit="s"),

        row("Collectors", 5),
        ts("Collector duration",
           [(f"prickle_collector_duration_seconds{n}", "{{node}} {{collector}}")],
           {"h": 8, "w": 12, "x": 0, "y": 6}, unit="s",
           desc="Per-collector, per pass. A collector approaching "
                "-collector.timeout is the thing to catch before it starts "
                "being killed."),
        ts("Collector errors",
           [(f"sum by (collector) (increase(prickle_collector_errors_total{n}[5m]))", "{{collector}}")],
           {"h": 8, "w": 12, "x": 12, "y": 6},
           desc="On a GPU host using the nvidia-smi source, a steady error "
                "rate here is usually NVIDIA persistence mode being off: each "
                "nvidia-smi then re-initialises the driver and the pass "
                "overruns its deadline. `nvidia-smi -pm 1`."),

        row("Cardinality", 14),
        ts("Series per collector",
           [(f"prickle_collector_series{n}", "{{node}} {{collector}}")],
           {"h": 8, "w": 12, "x": 0, "y": 15}, stack=True,
           desc="Useful whether or not a cap is set. A step change usually "
                "means a host gained containers, changed cgroup driver, or "
                "switched container runtime — containerd gives a pod sandbox "
                "its own cgroup and CRI-O does not, so the same pods count "
                "differently."),
        ts("Series dropped",
           [(f"sum by (node, collector) (increase(prickle_collector_series_dropped_total{n}[5m]))",
             "{{node}} {{collector}}")],
           {"h": 8, "w": 12, "x": 12, "y": 15},
           desc="Emitted outside every collector's own budget on purpose: a "
                "breach that erased its own evidence would leave a truncated "
                "scrape looking like a healthy smaller one."),

        row("Build", 23),
        table("Versions in the fleet", "prickle_build_info",
              {"h": 7, "w": 24, "x": 0, "y": 24},
              desc="Git tags are the only source of version truth; there is no "
                   "VERSION file. Two versions here means a partial rollout."),
    ]
    return dashboard("prickle-fleet-health", "Fleet Health",
                     "The exporter watching itself: collector health, cardinality, versions. "
                     "Starkdrift · prickle-exporter.", p)


DASHBOARDS = {
    "gpu-tenancy.json": gpu_tenancy,
    "node-overview.json": node_overview,
    "container-resources.json": container_resources,
    "fleet-health.json": fleet_health,
}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--check", action="store_true",
                    help="exit 1 if the checked-in files differ from generated")
    args = ap.parse_args()

    OUT.mkdir(parents=True, exist_ok=True)
    bad = False
    for name, build in DASHBOARDS.items():
        want = json.dumps(build(), indent=2, sort_keys=True) + "\n"
        path = OUT / name
        if args.check:
            got = path.read_text() if path.exists() else ""
            if got != want:
                print(f"stale: {path.relative_to(OUT.parent.parent.parent)} "
                      f"— re-run scripts/make-dashboards.py", file=sys.stderr)
                bad = True
        else:
            path.write_text(want)
            print(f"wrote {path.name}")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())

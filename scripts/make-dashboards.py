#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Generate the four Grafana dashboards SPEC.md §Distribution ships.

This file is the source of truth; packaging/grafana/dashboards/*.json is
generated from it and checked in, because Grafana loads JSON and nobody wants a
build step between a clone and a working quickstart. ci/check.sh re-runs this
and fails if the tree differs, so the two cannot drift.

Four dashboards of hand-written JSON would be some thousands of lines with the
same seven template variables copied into each. The variables are the part
SPEC.md is specific about, so they are defined once here and shared.

    python3 scripts/make-dashboards.py            # write
    python3 scripts/make-dashboards.py --check    # exit 1 if the tree differs
"""

import argparse
import json
import pathlib
import sys

OUT = pathlib.Path(__file__).resolve().parent.parent / "packaging" / "grafana" / "dashboards"

# SPEC.md §Metrics contract's closed set of identity labels. Each gets one
# textbox — "contains-search textbox template variables per identity label" —
# and the typed string filters the panels directly.
#
# There were paired dropdowns here until 2026-08-03, chained so that picking a
# node narrowed the namespaces, then the pods, then the containers. They are
# gone: the box that fed the dropdown is the whole control now, so a filter
# takes one action instead of two.
IDENTITY = [
    # (variable name, the label it filters)
    ("node",      "node"),
    ("namespace", "namespace"),
    ("pod",       "pod"),
    ("container", "container"),
    ("gpu",       "gpu_uuid"),
    ("mig",       "mig_uuid"),
]


def selector(constraints, extra=""):
    """Build a PromQL label selector from the textbox variables.

    Each box is read directly and wrapped as `.*<input>.*`, which is exactly how
    the dropdown's filter regex wrapped it before the dropdowns were removed —
    so an empty box still means "everything" and the default state is no filter.

    The wrapped input is a **regex, not a literal**: it reaches PromQL
    unescaped, so `pod-1` matches as a substring but an unbalanced `[` makes the
    panel error rather than match nothing. SPEC.md §Distribution accepts that,
    because escaping it would also take away `web-0[12]` from the operator who
    typed that on purpose.
    """
    parts = [f'{lbl}=~".*${name}_search.*"'
             for name, lbl in IDENTITY if lbl in constraints]
    if extra:
        parts.append(extra)
    return "{" + ",".join(parts) + "}" if parts else ""


def variables():
    """The templating list: a datasource, then one contains-textbox per label."""
    out = [{
        "name": "DS", "label": "Data source", "type": "datasource",
        "query": "prometheus", "current": {}, "hide": 0, "refresh": 1,
    }]
    for name, label in IDENTITY:
        # SPEC.md §Distribution: input is wrapped as .*<input>.* so typing
        # `gpu-` finds `node-gpu-04`. An empty box wraps to `.**`, which matches
        # everything — so the default state is "no filter" rather than "nothing
        # selected", and the dashboard is useful before anyone types.
        out.append({
            "name": f"{name}_search", "label": f"{label} contains",
            "type": "textbox", "query": "", "hide": 0,
            "current": {"text": "", "value": ""},
            "description": f"Substring match on {label}. Wrapped as .*input.* — leave empty for all.",
        })
    return out


def pod_qualified(expr):
    """Label a container series `<pod>/<container>`, truncated for reading, or
    just `<container>` when it is not in a pod.

    Presentation, so it lives here and not in the exporter: `pod` is already on
    every container series that has one, and adding a pre-joined label would put
    redundant bytes on every sample of every host to save a function call in
    four panels.

    Both identifiers are truncated because untruncated they are unreadable: a
    container ID is 64 hex characters, and `pod` is the pod's **UID**, not its
    name — the cgroup tree has no name to offer, so the kernel only ever sees
    the UID. Joined in full they make a ~100-character legend.

    12 characters of container ID is Docker's own short-ID convention, so the
    value still matches what `docker ps` prints and stays cross-referenceable.
    8 of a UID is enough to tell pods apart by eye on one node.

    The bounded repetitions are `{1,N}` rather than `{N}` for two reasons. A
    quantifier requiring exactly N would fail to match a shorter ID and silently
    leave the label unset, rendering an empty legend; and it is what preserves
    the no-pod case, because an empty `pod` matches `{1,8}` not at all, so the
    join produces a leading separator that the outer label_replace strips.
    """
    # pod_name is NOT on the hot series. It lives on prickle_container_info,
    # deliberately — SPEC.md §Metrics contract keeps descriptive attributes off
    # hot series — so it has to be joined in. Listing it in a `sum by` clause,
    # which is what this did until 2026-08-02, silently produces an empty
    # pod_name on every result: the "prefer the name" branch below could never
    # fire and every legend rendered the truncated UID. It looked right in the
    # generator and was wrong in Grafana.
    #
    # The join cannot drop a series. collectInfo runs for every container the
    # walk discovers, in the same pass and before any per-source collection, and
    # prickle_container_info is in the minimal preset — so a hot series without
    # a matching _info is not a state the exporter can produce.
    # The right-hand side is aggregated, not used raw. A one-to-many join
    # requires the right side to be unique per match group, and
    # prickle_container_info is not: during a DaemonSet rollout the outgoing and
    # incoming pod both report the same containers, so for the ~5 minutes before
    # the old series go stale there are two _info series per container differing
    # only in `instance`. The panel then fails outright with "found duplicate
    # series for the match group", for the whole dashboard window rather than
    # just the overlap. An instant query at any quiet moment shows nothing wrong,
    # which is what makes it easy to ship.
    #
    # max by (container, pod_name) collapses those to one. It picks a value, but
    # every duplicate carries the same pod_name for the same container — they are
    # the same reading seen twice — so there is nothing to choose between.
    joined = (f'({expr}) * on (container) group_left(pod_name) '
              f'max by (container, pod_name) (prickle_container_info)')
    inner = (f'label_replace({joined}, "cshort", "$1", "container", "^(.{{1,12}}).*$")')
    # Fallback FIRST, preference SECOND. label_replace overwrites the
    # destination whenever its regex matches the source — it does not "fill in
    # what is missing", which is what the previous ordering assumed. `pod` is a
    # non-empty UID on every pod series, so it always matched, so putting it
    # second meant the truncated UID overwrote the name on every legend that
    # had one. Reversed, the UID lands first and `pod_name` overwrites it only
    # when there is a name to overwrite it with: `^(.+)$` does not match an
    # empty label, so a container with no resolved name keeps the UID.
    inner = (f'label_replace({inner}, "pshort", "$1", "pod", "^(.{{1,8}}).*$")')
    inner = (f'label_replace({inner}, "pshort", "$1", "pod_name", "^(.+)$")')
    joined = f'label_join({inner}, "display", "/", "pshort", "cshort")'
    return f'label_replace({joined}, "display", "$1", "display", "^/(.+)$")'


def ts(title, exprs, gp, unit=None, desc=None, stack=False, axis=None):
    """A timeseries panel. exprs is a list of (promql, legend).

    axis is an optional (min, max) for the y-axis. Only worth setting where the
    metric has a real ceiling: Grafana otherwise autoscales to the data, so a
    pinned GPU draws a flat line at half panel height on a 0-200% axis and
    reads as half-utilised at a glance. A rate of CPU-seconds has no such
    ceiling — it passes 100% on any multi-core host — so this stays opt-in
    rather than applying to every percent panel.
    """
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
    if axis:
        p["fieldConfig"]["defaults"]["min"], p["fieldConfig"]["defaults"]["max"] = axis
    if desc:
        p["description"] = desc
    return p


def stat(title, expr, gp, unit=None, desc=None, text=False, legend=None):
    target = {"expr": expr, "refId": "A", "instant": True,
              "datasource": {"type": "prometheus", "uid": "${DS}"}}
    # Without a legendFormat a `by (label)` stat renders its series as the raw
    # selector — `{source="sysfs"}` rather than `sysfs`. Only worth setting on
    # the panels that group, which is why it is optional.
    if legend:
        target["legendFormat"] = legend
    p = {
        "type": "stat", "title": title, "gridPos": gp,
        "datasource": {"type": "prometheus", "uid": "${DS}"},
        "targets": [target],
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


def row(title, y, collapsed=False, panels=None):
    """A row header. `panels` belong to it only when it is collapsed.

    Grafana stores the two states differently and does not accept a mix: an
    expanded row is a header followed by its panels as siblings, a collapsed one
    is a header carrying them in its own `panels` array. Putting a collapsed
    row's children in the top-level list would render them as always-visible
    panels above the row and leave the row itself empty when opened.
    """
    return {"type": "row", "title": title, "gridPos": {"h": 1, "w": 24, "x": 0, "y": y},
            "collapsed": collapsed, "panels": panels or []}


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
def gpu_qualified(expr):
    """Label a GPU series `<node>/<index>/<gpu_uuid>`.

    A UUID alone does not say which machine or which slot a card is in, which
    is the first thing anyone asks when a card misbehaves. `index` is the
    driver's enumeration order on that node — what `nvidia-smi -L` prints — and
    it lives on prickle_gpu_info, not on the hot series, so it has to be joined
    in exactly as pod_name is for containers.

    The right-hand side is aggregated for the same reason it is there: during a
    DaemonSet rollout two exporters report the same card, and an unaggregated
    one-to-many join fails the whole panel with "found duplicate series for the
    match group".
    """
    joined = (f'({expr}) * on (gpu_uuid) group_left(index) '
              f'max by (gpu_uuid, index) (prickle_gpu_info)')
    return f'label_join({joined}, "gpu", "/", "node", "index", "gpu_uuid")'

def gpu_workload_qualified(expr):
    """Label a per-process GPU series `<node>/<index>/<pod>/<container>`.

    Two joins, because the three facts live in three places by design. The GPU
    process series carries node, gpu_uuid and container; `index` is on
    prickle_gpu_info; the pod is on prickle_container_info. SPEC.md §Metrics
    contract keeps descriptive attributes off hot series, so assembling them is
    the dashboard's job and not the exporter's.

    Both right-hand sides are aggregated. An unaggregated one-to-many join fails
    the entire panel the moment a DaemonSet rollout puts two exporters in the
    data, which an instant query will not show you.

    `pod` prefers the resolved name and falls back to the UID, in that order —
    the fallback is written first because label_replace overwrites whenever its
    regex matches, and a UID always matches.
    """
    with_index = (f'({expr}) * on (gpu_uuid) group_left(index) '
                  f'max by (gpu_uuid, index) (prickle_gpu_info)')
    with_pod = (f'({with_index}) * on (container) group_left(pod, pod_name) '
                f'max by (container, pod, pod_name) (prickle_container_info)')
    # A process on the host is in no container; label it so rather than
    # rendering an empty path segment that reads like a bug.
    inner = f'label_replace({with_pod}, "cshort", "host", "container", "^$")'
    inner = f'label_replace({inner}, "cshort", "$1", "container", "^(.{{1,12}}).*$")'
    inner = f'label_replace({inner}, "pshort", "host", "container", "^$")'
    inner = f'label_replace({inner}, "pshort", "$1", "pod", "^(.{{1,8}}).*$")'
    inner = f'label_replace({inner}, "pshort", "$1", "pod_name", "^(.+)$")'
    return f'label_join({inner}, "workload", "/", "node", "index", "pshort", "cshort")'

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
        # AMD has no equivalent of prickle_gpu_nvidia_source_info — there is
        # only one way to read sysfs, so there is no selection to report — and
        # this panel asked for that metric alone. On an AMD node it therefore
        # read "No data", which is the one answer that is wrong: it says the
        # exporter cannot tell you, on a host where it is reading two cards
        # perfectly well. Written when NVIDIA was the only vendor, and 0.8.0
        # shipped AMD without revisiting it.
        #
        # label_replace synthesises the source name AMD does not publish, so
        # both vendors land on the same `source` label and the panel keeps its
        # one meaning: which implementation answered.
        #
        # The value counts *nodes*, not cards. prickle_gpu_nvidia_source_info is
        # one series per node, so counting it directly already meant that; the
        # AMD side needs `max by (node)` first, or an eight-GPU box would report
        # eight where its NVIDIA neighbour reports one.
        stat("Live source",
             "count by (source) (prickle_gpu_nvidia_source_info) or "
             "count by (source) (label_replace(max by (node) "
             '(prickle_gpu_info{vendor="amd"}), "source", "sysfs", "", ""))',
             {"h": 4, "w": 4, "x": 20, "y": 1}, text=True, legend="{{source}}",
             desc="Which implementation answered, and on how many nodes: nvml "
                  "or smi on NVIDIA, sysfs on AMD. The two NVIDIA sources must "
                  "report identically. A mixed fleet shows both."),

        row("Utilization", 5),
        ts("GPU utilization",
           [(gpu_qualified(f"prickle_gpu_utilization_ratio{g} * 100"), "{{gpu}}")],
           {"h": 8, "w": 12, "x": 0, "y": 6}, unit="percent", axis=(0, 100),
           desc="ABSENT, not zero, once MIG is enabled — the driver reports "
                "[N/A] and reporting zero would fire idle-capacity alerts "
                "across a MIG fleet. A gap here on a MIG card is correct."),
        ts("Memory used per card",
           [(gpu_qualified(f"prickle_gpu_memory_used_bytes{g}"), "{{gpu}}"),
            (gpu_qualified(f"prickle_gpu_memory_total_bytes{g}"), "{{gpu}} total")],
           {"h": 8, "w": 12, "x": 12, "y": 6}, unit="bytes"),

        row("Per-process (opt-in)", 14),
        # By command first: it needs only the per-process flag, where the panel
        # below it additionally needs a pod to join against and the `container`
        # label. The one that draws under the weaker precondition goes first.
        ts("GPU memory by command",
           [(gpu_qualified(f"sum by (node, command, gpu_uuid) (prickle_gpu_process_memory_bytes{g})"),
             "{{gpu}} / {{command}}")],
           {"h": 8, "w": 24, "x": 0, "y": 15}, unit="bytes", stack=True,
           desc="Requires -collector.gpu.per-process. Keyed on `command`, the "
                "basename of the exe symlink — never a PID, which SPEC.md "
                "§Metrics contract forbids anywhere. Empty unless the flag is "
                "set. Under the nvidia-smi source these carry gpu_uuid only: "
                "that source cannot attribute a process to a MIG instance."),
        ts("GPU memory by pod and container",
           [(gpu_workload_qualified(
                f"sum by (node, gpu_uuid, container) (prickle_gpu_process_memory_bytes{g})"),
             "{{workload}}")],
           {"h": 8, "w": 24, "x": 0, "y": 23}, unit="bytes", stack=True,
           desc="Which pod is holding which card, as "
                "<node>/<gpu-index>/<pod>/<container>. The container comes from "
                "the GPU process's own /proc/<pid>/cgroup and the pod is joined "
                "from prickle_container_info, so a process outside a container "
                "reads `host` in both positions rather than leaving a blank "
                "segment. Requires -collector.gpu.per-process, which on "
                "Kubernetes also needs CAP_SYS_PTRACE: reading a foreign "
                "process's exe link is a PTRACE_MODE_READ operation and Yama "
                "ptrace_scope=1 is the default on Debian and Ubuntu."),

        # Last, and collapsed. Most cards are not partitioned, so for most
        # operators this row is two permanently empty panels — and an empty
        # panel above the ones that do have data reads as something broken.
        # Collapsed it costs a click on a MIG host and nothing anywhere else.
        row("MIG partitions", 31, collapsed=True, panels=[
            ts("MIG memory used",
               [(f"prickle_gpu_mig_memory_used_bytes{mig}", "{{mig_uuid}}")],
               {"h": 8, "w": 12, "x": 0, "y": 32}, unit="bytes",
               desc="A separate family from the card's memory: an instance's "
                    "memory is a partition of its parent's, and one family "
                    "holding both would double-count under sum()."),
            table("MIG topology",
                  f"prickle_gpu_mig_info{mig}", {"h": 8, "w": 12, "x": 12, "y": 32},
                  desc="profile comes from the driver's compute-instance name, "
                       "the same string nvidia-smi -L prints. Deriving it from "
                       "memory was wrong on every H200 profile."),
        ]),
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


K8S_CM = (pathlib.Path(__file__).resolve().parent.parent
          / "packaging" / "kubernetes-demo" / "grafana-dashboards.yaml")


def configmap(dashboards):
    """The same four dashboards as a ConfigMap for the Kubernetes demo.

    Generated rather than hand-copied for the obvious reason: two copies of
    four dashboards drift, and the one nobody looks at drifts silently. This
    and the JSON files come out of the same functions in the same run, and
    ci/check.sh fails if either is stale.
    """
    out = [
        "# SPDX-License-Identifier: Apache-2.0",
        "#",
        "# GENERATED by scripts/make-dashboards.py — do not edit.",
        "# Same four dashboards as packaging/grafana/dashboards/, so the compose",
        "# demo and the Kubernetes demo cannot show different things.",
        "apiVersion: v1",
        "kind: ConfigMap",
        "metadata:",
        "  name: prickle-dashboards",
        "  namespace: prickle-demo",
        "  labels:",
        "    app.kubernetes.io/part-of: prickle-exporter",
        "data:",
    ]
    for name, body in sorted(dashboards.items()):
        out.append(f"  {name}: |")
        for line in body.rstrip("\n").split("\n"):
            out.append("    " + line if line else "")
    return "\n".join(out) + "\n"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--check", action="store_true",
                    help="exit 1 if the checked-in files differ from generated")
    args = ap.parse_args()

    OUT.mkdir(parents=True, exist_ok=True)
    bad = False
    rendered = {}
    for name, build in DASHBOARDS.items():
        want = json.dumps(build(), indent=2, sort_keys=True) + "\n"
        rendered[name] = want
        path = OUT / name
        if args.check:
            got = path.read_text() if path.exists() else ""
            if got != want:
                print(f"stale: {path} — re-run scripts/make-dashboards.py", file=sys.stderr)
                bad = True
        else:
            path.write_text(want)
            print(f"wrote {path.name}")

    cm = configmap(rendered)
    if args.check:
        got = K8S_CM.read_text() if K8S_CM.exists() else ""
        if got != cm:
            print(f"stale: {K8S_CM} — re-run scripts/make-dashboards.py", file=sys.stderr)
            bad = True
    else:
        K8S_CM.parent.mkdir(parents=True, exist_ok=True)
        K8S_CM.write_text(cm)
        print(f"wrote {K8S_CM.name}")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())

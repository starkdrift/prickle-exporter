# Pod names, and what they cost

Turning a pod UID into a pod name, what it grants, and why the answer differs
between Kubernetes and systemd.

[← README](../README.md)

By default a container in a pod is identified by the pod's **UID**, not its
name:

```
prickle_container_info{pod="537209ed-f2d7-423a-8e0a-ec05d6280092", pod_name="", ...}
```

That is not a limitation of the exporter so much as of where it looks. The
cgroup tree carries a pod's UID and never its name — the kernel only ever sees
the UID. Most people want the name, and `-collector.container.pod-names` gets
it:

```
prickle_container_info{pod="537209ed-…", pod_name="web-frontend", namespace="default", ...}
```

It works by listing `/var/log/pods/<namespace>_<pod>_<uid>/`, which the
**kubelet** creates on every CRI runtime. One directory listing, no API call,
no second exporter, and nothing inside those directories is read — the names
*are* the directory names, so workload log content is never opened.

## The cost, stated plainly

It differs by how you run it. `/var/log/pods` is `root:root 0750`, so something
has to satisfy that mode.

On **Kubernetes** the chart runs the pod as uid 65532 with `runAsGroup: 0`, and
the directory's *group* bits are the entire grant. It costs group-root
membership: the exporter can read files that are group-readable and owned by
group root, and nothing else. It adds **no capability**, because a capability
added to a non-root uid is unusable — Kubernetes puts it in the bounding set
only, leaving `CapPrm`, `CapEff` and `CapAmb` all zero. The chart asked for
`CAP_DAC_READ_SEARCH` until 0.8.0 and never once used it.

On **systemd** the drop-in below sets `AmbientCapabilities=CAP_DAC_READ_SEARCH`,
which does work — systemd sets the ambient set, so the non-root `DynamicUser`
really holds the capability. That is the more expensive of the two: it bypasses
file-read and directory-search checks **everywhere on the host**, and in the
same test that proved it reads `/var/log/pods` it also read `/etc/shadow`.

So the decision is genuinely yours, and it is not obvious:

| | Default | `pod-names` on Kubernetes | `pod-names` under systemd |
|---|---|---|---|
| Pod identified by | UID | name and namespace | name and namespace |
| Capabilities | none | none | `CAP_DAC_READ_SEARCH` |
| Extra reach | none | files readable by group root | **any file on the host** |
| Container metrics | all of them | all of them | all of them |

Both routes assume `/var/log/pods` is `0750` with group root, which is what
these hosts ship. A node that ships it `0700` leaves running as uid 0 as the
only way, and on such a node the group-root route silently yields no names.

**Nothing else changes.** Every container is reported either way, with every
metric; only the labels differ. If you leave it off and later want names in
dashboards, a join against `kube_pod_info` from kube-state-metrics gets you
there without granting anything.

## Enabling it

```sh
# Kubernetes
helm install prickle packaging/helm/prickle-exporter -n monitoring \
  --set collectors.podNames.enabled=true

# systemd — a drop-in, so the shipped unit stays unprivileged
systemctl edit prickle
#   [Service]
#   ExecStart=
#   ExecStart=/usr/local/bin/prickle -collector.container.pod-names
#   AmbientCapabilities=CAP_DAC_READ_SEARCH
#   CapabilityBoundingSet=CAP_DAC_READ_SEARCH
```

If you enable it without granting the privilege, nothing breaks: the exporter
logs `open /var/log/pods: permission denied` once per pass, reports every
container as usual, and leaves `pod_name` empty.

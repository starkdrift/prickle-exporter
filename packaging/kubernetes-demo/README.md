# Kubernetes demo

The same four dashboards as the Compose quickstart, on a cluster: prickle as a
DaemonSet, Prometheus scraping it, and Grafana with everything provisioned.

```sh
# prickle itself, with pod names on — see the caveat below
helm install prickle packaging/helm/prickle-exporter -n prickle-demo --create-namespace \
  --set collectors.podNames.enabled=true

# Prometheus, Grafana, the dashboards, and a workload to draw
kubectl apply -f packaging/kubernetes-demo/

kubectl -n prickle-demo port-forward svc/grafana 3000:3000
kubectl -n prickle-demo port-forward svc/prometheus 9090:9090
```

Grafana is then at <http://localhost:3000> with anonymous admin and no login
form, and the **Prickle** folder already holds GPU Tenancy, Node Overview,
Container Resources and Fleet Health.

## What is in here

| File | |
|---|---|
| `00-namespace.yaml` | `prickle-demo` |
| `10-prometheus.yaml` | Prometheus, its RBAC, and a scrape config that finds prickle by pod discovery |
| `20-grafana.yaml` | Grafana with the datasource and dashboard provider |
| `grafana-dashboards.yaml` | **Generated** — the four dashboards as a ConfigMap |
| `30-workload.yaml` | Two pods, one deliberately throttled |

`grafana-dashboards.yaml` is written by `scripts/make-dashboards.py`, the same
run that writes `packaging/grafana/dashboards/*.json`, and `ci/check.sh` fails
if either is stale. Two hand-maintained copies of four dashboards would drift,
and the copy nobody opens would drift silently.

The RBAC in here belongs to **Prometheus**, which needs to find pods. prickle
still has no ServiceAccount and no API access of any kind.

Prometheus discovers prickle by pod rather than through its Service, because
the Service is headless and every pod reports about a different node — scraping
the Service name would hit one node at random and hide the rest.

`30-workload.yaml` exists because a correct demo with nothing running looks
identical to a broken one. `demo-throttled` requests a quarter of a CPU and then
burns everything it can, so the throttling panels show real stalls.

## Pod names

`--set collectors.podNames.enabled=true` is what turns
`pod="537209ed-f2d7-…"` into `pod_name="demo-throttled"`. It runs the container
as uid 65532 in **group root** (`runAsGroup: 0`), which is what reads the
kubelet's `0750 root:root` `/var/log/pods` — read
[docs/pod-names.md](../../docs/pod-names.md) before enabling it outside a demo.
It granted `CAP_DAC_READ_SEARCH` until 0.8.0, which was measured to do nothing
at all on Kubernetes.

Verified on a single-node kubeadm cluster: 26 containers, 14 pod names, three
namespaces, and both dashboard dropdowns populating. It needs **0.7.0 or
later** — the flag does not exist in 0.6.0 and the pod will crash-loop with
`flag provided but not defined`.

## GPU nodes

The single `helm install` above is written for a **uniform** cluster. On a
cluster where only some nodes have a GPU it leaves the GPU Tenancy dashboard
completely empty, and neither obvious fix works on its own:

- The stock image is `FROM scratch` and carries **no GPU support at all**, so a
  release using it reports nothing about the card even on the GPU node.
- `nvml.enabled=true` mounts the driver's `libnvidia-ml.so.1` as a hostPath with
  `type: File`. On a node without a driver that file does not exist and the pod
  sticks in **`ContainerCreating`** forever.

So install **two releases into the same namespace**, split on the
`nvidia.com/gpu.present` label — which the standalone NVIDIA device plugin does
**not** set for you (it comes from the GPU Operator's NFD, so label the node by
hand):

```sh
# non-GPU nodes. A nodeSelector cannot express "label absent"; only nodeAffinity can.
helm install prickle packaging/helm/prickle-exporter -n prickle-demo --create-namespace \
  --set collectors.podNames.enabled=true \
  --set-json 'affinity={"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"nvidia.com/gpu.present","operator":"DoesNotExist"}]}]}}}'

# GPU nodes
helm install prickle-nvml packaging/helm/prickle-exporter -n prickle-demo \
  --set collectors.podNames.enabled=true \
  --set collectors.perProcess=true \
  --set nvml.enabled=true \
  --set-string nodeSelector."nvidia\.com/gpu\.present"=true
```

Both carry `app.kubernetes.io/name=prickle-exporter`, which is what the
Prometheus in here selects on, so both are scraped with no config change.

**`30-workload.yaml` has no GPU workload** — it is two busybox pods, so the GPU
Tenancy dashboard draws a card at idle until something loads it. The panels that
attribute GPU memory to a pod also need `collectors.perProcess=true`, above.

Verified on a two-node kubeadm 1.34 cluster (H100 80GB worker, driverless
control plane) on 2026-08-05: every panel populated except the two MIG ones,
which are empty because the card is not partitioned.

### On AMD, one release covers the cluster

None of the split above applies. The AMD path is sysfs and DRM `fdinfo` — there
is no second image, no driver library to mount and nothing that can fail to
start on a driverless node, so the single `helm install` at the top of this file
reaches every node and reports the cards it finds:

```sh
helm install prickle packaging/helm/prickle-exporter -n prickle-demo --create-namespace \
  --set collectors.podNames.enabled=true \
  --set collectors.perProcess=true
```

**`collectors.perProcess=true` is where AMD needs something NVIDIA does not.**
It brings `collectors.appArmorUnconfined` with it, on by default since 0.8.0,
and without that the per-process panels come back **empty with nothing logged**
— containerd's default profile denies the ptrace-class access that reading
another process's `fdinfo` needs, and the AMD path has no driver API to ask
instead. Measured here twice, once by accident: installing a chart that predates
the setting emptied those two panels on an otherwise healthy cluster.

Verified on a single-node kubeadm 1.34 cluster with one MI300X on 2026-08-06,
against the released `0.8.0` image: two tenant pods in two namespaces sharing
the card, both resolved to pod name and container.

## Not a deployment

Anonymous admin, no Ingress, six hours of Prometheus retention on an
`emptyDir`. Reach it with `port-forward`, and do not put it on a network you do
not own.

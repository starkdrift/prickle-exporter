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
`pod="537209ed-f2d7-…"` into `pod_name="demo-throttled"`, and it grants the
container `CAP_DAC_READ_SEARCH` to read the kubelet's `/var/log/pods`. That
capability bypasses file-read checks host-wide — read
[the README section](../../README.md#pod-names-and-what-they-cost) before
enabling it outside a demo.

Verified on a single-node kubeadm cluster: 26 containers, 14 pod names, three
namespaces, and both dashboard dropdowns populating. It needs **0.7.0 or
later** — the flag does not exist in 0.6.0 and the pod will crash-loop with
`flag provided but not defined`.

## Not a deployment

Anonymous admin, no Ingress, six hours of Prometheus retention on an
`emptyDir`. Reach it with `port-forward`, and do not put it on a network you do
not own.

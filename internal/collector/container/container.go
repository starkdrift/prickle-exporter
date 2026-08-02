// SPDX-License-Identifier: Apache-2.0

// Package container implements the Phase 2 container collector: a walk of the
// cgroup tree, with identity taken from the directory names the container
// runtimes create (SPEC.md §Collectors).
//
// Both cgroup hierarchies are read. v2 is primary and v1 has its own reader in
// v1.go behind the hierarchy interface, because the two are different data
// models rather than spellings of one — see the comment there. What they are
// not allowed to differ in is the output: the same metric names, units and
// labels come out of either (SPEC.md §Hard constraints #4).
//
// Every parser here was developed against the captured cgroup tree in testdata/
// (SPEC.md §Testing rules). Nothing guesses at a file format or a path shape;
// testdata/README.md records exactly which layouts the capture covers and which
// it does not.
//
// Two things this package deliberately does not read:
//
//   - cgroup.procs, the only file in a cgroup that contains PIDs. SPEC.md
//     §Metrics contract: a PID never appears as a label or a value anywhere, and
//     pids.current already answers "how many processes", which is the only
//     question the file could have been used for.
//   - the pod, QoS and root slices above a container. Emitting those alongside
//     their children would make `sum(prickle_container_memory_usage_bytes)`
//     count every byte two or three times. A pod total is `sum by (pod)`.
//
// All paths are built through fsroot.Roots (SPEC.md §Hard constraints #3).
package container

import (
	"context"
	"errors"
	"time"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// Metric name prefix for every family in this package.
const prefix = "prickle_container_"

// DefaultDockerTimeout bounds the optional Docker socket enrichment. It is well
// under the sampler's per-collector deadline: a wedged daemon must cost the
// container names, not the container metrics.
const DefaultDockerTimeout = 2 * time.Second

// Options configures the container collector. The zero value is usable: it
// walks the live cgroup v2 mount with no Docker enrichment.
type Options struct {
	// Roots is the set of filesystem prefixes to read through.
	Roots fsroot.Roots

	// DockerSocket enables the optional enrichment path from SPEC.md
	// §Collectors: a GET-only request to the Docker API for human-readable
	// names and images, which land on prickle_container_info and never on a hot
	// series. Empty disables it, and it is empty by default — the exporter does
	// not open a socket nobody asked it to open.
	DockerSocket string

	// DockerTimeout bounds that request. Zero means DefaultDockerTimeout.
	DockerTimeout time.Duration

	// PodNames reads the kubelet's pod log directory to resolve a pod's UID to
	// its namespace and name (SPEC.md §Collectors). Off by default: that path
	// is root-only, so it costs the exporter's unprivileged posture.
	PodNames bool
}

// Collector walks the cgroup v2 tree and reports per-container resource usage.
type Collector struct {
	opts Options
}

// New returns a container collector. Unset Options fields take their defaults.
func New(opts Options) *Collector {
	if opts.DockerTimeout <= 0 {
		opts.DockerTimeout = DefaultDockerTimeout
	}
	return &Collector{opts: opts}
}

// Name implements collector.Collector.
func (c *Collector) Name() string { return "container" }

// Collect implements collector.Collector.
//
// The walk runs once and every source is then read per container, so a
// container whose io.stat is unreadable still reports its CPU and memory. A
// host with no cgroup mount at all produces no samples and no error: that is a
// fact about the machine, not a collection failure, and `prickle diagnose` is
// where it is said plainly rather than by erroring on every scrape.
func (c *Collector) Collect(ctx context.Context, out *exposition.Set) error {
	var errs []error

	// Both hierarchies are in scope (SPEC.md §Hard constraints #4). v2 is tried
	// first and v1 only if it found nothing, which resolves a hybrid host
	// without having to work out which layout its runtime chose.
	var (
		live       hierarchy
		containers []cgroup
	)
	for _, h := range c.hierarchies() {
		found, err := h.discover(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(found) > 0 {
			live, containers = h, found
			break
		}
	}
	if len(containers) == 0 {
		return errors.Join(errs...)
	}

	// Resolved once for the whole pass: both are per-host lookups, not
	// per-container ones, and both are enrichment — a failure costs a label
	// value, never a metric.
	devices, err := c.blockDevices()
	if err != nil {
		errs = append(errs, err)
	}
	names, err := c.dockerNames(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	pods, err := c.podNames()
	if err != nil {
		errs = append(errs, err)
	}

	sources := live.sources(devices)

	for _, cg := range containers {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}

		meta := pods[cg.pod]
		labels := cg.labels(meta.namespace)
		c.collectInfo(out, cg, names[cg.id], meta)

		for _, collect := range sources {
			if err := collect(out, cg, labels); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// collectInfo emits the companion _info gauge.
//
// SPEC.md §Metrics contract: descriptive attributes live here and never on a
// hot series, joined in queries with group_left. The runtime and QoS class come
// from the cgroup directory names; the name and image only exist when Docker
// enrichment is configured and the container is a Docker one.
//
// pod_name and namespace are empty unless -collector.container.pod-names is on
// and the kubelet's log directory was readable.
//
// `namespace` appears here as well as on the hot series, which looks like
// duplication and is not. The dashboards enumerate their dropdown values with
// label_values(prickle_container_info, …), so a label that exists only on hot
// series has no source to populate a namespace picker from — found by bringing
// the Kubernetes demo up and watching that dropdown stay empty while the hot
// series plainly carried the label. `pod` is on this gauge for the same
// reason. `pod` continues to carry the
// UID either way: it is the join key every existing rule was written against,
// and changing what a label means is the one thing SPEC.md §Versioning calls a
// major even when the new meaning is better.
func (c *Collector) collectInfo(out *exposition.Set, cg cgroup, dm dockerMeta, pm podMeta) {
	out.Gauge(prefix+"info",
		"Container identity: constant 1, carrying the descriptive attributes to join on.").
		Add(1,
			exposition.L("container", cg.id),
			exposition.L("pod", cg.pod),
			exposition.L("pod_name", pm.name),
			exposition.L("namespace", pm.namespace),
			exposition.L("runtime", cg.runtime),
			exposition.L("qos", cg.qos),
			exposition.L("name", dm.name),
			exposition.L("image", dm.image),
		)
}

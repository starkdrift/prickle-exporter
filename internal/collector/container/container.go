// SPDX-License-Identifier: Apache-2.0

// Package container implements the Phase 2 container collector: a walk of the
// cgroup v2 tree, with identity taken from the directory names the container
// runtimes create (SPEC.md §Collectors).
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
// host with no cgroup v2 mount produces no samples and no error: SPEC.md §Hard
// constraints #4 puts v1 out of scope, and `prickle diagnose` is where that is
// said plainly rather than as a failed collection on every scrape.
func (c *Collector) Collect(ctx context.Context, out *exposition.Set) error {
	containers, err := c.discover(ctx)
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return nil
	}

	var errs []error

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

	for _, cg := range containers {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}

		labels := cg.labels()
		c.collectInfo(out, cg, names[cg.id])

		for _, collect := range []func(*exposition.Set, cgroup, []exposition.Label) error{
			c.collectCPU,
			c.collectMemory,
			c.collectIO(devices),
			c.collectPIDs,
			c.collectPressure,
		} {
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
func (c *Collector) collectInfo(out *exposition.Set, cg cgroup, meta dockerMeta) {
	out.Gauge(prefix+"info",
		"Container identity: constant 1, carrying the descriptive attributes to join on.").
		Add(1,
			exposition.L("container", cg.id),
			exposition.L("pod", cg.pod),
			exposition.L("runtime", cg.runtime),
			exposition.L("qos", cg.qos),
			exposition.L("name", meta.name),
			exposition.L("image", meta.image),
		)
}

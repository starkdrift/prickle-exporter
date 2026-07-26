// SPDX-License-Identifier: Apache-2.0

// Package collector defines the contract every metric source implements.
//
// SPEC.md §Architecture: a sampler goroutine polls collectors on an interval,
// renders into a buffer, and swaps it under a mutex, so a slow collector can
// never stall a scrape. The interface is deliberately small — a collector
// writes samples and reports whether it had trouble; scheduling, timeouts and
// self-instrumentation belong to the sampler.
package collector

import (
	"context"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// Collector is one source of metrics.
//
// Collect is called on the sampler's goroutine with a per-collector deadline.
// It must respect ctx and it must never write to the filesystem (SPEC.md §Hard
// constraints #2).
//
// A returned error does not discard the samples already written to out. Partial
// data plus a raised prickle_collector_errors_total is more useful than an
// empty family: a host missing /proc/pressure should still report its CPU time.
type Collector interface {
	// Name is the short, stable identifier used in self-metric labels and in
	// `prickle diagnose` output. Lowercase, no spaces.
	Name() string

	// Collect samples the source and writes into out.
	Collect(ctx context.Context, out *exposition.Set) error
}

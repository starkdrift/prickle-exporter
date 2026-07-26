// SPDX-License-Identifier: Apache-2.0

// Package sampler polls collectors on an interval and serves the last
// completed render.
//
// SPEC.md §Architecture: a sampler goroutine polls collectors, renders into a
// buffer, and swaps it under a mutex; net/http serves the last completed render
// so a slow collector can never stall a scrape. The decoupling is the point —
// a scrape reads a []byte and a mutex, never a filesystem.
package sampler

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/starkdrift/prickle-exporter/internal/collector"
	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// contentType is the Prometheus text exposition format's media type.
const contentType = "text/plain; version=0.0.4; charset=utf-8"

// Options configures a Sampler.
type Options struct {
	// Interval between sampling passes. A scrape faster than this sees the
	// same bytes twice, which is correct: the data genuinely has not changed.
	Interval time.Duration

	// Timeout bounds a single collector's Collect call.
	Timeout time.Duration

	// ConstLabels are applied to every series — this is where the `node`
	// identity label from SPEC.md §Metrics contract enters.
	ConstLabels []exposition.Label

	// Version is reported on prickle_build_info.
	Version string

	// Logger receives collector errors. Nil means slog.Default().
	Logger *slog.Logger

	// Now is the clock, injectable for tests. Nil means time.Now.
	Now func() time.Time
}

// Sampler runs collectors and holds the rendered exposition document.
type Sampler struct {
	collectors []collector.Collector
	opts       Options
	log        *slog.Logger
	now        func() time.Time

	// errorTotals accumulates across passes. The Set is rebuilt from scratch
	// every render, so a counter's monotonicity has to live here rather than
	// in the exposition layer.
	errorTotals map[string]uint64

	mu       sync.RWMutex
	rendered []byte
}

// New returns a Sampler over collectors. Nothing is sampled until Run or
// SampleOnce is called.
func New(collectors []collector.Collector, opts Options) *Sampler {
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Sampler{
		collectors:  collectors,
		opts:        opts,
		log:         opts.Logger,
		now:         opts.Now,
		errorTotals: make(map[string]uint64, len(collectors)),
	}
}

// Run samples until ctx is cancelled.
//
// The first pass happens immediately so that /metrics is never empty for a
// whole interval after startup.
func (s *Sampler) Run(ctx context.Context) {
	s.SampleOnce(ctx)

	ticker := time.NewTicker(s.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SampleOnce(ctx)
		}
	}
}

// SampleOnce runs every collector once and swaps in the new render.
//
// Collectors run sequentially. They are cheap file reads, and a fixed order
// keeps the output byte-stable for the golden test; parallelism here would buy
// milliseconds and cost determinism.
func (s *Sampler) SampleOnce(ctx context.Context) {
	set := exposition.NewSet(s.opts.ConstLabels...)

	duration := set.Gauge("prickle_collector_duration_seconds",
		"Seconds the last sampling pass spent in each collector.")
	errorsTotal := set.Counter("prickle_collector_errors_total",
		"Collector failures since start.")
	success := set.Gauge("prickle_collector_success",
		"Whether the last sampling pass of this collector succeeded.")

	for _, c := range s.collectors {
		name := exposition.L("collector", c.Name())

		cctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
		start := s.now()
		err := c.Collect(cctx, set)
		elapsed := s.now().Sub(start)
		cancel()

		duration.Add(elapsed.Seconds(), name)
		if err != nil {
			s.errorTotals[c.Name()]++
			s.log.Warn("collector failed", "collector", c.Name(), "error", err)
		}
		errorsTotal.Add(float64(s.errorTotals[c.Name()]), name)
		success.Add(boolValue(err == nil), name)
	}

	set.Gauge("prickle_build_info",
		"Build identity: constant 1, carrying the exporter and Go versions.").
		Add(1,
			exposition.L("version", s.opts.Version),
			exposition.L("go_version", runtime.Version()))

	set.Gauge("prickle_render_timestamp_seconds",
		"Unix timestamp of the sampling pass these metrics come from. A scrape "+
			"serves the last completed render, so this is the age of the data, "+
			"not of the request.").
		Add(float64(s.now().UnixNano()) / 1e9)

	// A build-time naming or duplicate-series problem is the exporter's own
	// bug. Log it loudly; still serve what rendered.
	if err := set.Err(); err != nil {
		s.log.Error("exposition problems in render", "error", err)
	}

	var buf bytes.Buffer
	if _, err := set.WriteTo(&buf); err != nil {
		s.log.Error("render failed", "error", err)
		return
	}

	s.mu.Lock()
	s.rendered = buf.Bytes()
	s.mu.Unlock()
}

// Snapshot returns the last completed render. The slice is not copied and must
// not be modified.
func (s *Sampler) Snapshot() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rendered
}

// ServeHTTP serves the last completed render.
//
// It never triggers a collection. A scrape storm, or a Prometheus with a
// one-second interval pointed at a host whose disks have stalled, costs a
// mutex and a write.
func (s *Sampler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := s.Snapshot()
	if body == nil {
		// Before the first pass completes there is nothing truthful to say.
		http.Error(w, "no sample completed yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", contentType)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

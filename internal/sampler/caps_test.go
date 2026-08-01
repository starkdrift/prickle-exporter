// SPDX-License-Identifier: Apache-2.0

package sampler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/collector"
	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// noisyCollector contributes n series every pass, which is how a runaway
// cardinality source is modelled: it does not fail, it simply will not stop.
func noisyCollector(name string, n int) *fakeCollector {
	return &fakeCollector{
		name: name,
		collect: func(_ context.Context, out *exposition.Set) error {
			f := out.Gauge("prickle_test_noise", "One series per label value.")
			for i := 0; i < n; i++ {
				f.Add(1, exposition.L("i", strconv.Itoa(i)))
			}
			return nil
		},
	}
}

// sampleValue reads one sample's value out of a rendered document, matching on
// the whole label set so a prefix cannot pick up a neighbouring series.
func sampleValue(t *testing.T, rendered, prefix string) float64 {
	t.Helper()
	for _, line := range strings.Split(rendered, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("parsing %q: %v", line, err)
		}
		return v
	}
	t.Fatalf("no sample matching %q in:\n%s", prefix, rendered)
	return 0
}

// TestCapDropsTheExcess is the core of SPEC.md §Metrics contract's cardinality
// cap: past the limit the samples are dropped, and the ones already written
// are kept. A collector that blows its budget costs its own tail, not the pass.
func TestCapDropsTheExcess(t *testing.T) {
	const cap, want = 10, 50

	opts := testOptions()
	opts.MaxSeries = cap
	s := New([]collector.Collector{noisyCollector("noisy", want)}, opts)
	s.SampleOnce(context.Background())

	rendered := string(s.Snapshot())
	kept := strings.Count(rendered, "prickle_test_noise{")
	if kept != cap {
		t.Errorf("kept %d series, want %d", kept, cap)
	}

	if got := sampleValue(t, rendered, `prickle_collector_series{collector="noisy"}`); got != cap {
		t.Errorf("prickle_collector_series = %v, want %d", got, cap)
	}
	if got := sampleValue(t, rendered, `prickle_collector_series_dropped_total{collector="noisy"}`); got != want-cap {
		t.Errorf("dropped total = %v, want %d", got, want-cap)
	}
}

// TestCapDisabledKeepsEverything: zero means count but do not bound. An
// operator who turns the backstop off must get every series, not none.
func TestCapDisabledKeepsEverything(t *testing.T) {
	const want = 200

	opts := testOptions()
	opts.MaxSeries = 0
	s := New([]collector.Collector{noisyCollector("noisy", want)}, opts)
	s.SampleOnce(context.Background())

	rendered := string(s.Snapshot())
	if kept := strings.Count(rendered, "prickle_test_noise{"); kept != want {
		t.Errorf("kept %d series, want %d", kept, want)
	}
	if got := sampleValue(t, rendered, `prickle_collector_series_dropped_total{collector="noisy"}`); got != 0 {
		t.Errorf("dropped total = %v with the cap disabled, want 0", got)
	}
}

// TestDroppedTotalIsMonotonic: the Set is rebuilt every pass, so a counter that
// lived in it would reset to the per-pass figure and read as a counter reset in
// Prometheus. It has to accumulate in the Sampler, like errorTotals.
func TestDroppedTotalIsMonotonic(t *testing.T) {
	const cap, emit, passes = 4, 10, 3

	opts := testOptions()
	opts.MaxSeries = cap
	s := New([]collector.Collector{noisyCollector("noisy", emit)}, opts)

	for i := 1; i <= passes; i++ {
		s.SampleOnce(context.Background())
		got := sampleValue(t, string(s.Snapshot()),
			`prickle_collector_series_dropped_total{collector="noisy"}`)
		if want := float64((emit - cap) * i); got != want {
			t.Fatalf("after pass %d: dropped total = %v, want %v", i, got, want)
		}
	}
}

// TestCapIsPerCollector: the budget resets between collectors, so a greedy one
// cannot spend a quiet one's allowance. Without the reset the second collector
// here would be silenced by the first.
func TestCapIsPerCollector(t *testing.T) {
	const cap = 5

	opts := testOptions()
	opts.MaxSeries = cap
	s := New([]collector.Collector{
		noisyCollector("greedy", 100),
		noisyCollector("quiet", 2),
	}, opts)
	s.SampleOnce(context.Background())

	rendered := string(s.Snapshot())
	if got := sampleValue(t, rendered, `prickle_collector_series{collector="greedy"}`); got != cap {
		t.Errorf("greedy kept %v series, want %d", got, cap)
	}
	if got := sampleValue(t, rendered, `prickle_collector_series{collector="quiet"}`); got != 2 {
		t.Errorf("quiet kept %v series, want 2", got)
	}
	if got := sampleValue(t, rendered, `prickle_collector_series_dropped_total{collector="quiet"}`); got != 0 {
		t.Errorf("quiet dropped %v series; it never reached the cap", got)
	}
}

// TestSelfMetricsSurviveTheCap is the one that matters most when a cap fires.
//
// The self-metrics are what tell an operator the cap fired at all. If they were
// charged to the collector's budget — or dropped by it — a breach would erase
// its own evidence, and the scrape would look like a healthy smaller one. The
// cap here is one series, far below what the sampler itself emits.
func TestSelfMetricsSurviveTheCap(t *testing.T) {
	opts := testOptions()
	opts.MaxSeries = 1
	s := New([]collector.Collector{noisyCollector("noisy", 100)}, opts)
	s.SampleOnce(context.Background())

	rendered := string(s.Snapshot())
	for _, want := range []string{
		`prickle_collector_series{collector="noisy"}`,
		`prickle_collector_series_dropped_total{collector="noisy"}`,
		`prickle_collector_duration_seconds{collector="noisy"}`,
		`prickle_collector_errors_total{collector="noisy"}`,
		`prickle_collector_success{collector="noisy"}`,
		"prickle_build_info{",
		"prickle_render_timestamp_seconds",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("cap of 1 removed the self-metric %s:\n%s", want, rendered)
		}
	}

	if got := sampleValue(t, rendered, `prickle_collector_series{collector="noisy"}`); got != 1 {
		t.Errorf("kept %v series, want 1", got)
	}
}

// TestCapCountsWithoutBreach: below the cap, series is the real count and
// dropped stays zero, so the pair is usable as a cardinality gauge and not
// only as a breach alarm.
func TestCapCountsWithoutBreach(t *testing.T) {
	opts := testOptions()
	opts.MaxSeries = 1000
	s := New([]collector.Collector{noisyCollector("noisy", 37)}, opts)
	s.SampleOnce(context.Background())

	rendered := string(s.Snapshot())
	if got := sampleValue(t, rendered, `prickle_collector_series{collector="noisy"}`); got != 37 {
		t.Errorf("prickle_collector_series = %v, want 37", got)
	}
	if got := sampleValue(t, rendered, `prickle_collector_series_dropped_total{collector="noisy"}`); got != 0 {
		t.Errorf("dropped = %v below the cap, want 0", got)
	}
}

// TestCapDoesNotHideACollectorError: dropping series and failing are different
// things, and a collector that does both must still report the error.
func TestCapDoesNotHideACollectorError(t *testing.T) {
	opts := testOptions()
	opts.MaxSeries = 3
	both := &fakeCollector{
		name: "both",
		collect: func(_ context.Context, out *exposition.Set) error {
			f := out.Gauge("prickle_test_noise", "One series per label value.")
			for i := 0; i < 20; i++ {
				f.Add(1, exposition.L("i", strconv.Itoa(i)))
			}
			return fmt.Errorf("partial read")
		},
	}
	s := New([]collector.Collector{both}, opts)
	s.SampleOnce(context.Background())

	rendered := string(s.Snapshot())
	if got := sampleValue(t, rendered, `prickle_collector_errors_total{collector="both"}`); got != 1 {
		t.Errorf("errors_total = %v, want 1", got)
	}
	if got := sampleValue(t, rendered, `prickle_collector_success{collector="both"}`); got != 0 {
		t.Errorf("success = %v, want 0", got)
	}
	if got := sampleValue(t, rendered, `prickle_collector_series_dropped_total{collector="both"}`); got != 17 {
		t.Errorf("dropped = %v, want 17", got)
	}
}

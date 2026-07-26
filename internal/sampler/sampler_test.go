// SPDX-License-Identifier: Apache-2.0

package sampler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/starkdrift/prickle-exporter/internal/collector"
	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// fakeCollector records how often it ran and does whatever collect says.
type fakeCollector struct {
	name    string
	collect func(context.Context, *exposition.Set) error

	mu    sync.Mutex
	calls int
}

func (f *fakeCollector) Name() string { return f.name }

func (f *fakeCollector) Collect(ctx context.Context, out *exposition.Set) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.collect(ctx, out)
}

func (f *fakeCollector) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// quietLogger discards the warnings the error-path tests deliberately provoke.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func testOptions() Options {
	return Options{Version: "test", Logger: quietLogger()}
}

// TestServesNothingBeforeFirstSample: an empty 200 would read as "this host has
// no metrics", which is a lie. 503 until there is something true to say.
func TestServesNothingBeforeFirstSample(t *testing.T) {
	s := New(nil, testOptions())

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestServesLastRender(t *testing.T) {
	c := &fakeCollector{name: "fake", collect: func(_ context.Context, out *exposition.Set) error {
		out.Gauge("prickle_fake", "A fake metric.").Add(42)
		return nil
	}}

	s := New([]collector.Collector{c}, testOptions())
	s.SampleOnce(context.Background())

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != contentType {
		t.Errorf("Content-Type = %q, want %q", got, contentType)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"prickle_fake 42",
		`prickle_collector_success{collector="fake"} 1`,
		`prickle_collector_errors_total{collector="fake"} 0`,
		`prickle_build_info{version="test"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// TestScrapeDoesNotCollect is the property SPEC.md §Architecture is built
// around: serving is decoupled from sampling, so a scrape storm cannot touch
// the filesystem even once.
func TestScrapeDoesNotCollect(t *testing.T) {
	c := &fakeCollector{name: "fake", collect: func(context.Context, *exposition.Set) error { return nil }}
	s := New([]collector.Collector{c}, testOptions())
	s.SampleOnce(context.Background())

	for i := 0; i < 50; i++ {
		s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))
	}
	if got := c.callCount(); got != 1 {
		t.Errorf("collector ran %d times for 50 scrapes, want 1", got)
	}
}

// TestErrorTotalsAccumulate: the Set is rebuilt every pass, so counter
// monotonicity has to be held by the sampler. A counter that resets every
// scrape makes rate() produce garbage.
func TestErrorTotalsAccumulate(t *testing.T) {
	failing := &fakeCollector{name: "bad", collect: func(_ context.Context, out *exposition.Set) error {
		out.Gauge("prickle_partial", "Written before the failure.").Add(1)
		return errors.New("boom")
	}}
	healthy := &fakeCollector{name: "good", collect: func(_ context.Context, out *exposition.Set) error {
		out.Gauge("prickle_healthy", "A healthy metric.").Add(1)
		return nil
	}}

	s := New([]collector.Collector{failing, healthy}, testOptions())
	for i := 1; i <= 3; i++ {
		s.SampleOnce(context.Background())
		body := string(s.Snapshot())

		want := `prickle_collector_errors_total{collector="bad"} ` + strconv.Itoa(i)
		if !strings.Contains(body, want) {
			t.Fatalf("pass %d: want %q in:\n%s", i, want, body)
		}
		if !strings.Contains(body, `prickle_collector_errors_total{collector="good"} 0`) {
			t.Errorf("pass %d: healthy collector's error counter moved", i)
		}
		// A failing collector must not cost the healthy one its metrics, nor
		// discard its own partial output.
		if !strings.Contains(body, "prickle_healthy 1") {
			t.Errorf("pass %d: healthy collector's metric missing", i)
		}
		if !strings.Contains(body, "prickle_partial 1") {
			t.Errorf("pass %d: partial output from the failing collector discarded", i)
		}
	}
}

// TestCollectorTimeout checks that a stuck collector is cut off at the
// deadline and the ones after it still run.
func TestCollectorTimeout(t *testing.T) {
	stuck := &fakeCollector{name: "stuck", collect: func(ctx context.Context, _ *exposition.Set) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	after := &fakeCollector{name: "after", collect: func(_ context.Context, out *exposition.Set) error {
		out.Gauge("prickle_after", "Ran after the stuck collector.").Add(1)
		return nil
	}}

	opts := testOptions()
	opts.Timeout = 20 * time.Millisecond
	s := New([]collector.Collector{stuck, after}, opts)

	done := make(chan struct{})
	go func() {
		s.SampleOnce(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SampleOnce did not return; the per-collector deadline is not being applied")
	}

	body := string(s.Snapshot())
	if !strings.Contains(body, "prickle_after 1") {
		t.Errorf("collector after the stuck one did not run:\n%s", body)
	}
	if !strings.Contains(body, `prickle_collector_success{collector="stuck"} 0`) {
		t.Errorf("stuck collector not reported as failed:\n%s", body)
	}
}

// TestRunSamplesImmediately: waiting a whole interval before the first pass
// would leave /metrics returning 503 for ten seconds after startup.
func TestRunSamplesImmediately(t *testing.T) {
	sampled := make(chan struct{}, 1)
	c := &fakeCollector{name: "fake", collect: func(context.Context, *exposition.Set) error {
		select {
		case sampled <- struct{}{}:
		default:
		}
		return nil
	}}

	opts := testOptions()
	opts.Interval = time.Hour // only the immediate first pass can fire
	s := New([]collector.Collector{c}, opts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	select {
	case <-sampled:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not sample before the first tick")
	}
}

// TestHeadRequest checks that a HEAD scrape gets the headers and no body.
func TestHeadRequest(t *testing.T) {
	c := &fakeCollector{name: "fake", collect: func(_ context.Context, out *exposition.Set) error {
		out.Gauge("prickle_fake", "A fake metric.").Add(1)
		return nil
	}}
	s := New([]collector.Collector{c}, testOptions())
	s.SampleOnce(context.Background())

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned a %d-byte body", rec.Body.Len())
	}
}

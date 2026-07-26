// SPDX-License-Identifier: Apache-2.0

package exposition

import (
	"math"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	s := NewSet(L("node", "n1"))
	g := s.Gauge("prickle_host_load1", "1-minute load average.")
	g.Add(0.07)

	c := s.Counter("prickle_host_cpu_seconds_total", "Seconds the CPUs spent in each mode.")
	c.Add(76.99, L("mode", "user"))
	c.Add(48405.71, L("mode", "idle"))

	want := `# HELP prickle_host_cpu_seconds_total Seconds the CPUs spent in each mode.
# TYPE prickle_host_cpu_seconds_total counter
prickle_host_cpu_seconds_total{node="n1",mode="user"} 76.99
prickle_host_cpu_seconds_total{node="n1",mode="idle"} 48405.71
# HELP prickle_host_load1 1-minute load average.
# TYPE prickle_host_load1 gauge
prickle_host_load1{node="n1"} 0.07
`
	if got := s.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if err := s.Err(); err != nil {
		t.Errorf("unexpected errors: %v", err)
	}
}

// TestFamilyOrderIsStable checks the property the golden files depend on:
// families sorted by name, samples in insertion order.
func TestFamilyOrderIsStable(t *testing.T) {
	build := func() string {
		s := NewSet()
		s.Gauge("zebra", "z").Add(1)
		s.Gauge("alpha", "a").Add(1)
		s.Gauge("middle", "m").Add(1)
		return s.String()
	}
	first := build()
	for i := 0; i < 20; i++ {
		if got := build(); got != first {
			t.Fatalf("render is not deterministic:\n%s\nvs\n%s", first, got)
		}
	}
	if !strings.HasPrefix(first, "# HELP alpha") {
		t.Errorf("families not sorted by name:\n%s", first)
	}
}

func TestNoLabelsEmitsNoBraces(t *testing.T) {
	s := NewSet()
	s.Gauge("bare", "b").Add(1)
	if got, want := s.String(), "# HELP bare b\n# TYPE bare gauge\nbare 1\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscaping(t *testing.T) {
	s := NewSet()
	s.Gauge("escaped", "help with \\ backslash\nand a newline").
		Add(1, L("path", `/mnt/a "b" \c`), L("nl", "x\ny"))

	want := `# HELP escaped help with \\ backslash\nand a newline
# TYPE escaped gauge
escaped{path="/mnt/a \"b\" \\c",nl="x\ny"} 1
`
	if got := s.String(); got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestFormatValue(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{-1, "-1"},
		// Exact integers print without an exponent: %g would render this
		// counter as 4.67416665e+08.
		{467416665, "467416665"},
		{253343494144, "253343494144"},
		{0.07, "0.07"},
		{4.788821, "4.788821"},
		{1.0 / 3.0, "0.3333333333333333"},
		{math.Inf(1), "+Inf"},
		{math.Inf(-1), "-Inf"},
		{math.NaN(), "NaN"},
		// Beyond the integer window, fall back to %g rather than overflow int64.
		{1e19, "1e+19"},
	} {
		if got := formatValue(tc.in); got != tc.want {
			t.Errorf("formatValue(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRejectsBadNames covers the promlint rules enforced at build time, so a
// bad name fails a unit test rather than `promtool check metrics` in CI.
func TestRejectsBadNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(*Set)
		want string
	}{
		{"camelCase", func(s *Set) { s.Gauge("prickle_memTotal", "h").Add(1) }, "snake_case"},
		{"empty name", func(s *Set) { s.Gauge("", "h").Add(1) }, "empty metric name"},
		{"leading digit", func(s *Set) { s.Gauge("1bad", "h").Add(1) }, "invalid character"},
		{"colon", func(s *Set) { s.Gauge("a:b", "h").Add(1) }, "invalid character"},
		{"no help", func(s *Set) { s.Gauge("prickle_x", "").Add(1) }, "no help text"},
		{"counter without _total", func(s *Set) { s.Counter("prickle_x", "h").Add(1) }, "must end in _total"},
		{"gauge with _total", func(s *Set) { s.Gauge("prickle_x_total", "h").Add(1) }, "must not end in _total"},
		{"reserved label", func(s *Set) { s.Gauge("prickle_x", "h").Add(1, L("__name__", "v")) }, "reserved __ prefix"},
		{"bad label name", func(s *Set) { s.Gauge("prickle_x", "h").Add(1, L("a-b", "v")) }, "invalid character"},
		// The two that a live kernel actually produced: /proc/meminfo's
		// SReclaimable and SecPageTables, converted generically.
		{"abbreviated s", func(s *Set) { s.Gauge("prickle_host_memory_s_reclaimable_bytes", "h").Add(1) }, `abbreviated unit "s"`},
		{"abbreviated sec", func(s *Set) { s.Gauge("prickle_host_memory_sec_page_tables_bytes", "h").Add(1) }, `abbreviated unit "sec"`},
		{"abbreviated kb", func(s *Set) { s.Gauge("prickle_host_memory_kb", "h").Add(1) }, `abbreviated unit "kb"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSet()
			tc.fn(s)
			err := s.Err()
			if err == nil {
				t.Fatalf("expected an error; rendered:\n%s", s.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if s.String() != "" {
				t.Errorf("a rejected family still rendered:\n%s", s.String())
			}
		})
	}
}

// TestDuplicateSeriesIsDropped: Prometheus rejects an entire scrape over one
// duplicate series, so the repeat is dropped and reported rather than emitted.
func TestDuplicateSeriesIsDropped(t *testing.T) {
	s := NewSet()
	g := s.Gauge("prickle_x", "h")
	g.Add(1, L("a", "1"))
	g.Add(2, L("a", "1"))
	g.Add(3, L("a", "2"))

	got := s.String()
	want := `# HELP prickle_x h
# TYPE prickle_x gauge
prickle_x{a="1"} 1
prickle_x{a="2"} 3
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if err := s.Err(); err == nil || !strings.Contains(err.Error(), "duplicate series") {
		t.Errorf("duplicate not reported: %v", err)
	}
}

// TestSameFamilyTwice: two collectors may contribute to one family, but not
// with a different type or help text.
func TestSameFamilyTwice(t *testing.T) {
	s := NewSet()
	s.Counter("prickle_x_total", "h").Add(1, L("a", "1"))
	s.Counter("prickle_x_total", "h").Add(2, L("a", "2"))
	if err := s.Err(); err != nil {
		t.Fatalf("consistent redeclaration rejected: %v", err)
	}
	if n := strings.Count(s.String(), "# TYPE"); n != 1 {
		t.Errorf("TYPE emitted %d times, want 1", n)
	}

	s = NewSet()
	s.Gauge("prickle_y", "one").Add(1)
	s.Gauge("prickle_y", "another").Add(2)
	if err := s.Err(); err == nil || !strings.Contains(err.Error(), "two different help texts") {
		t.Errorf("mismatched help not reported: %v", err)
	}
}

// TestEmptyFamilyIsSkipped: a HELP/TYPE pair with no samples is legal but says
// nothing, and would appear whenever a collector's source is absent.
func TestEmptyFamilyIsSkipped(t *testing.T) {
	s := NewSet()
	s.Gauge("prickle_declared_but_empty", "h")
	s.Gauge("prickle_used", "h").Add(1)

	if got := s.String(); strings.Contains(got, "prickle_declared_but_empty") {
		t.Errorf("empty family rendered:\n%s", got)
	}
}

// TestAddToRejectedFamilyIsSafe: the constructors return nil for a bad name,
// and a collector must not panic on it mid-scrape.
func TestAddToRejectedFamilyIsSafe(t *testing.T) {
	s := NewSet()
	f := s.Gauge("BadName", "h")
	if f != nil {
		t.Fatal("expected nil for a rejected name")
	}
	f.Add(1, L("a", "b")) // must not panic
}

// SPDX-License-Identifier: Apache-2.0

// Package exposition renders the Prometheus text exposition format by hand.
//
// SPEC.md §Hard constraints #1: the standard library only, which rules out
// prometheus/client_golang. The output must pass `promtool check metrics` at
// all times, so this package enforces what promlint enforces — snake_case
// names, a HELP line on every family, `_total` on every counter — at build
// time rather than letting a bad name reach a scrape.
//
// A Set is written by one goroutine (the sampler) and rendered once. It is not
// safe for concurrent use; the sampler's buffer swap is what makes the rendered
// bytes safe to serve.
package exposition

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Kind is a Prometheus metric type. Only the two the exporter emits exist here;
// histograms and summaries would need their own bucket/quantile handling.
type Kind uint8

const (
	// Gauge is a value that can go up or down.
	Gauge Kind = iota
	// Counter is a monotonically increasing value. Its name must end in _total.
	Counter
)

func (k Kind) String() string {
	if k == Counter {
		return "counter"
	}
	return "gauge"
}

// Label is one label name/value pair.
type Label struct {
	Name  string
	Value string
}

// L is shorthand for constructing a Label.
func L(name, value string) Label { return Label{Name: name, Value: value} }

// Set is a collection of metric families rendered as one exposition document.
type Set struct {
	constLabels []Label
	families    map[string]*Family
	errs        []error

	// scope bounds the current collector's contribution. Nil outside
	// BeginScope/EndScope, which is how the sampler's own self-metrics stay
	// uncapped — they are what report the breach, so capping them would be
	// the one loss that hides all the others.
	scope *scope
}

// scope is one collector's series budget for one pass.
//
// A single field on Set rather than a per-collector handle, because SampleOnce
// runs collectors sequentially on one goroutine — the same property that makes
// the whole package safe without a mutex. If that ever becomes parallel, this
// becomes wrong, loudly and immediately, rather than subtly.
type scope struct {
	limit   int
	added   int
	dropped int
}

// BeginScope starts counting samples against a budget of limit series. A limit
// of zero or less counts without bounding, so a caller can measure a collector
// without capping it.
//
// SPEC.md §Metrics contract: caps are per collector, and on breach the extra
// samples are dropped and counted. Dropping is the whole point — a collector
// whose cardinality has run away must cost its own series and not the scrape.
func (s *Set) BeginScope(limit int) {
	s.scope = &scope{limit: limit}
}

// EndScope stops counting and reports what happened: how many samples were
// kept, and how many were dropped for exceeding the budget.
func (s *Set) EndScope() (added, dropped int) {
	if s.scope == nil {
		return 0, 0
	}
	added, dropped = s.scope.added, s.scope.dropped
	s.scope = nil
	return added, dropped
}

// Family is one metric name with its help text, type, and samples.
type Family struct {
	set     *Set
	name    string
	help    string
	kind    Kind
	samples []sample
}

type sample struct {
	labels []Label
	value  float64
}

// NewSet returns an empty Set. constLabels are prepended to every sample in
// every family — this is how the `node` identity label from SPEC.md §Metrics
// contract is applied once rather than by each collector.
func NewSet(constLabels ...Label) *Set {
	s := &Set{
		constLabels: constLabels,
		families:    make(map[string]*Family),
	}
	for _, l := range constLabels {
		if err := validLabelName(l.Name); err != nil {
			s.errf("constant label: %w", err)
		}
	}
	return s
}

// Gauge returns the gauge family called name, creating it on first use.
func (s *Set) Gauge(name, help string) *Family { return s.family(name, help, Gauge) }

// Counter returns the counter family called name, creating it on first use.
func (s *Set) Counter(name, help string) *Family { return s.family(name, help, Counter) }

// family looks up or creates a family. Repeated calls with the same name are
// how two collectors contribute samples to one family; a mismatched type or
// help text between those calls is a bug and is recorded as an error.
func (s *Set) family(name, help string, kind Kind) *Family {
	if f, ok := s.families[name]; ok {
		if f.kind != kind {
			s.errf("metric %q declared as both %s and %s", name, f.kind, kind)
			return nil
		}
		if f.help != help {
			s.errf("metric %q declared with two different help texts", name)
			return nil
		}
		return f
	}
	if err := validMetricName(name); err != nil {
		s.errf("%w", err)
		return nil
	}
	if help == "" {
		// promlint: every metric family needs a HELP line.
		s.errf("metric %q has no help text", name)
		return nil
	}
	if kind == Counter && !strings.HasSuffix(name, "_total") {
		// promlint: counters are expected to carry the _total suffix.
		s.errf("counter %q must end in _total", name)
		return nil
	}
	if kind == Gauge && strings.HasSuffix(name, "_total") {
		s.errf("gauge %q must not end in _total", name)
		return nil
	}
	f := &Family{set: s, name: name, help: help, kind: kind}
	s.families[name] = f
	return f
}

// Add appends a sample. A nil Family — which is what the constructors return
// once a name has been rejected — silently drops the sample, so one bad family
// costs its own series and not a panic in the middle of a scrape.
//
// Inside a scope the sample is also counted against that collector's budget,
// and dropped once the budget is spent. The drop is deliberately silent here:
// a breach is reported once per pass on the self-metrics, not once per sample,
// because a runaway collector would otherwise turn its own cardinality problem
// into a log-volume problem.
func (f *Family) Add(value float64, labels ...Label) {
	if f == nil {
		return
	}
	for _, l := range labels {
		if err := validLabelName(l.Name); err != nil {
			f.set.errf("metric %q: %w", f.name, err)
			return
		}
	}
	if sc := f.set.scope; sc != nil {
		if sc.limit > 0 && sc.added >= sc.limit {
			sc.dropped++
			return
		}
		sc.added++
	}
	f.samples = append(f.samples, sample{labels: labels, value: value})
}

// Err reports every problem found while building the Set. The sampler logs it
// and still serves what rendered, on the principle that a partial scrape beats
// no scrape.
func (s *Set) Err() error { return errors.Join(s.errs...) }

func (s *Set) errf(format string, args ...any) {
	s.errs = append(s.errs, fmt.Errorf(format, args...))
}

// WriteTo renders the Set in the Prometheus text exposition format.
//
// Families are emitted in name order and samples in the order they were added,
// so the output is byte-stable across runs and can be checked against a golden
// file. Duplicate series within a family are dropped and recorded as an error:
// Prometheus rejects an entire scrape over one duplicate, so dropping the
// repeat is what keeps the rest of the payload ingestible.
func (s *Set) WriteTo(w io.Writer) (int64, error) {
	names := make([]string, 0, len(s.families))
	for name := range s.families {
		names = append(names, name)
	}
	sort.Strings(names)

	cw := &countingWriter{w: w}
	var buf bytes.Buffer
	for _, name := range names {
		f := s.families[name]
		if len(f.samples) == 0 {
			// A family with no samples would emit a bare HELP/TYPE pair.
			// Valid, but it tells a querier nothing, so skip it.
			continue
		}
		buf.Reset()
		buf.WriteString("# HELP ")
		buf.WriteString(f.name)
		buf.WriteByte(' ')
		writeEscapedHelp(&buf, f.help)
		buf.WriteByte('\n')
		buf.WriteString("# TYPE ")
		buf.WriteString(f.name)
		buf.WriteByte(' ')
		buf.WriteString(f.kind.String())
		buf.WriteByte('\n')

		seen := make(map[string]struct{}, len(f.samples))
		for _, sm := range f.samples {
			line := buf.Len()
			buf.WriteString(f.name)
			s.writeLabels(&buf, sm.labels)
			key := buf.String()[line:]
			if _, dup := seen[key]; dup {
				buf.Truncate(line)
				s.errf("metric %q: duplicate series %s", f.name, key)
				continue
			}
			seen[key] = struct{}{}
			buf.WriteByte(' ')
			buf.WriteString(formatValue(sm.value))
			buf.WriteByte('\n')
		}
		if _, err := io.WriteString(cw, buf.String()); err != nil {
			return cw.n, err
		}
	}
	return cw.n, nil
}

// writeLabels emits {a="1",b="2"}, constant labels first. An empty label set
// emits nothing at all rather than an empty pair of braces.
func (s *Set) writeLabels(buf *bytes.Buffer, labels []Label) {
	if len(s.constLabels) == 0 && len(labels) == 0 {
		return
	}
	buf.WriteByte('{')
	first := true
	write := func(l Label) {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.WriteString(l.Name)
		buf.WriteString(`="`)
		writeEscapedLabelValue(buf, l.Value)
		buf.WriteByte('"')
	}
	for _, l := range s.constLabels {
		write(l)
	}
	for _, l := range labels {
		write(l)
	}
	buf.WriteByte('}')
}

// String renders the Set to a string. Convenience for tests and `prickle
// diagnose`; the serving path uses WriteTo into a reusable buffer.
func (s *Set) String() string {
	var b strings.Builder
	_, _ = s.WriteTo(&b)
	return b.String()
}

// formatValue renders a float the way the text format expects.
//
// Exact integers are printed without an exponent: the shortest %g form of a
// byte counter is 4.67416665e+08, which is legal but makes golden files and
// `curl /metrics` output needlessly hard to read.
func formatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e18 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// writeEscapedHelp escapes a HELP text: backslash and newline only. A double
// quote is literal in help text.
func writeEscapedHelp(buf *bytes.Buffer, s string) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		default:
			buf.WriteByte(c)
		}
	}
}

// writeEscapedLabelValue escapes a label value: backslash, double quote and
// newline.
func writeEscapedLabelValue(buf *bytes.Buffer, s string) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			buf.WriteString(`\\`)
		case '"':
			buf.WriteString(`\"`)
		case '\n':
			buf.WriteString(`\n`)
		default:
			buf.WriteByte(c)
		}
	}
}

// validMetricName checks [a-z_][a-z0-9_]*.
//
// Stricter than the format's own [a-zA-Z_:][a-zA-Z0-9_:]*: promlint rejects
// camelCase, and a colon is reserved for recording rules. SPEC.md §Metrics
// contract requires snake_case, so an uppercase letter is a build-time error
// here rather than a promtool failure later.
func validMetricName(name string) error {
	if name == "" {
		return errors.New("empty metric name")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		case c >= 'A' && c <= 'Z':
			return fmt.Errorf("metric name %q is not snake_case", name)
		default:
			return fmt.Errorf("metric name %q contains invalid character %q", name, c)
		}
	}
	for _, part := range strings.Split(name, "_") {
		if _, bad := abbreviatedUnits[part]; bad {
			return fmt.Errorf("metric name %q contains the abbreviated unit %q; spell the word out", name, part)
		}
	}
	return nil
}

// abbreviatedUnits is promlint's list of unit abbreviations, which it rejects
// as name components.
//
// It is duplicated here rather than left to `promtool check metrics` because
// several of the names this exporter emits are derived from kernel field names
// at runtime — /proc/meminfo's SReclaimable becomes s_reclaimable, and a kernel
// newer than the fixture tree can introduce a field nobody has linted. Catching
// it here costs that one metric and a logged error; catching it in CI would
// mean a released binary whose whole endpoint fails lint on somebody's host.
//
// Fix a rejection by adding the field to meminfoAliases in the host collector,
// not by relaxing this check.
var abbreviatedUnits = map[string]struct{}{
	"s": {}, "ms": {}, "us": {}, "ns": {}, "sec": {},
	"b": {}, "kb": {}, "mb": {}, "gb": {}, "tb": {},
	"kib": {}, "mib": {}, "gib": {}, "tib": {},
}

// validLabelName checks [a-zA-Z_][a-zA-Z0-9_]* and rejects the __ prefix,
// which Prometheus reserves for itself.
func validLabelName(name string) error {
	if name == "" {
		return errors.New("empty label name")
	}
	if strings.HasPrefix(name, "__") {
		return fmt.Errorf("label name %q uses the reserved __ prefix", name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return fmt.Errorf("label name %q contains invalid character %q", name, c)
		}
	}
	return nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

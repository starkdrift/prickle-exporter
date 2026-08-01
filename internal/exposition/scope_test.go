// SPDX-License-Identifier: Apache-2.0

package exposition

import (
	"strconv"
	"strings"
	"testing"
)

// addN writes n samples to one family.
func addN(s *Set, n int) {
	f := s.Gauge("prickle_test_scope", "One series per label value.")
	for i := 0; i < n; i++ {
		f.Add(1, L("i", strconv.Itoa(i)))
	}
}

// TestOutsideAScopeNothingIsCounted: a Set with no scope open behaves exactly
// as it did before caps existed. This is the path `prickle diagnose` and every
// collector unit test take.
func TestOutsideAScopeNothingIsCounted(t *testing.T) {
	s := NewSet()
	addN(s, 5)

	added, dropped := s.EndScope()
	if added != 0 || dropped != 0 {
		t.Errorf("EndScope with no scope open = (%d, %d), want (0, 0)", added, dropped)
	}
	if got := strings.Count(s.String(), "prickle_test_scope{"); got != 5 {
		t.Errorf("rendered %d series, want 5 — an unscoped Set must not drop anything", got)
	}
}

// TestScopeEndsWhenItEnds guards against a budget leaking into whatever the
// caller does next. The sampler opens a scope per collector and emits its
// self-metrics between them; a scope that outlived EndScope would charge them
// to the collector that just ran.
func TestScopeEndsWhenItEnds(t *testing.T) {
	s := NewSet()

	s.BeginScope(2)
	addN(s, 6)
	added, dropped := s.EndScope()
	if added != 2 || dropped != 4 {
		t.Fatalf("EndScope = (%d, %d), want (2, 4)", added, dropped)
	}

	// Everything after this point is outside the budget.
	s.Gauge("prickle_test_after", "Written after the scope closed.").Add(1)
	if !strings.Contains(s.String(), "prickle_test_after") {
		t.Error("a sample written after EndScope was dropped; the budget outlived its scope")
	}
}

// TestScopeCountsAcrossFamilies: the cap is a per-collector series budget, not
// a per-family one. A collector emitting three families of ten is the same
// thirty series as one family of thirty.
func TestScopeCountsAcrossFamilies(t *testing.T) {
	s := NewSet()
	s.BeginScope(15)
	// Not single letters: `b` is in promlint's abbreviated-units list, so
	// prickle_test_b would be rejected at family creation and its Adds would
	// silently be no-ops — which looks exactly like the cap working.
	for _, name := range []string{"prickle_test_alpha", "prickle_test_beta", "prickle_test_gamma"} {
		f := s.Gauge(name, "Ten series.")
		for i := 0; i < 10; i++ {
			f.Add(1, L("i", strconv.Itoa(i)))
		}
	}
	added, dropped := s.EndScope()
	if added != 15 || dropped != 15 {
		t.Errorf("EndScope = (%d, %d), want (15, 15)", added, dropped)
	}
}

// TestScopeWithNoLimitStillCounts: a limit of zero measures without bounding,
// which is what makes prickle_collector_series meaningful when an operator has
// turned the cap off.
func TestScopeWithNoLimitStillCounts(t *testing.T) {
	s := NewSet()
	s.BeginScope(0)
	addN(s, 40)
	added, dropped := s.EndScope()
	if added != 40 || dropped != 0 {
		t.Errorf("EndScope = (%d, %d), want (40, 0)", added, dropped)
	}
}

// TestCappedSamplesAreNotErrors: a drop is a recorded condition, not a
// malformed document. Set.Err is how the sampler learns it built something
// invalid, and a cap firing must not make it start crying wolf.
func TestCappedSamplesAreNotErrors(t *testing.T) {
	s := NewSet()
	s.BeginScope(1)
	addN(s, 10)
	s.EndScope()

	if err := s.Err(); err != nil {
		t.Errorf("Set.Err after a cap fired = %v, want nil", err)
	}
}

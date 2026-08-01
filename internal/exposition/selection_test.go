// SPDX-License-Identifier: Apache-2.0

package exposition_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

func selectorFor(t *testing.T, preset string, include ...string) *exposition.Selector {
	t.Helper()
	sel, err := exposition.NewSelector(preset, include)
	if err != nil {
		t.Fatalf("NewSelector(%q, %v): %v", preset, include, err)
	}
	return sel
}

func rendered(t *testing.T, sel *exposition.Selector) (string, int) {
	t.Helper()
	s := exposition.NewSet()
	s.Select(sel)
	s.Gauge("prickle_host_load1", "Load.").Add(1)
	s.Gauge("prickle_host_procs_running", "Running.").Add(2)
	s.Counter("prickle_host_forks_total", "Forks.").Add(3)
	s.Gauge("prickle_container_info", "Info.").Add(1)
	s.Gauge("prickle_container_processes", "Procs.").Add(4)
	// Self-metrics, which no preset may withhold.
	s.Gauge("prickle_collector_series", "Series.").Add(5)
	s.Gauge("prickle_build_info", "Build.").Add(1)
	s.Gauge("prickle_render_timestamp_seconds", "Stamp.").Add(6)
	if err := s.Err(); err != nil {
		t.Fatalf("exposition problems: %v", err)
	}
	return s.String(), s.Withheld()
}

func TestFullExposesEverything(t *testing.T) {
	out, withheld := rendered(t, selectorFor(t, exposition.PresetFull))
	for _, want := range []string{"prickle_host_procs_running", "prickle_host_forks_total",
		"prickle_container_processes"} {
		if !strings.Contains(out, want) {
			t.Errorf("full preset withheld %s", want)
		}
	}
	if withheld != 0 {
		t.Errorf("full preset withheld %d families, want 0", withheld)
	}
}

func TestMinimalWithholdsTheRest(t *testing.T) {
	out, withheld := rendered(t, selectorFor(t, exposition.PresetMinimal))
	for _, want := range []string{"prickle_host_load1", "prickle_container_info"} {
		if !strings.Contains(out, want) {
			t.Errorf("minimal preset withheld %s, which the dashboards use", want)
		}
	}
	for _, gone := range []string{"prickle_host_procs_running", "prickle_host_forks_total",
		"prickle_container_processes"} {
		if strings.Contains(out, gone) {
			t.Errorf("minimal preset exposed %s, which no dashboard queries", gone)
		}
	}
	if withheld != 3 {
		t.Errorf("Withheld() = %d, want 3", withheld)
	}
}

// TestSelfMetricsSurviveEveryPreset is the counterpart of the cardinality cap's
// exemption. A scrape that has been reduced must still be able to say so, and
// prickle_build_info is the only in-band answer to "which build produced this".
func TestSelfMetricsSurviveEveryPreset(t *testing.T) {
	for _, tc := range []struct {
		name string
		sel  *exposition.Selector
	}{
		{"minimal", selectorFor(t, exposition.PresetMinimal)},
		{"custom matching nothing", selectorFor(t, exposition.PresetCustom, `^nothing_matches_this$`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := rendered(t, tc.sel)
			for _, want := range []string{"prickle_collector_series",
				"prickle_build_info", "prickle_render_timestamp_seconds"} {
				if !strings.Contains(out, want) {
					t.Errorf("%s withheld the self-metric %s", tc.name, want)
				}
			}
		})
	}
}

func TestCustomUsesTheGivenPatterns(t *testing.T) {
	out, _ := rendered(t, selectorFor(t, exposition.PresetCustom, `^prickle_host_forks_total$`))
	if !strings.Contains(out, "prickle_host_forks_total") {
		t.Error("custom preset withheld a family its pattern matches")
	}
	if strings.Contains(out, "prickle_host_load1") {
		t.Error("custom preset exposed a family no pattern matches")
	}
}

// TestMisusedFlagsAreErrors: an include list silently ignored would make an
// operator believe a metric was absent from the host rather than from the flag.
func TestMisusedFlagsAreErrors(t *testing.T) {
	for _, tc := range []struct {
		preset  string
		include []string
	}{
		{exposition.PresetFull, []string{"^x$"}},
		{exposition.PresetMinimal, []string{"^x$"}},
		{exposition.PresetCustom, nil},
		{"nonsense", nil},
		{exposition.PresetCustom, []string{"^([unclosed"}},
	} {
		if _, err := exposition.NewSelector(tc.preset, tc.include); err == nil {
			t.Errorf("NewSelector(%q, %v) succeeded; want an error", tc.preset, tc.include)
		}
	}
}

// TestMinimalCoversDashboards is the guard SPEC.md §Metrics contract requires:
// the dashboards must be a SUBSET of the minimal set, and the dependency runs
// that way round on purpose. Deriving the set from the dashboards instead would
// let an edit to a panel silently change what every scrape in a fleet returns.
func TestMinimalCoversDashboards(t *testing.T) {
	sel := selectorFor(t, exposition.PresetMinimal)

	root := filepath.Join("..", "..", "packaging", "grafana", "dashboards")
	files, err := filepath.Glob(filepath.Join(root, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no dashboards found; this test would pass vacuously")
	}

	metric := regexp.MustCompile(`prickle_[a-z0-9_]+`)
	missing := map[string][]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, name := range metric.FindAllString(string(b), -1) {
			if seen[name] {
				continue
			}
			seen[name] = true
			// Round-trip through a Set: exposes() is unexported, and this also
			// checks the behaviour a scrape would actually get.
			//
			// The constructor has to match the name. Set.Gauge rejects a
			// _total suffix and Set.Counter requires one — both return nil,
			// which is the same signal selection uses, so probing with the
			// wrong one reports every counter in the dashboards as withheld.
			s := exposition.NewSet()
			s.Select(sel)
			var f2 *exposition.Family
			if strings.HasSuffix(name, "_total") {
				f2 = s.Counter(name, "probe")
			} else {
				f2 = s.Gauge(name, "probe")
			}
			if f2 == nil {
				missing[filepath.Base(f)] = append(missing[filepath.Base(f)], name)
			}
		}
	}
	for f, names := range missing {
		sort.Strings(names)
		t.Errorf("%s queries metrics the minimal preset withholds: %v\n"+
			"    add them to minimalFamilies in selection.go, or stop querying them",
			f, names)
	}
}

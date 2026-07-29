// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// syntheticID is the container ID for the hand-written trees in this file. The
// captured tree cannot serve them: SPEC.md §Collectors' CPU quota is the one
// shape no cgroup on the rental had, so every cpu.max there reads `max 100000`
// and the whole quota path would go untested against fixtures alone
// (testdata/README.md §Coverage gaps).
const syntheticID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd01"

// throttledCPUStat is a cpu.stat from a container that has hit its quota.
//
// The capture has nr_periods=0 and nr_throttled=0 everywhere, because nothing
// on that host had a limit to be throttled against. The field layout is the
// captured one; only the values differ, and they are the arithmetic under test:
// 12500000µs is 12.5s, 8250000µs is 8.25s.
const throttledCPUStat = `usage_usec 12500000
user_usec 9000000
system_usec 3500000
nr_periods 4212
nr_throttled 317
throttled_usec 8250000
`

// collectScope writes one container cgroup holding exactly files, collects it,
// and returns the rendered document with the collector's error.
func collectScope(t *testing.T, files map[string]string) (string, error) {
	t.Helper()

	dir := t.TempDir()
	scope := filepath.Join(dir, "sys", "fs", "cgroup", "system.slice", "docker-"+syntheticID+".scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(scope, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	set := exposition.NewSet()
	err := New(Options{Roots: fsroot.At(dir)}).Collect(context.Background(), set)
	if setErr := set.Err(); setErr != nil {
		t.Fatalf("exposition problems: %v", setErr)
	}
	return set.String(), err
}

// sampleValue returns the value of the one sample of family in rendered, or ""
// when the family emitted none.
func sampleValue(t *testing.T, rendered, family string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(rendered, "\n") {
		if !strings.HasPrefix(line, family+"{") && line != family {
			continue
		}
		_, value, _ := strings.Cut(line, "} ")
		found = append(found, value)
	}
	if len(found) > 1 {
		t.Fatalf("%s emitted %d samples, want at most 1: %v", family, len(found), found)
	}
	if len(found) == 0 {
		return ""
	}
	return found[0]
}

// TestCPULimitFromQuota covers cpu.max.
//
//	max 100000
//	200000 100000
//
// Two fields: quota in microseconds per period, and the period. The reported
// limit is their ratio, in cores — the number the operator set and the number
// that compares against a rate() of cpu_usage_seconds_total.
//
// Every case also carries a valid cpu.stat, so each one additionally asserts
// the partial-collection contract from internal/collector: a malformed cpu.max
// costs its own sample and leaves the rest of the CPU family standing.
func TestCPULimitFromQuota(t *testing.T) {
	const family = "prickle_container_cpu_limit_cores"

	tests := []struct {
		name    string
		cpuMax  string
		want    string // expected sample value; "" means no sample at all
		wantErr bool
	}{
		{"unlimited — the only shape the capture contains", "max 100000\n", "", false},
		{"two whole cores", "200000 100000\n", "2", false},
		{"a fractional limit", "50000 100000\n", "0.5", false},
		{"a quota above one core, default period", "2500000 100000\n", "25", false},
		{"a non-default period", "150000 50000\n", "3", false},
		{"quota with no period", "400000\n", "", true},
		{"a zero period, which would divide by zero", "400000 0\n", "", true},
		{"a non-numeric quota", "lots 100000\n", "", true},
		{"a non-numeric period", "400000 often\n", "", true},
		{"an empty file", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := collectScope(t, map[string]string{
				"cpu.max":  tt.cpuMax,
				"cpu.stat": throttledCPUStat,
			})

			switch {
			case tt.wantErr && err == nil:
				t.Error("expected an error, so prickle_collector_errors_total rises")
			case !tt.wantErr && err != nil:
				t.Errorf("unexpected error: %v", err)
			}

			if got := sampleValue(t, out, family); got != tt.want {
				t.Errorf("%s = %q, want %q", family, got, tt.want)
			}

			// Whatever cpu.max said, cpu.stat was readable and must have been
			// reported: one bad file costs one metric, not the family.
			if got := sampleValue(t, out, "prickle_container_cpu_usage_seconds_total"); got != "12.5" {
				t.Errorf("cpu_usage_seconds_total = %q, want 12.5: a bad cpu.max cost an unrelated metric", got)
			}
		})
	}
}

// TestCPULimitIsAbsentWithoutTheFile checks that a cgroup with no cpu
// controller enabled is not an error. cpu.max is absent on a cgroup whose
// parent has not delegated cpu, which is a supported host, not a broken one.
func TestCPULimitIsAbsentWithoutTheFile(t *testing.T) {
	out, err := collectScope(t, map[string]string{"memory.current": "4096\n"})
	if err != nil {
		t.Errorf("a cgroup without the cpu controller is supported, not an error: %v", err)
	}
	for _, family := range []string{
		"prickle_container_cpu_limit_cores",
		"prickle_container_cpu_usage_seconds_total",
		"prickle_container_cpu_weight",
	} {
		if got := sampleValue(t, out, family); got != "" {
			t.Errorf("%s = %q with no cpu controller, want no sample", family, got)
		}
	}
	if got := sampleValue(t, out, "prickle_container_memory_usage_bytes"); got != "4096" {
		t.Errorf("memory_usage_bytes = %q, want 4096", got)
	}
}

// TestCPUThrottlingCounters covers the three counters that are zero throughout
// the captured tree, because nothing on that host had a quota to be throttled
// against. Without this they are emitted but their arithmetic is never checked
// against a value that would expose a wrong unit.
func TestCPUThrottlingCounters(t *testing.T) {
	out, err := collectScope(t, map[string]string{
		"cpu.max":    "200000 100000\n",
		"cpu.stat":   throttledCPUStat,
		"cpu.weight": "512\n",
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	for _, want := range []struct{ family, value string }{
		{"prickle_container_cpu_periods_total", "4212"},
		{"prickle_container_cpu_throttled_periods_total", "317"},
		{"prickle_container_cpu_throttled_seconds_total", "8.25"},
		{"prickle_container_cpu_usage_seconds_total", "12.5"},
		{"prickle_container_cpu_limit_cores", "2"},
		{"prickle_container_cpu_weight", "512"},
	} {
		if got := sampleValue(t, out, want.family); got != want.value {
			t.Errorf("%s = %q, want %q", want.family, got, want.value)
		}
	}

	// The mode split carries a label, so it needs the fuller match.
	for _, want := range []string{
		`mode="user"} 9`,
		`mode="system"} 3.5`,
	} {
		if !strings.Contains(out, "prickle_container_cpu_seconds_total{") || !strings.Contains(out, want) {
			t.Errorf("missing cpu_seconds_total sample %s in:\n%s", want, out)
		}
	}
}

// TestMicrosecondConversionIsExact pins the divisor in cpu.go.
//
// Every µs-to-second conversion there divides by 1e6 rather than multiplying by
// 1e-6, because 1e6 is exactly representable in float64 and 1e-6 is not. At the
// values a real container reports the two agree — 8250000µs renders as 8.25
// either way — so nothing above would notice the difference. They diverge in
// the single-digit microseconds, which is where a container throttled briefly
// actually lands, and where a multiply prints 4.9999999999999996e-06 for what
// is exactly five microseconds.
func TestMicrosecondConversionIsExact(t *testing.T) {
	out, err := collectScope(t, map[string]string{
		"cpu.stat": "usage_usec 5\nuser_usec 5\nsystem_usec 0\nthrottled_usec 5\n",
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, family := range []string{
		"prickle_container_cpu_usage_seconds_total",
		"prickle_container_cpu_throttled_seconds_total",
	} {
		if got := sampleValue(t, out, family); got != "5e-06" {
			t.Errorf("%s = %q, want %q — convert by dividing by 1e6, not by multiplying by 1e-6",
				family, got, "5e-06")
		}
	}
}

// TestMalformedFlatKeyedCostsOneKey checks the promise in readFlatKeyed: a key
// whose value does not parse is reported and the rest of the file still lands.
// A kernel newer than the captured one can add a key in a shape this does not
// expect, and that must cost one metric rather than the container's whole
// memory family.
func TestMalformedFlatKeyedCostsOneKey(t *testing.T) {
	out, err := collectScope(t, map[string]string{
		"memory.current": "8192\n",
		"memory.stat": `anon 4096
file not-a-number
slab 2048
`,
	})
	if err == nil {
		t.Error("expected the unparsable key to be reported")
	} else if !strings.Contains(err.Error(), "file") {
		t.Errorf("error does not name the offending key: %v", err)
	}

	if got := sampleValue(t, out, "prickle_container_memory_anon_bytes"); got != "4096" {
		t.Errorf("memory_anon_bytes = %q, want 4096: a bad key cost a good one", got)
	}
	if got := sampleValue(t, out, "prickle_container_memory_slab_bytes"); got != "2048" {
		t.Errorf("memory_slab_bytes = %q, want 2048: parsing stopped at the bad key", got)
	}
	if got := sampleValue(t, out, "prickle_container_memory_file_bytes"); got != "" {
		t.Errorf("memory_file_bytes = %q, want no sample for the unparsable key", got)
	}
}

// TestMalformedLimitIsReported covers readLimit's third case: a limit file that
// is neither "max" nor an integer.
func TestMalformedLimitIsReported(t *testing.T) {
	out, err := collectScope(t, map[string]string{
		"memory.current": "8192\n",
		"memory.max":     "unlimited\n",
	})
	if err == nil {
		t.Fatal("expected an error for a limit that is neither max nor a number")
	}
	if got := sampleValue(t, out, "prickle_container_memory_limit_bytes"); got != "" {
		t.Errorf("memory_limit_bytes = %q, want no sample", got)
	}
	if got := sampleValue(t, out, "prickle_container_memory_usage_bytes"); got != "8192" {
		t.Errorf("memory_usage_bytes = %q, want 8192", got)
	}
}

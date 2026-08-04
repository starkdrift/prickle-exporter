// SPDX-License-Identifier: Apache-2.0

package exposition

import (
	"fmt"
	"regexp"
	"strings"
)

// Preset names for -metrics.preset (SPEC.md §Metrics contract).
const (
	PresetMinimal = "minimal"
	PresetFull    = "full"
	PresetCustom  = "custom"
)

// alwaysExposed are the families no preset may withhold.
//
// The same reasoning as the cardinality cap's exemption: these are how a scrape
// says something about itself, and a reduced scrape that cannot report being
// reduced is indistinguishable from a healthy smaller one. prickle_build_info
// also carries the version, without which "which build produced this" has no
// answer at scrape time.
var alwaysExposed = []string{
	"prickle_collector_",
	"prickle_build_info",
	"prickle_render_timestamp_seconds",
}

// minimalFamilies is the default set: what the four shipped dashboards query.
//
// **This list is the source of truth, not the dashboards.** Deriving it from
// them would mean an edit to a panel silently changed what every scrape in a
// fleet returns. TestMinimalCoversDashboards asserts the dashboards are a
// subset of this, so the two cannot drift apart without failing the build —
// the dependency runs that way round on purpose.
//
// Anchored patterns rather than a name list because the host memory families
// are generated from /proc/meminfo's own field names, which vary by kernel and
// cannot be enumerated ahead of time.
var minimalFamilies = []string{
	// Host: what Node Overview draws.
	`^prickle_host_cpu_seconds_total$`,
	`^prickle_host_memory_(mem_total|mem_available|cached)_bytes$`,
	`^prickle_host_load1$`,
	`^prickle_host_disk_(read|written)_bytes_total$`,
	`^prickle_host_network_(receive|transmit)_bytes_total$`,
	`^prickle_host_pressure_stalled_seconds_total$`,

	// Containers: what Container Resources draws.
	`^prickle_container_info$`,
	`^prickle_container_cpu_usage_seconds_total$`,
	`^prickle_container_cpu_throttled_(seconds|periods)_total$`,
	`^prickle_container_memory_(usage|limit)_bytes$`,
	`^prickle_container_io_(read|written)_bytes_total$`,

	// GPU: what GPU Tenancy draws.
	`^prickle_gpu_info$`,
	`^prickle_gpu_(utilization_ratio|temperature_celsius|power_watts)$`,
	`^prickle_gpu_memory_(used|total)_bytes$`,
	`^prickle_gpu_mig_(info|memory_used_bytes)$`,
	`^prickle_gpu_amd_partition_info$`,
	`^prickle_gpu_nvidia_source_info$`,
	`^prickle_gpu_process_memory_bytes$`,
}

// Selector decides which metric families a Set will expose.
type Selector struct {
	// nil means expose everything.
	allow []*regexp.Regexp
}

// NewSelector builds a Selector for a preset. include is used only by
// PresetCustom, and supplying it with any other preset is an error rather than
// a silent no-op: an operator who wrote a filter and had it ignored would
// believe a metric was absent from the host rather than from the flag.
func NewSelector(preset string, include []string) (*Selector, error) {
	switch preset {
	case PresetFull:
		if len(include) > 0 {
			return nil, fmt.Errorf("-metrics.include is only meaningful with -metrics.preset=%s", PresetCustom)
		}
		return &Selector{}, nil

	case PresetMinimal, "":
		if len(include) > 0 {
			return nil, fmt.Errorf("-metrics.include is only meaningful with -metrics.preset=%s", PresetCustom)
		}
		return compile(minimalFamilies)

	case PresetCustom:
		if len(include) == 0 {
			return nil, fmt.Errorf("-metrics.preset=%s needs at least one -metrics.include pattern", PresetCustom)
		}
		return compile(include)

	default:
		return nil, fmt.Errorf("unknown -metrics.preset %q: want %s, %s or %s",
			preset, PresetMinimal, PresetFull, PresetCustom)
	}
}

func compile(patterns []string) (*Selector, error) {
	s := &Selector{allow: make([]*regexp.Regexp, 0, len(patterns))}
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("metric pattern %q: %w", p, err)
		}
		s.allow = append(s.allow, re)
	}
	return s, nil
}

// exposes reports whether a family name survives the selection.
func (s *Selector) exposes(name string) bool {
	if s == nil || s.allow == nil {
		return true
	}
	for _, p := range alwaysExposed {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	for _, re := range s.allow {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

// MinimalPatterns returns the default set's patterns, for tests and for
// `prickle diagnose` to describe what it is withholding.
func MinimalPatterns() []string { return append([]string(nil), minimalFamilies...) }

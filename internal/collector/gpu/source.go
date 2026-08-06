// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"errors"
	"fmt"
)

// The values -collector.gpu.nvidia-source accepts.
const (
	// SourceAuto tries NVML first and falls back to nvidia-smi.
	SourceAuto = "auto"
	// SourceNVML forces the dlopen path, failing when it will not load.
	SourceNVML = "nvml"
	// SourceSMI forces the subprocess path.
	SourceSMI = "smi"
)

// ErrUnavailable is what a source constructor returns when its path is not
// available on this host or in this build.
var ErrUnavailable = errors.New("source unavailable")

// selectSource picks the NVIDIA implementation, once, at startup.
//
// SPEC.md §Collectors fixes the order: attempt the NVML load, fall back
// silently to nvidia-smi. "Silently" means no log line and no error on a host
// that simply has no driver libraries — the fallback is a supported
// configuration, not a degraded one — but the reason is kept so `prickle
// diagnose` can state it when asked.
//
// A returned nil source with a nil error is a host with no NVIDIA GPU at all.
// That is not a failure; the collector emits nothing.
//
// The second return is the candidates that declined *before* the live one, and
// it is returned on success as well as on failure. It used to be discarded the
// moment something loaded, which left `prickle diagnose` unable to answer the
// question SPEC.md §Collectors requires it to answer — "when NVML failed to
// load, why" — in the one case where an operator actually asks it. A host where
// NVML fails and nvidia-smi succeeds is the documented common case, not an edge
// one: it is every slim container without the driver libraries. Reported as
// `live source: smi` with no reason given, that is indistinguishable from a
// deliberate choice.
func selectSource(opts Options) (nvidiaSource, []error, error) {
	var attempted []error

	for _, candidate := range candidates(opts) {
		source, err := candidate.build(opts)
		if err == nil {
			return source, attempted, nil
		}
		attempted = append(attempted, fmt.Errorf("%s: %w", candidate.name, err))
	}

	if len(attempted) == 0 {
		return nil, nil, fmt.Errorf("unknown NVIDIA source %q; want %s, %s or %s",
			opts.NVIDIASource, SourceAuto, SourceNVML, SourceSMI)
	}
	return nil, attempted, errors.Join(attempted...)
}

// sourceCandidate is one implementation and how to construct it.
type sourceCandidate struct {
	name  string
	build func(Options) (nvidiaSource, error)
}

// candidates returns the implementations to try, in order.
func candidates(opts Options) []sourceCandidate {
	if opts.nvidiaCandidates != nil {
		return opts.nvidiaCandidates(opts)
	}
	nvml := sourceCandidate{SourceNVML, newNVMLSource}
	smi := sourceCandidate{SourceSMI, newSMISource}

	switch opts.NVIDIASource {
	case SourceAuto:
		return []sourceCandidate{nvml, smi}
	case SourceNVML:
		return []sourceCandidate{nvml}
	case SourceSMI:
		return []sourceCandidate{smi}
	default:
		return nil
	}
}

// SourceName reports which implementation is live, or "" when none loaded.
// For `prickle diagnose`.
func (c *Collector) SourceName() string {
	if c.source == nil {
		return ""
	}
	return c.source.Name()
}

// SelectionError reports why no source loaded. Nil when one did.
func (c *Collector) SelectionError() error { return c.selectErr }

// DeclinedSources reports the implementations that were tried and refused
// before the live one, in the order they were tried. Empty when the first
// choice loaded, and empty on a build or a host where there was nothing else to
// try.
//
// For `prickle diagnose`: a live source says which path is serving metrics, and
// this says what the preferred path had to say for itself. On a static build
// that is "nvml: source unavailable: this binary is not built with NVML", which
// is a statement about the artifact; on the nvml build it is the dlopen error,
// which is a statement about the host. An operator deciding whether to install
// the other artifact needs to tell those apart.
func (c *Collector) DeclinedSources() []error { return c.declined }

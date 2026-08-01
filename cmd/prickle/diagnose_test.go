// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/starkdrift/prickle-exporter/internal/collector/gpu"
)

// TestDescribeSMITimeout covers the hint added after an idle H100 showed the
// nvidia-smi source overrunning its deadline on every pass — a failure that
// reads as a broken nvidia-smi and is really an uninitialised driver.
func TestDescribeSMITimeout(t *testing.T) {
	cfg := config{timeout: 5 * time.Second}

	tests := []struct {
		name   string
		source string
		err    error
		want   bool
	}{{
		// What the subprocess reports when the deadline kills it. Both
		// spellings occur: whichever side notices the overrun first wins.
		name:   "smi killed by the deadline",
		source: gpu.SourceSMI,
		err:    errors.New("nvidia-smi: signal: killed"),
		want:   true,
	}, {
		name:   "smi reporting the context error",
		source: gpu.SourceSMI,
		err:    fmt.Errorf("nvidia-smi: %w", context.DeadlineExceeded),
		want:   true,
	}, {
		// A real nvidia-smi failure must not be explained away as a latency
		// problem — the advice would send an operator to the wrong place.
		name:   "smi failing for some other reason",
		source: gpu.SourceSMI,
		err:    errors.New("nvidia-smi: exit status 2: unrecognised option"),
		want:   false,
	}, {
		// NVML holds the library open, so it does not have this problem and
		// must not be told to enable persistence mode.
		name:   "nvml timing out",
		source: gpu.SourceNVML,
		err:    fmt.Errorf("nvml: %w", context.DeadlineExceeded),
		want:   false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			describeSMITimeout(&b, cfg, tt.source, tt.err)

			got := b.String() != ""
			if got != tt.want {
				t.Fatalf("hint printed = %v, want %v; output:\n%s", got, tt.want, b.String())
			}
			if !tt.want {
				return
			}
			for _, want := range []string{"persistence mode", "prickle-nvml", "-collector.timeout", "5s"} {
				if !strings.Contains(b.String(), want) {
					t.Errorf("hint does not mention %q:\n%s", want, b.String())
				}
			}
		})
	}
}

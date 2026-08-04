// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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

// TestDescribePodNames covers the section added after `prickle diagnose`
// printed a wholly healthy report on a node where every pod name was missing.
//
// Pod-name resolution fails silently by design — an unreadable directory is
// not a collection error, because only the names are lost — so this subcommand
// is the only place that can say so. A test that let the permission case go
// unmentioned would let that silence back in.
func TestDescribePodNames(t *testing.T) {
	const path = "/host/var/log/pods"

	tests := []struct {
		name     string
		on       bool
		st       podLogs
		inPod    int
		podNamed int
		want     []string
		notWant  []string
	}{{
		name:    "off says how to turn it on",
		on:      false,
		st:      podLogs{path: path},
		want:    []string{"pod names: off", "-collector.container.pod-names"},
		notWant: []string{"READING NOTHING"},
	}, {
		// The whole point of the section.
		name:  "unreadable is stated loudly, with the uid and the fix",
		on:    true,
		st:    podLogs{path: path, err: &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}},
		inPod: 18,
		want: []string{
			"ON, AND READING NOTHING", path, "permission denied",
			"uid=", "gid=", "runAsGroup: 0", "bounding set", "18",
		},
	}, {
		// A plain server running podman is not a broken Kubernetes node.
		name:    "absent directory is not an alarm",
		on:      true,
		st:      podLogs{path: path, err: &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}},
		want:    []string{"does not exist", "not a Kubernetes node"},
		notWant: []string{"READING NOTHING", "permission denied"},
	}, {
		name:    "some other error is passed through rather than guessed at",
		on:      true,
		st:      podLogs{path: path, err: errors.New("input/output error")},
		want:    []string{"could not be listed", "input/output error"},
		notWant: []string{"READING NOTHING"},
	}, {
		name:    "no container is in a pod",
		on:      true,
		st:      podLogs{path: path, dirs: 3},
		inPod:   0,
		want:    []string{"nothing to resolve"},
		notWant: []string{"READING NOTHING"},
	}, {
		// A pruned log directory loses one name; that is not the privilege
		// problem and must not be reported as one.
		name:     "a partial resolution explains the gap",
		on:       true,
		st:       podLogs{path: path, dirs: 12},
		inPod:    14,
		podNamed: 12,
		want:     []string{"12 of 14", "pruned"},
		notWant:  []string{"READING NOTHING", "permission denied"},
	}, {
		name:     "everything resolved says so plainly",
		on:       true,
		st:       podLogs{path: path, dirs: 14},
		inPod:    18,
		podNamed: 18,
		want:     []string{"pod names: on", "18 of 18", path},
		notWant:  []string{"pruned", "READING NOTHING"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			describePodNames(&b, config{podNames: tt.on}, tt.st, tt.inPod, tt.podNamed)
			got := b.String()

			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("output is missing %q\ngot:\n%s", want, got)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("output should not contain %q\ngot:\n%s", notWant, got)
				}
			}
		})
	}
}

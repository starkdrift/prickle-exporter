// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/fsroot"
)

// DefaultSMICommand is the binary the fallback source spawns.
const DefaultSMICommand = "nvidia-smi"

// queryGPUFields is the --query-gpu column set, in order.
//
// This is the exact query scripts/capture-fixtures.sh runs, so the captured
// CSV is a faithful record of what this code parses. Changing it without a new
// capture would leave the fixture describing a format the code no longer reads
// (SPEC.md §Testing rules).
const queryGPUFields = "index,uuid,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw"

// queryProcessFields is the --query-compute-apps column set, in order.
//
// The pid column is requested because the captured fixture requested it, and
// is discarded in parseComputeApps. See the package comment.
const queryProcessFields = "gpu_uuid,pid,process_name,used_gpu_memory"

// commandRunner executes nvidia-smi.
//
// A subprocess is not a file and cannot be pointed at a fixture tree, so it
// goes behind an interface for the same reason Statfser does in the host
// collector (SPEC.md §Collectors). Tests replay captured output through it.
type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// execRunner runs the real binary.
type execRunner struct{}

// Run executes name with args and returns its stdout.
//
// SPEC.md §Hard constraints #2 permits spawning nvidia-smi and nothing else;
// this is the only place in the exporter that starts a process. Stderr is
// folded into the error rather than the output, because nvidia-smi writes its
// diagnostics there and an operator needs them to know why a query failed.
func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	stdout, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return stdout, nil
}

// smiSource reads the devices by spawning nvidia-smi.
//
// SPEC.md §Collectors calls this the fallback and explicitly not deprecated:
// it is what works inside slim containers that lack the driver libraries, and
// it stays tested. Its two verified limitations — bracketed [N/A] utilization
// while MIG is on, and the parent GPU UUID for MIG-resident processes — are
// handled at the parse boundary, not papered over.
type smiSource struct {
	command string
	runner  commandRunner
	// roots resolves /proc for the cgroup lookup that attributes a process to
	// a container. The same prefix the NVML source uses for the exe symlink.
	roots fsroot.Roots
}

// newSMISource returns the nvidia-smi source, or an error when the binary is
// not on PATH.
func newSMISource(opts Options) (nvidiaSource, error) {
	command := opts.SMICommand
	if command == "" {
		command = DefaultSMICommand
	}
	runner := opts.runner
	if runner == nil {
		if _, err := exec.LookPath(command); err != nil {
			return nil, fmt.Errorf("%s not found: %w", command, err)
		}
		runner = execRunner{}
	}
	return &smiSource{command: command, runner: runner, roots: opts.Roots}, nil
}

// Name implements nvidiaSource.
func (s *smiSource) Name() string { return SourceSMI }

// Close implements nvidiaSource. The source holds nothing open — each Read is
// its own subprocess.
func (s *smiSource) Close() error { return nil }

// Read implements nvidiaSource with three queries.
//
// Each is independent: a failing MIG listing costs the MIG topology and leaves
// the device metrics standing, because a host whose driver refuses `mig -lgi`
// still reports memory and power.
func (s *smiSource) Read(ctx context.Context) (snapshot, error) {
	var snap snapshot
	var errs []error

	out, err := s.runner.Run(ctx, s.command,
		"--query-gpu="+queryGPUFields, "--format=csv,noheader,nounits")
	if err != nil {
		// Without the device list there is nothing to attach anything else to.
		return snapshot{}, err
	}
	snap.devices, err = parseQueryGPU(string(out))
	if err != nil {
		errs = append(errs, err)
	}

	// MIG topology comes from `nvidia-smi -L`, and so does the answer to
	// whether MIG is on at all: the captured --query-gpu field set has no
	// mig.mode column, and adding one would mean parsing output no capture
	// records. -L is definitive either way — a partitioned card lists its
	// instances underneath itself, a Default-mode card lists none — and it is
	// the only captured output carrying MIG UUIDs. `mig -lgi` has the instance
	// IDs but no UUID, and a UUID is the identity SPEC.md §Metrics contract
	// requires.
	if out, err := s.runner.Run(ctx, s.command, "-L"); err != nil {
		errs = append(errs, err)
	} else if err := attachMIG(snap.devices, string(out)); err != nil {
		errs = append(errs, err)
	}

	if out, err := s.runner.Run(ctx, s.command,
		"--query-compute-apps="+queryProcessFields, "--format=csv,noheader,nounits"); err != nil {
		errs = append(errs, err)
	} else {
		snap.processes, err = parseComputeApps(string(out), s.containerOf)
		if err != nil {
			errs = append(errs, err)
		}
	}

	return snap, errors.Join(errs...)
}

// containerOf implements resolveContainer for the nvidia-smi source.
func (s *smiSource) containerOf(pid uint32) string { return containerOfPID(s.roots, pid) }

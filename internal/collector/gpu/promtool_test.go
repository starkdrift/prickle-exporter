// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// TestPromtoolCheckMetrics runs the gate SPEC.md §Metrics contract requires the
// output to pass at all times.
//
// A developer without promtool gets a skip; CI does not, because ci/check.sh
// fails when promtool is missing.
func TestPromtoolCheckMetrics(t *testing.T) {
	promtool, err := exec.LookPath("promtool")
	if err != nil {
		t.Skip("promtool not in PATH; ci/check.sh enforces this gate")
	}

	cmd := exec.Command(promtool, "check", "metrics")
	cmd.Stdin = strings.NewReader(collectFixture(t, func(o *Options) { o.PerProcess = true }))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		t.Errorf("promtool check metrics failed (%v):\n%s", err, out.String())
	}
}

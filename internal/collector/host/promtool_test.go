// SPDX-License-Identifier: Apache-2.0

package host

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// TestPromtoolCheckMetrics runs the gate SPEC.md §Metrics contract requires the
// output to pass at all times.
//
// It lives in the test suite rather than only in CI because promlint's rules
// are not all expressible in the exposition package's build-time validation —
// the abbreviated-unit check is what caught `s_reclaimable`, and nothing in the
// Go code would have. A developer without promtool gets a skip; CI does not,
// because ci/check.sh fails when promtool is missing.
func TestPromtoolCheckMetrics(t *testing.T) {
	promtool, err := exec.LookPath("promtool")
	if err != nil {
		t.Skip("promtool not in PATH; ci/check.sh enforces this gate")
	}

	cmd := exec.Command(promtool, "check", "metrics")
	cmd.Stdin = strings.NewReader(collectFixture(t))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		t.Errorf("promtool check metrics failed (%v):\n%s", err, out.String())
	}
}

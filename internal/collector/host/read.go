// SPDX-License-Identifier: Apache-2.0

package host

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// readLines reads a whole procfs file and splits it into lines.
//
// Files under /proc report size 0 and must be read in one shot rather than
// stat'ed and streamed — the same reason scripts/capture-fixtures.sh uses `cat`
// and not `cp`. They are also small enough that a single ReadFile is both the
// simplest and the most consistent option: the kernel renders the file
// atomically per read, so one call cannot observe a half-updated snapshot.
func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSuffix(string(b), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// parseUint parses a procfs unsigned field, naming the file and field in the
// error so a malformed line is diagnosable without a debugger.
func parseUint(path, field, s string) (uint64, error) {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: field %s: %q is not an unsigned integer", path, field, s)
	}
	return v, nil
}

// parseFloat parses a procfs decimal field.
func parseFloat(path, field, s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: field %s: %q is not a number", path, field, s)
	}
	return v, nil
}

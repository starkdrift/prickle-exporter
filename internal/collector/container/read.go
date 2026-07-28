// SPDX-License-Identifier: Apache-2.0

package container

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/starkdrift/prickle-exporter/internal/exposition"
)

// unlimited is what a cgroup v2 limit file holds when no limit is set.
const unlimited = "max"

// readFile reads a whole cgroup file and trims the trailing newline.
//
// Like procfs, cgroup files report size 0 and must be read in one shot rather
// than stat'ed and streamed — the same reason scripts/capture-fixtures.sh uses
// `cat` and not `cp`.
func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(b), "\n"), nil
}

// readLines reads a cgroup file and splits it into non-empty lines.
func readLines(path string) ([]string, error) {
	text, err := readFile(path)
	if err != nil || text == "" {
		return nil, err
	}
	return strings.Split(text, "\n"), nil
}

// readFlatKeyed parses a cgroup v2 flat-keyed file — one `key value` pair per
// line, as in cpu.stat and memory.stat:
//
//	usage_usec 51099
//	user_usec 22710
//
// Values are read as unsigned integers, which every key in both files is. An
// unparsable line is reported and the rest of the file still returns: a kernel
// newer than the captured one may add a key in a shape this does not expect,
// and that must cost one metric rather than the container's whole memory
// family.
func readFlatKeyed(path string) (map[string]uint64, error) {
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}
	values := make(map[string]uint64, len(lines))
	var bad []string
	for _, line := range lines {
		key, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		v, err := strconv.ParseUint(rest, 10, 64)
		if err != nil {
			bad = append(bad, key)
			continue
		}
		values[key] = v
	}
	if len(bad) > 0 {
		return values, fmt.Errorf("%s: unparsable value for %s", path, strings.Join(bad, ", "))
	}
	return values, nil
}

// readLimit reads a cgroup limit file — memory.max, pids.max, memory.high.
//
// The literal `max` means no limit, and is reported as set=false rather than as
// a huge number: exposing the kernel's internal sentinel as a byte count would
// put a 9.2-exabyte memory limit on every unconstrained container.
func readLimit(path string) (value uint64, set bool, err error) {
	text, err := readFile(path)
	if err != nil {
		return 0, false, err
	}
	if text == unlimited || text == "" {
		return 0, false, nil
	}
	v, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%s: %q is neither %q nor an unsigned integer", path, text, unlimited)
	}
	return v, true, nil
}

// parseUint parses a cgroup field, naming the file in the error so a malformed
// value is diagnosable without a debugger.
func parseUint(path, s string) (uint64, error) {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an unsigned integer", path, s)
	}
	return v, nil
}

// with returns the identity labels plus one dimensional label, in a slice of
// its own. Appending to the caller's slice would let two containers' label sets
// share a backing array and overwrite each other.
func with(labels []exposition.Label, extra exposition.Label) []exposition.Label {
	out := make([]exposition.Label, len(labels)+1)
	copy(out, labels)
	out[len(labels)] = extra
	return out
}

// skipMissing turns "this controller is not enabled here" into no error.
//
// Not every cgroup carries every file: io.stat is absent without the io
// controller, and memory.pressure without CONFIG_PSI. Those are supported
// configurations, so they cost their own samples and nothing else.
func skipMissing(err error) error {
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}

// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is two directories up from cmd/prickle.
const repoRoot = "../.."

// registeredFlags is the flag surface as the binary actually defines it.
func registeredFlags(t *testing.T) map[string]bool {
	t.Helper()
	fs := flag.NewFlagSet("prickle", flag.ContinueOnError)
	var cfg config
	cfg.register(fs)
	out := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { out[f.Name] = true })
	return out
}

// flagRef matches a flag as the shipped packaging spells it: one leading dash,
// a dotted name. Go's flag package accepts one dash or two identically, and the
// packaging uses one throughout.
var flagRef = regexp.MustCompile(`-((?:collector|path|web|metrics|sample|log)\.[a-z0-9.-]+)`)

// TestPackagingReferencesOnlyRealFlags couples the shipped artifacts to the
// binary's flag surface.
//
// The units, the chart, the compose file and the docs all pass flags to a
// binary none of them can typecheck against. Renaming a flag in config.go
// leaves every one of them referring to a flag that no longer exists, and
// nothing in the build notices: the Go tests pass, the chart still templates,
// and the failure surfaces as a pod that will not start.
//
// That is not hypothetical. The 0.6.0 chart passed
// -collector.container.pod-names to an image whose binary predated the flag,
// and the whole deployment crash-looped with `flag provided but not defined`.
// That particular case was version skew rather than a rename — the chart's
// appVersion pin is what addresses it — but it is the same failure, reached by
// the other route, and this closes the route a test can see.
func TestPackagingReferencesOnlyRealFlags(t *testing.T) {
	defined := registeredFlags(t)

	roots := []string{
		filepath.Join(repoRoot, "packaging"),
		filepath.Join(repoRoot, "docs"),
		filepath.Join(repoRoot, "README.md"),
	}

	checked := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			// Dashboard JSON contains PromQL, not command lines, and its
			// metric names would never match flagRef anyway. Skipping it keeps
			// the failure message honest about where a reference came from.
			if strings.HasSuffix(path, ".json") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range flagRef.FindAllStringSubmatch(string(body), -1) {
				// Prose ends sentences with the flag name: "...is
				// -collector.gpu.per-process." The trailing stop is
				// punctuation, not part of the flag.
				name := strings.TrimRight(m[1], ".")
				checked++
				if !defined[name] {
					t.Errorf("%s references -%s, which the binary does not define",
						filepath.Base(path), name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// A regexp that silently stops matching would turn this test into a
	// tautology that passes on an empty set.
	if checked == 0 {
		t.Fatal("found no flag references at all; flagRef has stopped matching")
	}
	t.Logf("checked %d flag references against %d defined flags", checked, len(defined))
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	// flag.ContinueOnError writes the usage message to the flag set's output,
	// which defaults to stderr. Nothing here asserts on it; the contract is
	// that run returns an error rather than starting a server.
	if err := run([]string{"-no-such-flag"}); err == nil {
		t.Error("run accepted an undefined flag; it must fail before binding a port")
	}
}

func TestRunRejectsBadPresetBeforeListening(t *testing.T) {
	// A misused selection flag must stop the process at startup. Failing at
	// the first scrape instead would look exactly like a metric the host does
	// not have, which is the one thing the preset feature must never resemble.
	err := run([]string{"-metrics.preset=nonsense", "-web.listen-address=127.0.0.1:0"})
	if err == nil {
		t.Fatal("run accepted an unknown preset")
	}
	if !strings.Contains(err.Error(), "nonsense") {
		t.Errorf("error does not name the offending value: %v", err)
	}
}

func TestRunRejectsBadIncludeRegexp(t *testing.T) {
	err := run([]string{
		"-metrics.preset=custom",
		"-metrics.include=prickle_(unclosed",
		"-web.listen-address=127.0.0.1:0",
	})
	if err == nil {
		t.Fatal("run accepted an uncompilable regexp; a typo must not silently withhold every metric")
	}
}

// TestDiagnoseReportsAFixtureTree runs the subcommand end to end against a
// captured tree.
//
// diagnose is the command an operator reaches for when the exporter reports
// nothing, so it is the one place where being wrong is most expensive — it
// once claimed a driverless H100 had no NVIDIA GPU at all. It had 3.5% coverage
// on a 514-line file. This exercises the container, cgroup and filesystem
// description paths against a tree whose contents are known.
func TestDiagnoseReportsAFixtureTree(t *testing.T) {
	const fixture = "../../internal/collector/container/testdata/podman-alma9-20260801"
	if _, err := os.Stat(fixture); err != nil {
		t.Skipf("fixture missing: %v", err)
	}

	var buf bytes.Buffer
	if err := diagnose([]string{"-path.rootfs=" + fixture}, &buf); err != nil {
		t.Fatalf("diagnose returned an error on a valid tree: %v", err)
	}
	out := buf.String()

	// Three containers, all attributed to podman. This capture is the one that
	// would report six if the conmon monitor scopes were ever counted, so the
	// count is asserted alongside the per-runtime breakdown rather than on its
	// own — "3" appears elsewhere in the output, "podman 3" does not.
	for _, want := range []string{"containers found: 3", "podman 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnose output does not contain %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "containers found: 6") {
		t.Error("diagnose counted the conmon monitor scopes as containers")
	}
}

func TestDiagnoseRejectsUnknownFlag(t *testing.T) {
	var buf bytes.Buffer
	if err := diagnose([]string{"-no-such-flag"}, &buf); err == nil {
		t.Error("diagnose accepted an undefined flag")
	}
}

func TestNodeNameFlagWins(t *testing.T) {
	cfg := config{node: "explicit-node"}
	got, err := cfg.nodeName()
	if err != nil {
		t.Fatal(err)
	}
	if got != "explicit-node" {
		t.Errorf("nodeName() = %q, want the flag value", got)
	}

	// Unset, it falls back to the hostname rather than to an empty label: an
	// empty `node` would collapse every host in a fleet onto one series.
	cfg = config{}
	got, err = cfg.nodeName()
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Error("nodeName() returned empty with no flag set")
	}
}

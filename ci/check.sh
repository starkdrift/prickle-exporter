#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The SPEC.md §Session checklist gate, in one command. Run it before every
# commit; CI runs the same script, so a green run here is a green run there.
#
#   ./ci/check.sh
#
# Unlike scripts/capture-fixtures.sh, this one stops at the first failure:
# there is no "partial result is better than none" argument for a pre-commit
# check.
set -euo pipefail

cd "$(dirname "$0")/.."

fail() { printf '\n\033[1;31mFAIL\033[0m %s\n' "$*" >&2; exit 1; }
step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

GO=${GO:-go}
command -v "$GO" >/dev/null || fail "go not found; set GO=/path/to/go"

step "gofmt"
# gofmt -l, not go fmt: a check must report, not rewrite the tree under you.
GOFMT=${GOFMT:-$(dirname "$(command -v "$GO")")/gofmt}
unformatted=$("$GOFMT" -l .)
[ -z "$unformatted" ] || fail "needs gofmt:"$'\n'"$unformatted"

step "go vet"
"$GO" vet ./...

step "go test -race (SPEC.md §Architecture)"
# -race, not a plain run. The sampler renders into a buffer and swaps it under a
# mutex while net/http serves the previous one from another goroutine — that
# non-blocking swap is the architecture's load-bearing claim, and the race
# detector is the only thing in this checklist that can falsify it. Running the
# suite once, under the detector, costs a second and covers what the plain run
# covered.
#
# The detector needs cgo and a C toolchain, which the release build deliberately
# does not use (CGO_ENABLED=0, SPEC.md §Distribution). That is not a conflict:
# this gate tests the source, the release builds the artifact. CGO_ENABLED is
# forced on here so an exported CGO_ENABLED=0 — which the README's own build
# line encourages — does not turn the gate into an error about cgo.
if ! command -v "${CC:-gcc}" >/dev/null && ! command -v clang >/dev/null; then
  fail "no C compiler for the race detector; it is a required gate, not an optional one.
      Install gcc or clang (Fedora: sudo dnf install gcc)."
fi
CGO_ENABLED=1 "$GO" test -race ./...

step "nvml build (SPEC.md §Distribution)"
# prickle-nvml is a shipped artifact built from source no other step compiles:
# //go:build nvml is invisible to the default build, vet and test above, so an
# edit to the gpu package's shared code can break it silently. This compiles
# and vets it. It cannot be *run* here — that needs an NVIDIA driver — and
# SPEC.md §Testing rules is explicit that the NVML path is verified on
# hardware; this is the weaker check that it still builds.
CGO_ENABLED=1 "$GO" vet -tags nvml ./...
CGO_ENABLED=1 "$GO" build -tags nvml -o /dev/null ./cmd/prickle
CGO_ENABLED=1 "$GO" test -tags nvml ./internal/collector/gpu/

step "zero third-party dependencies (SPEC.md §Hard constraints #1)"
if [ -s go.sum ]; then
  fail "go.sum is non-empty; the standard library is the only permitted dependency"
fi
if grep -qE '^\s*require' go.mod; then
  fail "go.mod has a require block; the standard library is the only permitted dependency"
fi

step "SPDX headers"
missing=$(find . -name '*.go' -not -path './.git/*' \
  -exec grep -L 'SPDX-License-Identifier: Apache-2.0' {} +) || true
[ -z "$missing" ] || fail "missing SPDX header:"$'\n'"$missing"

step "promtool check metrics (SPEC.md §Metrics contract)"
command -v promtool >/dev/null || fail "promtool not found; it is a required gate, not an optional one"
for golden in internal/collector/*/testdata/golden/*.prom; do
  printf '  %s\n' "$golden"
  promtool check metrics < "$golden" || fail "promtool rejected $golden"
done

step "documentation links resolve"
# Added after splitting the README into docs/: eleven links broke silently
# because they were written repo-relative and the files moved a directory down,
# and every other gate stayed green. A dead link in the front door is the first
# thing a new reader hits.
if command -v python3 >/dev/null 2>&1; then
  python3 - <<'PYCHECK' || fail "a documentation link does not resolve"
import pathlib, re, sys
bad = []
for f in pathlib.Path(".").rglob("*.md"):
    if any(part in (".git", "node_modules") for part in f.parts):
        continue
    for text, target in re.findall(r"\[([^\]]+)\]\(([^)#\s]+)(?:#[^)]*)?\)", f.read_text()):
        if target.startswith(("http", "mailto", "#")):
            continue
        if not (f.parent / target).exists():
            bad.append(f"  {f}: [{text}]({target})")
if bad:
    print("\n".join(bad), file=sys.stderr)
    sys.exit(1)
PYCHECK
  printf '  ok  every relative link in every .md resolves\n'
else
  printf '  \033[1;33mSKIP\033[0m python3 not found; links unchecked\n'
fi

step "every fixture is accounted for in its README (SPEC.md §Testing rules)"
# Added after two releases of stale documentation: scripts/README.md told
# contributors that cgroupfs Docker and the kubepods/<qos>/pod<uid>/<hex> layout
# were "unimplemented today, so those hosts report no containers at all", long
# after both shipped and were captured. Nothing failed, because no gate reads
# prose. This one cannot check a claim, but it can check the inventory the claim
# is about: a capture added without a line in the README is exactly how the gap
# table fell behind the tree.
for dir in internal/collector/*/testdata/*/; do
  name=$(basename "$dir")
  [ "$name" = golden ] && continue
  readme="$(dirname "$dir")/README.md"
  [ -f "$readme" ] || fail "$dir has no README.md beside it to record it in"
  grep -q -- "$name" "$readme" \
    || fail "fixture '$name' is not mentioned in $readme.
      Every capture is recorded there with what it covers; a fixture the README
      does not name is one the coverage-gap table has silently fallen behind."
done
printf '  ok  every fixture directory is named in its README\n'

step "grafana dashboards are in sync (SPEC.md §Distribution)"
# The JSON is generated and checked in: Grafana loads JSON and a quickstart
# should not need a build step, but four dashboards sharing eleven template
# variables cannot be hand-maintained either. This fails if the tree and the
# generator disagree.
if command -v python3 >/dev/null 2>&1; then
  python3 scripts/make-dashboards.py --check \
    || fail "packaging/grafana/dashboards is stale; re-run scripts/make-dashboards.py"
  printf '  ok  four dashboards match scripts/make-dashboards.py\n'
else
  printf '  \033[1;33mSKIP\033[0m python3 not found; dashboard sync unchecked\n'
fi

step "no PIDs in any fixture (SPEC.md §Metrics contract)"
# cgroup.procs is the one file in a cgroup that contains PIDs. The collector
# never reads it and no fixture carries it, but until now the capture script
# collected it and a hand-editing step kept it out of commits — a SPEC
# violation one forgotten step away from the tree. The script no longer
# captures it; this makes the guarantee independent of the script.
if procs=$(find . -path ./.git -prune -o -name 'cgroup.procs' -print 2>/dev/null) && [ -n "$procs" ]; then
  printf '%s\n' "$procs" | sed 's/^/    /'
  fail "a cgroup.procs file is in the tree; SPEC.md §Metrics contract forbids a PID anywhere"
fi
printf '  ok  no cgroup.procs anywhere in the tree\n'

step "the released version is stated consistently (SPEC.md §Versioning)"
# The README said "This is 0.6.x" for hours after the 0.7.0 tag went out, and
# nothing caught it: every other gate here reads structure, and a version is
# prose. It is also the one piece of prose a reader acts on — an operator
# copying the docker run line gets whatever tag it names.
#
# CHANGELOG.md is the source of truth rather than `git describe`, deliberately.
# CI checks out shallow with no tags, so a tag-derived check would be skipped
# exactly where it needs to run. The changelog is in-tree and is written before
# the tag is cut. [Unreleased] is skipped: it is not a version.
ver=$(grep -m1 -oE '^## \[[0-9]+\.[0-9]+\.[0-9]+\]' CHANGELOG.md | tr -d '#[] ')
[ -n "$ver" ] || fail "no released version heading found in CHANGELOG.md"
series="${ver%.*}.x"

check_version() {
  # $1 file, $2 human description, $3 the string that must be present
  grep -qF -- "$3" "$1" \
    || fail "$2 disagrees with CHANGELOG.md, which says $ver.
      Expected to find: $3
      In:               $1"
  printf '  ok  %s\n' "$2"
}

check_version README.md \
  "the README's container image tag" \
  "ghcr.io/starkdrift/prickle-exporter:$ver"
check_version README.md \
  "the README's Status version" \
  "This is \`$series\`"
check_version packaging/helm/prickle-exporter/Chart.yaml \
  "the chart's appVersion" \
  "appVersion: \"$ver\""

# The chart's own `version:` is deliberately NOT checked. It versions the chart,
# not the binary, and moves on its own schedule — asserting they match would
# force a chart release for every exporter release and vice versa.

step "naming discipline (SPEC.md §Identity)"
# Two checks, because the obvious one cannot be made to work on its own.
#
# The deny-list below can only catch a discarded name somebody still remembers,
# and the names discarded during this project's naming pass were never written
# down — ci/denied-names.txt has been empty since it was created, so the gate
# reported VACUOUS on every run and protected nothing. A step that cannot fail
# is worse than an absent one: it prints reassurance next to seven checks that
# mean something.
#
# So this runs first, and needs no privileged knowledge. SPEC.md §Identity says
# the names in the identity table are the only ones used anywhere in the tree.
# A discarded candidate for *this* project would be an exporter name, so assert
# that the only exporter-shaped identifiers present are the canonical one and
# the third-party exporters the docs legitimately name. Anything else is either
# a discarded name resurfacing or a new one nobody agreed to.
allowed_exporters='prickle-exporter|dcgm-exporter|node_exporter'
foreign=$(git grep -IohE '\b[a-z][a-z0-9]{2,}[-_]exporter\b' -- . \
            ':(exclude)ci/check.sh' ':(exclude)ci/denied-names.txt' \
          | sort -u | grep -vE "^($allowed_exporters)\$" || true)
if [ -n "$foreign" ]; then
  printf '  unexpected exporter names in the tree:\n%s\n' "$foreign" | sed 's/^/    /'
  fail "an exporter name outside SPEC.md §Identity appears; if it is legitimate, add it to allowed_exporters"
fi
printf '  ok  only %s and known third-party exporters appear\n' "prickle-exporter"

# The deny-list proper, for names somebody does supply. The grep excludes
# itself, the deny-list, and SPEC.md — the three places a discarded name may
# legitimately appear.
mapfile -t denied < <(grep -vE '^\s*(#|$)' ci/denied-names.txt || true)
if [ ${#denied[@]} -eq 0 ]; then
  printf '  note: ci/denied-names.txt is empty; the check above is what is protecting the tree.\n'
else
  for name in "${denied[@]}"; do
    if git grep -In -i -- "$name" -- . \
        ':(exclude)ci/denied-names.txt' \
        ':(exclude)ci/check.sh' \
        ':(exclude)SPEC.md'; then
      fail "denied name '$name' appears in the tree"
    fi
    printf '  ok  %s\n' "$name"
  done
fi

step "metric prefix is never abbreviated (SPEC.md §Identity)"
if git grep -InE '"prick_|"prkl_|prickle_exporter_' -- '*.go' '*.prom'; then
  fail "abbreviated or wrong metric prefix; it is always the full word prickle_"
fi

printf '\n\033[1;32mAll checks passed.\033[0m\n'

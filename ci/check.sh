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

step "denied names (SPEC.md §Identity)"
# The grep excludes itself, the deny-list, and SPEC.md — the three places a
# discarded name may legitimately appear.
mapfile -t denied < <(grep -vE '^\s*(#|$)' ci/denied-names.txt || true)
if [ ${#denied[@]} -eq 0 ]; then
  printf '  \033[1;33mVACUOUS\033[0m ci/denied-names.txt has no entries; nothing was checked.\n'
  printf '  Add the names discarded during naming, or this gate protects nothing.\n'
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

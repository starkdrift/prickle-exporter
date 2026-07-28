#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# dev-run.sh — start prickle from source, with dev-friendly defaults.
#
#   ./scripts/dev-run.sh                 # live host: debug logs, 2s interval
#   ./scripts/dev-run.sh fixture         # against a captured fixture tree
#   ./scripts/dev-run.sh diagnose        # `prickle diagnose` on this host
#   ./scripts/dev-run.sh scrape          # start, scrape once, print, stop
#
# Everything after the subcommand is passed straight to the binary, after this
# script's own flags, so it overrides them — Go's flag package keeps the last
# occurrence of a repeated flag:
#
#   ./scripts/dev-run.sh run -collector.cpu.per-core -sample.interval=1s
#   ./scripts/dev-run.sh scrape -path.rootfs=internal/collector/host/testdata/...
#
# Environment: GO, ADDR, TELEMETRY_PATH, INTERVAL, LOG_LEVEL, NODE.
#
# Read-only, like the exporter itself (SPEC.md §Hard constraints #2) — no root
# needed for Phase 1, every /proc file the host collector reads is world
# readable. Nothing here is a release build; that is Phase 5 (SPEC.md
# §Distribution).
set -euo pipefail

cd "$(dirname "$0")/.."

say()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
note() { printf '    %s\n' "$*"; }
fail() { printf '\n\033[1;31mFAIL\033[0m %s\n' "$*" >&2; exit 1; }

GO=${GO:-go}
command -v "$GO" >/dev/null || fail "go not found; set GO=/path/to/go"

# SPEC.md §Identity fixes the port at 10047. ADDR exists for the case where
# something else on this workstation already holds it, not as a config knob.
ADDR=${ADDR:-:10047}
TELEMETRY_PATH=${TELEMETRY_PATH:-/metrics}
INTERVAL=${INTERVAL:-2s}          # 10s in production; a dev loop wants faster
LOG_LEVEL=${LOG_LEVEL:-debug}
NODE=${NODE:-}

# Flags common to `run`, `fixture` and `scrape`.
base_flags() {
  printf '%s\n' \
    "-web.listen-address=$ADDR" \
    "-web.telemetry-path=$TELEMETRY_PATH" \
    "-sample.interval=$INTERVAL" \
    "-log.level=$LOG_LEVEL"
  [ -n "$NODE" ] && printf '%s\n' "-node=$NODE"
  return 0
}

# find_fixture prints the fixture tree to run against.
#
# It looks for captured trees rather than hardcoding one, so a new capture
# landing under testdata/ does not leave this script pointing at the old one.
# A tree is identified by having a proc/ directory (SPEC.md §Testing rules: the
# layout mirrors real paths so fsroot points straight at it).
find_fixture() {
  local trees=() d
  shopt -s nullglob
  for d in internal/collector/*/testdata/*/; do
    [ -d "${d}proc" ] && trees+=("${d%/}")
  done
  shopt -u nullglob

  case ${#trees[@]} in
    0) fail "no fixture tree under internal/collector/*/testdata/ has a proc/ directory.
      Capture one with scripts/capture-fixtures.sh (see scripts/README.md)." ;;
    1) printf '%s\n' "${trees[0]}" ;;
    *) printf 'more than one fixture tree; pass the one you want:\n' >&2
       printf '  %s\n' "${trees[@]}" >&2
       printf 'Each is the subset of a capture that its own collector reads, so\n' >&2
       printf 'no one tree exercises every phase at once — pick the phase you are\n' >&2
       printf 'working on.\n' >&2
       exit 1 ;;
  esac
}

# scrape_url derives the URL to scrape, honouring a -web.listen-address or
# -web.telemetry-path passed through as an extra argument. Without this, a
# passthrough flag would move the server and leave the scrape knocking on the
# old address.
scrape_url() {
  local addr=$ADDR path=$TELEMETRY_PATH prev='' arg
  for arg in "$@"; do
    case $prev in
      -web.listen-address|--web.listen-address) addr=$arg ;;
      -web.telemetry-path|--web.telemetry-path) path=$arg ;;
    esac
    case $arg in
      -web.listen-address=*|--web.listen-address=*) addr=${arg#*=} ;;
      -web.telemetry-path=*|--web.telemetry-path=*) path=${arg#*=} ;;
    esac
    prev=$arg
  done
  # ":10047" and "0.0.0.0:10047" are both reached over the loopback.
  local host=${addr%:*} port=${addr##*:}
  case $host in ''|0.0.0.0|'[::]') host=127.0.0.1 ;; esac
  printf 'http://%s:%s%s\n' "$host" "$port" "$path"
}

cmd=${1:-run}
[ $# -gt 0 ] && shift

case $cmd in
run)
  say "go run ./cmd/prickle  (live host)"
  note "metrics: $(scrape_url "$@")"
  note "Ctrl-C to stop."
  mapfile -t flags < <(base_flags)
  exec "$GO" run ./cmd/prickle "${flags[@]}" "$@"
  ;;

fixture)
  # An explicit tree as the first argument wins; anything starting with '-' is a
  # flag, so `fixture -log.level=info` still auto-detects.
  tree=''
  if [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; then tree=$1; shift; fi
  [ -n "$tree" ] || tree=$(find_fixture)
  [ -d "$tree/proc" ] || fail "$tree has no proc/ directory; not a fixture tree"

  say "go run ./cmd/prickle  (fixture tree)"
  note "rootfs:  $tree"
  note "metrics: $(scrape_url "$@")"
  note "Statfs is a syscall, not a file (SPEC.md §Collectors), so the"
  note "filesystem series come from THIS host, and mount points in the"
  note "fixture that do not exist here report prickle_filesystem_error 1."
  mapfile -t flags < <(NODE=${NODE:-prickle-fixture} base_flags)
  exec "$GO" run ./cmd/prickle "${flags[@]}" -path.rootfs="$tree" "$@"
  ;;

diagnose)
  # No listen address or interval here: diagnose starts no server.
  say "go run ./cmd/prickle diagnose"
  exec "$GO" run ./cmd/prickle diagnose -log.level="$LOG_LEVEL" \
    ${NODE:+-node="$NODE"} "$@"
  ;;

scrape)
  command -v curl >/dev/null || fail "curl not found; scrape needs it"
  url=$(scrape_url "$@")

  # Built, not `go run`: `go run` execs the binary as a child, so killing it
  # would leave the exporter holding the port. A real PID is killable.
  say "go build -o bin/prickle ./cmd/prickle"
  "$GO" build -o bin/prickle ./cmd/prickle

  say "starting for one scrape of $url"
  mapfile -t flags < <(base_flags)
  ./bin/prickle "${flags[@]}" "$@" &
  pid=$!
  trap 'kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null || true' EXIT

  # The sampler answers 503 until its first pass completes (internal/sampler),
  # so wait for a 200 rather than for the port to open.
  for _ in $(seq 50); do
    kill -0 "$pid" 2>/dev/null || fail "exporter exited before serving a scrape"
    [ "$(curl -s -o /dev/null -w '%{http_code}' "$url")" = 200 ] && break
    sleep 0.2
  done

  out=bin/dev-scrape.prom
  curl -sf "$url" -o "$out" || fail "no completed sample after 10s"
  say "$(grep -cv '^#' <"$out") series -> $out"
  cat "$out"

  # The same gate CI runs on the golden files (ci/check.sh), against live
  # output — where the host, not the fixture, decides what gets named.
  if command -v promtool >/dev/null; then
    say "promtool check metrics"
    promtool check metrics <"$out" || fail "promtool rejected the live output"
    note "ok"
  else
    say "promtool not installed — skipping the metrics lint"
    note "ci/check.sh requires it; install it before committing."
  fi
  ;;

help|-h|--help)
  # The header comment down to the environment list, minus the '# ' prefix.
  sed -n '3,18p' "$0" | sed 's/^# \{0,1\}//'
  ;;

*)
  fail "unknown command '$cmd'; try: run, fixture, diagnose, scrape, help"
  ;;
esac

#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Verify that prickle's listen port is still registered to this project on the
# Prometheus default-port wiki.
#
#   ./ci/check-port-registration.sh
#
# SPEC.md §Identity fixes the port and requires it registered upstream. The
# wiki is a world-writable page in someone else's repository: a row can be
# edited, reassigned or dropped by anyone, and nothing notifies us. This script
# is the watchdog for that.
#
# It is deliberately NOT called from ci/check.sh. That script is a hermetic
# pre-commit gate; this one needs the network, and a commit must not fail
# because GitHub is down or a developer is on a plane. Run it on a schedule
# instead.
#
# Exit codes are three-way on purpose — "we could not tell" must never be
# reported as "our registration is gone":
#
#   0  registered to this project
#   1  inconclusive: no curl, fetch failed, or the page is not what we expect
#   2  REGRESSION: the row is missing, or the port now belongs to someone else
set -euo pipefail

cd "$(dirname "$0")/.."

bold=$'\033[1m'; red=$'\033[1;31m'; green=$'\033[1;32m'; yellow=$'\033[1;33m'; off=$'\033[0m'

# 1 = inconclusive, 2 = real regression. Keep them distinct at every call site.
unknown() { printf '\n%sINCONCLUSIVE%s %s\n' "$yellow" "$off" "$*" >&2; exit 1; }
regress() { printf '\n%sFAIL%s %s\n' "$red" "$off" "$*" >&2; exit 2; }

WIKI_URL=${WIKI_URL:-https://raw.githubusercontent.com/wiki/prometheus/prometheus/Default-port-allocations.md}
PAGE_URL=${PAGE_URL:-https://github.com/prometheus/prometheus/wiki/Default-port-allocations}
REPO=${REPO:-starkdrift/prickle-exporter}

printf '%s==> Prometheus default-port registration%s\n' "$bold" "$off"

# The port comes from SPEC.md rather than a literal here. SPEC.md §Identity is
# the source of truth for it, and a script that checked a hardcoded 10047 after
# the spec moved would pass while verifying nothing. If the row cannot be
# parsed, that is inconclusive — not a silent fallback to a guess.
#
# Anchored on the `:PORT` form the table actually uses, not on "first number in
# the row" — that would quietly pick up any digit someone added to the prose
# and then report a false regression against a port we never listened on.
port=$(grep -E '^\| Listen address \|' SPEC.md | grep -oE ':[0-9]{4,5}\b' | head -1 | tr -d ':' || true)
[ -n "$port" ] || unknown "could not read the listen port from SPEC.md §Identity; has the table changed?"

printf '  %-9s %s (from SPEC.md §Identity)\n' "port" "$port"
printf '  %-9s %s\n' "expects" "$REPO"
printf '  %-9s %s\n' "source" "$PAGE_URL"

command -v curl >/dev/null || unknown "curl not found"

# --fail so an HTTP error is an error rather than a saved error page.
doc=$(curl -sSL --fail --max-time 30 "$WIKI_URL" 2>&1) \
  || unknown "could not fetch the wiki page: $doc"

# Sanity-gate the payload before drawing conclusions from it. If GitHub moves
# the wiki, rate-limits us, or serves a login interstitial, we would otherwise
# find no matching row and loudly report a regression that did not happen.
rows=$(grep -cE '^\|[[:space:]]*[0-9]{2,5}[[:space:]]*\|' <<<"$doc" || true)
if [ "$rows" -lt 100 ]; then
  unknown "fetched page has only $rows port rows; it is probably not the allocation table"
fi
printf '  %-9s %s port rows\n' "fetched" "$rows"

# Every row claiming this port. More than one means someone added a duplicate
# claim, which is worth seeing in full rather than collapsing to a yes/no.
mapfile -t matches < <(grep -E "^\|[[:space:]]*${port}[[:space:]]*\|" <<<"$doc" || true)

if [ ${#matches[@]} -eq 0 ]; then
  regress "port $port has no row on the wiki — the registration was removed."
fi

ours=0
for row in "${matches[@]}"; do
  printf '  %-9s %s\n' "row" "$row"
  grep -qF "$REPO" <<<"$row" && ours=1
done

if [ "$ours" -eq 0 ]; then
  regress "port $port is registered, but not to $REPO — it was reassigned."
fi

if [ ${#matches[@]} -gt 1 ]; then
  printf '\n%sWARNING%s %s rows claim port %s; ours is one of them. Worth resolving upstream.\n' \
    "$yellow" "$off" "${#matches[@]}" "$port"
fi

printf '\n%sOK%s port %s is registered to %s.\n' "$green" "$off" "$port" "$REPO"

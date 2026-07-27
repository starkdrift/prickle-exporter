#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Install the pinned promtool into a directory (default ./bin).
#
#   ./ci/install-promtool.sh [dest]
#   PATH="$PWD/bin:$PATH" ./ci/check.sh
#
# ci/check.sh treats promtool as a required gate and refuses to run without it
# (SPEC.md §Metrics contract). This is how CI gets it and the shortest path for
# a developer to get the identical version, so "passes locally, fails in CI"
# cannot be a promtool version difference.
#
# Deliberately a checksum-verified tarball rather than
# `go install github.com/prometheus/prometheus/cmd/promtool@latest`:
#
#   - `go install` drags Prometheus's entire module graph through the Go
#     toolchain to obtain a linter. go.sum would stay empty, so the letter of
#     SPEC.md §Hard constraints #1 survives, but keeping the module graph
#     untouched by tooling is the same instinct.
#   - The version would float. A gate that lints differently week to week is
#     not a gate.
#   - It is roughly ten times faster.
#
# To bump: change PROM_VERSION, then replace both checksums from
# https://github.com/prometheus/prometheus/releases/download/v<version>/sha256sums.txt
set -euo pipefail

PROM_VERSION=${PROM_VERSION:-3.13.1}

# From the release's own sha256sums.txt. Both architectures are pinned because
# CI runs the gate on amd64 and arm64 alike.
SHA256_amd64=962b812371aff838d152b6ff2d56fdb7a6396f5542f48ebf73421b9721f0d103
SHA256_arm64=fbd8e5e0f6ad2e7d053e717739186caee4fd0cab2cf9335bfc86c292fe2a2bfe

dest=${1:-bin}

fail() { printf '\033[1;31mFAIL\033[0m %s\n' "$*" >&2; exit 1; }

case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) fail "unsupported architecture $(uname -m); add its checksum to this script" ;;
esac

want=$(eval echo "\$SHA256_${arch}")
tarball="prometheus-${PROM_VERSION}.linux-${arch}.tar.gz"
url="https://github.com/prometheus/prometheus/releases/download/v${PROM_VERSION}/${tarball}"

mkdir -p "$dest"
dest=$(cd "$dest" && pwd)

# Already the right version? Do nothing. Keeps repeat local runs instant.
if [ -x "$dest/promtool" ] && "$dest/promtool" --version 2>&1 | grep -q "$PROM_VERSION"; then
  printf 'promtool %s already at %s/promtool\n' "$PROM_VERSION" "$dest"
  exit 0
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

printf 'fetching %s\n' "$url"
curl -sSL --fail --max-time 120 -o "$tmp/$tarball" "$url" \
  || fail "could not download $url"

got=$(sha256sum "$tmp/$tarball" | cut -d' ' -f1)
# The whole point of the pin. A mismatch means the release was altered or the
# download was corrupted; either way, do not run the binary.
[ "$got" = "$want" ] || fail "checksum mismatch for $tarball
  expected $want
  got      $got"

tar -xzf "$tmp/$tarball" -C "$tmp" --strip-components=1 \
  "prometheus-${PROM_VERSION}.linux-${arch}/promtool"
install -m 0755 "$tmp/promtool" "$dest/promtool"

printf 'installed %s to %s/promtool\n' "$("$dest/promtool" --version 2>&1 | head -1)" "$dest"
printf 'add it to PATH:  export PATH="%s:$PATH"\n' "$dest"

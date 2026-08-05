#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# capture-dashboard.sh — prickle-exporter (Starkdrift)
#
# Render a provisioned Grafana dashboard to a PNG suitable for the README.
# Read-only: it drives a headless browser against a Grafana you already have
# running, and touches nothing else.
#
# The fiddly parts, all handled here so the next capture does not rediscover
# them:
#   * `kiosk` drops Grafana's nav and top bar but KEEPS the `<label> contains`
#     textboxes, which the dashboards' filtering story depends on showing.
#   * `theme=dark` on the URL beats the per-user preference, which under the
#     demo's anonymous admin is whatever the last person clicked.
#   * A headless screenshot is viewport-sized, so the page is rendered tall and
#     the trailing background is then cropped off rather than guessed at.
#   * Palette-quantising takes a full dashboard from ~150 KB to ~60 KB with no
#     visible change. The README loads this on every view of the front page.
#
# Usage:
#   capture-dashboard.sh <grafana-base-url> <dashboard-uid> <output.png> [options]
#
#   --from VALUE      Grafana time range start (default: now-5m)
#   --to VALUE        Grafana time range end   (default: now)
#   --width PX        Viewport width  (default: 1600)
#   --height PX       Viewport height before cropping (default: 1400)
#   --theme NAME      dark | light (default: dark)
#   --no-quantize     Keep 24-bit colour; larger file
#
# Example, against a port-forwarded demo:
#   kubectl -n prickle-demo port-forward svc/grafana 3000:3000 &
#   scripts/capture-dashboard.sh http://localhost:3000 prickle-gpu-tenancy \
#     assets/dashboards/gpu-tenancy-nvidia.png
#
# Requires: a Chrome/Chromium binary, python3 with Pillow.

set -euo pipefail

die() { printf 'capture-dashboard.sh: %s\n' "$*" >&2; exit 1; }

[ $# -ge 3 ] || die "usage: capture-dashboard.sh <grafana-url> <uid> <out.png> [options]"

BASE=${1%/}; UID_=$2; OUT=$3; shift 3
FROM=now-5m TO=now WIDTH=1600 HEIGHT=1400 THEME=dark QUANTIZE=1

while [ $# -gt 0 ]; do
  case $1 in
    --from)   FROM=$2; shift 2 ;;
    --to)     TO=$2; shift 2 ;;
    --width)  WIDTH=$2; shift 2 ;;
    --height) HEIGHT=$2; shift 2 ;;
    --theme)  THEME=$2; shift 2 ;;
    --no-quantize) QUANTIZE=0; shift ;;
    *) die "unknown option: $1" ;;
  esac
done

CHROME=""
for c in google-chrome google-chrome-stable chromium chromium-browser; do
  command -v "$c" >/dev/null 2>&1 && { CHROME=$c; break; }
done
[ -n "$CHROME" ] || die "no Chrome/Chromium binary found"
python3 -c 'import PIL' 2>/dev/null || die "python3 with Pillow is required"

URL="$BASE/d/$UID_/?theme=$THEME&from=$FROM&to=$TO&kiosk"
RAW=$(mktemp -t dashboard-raw-XXXXXX.png)
trap 'rm -f "$RAW"' EXIT

printf 'rendering %s\n' "$URL"
# --virtual-time-budget lets the panels finish their queries before the shot.
"$CHROME" --headless=new --no-sandbox --disable-gpu --hide-scrollbars \
  --window-size="$WIDTH,$HEIGHT" --virtual-time-budget=45000 \
  --screenshot="$RAW" "$URL" >/dev/null 2>&1

[ -s "$RAW" ] || die "the browser produced no image"

mkdir -p "$(dirname "$OUT")"
QUANTIZE=$QUANTIZE python3 - "$RAW" "$OUT" <<'PY'
import os
import sys
from PIL import Image

src, dst = sys.argv[1], sys.argv[2]
im = Image.open(src).convert("RGB")
w, h = im.size
px = im.load()

# Crop the trailing page background. Sampling every third column is plenty to
# find the last row with any panel content on it.
bg = px[5, h - 5]
last = h - 1
for y in range(h - 1, -1, -1):
    if any(sum(abs(a - b) for a, b in zip(px[x, y], bg)) > 12 for x in range(0, w, 3)):
        last = y
        break
im = im.crop((0, 0, w, min(h, last + 13)))

if os.environ.get("QUANTIZE") == "1":
    im = im.quantize(colors=256, method=Image.MEDIANCUT, dither=Image.NONE)
im.save(dst, optimize=True)
print(f"wrote {dst} ({im.size[0]}x{im.size[1]}, {os.path.getsize(dst) // 1024} KiB)")

if last >= h - 14:
    print("NOTE: content reached the bottom edge - re-run with a larger "
          "--height or the dashboard is cut off", file=sys.stderr)
PY

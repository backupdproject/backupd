#!/usr/bin/env bash
# Re-export every raster brand asset from its committed SVG source (FR-44,
# #893).
#
# WHY THIS IS A SCRIPT AND NOT A README PARAGRAPH. FR-44's requirement is
# not "the favicons were regenerated once"; it is that every declared size
# derives from one source, so that the next person who touches the mark
# cannot re-export five of the eight and leave three showing the old
# drawing. Before this file the rasters in the tree were hand-made one-offs
# at three different framings and nothing recorded which source or which
# scale produced them, which is why reproducing them here had to start with
# `magick -trim` on the committed PNGs.
#
# The framings below are those measurements, written down:
#
#   natural      the 48-unit viewBox rendered straight, so the mark's ink
#                occupies 39/48 = 81.25% of the square. The two favicon
#                PNGs and both .ico members. A favicon is already tiny and
#                padding it wastes the only pixels it has.
#   safe-area    the mark at 49% of the canvas, centred. Apple's touch
#                icon is drawn on a full-bleed tile that the OS rounds and
#                may add effects to, so the art keeps well clear of the
#                edge; docs/assets/logo-mark-256.png was made at the same
#                ratio and keeps it.
#   bleed        the mark's ink filling the canvas height, centred, on a
#                2:1 canvas. docs/assets/logo-mark-640x320.png only.
#
# 49% of the canvas is an ink box of 0.49*C, and the ink is 39/48 of the
# render size, so the render size is 0.49*48/39 = 0.6031 of the canvas.
#
# Needs rsvg-convert (librsvg) and ImageMagick's `magick`. Both are in the
# image-tooling set this repository already assumes for docs/site/tools;
# the script names whichever is missing and exits 1 rather than producing a
# half-regenerated set.
#
# Idempotent by construction: same sources, same sizes, same output. Run it
# and `git status` should be clean unless a source actually changed.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

missing=""
for tool in rsvg-convert magick; do
  command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
if [ -n "$missing" ]; then
  echo "export-rasters: missing:$missing" >&2
  echo "export-rasters: install librsvg (rsvg-convert) and ImageMagick (magick), then re-run." >&2
  exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# natural <source.svg> <px> <out.png>
natural() {
  rsvg-convert -w "$2" -h "$2" "$1" -o "$3"
}

# safe_area <source.svg> <canvas px> <out.png> [background]
# The mark at 49% of the canvas, centred. A background argument makes the
# tile opaque (Apple's icon is composited on white if it is not).
safe_area() {
  src="$1"; canvas="$2"; out="$3"; bg="${4:-none}"
  render="$(awk -v c="$canvas" 'BEGIN{printf "%d", (c*0.49*48/39)+0.5}')"
  rsvg-convert -w "$render" -h "$render" "$src" -o "$tmp/sa.png"
  if [ "$bg" = none ]; then
    magick "$tmp/sa.png" -background none -gravity center -extent "${canvas}x${canvas}" "$out"
  else
    magick "$tmp/sa.png" -background "$bg" -gravity center -extent "${canvas}x${canvas}" \
      -alpha remove -alpha off "$out"
  fi
}

# bleed <source.svg> <w> <h> <out.png>
# The mark's ink filling the canvas height, centred on a wider canvas.
bleed() {
  src="$1"; cw="$2"; ch="$3"; out="$4"
  render="$(awk -v h="$ch" 'BEGIN{printf "%d", (h*48/39)+0.5}')"
  rsvg-convert -w "$render" -h "$render" "$src" -o "$tmp/bl.png"
  magick "$tmp/bl.png" -background none -gravity center -extent "${cw}x${ch}" "$out"
}

# The two favicon sets are byte-identical today and stay that way: the site
# and the app ship the same mark at the same sizes, and two copies that
# drift is a bug nobody sees until one tab shows the old drawing.
for dir in docs/site/assets ui/shared/public; do
  src="$dir/icon.svg"
  [ "$dir" = ui/shared/public ] && src="$dir/favicon.svg"

  natural "$src" 16 "$dir/favicon-16.png"
  natural "$src" 32 "$dir/favicon-32.png"

  # The .ico carries three members, 16, 32 and 48, which is what the
  # committed file already held; only the first two are also shipped as
  # standalone PNGs, so the 48 is rendered here and thrown away.
  natural "$src" 48 "$tmp/favicon-48.png"
  magick "$dir/favicon-16.png" "$dir/favicon-32.png" "$tmp/favicon-48.png" "$dir/favicon.ico"

  # The touch icon is the white mark on the brand tile: iOS ignores alpha
  # and composites on white, so an alpha PNG there is a white mark on
  # white. docs/site/assets/logo-mark-light.svg is the white-on-anything
  # source and is used for both copies.
  safe_area docs/site/assets/logo-mark-light.svg 180 "$dir/apple-touch-icon.png" "#3a628f"
done

safe_area assets/logo-mark.svg 256 docs/assets/logo-mark-256.png
bleed     assets/logo-mark.svg 640 320 docs/assets/logo-mark-640x320.png

echo "export-rasters: ok (8 PNG, 2 ICO with three members each, from 4 SVG sources)"

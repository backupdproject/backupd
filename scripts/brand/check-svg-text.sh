#!/usr/bin/env bash
# FR-44's SVG string scan (#893): no brand source SVG carries a TEXT NODE.
#
# WHAT IT LOOKS FOR, and why those four elements. `<text>` and its `<tspan>`
# children are letterforms a font draws at render time; `<title>` and
# `<desc>` are strings a reader or a store listing may surface. All four are
# a place the product's NAME can sit inside a file that
# scripts/rename/check-brand-drift.sh treats as source and a raster export
# turns into pixels nobody greps. The previous lockup carried the name in a
# `<text>` element and five icons carried it in a `<title>`, so a rename
# that edited every one of them still left the name reachable only by
# reading the art.
#
# WHAT IT CANNOT DO, stated here because FR-44 is built around the gap: once
# the letters are `<path>` data this scan is blind, and it was always blind
# to `.png` and `.ico`. It catches the cheap half. The other half is a human
# looking at the pictures, which is recorded as an outstanding acceptance
# step in docs/design/brand-assets.md and as `PARTIAL` on row R2.14 of
# docs/conformance/epic-r-matrix.md. A green run here is not a claim that
# the art says the right thing; it is a claim that no text node is left for
# the next rename to miss.
#
# COMMENTS ARE STRIPPED FIRST. An XML comment is not a text node: it draws
# nothing, it reaches no screen reader and no store listing, and these
# files' comments have to be able to say which elements are banned without
# the check that bans them going red on the sentence explaining it. The
# stripper is stateful across lines because an SVG comment routinely spans
# ten of them.
#
# SCOPE: every tracked `.svg` in the tree, and every `.svg` that is present
# and not ignored. All eleven of them are brand art today; a twelfth added
# anywhere is scanned on the same terms, which is the point of scoping by
# extension rather than by a list that has to be maintained.
#
# ONE EXEMPTION, PINNED TO ITS PATH, AND IT IS A STRONGER RULE THAN THE BAN.
# distribution/packaging's CheckStoreIcon REQUIRES a store-listing icon to
# carry a `<title>`: several catalogue front ends read it as the image's alt
# text and none of them reads `aria-label`, so dropping it turns eleven
# provider submissions red (materials-icon). That rule and this one are both
# right, and the honest resolution is not to weaken either: the one file it
# applies to, `docs/submission/icon.svg`, is named in `titled` below, and for
# a named file a `<title>` is allowed only if its TEXT IS THE PRODUCT NAME.
# So the planted violation FR-44 names still turns this red in that file too
# -- more definitely than the ban would, because the ban would have been
# satisfied by deleting the element and this is satisfied only by the right
# string. `<text>`, `<tspan>` and `<desc>` stay banned there like everywhere
# else, and the exemption is by path, so the same `<title>` in any other
# file is still a finding.
#
# Pinned by path rather than by a flag in the file for the reason
# check-brand-drift.sh pins its `preexisting` entries by path: an exception
# that travels with the file is an exception anybody can grant.
#
# Exit code contract, the same as the brand-drift guard next door:
#
#   0  no SVG carries a text node
#   1  at least one does, and the run printed file, line and element
#
# Registered in scripts/ci-local.sh in the same gate step as
# scripts/rename/check-brand-drift.sh, per FR-44. scripts/brand/selftest.sh
# is the proof it can still go red.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

# Tracked plus present-and-not-ignored, so an asset somebody has written but
# not yet added is scanned rather than waved through at exactly the moment
# it is easiest to get wrong.
# `git ls-files -z` and `xargs -0`, not a `for` over a word-split string:
# `docs/design/` carries a dated record whose filename contains a space, so
# a path this would word-split on is not hypothetical in this tree. A
# NUL-separated list also keeps this working on bash 3.2, which is what
# macOS ships and which has no `mapfile`.
svg_count="$(git ls-files --cached --others --exclude-standard -- '*.svg' | sort -u | wc -l | tr -d ' ')"

if [ "$svg_count" -eq 0 ]; then
  echo "check-svg-text: FAILED: no .svg found at all, so this check inspected nothing" >&2
  exit 1
fi

# The product's name, and the only string an exempt file's `<title>` may
# hold. Spelled once, here, so that the next rename finds it: this line is
# exactly the kind of string FR-44 exists because nobody found.
product_name="retnd"

# The paths whose `<title>` is required by something else, one per line,
# with the requirement. Nothing else is exempt from anything.
titled="$(
  cat <<'EOF'
docs/submission/icon.svg
EOF
)"

findings="$(
  git ls-files -z --cached --others --exclude-standard -- '*.svg' |
    TITLED="$titled" NAME="$product_name" xargs -0 awk '
    function load(blob, set,   n, i, parts) {
      n = split(blob, parts, "\n")
      for (i = 1; i <= n; i++) if (parts[i] != "") set[parts[i]] = 1
    }
    BEGIN { load(ENVIRON["TITLED"], titled); name = ENVIRON["NAME"] }
    FNR == 1 { in_comment = 0 }
    {
      line = $0
      out = ""
      # Strip XML comments, carrying the open state across lines.
      while (length(line) > 0) {
        if (in_comment) {
          i = index(line, "-->")
          if (i == 0) { line = ""; break }
          line = substr(line, i + 3)
          in_comment = 0
          continue
        }
        i = index(line, "<!--")
        if (i == 0) { out = out line; line = ""; break }
        out = out substr(line, 1, i - 1)
        line = substr(line, i + 4)
        in_comment = 1
      }

      # textPath and textArea are named too: they are text nodes under
      # another tag, and `text` on its own does not match them because the
      # next character is a letter. The trailing class is what stops
      # `<titlebar>` being a finding and what lets `<title>`, `<title >`
      # and `<title/>` all be one.
      n = split(out, chunks, "<")
      for (k = 2; k <= n; k++) {
        tag = chunks[k]
        if (tag !~ /^(text|textPath|textArea|tspan|title|desc)([^A-Za-z0-9_-]|$)/) continue
        match(tag, /^[A-Za-z]+/)
        elem = substr(tag, RSTART, RLENGTH)

        # The pinned exemption: in a file on `titled`, a `<title>` is
        # allowed, and its text has to be the product name. Matched against
        # the whole comment-stripped line rather than against this chunk,
        # because splitting on "<" puts the closing tag in the NEXT chunk.
        # The element has to open and close on one line: a text this check
        # cannot read is a text this check is not checking. The closing
        # `</title>` chunk starts with `/`, so the loop below never reports
        # it as an element of its own.
        if (elem == "title" && (FILENAME in titled)) {
          if (match(out, /<title[^>]*>[^<]*<\/title[ \t]*>/)) {
            body = substr(out, RSTART, RLENGTH)
            sub(/^<title[^>]*>/, "", body)
            sub(/<\/title[ \t]*>$/, "", body)
            gsub(/^[ \t]+|[ \t]+$/, "", body)
            if (body == name) continue
            printf "  %s:%d: <title>%s</title> (this path may carry a <title>, and it has to read %s)\n", FILENAME, FNR, body, name
            continue
          }
          printf "  %s:%d: <title> that does not open and close on one line, so its text cannot be checked\n", FILENAME, FNR
          continue
        }

        printf "  %s:%d: <%s>\n", FILENAME, FNR, elem
      }
    }
  '
)"

if [ -n "$findings" ]; then
  echo "check-svg-text: FAILED: a brand SVG carries a text node (FR-44, #893):" >&2
  printf '%s\n' "$findings" >&2
  cat >&2 <<'EOF'
check-svg-text: <text>, <tspan>, <title> and <desc> are the four places the
  product's name can sit inside a picture. A rename edits source; it does
  not edit art, and a raster export of that art is not greppable by
  anything. So brand SVGs carry no text node at all:
    * a name for assistive technology goes on role="img" aria-label="...",
      which is also what ui/shared/src/components/Logo.tsx uses, and for the
      further reason that a <title> child is drawn as a hover tooltip;
    * a description for a human reading the file goes in an XML comment,
      which this check strips before it looks;
    * letterforms are <path> data. docs/assets/logo-light.svg is the worked
      example, and docs/design/brand-assets.md records the construction.
  The one exception is a store-listing icon, whose <title> distribution/
  packaging's CheckStoreIcon requires because catalogue front ends read it
  as alt text. Those paths are on `titled` in this script, they are pinned
  by path, and their <title> has to read the product name on one line --
  the wrong string there is this finding, not a pass.
EOF
  exit 1
fi

echo "check-svg-text: ok ($svg_count SVG, no <text>/<tspan>/<desc>, and the one pinned <title> reads $product_name)"

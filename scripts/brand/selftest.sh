#!/usr/bin/env bash
# Mutation self-test for FR-44's two brand checks (#893).
#
# Both checks are green on this tree, and a check that is green on the only
# tree anybody runs it against has proven nothing -- the pattern could be
# misanchored, the manifest parser could be returning nothing, the file list
# could be empty. FR-44 names the two planted violations that have to make
# them red:
#
#   a row in the manifest with no file, and a file with no row
#   a <title> carrying the old name planted in a source SVG
#
# so those are the first cases below, and the rest are the controls that make
# a red run distinguishable from a check that matches everything.
#
# THE CONTROL THAT MATTERS MOST is the comment one. check-svg-text.sh strips
# XML comments before it looks, because these files' comments have to be able
# to name the elements they are forbidden to contain -- and a stripper is
# also the obvious place to accidentally strip the whole file and report a
# clean tree. So one case plants the banned elements inside a comment and
# requires green, and its partner plants them on the next line outside the
# comment and requires red, which is the pair that says the stripper stops
# where it should.
#
# Throwaway git repositories per case, following scripts/rename/selftest.sh:
# both checks scope themselves with `git ls-files`, so they need tracked
# files, and nothing here copies product source, so there is no anchor to
# drift.
#
# Every case runs even after one has failed; the tally at the end is the
# result.
#
# Run directly (`bash scripts/brand/selftest.sh`) or let the gate run it:
# scripts/ci-local.sh invokes it beside the two checks themselves.
set -uo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
svg_check="$repo_root/scripts/brand/check-svg-text.sh"
manifest_check="$repo_root/scripts/brand/check-brand-assets.sh"

checks=0
failures=0

pass() {
  checks=$((checks + 1))
  echo "  ok   $1"
}

fail() {
  checks=$((checks + 1))
  failures=$((failures + 1))
  echo "  FAIL $1"
  if [ -n "${2:-}" ]; then
    printf '%s\n' "$2" | sed 's/^/         /'
  fi
}

new_repo() {
  tree="$(mktemp -d)"
  (
    cd "$tree"
    git init -q
    git config user.email selftest@example.invalid
    git config user.name selftest
  )
  echo "$tree"
}

# write <tree> <path> <<<content
write() {
  mkdir -p "$(dirname "$1/$2")"
  cat >"$1/$2"
}

commit() {
  (cd "$1" && git add -A && git commit -q -m "case" >/dev/null 2>&1)
}

# A source SVG with no text node: the shape everything in this tree has
# after #893. Used as the green baseline and as the file the red cases
# mutate.
clean_svg() {
  cat <<'EOF'
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48" role="img" aria-label="retnd">
  <circle cx="24" cy="24" r="17" fill="none" stroke="#3a628f" stroke-width="5"/>
</svg>
EOF
}

run() {
  out="$(cd "$1" && bash "$2" 2>&1)"
  status=$?
}

green() {
  run "$2" "$3"
  if [ "$status" -eq 0 ]; then
    pass "$1"
  else
    fail "$1 (expected exit 0, got $status)" "$out"
  fi
  rm -rf "$2"
}

# red <label> <tree> <check> <substring the report must name>...
red() {
  label="$1"; tree="$2"; check="$3"
  shift 3
  run "$tree" "$check"
  if [ "$status" -eq 0 ]; then
    fail "$label (the check accepted it)" "$out"
    rm -rf "$tree"
    return
  fi
  for needle in "$@"; do
    if ! printf '%s' "$out" | grep -Fq -- "$needle"; then
      fail "$label (went red, but never named '$needle')" "$out"
      rm -rf "$tree"
      return
    fi
  done
  pass "$label"
  rm -rf "$tree"
}

echo "==> check-svg-text.sh"

tree="$(new_repo)"
clean_svg | write "$tree" assets/logo-mark.svg
commit "$tree"
green "a source SVG whose name is on aria-label is accepted" "$tree" "$svg_check"

# FR-44's named planted violation, spelled the way the spec spells it.
tree="$(new_repo)"
{
  echo '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">'
  echo '  <title>backupd</title>'
  echo '  <circle cx="24" cy="24" r="17"/>'
  echo '</svg>'
} | write "$tree" docs/site/assets/icon.svg
commit "$tree"
red "a planted <title> carrying the old name goes red" "$tree" "$svg_check" \
  "docs/site/assets/icon.svg:2:" "<title>"

tree="$(new_repo)"
{
  echo '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 168 48">'
  echo '  <text x="60" y="31">a lockup set as live type</text>'
  echo '</svg>'
} | write "$tree" docs/assets/logo-light.svg
commit "$tree"
red "a wordmark left as a <text> element goes red" "$tree" "$svg_check" \
  "docs/assets/logo-light.svg:2:" "<text>"

tree="$(new_repo)"
{
  echo '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 256 256">'
  echo '  <desc>a sentence a store listing may surface</desc>'
  echo '</svg>'
} | write "$tree" docs/submission/icon.svg
commit "$tree"
red "a <desc> goes red, because a listing can surface it" "$tree" "$svg_check" \
  "docs/submission/icon.svg:2:" "<desc>"

# The pinned exemption, four ways. docs/submission/icon.svg is the one path
# allowed a <title>, because distribution/packaging's CheckStoreIcon
# requires a listing icon to carry one; the exemption is only worth having
# if it is narrower than the ban it replaces, so these say in what way.
tree="$(new_repo)"
{
  echo '<svg xmlns="http://www.w3.org/2000/svg" width="256" height="256" role="img" aria-label="retnd">'
  echo '  <title>retnd</title>'
  echo '</svg>'
} | write "$tree" docs/submission/icon.svg
commit "$tree"
green "the store icon's required <title> is accepted when it reads the product name" \
  "$tree" "$svg_check"

tree="$(new_repo)"
{
  echo '<svg xmlns="http://www.w3.org/2000/svg" width="256" height="256" role="img" aria-label="retnd">'
  echo '  <title>backupd</title>'
  echo '</svg>'
} | write "$tree" docs/submission/icon.svg
commit "$tree"
red "the same <title> carrying the old name goes red in the exempt file too" \
  "$tree" "$svg_check" "docs/submission/icon.svg:2:" "backupd" "has to read retnd"

tree="$(new_repo)"
{
  echo '<svg xmlns="http://www.w3.org/2000/svg" width="256" height="256" role="img" aria-label="retnd">'
  echo '  <title>'
  echo '    retnd'
  echo '  </title>'
  echo '</svg>'
} | write "$tree" docs/submission/icon.svg
commit "$tree"
red "a <title> the check cannot read on one line is refused, not waved through" \
  "$tree" "$svg_check" "cannot be checked"

tree="$(new_repo)"
{
  echo '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48" role="img" aria-label="retnd">'
  echo '  <title>retnd</title>'
  echo '</svg>'
} | write "$tree" docs/site/assets/icon.svg
commit "$tree"
red "the exemption is pinned to its path: the same correct <title> elsewhere is a finding" \
  "$tree" "$svg_check" "docs/site/assets/icon.svg:2:" "<title>"

tree="$(new_repo)"
{
  echo '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">'
  echo '  <text><tspan>split across two runs</tspan></text>'
  echo '</svg>'
} | write "$tree" assets/logo-mark.svg
commit "$tree"
red "a <tspan> is named in its own right" "$tree" "$svg_check" "<tspan>"

# The comment pair. These two files differ by one line's worth of `-->`, and
# the check has to answer them differently.
tree="$(new_repo)"
{
  echo '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48" role="img" aria-label="retnd">'
  echo '  <!-- No <text>, no <title> and no <desc> in this file: the name is'
  echo '       on aria-label instead, and the letters are <path> data.'
  echo '       <tspan> is out too. -->'
  echo '  <path d="M4 4H44"/>'
  echo '</svg>'
} | write "$tree" assets/logo-mark.svg
commit "$tree"
green "the banned element names inside an XML comment stay green" "$tree" "$svg_check"

tree="$(new_repo)"
{
  echo '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">'
  echo '  <!-- this comment mentions <title> and then closes -->'
  echo '  <title>the old name</title>'
  echo '</svg>'
} | write "$tree" assets/logo-mark.svg
commit "$tree"
red "a real <title> on the line after a comment mentioning one goes red" \
  "$tree" "$svg_check" "assets/logo-mark.svg:3:"

tree="$(new_repo)"
{
  echo '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48" role="img" aria-label="retnd">'
  echo '  <path d="M4 4H44" textLength="40" text-anchor="middle"/>'
  echo '</svg>'
} | write "$tree" assets/logo-mark.svg
commit "$tree"
green "textLength and text-anchor as ATTRIBUTES stay green" "$tree" "$svg_check"

# A file written but not yet `git add`ed is scanned, because that is exactly
# when a new asset is easiest to get wrong.
tree="$(new_repo)"
clean_svg | write "$tree" assets/logo-mark.svg
commit "$tree"
{
  echo '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">'
  echo '  <title>the old name</title>'
  echo '</svg>'
} | write "$tree" apps/newprovider/icon.svg
red "an untracked new SVG is scanned too" "$tree" "$svg_check" \
  "apps/newprovider/icon.svg:2:"

echo "==> check-brand-assets.sh"

# manifest_repo <tree> <<<rows : a tree with one real asset and whatever
# inventory rows the case wants.
manifest_repo() {
  tree="$(new_repo)"
  clean_svg | write "$tree" assets/logo-mark.svg
  echo "$tree"
}

manifest_with() {
  {
    echo '# Brand assets'
    echo
    echo '| Asset | Sizes | Source | Embedded at |'
    echo '| --- | --- | --- | --- |'
    cat
  } | write "$1" docs/design/brand-assets.md
}

tree="$(manifest_repo)"
manifest_with "$tree" <<'EOF'
| `assets/logo-mark.svg` | `viewBox 0 0 48 48` | drawn | nothing |
EOF
commit "$tree"
green "a manifest that matches the tree exactly is accepted" "$tree" "$manifest_check"

# FR-44's first named planted violation.
tree="$(manifest_repo)"
manifest_with "$tree" <<'EOF'
| `assets/logo-mark.svg` | `viewBox 0 0 48 48` | drawn | nothing |
| `docs/site/assets/apple-touch-icon.png` | 180x180 | drawn | nothing |
EOF
commit "$tree"
red "a row naming a file that does not exist goes red" "$tree" "$manifest_check" \
  "docs/site/assets/apple-touch-icon.png" "row for something that is not in the tree"

# FR-44's second, and the direction the whole manifest exists for.
tree="$(manifest_repo)"
clean_svg | write "$tree" apps/newprovider/icon.svg
manifest_with "$tree" <<'EOF'
| `assets/logo-mark.svg` | `viewBox 0 0 48 48` | drawn | nothing |
EOF
commit "$tree"
red "an asset on disk with no row goes red" "$tree" "$manifest_check" \
  "apps/newprovider/icon.svg" "no row in"

# The same direction for a raster, because the surface patterns for `.png`
# are per-directory and a `.png` outside them is deliberately not brand art.
tree="$(manifest_repo)"
printf 'not really a png\n' >"$tree/x.bin"; mv "$tree/x.bin" "$tree/nope"
mkdir -p "$tree/ui/shared/public"
printf 'stand-in\n' >"$tree/ui/shared/public/favicon-16.png"
manifest_with "$tree" <<'EOF'
| `assets/logo-mark.svg` | `viewBox 0 0 48 48` | drawn | nothing |
EOF
commit "$tree"
red "an unlisted raster in a brand directory goes red" "$tree" "$manifest_check" \
  "ui/shared/public/favicon-16.png"

# A directory row covers what is under it, and only what is under it.
tree="$(manifest_repo)"
mkdir -p "$tree/docs/site/screens"
printf 'stand-in\n' >"$tree/docs/site/screens/ui-01.png"
printf 'stand-in\n' >"$tree/docs/site/screens/ui-02.gif"
manifest_with "$tree" <<'EOF'
| `assets/logo-mark.svg` | `viewBox 0 0 48 48` | drawn | nothing |
| `docs/site/screens/` | 2 files | capture tooling | the site |
EOF
commit "$tree"
green "a directory row covers the files beneath it" "$tree" "$manifest_check"

tree="$(manifest_repo)"
manifest_with "$tree" <<'EOF'
| `assets/logo-mark.svg` | `viewBox 0 0 48 48` | drawn | nothing |
| `docs/site/screens/` | 2 files | capture tooling | the site |
EOF
commit "$tree"
red "a directory row with nothing under it goes red" "$tree" "$manifest_check" \
  "docs/site/screens/" "directory row"

# The failure that would make every other case above meaningless.
tree="$(manifest_repo)"
{
  echo '# Brand assets'
  echo
  echo 'Prose only. Nobody wrote the table.'
} | write "$tree" docs/design/brand-assets.md
commit "$tree"
red "a manifest with no parsable row is refused, not treated as empty" \
  "$tree" "$manifest_check" "inspected nothing"

tree="$(new_repo)"
clean_svg | write "$tree" assets/logo-mark.svg
commit "$tree"
red "a missing manifest is refused" "$tree" "$manifest_check" \
  "docs/design/brand-assets.md does not exist"

echo "==> brand-asset self-test: $checks checks, $failures failure(s)"
[ "$failures" -eq 0 ] || exit 1

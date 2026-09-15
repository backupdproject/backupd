# Brand assets

Every brand asset in this repository, the sizes each one is declared at, the
source it is exported from, and the places that embed or reference it.

This file is FR-44's first check (`docs/EPIC-R-rename-backupd-to-retnd.md`,
issue #893) and it is **held to the filesystem in both directions** by
`scripts/brand/check-brand-assets.sh`, which runs in `scripts/ci-local.sh`:

- a row naming a path that is not in the tree fails the run;
- a brand asset in the tree with no row fails the run.

The second direction is the one that matters. An inventory checked only the
first way still cannot see the asset nobody wrote down, and the asset nobody
wrote down is precisely the one that is still showing the previous name after
the rename has been declared finished. `scripts/brand/selftest.sh` plants each
of those two states and requires the check to go red on each.

Unlike the dated notes beside it, **this file is kept in step**. It is an
inventory of what is in the tree right now, not a record of a decision at a
moment in time.

## The inventory

`Sizes` is what the asset is declared at, not what a browser might scale it to.
A `viewBox` entry means the file is resolution-independent and the number is its
user-unit coordinate system.

| Asset | Sizes | Source | Embedded / referenced at |
| --- | --- | --- | --- |
| `assets/logo-mark.svg` | `viewBox 0 0 48 48` | drawn | export source for `docs/assets/logo-mark-256.png` and `docs/assets/logo-mark-640x320.png` |
| `docs/assets/logo-light.svg` | `viewBox 0 0 122.2 48` | drawn | `README.md`, `width="175"`, the light-scheme `<img>` |
| `docs/assets/logo-dark.svg` | `viewBox 0 0 122.2 48` | drawn | `README.md`, the `prefers-color-scheme: dark` `<source>` |
| `docs/assets/logo-mark-256.png` | 256×256, safe-area framing | `assets/logo-mark.svg` | nothing in-tree: it exists for use outside the repository |
| `docs/assets/logo-mark-640x320.png` | 640×320, bleed framing | `assets/logo-mark.svg` | nothing in-tree: social-card proportions, for use outside the repository |
| `docs/site/assets/icon.svg` | `viewBox 0 0 48 48` | drawn | `<link rel="icon">` on all six `docs/site/*.html`; export source for the site favicons |
| `docs/site/assets/logo-mark-light.svg` | `viewBox 0 0 48 48` | drawn | the hero `<img>` on all six `docs/site/*.html`; export source for both `apple-touch-icon.png` |
| `docs/site/assets/favicon.ico` | 16×16, 32×32, 48×48 | `docs/site/assets/icon.svg` | `<link rel="icon" type="image/x-icon">` on all six `docs/site/*.html` |
| `docs/site/assets/favicon-16.png` | 16×16 | `docs/site/assets/icon.svg` | `<link rel="icon" sizes="16x16">` on all six `docs/site/*.html` |
| `docs/site/assets/favicon-32.png` | 32×32 | `docs/site/assets/icon.svg` | `<link rel="icon" sizes="32x32">` on all six `docs/site/*.html` |
| `docs/site/assets/apple-touch-icon.png` | 180×180 | `docs/site/assets/logo-mark-light.svg` | `<link rel="apple-touch-icon">` on all six `docs/site/*.html` |
| `ui/shared/public/icon.svg` | `viewBox 0 0 48 48` | drawn | the in-app mark; `apps/unraid/frontend/webui.json`; `distribution/packaging`'s store-icon control reads it |
| `ui/shared/public/favicon.svg` | `viewBox 0 0 48 48` | drawn | `ui/shared/index.html`; export source for the app favicons |
| `ui/shared/public/favicon.ico` | 16×16, 32×32, 48×48 | `ui/shared/public/favicon.svg` | `ui/shared/index.html` |
| `ui/shared/public/favicon-16.png` | 16×16 | `ui/shared/public/favicon.svg` | `ui/shared/index.html` |
| `ui/shared/public/favicon-32.png` | 32×32 | `ui/shared/public/favicon.svg` | `ui/shared/index.html` |
| `ui/shared/public/apple-touch-icon.png` | 180×180 | `docs/site/assets/logo-mark-light.svg` | `ui/shared/index.html` |
| `docs/submission/icon.svg` | `viewBox 0 0 256 256` | drawn | the store-listing icon for **all eleven** providers: `distribution/packaging/submission.json`'s `materials-icon`, the six `docs/submission/*.md` material tables, `apps/truenas/catalog/app.yaml`, and both `apps/unraid/template/*.xml` `<Icon>` URLs |
| `apps/casaos/icon.svg` | `viewBox 0 0 64 64` | drawn | `apps/casaos/compose/retnd.yml`'s `x-casaos` icon URL; `distribution/packaging/compliance.json` |
| `apps/zimaos/icon.svg` | `viewBox 0 0 64 64` | drawn | `apps/zimaos/compose/retnd.yml`'s `x-casaos` icon URL; `distribution/packaging/compliance.json` |
| `apps/portainer/logo.svg` | `viewBox 0 0 64 64` | drawn | `apps/portainer/templates.json`'s `logo` URL; `distribution/packaging/compliance.json` |
| `docs/design/893-16px-wordmark-or-monogram.png` | 2240×3660 (1120 CSS px at 2×) | `docs/design/893-16px-wordmark-or-monogram.html` | the picture beside the 16-pixel decision, per the `docs/design/` note convention |
| `docs/design/814-workflow-ui.png` | export of its `.html` | dated design note | record only: see **Design-note art** below |
| `docs/design/815-step-terminal.png` | export of its `.html` | dated design note | record only |
| `docs/design/906-shell-verification-findings.png` | export of its `.html` | dated design note | record only |
| `docs/design/activity-error-diagnostic.png` | export of its `.html` | dated design note | record only |
| `docs/design/activity-terminal.png` | export of its `.html` | dated design note | record only |
| `docs/design/cancel-edit-mode.png` | export of its `.html` | dated design note | record only |
| `docs/design/global-terminal.png` | export of its `.html` | dated design note | record only |
| `docs/design/retention-plan-destinations.png` | export of its `.html` | dated design note | record only |
| `docs/design/run-backup-set.png` | export of its `.html` | dated design note | record only |
| `docs/design/s3-destination-wizard.png` | export of its `.html` | dated design note | record only |
| `docs/design/set-activity-terminal.png` | export of its `.html` | dated design note | record only |
| `docs/design/ssh-auth-wizard.png` | export of its `.html` | dated design note | record only |
| `docs/site/screens/` | 35 PNG, 20 GIF, 11 superseded | `docs/site/tools/capture-*.mjs` | the six `docs/site/*.html` pages. The one **directory row** in this table: these are product captures re-recorded by committed tooling and gated by the site half of #893, not art anybody hand-maintains, and 66 per-file rows would go stale on the next recording |

## Re-exporting

`scripts/brand/export-rasters.sh` regenerates every raster row above from the
four SVG sources. It is idempotent: run it against an unchanged tree and
`git status` stays clean.

Three framings, measured off the committed PNGs before they were replaced so
that the re-export reproduced what was there rather than quietly re-cropping it:

| Framing | Ink as a share of the canvas | Used by |
| --- | --- | --- |
| natural | 81.25% — the `48` viewBox rendered straight | the favicon PNGs and all three `.ico` members |
| safe-area | 49%, centred | `apple-touch-icon.png`, `logo-mark-256.png` |
| bleed | ink fills the canvas height, centred on 2:1 | `logo-mark-640x320.png` |

## The one `<title>` that survives, and why it is not a hole

`scripts/brand/check-svg-text.sh` refuses `<text>`, `<tspan>`, `<title>` and
`<desc>` in a source SVG. `docs/submission/icon.svg` carries a `<title>`
anyway, and it is pinned in the check by path rather than missed by it.

Two requirements collide here and both are right.
`distribution/packaging`'s `CheckStoreIcon` **requires** a store-listing icon to
carry a non-empty `<title>`, because several catalogue front ends read it as the
image's alt text and none of them reads `aria-label`; dropping it turns all
eleven providers' `materials-icon` rows red in
`docs/conformance/submission-preflight.md`, which is how this was found rather
than argued about. FR-44 bans `<title>` because it is a brand string a rename
can miss.

The resolution is narrower than either: for a pinned path the element is
allowed and **its text is checked**, on one line, against the product name. So
the violation FR-44 names — a `<title>` carrying the retired name — still turns
the scan red in that file, and more definitely than the ban would have, because
the ban was satisfiable by deleting the element and this is satisfiable only by
the right string. `<text>`, `<tspan>` and `<desc>` stay banned there. The
exemption is by path, so the same `<title>` in any other SVG is still a finding.
`scripts/brand/selftest.sh` holds all four of those claims.

## The wordmark, and how it was re-fitted

The previous lockup set its wordmark as an SVG `<text>` element in IBM Plex
Mono 600 at 22px, two-tone, with the trailing `d` in a muted tone. Two things
had to change and they are different changes.

**The text became geometry.** A `<text>` element is a string a rename can miss
and a raster export turns into pixels nothing can grep — the whole subject of
FR-44 — so the letters are `<path>` data now, and
`scripts/brand/check-svg-text.sh` keeps them that way.

**Six glyphs became four, so the lockup was re-fitted rather than
re-lettered.** The construction, all of it in the mark's own 48-unit
coordinate system:

- **Monoline, stroke 3, round caps and joins.** This is the mark's own
  drawing language — the mark is a round-capped stroke of 5 on the same grid —
  so the wordmark is built from it rather than beside it.
- **One vertical system.** Baseline `y=32`, x-height 11.4 (`y=20.6`), ascender
  16 (`y=16`), used by `t` and `d`. The ink band is therefore `y=14.5` to
  `y=33.5`, whose centre is `y=24`: the mark's own centre, which is what makes
  the two sit on one optical line without a nudge.
- **One glyph width.** Centreline width 8 for all four, so `r`, `e`, `t`, `n`
  and `d` are the same skeleton width and only their arcs differ. The arcs are
  exact: `r` and `n` spring on a semicircle of radius 4 whose apex is the
  x-height line, and `e` and `d` are half-ellipses of `rx=4, ry=5.7`.
- **Spacing is optical, not monospaced, and that is the re-fit.** The previous
  lockup inherited a monospace pitch of 13.0 from the typeface. Four glyphs do
  not carry a mono rhythm — with six, the even pitch reads as a system; with
  four it reads as a gap — so the glyphs are placed on ink-edge gaps instead:
  0.4 after `r` (whose right side is empty below the x-height, so the nominal
  gap reads as a hole), 1.4 for `e|t` and `t|n`, and 1.6 for `n|d`, which is
  the only pair that is two full stems facing each other. Left edges land at
  `x = 60, 70.8, 83.2, 95.6, 108.2`.
- **The lockup box follows the ink.** `viewBox` went from `0 0 168 48` to
  `0 0 122.2 48`: the mark's ink ends at `x=43.5`, the wordmark's ink runs
  `x=58.5` to `x=117.7`, and the right padding is set to 4.5 to match the
  mark's own inset on the left. `README.md`'s `width` went from 240 to 175,
  which is the width that renders the mark at exactly the size it was before.
- **The accent stayed on the `d`.** The previous name ended in `d` and so does
  this one, so the two-tone split needed no letter reassignment: `d` is painted
  `#8b8d8a` on light and `#767976` on dark, the same two values as before, and
  `retn` carries the full-strength tone. Keeping the tones unchanged was
  deliberate — the re-fit changes the geometry and only the geometry.

`docs/design/893-16px-wordmark-or-monogram.html` is the dated note for the one
decision FR-44 names explicitly: at 16 px the favicon is the **mark**, not the
wordmark and not a `d` monogram, with the three candidates rendered at 16 px and
shown at 8× beside the reasoning.

## Design-note art

The twelve `docs/design/*.png` mockups other than this issue's own are
**deliberately not regenerated**, and that is a decision rather than an
omission. `docs/epic-checklist.md` §5 and
`docs/EPIC-R-rename-backupd-to-retnd.md` §6 both say a dated design note
records a decision at the moment it was taken and is not kept in step with
later work; their `.html` sources are on the brand-drift guard's `preexisting`
list for that reason, and re-exporting the pictures from sources that are
deliberately stale would make the pictures disagree with their own sources
while claiming to be current. They are in the inventory so that they are
accounted for, and their row says `record only`.

## What is NOT redrawn, and why

- **The mark.** Option 1a "Cycle" (`docs/design/Logo Options.dc.html`) is
  unchanged: a broken ring reading as a transfer cycle in progress. It depicts
  no name, so a rename has nothing to say to it, and every raster above is the
  same drawing at a different size.
- **`apps/casaos/icon.svg`, `apps/zimaos/icon.svg`, `apps/portainer/logo.svg`.**
  These three provider tiles draw a *different* broken ring, in teal on dark
  slate, rather than the product mark in the product's blue. That divergence is
  real and predates this issue; it is out of FR-44's scope because it is not a
  name, and R2.1 (#891) regenerated these files already. Recorded here so the
  next person to look does not have to rediscover it.
- **The in-product wordmark is type, not art.** `ui/shared/src/components/Logo`'s
  `Wordmark` and `docs/site/*.html`'s `.brand-name` header both set the name in
  `--font-mono` with the trailing `d` in the muted text token — the same split,
  in the same order, as the drawn lockup, but rendered by the reader's font
  rather than by path data. That is intentional: those two surfaces theme with
  the provider accent and must not carry an asset per provider. It does mean
  the drawn lockup and the in-product lockup are different objects that happen
  to agree, and nothing automated holds them to each other.

## Human acceptance (FR-44)

**Status: OUTSTANDING. This step has not been performed.**

FR-44 requires a named human-in-the-loop acceptance step, with this manifest in
front of the reviewer, asserting that no asset listed above *depicts* the
previous name. It is recorded here and as row **R2.14** of
`docs/conformance/epic-r-matrix.md`, where the outcome is `PARTIAL` and will
stay `PARTIAL` — not because the work is unfinished, but because half of this
row is a judgement and calling a judgement `PASS` is how "unverified" becomes
"certified".

What is automated, and has been watched to fail:

| Half | Check | Falsification, run |
| --- | --- | --- |
| Manifest completeness | `scripts/brand/check-brand-assets.sh` | a row with no file, and a file with no row; red on each, both automated in `scripts/brand/selftest.sh` |
| Text nodes in source SVGs | `scripts/brand/check-svg-text.sh` | a planted `<title>` carrying the old name in a source SVG; red, automated in the same self-test — including in `docs/submission/icon.svg`, whose `<title>` is pinned rather than banned (above) |

What is **not** automated, and cannot be:

- **No check reads a picture.** `check-brand-drift.sh` greps tracked source and
  passes `-I`, so it never opens a raster at all; `check-svg-text.sh` goes blind
  the moment letters are `<path>` data, which is exactly what this issue made
  them. Both facts are structural, not a gap waiting on tooling.
- **The letterforms have had no design review.** They are a *constructed*
  wordmark: one stroke weight, one glyph width, one x-height, arcs that are
  exact semicircles and half-ellipses, and inter-glyph gaps chosen by
  measurement. There is no optical stem correction, no overshoot on the round
  glyphs, and no ink traps, because those are judgements and none was made.
  A designer looking at this may well re-cut `e` and `r`.
- **Nothing here was seen against real hardware.** The store-listing icon and
  the touch icon are judged on a NAS tab bar and on a phone home screen; they
  were checked as rendered PNGs on a workstation.

The reviewer, when there is one, should look at: the two lockups at
`README.md`'s own 175 px; `favicon-16.png` in an actual browser tab strip
beside other favicons; `apple-touch-icon.png` on a real home screen; and
`docs/submission/icon.svg` at the size each of the eleven stores renders a
listing tile. Record the outcome by replacing this section, and only then is
R2.14's second half anything other than outstanding.

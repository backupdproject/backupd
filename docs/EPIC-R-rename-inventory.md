# EPIC R rename inventory: every token marked for renaming, in both repositories

The scope of `docs/EPIC-R-rename-backupd-to-retnd.md`, enumerated token by token so
that nothing is missed by being unnamed. This file is the committed artifact the
spec's §2 points at, and it is the list the guard's `pending` entries are derived
from (FR-40).

**The finding this file exists to make unavoidable: the previous two renames did not
finish, and this EPIC renames what they left behind as well.** Three brands' worth of
identifiers are live in these two trees right now — `rclone-manager`'s
`RCLONE_MANAGER_*` and `RM_*` environment prefixes, `backup-manager`'s `bm_` cookies,
and `backupd` itself. EPIC R takes all of them to `retnd`.

## Method, and how to reproduce a number

Both repositories, tracked files only, `-I` so no binary is scanned:

```sh
git grep -lIE  <pattern>   | wc -l      # files
git grep -cIE  <pattern>   | awk -F: '{s+=$NF} END{print s}'   # LINES
git grep -ohIE <pattern>   | wc -l      # OCCURRENCES
```

`backupd` @ `6a528c98` (public, 1,938 tracked files) and `backupd-tests` @ `35b15e2`
(private, 230 tracked files). Patterns marked *ci* were run with `-i`.

**Lines are not occurrences**, and the difference is large enough to matter: `backupd`
is 7,679 lines and 10,522 occurrences in the main repository. Both are given below so
a number can be checked rather than trusted.

Two corrections to the figures this inventory was commissioned from, because a guard
treats these as different patterns and conflating them would allowlist the wrong one:

- the "`BM_[A-Z]` 53/13f" figure is actually **`bm_[a-z]`** (53 occurrences, 13 files).
  Real `BM_[A-Z]` is 2 occurrences in 1 file, and that file is the guard's own
  self-test, which plants the string on purpose.
- likewise in `backupd-tests`: "`BM_[A-Z]` 20/6f" is `bm_[a-z]`; `BM_[A-Z]` is zero.
- `rbm_[a-z]` is 2 occurrences in 1 file, also the self-test. So `BM_` and `rbm_` have
  **no real occurrences in either tree**, which is the one piece of good news here.

## Org-wide totals

| Token | `backupd` files / lines / occ | `backupd-tests` files / lines / occ | Org lines |
|---|---|---|---|
| `backupd` *(ci)* | 1,339 / 7,679 / 10,522 | 153 / 527 / 682 | **8,206** |
| `backupdproject` *(ci)* | 904 / 2,363 / 2,369 | 44 / 115 / 121 | **2,478** |
| `RM_[A-Z]` | 38 / 201 / 207 | 24 / 149 / 173 | **350** |
| `backup_manager` *(ci)* | 37 / 112 / 127 | 1 / 1 / 1 | **113** |
| `rclone_manager` *(ci)* | 19 / 48 / 53 | 0 / 0 / 0 | **48** |
| `rclone-manager` *(ci)* | 17 / 27 / 27 | 3 / 3 / 3 | **30** |
| `backup-manager` *(ci)* | 4 / 8 / 8 | 1 / 2 / 2 | **10** |
| `bm_[a-z]` | 13 / 51 / 53 | 6 / 20 / 20 | **71** |
| `backup manager` *(ci, space)* | 3 / 3 / 3 | 0 / 0 / 0 | **3** |
| `BM_[A-Z]` | 1 / 2 / 2 *(self-test only)* | 0 / 0 / 0 | **2** |
| `rbm_[a-z]` | 1 / 2 / 2 *(self-test only)* | 0 / 0 / 0 | **2** |

124 filenames in the main repository carry a token, 101 of them under `core/cmd`.

## The actionable table

Owning sub-issue, and what each class costs. "Gated" means a check already fails if
it is missed; "guard" means the extended brand-drift guard of R1.2 is the only thing
that would notice.

| # | Token class | Target | Repos | Files / lines | Owner | Gated? | Compat note |
|---|---|---|---|---|---|---|---|
| 1 | `github.com/backupdproject/backupd` (module path, imports) | `github.com/retnd/retnd` | both | 840 / 2,163 occ | **R1.3** | gated: nothing compiles | One atomic commit. Module path does not match its fetch location until the cutover; that transient is declared and guarded (FR-41) |
| 2 | `backupd` / `backupd-web` as binary, `cmd` directory and filename | `retnd` / `retnd-web` | main | 129 filenames | **R1.3** | gated (7 tests) | 101 of the 124 renamed filenames are here |
| 3 | `cliecho.Binary` / `WebBinary`, `legacyName` | `retnd` / `retnd-web`; `legacyName` `rbm` → `backupd` | main | 2 files | **R1.3** | gated | `cliname.go` is the single spelling by design |
| 4 | `BACKUPD_[A-Z]*` (46 names) | `RETND_*` | main | 84 / 279 occ | **R1.4** | guard | **Silent** if hard-cut: 20 are exported into operator Bash. Both names one release |
| 5 | `RM_[A-Z]*` (the black-box suites' env contract) | `RETND_*` | **both** | 62 / 350 | **R2.5** | gated both sides | Cross-repo, in lockstep with `tests-repo.pin`. Comes **off** the guard's `preexisting` list, where it has been parked since #794 |
| 6 | `RCLONE_MANAGER_[A-Z]*` — incl. `RCLONE_MANAGER_SOURCE_PORT` (installer, documented in `docs/install.md`), `RCLONE_MANAGER_MACHINES_NETWORK`, and `RCLONE_MANAGER_UNIT` heredoc sentinels | `RETND_*` | main | 19 / 48 | **R1.4** (installer sites with **R1.5**, the prose with R2.2) | **guard-invisible today** | The hole this inventory found: none of the guard's four patterns matches `RCLONE_MANAGER_`, so a first-brand env prefix has sat green for two renames — and one of them is operator-facing |
| 7 | `backupd_session` / `backupd_csrf`, and `bm_session` / `bm_csrf` read-compat | `retnd_session` / `retnd_csrf`; the `bm_` window **closes here** rather than being extended | both | 19 / 71 | **R1.4** | gated | #794's `bm_` window has already outlived its release; EPIC R does not open a third nested window |
| 8 | `backupd_*` metric series (14) | `retnd_*` | main | 22 / 76 occ | **R1.4** | gated (name pin) | **Silent** if hard-cut. Gauges duplicated one release, never a counter |
| 9 | `/etc/backupd`, `/var/lib/backupd` and every persisted absolute value | `/etc/retnd`, `/var/lib/retnd` | main | 152 / 376 occ | **R1.5** | partly | **Data-loss class.** FR-38's four-cell adoption table |
| 10 | compose service / project / container names, systemd units, `release-manifest.json` keys | `retnd*` | main | 54 / ~100 | **R1.5** | gated | Loud. `/backupd-web` alias entrypoint for one release |
| 11 | `ghcr.io/backupdproject/backupd` | `ghcr.io/retnd/retnd` | main | 46 / 121 occ | **R2.5** (cutover only) | gated | Registry paths are **not** redirected; mirrored one release from the retained old org |
| 12 | `backupd` across the eleven providers and `distribution/packaging/*.json` | `retnd` | main | 230 / 1,394 occ | **R2.1** | gated | Manifests are derived: regenerate, never hand-edit |
| 13 | `backupd` in prose: README, 56 `docs/*.md`, 13 ADRs, CONTRIBUTING, CLA, `docs/submission/*.md` | `retnd` | both | 92 / ~480 | **R2.2** | partly | `CHANGELOG.md` is history and keeps its entries |
| 14 | `backupd` on the site, in `<title>`s and in the brand **art** | `retnd`, art **redrawn** | main | 12 / 139 occ + ~70 assets | **R2.3** | structurally ungated for art | FR-44: manifest + SVG string scan + human acceptance. No text guard can read a `<path>` or a raster |
| 15 | `Backupd` / `BackupdError` in the UI, `tooltips.json`, `fieldHelpCopy.ts` | `retnd` / `RetndError` | main | 201 / 757 occ | **R2.4** | gated | The nine `BackupD[a-zA-Z]` identifiers must **not** move |
| 16 | `@backupd/ui-shared`, `@backupd/provider-conformance` + two lockfiles | `@retnd/*` | main | 4 / 6 | **R2.4** | gated (workspace resolution) | An npm scope is a package name, not prose; nearly missed |
| 17 | `backupd` in `backupd-tests`: `suites/{cli,web-ui,equivalence}`, fixtures, tools, `build-under-test.json` | `retnd` | tests | 153 / 527 | **R2.5** | gated their side | Lockstep with `tests-repo.pin`; their side lands first or the gate is red |
| 18 | `backupdproject` org + `backupd`/`backupd-tests` repo names, Pages origin, 4 `raw.githubusercontent.com` URLs, cosign/OIDC identity, absolute links | `retnd/retnd`, `retnd/retnd-tests`, `retnd.github.io/retnd` | both | 948 / 2,478 | **R2.5** | gated (release gate, Pages) | FR-41. Redirects cover git/web and preserve every `#N`; they do not cover the raw host, the registry path, Pages or the signing identity |
| 19 | `rclone-manager`, `rclone_manager`, `backup-manager`, `backup_manager` as prose and identifiers | `retnd` | both | ~60 / ~190 | **R2.2** (prose), **R1.4** (identifiers), **R1.2** (guard lists) | guard, once R1.2 adds the patterns | The unfinished half of two earlier renames. `CHANGELOG.md` and the dated `docs/design/` notes keep theirs |
| 20 | `backup manager` with a space — incl. the filename `docs/design/Backup Manager.dc.html` | **unchanged** | main | 3 / 3 | **R1.2** (`preexisting`) | guard | A dated design record. Renaming it falsifies the record (checklist §5) |
| 21 | `BM_[A-Z]`, `rbm_[a-z]` | **unchanged** | main | 1 / 4 | **R1.2** | gated | Self-test plants only. Zero real occurrences in either tree |
| 22 | `ibm_signer.go` (rclone's own S3 backend, in `compliance.json` and `source-offer.md`) | **unchanged** | main | 2 / 5 | **R1.2** (lookalike control) | gated | The guard's original false-positive case, and still one |
| 23 | `provenance/**`, `NOTICE`, `compliance.json`, published `release-manifest.json` entries | regenerated **forward** | main | 5 / 314 occ | **R2.5** | gated | Immutable records. They grow; they are never rewritten |

## Definition-of-done row this file contributes

> The org-wide deep grep, across both repositories, for
> `backupd|backupdproject|rclone[-_ ]manager|backup[-_ ]manager|RCLONE_MANAGER_|RM_[A-Z]|BM_[A-Z]|bm_|rbm_`
> returns **only** the enumerated allowlist: immutable history (rows 20, 22, 23,
> `CHANGELOG.md`, git messages, the dated design notes, this document and the spec)
> plus the time-boxed compat shims of FR-43, each with its named removal release.
> After that release the allowlist is history-only.

The grep is the going-forward extension of `check-brand-drift.sh`: R1.2 adds the
patterns for `backupdproject`, `rclone[-_ ]manager`, `backup[-_ ]manager` and
`RCLONE_MANAGER_`, so from Phase 1 onward a new occurrence of any of the three brands
is a creation and goes red, rather than something a fourth rename discovers.

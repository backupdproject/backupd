# ADR 0023: Moving the repository coordinates once, and last

- Status: accepted, and **performed**
- Date: 2026-09-14; the transfer it decides was carried out on 2026-09-15
- Scope: EPIC R (#885), FR-41. Recorded by R2.2 (#892); the transfer itself was
  performed by R2.5 (#895), in the PR that closed it. Builds on the in-tree
  rename that R1.3 (#888) swept and does not re-litigate FR-36 through FR-40.

## Context

EPIC R renames the product to `retnd`, and the product's name is inside its
coordinates: the Go module path is `github.com/<org>/<repo>`, the image path
is `ghcr.io/<org>/<repo>`, and the documentation site is served from
`<org>.github.io/<repo>`. So the rename cannot stop at the tree. The
organisation `backupdproject` holds exactly two repositories, `backupd`
(public) and `backupd-tests` (private), and both move into a new organisation
`retnd`:

| Before | After |
|---|---|
| org `backupdproject` | org `retnd` |
| `backupdproject/backupd` (public) | `retnd/retnd` |
| `backupdproject/backupd-tests` (private) | `retnd/retnd-tests` |
| `github.com/backupdproject/backupd` (module) | `github.com/retnd/retnd` |
| `ghcr.io/backupdproject/backupd` | `ghcr.io/retnd/retnd` |
| `https://backupdproject.github.io/backupd/` | `https://retnd.github.io/retnd/` |

This is the third rename of this product, and the previous two are why this is
an ADR rather than a checklist item: `rclone-manager` left `RM_` environment
variables behind and `backup-manager` left `bm_` cookies behind, and four
follow-up issues exist solely to finish work a rename had declared done.

## Decision 1: the whole in-tree rename lands first, green; the transfer is the last act

The organisation creation and the two repository transfers happen **after**
everything in the tree is green, not in the middle of it.

The alternative was rejected on diagnosability rather than on taste. The
module-path sweep was 2,273 occurrences across 840 files, five `go.mod` files
and `go.work`, and it is atomic: there is no intermediate commit in which the
tree compiles. A transfer performed inside that window means the sweep and the
move can fail in the same afternoon, and neither can be diagnosed without
undoing the other. Held apart, each has one failure mode and one rollback.

## Decision 2: the organisation name is reserved as a precondition, not as a cutover step

The `retnd` organisation name is reserved **before** R1.3 sweeps to it, and
that ordering is load-bearing: after that sweep, 2,273 occurrences and the
image path both name an organisation this project does not own, so losing the
name would invalidate the module path and the image reference at once. It is
therefore a Phase 1 exit-gate line rather than a step in the cutover.

## Decision 3: the module path is swept once, straight to its final value, and the mismatch window is a declared transient

`github.com/retnd/retnd` is what the tree said from R1.3 onwards, before the
repository lived there. Sweeping to an interim path would have meant doing the
2,273-occurrence change twice.

From R1.3 until the cutover, the module path therefore did not match the
location the module is fetched from. That cost exactly nothing for as long as
nothing instructed anybody to fetch it: every build here is a workspace build,
or `GOWORK=off` over the `replace` directives in the five `go.mod` files.
`scripts/rename/check-no-module-fetch-instruction.sh` is what kept that true
— it failed on a tree that told a reader to `go install` or `go get` this
module — and #895's cutover PR **deleted** it, along with its steps in
`scripts/ci-local.sh` and `.github/workflows/ci.yml`, because the instruction
it forbade is now simply correct.

**The transient is closed, and it is closed by measurement rather than by
this sentence.** `go.mod` declares `module github.com/retnd/retnd` and
`git remote get-url origin` answers `https://github.com/retnd/retnd.git`, so
the path and the fetch location are the same string. That is the third of the
three probes the cutover is recorded with.

## Decision 4: the redirect is relied on for what it actually covers, and for nothing else

GitHub's transfer and rename set up redirects for web and git operations, and
the repository object is the same object. So **issue and pull-request numbers
are preserved**: every `#N` in the 66 KB of `CHANGELOG.md`, the 2,183 lines of
`docs/EPIC.md` and every ADR keeps resolving, clones and `git remote` URLs
follow the redirect, and so do release download URLs. That mitigation is
recorded here so that nobody re-litigates it from memory, and it is the reason
this epic does not have to rewrite its own history.

## Decision 5: the four references a redirect does not cover are re-established by hand, in the same window

Each one is followed by what was actually observed on 2026-09-15, because the
point of enumerating them was to make each one falsifiable rather than assumed.

1. **The signing identity is re-issued, not re-pointed.** `cosign verify`'s
   `certificate-identity` is the OIDC subject of the release workflow, derived
   from the repository path, and it is baked immutably into every signature
   already published. New releases carry `retnd/retnd`'s identity and already
   published ones keep `backupdproject/backupd`'s, so the documented `verify`
   command presents the old identity for releases published before the cutover
   and the new one for releases after it, **keyed by version**. The SLSA
   provenance builder identity moves with it. Any downstream policy pinning
   the old identity is named in the release notes, because a policy that
   silently starts rejecting new releases is the worst shape this can take.

   *Done in the tree, and unproven in the registry.* Both identities are
   constants in `distribution/packaging/signing.go` (`SigningIdentity`,
   `PreCutoverSigningIdentity`) with `LastPreCutoverRelease` as the boundary,
   `docs/compliance/release-provenance.md` prints both commands keyed by that
   version, and `TestComplianceDocsPrintTheCommandThatPasses` refuses a pin
   that is neither and refuses the file for dropping either one. What cannot
   be shown yet is the *behaviour*: `gh release list` names no release and
   `gh api orgs/retnd/packages?package_type=container` and the same call
   against `backupdproject` both answer an empty list, so there is no signed
   artifact in either registry path to verify against either identity. That is
   why Decision 7 makes the first release after the cutover the checkpoint.
2. **`raw.githubusercontent.com` does not follow repository redirects.** Four
   URLs are served from the raw host — the installer script in the README, the
   site, two provider listings and `docs/submission/icon.svg`. They are
   republished at the new path and every occurrence updated, and the old path
   is treated as dead rather than assumed to redirect. An operator with the
   installer URL in a runbook is told, in the release notes, that it changed.

   *Done, and the assumption was the safe one.* Every new raw URL answers
   `200`: `main/scripts/install/install_docker_host.py`,
   `main/docs/submission/icon.svg`, `main/apps/portainer/logo.svg` and the
   CasaOS, ZimaOS and Unraid listing assets. Measured surprise worth writing
   down rather than hiding: the OLD raw path answers `200` today too, so the
   raw host is in fact following the transfer at the moment. It is still
   treated as dead, because a redirect nobody documents is a redirect nobody
   owes you, and every occurrence was moved anyway.
3. **A registry path is not redirected to a new owner.**
   `ghcr.io/backupdproject/backupd` is mirrored **for one release** from a thin
   retained repository in the retained `backupdproject` organisation, which
   makes "do not delete the old organisation" an explicit requirement of this
   epic rather than an oversight. The mirror is on
   `scripts/rename/check-brand-drift.sh`'s alias list with the issue that
   deletes it (#947) and its removal release.

   *Declared, and a formality rather than a rescue.* `image.reference` in
   `distribution/packaging/canonical.json` is `ghcr.io/retnd/retnd:0.4.0` and
   `image.mirror.reference` names the retained path, which is the state guard 7
   in `scripts/bdtools/release/publish_image.py` requires — both pushed, same
   build, same digest. But
   `gh api orgs/backupdproject/packages?package_type=container` answered an
   **empty list**, and an anonymous `ghcr.io` pull token for either package
   path is refused, so no image has ever actually been published under the old
   path and there is no existing `docker pull` of it to strand. The mirror
   stays declared until #947 regardless: one extra push is cheap, and being
   wrong about that reading costs an operator a 404 with no explanation.
4. **The Pages origin ceases to exist.** `backupdproject.github.io/backupd`
   has no redirect at all, so the eight-plus absolute links move with the
   cutover and the old origin is documented as dead in the release notes and
   in the site's own honest section.

   *Done, and this one was exactly as predicted.* `https://retnd.github.io/retnd/`
   answers `200` and `https://backupdproject.github.io/backupd/` answers `404`.
   Pages followed the transfer on its own; the old origin is gone, and it is the
   one reference of the four where "assume it redirects" would have been wrong.

## Decision 6: the org-level carry-over is a written checklist, because none of it is in a diff

Everything that is repository *configuration* rather than repository content
has to be rebuilt on the other side, and nothing in the repository will notice
it is missing until a release fails. R2.5 carries it as a checklist:
organisation profile, avatar and branding; teams and members; organisation and
repository secrets and variables; GHCR package ownership and visibility;
Actions settings (permissions, allowed actions, default `GITHUB_TOKEN`
permissions); branch-protection rules and the required-check list, which names
jobs by name and is silently empty on a fresh repository; the Pages source and
any custom domain; the label set, including `epic-r`, `R:phase-1` and
`R:phase-2`; and both repository descriptions and topics, which SHALL say
`retnd` — `backupd-tests`'s description still said `rclone-manager` two renames
later, which is this epic's thesis stated by the repository itself.

**What the checklist actually found, on 2026-09-15.** Most of it carried over
by itself, because a transfer moves the repository object: issues, pull
requests and their numbering, the label set (all 62, `epic-r`, `R:phase-1` and
`R:phase-2` among them), and the Pages source, which republished at
`retnd.github.io/retnd` without being asked. Descriptions, topics and the
organisation profile were updated by hand, including `retnd-tests`'s, which
now reads "Black-box end-to-end test suites for retnd". Two items turned out
to be **moot rather than done**, and saying which is the point of a checklist:

- **GHCR package ownership and visibility.** There is nothing to move.
  `gh api orgs/backupdproject/packages?package_type=container` answers an empty
  list, so the organisation has never owned a container package.
- **Branch protection and the required-check list.** There is nothing to
  restore. `gh api repos/retnd/retnd/branches/main/protection` answers `404
  Branch not protected`, and so did the same call against the old coordinates
  *before* the transfer. This repository has never had branch protection
  configured, so "restored" was never the right word, and the control this ADR
  and row R2.17 of the conformance matrix ask for — a deliberately failing
  check *blocked* by protection on the new repository — cannot be run against
  a repository that has none. R2.17 is `PARTIAL` for that reason and says so.
  Configuring protection for the first time is a decision about how this
  project merges, which is not a rename's to make.

## Decision 7: the rollback window closes at the first release after the transfer, not at the transfer

A transfer is reversible: the organisation retains the name, a repository can
be transferred back, and redirects are re-established in the other direction.
The rollback note states the window, and the order things are undone in:
transfer both repositories back, then re-point the module path and the image
reference, then restore the required-check list.

One thing is **not** reversible: a signature issued under the new identity
stays issued. That is why the checkpoint is the first release published after
the cutover rather than the transfer itself — until then, nothing irreversible
has been produced.

**The window is open as of 2026-09-15, and the procedure above is unchanged
except for its last step, which is empty:** there is no required-check list to
restore, because there was none to begin with (Decision 6). Rolling back is
therefore two GitHub transfers plus reverting the module path and the image
reference, and nothing irreversible has been produced yet — `gh release list`
names no release, so no signature has been issued under the new identity at
all. The window closes the first time a release is published from here.

## What has happened

Recording a decision as performed means saying which half is performed, and
this table is now the record of a transfer that has been carried out rather
than of one that is coming.

| Step | State |
|---|---|
| `retnd` organisation name reserved | Phase 1 exit-gate line, held before R1.3 swept to it |
| module path swept to `github.com/retnd/retnd` | done, R1.3 (#888) |
| `scripts/rename/check-no-module-fetch-instruction.sh` guarding the transient | **deleted**, R2.5 (#895), with its steps in `scripts/ci-local.sh` and `.github/workflows/ci.yml`: the path and the fetch location are the same string now |
| organisation created, both repositories transferred | **done**, 2026-09-15, R2.5 (#895). `git ls-remote https://github.com/backupdproject/backupd.git HEAD` still resolves, and issue #895 resolves at both coordinates |
| cosign/OIDC identity re-issued, the documented `verify` keyed by version | **done in the tree**, R2.5 (#895); unproven in the registry until a release is published, see Decision 5.1 |
| four `raw.githubusercontent.com` URLs republished | **done**, R2.5 (#895); all answer `200` at the new path |
| image path moved with its one-release mirror | **done**, R2.5 (#895): `image.reference` is `ghcr.io/retnd/retnd:0.4.0`, `image.mirror` retains the old path until #947 |
| Pages origin moved, old origin documented as dead | **done**, R2.5 (#895): new `200`, old `404` |
| org-level carry-over and the rollback note | **done**, R2.5 (#895), with two items recorded as moot rather than ticked: see Decision 6 |

No absolute `backupdproject` URL is left in the tree outside an enumerated
allowlist: five alias entries for the one-release `ghcr.io` mirror, five
`preexisting` pins for the immutable pre-cutover signing identity, the two gate
steps that spell the guard's own pattern list and this ADR, and the four
documents that RECORD the rename. The guard's `pending` list no longer carries
`backupdproject` at all, which is what makes the cutover's completion a measured
fact rather than somebody's reading of this document.

## Consequences

- The rename is diagnosable: an in-tree failure is a failed build on a
  repository nobody moved, and a failed transfer is a transfer on a tree that
  was already green.
- For one release the project publishes two image paths, and the old
  organisation cannot be deleted. Its deletion is the shim-removal issue's last
  line, not this epic's.
- `cosign verify` needs the version to pick an identity, so the documented
  command is two commands with a boundary between them, and that boundary is a
  release number somebody has to know. This is the irreducible cost of a
  transfer and the reason it is written down rather than discovered.
- Anything cached outside GitHub that pinned a raw URL, the registry path or
  the Pages origin breaks at the cutover and is named in the release notes,
  because those are the three references a redirect cannot save.

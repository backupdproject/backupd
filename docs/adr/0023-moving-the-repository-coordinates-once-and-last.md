# ADR 0023: Moving the repository coordinates once, and last

- Status: accepted
- Date: 2026-09-14
- Scope: EPIC R (#885), FR-41. Recorded by R2.2 (#892); the transfer itself is
  performed by R2.5 (#895). Builds on the in-tree rename that R1.3 (#888)
  swept and does not re-litigate FR-36 through FR-40.

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

From R1.3 until the cutover, the module path therefore does not match the
location the module is fetched from. That costs exactly nothing for as long as
nothing instructs anybody to fetch it: every build here is a workspace build,
or `GOWORK=off` over the `replace` directives in the five `go.mod` files.
`scripts/rename/check-no-module-fetch-instruction.sh` is what keeps that true
— it fails on a tree that tells a reader to `go install` or `go get` this
module — and the cutover PR **deletes** that check, because afterwards the
instruction it forbids is simply correct.

## Decision 4: the redirect is relied on for what it actually covers, and for nothing else

GitHub's transfer and rename set up redirects for web and git operations, and
the repository object is the same object. So **issue and pull-request numbers
are preserved**: every `#N` in the 66 KB of `CHANGELOG.md`, the 2,183 lines of
`docs/EPIC.md` and every ADR keeps resolving, clones and `git remote` URLs
follow the redirect, and so do release download URLs. That mitigation is
recorded here so that nobody re-litigates it from memory, and it is the reason
this epic does not have to rewrite its own history.

## Decision 5: the four references a redirect does not cover are re-established by hand, in the same window

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
2. **`raw.githubusercontent.com` does not follow repository redirects.** Four
   URLs are served from the raw host — the installer script in the README, the
   site, two provider listings and `docs/submission/icon.svg`. They are
   republished at the new path and every occurrence updated, and the old path
   is treated as dead rather than assumed to redirect. An operator with the
   installer URL in a runbook is told, in the release notes, that it changed.
3. **A registry path is not redirected to a new owner.**
   `ghcr.io/backupdproject/backupd` is mirrored **for one release** from a thin
   retained repository in the retained `backupdproject` organisation, which
   makes "do not delete the old organisation" an explicit requirement of this
   epic rather than an oversight. The mirror is on
   `scripts/rename/check-brand-drift.sh`'s alias list with the issue that
   deletes it (#895) and its removal release.
4. **The Pages origin ceases to exist.** `backupdproject.github.io/backupd`
   has no redirect at all, so the eight-plus absolute links move with the
   cutover and the old origin is documented as dead in the release notes and
   in the site's own honest section.

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

## What has happened at the time of writing

Recording a decision as performed means saying which half is performed.

| Step | State |
|---|---|
| `retnd` organisation name reserved | Phase 1 exit-gate line, held before R1.3 swept to it |
| module path swept to `github.com/retnd/retnd` | done, R1.3 (#888) |
| `scripts/rename/check-no-module-fetch-instruction.sh` guarding the transient | in the tree and gated; deleted by the cutover PR |
| organisation created, both repositories transferred | **not yet**, R2.5 (#895) |
| cosign/OIDC identity re-issued, the documented `verify` keyed by version | **not yet**, R2.5 (#895) |
| four `raw.githubusercontent.com` URLs republished | **not yet**, R2.5 (#895) |
| image path moved with its one-release mirror | **not yet**, R2.5 (#895); the tree still names `ghcr.io/backupdproject/backupd` on purpose (FR-39: the reference moves exactly once) |
| Pages origin moved, old origin documented as dead | **not yet**, R2.5 (#895) |
| org-level carry-over and the rollback note | **not yet**, R2.5 (#895) |

Every absolute `backupdproject` URL still in the tree is deliberate and belongs
to #895. The guard's `pending` list carries `backupdproject` as a single token
for exactly that reason, so the cutover's completion is the list emptying
rather than somebody's reading of this document.

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

# Incremental backup in the web UI — the design gate for #788

This is the design artifact EPIC K's K9 (#788) requires **before any
production UI code is written**: the whole incremental-backup setup and
operation workflow, as screens, so the shape can be argued about while
changing it is still cheap.

It is a **renderable** mock-up rather than a picture. `ui/shared/src/mockup`
mounts thirteen screens plus an eight-step wizard behind a dev-only route,
built out of this product's own design system, so a reviewer reads real
typography, real spacing, real card and badge treatments and real copy.

```
cd ui/shared && npm run dev
open http://localhost:5173/mockup
```

The route exists only in a dev build. `App.tsx` guards it with
`import.meta.env.DEV`, which is statically false in a release, so the
branch and everything under `src/mockup` leave the shipped bundle
entirely — verified by grepping the production bundle for the mock-up's
own strings after `npm run build`.

It sits **above** the sign-in gate deliberately: the mock-up talks to
nothing, and a static design that needs a running service and an account
to look at is a design nobody reviews.

## What is mocked, and where each screen came from

| # | Screen | Route | Reuses |
|---|---|---|---|
| 1 | Repository domains | `/mockup/domains` | `PageHeader`, `.card`, `.table`, `Banner`, `StatusBadge` — the `BackupSetsPage` list shape |
| 2 | Define a repository domain | `/mockup/domain-new` | `PageHeader` with `back`, `.field`/`.input`, `PasswordInput`, the wizard's `Choice` radio cards, `WarningBanner` |
| 3 | Backup defaults (deployment) | `/mockup/defaults` | `SettingsPage`'s card stack, `.select`, `.table`, the wizard's `Toggle` |
| 4 | Add backup set — 8-step wizard | `/mockup/wizard` | `BackupSetWizardPage`'s step rail, `StepBody`, `Choice`, `Toggle`, `Field`, the card-with-footer-controls |
| 5 | Backup set configuration | `/mockup/set-config` | `BackupSetDetailPage`'s `Section` card and `Cell`/`Row` pairs, `FieldHelp`-style notes, `WarningBanner` |
| 6 | Backup set detail — incremental | `/mockup/set-incremental` | `DashboardPage`'s `MetricCard` strip, `StatusBadge`, `.card` |
| 7 | Backup set detail — artifact | `/mockup/set-artifact` | the same, drawn for the other engine |
| 8 | Snapshots | `/mockup/snapshots` | `.table`/`.table-scroll`, `StatusBadge`, `BackupsPage`'s row-actions pattern |
| 9 | Snapshot detail | `/mockup/snapshot` | `MetricCard` strip, `Row` definition pairs, `Banner` in the `ErrorState` shape |
| 10 | Restore | `/mockup/restore` | the wizard rail again, `.activity-bar` from the activity strip for live progress |
| 11 | Repository health | `/mockup/health` | `HealthSummary`'s vocabulary, `WarningBanner`, check-list rows |
| 12 | Retention and holds | `/mockup/retention` | `RetentionPolicyCard`'s chain table, `.dialog-scrim`/`.dialog`, `btn--destructive-confirm` |
| 13 | Maintenance | `/mockup/maintenance` | `.card` per domain, `Cell` grid, disabled actions with a stated reason |

Nothing new was invented at the component level. Three shapes are
*copied* into `src/mockup/parts.tsx` rather than imported, because the
originals are private functions inside a page: `BackupSetDetailPage`'s
`Cell`/`Row`, `BackupSetWizardPage`'s `StepBody`/`Choice`/`Toggle`, and
its step rail. **The production wave should promote those into
`components/` and delete the copies**, not copy them a third time.

## The wizard, step by step

Eight steps. The order is the argument:

1. **Source** — server, credentials, directory. Unchanged from today.
2. **Connection test** — reachability, host key, authentication, the
   source path listing, the **write probe** (#852) and clock. It comes
   before the engine because its answer constrains step 7.
3. **Engine** — Artifact or Incremental. The one irreversible choice, so
   it is asked once there is enough context to answer it and never
   offered again after the set has run.
4. **Repository domain** — pick one or define one. Only reachable as a
   real question for the incremental engine, which is why it cannot come
   earlier.
5. **Source consistency** — live / quiesced / frozen image.
6. **Verification and schedule** — level, sample percent, the two
   cadences, poll interval.
7. **Retention and holds** — the inherited chain, last-known-good
   protection, and the source-deletion control #852 governs.
8. **Review** — every answer as tiles, then one save.

Steps 4, 5 and 6 do not disappear for an Artifact set: they say why they
do not apply. A rail that changes length under the operator teaches
nothing; a step that explains itself teaches the difference between the
two engines at the moment it matters.

## Decisions this mock-up is asking for a verdict on

**Artifact and Incremental are told apart by a badge, not by layout.**
Same page, same cards, one accented `StatusBadge` and a different set of
metrics. An operator who runs both should not have to learn two
interfaces; an operator who runs one should never wonder which they are
looking at.

**Four byte counts, never one, and absent is not zero.** Every surface
that reports a run shows entries scanned, logical size, read from source,
written to repository and reused as five separate figures
(`backupengine.TreeSnapshotInfo`). A single "backed up" total would
report a deduplicating repository as growing by the size of the source
every night. Every one of those counters is nullable on the wire, along
with `source_complete` and `duration_seconds`, and absent means nobody
measured it: the screens print **not measured**, never `0`, because a
zero is a measurement and "reused 0 bytes" sends an operator hunting a
fault in a backup that is working. One fixture snapshot carries absent
counters so the mock-up shows that row rather than only describing it.
`src/mockup/format.ts` is the single place the rule is applied.

**A retention verdict names its tiers, not the chain.** Each verdict
carries `{tier, selected_by}` pairs and an action of KEEP, DELETE or
REFUSE; the tier's granularity and window are stated once above the
table, where the chain is, rather than repeated per row. The badges are
the product's own `RetentionTierBadges`, which already draws the
placement in brackets and draws last-known-good protection **bare** — a
parenthesised word after "Protected" would read as a placement, and
protection is not one. REFUSE is drawn neutral rather than as a third
shade of delete: the pass removed nothing, so nothing is pending.

**A restore names a snapshot and answers conflicts three ways.** The
parameters are `snapshot_id`, an optional `source_path`, `target_path`
and `conflict` of refuse / skip / overwrite, defaulting to refuse. It is
the snapshot id rather than a run id because the repository, not the
catalog, decides whether the id names anything — which is what keeps a
restore working when the journal has lost the row, and that is precisely
the situation somebody is restoring in. "Skip" is what makes an
interrupted restore resumable without choosing between starting again and
overwriting.

**Repository health speaks the backup-set health vocabulary.** `state` is
HEALTHY / DEGRADED / FAILING, the same three words and the same severity
scale a set uses, so one dashboard does not carry two. `clock_skew_seconds`
is nullable and **signed**, and the sign is the message: negative means
this machine is behind the history already stored, the direction that
dates a new snapshot before an older one, so the mock-up spells "184 s
behind the repository" rather than an unsigned magnitude.

**Asked-for and achieved verification are both shown.** ADR 0014's
achieved level is the answer, and a failed check carries no level at all.
The snapshot list badges the achieved level; the detail page shows both.

**Co-tenancy is stated where the decision is made.** The six things a
shared domain shares (`model.RepositoryBoundaries`) appear in the domain
form and in the wizard's domain step, not in a help page.

**Maintenance ownership is a first-class column.** A domain another
instance owns has its actions disabled with the reason in the card, since
"press it and find out" is how two instances end up compacting one store.

**Every mutating control is a durable operation.** Run, verify, restore,
hold, release and maintenance all submit one operation with an
idempotency key and are then watched, which is what keeps CLI and Web
parity honest: the CLI submits the same operations.

## #852: the write probe and the refusal it arms

New requirement, folded into this gate because it changes two screens.

The connection test reports a **write permission** line: Backupd creates
a scratch file under the source path and deletes it again. That result,
and nothing softer, arms *Delete from source after a verified backup*
(`read_only = false`):

- **writable** — the control is enabled, with `sets.source-delete`
  explaining what it does;
- **read-only** — the control is disabled and unchecked, its note reads
  "Unavailable: these credentials cannot write to the source", and the
  registry tooltip `sets.source-delete.read-only` says why and what to
  do: grant write permission on the source path and re-run the test.

Both states are in the mock-up on both surfaces (wizard step 7 and the
per-set configuration page), reachable through a labelled mock-up control
so a reviewer can see each without a second build. The safety rule stated
plainly: **never offer to delete from a source we cannot prove we can
write to.**

The api/v1 half of this is **already implemented** on the branch this
mock-up is a gate for (#852, merged into `feat/788-api`), so the UI wave
consumes it rather than designing around a gap:

- `ConnectionCheck` carries a seventh step, `write_probe`, and `writable`
  is a **required** boolean on `TestConnectionResponse`. Absent is read as
  **false** — the control at the other end deletes a producer's files, so
  the missing answer is the refusing one.
- Setting `read_only = false` against a source the probe proved
  non-writable is **refused, never coerced**: `409
  BACKUP_SET_SOURCE_NOT_WRITABLE` (`service.ErrSourceNotWritable`), on
  create, on first-run create, on a connection-changing edit, and on the
  CLI's own read-only verb, all through one shared write path.

The UI therefore reads `ConnectionTestOutcome.writable` to arm or refuse
the control, and has to render that 409 as a refusal an operator can act
on rather than as an unexpected failure — the two states this mock-up
draws are exactly the two the service already enforces.

## Vocabulary

Operator-facing words carry the api/v1 field beside them in monospace, so
a reviewer can check the words and the contract in one pass. The contract
is #788's own (`engine`, `repository_domain`, `source_consistency`,
`verification_level`, `verification_level_achieved`, `entries_scanned`,
`logical_bytes`, `source_bytes_read`, `repository_bytes_written`,
`content_reused_bytes`, `source_complete`, `last_known_good`, snapshot
holds, the snapshot-retention verdicts, the `/repositories` health
resource and `/repositories/{domain}/maintenance`).

Every mutating control on these screens is one `POST /operations` with an
idempotency key and a single nested parameter object — `run_backup_set`,
`restore_snapshot`, `verify_snapshot`, `hold_snapshot`,
`release_snapshot_hold` — followed by polling that operation. There is no
new mutating route, which is what makes CLI and Web parity a property of
the contract rather than a thing to remember.

**Those annotations ship.** They began as a review aid, and the verdict
on them is that they earn a place in the product for a second reason:
these are the screens an operator compares with `backupd snapshot list`
output and quotes at a support engineer, and `source_bytes_read` is the
word both of those conversations use. `WireField`
(`ui/shared/src/components/Definitions.tsx`) is the one component that
draws them, quiet enough to read past, and it is on the production
screens — the snapshot list and inspector, the per-set configuration
card, repository maintenance, the wizard's engine step and the
define-a-domain form. What does NOT ship is a vendor word as a label:
`kopia` appears only inside an annotation, where it is the contract's
spelling, and never as the name of anything an operator is asked to
choose.

No vendor jargon reaches an operator: "repository domain", "snapshot",
"restore point", "verification level" and "maintenance" are this
product's own nouns. The engine's configured value is `kopia` and it
appears only in the monospace annotations, where it is the contract's
word rather than the interface's.

## Placeholder data

One coherent fictional deployment (`src/mockup/data.ts`): three
repository domains — a shared local one, a shared off-site one that is
overdue for maintenance, and an isolated one owned by another instance —
four backup sets, three incremental and one artifact, and five snapshots
including one that failed verification against a declared frozen-image
source and two under holds. It is one site rather than per-screen samples
because the workflow is what is being reviewed: the domain chosen in the
wizard is the domain the snapshot list attributes a snapshot to.

## Status

This was the design gate, and the production UI wave has landed against
it: `src/mockup`, the `/mockup` branch in `App.tsx` and the mock-up's
own registry tooltip entries are gone, except the entries the real
screens adopted — `sets.source-delete`, `sets.source-delete.read-only`,
`snapshots.reused`, `snapshots.achieved-level`,
`repositories.maintenance-owner` and the `wizard.incremental.*` set.
The three shapes the mock-up copied rather than imported were promoted
instead of copied a third time: `StepBody`, `StepRail` and
`StepControls` live in `ui/shared/src/components/WizardStep.tsx`, and
`Cell`/`Row`/`Note`/`WireField` in
`ui/shared/src/components/Definitions.tsx`.

This document is what survives, and it is a record of the decisions
rather than of the current build: read it for why a screen is shaped the
way it is, never as a statement of what ships today.

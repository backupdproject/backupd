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

**Four byte counts, never one.** Every surface that reports a run shows
entries scanned, logical size, read from source, written to repository
and reused, as five separate figures (`backupengine.TreeSnapshotInfo`).
A single "backed up" total would report a deduplicating repository as
growing by the size of the source every night. Where the engine could not
account for reuse the screen says **not measured**; it never draws a
zero, because a zero is a claim that nothing deduplicated.

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

The api/v1 delta this implies — a writable step on the connection-test
result, and refusing `read_only = false` for a source that failed the
probe — belongs to whoever owns #852, not to #788's API work.

## Vocabulary

Operator-facing words carry the api/v1 field beside them in monospace, so
a reviewer can check the words and the contract in one pass. The contract
is #788's own (`engine`, `repository_domain`, `source_consistency`,
`verification_level`, `verification_level_achieved`, `entries_scanned`,
`logical_bytes`, `source_bytes_read`, `repository_bytes_written`,
`content_reused_bytes`, `last_known_good`, snapshot holds, the
`/repositories` health resource and `/repositories/{domain}/maintenance`).
Those annotations are a review aid and go with the mock-up.

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

This is a design artifact, not the shipped UI. When the production UI
wave lands, `src/mockup`, the `/mockup` branch in `App.tsx` and the
mock-up's registry tooltip entries go with it — except the entries the
real screens adopt, which is the intended path for
`sets.source-delete`, `sets.source-delete.read-only`,
`snapshots.reused`, `snapshots.achieved-level`,
`repositories.maintenance-owner` and the `wizard.incremental.*` set.
This document is what survives.

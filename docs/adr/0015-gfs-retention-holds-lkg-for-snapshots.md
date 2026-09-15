# ADR 0015: GFS retention, holds and last-known-good for snapshots

- Status: accepted
- Date: 2026-09-12
- Supersedes: nothing. Extends ADR 0013 (the snapshot lifecycle, catalog
  and crash reconciliation), whose "no retention, no pruning" consequence
  this closes, and ADR 0011 (the repository adapter), whose
  `DeleteSnapshot` primitive this becomes the only caller of.

## Context

The incremental engine produces one snapshot per run of a backup set, and
until now nothing ever removed one. A snapshot catalog that only grows is
a catalog that eventually fills a repository, so something has to decide
which restore points stop being worth keeping.

This product already has that decision, written twice over for artifacts:
FR-18's GFS chain (`core/internal/retention/gfs.go`), FR-19's
last-known-good protection (`lastknowngood.go`), and FR-20's delete safety
with its mandatory dry-run (`prune.go`). Those are not simple. They carry
calendar-bucket arithmetic that survives DST and leap years, a tie-break
that makes two runs over the same history render identically, a
distinction between the timestamp this manager observed and the one a
producer claimed, and a set of refusals that exist because each of them
was once a way to delete the wrong thing.

The engine also has an opinion. It ships snapshot-removal primitives, a
policy language of its own, and — the part that mattered most here — a
default retention policy that its uploader applies without being asked.

So the question this ADR answers is not "how should snapshots be retained".
It is "whose retention runs, and how is everything else prevented from
running".

## Decision

### 1. One retention engine, reached through a projection

`core/internal/snapshotretention` does not implement GFS. It projects the
catalog's snapshot runs onto `state.Record`, the shape
`internal/retention` already classifies, and calls `retention.DecideKeep`.

The alternative — a second GFS implementation for snapshots, with a test
asserting it agrees with the artifact one — was rejected for the reason
every duplicated policy is eventually rejected: the two agree on the day
the test is written, and then a tier granularity, a DST edge or a
tie-break is fixed in one of them. A projection has exactly one
implementation to be correct.

The projection is narrow, and each choice in it is a claim:

- **Only a run at SUCCESS carrying a manifest, with no failed
  verification, is in scope.** That is the snapshot equivalent of FR-18's
  "managed, completed backups": an artifact that is still in flight, that
  failed, or that was quarantined never receives a GFS verdict either.
- **A snapshot is bucketed by its run's own start time, and there is no
  producer placement.** FR-18 admits a second, untrusted timestamp because
  an artifact is a file somebody else wrote. This manager took the
  snapshot, off its own clock; there is no third party to admit.
- **A manifest id this product cannot address is a refusal, not an
  omission.** A plan that silently fails to mention a snapshot in the
  repository is a plan whose gaps are invisible to the operator confirming
  it.

### 2. A hold is a durable row, composed as a protection term

`snapshot_holds` (migration 0011) records one person's decision that one
snapshot must not be deleted: who, why, when, and — separately, never as a
DELETE — who released it and when. Several holds may sit on one snapshot,
because a legal hold and an operator's "not until the migration is signed
off" are two decisions and releasing one must not release the other.

A hold is composed into the classifier's verdicts the way FR-19's
protected term is (`ApplyLastKnownGood`): the snapshot's verdict gains a
`HOLD` tier attributed to `PROTECTION`, never to a calendar placement.
Filtering held snapshots out of the delete list afterwards would have been
less code and would have produced a preview that could only say "this was
not deleted", where this one says which hold, placed by whom, and why.

Holds are refused where they would protect nothing: a run with no
committed manifest, a run whose snapshot has already been deleted, a run
whose delete intent is already durable (the manifest is on its way out and
the row still says SUCCESS for a few seconds longer), and a run at LOST,
whose manifest is not in the repository at all. Replaying a hold id whose
hold has since been *released* is refused too, with its own error: handing
the released row back with no error tells a caller that checks the error
rather than `Active()` that its snapshot is protected, when the protection
was deliberately ended. A hold that is accepted and protects nothing is
worse than no hold, because somebody reads the list and stops worrying.

### 3. Last-known-good is protected twice, from two different facts

The classifier computes FR-19's protection over the projected timeline.
The catalog independently carries a durable `last_known_good` flag that
moves only when a run reaches SUCCESS or when reconciliation re-points it
(ADR 0013). The delete path refuses a snapshot protected by *either*, and
treats a disagreement between them as a contradiction that refuses rather
than as a tie to break — unless the operator has explicitly set
`protect_last_known_good: false`. That false says out loud that retention
may empty this backup set, and since the catalog's flag is set on every
successful run, honouring it anyway would refuse the newest snapshot for
ever and silently stop a documented configuration from working. The
set-wide restore-point guard and the per-snapshot refusal read the same
resolved setting.

This is what makes the §39 regression structural rather than incidental: A
is verified, B completes and its verification fails, and B never enters
the timeline at all because a failed run is not a completed backup. A
retention engine that ranked "the newest run with a manifest" would
protect B — whose manifest is real and in the repository — leave A outside
every window, and delete the only snapshot anybody could restore from.

The set-wide guard from FR-30 is ported with it: if the restore point a
set advertises cannot be resolved in the repository, the whole pass is
refused, not the one snapshot it is about. Half a pass carried out removes
exactly the snapshots that were still restorable.

### 4. The engine cannot expire a snapshot retnd wrote: every manifest is pinned

The vendor's uploader checkpoints a long upload every 45 minutes, and
every checkpoint ends by applying the repository's effective retention
policy — listing the source's snapshots and deleting the manifests the
policy no longer keeps. The default policy keeps the latest 10, 7 daily, 4
weekly, 24 monthly and 3 annual. On any repository holding more than that,
a backup lasting longer than the checkpoint interval quietly expired older
snapshots: not in the catalog, not in a plan anybody confirmed, and not
subject to a hold.

The guarantee is a **pin**. Every manifest
`core/internal/backupengine/kopia` saves — the filesystem path, the
streaming path and the tree path — carries the pin `backupd`, and the
vendor's expiry keeps any pinned manifest whatever the policy says
(`snapshot/policy/expire.go`). The vendor's `DeleteManifest`, which is
what this adapter's `DeleteSnapshot` calls, ignores pins entirely. That
asymmetry is exactly the arrangement this product needs: the engine may
never expire a snapshot retnd wrote, and retnd may still delete one
when its own retention pass decides to.

A neutral **global** policy is stored as well, at open, and it is defence
in depth rather than the guarantee. What a checkpoint reads is the
*effective* policy, not the global one: a host, `user@host` or path policy
overrides it and a `NoParent` policy discards it. One `kopia policy set
--host nas-1 --keep-latest 1` from somebody's own kopia install against a
shared bucket leaves the open-time check reading "already neutral" while
the next long upload expires everything older than the newest snapshot.
The correction is spelled as six explicit zeros — the vendor's own
spelling for "no count applies": `EffectiveKeepLatest` reads that as
`MaxInt` — and it changes the six counts only, leaving the rest of the
retention section (`IgnoreIdenticalSnapshots`) and every other policy
section as the operator left them.

The correction is **best-effort and never refuses the open**. Opening is
also how a restore, a verification drill and reconciliation reach a
repository, and storage that will not accept a write is an ordinary
disaster-recovery posture: a WORM bucket, an object lock, a read-only
mount, a legal hold. Refusing there would turn "this product cannot
correct a policy" into "this product cannot restore". The failure is
carried on the open handle and reported by `Health` as an
`engine_retention` warning, which is the honest statement of what is left
at risk: nothing retnd wrote, and any snapshot in that repository
written by something else.

### 5. The delete path: decide, record the intent, then remove one manifest

`Decide` computes and mutates nothing; `Apply` calls the same code and
then, immediately before each irreversible call, re-derives every
condition against freshly read rows — the run's phase and manifest, the
lineage's holds, and the set's restore point. A hold placed while a long
pass is running stops the deletes that have not happened yet.

The two durable writes are ordered, and the order is the crash story. The
intent (`delete_requested_at`) is written before the repository is asked,
so a run with an intent whose manifest is gone reads as a delete that
completed, and a run with no intent whose manifest is gone reads as a loss
to investigate. Reversed, this product would raise an incident about a
deletion it performed on purpose. A catalog that cannot record the intent
therefore stops the delete before the repository is asked at all.

An intent a later pass decides against is **withdrawn**. A run this pass
KEEPS whose row still carries the intent of an earlier one — a delete the
repository refused, a window an operator widened, a hold somebody placed —
has that intent cleared, because an intent is read as a standing statement
and reconciliation would otherwise report a delete this product no longer
means to perform, every cycle, indistinguishably from one it does. A
REFUSE never withdraws it: there the delete did not happen *this time* and
the intent still describes what this product means to do.

### 6. Retention removes manifests; it cannot reach storage

The port retention holds is two methods wide — look one manifest up,
remove one manifest — and there is no maintenance, stats, blob, pack or
index surface on it. Reclaiming the storage behind an unreferenced
manifest is repository maintenance (issue #786), separately, and the
guarantee is structural rather than a comment: a test pins the port's
method set, a second test refuses storage vocabulary anywhere in the
package's identifiers, and a third runs a real pass against a real
repository and asserts that every blob file present before it is present
after it.

## Consequences

Deleting a snapshot frees nothing on its own, and every surface that
reports a retention pass has to say so rather than imply a number of bytes
was recovered. That is not a limitation of this design; it is what
deduplication means.

A snapshot older than the history window a pass reads (1024 runs by
default) is invisible to it and is therefore never deleted. That is the
fail-safe direction — more retention, not less — and a deployment that
wants those reached raises the limit.

A failed run's manifest is never deleted by retention, because retention
never classified it. What becomes of it is a reconciliation question, and
answering it by deleting is the one answer that cannot be taken back.

Nothing here is wired to a CLI, an API or the UI: that is issue #788, and
it consumes `Decide` for the preview and `Apply` for the pass. Until it
lands, the only callers are tests.

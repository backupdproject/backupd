# ADR 0017: Safe repository maintenance

- Status: accepted
- Date: 2026-09-12
- Supersedes: nothing. Completes ADR 0015 (GFS retention, holds and
  last-known-good for snapshots), whose "deleting a snapshot frees nothing
  on its own" consequence this is the other half of, and extends ADR 0011
  (the repository adapter), whose `Maintain` primitive this becomes the
  only production caller of.

## Context

Retention deletes a manifest. That is all it does, deliberately: the
content behind the manifest stays in the repository's packs until the
engine's own maintenance decides nothing references it any more. So after
issue #785 landed, this product could decide a backup was no longer worth
keeping and could not recover one byte of the storage it occupied.

The engine has maintenance, in two modes, and it is not a function that
can simply be called on a timer. Three properties of it decide everything
in this ADR:

1. **It rewrites indexes and deletes content.** Two processes doing that
   concurrently against one repository is how a repository loses content
   it still references. The engine's own answer is an owner string written
   into the repository and checked before maintenance will run — and this
   product's adapter has always overridden that check (`force=true`),
   because deferring to whichever machine created the repository means a
   maintenance window that silently never runs.

2. **Its safety comes from time, not from locking.** `SafetyFull` refuses
   to collect content younger than 24 hours, requires two garbage
   collections four hours apart to agree before deleted contents leave the
   index, and leaves an unreferenced pack blob alone for a day. Those
   margins are what make maintenance safe beside a snapshot that is still
   being written, and the engine offers a `SafetyNone` that removes all of
   them.

3. **It is repository-scoped.** Maintenance is about a Repository Domain,
   not a backup set. Several backup sets can share one domain, and one
   deployment can hold several independent domains.

Issue #786 is therefore not "call Maintain on a schedule". It is: who is
allowed to call it, what must not be running when they do, and what is
never allowed to be traded away for speed.

## Decision

### 1. Maintenance is a Repository Domain operation with exactly one owner

`core/internal/repomaintenance` runs maintenance against a domain, never
against a backup set. Ownership lives in the durable record #781 added
(`backupengine.MaintenanceOwnership`, one JSON file per domain beside the
repository's other local state), and the rules are three:

- an unclaimed repository is claimed by the instance that first maintains
  it, because "no record" is the one unambiguous state — nobody has ever
  maintained this repository, so there is no owner to displace;
- a repository claimed by another instance is refused (`ErrNotOwner`), and
  the refusal happens before the repository is touched at all, not even to
  read its size;
- ownership moves only through `Transfer`, a package-level function that
  takes the owner the administrator believes holds it and refuses if the
  record says otherwise. It is not a method on the thing that wants the
  repository, because "claim this for me" is the operation this design
  exists to make impossible.

An unreadable record is never read as an unclaimed one. The answer to
unclaimed is "claim it", and claiming a repository another instance is
maintaining right now is the exact failure the record prevents.

### 2. Fencing is per-domain, and full maintenance is the exclusive side

`repomaintenance.Fence` is a per-domain readers/writer gate with a FIFO
queue. Full maintenance takes the exclusive side; a destructive snapshot
operation — a manifest delete, which is the only thing in this product
that removes anything from a repository — takes the shared side; quick
maintenance takes the shared side too, so it cannot run during a full
window but does not block a retention pass.

The unit is the domain and never the process, because "operations on
independent repositories may run concurrently" is half the requirement and
a single mutex would break it in a way that presents as a deployment whose
four nightly maintenance windows have quietly become serial.

A retention pass is fenced by wrapping rather than by being told about
maintenance: `Fence.GuardSnapshots` returns exactly the two-method port
`snapshotretention.Pruner` takes. The dependency points this way round
because retention decides what to delete and knows nothing about
maintenance windows, and a fence it had to be told about is a fence
somebody forgets.

`LookupSnapshot` is deliberately not fenced. It reads, so it cannot be
half of a destructive race, and fencing it would put a retention pass's
whole planning phase behind a maintenance window for no safety.

This is an in-process coordination primitive and does not pretend to be a
distributed lock. What makes even an unfenced concurrent pass
non-destructive is the vendor's safety parameters, which is the next
decision.

### 3. The vendor's safety parameters are not a parameter of this product

The adapter passes `maintenance.SafetyFull`, and nothing above it can
choose otherwise: the mode is `backupengine.MaintenanceMode`, which has
two values and no safety field, and the maintenance package cannot import
the engine at all.

That is a decision with a measurable cost — reclamation takes days rather
than minutes — and it is not negotiable for production automation.
`SafetyNone` exists, is faster, and trades away the only property that
matters: the guarantee that a garbage collection running while a snapshot
is being written cannot collect the content that snapshot is about to
reference.

Two tests hold the line, and both matter. One parses this package's
sources and requires the safety argument at the vendor's maintenance entry
point to be spelled `maintenance.SafetyFull` — a test that merely grepped
for the absence of `SafetyNone` would pass against a locally-built
`SafetyParameters{}` with every margin at zero. The other asserts that the
vendor's own `SafetyFull` still HAS margins in it, which is the assertion
a future version bump breaks: a `SafetyFull` that had been redefined to
zero would leave every other test in the suite green while this product
ran unsafe maintenance under a safe-sounding name.

### 4. The maintenance port cannot reach a manifest

`repomaintenance.Repository` is two methods: `Maintain` and `Stats`. There
is no `DeleteSnapshot`, no snapshot listing and nothing that names a
manifest, because a maintenance window that could delete a snapshot would
be a second retention policy — one with no catalog, no holds and no
last-known-good protection behind it.

It is the mirror of the port ADR 0015 carved for retention, which cannot
reach storage. Both are pinned by tests, in both packages, so widening
either fails a build rather than a review.

### 5. Failure is recorded, alerted, and changes nothing else

Every window appends to the record: the mode, whether the engine did any
work, what the storage measured either side of it, and how it ended. The
history is bounded (32 windows) and the counters that a bounded history
cannot answer — total runs, total failures, total bytes reclaimed — are
durable fields beside it.

A failed window records the failure, backs the repository off so a broken
NAS is not retried every cycle, and raises a condition through the
existing alerting model. That model's vocabulary was four conditions and
is now five: `alert.MaintenanceFailed`, scoped to the repository domain
rather than to a backup set. It earns the fifth slot on the terms
`internal/alert`'s own doc sets — it is true while maintenance keeps
failing, it resolves when a window succeeds, and it needs a human, because
maintenance is the only thing that reclaims storage and the condition that
would otherwise eventually fire (critical storage pressure) is about a
different filesystem and arrives far too late.

The alert says, in words, that nothing was lost. The obvious reading of
"maintenance failed" is that the repository is damaged, and the reflexes
that follow from that reading are worse than the fault.

## Alternatives considered

**Let the engine own maintenance scheduling.** It has a schedule, an
owner and an interval of its own, and using them would have been less
code. It was rejected for the reason the adapter already overrides the
owner check: the recorded owner is whichever machine created the
repository, and this product cannot tell an owner that is deliberately
elsewhere from one that no longer exists. A maintenance window that never
runs produces no error at all, only a repository that grows.

**Weaken safety in tests so reclamation is observable quickly.** The
reclamation test needed to see content actually leave the disk, and every
margin that delays it is measured in days. Reducing safety in the test
would have proved that a configuration this product must never ship does
reclaim space. So the test moves time instead: the repository's clock and
the timestamps its storage stamps on blobs are moved together, because the
engine compares the two and refuses to run when they disagree — correctly,
since that comparison is how it detects the clock skew that would let one
process collect another's content.

**A global maintenance lock.** Simpler than a per-domain gate, and it
would have satisfied the fencing requirement while breaking the
concurrency one.

## Consequences

Reclamation is not immediate and cannot be made immediate. A snapshot
deleted today frees storage after several daily maintenance windows —
four, in the fixture that measures it — because that is how long the
engine's safety margins take to converge. Every surface that reports a
retention pass has to say so.

A repository whose maintenance owner is another instance is never
maintained by this one, even if that instance is gone. Recovering from
that is an administrative action (`Transfer`) and not an inference, which
is the trade this ADR makes deliberately: the alternative is an instance
that decides for itself that somebody else looks inactive.

Quick maintenance does not measure what it reclaimed. `Stats` walks the
storage's blob listing, which on a bucket is a real number of requests,
and a quick window reclaims no blobs; paying for two listings an hour to
report a number that is always zero is a cost with no reader.

Nothing here is wired to a CLI, an API, the UI or a Prometheus scrape:
that is issue #788, and what it consumes is `Runner.RunDue` for the
window, `Measure` for the numbers and `AlertConditions` for the
notification. Until it lands, the only callers are tests.

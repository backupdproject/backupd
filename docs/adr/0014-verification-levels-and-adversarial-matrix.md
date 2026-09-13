# ADR 0014: Verification levels and the adversarial matrix

- Status: accepted
- Date: 2026-09-12
- Supersedes: nothing. Extends ADR 0006 (embed Kopia behind a
  `backupengine` adapter), ADR 0011 (the repository adapter) and ADR 0013
  (the snapshot lifecycle, catalog and crash reconciliation), whose
  "verification depth is not yet selectable" consequence this closes.

## Context

ADR 0013 gave a snapshot run a state machine, a catalog and a
verification phase. What it could not give it was a verification that
meant anything specific: the repository port offered exactly one
verification, it read every byte, and so a run recorded `content_full`
as *achieved* no matter what its set was configured for. Two deployments
with opposite intentions — one that wanted a cheap nightly structural
check, one that wanted a nightly restore drill — produced identical rows.

The claim being protected is the one EPIC K is named after. **Structural
verification is not a restore test.** An engine can prove that every
content identifier a snapshot references resolves and that every hash in
its index matches, and none of that proves a restore produces the tree an
operator expects: the pack blob holding the content can be gone from the
bucket, the bytes in it can have rotted, the restore path itself can
write placeholder stubs and report plausible statistics. A product that
reports "verified" without saying which of those it did is making the
promise it has not tested.

Nor is a level ladder worth anything unless the adversarial cases are
actually exercised. A verification that has never been shown a damaged
repository is a function that returns nil.

## Decision

### 1. The level is a parameter, and the ACHIEVED level is the answer

`backupengine.Repository.Verify` takes a `VerifyRequest{Level,
SamplePercent, RestoreTarget}` and returns a `VerifyReport` whose `Level`
is what the verification **actually performed**. The caller never assumes
the two are equal, and an adapter that cannot do what was asked returns
an error rather than a shallower level with a nil error. A failed
verification reports no level at all: it proved nothing, and the catalog
row's achieved column is explicitly written back to empty so a failure
cannot inherit the claim of the run before it.

The four rungs, over one walk of the snapshot tree:

| Level | What it does | What it finds |
|---|---|---|
| `structural` | walks the manifest's tree, resolves every content identifier through the index | a manifest or tree that does not resolve |
| `content_sample` | the above, plus the existence of every backing pack blob, plus a deterministic **count** of file content read, decrypted, decompressed and hash-checked | content the repository references and no longer has, without reading the whole tree |
| `content_full` | the above with every file read | damage inside a blob that is still present |
| `restore_drill` | restores the snapshot to a real directory through the real restore path, hashes every restored file against the repository's own bytes, and refuses anything under the target the snapshot did not name | a restore path that truncates, stubs, misplaces or invents files |

Four of those decisions are worth their sentences:

- **The sample is a count, not a coin flip and not a stride.** The
  vendor's verifier samples with `100*rand.Float64() < percent`, so two
  runs of one configuration read different amounts and, on a small tree,
  one of them reads nothing while the catalog still records
  `content_sample`. A stride of `ceil(100/percent)` fixes the randomness
  and introduces a worse problem: it answers 51% and 99% identically —
  every second file — so a deployment that raises its sampling because an
  audit asked for it gets the check it already had, and no row says so.
  The selection is therefore exactly `ceil(files*percent/100)` of the
  files the walk finds, always including the first, so a row that says a
  51% sample happened means 51% of the files were read.
- **Sampling is a percentage of files, not of bytes.** Damage arrives per
  file; a byte-proportional sample spends its whole budget inside the
  largest file and never looks at the others.
- **The blob-existence check belongs to the sampled rung, not the
  structural one.** It is what finds a pack blob deleted from a bucket
  without reading a single file, and it costs one existence check per
  distinct pack the snapshot references — real storage traffic, which the
  rung that runs on every single backup must not pay. It is deliberately
  NOT a listing of the repository: heap and request count belong to the
  snapshot being verified, not to the history of every set sharing the
  domain.
- **A rung that read nothing reports `structural`.** A tree of
  directories with no files in it passes every rung vacuously — the
  content read has nothing to read, the drill has nothing to restore — and
  a row claiming `restore_drill` over zero restored files would hand
  last-known-good to a check that touched no byte. The achieved level is
  what was performed, so it degrades to the rung that really was.

### 2. The success policy

- **Structural is the floor of every rung**, so it happens on every run
  by construction rather than as a separate pass.
- **The set's configured level is what a run must prove.** A run that
  proves less **fails**. This is the whole of "last-known-good requires
  the configured level": only a SUCCESS row carries the flag, and a run
  cannot reach SUCCESS on a check shallower than its set asked for.
- **A cadence may raise the bar for one run and may never lower it.**
  `verification_full_every` and `verification_restore_drill_every`
  escalate a run to a deeper rung; the configured level remains the
  floor.
- **The cadence is measured from what was PROVEN, not from a counter.** A
  run-counting cadence marks the deep check done on the *attempt*, so a
  deployment whose weekly full read keeps failing silently stops having
  one. Reading the newest run whose *achieved* level satisfies the rung
  means a failing deep check stays due until one succeeds.
- **A configured drill with nowhere to restore to is refused before the
  run's first durable write**; a drill *cadence* with nowhere to restore
  to is silently dropped. The asymmetry is deliberate: a configured level
  is an operator's instruction, and a cadence is this product's own idea
  about when to look harder — an idea of ours must never fail somebody's
  backup.
- **A passing drill's restored trees are deleted; a failing drill's are
  kept.** The first is a second copy of somebody's data on a disk they
  pay for; the second is the only available evidence of what a restore of
  this snapshot actually produces.
- **Every drill attempt gets its own fresh directory.** A drill restores
  with `Overwrite=false` — a verification that can overwrite is a
  verification that can destroy data — so a run that died mid-restore left
  partial files, and a recovery pass restoring into the same
  `restore-drill-<runID>` failed on the first file that was already
  there: an intact committed snapshot moved to FAILED over the wreckage
  of the attempt that crashed. Each attempt therefore allocates
  `restore-drill-<runID>.attempt-N` beside the old one, the old one is
  left alone while nothing is proven, and once the snapshot IS proven
  every attempt directory for that run is removed together.
- **A recovery pass proves the level the row was admitted under**, with
  no cadence escalation: re-verifying at today's configured level would
  fail a good snapshot whenever somebody raised a set's level between the
  crash and the recovery, and escalating would turn one interrupted run
  into a full restore during the recovery of a deployment that has just
  come back up.

### 3. Where the drill's restore lives

The lifecycle package's repository port stays four calls
(`SnapshotTree`, `Verify`, `LookupSnapshot`, `ListSnapshots`) — it still
cannot delete a snapshot or restore to a path of its own choosing. The
drill happens *inside* `Verify`, against a directory the caller names,
so the top rung costs the port no new capability. The directory is
inside the reserved namespace under the backup root
(`backupengine.ReservedLocalStateDir`), because a restored tree written
anywhere artifact discovery walks would be presented to the pruner as a
few thousand unrecognised artifacts.

### 4. The adversarial matrix

This ADR's issue is the test deliverable, so the matrix is listed here as
the thing that must keep existing:

| Row | Where |
|---|---|
| four levels reporting independently | `internal/backupengine/kopia/verify_test.go` |
| corruption fixtures: a deleted pack blob (structural passes, sampled fails naming the blob) and a corrupted pack blob (structural and blob checks pass, full read fails) | same |
| restore-hash validation against the ORIGINAL source tree | same, and `tests/verificationmatrix` |
| path traversal and symlink escape, end to end through the real engine and the real restore path, plus a fuzz target over entry names | `internal/backupengine/kopia/adversarial_test.go` |
| source-mutation determinism at the engine boundary | same |
| the crash matrix over every state-machine boundary, walked from the phase table and `VerdictKinds()` rather than from a list | `internal/snapshotlifecycle/crashmatrix_test.go` |
| the success policy: configured floor, cadence, drill evidence, per-attempt drill directories, recovery level | `internal/snapshotlifecycle/verification_test.go` |
| the levels, reuse, damage and a crash DURING a drill's restore, proved through the real driver on the real catalog against a real repository | `tests/verificationmatrix` |
| the ladder, reuse and a deleted pack object over a real S3 API, on an ephemeral MinIO container the CI job is required to bring up | `tests/miniointegration/kopiaverification_test.go` |
| a large flat namespace within a bound, and a sampled verification's heap bounded by the snapshot rather than by the repository | `internal/backupengine/kopia/largenamespace_test.go`, `verify_test.go` |

One production refusal came out of writing them: an entry name
containing a NUL byte was accepted, stored perfectly, and then could not
be restored by any filesystem — a restore point that cannot exist. It is
refused at the same place traversal names are (`checkTreeEntry`), which
is the only point at which refusing it costs nothing.

## Consequences

- A deployment that configures `verification_level: restore_drill` gets
  a real restore of its source on every run, and needs the disk for it.
  That is the cost of the strongest evidence, and it is why the default
  is still `structural` and why the cadences default to never.
- A set whose engine proves less than it was configured for now FAILS
  where it previously succeeded with an overstated row. That is the
  intended direction, and it is visible: the reason names both levels.
- `content_sample` is the rung most deployments should run nightly, and
  its cost is one existence check per pack the snapshot references plus a
  stated fraction of the tree read back. Both are proportional to the
  SNAPSHOT: a set verifying a small tree in a domain holding years of
  other sets' history pays for its own tree and nothing else, in requests
  and in memory.
- Verification reads no cache by design (ADR 0011's `connectOptions`), so
  every rung above structural pays real storage traffic. That is what
  makes the corruption fixtures meaningful and it is not free.
- The drill compares restored bytes against the repository's own bytes.
  It therefore proves the restore PATH, not that the repository matches
  the original source; only the test suites, which still hold the source
  tree, can check that last step, and `tests/verificationmatrix` does.
- Retention, holds and the last-known-good POLICY engine remain #785's.
  This ADR fixes what "last-known-good" is allowed to mean; it does not
  decide which restore points are kept.

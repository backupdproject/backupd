# ADR 0016: Restoring a snapshot to a local destination, and why the snapshot is treated as hostile input

## Status

Accepted as K8 (#787) of EPIC K Phase 2. Implemented in
`core/internal/backupengine/engine.go` (the restore request, report and
conflict policy), `core/internal/backupengine/kopia/localrestore.go` (the
extraction), `core/internal/app/snapshotrestore.go` (which restore point,
which repository) and `core/service/snapshotrestore.go` (the durable
operation). Proved by `core/internal/backupengine/kopia`'s restore and
escape suites — including a fuzz target that plants hostile entry names in
a real repository — by `core/internal/app`'s end-to-end restore over a real
Kopia repository, and by `core/service`'s operation tests.

It stands on ADR 0006 (one adapter package imports the engine), ADR 0011
(where a repository lives and how it is unlocked), ADR 0013 (the catalog
that knows which restore point is the good one) and #782/#784's entry-name
rule, which this reuses rather than reimplements.

## Context

Everything in EPIC K up to this point writes. A repository that cannot be
read back is not a backup, and the only evidence that it can is a restore
that puts real bytes on a real disk. Four decisions had to be made before
that could be a product feature rather than a test helper.

1. **Which destinations.** An operator restoring in an incident wants the
   files on the machine in front of them. Streaming a restore back to the
   remote the data came from is a different piece of work — a write against
   somebody else's endpoint, with its own resume, partial-write and
   permission story — and the adversarial review of Phase 2 flagged it as a
   material scope increase for v1.

2. **Which granularity.** "Restore the snapshot" is the rarest of the three
   requests. The common ones are "this directory" and "this one file
   somebody deleted", and a product that can only unpack a whole restore
   point makes an operator find a terabyte of scratch space to get a
   50 KB file back.

3. **What a stored entry name is.** The vendor's restore joins each entry's
   name onto the target and writes it. That is safe if every name in the
   repository was written by a program that validated it. This product
   cannot assume that: a repository domain may be shared
   (`model.RepositoryDomain.MayShare`), an operator may have run the
   vendor's own CLI against the same bucket, and the names come off
   somebody else's filesystem, where whoever can create a file chooses what
   it is called. One stored `../../etc/cron.d/x` is a write outside the
   destination every time anybody restores it.

4. **What a restore that stopped halfway leaves behind.** A restore is long
   and interruptible: a process shutdown, a cancelled request, a full disk.
   The failure that matters is not the stopping — it is a destination that
   looks finished, or a file present under its real name with half its
   bytes, because the next thing to read it is a person or a program that
   believes a completed restore.

## Decision

### Local only, and the absence is stated in the port

`backupengine.RestoreRequest` has a `TargetPath` and no other destination.
Remote restore is not a field left unset; it is documented in that type as
separate work. A flag that looked like a mode would invite somebody to
implement it as one.

### One path field selects the granularity

`RestoreRequest.SourcePath` is a slash-separated path inside the snapshot.
Empty restores the whole thing, a directory restores that subtree under the
destination, and a file restores that file *inside* the destination keeping
its own name. One field rather than a mode plus a path, because the three
requests are one question — what part of the restore point do you want —
and the answer to it is a path.

### The conflict policy is a named value whose zero is the safe one

`RestoreConflict` is `refuse` (what silence means), `skip` or `overwrite`.
A bool would have made the destructive choice reachable by leaving a field
unset. An unrecognised string is refused rather than guessed
(`ParseRestoreConflict`), because guessing which of three a misspelling
meant is how "skip" becomes "overwrite" on somebody's data — and it is
refused at the API boundary, before a durable operation row exists for work
that could never run.

### The extraction is ours, and every entry name goes through the writer's own rule

`restore.Entry` and `FilesystemOutput` are no longer used. The walk is this
repository's own code, and each stored entry name must pass
`checkEntryName` — literally the function the snapshot write path refuses
names with, extracted from `checkTreeEntry` for this and shared by both
ends. One rule rather than two, because a restore applying a different rule
from the writer would either refuse names this product stores on purpose or
accept names it refuses to store, and both are discovered on the day
somebody is restoring.

The rule is #782's: an entry name is one path element. Empty, `.`, `..`, a
path separator or a NUL byte are refused. What is deliberately **not**
refused is a name that merely looks like a path — a backslash, a colon, a
leading `..`, bytes that are not valid UTF-8 — because each of those is one
legal element on the platforms this product runs on, and #784's contract is
that they are stored and restored verbatim. Refusing them would be the
worse bug of the two: a backup that silently drops every file whose name
contains a backslash is a backup nobody can rely on.

What makes such a name dangerous is joining it to a directory on a platform
whose separator differs, so that is checked where it happens: the assembled
path must still be under the destination resolved through
`filepath.EvalSymlinks`. Two checks for one property, differently shaped on
purpose — the first refuses a NAME, the second refuses a PATH this code
built.

A snapshot holding such a name stops the restore. It is not skipped: a
restore that silently omits files is a restore that lies about being
complete, and a name shaped like an escape is evidence about the whole
snapshot rather than about one file in it.

Two further rules fall out of the same threat model. A restore never writes
through a symbolic link that is already in the destination — following one
places the whole subtree wherever it points — and it never follows a link
it restores, whose target is content rather than an instruction.

### Nothing partially written ever carries an entry's real name

Every file is written to a `.backupd-restore-*` working file in its
destination directory and renamed into place. A cancelled, refused or
failed restore removes the working file, so the destination holds fewer
files rather than a plausible wrong one. `RestoreReport.Complete` is set
only when the walk reached the end of what was asked for, and the durable
operation refuses to record a restore as completed without it — the guard
lives where the claim is written, not only where it is made.

### Integrity is checked by reading the disk back

With `VerifyContent`, each file is hashed as it is written and the working
file is read back and hashed again before the rename, so a file whose bytes
did not survive the write is never published at all. A mismatch fails the
restore rather than being reported as a finding: the file would be present,
wrong, and never looked at again. The paths that record a restore as
evidence — the verification ladder's restore drill, and the durable
operation — always ask for it.

### A restore is a durable operation, and not the archived-copy one

`restore_snapshot` joins `run_cycle`, `run_backup_set` and
`restore_placement` on the operations table, so a restore survives a closed
tab and a restarted process exactly as a run does, and appears in the same
list, the same poll and the same CLI-equivalence table. It is deliberately
**not** in `externallyExecutedActions`: this process executes it, so a
process that died really did abandon it and the startup sweep is right to
fail the row.

It takes no single-flight lock. A cycle and a restore contend for nothing —
the restore reads a repository and writes outside the backup pipeline — and
the moment an operator most wants one is during an incident, which is
exactly when a scheduled cycle is also running.

## Consequences

- A restore point can be read back at three granularities, on any
  deployment, without the vendor's CLI.
- A hostile or merely foreign snapshot cannot write outside the directory
  an operator named. The property is fuzzed, and the seed corpus is the
  table of shapes an escape takes.
- The restore drill (ADR 0014's top verification rung) now runs through
  this extraction as well, so the deepest verification level and the
  operator-facing restore are one code path rather than two that could
  drift.
- Remote restore remains unavailable and is post-v1. An operator who needs
  data back onto a remote restores locally and copies, which is slower and
  is a thing they can actually do today.
- Ownership is not restored unless asked for. An unprivileged process
  cannot set it, and a restore refused over metadata nobody asked about is
  a restore that did not happen.

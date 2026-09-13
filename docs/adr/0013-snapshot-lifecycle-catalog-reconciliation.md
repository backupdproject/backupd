# ADR 0013: Snapshot lifecycle, catalog and crash reconciliation

- Status: accepted
- Date: 2026-09-12
- Supersedes: nothing. Extends ADR 0006 (embed Kopia behind a
  `backupengine` adapter), ADR 0011 (the repository adapter) and ADR 0012
  (the production source adapter).

## Context

The embedded engine can store a snapshot. Nothing about that made a
backup: a manifest returned by the engine says content was written, and
everything a person means by "the backup worked" happens after it — the
source was read completely, the snapshot was verified, and this manager
durably recorded that both were true.

Before this change the incremental engine had no lifecycle at all. A set
configured `engine: kopia` was refused by the run cycle (#780's
fail-closed guard), the only snapshot port was per-object
(`StreamingRepository`, one snapshot per file, ADR 0012's documented
Phase-1 interim), and there was no catalog, no state machine and no
answer to "what does a crash halfway through leave behind".

Two durable stores hold a backup set's snapshot state — this manager's
catalog and the repository itself — and a process that stops existing
between two writes leaves them disagreeing. That is the problem this ADR
is about.

## Decision

### 1. One snapshot per run, through a pull-shaped port

`backupengine.TreeRepository.SnapshotTree` stores one Kopia snapshot per
backup-set run, under one `SourceInfo`, tagged with the set and the
repository domain. The port is pull-shaped (`SourceDir`,
`SourceDirIterator`, `SourceEntry`) because the engine underneath is: its
uploader walks a tree and asks for children. The per-object push `Sink`
is therefore not an implementation of it and could not have been; the
source side keeps the reading safety it already owned (path refusals,
capability gates, cancellation, mutation detection) and exposes it as a
tree (`source.Tree`, `Adapter.OpenTree`).

`RepositorySink` is demoted, not deleted: it remains the per-object
interim that this package's own tests — including the real-repository
integration test — read a source through. The production path is the tree
snapshot.

The set's identity in the repository is derived from
`model.SourceIdentity` (a digest), never from the source's path, and its
host is the literal `backupd` rather than this machine's name. A hostname
is not stable, and the engine treats host/user/path as a source's
identity: a rename would present the same source as a new one and store a
second full copy of an unchanged tree.

### 2. A strict, durable state machine

`PENDING → SOURCE_SCAN → SNAPSHOT_WRITE → MANIFEST_COMMITTED →
VERIFICATION → CATALOG_COMMIT → SUCCESS`, persisted on the existing
durable-operation and idempotency infrastructure (`internal/state`), with
the legal edges in one table (`internal/snapshotlifecycle`).

Each phase is written **before** the work it names. That ordering is the
whole crash story: a row at `SNAPSHOT_WRITE` says an upload was in
flight, and the same row carrying a manifest id says the upload finished
and the process died before it could say anything else. Writing phases
afterwards would collapse those into one indistinguishable state.

Consequences that are deliberately negative:

- A Kopia manifest is not success. Until `SUCCESS`, no restore point is
  advertised, the previous last-known-good snapshot is neither pruned nor
  replaced, and the set's success timestamp does not move.
- Only a transition to `SUCCESS` may set the last-known-good flag, inside
  the transaction that makes it. A failed newer snapshot therefore cannot
  replace an older good one — a property of the schema rather than a rule
  code has to remember.
- `FAILED` is a dead end. A retry means reading the source again, which
  is a new pass at a new moment and therefore a new run with its own row;
  re-entering a failed row would overwrite the evidence of what went
  wrong.
- A `FAILED` row may still carry a snapshot id. A run whose source scan
  came back incomplete holds a well-formed manifest of a tree with a hole
  in it. It is kept, attributed and visible, because deleting it would
  destroy the only copy of what it does hold on the authority of the code
  that noticed the hole.

### 3. A snapshot catalog, not a fake artifact

`snapshot_runs` (migration 0009) records, per run: backup set, engine,
repository domain, snapshot/manifest id, source identity, source
consistency mode, start/completion, file and directory counts, logical
bytes, source bytes read, repository bytes written, content reused,
verification level configured **and** achieved, verification status,
hold/delete intent, retention state and last-known-good, plus an
append-only phase transition log. It duplicates none of the repository's
own metadata: the manifest id is opaque and is the only handle stored.

The counters are nullable. "Nobody measured this" and "this measured
zero" are different facts, and an adopted snapshot (below) has the first;
rendering it as the second would report a run that stored nothing.

### 4. Crash reconciliation, one rule per boundary, and nothing deletes

| Boundary | Verdict |
|---|---|
| died before the upload (`PENDING`, `SOURCE_SCAN`) | `FAILED`; nothing exists to adopt |
| died during the upload, no manifest | `FAILED`; unreferenced content is maintenance's business |
| manifest exists, catalog never recorded it | adopted onto the row, then verified |
| catalog row names a manifest the repository lacks | `FAILED` (pre-success) or `LOST` (post-success) |
| died after the manifest commit | verified and committed |
| died during verification | re-entered from the manifest and verified again (the one backward edge) |
| died after verification, before the catalog commit | completed **without** re-verifying: the pass is already durable |
| died during a snapshot delete | intent plus missing snapshot → `DELETED`; intent plus present snapshot → reported, never re-issued (retention owns deletes) |
| died during maintenance | reported; nothing changes. Maintenance never removes referenced content, so an interruption leaves unreferenced blobs and nothing else |

Two rules hold across all of them:

- **An unknown repository snapshot is quarantined, never deleted.** It
  may be from a run whose catalog write never landed, from a co-tenant
  set in a shared domain, or from an operator's own use of the vendor's
  tooling against their own bucket — and the evidence that would tell
  those apart is exactly what the crash destroyed. Quarantine rows are
  keyed by domain and snapshot id, so a repeating pass records each one
  once.
- **A pass that cannot read the repository decides nothing.** An
  unreachable repository presents exactly like one that has lost every
  snapshot in it, and a pass that decided from half the evidence would
  mark a whole deployment's restore points lost.

### 5. Engine dispatch in the run cycle

`processBackupSet` routes an incremental set to the snapshot pipeline and
returns; the artifact pipeline is untouched. Every other engine — and any
engine this build has not been taught — is still refused by
`unrunnableEngine`, which also still guards `Fetch` and `ReconcileAll`,
because those *are* the artifact pipeline.

A repository is created on the first run that needs one, at the location
derived from the declared domain, and **only** if the catalog holds no
snapshot this set ever committed. "Open failed with not-found, so create
one" is the version that loses a deployment's history: an unmounted or
sleeping share presents identically, and creating a repository there
would give the set a fresh empty one on the wrong filesystem, silently,
until somebody needed a restore.

### 6. Four numbers, never one

Scanned entries, logical bytes, source bytes read, repository bytes
written and content reused are separate fields on the catalog row, the
run result, the `snapshot_stats` event, the operation surface and the
Prometheus exposition — where they are five separate metrics rather than
one family with a `kind` label, because a labelled family is the shape a
dashboard sums. Presenting the logical size as "uploaded" reports a
deduplicated repository as growing by the size of the source every night,
which EPIC K names as the claim never to make.

## Consequences

- Verification depth is not yet selectable. The repository port offers one
  verification and it reads every byte, so a run records
  `content_full` as *achieved* whatever its set asked for. #784 adds the
  ladder; the seam is a level-parameterised verification on the port plus
  the achieved level the runner already records separately.
- A tree run is serial by construction: the source is a single forward
  cursor, so the uploader runs with one parallel upload and
  `Options.Concurrency` does not apply. Per-object retry does not apply
  either — the retry unit is the run.
- The bridge from the enumerator's depth-first arrival order to a tree
  requires depth-first grouped enumeration and refuses anything else
  loudly (`ErrEnumerationNotGrouped`). A mis-shaped snapshot is not an
  acceptable alternative to a refusal.
- An empty source directory does not reach the snapshot, because the
  enumerator descends into directories rather than yielding them. It is
  recorded here as a known, bounded gap rather than fixed by buffering a
  directory index.
- A repository domain now needs `passphrase` (file, env or command) in
  configuration, required only for a domain a backup set actually
  references. A repository this product creates is always encrypted, and
  there is no field to paste a passphrase into.
- The API surfaces snapshot facts on the existing durable-operation read,
  derived per poll rather than frozen into the operation's result text, so
  a client sees a restore point that was later found `LOST`. Wiring those
  fields through the generated `api/v1` bindings and the web UI is #788's.

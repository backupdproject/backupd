# The incremental engine: snapshots, repositories, and what "incremental" does and does not mean

This is the operator account of EPIC K, the embedded incremental backup
engine (#780-#789). It is the document to read before turning it on, and
the one to quote when a report does not say what somebody expected it to
say.

Two engines ship in one binary and a backup set names exactly one of
them:

- **`artifact`**, the engine this product started as. A producer writes a
  finished file — a dump, a tarball, an image — and this manager
  discovers it, pulls the whole thing, verifies it, commits it durably,
  records that it did, and only then removes the remote copy. One
  artifact in, one artifact out, byte for byte.
- **`kopia`**, the incremental engine. A source is a *tree* rather than a
  finished file, and each run stores a **snapshot** of that tree into an
  encrypted, content-addressed **repository**, storing only content the
  repository does not already hold.

Everything below is about the second one. Nothing below changes anything
about the first: an artifact backup set behaves exactly as it did before
EPIC K existed, and nothing in the artifact path consults the incremental
engine's configuration, its gate, or its repositories.

**Contents**

- [The production gate](#the-production-gate)
- [Artifact and incremental, side by side](#artifact-and-incremental-side-by-side)
- [Scanning is not incremental storage](#scanning-is-not-incremental-storage)
- [The five numbers a run reports](#the-five-numbers-a-run-reports)
- [Source-consistency modes](#source-consistency-modes)
- [The metadata-trust model](#the-metadata-trust-model-and-what-it-is-for-here)
- [Repository domains](#repository-domains)
- [Where a repository's bytes live](#where-a-repositorys-bytes-live)
- [Verification](#verification)
- [Snapshot retention, holds and the restore point](#snapshot-retention-holds-and-the-restore-point)
- [Maintenance](#maintenance)
- [Read-only sources and the two write probes](#read-only-sources-and-the-two-write-probes)
- [Configuration reference](#configuration-reference)
- [What this build does not do](#what-this-build-does-not-do)

Runbooks — creating an incremental set, repository corruption, credential
recovery — are in [`docs/incremental-runbooks.md`](incremental-runbooks.md).
The commands are on [the reference
page](https://backupdproject.github.io/backupd/reference.html#cli); the
wire contract is `api/v1/openapi.json` and
[`docs/api/contract.md`](api/contract.md).

## The production gate

The incremental engine is **off by default** and has to be turned on
deliberately. One block in `config.yaml`:

```yaml
incremental_engine:
  enabled: true
```

An absent block is disabled. One environment variable overrides the file,
in **both** directions — `RETND_INCREMENTAL_ENGINE=1|true|yes|on`
enables it even where the file says otherwise, and `0|false|no|off`
disables it even where the file enables it:

```bash
RETND_INCREMENTAL_ENGINE=1 retnd run
```

Unset or empty defers to the file. A value that is neither spelling is
**refused when the configuration is validated**, naming the variable and
the spellings it accepts, rather than being quietly read as "off": a typo
in the thing that decides whether backups happen must not resolve to the
answer that means they do not.

One function resolves it, `config.Config.IncrementalEngineEnabled()`, and
nothing else re-derives it.

The variable is read by the **engine** process — the one that runs the
cycle — and not by the web host, so it only has to be set where `retnd`
itself runs. On the standard container deployment that means it is a tool
for a command you launch (`docker compose exec -e …`), a CLI-only install,
or a systemd unit; `container/compose.yaml` declares the environment it
passes through by name and does not pass this one, so the place to enable
the engine on a compose or appliance deployment is `config.yaml`. See
[`docs/deployment.md`](deployment.md#the-incremental-engines-gate-and-where-to-set-it-in-a-container-deployment).

### What the gate stops, and what it deliberately does not

With the gate **off**:

| Refused | Unaffected |
| --- | --- |
| creating or updating a backup set with `engine: kopia` | every `artifact` backup set, entirely |
| an incremental set's snapshot cycle | loading and validating a configuration that already declares `kopia` sets |
| `snapshot verify` | `backup-set test-connection`, on either engine |
| `snapshot restore` | artifact discovery, transfer, retention and prune |
| repository maintenance | the journal, the API, the web interface |
| the repository probes and their alerts | |

The refusal is one sentence, from one place
(`config.IncrementalEngineDisabledMessage()`):

```text
the incremental (kopia) backup engine is disabled in this deployment: set
incremental_engine.enabled: true in config.yaml, or
RETND_INCREMENTAL_ENGINE=1 in the environment, to enable it; existing
artifact backup sets are unaffected
```

It reaches a caller as `config.ErrIncrementalEngineDisabled`, an HTTP
**409** with error code `INCREMENTAL_ENGINE_DISABLED`, and an ordinary
CLI failure (exit `1`).

**Turning the gate off never bricks a daemon.** A configuration holding
incremental sets still loads, still validates, and still backs up every
artifact set on its schedule; the incremental sets report the refusal
above. That asymmetry is the whole design of the flag: a deployment that
has to disable the engine at 3am — because a repository is unreachable,
because a maintenance pass went wrong, because a release is being rolled
back — must not have to choose between the engine and the rest of its
backups.

A snapshot restore is **refused rather than silently skipped** when the
gate is off. A restore that reported success and wrote nothing is the one
failure mode a backup product may never have.

## Artifact and incremental, side by side

| | `artifact` | `kopia` |
| --- | --- | --- |
| What the source is | finished files a producer wrote | a directory tree |
| Which sources it reads | any bundled backend, **including `sftp`** | a backend whose listing can be bounded: `local_volume`, `s3`. **Not `sftp`** ([why](#what-this-build-does-not-do)) |
| What one run produces | one artifact, copied whole | one snapshot manifest |
| What is stored | the whole file, every time | only content the repository does not already hold |
| Identity | set plus remote basename (`model.ArtifactID`) | the set's `uuid` lineage plus a manifest id |
| Where it lands | the backup root, as files you can see | an encrypted repository under a reserved namespace |
| Restore | copy the artifact back yourself | `retnd snapshot restore` |
| Deletes the remote original | yes, after a verified backup, unless read-only | **no**, never |
| Retention | FR-18 GFS over artifacts | the same GFS chain, over snapshots |
| Deduplication across runs | none | the reason the engine exists |
| Config keys it reads | `local_path`, `include`, `completion`, `validation` | `repository_domain`, `source_consistency`, `verification_*` |

That first pair of rows is the engine/source split this product intends,
rather than an accident of what got implemented first. **The `artifact`
engine is how a remote machine's finished output is pulled off it** — it
is the SFTP half of this product, and it stays the answer for a producer
writing dumps onto a server somewhere. **The incremental engine snapshots
a source tree this deployment can walk in bounded memory**: a local
volume, a mounted export, an object-store prefix. A tree over SFTP is
refused at run time rather than walked, for the reason
[below](#what-this-build-does-not-do).

The engine is chosen once, at creation, with `--engine`. It is **not**
editable afterwards: `backup-set patch` has no `--engine` flag, and the
two engines identify what they store in incompatible ways, so a set that
changed engine in place would abandon everything it had already stored
under an identity nothing would look for again. Moving a source onto the
incremental engine is therefore a *new backup set*, and it is a
deliberate operator procedure — see [the migration
runbook](incremental-runbooks.md#runbook-1-moving-a-source-onto-the-incremental-engine).

**retnd never converts an `artifact` set into a `kopia` one.** Not on
upgrade, not on a configuration reload, not as a convenience. An existing
artifact backup set that was running before the incremental engine
existed keeps running exactly as it was, with the same artifacts, the
same retention verdicts and the same journal rows.

The keys are refused across the line rather than ignored. An incremental
create that passes `--local-path` or `--completion-strategy` is refused
by name:

```text
retnd: service: invalid request: invalid config (3 problems):
  - sources[0].backup_sets[1]: local_path is only read by the "artifact" engine, and this set runs the "kopia" engine; remove the key, or set engine: artifact
  - sources[0].backup_sets[1]: completion.strategy is only read by the "artifact" engine, and this set runs the "kopia" engine; remove the key, or set engine: artifact
  - sources[0].backup_sets[1]: repository_domain "nodomain" is not declared in repository_domains (declared: production)
```

A key that cannot ever be acted on is refused rather than ignored, and
`--repository-domain` has no default at all, because which security
boundary a source's history is kept in is exactly the decision that has
to be intentional.

## Scanning is not incremental storage

This is the section to read if you only read one.

**Every run walks the whole source tree, and reads all of it.** There is
no "incremental scan", no change journal, no watcher, and no inotify. A
run enumerates every entry under the source root, streams every file's
content through, and stores only the content the repository does not
already hold. What is incremental is **the storage**, and in this build
that is the only thing that is.

Three costs, and they scale with three different things:

| Cost | Scales with | Every run? |
| --- | --- | --- |
| **Scan** — enumerate the tree, stat every entry | the **number of entries** | yes, all of them |
| **Read** — pull bytes off the source | the **size of the tree** | yes, all of it |
| **Store** — write bytes into the repository | the content the repository does not already hold | rarely, and often almost nothing |

So a million-entry source with nothing changed in it is still a
million-entry walk and still a full read. What collapses is the third
row: almost nothing is written, because almost nothing is new.

### Why the read is not skipped, when the engine could skip it

The engine underneath can reuse a file's content from the previous
snapshot on a metadata comparison, and the tree path in this product
**deliberately does not let it**. The rule is one sentence:

> Content is reused, files are not.

Deduplication happens *below* the boundary, on content that has already
been read. The reason is what the metadata actually is: a size and a
modification time captured at scan time, from a live directory somebody
else is writing to. A modification time is settable — `rsync --times`,
`cp -p`, `tar -p`, an editor that preserves timestamps, restoring the
source itself from a backup — so skipping a file's content because its
size and mtime look familiar is how a rewritten file silently keeps its
old content in every snapshot from now on, discovered on the day somebody
restores it.

That is a real trade and it is worth being blunt about which way it went:
**this engine buys storage, not source I/O.** A nightly run over a 2 TB
tree reads 2 TB off the source every night. What it does not do is store
2 TB every night. If the bottleneck is the source's own disk or its
network link rather than the space the copies occupy, this engine does not
solve that problem, and the honest answer is a smaller source root, an
exclude list, or a longer interval.

`source_bytes_read` is therefore close to `logical_bytes` on every run,
and the gap that matters is between `source_bytes_read` and
`repository_bytes_written`. `content_reused_bytes` is that gap, stated
directly.

The walk itself is not avoided either, and a source large enough for the
enumeration alone to exceed the backup window is a source this design
does not rescue.

The walk is **bounded in memory**, which is the property that makes the
engine usable on a NAS at all. It streams entries and holds resumable
directory frames rather than a slice of everything or a stack of pending
paths, so peak memory does not grow with entry count. ADR 0008's measured
figures, on an Apple M5 with APFS:

| entries in one flat directory | peak heap | time to first entry |
| --- | --- | --- |
| 100,000, unbounded `List` | 44.4 MiB | 320 ms |
| 100,000, bounded enumerator | 3.5 MiB | 1.97 ms |
| 1,000,000, unbounded `List` | 493.4 MiB | 102.45 s |
| 1,000,000, bounded enumerator | 4.2 MiB | 12.26 ms |

Ten times the entries, roughly 1.2x the memory. The same holds for a tree
that is mostly directories, which is the shape these deployments actually
have: 1,000,000 subdirectories cost 3.8 MiB with resumable frames against
63.1 MiB with a stack of pending paths.

**Incremental storage** is the other half. A snapshot is a manifest over
content-addressed blobs. Content the repository already holds — because
last night's run stored it, because another file in the same tree has the
same content, because a set sharing the domain stored it — is referenced,
not written again. A snapshot of a 100 GB tree that changed by 1% is not
a second 100 GB copy; it is a manifest plus roughly the changed content,
compressed.

Two consequences worth stating plainly, because both surprise people:

1. **A snapshot is not a file you can see.** There is no artifact on the
   backup root to copy. The bytes live inside the repository's own
   namespace, encrypted, and the only supported way to get data back out
   is `retnd snapshot restore` (or the Restore screen, which submits
   the same operation).
2. **Deleting a snapshot frees nothing on its own.** Retention removes a
   manifest. The content behind it stays in the repository's packs until
   [maintenance](#maintenance) decides nothing references it any more.

## The five numbers a run reports

A run reports five separate measurements and **deliberately no total**.
`retnd snapshot list`, `snapshot show`, the API's `Snapshot` model and
every screen show all five:

| Field | What it measures |
| --- | --- |
| `entries_scanned` | how many source entries the pass considered, of every kind, **including the ones it deliberately skipped**. Not files plus directories: that reconstruction leaves out everything the pass refused |
| `logical_bytes` | the size of the tree as the source described it. This is what was **scanned**, and never what was uploaded |
| `source_bytes_read` | bytes the pass actually pulled off the source. An incremental engine still reads the source; this is what that cost |
| `repository_bytes_written` | bytes that actually landed in the repository's storage, after deduplication and compression. **The only one of these numbers that is storage growth** |
| `content_reused_bytes` | bytes the pass did not have to store again. The gap between read and written |

A single "bytes backed up" figure would report a 100 GB tree
deduplicated down to 200 MB of new content as a 100 GB upload, every
night, forever. That is why there is no such field and no plan for one.

**Absent is not zero.** Every one of those counters is nullable on the
wire, along with `source_complete` and `duration_seconds`, and null means
*nobody measured this* — the honest state of a run that died before its
manifest was recorded, and of a snapshot adopted from the repository by
crash reconciliation. Every surface prints `not measured`, never `0`:

```text
entries scanned: not measured
files:           not measured
directories:     not measured
logical size:    not measured
written:         not measured
reused:          not measured
```

"reused: 0 bytes" would send an operator hunting a fault in a backup that
is working, and "files: 0" would report an empty source every time a scan
crashed.

A run also carries the phase it reached: `SOURCE_SCAN`,
`SNAPSHOT_WRITE`, `MANIFEST_COMMITTED`, `VERIFICATION`, `SUCCESS`,
`FAILED`, `QUARANTINED`, `LOST` or `DELETED`. **Only `SUCCESS` is a
restore point.** A run has a `run_id` from the moment the pass starts and
a `snapshot_id` — the engine's manifest id — only once a manifest has
been committed, so a failed run has the first and not the second.

## Source-consistency modes

An incremental run reads a tree over a period of time while something
else may be writing to it. What that run can honestly claim about the
result depends entirely on what the operator arranged, and **this product
cannot detect which of the three arrangements is in force**. So it is
declared, per set, with `source_consistency` (`--source-consistency`),
and recorded **on every run** rather than only in the configuration: a
claim about the moment the data was read is not a claim about the
configuration as it stands now.

| Mode | What it means | A point in time? | A mutation observed during the run is |
| --- | --- | --- | --- |
| `live_best_effort` | the source is read while whatever writes to it keeps writing | **no** | ordinary — the mode exists to accommodate it |
| `externally_quiesced` | the operator stopped, flushed or locked the writers for the run | no | a **contract violation**: the arrangement did not hold |
| `external_snapshot` | the run reads a frozen image — LVM, ZFS, VSS, a read-only clone | **yes** | a **contract violation**: the path was not the snapshot |

`live_best_effort` is the default, because it needs no cooperation and it
promises the least: each file is captured coherently **or reported**, and
the set of files is not a point in time. Two files in one
`live_best_effort` snapshot may be from either side of a change that
happened between them.

**Only `external_snapshot` is ever rendered as "consistent".** Under the
other two, a run whose every file was captured coherently is `complete`,
which is a different sentence from consistent, and both appear in a run
report.

Declaring a mode buys one thing that matters: a mutation observed during
the run can be reported **against the claim**. Both of the stronger
arrangements fail silently otherwise — a quiesce hook that stopped the
wrong container, a "snapshot path" that was actually the live mount — and
"this run was complete, and your belief about it is false" is exactly the
sentence an operator needs and never gets anywhere else.

Not every failure is evidence of a broken claim. Only `incomplete`,
`retried` and `vanished` captures are evidence the source moved; an
`EACCES` is not, and reporting a permission error as a broken quiesce
sends somebody to look at the wrong thing.

### Every capture has a proven window

The reader that captures one file bounds its own read window and ends in
exactly one of six outcomes. There is no "probably fine":

| Outcome | Verified? | Run incomplete? | Meaning |
| --- | --- | --- | --- |
| `stable` | yes | no | nothing moved; metadata identical before and after |
| `retried` | yes | no | something moved, and a later attempt within the bound saw a file that held still. The stored digest is the settled content, never the torn read |
| `incomplete` | **no** | yes | the source kept moving: not captured coherently within the retry bound |
| `unreadable` | **no** | yes | the source did not answer: an open, read or stat failed, or the run was cancelled |
| `vanished` | **no** | yes | the path no longer names anything |
| `not_a_file` | no | no | the path names a symlink, directory, socket or device |

The checks behind those verdicts are stat-before, **fstat on the open
descriptor** after (which answers about the object the bytes came from
even after the path was replaced), byte count against that fstat's size,
a stat of the *path* after the read (which catches a rename or a delete
that leaves the descriptor perfectly readable), a device+inode comparison
that tells "rewritten" apart from "a different file renamed onto this
path", and — the only check that sees a mutation whose metadata was
restored — a second full read with the digests compared.

The full argument, including why this is two axes and not one, is
[ADR 0009](adr/0009-source-consistency-and-metadata-trust-model.md).

## The metadata-trust model, and what it is for here

The section above says the tree path reads every byte. This section is
the model behind that decision, and it is worth reading because it is
what a run *reports* about its source even where it does not change what
the run reads.

The pinned engine (kopia v0.23.1) reuses a file's content from the
previous snapshot when the current directory entry and the stored one
agree on **name, mode, owner, modification time and size**. That is the
complete test, and it is the correct, standard heuristic for a snapshot
tool. It is also not proof of identity, and the resolutions available
here are coarse: SFTP carries mtime as whole seconds, and this engine's
transport keeps unix seconds on every path, so even a filesystem that
records nanoseconds contributes none of them.

So each backend is classified by what it can actually *prove*:

| Backend | mtime precision as kept here | backend hash without a read | generation id | trust class |
| --- | --- | --- | --- | --- |
| `local_volume` | 1s (the disk keeps 1ns; the transport keeps seconds) | none | no | **weak** |
| `s3` | 1ms | md5 (ETag, single-part objects only) | yes | **strong** |
| `sftp` | 1s | none | no | **weak** |

No timestamp resolution, however fine, reaches `strong`. A hash that
costs a full read of the file **is** the read, so it is not evidence that
lets a read be skipped and is not recorded as one. `unknown` — an
unclassified backend, or one whose sizes do not hold still — is treated
more harshly than `weak` on purpose: a weak signal can be sampled
around, an unestablished one cannot.

The class then feeds a verification policy, under one of two presets:

| Trust class | `trust_metadata` | `conservative` |
| --- | --- | --- |
| `strong` | compare the backend's hash or generation id every run; read anything it cannot answer for; full read floor **90 days** | the same, plus every path read in full **every 30 days** |
| `weak` | re-read **5% of paths** every run (a deterministic sample, hashed from the path), every path at least **weekly** | **read and hash every file every run** |
| `unknown` | read and hash every file every run | read and hash every file every run |

Two properties hold over every input, including classes and presets this
code does not recognise: **no policy ever permits skipping content
forever**, and any policy that may skip content names the interval after
which it stops. An unrecognised preset is treated as `conservative` and
an unrecognised class as `unknown` — a typo must never buy a weaker
policy than the one that was asked for. Sampling is a hash of the path
rather than a random draw, so a run's cost and a run's coverage are both
reproducible.

**In this build the preset is not a configuration key.** The tree
snapshot path is pinned to `conservative`, and not because it is the
safe-looking default: a tree run reads every byte the source offers
regardless, so `conservative` is the preset that *describes what actually
happens*. Recording `trust_metadata` on a run report would put a claim on
the record that the run did not act on. So there is no
`metadata-trust` key in `config.yaml` and no flag for it; the class and
the policy are reported on the run, and the read happens either way.

What the classification buys today is therefore honesty rather than
speed: a run says which backend it read, what that backend can prove, and
which policy would have applied. If a future change lets the tree path
skip content, this is the model it will have to satisfy first — and it
will still not be allowed to skip forever.

## Repository domains

A **repository domain** is a security boundary: one encrypted repository,
one passphrase, one deduplication span, one maintenance owner, one
corruption fate. A backup set *names* one, and that is required for an
incremental set and refused for an artifact one.

Two sets sharing a domain share all six of those things. That is the
point of sharing — deduplication across sets is where the storage saving
comes from — and it is also the reason the decision is never defaulted.

Domains are declared at the top level of `config.yaml`:

```yaml
repository_domains:
  - id: production
    description: Snapshots of the production tree
    isolation: shared
    passphrase:
      file: /etc/retnd/secrets/production.passphrase
```

| Field | Required | Notes |
| --- | --- | --- |
| `id` | yes | how a backup set names it, and the directory its bytes live in. No slashes, not `.` or `..`, no control characters |
| `description` | no | never interpreted. It exists because a list of five domain ids with no prose is a list nobody reviews, and reviewing it is how an isolation mistake gets caught before an audit does |
| `isolation` | **yes** | `shared` or `isolated`. There is no default in either direction: defaulting to shared would make an isolation boundary a belief, and defaulting to isolated would silently forgo the deduplication that is the reason to run the engine |
| `passphrase` | yes, for a domain a set references | `file`, `env` or `command` — a reference, never material |

There is **no field to paste a passphrase into**, and there will not be.
The three sources are the same three this file already offers for the SSH
key and for `key_encryption`: `file` reads a path, `env` names an
environment variable, `command` is an argv array whose stdout is the
secret (invoked directly, never through a shell), which is the path a
secrets manager is adopted through without this project taking a
dependency on anybody's SDK.

A declared domain that no set references yet is legal and staged: that is
how an operator builds one up. The passphrase requirement is enforced
where the reference is.

**A domain can be declared without editing this file**, and only
declared. `retnd repository create <domain> --isolation shared|isolated
--passphrase-file F` (also `--passphrase-env`, and `--passphrase-command`
once per argv word), `POST /repositories`, and the *Define a repository
domain* screen all write the same `repository_domains:` entry atomically
and hot-reload the deployment, so the domain is nameable by a backup set
at once. What they write is the DECLARATION: no store is created, no
storage is opened and no passphrase is resolved — the repository is
realized by the first backup run that stores a snapshot in the domain,
which is what already happened for a domain named on the add-backup-set
wizard's repository step. There is still no verb that EDITS or REMOVES a
domain: changing or retiring one is an edit to `config.yaml` — see [the
migration
runbook](incremental-runbooks.md#runbook-1-moving-a-source-onto-the-incremental-engine).

## Where a repository's bytes live

In this build, one place: a **local** repository under the backup root, in
a reserved namespace.

```text
<backup_root>/.backupd/repositories/<domain>/    the repository's own blobs
<backup_root>/.backupd/state/                    connection config, index caches, maintenance ownership
```

The reservation is the mechanism, not the dot prefix. A backup root is a
populated, exported, walked directory: artifacts arrive in it, a catalog
records them, retention decides which stop being protected, and a prune
deletes those. A content-addressed repository dropped into that same
directory is a few thousand files with names like `p0a1b2c3` and no
sidecar manifest, and the most likely outcome is not an error — it is a
prune that removes pack files, and a repository that cannot restore
anything, discovered later. So one predicate decides it: everything under
`<backup_root>/.backupd` is repository internals, is never an artifact,
and artifact management may not enter it.

If you exclude one path from an SMB share, a virus scanner, or a backup
of the backup, exclude `.backupd`.

The engine's repository adapter also speaks **S3** natively, and that path
is exercised against a real object store in the repository's own
integration tests. It is not reachable from `config.yaml` in this build:
`repository_domains` has no location field, and every domain resolves to a
local repository under the backup root. A deployment that wants its
snapshots off-site today puts the backup root on storage that is itself
off-site.

## Verification

A snapshot is not a restore point because a run said `SUCCESS`. It is one
because something read it back. Four rungs, configured per set with
`verification_level`, and a run must prove its configured level to become
a restore point:

| Level | What it proves | What it costs |
| --- | --- | --- |
| `structural` | the manifest and the directory structure resolve; every referenced object is addressable | the repository's own metadata; effectively nothing. **The default** |
| `content_sample` | a percentage of the snapshot's **files** is read back and hashed | `verification_sample_percent` of the content |
| `content_full` | every file in the snapshot is read back | a full read of the snapshot |
| `restore_drill` | the snapshot is actually restored and compared | a full read plus a full write |

`verification_sample_percent` is a percentage of **files**, not of bytes,
because damage arrives per file and a byte-proportional sample spends its
whole budget inside the largest one. The sample is a fixed stride, so a
stated percentage is read every time rather than on average.

Two cadences raise what a run proves without raising the level:

- `verification_full_every` — how often a full content read happens
  regardless of the level;
- `verification_restore_drill_every` — how often a real restore is
  performed and compared.

Both default to **never**, which is the only safe default: nobody should
acquire a nightly full read of their entire source, or a nightly restore
of it, by leaving a key out. A cadence only ever raises what a run
proves; it cannot lower the configured level. A restore drill writes a
whole restored tree, and it writes it inside the reserved namespace, for
the reason the namespace exists.

**Asked-for and achieved are two fields, never one.**
`verification_level` is what the set asked for;
`verification_level_achieved` is what a check actually proved, and it is
absent when nothing was proven. A run that asked for a full content
verification and managed only a structural one reads as exactly that.
`verification_status` is `pending`, `passed` or `failed`, and absent when
nothing has been attempted.

`retnd snapshot verify` proves it now, at a depth you state:

```bash
retnd snapshot verify production/uploads-tree --level content_sample --sample-percent 10
```

Without `--run` it verifies the set's last known good snapshot; without
`--level` it uses the level the set is configured for; it exits non-zero
when the verification finds damage. It records nothing onto the snapshot
row — what a *run* proved is what that run proved, and an on-demand check
months later is a different claim about a different moment, reported on
the operation that performed it.

## Snapshot retention, holds and the restore point

Snapshot retention is **the same GFS chain** the deployment already uses
for artifacts, not a second implementation. The snapshot catalog is
projected onto the shape `internal/retention` already classifies, and the
existing `DecideKeep` decides. A second GFS implementation would agree
with the first on the day its test was written and then diverge on a DST
edge or a tie-break.

Three narrowings come with the projection:

- only a run at `SUCCESS` carrying a manifest, with no failed
  verification, is in scope — the snapshot equivalent of "managed,
  completed backups";
- a snapshot is bucketed by its run's own start time, and there is no
  producer-claimed timestamp to admit: this manager took the snapshot,
  off its own clock;
- a manifest id this product cannot address is a **refusal**, not an
  omission. A plan that silently fails to mention a snapshot in the
  repository is a plan whose gaps are invisible to the operator
  confirming it.

A verdict is `KEEP`, `DELETE` or `REFUSE`, and never two of them.
`REFUSE` means the snapshot was a delete candidate and something stopped
it; folding it into `KEEP` would hide the only one of the three that
needs somebody to look at it. A `KEEP` names every tier that selected it;
a `REFUSE` names what refused.

```bash
retnd snapshot retention production/uploads-tree   # a preview; it deletes nothing
```

**Last known good.** Exactly one snapshot per backup set may be offered
as a restore point, and it is the one every command that takes an
optional snapshot defaults to. Retention never deletes it.

**Holds.** A hold is a durable statement that one named snapshot must not
be deleted, whoever's retention policy says otherwise, until somebody
releases it:

```bash
retnd snapshot hold production/uploads-tree --reason "incident 4412, legal hold"
retnd snapshot holds production/uploads-tree
retnd snapshot unhold production/uploads-tree hold_01J...
```

`--reason` is **required**. A hold nobody explained is one nobody except
the person who placed it dares release, and they leave — which makes it
permanent by accident. A hold records who placed it and why, and
separately who released it and when; releasing is never a delete of the
row. Several holds may sit on one snapshot, because a legal hold and an
operator's "not until the migration is signed off" are two decisions with
two owners. Releasing a hold deletes nothing: it returns the snapshot to
whatever the retention policy already said about it.

## Maintenance

Retention deletes a manifest. **Maintenance is what reclaims the
storage**, by rewriting indexes and collecting content nothing references
any more. It is a repository-domain operation, never a backup-set one.

Two properties decide how it is treated:

1. **Two processes maintaining one repository concurrently is how a
   repository loses content it still references.** So exactly one
   instance owns maintenance for a domain: an unclaimed repository is
   claimed by the first instance that maintains it, and a repository
   claimed by another instance is refused *before the repository is
   touched at all*. A screen showing a domain somebody else owns disables
   its actions with the reason stated, because "press it and find out" is
   how two instances end up compacting one store.
2. **Its safety comes from time, not from locking.** The engine's full
   safety margins — content younger than 24 hours is not collected, two
   garbage collections four hours apart must agree before deleted
   contents leave the index, an unreferenced pack blob is left alone for
   a day — are what make maintenance safe beside a snapshot that is still
   being written. The engine offers a mode that removes all of them. This
   product does not use it and does not expose it.

`repository maintenance` reports who owns it, when quick and full last
ran, when the next run is eligible, and whether it is overdue. It opens
nothing, so it answers while the repository itself is unreachable:

```text
$ retnd repository maintenance production
production
  owner:         nobody has claimed maintenance for this repository
  last quick:    never
  last full:     never
  next eligible: never
  due:           yes, : no maintenance has ever been recorded for this repository
  overdue:       false
  runs:          0 (0 failed)
  reclaimed:     0 bytes
```

Quick and full are not interchangeable: **full maintenance is what
actually reclaims storage**, and a repository with recent quick runs and
no full run is growing.

In this build there is no scheduled maintenance window and no verb that
starts one: `repomaintenance` is implemented, fenced and tested, and
nothing in the daemon, the API or the CLI calls it yet. What the surfaces
give you is the decision — due, overdue, who owns it, what the retained
history reclaimed — and an overdue repository raises the alert condition
rather than quietly compacting itself.

`repository health` is the other half, and it reports every probe
separately rather than reducing them to one boolean, because the remedies
are different: unreachable is a mount, unwritable is a permission,
invalid credentials is a passphrase, and overdue maintenance is a
schedule.

```text
$ retnd repository health
production  FAILING
  shared:          true
  reachable:       false
  readable:        false
  writable:        false
  credentials:     the declared passphrase did not open this repository
  clock:           nothing durable to compare against yet
  maintenance:     never run
  last snapshot:   none
  last verified:   none
  backup set:      production/uploads-tree
  this build cannot place this repository domain's storage, so nothing could be opened
```

`FAILING` is reserved for a repository that cannot take a backup at all —
unreachable, unreadable, unwritable, or wrong credentials. Everything
that still works and is worth attention — overdue maintenance, a drifted
clock, a verification that failed — is `DEGRADED`, the same line the
backup-set half of this model already draws. The verb exits non-zero when
any repository is `FAILING`.

`clock_skew_seconds` is **signed and nullable**, and the sign is the
message: negative means this machine is behind the newest durable
timestamp already on record, which is the direction that dates a new
snapshot before an older one and makes retention bucket them into the
wrong periods. Null means there is nothing durable to compare against
yet, which is what a brand-new deployment genuinely is.

A `reachable` and `readable` repository with `credentials_valid: false` is
the signature of a rotated or mis-referenced secret, and a `reachable`
location that is **not** readable is the shape an empty or wrong mount
presents — which is exactly the case that must never be mistaken for "no
repository yet".

## Read-only sources and the two write probes

There are two write probes in this product. They answer different
questions about different things, and confusing them sends people to the
wrong remedy.

**The source write probe (#852)** proves this deployment's own
credentials can write to the *source*. It is step seven of the connection
test: a uniquely named dotfile is created under the configured remote
path, removed, and confirmed gone. Both answers **pass** — a read-only
source is a posture, not a failure — so what the probe decides is
`writable`, not the verdict.

What that answer arms is deleting the remote original after a verified
backup. retnd may only do that when the source's own credentials can
actually write there, and until #852 the only proof was the first cycle
that tried: an account that can read every byte and unlink nothing — an
ordinary, frequently recommended posture — looked identical to one that
could do both.

- `writable: true` — *delete from source* may be enabled.
- `writable: false` — enabling it is **refused**, never coerced:
  `ErrSourceNotWritable`, HTTP **409**
  `BACKUP_SET_SOURCE_NOT_WRITABLE`. That refusal is applied from one
  place and covers create, first-run create, a connection-changing edit,
  and `backup-set read-only off`. A `200` for "back up and delete the
  originals" that quietly did not delete is the failure this exists to
  end.
- `writable` is a **required** field on the wire and never omitted:
  absent and false must not be the same thing to a client, and the
  control at the other end deletes somebody's files, so a missing answer
  is read as the refusing one.
- Nothing was proven means `false`. Nothing may enable a delete on an
  absence of evidence.

Turning read-only **on** is never gated — making a set safer does not
need the source's permission:

```bash
retnd backup-set read-only production/uploads-tree on    # never refused
retnd backup-set read-only production/uploads-tree off   # refused unless the probe proved writable
```

The probe's errors are classified and then **dropped for their own
text**, so no remote path, chroot home or probe filename can reach a log,
a feed or an API response. The one outcome that leaves litter — written
but not removed — is its own sentinel with its own sentence, naming the
prefix to look for.

This probe is **not** behind the incremental-engine gate. It runs on both
engines, because an artifact set's connection test has to prove
writability whether or not the incremental engine is enabled anywhere,
and a `kopia` set's connection test also still runs: it reads the source
and opens no repository.

**The repository write probe** is the `writable` field on repository
health. It proves this deployment can write to the *repository*, by
writing something in the reserved namespace and removing it again, never
by inferring writability from a successful read. It lives inside the
repository open/health path, so it only happens once the gate is open —
with the gate off nothing opens a repository, no repository probe runs,
and no probe-derived repository state is written.

An incremental set is never the mechanism that deletes a remote original
anyway: the incremental engine does not delete from the source at all.
The probe still runs for a `kopia` set, because the answer is a fact
about the source's credentials that belongs in the connection report.

## Configuration reference

Deployment level:

```yaml
incremental_engine:
  enabled: true                    # default false. RETND_INCREMENTAL_ENGINE overrides, both ways

repository_domains:
  - id: production
    description: Snapshots of the production tree
    isolation: shared              # shared | isolated, required, no default
    passphrase:
      file: /etc/retnd/secrets/production.passphrase
```

Backup-set level:

| Key | CLI flag | Default | Read by |
| --- | --- | --- | --- |
| `engine` | `--engine` (create only) | `artifact` | both |
| `uuid` | — (minted on create) | minted | `kopia` only; refused for `artifact` |
| `repository_domain` | `--repository-domain` (create only) | **none; required** | `kopia` only; refused for `artifact` |
| `source_consistency` | `--source-consistency` | `live_best_effort` | `kopia` |
| `verification_level` | `--verification-level` | `structural` | `kopia` |
| `verification_sample_percent` | `--verification-sample-percent` | the engine's own default | `kopia` |
| `verification_full_every` | `--verification-full-every` | never | `kopia` |
| `verification_restore_drill_every` | `--verification-restore-drill-every` | never | `kopia` |
| `source_mount_prefix` | — | none | `kopia` |
| `local_path`, `include`, `completion`, `validation`, `revalidation` | the artifact flags | — | `artifact` only; **refused** for `kopia` |

`uuid` is the set's snapshot **lineage** key, and it is why a set can be
renamed. Names are editable; if the lineage hung off the name, renaming a
set would orphan every snapshot it had ever taken, and the next run would
find no predecessor, re-read the whole source and store a second full
copy. It is not derived from the name either, because deriving it would be
that same bug with extra steps.

`source_mount_prefix` is the leading part of `remote_path` that describes
how *this deployment* reaches the source — a container bind mount, an
install prefix, the temporary directory an external snapshot is mounted
under — rather than part of the source's own identity. It is stripped
before the path becomes part of what the snapshot records, so moving the
mount does not re-store the tree.

A configuration write goes where every configuration write in this
product goes: written here when nothing is serving the deployment, handed
to the serving process over its API when there is a route to it, and
refused when something is serving and there is no route. See [the
reference page](https://backupdproject.github.io/backupd/reference.html#cli-modes).

## What this build does not do

Stated as limitations rather than left to be discovered:

- **The source of an incremental set must be a backend whose directory
  listing can be bounded, which is the engine/source split rather than a
  temporary gap.** `local_volume` and `s3` declare
  `bounded_listing: true`; **`sftp` does not**. A `kopia` set pointed at
  an SFTP source passes configuration validation and is then refused at
  run time, before any tree is walked:

  ```text
  backend: this backend cannot list a directory in bounded memory: "sftp". Back this source up from an explicit path list instead
  ```

  That refusal is the capability matrix working as designed: silence is
  never a default, and a listing this process cannot bound is one it will
  not attempt on a NAS. **Remote-pull sources, SFTP included, are the
  `artifact` engine's job** — pulling a producer's finished files off
  another machine is what that engine is for, and it is not a downgrade
  to use it. The incremental engine is for a tree this deployment can
  walk: a local volume, a mounted export, an object-store prefix.
- **A repository's storage is local, under the backup root.** The adapter
  speaks S3 and is tested against it; `config.yaml` has no field to place
  a domain anywhere else.
- **No verb runs maintenance.** The decision is reported; the pass is not
  scheduled.
- **No route creates a repository domain.** `config.yaml` is where a
  domain is declared.
- **No engine change in place, and no automatic migration.** An
  `artifact` set is never converted. See [the migration
  runbook](incremental-runbooks.md#runbook-1-moving-a-source-onto-the-incremental-engine).
- **Restore is local.** `snapshot restore` writes onto a directory the
  deployment can reach. There is no direct restore back to a remote
  source, deliberately.
- **The `retnd snapshot retention` preview opens the repository** before
  it decides anything, so against a domain whose repository has never
  been created it fails rather than reporting an empty plan. `repository
  health` names that condition precisely; use it first.

## The decisions behind all of this

| ADR | What it settles |
| --- | --- |
| [0006](adr/0006-embed-kopia-behind-backupengine-adapter.md) | embedding the engine behind an adapter rather than shelling out or forking |
| [0007](adr/0007-rclone-streaming-source-to-kopia.md) | how a source's bytes reach the engine |
| [0008](adr/0008-bounded-source-enumeration-and-capability-matrix.md) | bounded enumeration, and a capability matrix that refuses what it cannot bound |
| [0009](adr/0009-source-consistency-and-metadata-trust-model.md) | consistency modes, metadata trust, and the capture reader |
| [0010](adr/0010-backup-engine-and-repository-domain-models.md) | the engine and repository-domain domain models |
| [0011](adr/0011-kopia-repository-adapter.md) | the repository adapter |
| [0012](adr/0012-production-source-adapter.md) | the production source adapter |
| [0013](adr/0013-snapshot-lifecycle-catalog-reconciliation.md) | the snapshot lifecycle, catalog and crash reconciliation |
| [0014](adr/0014-verification-levels-and-adversarial-matrix.md) | the verification ladder |
| [0015](adr/0015-gfs-retention-holds-lkg-for-snapshots.md) | retention, holds and last-known-good for snapshots |
| [0016](adr/0016-local-snapshot-restore.md) | local snapshot restore |
| [0017](adr/0017-safe-repository-maintenance.md) | one maintenance owner per domain, and the margins left alone |
| [0018](adr/0018-shipping-the-incremental-engine-behind-a-gate.md) | shipping all of it behind a production gate |

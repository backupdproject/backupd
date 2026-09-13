# ADR 0018: Shipping the incremental engine behind a production gate

- Status: accepted
- Date: 2026-09-13
- Supersedes: nothing. This is the capstone of EPIC K (#779) and the
  record of what its eleven preceding decisions add up to: ADRs
  0006-0017, issues #780-#789.

## Context

EPIC K put a second backup engine inside a product that already had one.
The preceding ADRs each settled one question and each was taken on its
own evidence; this one answers the question none of them could, which is
**how a feature of this size becomes something a deployment already
carrying somebody's only backups is allowed to receive**.

The engine, as of #788, is complete enough to be dangerous. It reads
sources, creates and opens encrypted repositories, walks trees in bounded
memory, stores deduplicated snapshots, verifies them at four depths,
retains them under the product's existing GFS chain, holds them, restores
them, reconciles a crash against two durable stores, and reports all of
it on two operator surfaces. Nothing gated any of it. A deployment
upgrading to this build acquired every one of those code paths, live, on
a restart it performed for an unrelated reason.

Three things follow, and they are what this ADR decides.

### The risk is not the engine's correctness, it is its reach

Every individual mechanism has evidence behind it. What has no evidence
is the interaction between a new engine and a specific operator's
specific NAS: a share that sleeps, a clock that drifts, a filesystem that
fills, a scanner that walks the backup root, an SMB export that now
contains a few thousand pack files. The artifact pipeline's behaviour in
those conditions is known because it has been run in them for a long
time. The incremental engine's is not.

A feature flag does not make an engine correct. What it buys is that the
blast radius of being wrong is a deployment that has not enabled it,
which is every deployment until somebody decides otherwise.

### "Off" has to mean off without meaning broken

The obvious implementation of a kill switch — refuse to load a
configuration that mentions the gated feature — is the wrong one here,
and it is wrong in the worst direction. A deployment that has to disable
the engine is a deployment that is already having a bad day: a repository
will not open, a maintenance pass is suspected, a release is being rolled
back. If disabling the engine also stops the daemon loading, the operator
is choosing between the engine and *the rest of their backups*, at
exactly the moment they can least afford to.

### The two engines must not become one code path with a branch in it

The strongest safety property available is not a flag at all. It is that
the artifact pipeline does not consult the incremental engine's
configuration, its gate, its repositories or its catalog — so "does the
gate change anything for an artifact set" has the answer "there is
nothing for it to change", rather than "no, we checked".

## Decision

### 1. One flag, resolved in one place, defaulting to off

```yaml
incremental_engine:
  enabled: true
```

`config.Config.IncrementalEngine.Enabled`, with
`BACKUPD_INCREMENTAL_ENGINE` overriding the file **in both directions**
(`1|true|yes|on` enables, `0|false|no|off` disables, unset or empty
defers to the file), resolved by `config.IncrementalEngineEnabled()` and
nowhere else.

Both directions, rather than an enable-only override, because the two
cases are symmetric and both are real: a fleet rollout that turns the
engine on without editing every config file, and an incident that turns
it off without editing any.

The default is **off**. A feature that arrives enabled is a feature
somebody received rather than chose, and an absent block is the
configuration of every deployment that predates the engine.

An unparseable value is **refused at validation**, naming the variable
and the spellings it accepts, rather than falling back to off. A typo in
the switch that decides whether backups happen must not silently resolve
to the answer that means they do not — the operator who wrote `TRUE` in a
spelling this code does not accept believes the engine is running.

### 2. What is gated is the engine's own paths, and nothing else

Gated: creating or updating a set with `engine: kopia`, the incremental
snapshot cycle, verification, maintenance, snapshot restore, and the
repository probes and their alerts.

Not gated: loading and validating a configuration that already declares
`kopia` sets; the artifact pipeline in its entirety; and
`backup-set test-connection` on either engine, including the #852 source
write probe, which is a fact about a source's credentials rather than
about the engine and which an artifact set needs proven whether or not
the incremental engine exists anywhere.

A configuration holding `kopia` sets therefore loads with the gate off,
backs up every artifact set on schedule, and reports the refusal for the
incremental ones. That asymmetry is the decision, not an implementation
convenience.

### 3. One refusal sentence, from one place, and never a silent skip

`config.ErrIncrementalEngineDisabled`, rendered from
`config.IncrementalEngineDisabledMessage()`, surfacing as HTTP **409**
`INCREMENTAL_ENGINE_DISABLED` and CLI exit `1`. The sentence names both
ways to enable it and states that artifact sets are unaffected, because
the operator reading it is usually asking both questions at once.

A gated **restore** refuses. It does not skip, it does not succeed
having written nothing, and it does not report a partial success. A
restore that reported success and produced no data is the single failure
mode a backup product may never have, and a feature flag is not an
excuse to introduce one.

### 4. The gate is the release criterion, not a substitute for evidence

The flag does not stand in for the epic's other acceptance criteria and
did not shorten them. #789 also required, and got: the end-to-end
scenario as an automated suite, the scale benchmarks, a clean-environment
restore, credential-absence assertions across logs, API, metrics, catalog
and errors, a compliance and SBOM pass over the engine's full transitive
dependency tree, and documentation that explains **scanning versus
incremental storage** in terms an operator can check against their own
run reports.

That last one is a release criterion rather than a nicety, and it is why
[`docs/incremental-engine.md`](../incremental-engine.md) exists as a
document rather than a paragraph in the README. The engine's single most
misreadable property is that it scans everything every run and stores
almost nothing — five separate measurements, no total, and a null that
means *nobody measured this* rather than zero. An operator who believes
`logical_bytes` is an upload figure will conclude their deduplicating
repository is growing by the size of their source every night, and will
be wrong in a direction that costs them either money or trust.

## Consequences

- A deployment upgrading to this build acquires no new behaviour until it
  writes two lines. That is the intended outcome and the reason the
  default is off.
- Enabling the engine is reversible at any point before the artifact set
  beside it is retired, and the rollback is one flag.
- The gate adds one resolution function and one sentinel. It does not add
  a second configuration surface, a per-set override, or a staged rollout
  mechanism: a per-set flag would mean a set whose engine is `kopia` and
  whose gate is off, which is a state with no useful meaning that every
  reader would have to reason about.
- The engine is **not** the product's default and this ADR does not
  propose a date on which it becomes one. That decision needs field
  evidence from deployments that chose it, which is precisely what a
  default would prevent this project from ever collecting.
- Three limitations ship documented rather than hidden, because a flag
  that is on does not make them go away: an incremental set's source must
  be a bounded-listing backend (`local_volume`, `s3`; **not** `sftp`),
  every repository domain's storage is local under the backup root, and
  no verb or schedule runs maintenance — the surfaces report its
  decision. See
  [`docs/incremental-engine.md`](../incremental-engine.md#what-this-build-does-not-do).

## What EPIC K decided, in one place

| ADR | Issue | The decision |
| --- | --- | --- |
| [0006](0006-embed-kopia-behind-backupengine-adapter.md) | Phase 0 | embed the engine behind a `backupengine` adapter: no fork, no CLI, no vendor vocabulary reaching an operator |
| [0007](0007-rclone-streaming-source-to-kopia.md) | Phase 0 | an rclone stream reaches the engine as a streaming file, with no staging and no fake `Seek` |
| [0008](0008-bounded-source-enumeration-and-capability-matrix.md) | #792 (K0.3) | enumerate in bounded memory, and refuse a backend whose listing cannot be bounded |
| [0009](0009-source-consistency-and-metadata-trust-model.md) | #793 (K0.4) | consistency modes are declared, metadata trust is derived, and no policy skips content forever |
| [0010](0010-backup-engine-and-repository-domain-models.md) | #780 (K1) | the engine and the repository domain as domain models, with isolation stated rather than defaulted |
| [0011](0011-kopia-repository-adapter.md) | #781 (K2) | the repository adapter (local and native S3), its reserved namespace and its encryption requirement |
| [0012](0012-production-source-adapter.md) | #782 (K3) | the production source adapter, with the reading safety the transport already owned |
| [0013](0013-snapshot-lifecycle-catalog-reconciliation.md) | #783 (K4) | a strict durable state machine, a snapshot catalog, and one reconciliation rule per crash boundary — nothing deletes |
| [0014](0014-verification-levels-and-adversarial-matrix.md) | #784 (K5) | four verification rungs, and achieved reported separately from asked-for |
| [0015](0015-gfs-retention-holds-lkg-for-snapshots.md) | #785 (K6) | one retention engine, reached through a projection; holds as durable rows; last-known-good never deleted |
| [0017](0017-safe-repository-maintenance.md) | #786 (K7) | one maintenance owner per domain, an exclusive fence, and the vendor's safety margins left alone |
| [0016](0016-local-snapshot-restore.md) | #787 (K8) | restore is local, names a manifest rather than a run, and answers conflicts three ways |
| — (design record: [788-incremental-ui-mockup](../design/788-incremental-ui-mockup.md)) | #788 (K9) | the operator surface: six read routes, four snapshot actions on `POST /operations`, and no Kopia namespace anywhere |
| 0018 (this) | #789 | ship all of it off by default, behind one flag, with the artifact pipeline provably untouched |

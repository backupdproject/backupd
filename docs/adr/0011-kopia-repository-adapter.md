# ADR 0011: Where an embedded-engine repository lives, how it is unlocked, and what it has to prove before it is used

## Status

Accepted as K2 (#781) of EPIC K Phase 1. Implemented in
`core/internal/backupengine` (`engine.go`'s repository location and reports,
`reserved.go`, `maintenanceowner.go`), its single adapter
(`core/internal/backupengine/kopia/repository.go`), and the new secret
resolver `core/internal/secretref`. Proved locally by
`core/internal/backupengine/kopia`'s own tests and against a real S3 API by
`core/tests/miniointegration/kopiarepository_test.go`.

It stands on ADR 0006 (the embedded engine lives behind
`core/internal/backupengine` and exactly one adapter package imports it) and
ADR 0010 (a Repository Domain is the security boundary; where its bytes live
is this ADR's business, not the model's).

## Context

The engine embedded by ADR 0006 was proved against one storage backend, a
local directory chosen by the caller, with a passphrase carried as a plain
string on the location struct. That is enough for a feasibility spike and
not enough for a product. Four things were missing, and each of them is a
decision rather than an implementation detail.

1. **A repository has to have a name that survives a restart.** A location
   identified by its directory is identified by a mount point, and mount
   points move: a NAS replaced, a volume restored, a share remounted
   elsewhere. Everything downstream — the catalog's snapshot ids,
   maintenance ownership, retention — refers to a repository that has to
   still be the same repository tomorrow.

2. **A repository in a backup root is a few thousand files that look like
   nothing.** A backup root is populated, exported over SMB or AFP, walked
   by discovery, recorded by a catalog and pruned by retention. Pack and
   index blobs sitting in it have no sidecar manifest and no catalog row,
   which makes them candidates for adoption, for quarantine, or for
   deletion. The likely outcome is not an error: it is a prune that removes
   pack files and a repository that cannot restore anything, found out
   about later.

3. **"S3-compatible" is a marketing claim.** The embedded engine's own
   storage contract requires read-after-write on GETs *and on listings*,
   atomic whole-blob writes, honest range reads, and timestamps that do not
   run backwards. A number of products answering S3 on a port provide some
   of those. The ones they most often miss are the ones whose absence
   cannot be recovered from: an index blob that is not yet visible in a
   listing is not an error, it is content the repository has forgotten.

4. **Two secrets now have to be inside this process.** A repository
   passphrase and, for a bucket, object-store credentials. The medium plane
   (`internal/transport/rclone`) avoids this by handing rclone the
   credentials *file* so the material never enters this program's memory.
   The repository plane cannot: the native providers take a passphrase and
   an access key as parameters, and there is no subprocess to hand a path
   to.

## Decision

### A repository is identified by its Repository Domain, and its storage is derived

`RepositoryLocation` carries `Domain` (ADR 0010's
`model.RepositoryDomainID`), a `Root` for local repositories or an
`S3Storage` for a bucket, a `StateDir` for this process's own local state,
and two secret references. It no longer carries a repository directory, a
config file path or a cache path: all three are derived from the domain id.

Deriving rather than carrying removes a whole failure class at its source.
The config file is named after the domain, so two repositories cannot share
connection state by accident; the storage directory is named after the
domain, so two domains cannot share a directory and overwrite each other's
format blob; and reopening after a restart needs nothing but the two things
an operator declared, which is what
`TestRepositoryReopensByStableIdAfterRestart` proves by deleting every byte
of local state before the second open.

The identity check survives this and is not redundant. The case it catches
is now the realistic one: the backup root moved, the domain id did not, so
both locations resolve to one config file, and the file's contents describe
the old root. `TestOpenRepositoryConnectsTheRequestedStorage` constructs
exactly that.

### Local repositories live in a reserved namespace, and reserved means a predicate

`<backup-root>/.backupd/repositories/<domain>/` holds blobs;
`<backup-root>/.backupd/state/` holds this manager's own local state about
them. Both are under one reserved directory, and the rule is not the dot
prefix — the dot prefix is politeness towards an operator browsing the
share. The rule is `backupengine.LocalPathIsReserved`, one predicate,
compared as paths and never as strings, that artifact management consults
and that is never true for an artifact.

The test that matters is not that the predicate returns the right answers
for a list of paths, though it does. It is
`TestReservedNamespaceIsInvisibleToArtifactManagement`, which stands up a
real repository with real pack and index blobs in a backup root that also
holds a real artifact and its sidecar, and then walks the entire root
asserting that *every* file is either that artifact or reserved. A future
change that writes a lock file, a log or a cache beside the artifacts fails
there, which is the only place it would fail before a prune deleted it.

### Native providers only, and the target is probed before it is trusted

Two backends are registered: filesystem and native S3. The vendor also ships
a provider that proxies to rclone, and registering it would have made every
backend rclone speaks available for one import line. It is refused. rclone
stays on the source side, where a wrong answer is a failed read; a
repository built on storage that does not meet the contract above does not
fail, it loses content.

Inside the S3 backend the same scepticism is applied to the endpoint rather
than to the client. `probeStorage` writes one blob to a reserved blob-id
prefix and checks, in order: that it can be read back whole, that a range
read at a non-zero offset returns *that range*, that a listing shows it
immediately, that a delete removes it, that the listing then agrees, and
that the storage reported a timestamp for it.

It runs at create time, before the repository format is written, so an
unsupported target is left as an empty bucket and an explicit
`ErrStorageUnsupported` naming the missing property — not as a half-usable
repository discovered during a restore.

At create time, and for a bucket only, it also writes a **21 MiB** blob and
compares it back byte for byte. A 256-byte PUT proves very little about a
bucket that will be asked to hold 20 MiB pack blobs (the vendor's
`MaxPackSize` default), and the class of endpoint that answers 200 to
everything while mangling large bodies — a mishandled chunked-signature
stream, a proxy with a body limit, a gateway that truncates — is precisely
the class whose damage is silent. Note what this is *not*: the vendor sets
`DisableMultipart` on every `PutObject`
(`repo/blob/s3/s3_storage.go`), so nothing this product writes is split
into parts at this pin. The probe size is above minio-go's 16 MiB part
size anyway, so it covers that path too if a future version drops the flag.

The cheap properties also run on every `Health` call, deliberately: a
bucket policy that starts denying deletes, or a filesystem that fills up,
is as interesting as a target that never had the property. The 21 MiB
write does not, and that is a cost decision stated once — health runs
before every backup, and 21 MiB per preflight would make the preflight the
most expensive thing in a cycle on a domestic uplink.

The faults are injected in unit tests (`probe_internal_test.go`), one
property at a time, because the endpoints worth refusing are the ones
nobody has in a test rig — including a storage that stores small blobs
faithfully and corrupts every large one, which passes the cheap probe and
is refused by the create-time one. The MinIO run proves the other half: a
conforming endpoint passes, over the wire, with signed requests and
21 MiB single-PUT bodies.

### Clock skew is a warning, and time synchronisation is not this product's job

Almost everything a repository does about concurrency and reclamation is
expressed in timestamps: a maintenance lock is held until a time, recently
written content is too new to garbage-collect until a time, and a
snapshot's own start time is what retention later reasons about. A clock an
hour fast can make one process treat another's live lock as expired; an
hour slow can make fresh content look old enough to reclaim. Neither
produces an error when it happens.

So the probe's timestamp — which for S3 is the server's own
`Last-Modified`, a genuine comparison against another machine's clock — is
compared against this host's, and a disagreement over five minutes is a
`HealthWarningClockSkew` warning with the measurement in it.

It is a warning and never a refusal. A product that stops backing up
because an NTP server was unreachable has traded a risk for a certainty.
Fixing the clock is the operating system's job, and this ADR does not
propose implementing time synchronisation.

### Both secrets are references, resolved as late as possible

`RepositoryLocation.Passphrase` and `S3Storage.Credentials` are
`secretref.Ref`: a file, an environment variable, or a command whose stdout
is the material. There is no field a literal secret fits into, which is the
enforcement rather than a preference — a test that wanted to shortcut would
have to add one.

A `Ref` is therefore safe to render into an error and safe to keep for the
life of an open repository handle, which is what lets `Health` reach the
storage again without being handed the credentials a second time. The
material is resolved at the moment a provider is built and is not retained
by the adapter; buffers holding it are zeroed as far as Go allows.

It is safe to LOG on every rendering path too, and that took four methods
rather than one. `String()` is honoured by `fmt`, and by nothing
else: `log/slog`'s JSON handler ignores `Stringer` entirely and reflects
over exported fields, so a `Ref` logged as a structured attribute printed
its whole `Command` argv — the vault path, the role and whatever token the
operator's resolver takes. `encoding/json` did the same to anything a
`Ref` is a field of, and `%#v` to a debug print. So `Ref` now has
`LogValue`, `MarshalText`, `MarshalJSON` and `GoString`, all rendering
exactly what `String()` does, and deliberately no `UnmarshalJSON`: a `Ref`
must not be parseable back out of JSON, because that would be a second,
undocumented way to declare a secret source.

A `file` source is also checked for custody before it is read, which is
the rule `internal/transport/rclone` already applies to a credentials file
and an ssh key: a secret file readable by any account but its owner,
sitting in a directory another account can write, reached through a
symbolic link, or not a regular file at all, is refused with `ErrCustody`
rather than read. The mode and the file type are checked on the open
descriptor rather than on the path, so nothing can be swapped between the
check and the read, and the open uses `O_NONBLOCK` so that a fifo left
where the file should be is refused instead of blocking the process
forever.

`core/internal/secretref` is a new package, and the honest account of why
is this: the *declaration* is not new — `config.Passphrase`,
`config.MediumCredentials` and `transport.MediumCredentials` are the same
three fields, and `TestRefFieldSetMatchesTheConfiguredOnes` fails if any of
them grows a fourth, because a source an operator can write and this
resolver cannot read is what would make "one custody model" untrue. The
*mechanism* is duplicated: `internal/transport/rclone` resolves its own
copies privately, in a shape built around rclone's configmap, and unifying
the two means refactoring that package's most security-sensitive file.
That is tracked separately and is not something to attempt sideways from a
repository adapter.

### What the state directory holds, and what it must never hold

`<StateDir>/<domain>.config` is the vendor's `LocalConfig`: which storage
this repository is connected to, plus client options. For a bucket
repository the storage half used to be the vendor's own `s3.Options`, and
`s3.Options` carries `AccessKeyID`, `SecretAccessKey` and `SessionToken`
with ordinary `json` tags — the `kopia:"sensitive"` tag on them is a
CLI-display hint and nothing more. So connecting wrote the operator's
resolved cloud credentials to the backup host in cleartext and left them
there, which defeats the whole of the section above: an operator who kept
the secret in a 0600 file or behind a Vault command got a plaintext copy
anyway, made by the program they were trusting not to do that.

So this adapter registers its **own** storage type, `backupd-s3`
(`kopia/s3connection.go`), and that is what gets persisted. Its config
holds the bucket, the key prefix, the endpoint, the region, the TLS
decision and an operator's `RootCA` — all coordinates, none of them
resolvable — plus a credential *handle*: a 16-byte nonce naming an
in-memory registration of the operator's `secretref.Ref`, valid only
inside this process and released when the repository handle closes. Every
`repo.Open` therefore resolves the credential from the declared source
again, at the moment the storage is built.

The handle is a nonce and not the `Ref` itself for two reasons. A `Ref`
names an executable to run, so persisting one would turn a state file into
a choice of program this daemon then executes; and it would put the
operator's resolver argv on disk, which is exactly what the logging
methods above refuse to render into a log line.

What this costs is stated plainly: a config file alone cannot reopen a
bucket repository in a new process. That costs nothing, because
`OpenRepository` always reconnects before it opens — the config it reads is
always one the same process just wrote, naming a registration that process
holds — and a stale config from a previous run is overwritten before it is
read.

The state directory is 0700 and the config file 0600, asserted by this
adapter rather than inherited from the vendor's defaults. The regression
test reads the *bytes* of every file under the state directory and fails on
any part of the credential or the passphrase, and separately fails if the
persisted config has a field named after one of the vendor's three secret
options — the byte assertion is the one that survives a vendor upgrade
that renames a field.

`Close` does not disconnect, and with the credentials gone that is no
longer a custody question. It is a correctness one: the vendor's
`Disconnect` deletes the maintenance lock file it keeps beside the config
(`<config>.mlock`) along with the config itself, and maintenance ownership
is state this product means to keep across a restart. What is left behind
is coordinates and a dead nonce.

So: `<StateDir>` holds where a repository is, what this process calls it,
and a nonce. It holds no credential material, no resolver argv, no path to
either, and (see the caching consequence below) no repository content.

### Maintenance ownership is recorded now and decided later

A repository has exactly one maintenance owner; two processes reclaiming
space concurrently is how a repository loses content it still references.
The adapter already overrides the vendor's own owner check (deferring to
whichever machine created the repository means a maintenance window that
silently never runs), so this product owes the same guarantee from its own
side, and that needs a durable record: repository, owner, last quick, last
full, next eligible, last result.

`backupengine.MaintenanceOwnership` and a file-backed store are that record
and nothing more. There is deliberately no method that decides whether
maintenance may run: `NextEligible` is a recorded fact, nothing here reads
the clock to compare against it, and a half-made scheduling decision in
this file would be a second scheduler for #786 to contend with rather than
a foundation to build on. `Load` distinguishes "no record" (nobody has
owned this yet; claim it) from "unreadable record" (something is wrong;
absolutely do not claim it), because reading the second as the first is how
two instances both decide they own maintenance.

### S3 test fixture: why MinIO, not kumo

AWS-service simulation in this repository is otherwise the kumo emulator
(`ghcr.io/sivchari/kumo`), and this suite is a deliberate, recorded
exception rather than an oversight.

kumo cannot back a Kopia repository. It accepts an AWS SigV4 **streaming**
upload (`x-amz-content-sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, an
`aws-chunked` body) with a 200 and then stores the chunk-framed request
body verbatim instead of decoding the framing, so every object arrives
corrupt. Measured through this adapter's own capability probe:

| written | read back |
| --- | --- |
| 1 byte | 173 bytes |
| 256 bytes | 430 bytes |

A constant +172 bytes, which is exactly the framing: a chunk header
(`100;chunk-signature=` plus a 64-character signature), a CRLF, a
terminating zero-length chunk, and a final CRLF. An **unsigned** `curl`
PUT to the same server round-trips byte-identically, which places the
fault on the signed streaming path rather than on storage. Streaming
signatures are minio-go's default for a plaintext endpoint, and minio-go
is the client inside the vendor's native S3 provider, so this is the
default path for every write this product would make.

The probe refuses it with `ErrStorageUnsupported` and the message
"returned 430 bytes that are not the 256 that were just written to it".
That is the probe doing exactly what the section above builds it for, and it is
the reason the probe is not negotiable: weakening it so a convenient
fixture passes would delete the only check that catches silent write
corruption.

**The rule this sets:** an S3 emulator is chosen by fidelity to the exact
wire protocol the client speaks — `aws-chunked` streaming uploads, range
reads, listing consistency, server timestamps — and never by how little it
costs to start. A fixture that is wrong about the protocol does not make a
client test easier, it makes it meaningless. MinIO is used here because it
is right about the protocol, and it runs as an **ephemeral container,
started per run and torn down per run including on the failure path**: the
teardown is registered as soon as the container exists and before
readiness is waited on, so a server that never comes up is still removed,
and the container carries the `tests/dockerlease` label so a process that
is killed outright is swept later.

**What this costs, stated rather than papered over:** kumo does not
authenticate requests at all, and MinIO does, so a refusal that depends on
the endpoint rejecting a signature is provable on MinIO and would not have
been on kumo. The coverage that matters either way is the refusal this
product owns: a credential source that does not contain
shared-credentials text is refused *before* a request is made, naming the
file and echoing none of the material, because the alternative is an
`AccessDenied` nobody can trace back to a bad resolver. Both are in the
S3 matrix.

The defect is worth reporting upstream and is not this repository's to
fix; the reproduction above is byte-exact and does not depend on anything
in this tree.

## Consequences

- `Repository` gains `LookupSnapshot`, `Health` and `Stats`. Lookup exists
  because the catalog stores an id and later has to ask what became of it;
  answering that by listing a source's snapshots costs a manifest load
  each and cannot answer for a source whose identity has since changed.
  Health and Stats are separate because their costs differ by orders of
  magnitude: health is a cheap preflight, and stats walks the storage's
  blob listing.
- `RepositoryStats` reports PHYSICAL numbers, read from the storage's own
  listing rather than the repository index, because the question is what
  the repository costs and the answer includes blobs an interrupted
  maintenance left behind.
- `RepositoryStats.Sources` is the co-tenancy number, and it counts
  distinct **backup-set tags** (`backupengine.TagKeyBackupSet`, the literal
  `backupd.set`, set by the sink that writes the snapshot) rather than the
  engine's own sources. A streaming backup set writes one engine source
  *per object*, so a count of those would report forty co-tenants for one
  set of forty database dumps, and two single-object sets would report two
  and be indistinguishable from the breach the number exists to find.
  Snapshots with no set tag — which this product does not write, and an
  operator's own use of the vendor's CLI against the same bucket would —
  count as one unattributed tenant between them, because content sharing a
  domain's key that nothing can attribute is still content sharing the
  key. This is the contents-level half of ADR 0010's isolation promise: the
  model decides what *may* share a domain, and only this number can find a
  boundary that has already been crossed.
- `Health` returns a populated report *and* an error when a repository is
  unreachable, rather than an error alone. The error is for the caller that
  must refuse to start a backup; the report is for the caller that must
  show an operator a status, and "unreachable" and "healthy" have to be
  distinguishable without parsing an error string. Every probe failure is
  `Reachable: false` plus a `HealthWarningUnreachable` carrying the
  measurement — including the failures where the endpoint answered, because
  storage that accepts a blob and then does not list it is not reachable
  for a repository's purposes.
- **Local caching is off, deliberately.** The spike set a `CacheDirectory`
  and no sizes, which was dead code: the vendor short-circuits on
  `ContentCacheSizeBytes == 0` and writes an empty `CachingOptions`. The
  obvious repair — set the sizes — was made, measured and reverted, because
  the vendor's caching switch is all-or-nothing and with the content cache
  on, a repository missing a pack blob VERIFIES CLEAN: the verifier reads
  the content out of `<StateDir>/<domain>.cache/contents` while the bucket
  no longer holds it. Verification failed again only once the cache
  directory was deleted. That is disqualifying for a product whose claim is
  restorability, and the case where the cache is warmest is
  verify-immediately-after-backup, so caching would be most likely to lie
  in exactly the flow that matters most; a restore served from cache proves
  a restore nobody can repeat. The cost is that every content, metadata and
  index read goes to the storage. Re-enabling it needs a read path that
  bypasses the cache for verification and restore, which the vendor does
  not offer at this pin.
  `TestVerifyReportsDamageAsBothErrorAndFindings` is the guard on this
  decision: it fails if caching is turned back on.
- Space reclamation is still not observable immediately after maintenance,
  and the S3 matrix says so rather than asserting otherwise. Maintenance
  runs at full safety, which keeps recently written content out of garbage
  collection so that it is safe to run while a snapshot is in progress;
  every blob in a test's repository is seconds old.
- Configuration does not yet name a repository's storage. An operator
  cannot point a Repository Domain at a bucket from `config.yaml` until the
  wiring issue lands; `S3Storage` is deliberately the same set of facts
  `config.StorageMedium` already collects, so that wiring is a mapping and
  not a second vocabulary.

## Alternatives considered

**Let the operator choose the repository directory.** Rejected on the
strength of consequence 2 in the context: the directory an operator would
choose is inside the backup root, which is the one place it must not be,
and a warning in a doc comment is not a mechanism. Deriving the path also
made the config-file collision impossible by construction rather than
merely checked.

**Use the vendor's rclone-backed repository provider.** Rejected. It would
have delivered every backend rclone speaks for one import line, and it
would have made the storage contract unenforceable: an rclone remote
provides whatever its own backend provides, and the properties a repository
needs are exactly the ones a file-copy tool has no reason to guarantee.

**Trust the endpoint and report failures as they arrive.** Rejected. The
failures that matter do not arrive as failures. A listing that is
eventually consistent returns a successful, short listing, and the
repository proceeds on it.

**Resolve secrets once at startup and hold them.** Rejected: it maximises
exactly the window this design is trying to minimise, and it would have put
material on a struct that is otherwise safe to log, which is the property
every leak assertion in this change depends on.

**Refuse to open a repository whose clock is skewed.** Rejected. See the
decision: the correct response to "NTP is broken" is not "stop backing up".

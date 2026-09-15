# Incremental-engine runbooks

Four procedures for the incremental engine: putting a source on it,
recovering a repository that will not open or has lost content,
recovering a credential, and dealing with the production gate refusing.

The engine itself — what it does, what it measures, what it promises — is
[`docs/incremental-engine.md`](incremental-engine.md). The artifact
pipeline's own 3am document, the one about a backup that did not arrive,
is [`docs/recovery.md`](recovery.md) and is unaffected by anything here.

- [Runbook 1: moving a source onto the incremental engine](#runbook-1-moving-a-source-onto-the-incremental-engine)
- [Runbook 2: a repository that will not open, or has lost content](#runbook-2-a-repository-that-will-not-open-or-has-lost-content)
- [Runbook 3: credential recovery](#runbook-3-credential-recovery)
- [Runbook 4: the engine is refusing because the gate is off](#runbook-4-the-engine-is-refusing-because-the-gate-is-off)

---

## Runbook 1: moving a source onto the incremental engine

### The one thing to read before anything else

**retnd never converts an `artifact` backup set into a `kopia` one.**
Not on upgrade, not on a configuration reload, not when the incremental
engine is enabled, not as a convenience, and there is no flag that asks
it to. An existing artifact set keeps its engine, its artifacts, its
retention verdicts and its journal rows forever.

There are three reasons, and each of them on its own is sufficient:

1. **The two engines identify what they store in incompatible ways.** An
   artifact is identified by backup set plus remote basename; a snapshot
   is identified by the set's `uuid` lineage plus an opaque manifest id.
   A set that changed engine in place would abandon everything it had
   already stored under an identity nothing would ever look for again.
2. **They read different sources.** The artifact engine discovers
   finished files a producer wrote. The incremental engine walks a tree.
   The same `remote_path` does not necessarily mean the same thing to
   both.
3. **A conversion would be a migration of somebody's only copy,
   performed by a process nobody was watching.** If it went wrong it
   would go wrong at the one moment nobody is looking: on a restart, on
   an upgrade, at 4am.

So moving a source onto the incremental engine is **a new backup set
that you create, running beside the old one until you are satisfied**.
That is the whole procedure below, and every step of it is reversible
until the last one.

### Step 0 — check the source can be snapshotted at all

An incremental set's source must be a backend whose directory listing
this process can bound: **`local_volume` or `s3`**. `sftp` cannot be, and
a `kopia` set pointed at an SFTP source will pass configuration
validation and then fail every cycle with

```text
backend: this backend cannot list a directory in bounded memory: "sftp". Back this source up from an explicit path list instead
```

If the source is a directory tree on another machine reached over SSH,
**stop here**: that is the artifact engine's job, and it is what the
existing set is already doing correctly. Mount the tree locally (NFS,
SMB, a bind mount into the container) if you want it snapshotted, and
point the incremental set at the mount.

### Step 1 — turn the gate on

The incremental engine is off by default. In `config.yaml`:

```yaml
incremental_engine:
  enabled: true
```

`RETND_INCREMENTAL_ENGINE=1` in the environment does the same and overrides
the file in both directions, but on the standard container deployment it is
for a command you launch yourself rather than for the long-running processes:
`container/compose.yaml` declares the environment it passes through by name
and does not pass this one, so `container/.env` is not a place to set it. See
[`docs/deployment.md`](deployment.md#the-incremental-engines-gate-and-where-to-set-it-in-a-container-deployment).
**On a compose deployment, put it in `config.yaml`.**

Leaving the gate off costs nothing until you get to step 4; with it off,
the create in step 4 is refused with `INCREMENTAL_ENGINE_DISABLED`.

### Step 2 — declare a repository domain, and back its passphrase up out of band

**Nothing writes `repository_domains` for you.** No CLI verb and no API
route creates a repository domain in this build; the web interface's
*Define a repository domain* screen states that on its face. Edit
`config.yaml`:

```yaml
repository_domains:
  - id: production
    description: Snapshots of the production upload tree
    isolation: shared
    passphrase:
      file: /etc/retnd/secrets/production.passphrase
```

- `isolation` is required and has no default. `shared` means every set in
  this domain shares one encryption key, one credential, one
  deduplication span, one maintenance owner and one corruption fate —
  which is where the storage saving comes from. `isolated` means it does
  not share, and forgoes that saving.
- `passphrase` is a **reference**: `file`, `env` or `command`. There is
  no field to paste a passphrase into.

Then, before the first run:

> **Copy the passphrase somewhere that is not this machine and is not
> inside the repository it unlocks.** A repository this product creates is
> always encrypted, and the passphrase is the only thing that opens it.
> Lose it and every snapshot in that domain is unrecoverable ciphertext —
> there is no escrow, no recovery key, no vendor with a copy, and no
> support procedure that gets it back. A password manager entry, a sealed
> envelope, your organisation's secrets manager: anything, as long as it
> survives the loss of the NAS.

Generate one that is worth protecting:

```bash
umask 077
head -c 32 /dev/urandom | base64 > /etc/retnd/secrets/production.passphrase
chmod 600 /etc/retnd/secrets/production.passphrase
```

If this deployment sets `key_encryption`, note that it protects secret
material **this manager writes** (an imported SSH key), not a passphrase
file you placed yourself — that file's protection is its filesystem
permissions and where you put it.

### Step 3 — have the serving process read the new configuration

There is no config watcher and no SIGHUP reload in this build. A change
you make in `config.yaml` by hand is read when a process **starts**. So
either make the edits while nothing is serving the deployment, or restart
the engine afterwards:

```bash
docker compose -p retnd ... restart retnd
```

Then confirm the domain is declared and see what the repository probe
says about it. A domain nothing has ever written to reports `FAILING`
with `reachable: false`, and that is the correct answer before the first
run: nothing has been created there, deliberately.

```bash
retnd repository health
```

### Step 4 — create the incremental set, beside the artifact one

A different set id. The old set keeps running.

```bash
retnd backup-set create production/uploads-tree \
  --engine kopia \
  --repository-domain production \
  --host nas-export.internal --user backup --remote-path /srv/uploads \
  --ssh-key-id <id> --known-hosts-line "<line>" \
  --source-consistency externally_quiesced \
  --verification-level content_sample --verification-sample-percent 10 \
  --verification-full-every 720h
```

Things that will refuse you, on purpose:

| Refusal | Why |
| --- | --- |
| `--local-path`, `--completion-strategy`, `--include`, `--validator-id` | artifact-only keys. A key that cannot ever be acted on is refused rather than ignored |
| no `--repository-domain` | required for an incremental set, and it has no default: which boundary a source's history is kept in has to be intentional |
| a `--repository-domain` nothing declares | the refusal names what *is* declared |
| `--engine` on `backup-set patch` | there is no such flag. The engine is chosen once |

The create proves the connection before it writes anything and is refused
when it cannot. `--no-verify` writes it anyway, says in so many words
that nothing was proven, and leaves the set marked unverified until a
connection test passes.

Pick `--source-consistency` honestly. `live_best_effort` is the default
and the weakest claim; `externally_quiesced` says you stop the writers
for the run; `external_snapshot` says the path is a frozen image. The
stronger two are worth declaring precisely because a run that observes
the source moving then reports it **against** your claim, which is how a
quiesce hook that stopped the wrong container gets found.

### Step 5 — first run, and where the bytes go

```bash
retnd fetch --backup-set production/uploads-tree
```

The first run that needs one **creates** the repository, at the location
derived from the declared domain, and only if the catalog holds no
snapshot this set ever committed. (Not "open failed with not-found, so
create one": an unmounted or sleeping share presents identically, and
creating a repository there would hand the set a fresh empty one on the
wrong filesystem, silently, until somebody needed a restore.)

It lands here:

```text
<backup_root>/.backupd/repositories/production/   the repository's blobs
<backup_root>/.backupd/state/                     connection config, caches, maintenance ownership
```

**Exclude `.backupd` from anything that walks the backup root** — an SMB
share, a virus scanner, a backup of the backup. Artifact management
already treats that path as reserved and will not enter it.

### Step 6 — prove it before you trust it

```bash
retnd snapshot list production/uploads-tree
retnd snapshot show production/uploads-tree <run-id>
retnd repository health
retnd snapshot verify production/uploads-tree --level content_full
```

What to look at, and what a good answer looks like:

- the run's phase is `SUCCESS`. Nothing else is a restore point — a
  committed manifest is not success.
- `source_complete` is true. Null means nobody recorded a verdict, which
  is not the same claim as "the pass was incomplete", and neither is a
  restore point.
- the five numbers make sense: `logical_bytes` is the size of the tree,
  `repository_bytes_written` is what actually grew the store. On a
  **second** run over an unchanged tree, `entries_scanned` should be
  roughly what it was — every run walks the whole tree — while
  `repository_bytes_written` collapses and `content_reused_bytes` takes
  up the slack. That contrast is the proof that the engine is storing
  incrementally rather than copying the source again.
- `not measured` anywhere is a counter nobody took, never a zero.

Then do the thing that actually matters, from a directory you can throw
away:

```bash
retnd snapshot restore production/uploads-tree --to /var/tmp/restore-drill
diff -r /var/tmp/restore-drill /srv/uploads | head
```

`--conflict` defaults to `refuse`, so the restore stops rather than
replacing anything already at the destination.

### Step 7 — run both sets until you believe it, then retire the old one

Keep the artifact set running for at least one full retention window, so
there is a period where both mechanisms hold the same data. When you are
satisfied:

```bash
retnd backup-set enabled production/uploads off     # stops the scheduler offering it; a pass inside it finishes
# ... leave it off for as long as you want the artifacts retained ...
retnd backup-set remove production/uploads          # configuration only
```

`remove` is configuration only. The artifacts it collected **stay on
storage and stay listed by `retnd artifacts`**, and creating the set
again with the same source and name takes them back. Until you prune
them, nothing has been given up.

### Rolling back

Before step 7, rollback is: `backup-set enabled <incremental-set> off`,
or turn the gate off entirely
(`RETND_INCREMENTAL_ENGINE=0`/`enabled: false`), which leaves the
artifact set backing up exactly as before. A configuration holding
incremental sets still loads with the gate off; only the incremental
sets report the refusal.

After step 7, rollback is re-creating the artifact set with the same
source and name, which takes its retained artifacts back.

---

## Runbook 2: a repository that will not open, or has lost content

### First: do not write to it, and do not reach for vendor tooling

Two instincts to resist.

**Do not run the vendor's CLI against the repository.** This product's
repository is a normal kopia repository, so somebody's own `kopia`
binary will happily open it — and a maintenance or `snapshot delete` run
from outside this deployment is a write nothing here fenced, against a
store this deployment's ownership record says it owns. That is how a
repository loses content it still references. An unknown snapshot
appearing in the repository is quarantined by this product, never
deleted, precisely because it cannot tell an operator's own vendor-tool
use from a co-tenant set from a crashed run.

**Do not "just let it re-create".** A repository is created only on a run
whose set has no committed snapshot in the catalog. If a mount is missing
and you delete the catalog to unstick it, you have converted a
recoverable mount problem into a fresh empty repository on the wrong
filesystem.

Take the pressure off first:

```bash
retnd backup-set enabled <source>/<set> off      # for every set in the affected domain
```

### Step 1 — get the precise condition, not "it's broken"

```bash
retnd repository health
```

Every probe is reported separately because the remedies are different.
Read the row, not the colour:

| What health says | What it means | What to do |
| --- | --- | --- |
| `reachable: false` | the storage could not be contacted at all | it is a mount, a path or a network. Nothing is wrong with the repository. Fix the mount and re-run health |
| `reachable: true`, `readable: false` | the storage answered and this deployment could not read the repository's own format metadata | **this is the shape an empty or wrongly-mounted location presents.** Check you are looking at the right filesystem before anything else. The report says so explicitly when it holds no repository: *the storage answered and holds no repository, which is what an empty or wrongly-mounted location looks like; nothing has been created here, deliberately* |
| `reachable: true`, `readable: true`, `credentials_valid: false` | the format blob was read and the key was refused | a rotated or mis-referenced secret. [Runbook 3](#runbook-3-credential-recovery) |
| `writable: false`, everything else true | this deployment cannot write there | a permission or a full filesystem. The probe writes in the reserved namespace and removes it again; it never infers writability from a read |
| `clock_sane: false`, negative `clock_skew_seconds` | this machine's clock is behind the newest timestamp already stored | **fix the clock before the next run.** New snapshots would be dated before older ones and retention would bucket them into the wrong periods |
| `DEGRADED` with `maintenance: overdue` | storage freed by deleted snapshots is still occupied | no restore point is affected. It is a schedule problem |
| `last verified` failed | at least one restore point could not be proven readable | continue below |

`FAILING` is reserved for a repository that cannot take a backup at all.
Overdue maintenance, a drifted clock and a failed verification are
`DEGRADED`: they need attention, not panic.

### Step 2 — find out what is actually damaged

```bash
retnd snapshot list <source>/<set>
```

Phases tell you where the damage is:

- **`LOST`** — the catalog names a manifest the repository does not have,
  on a run that had already reached `SUCCESS`. That is content gone, and
  it is the row to take seriously.
- **`FAILED` carrying a snapshot id** — a run whose source scan came back
  incomplete holds a well-formed manifest of a tree with a hole in it. It
  is kept, attributed and visible rather than deleted, because deleting
  it would destroy the only copy of what it does hold.
- **`QUARANTINED`** — a snapshot in the repository this deployment cannot
  attribute. Nothing deletes it.

One rule matters more than any row: **a pass that cannot read the
repository decides nothing.** An unreachable repository presents exactly
like one that has lost every snapshot in it, so this product refuses to
conclude anything from half the evidence. If every row went `LOST` at
once, suspect the mount, not the data.

Then prove what is left, deepest check first, on the snapshot you would
actually restore from:

```bash
retnd snapshot verify <source>/<set> --level content_full
```

It exits non-zero when it finds damage. A structural pass proves the
manifest and the structure resolve; only a content pass reads the bytes.

### Step 3 — protect what is still good, before anything else touches it

```bash
retnd snapshot hold <source>/<set> --reason "repository damage, incident <n>: do not let retention take this"
```

A hold stops retention deleting that snapshot whatever the tier chain
says, until somebody releases it. Place one on the newest snapshot that
verified, and on anything an investigation will want. `--reason` is
required and it is not bureaucracy: a hold nobody explained is one nobody
dares release, which makes it permanent by accident.

Check what is already held, and what retention currently thinks:

```bash
retnd snapshot holds <source>/<set>
retnd snapshot retention <source>/<set>      # a preview; it deletes nothing
```

A `REFUSE` verdict is the one to read: it means the snapshot was a delete
candidate and something stopped it, and it names what. Note that the
retention preview **opens the repository** before it decides anything, so
it fails outright against a repository that will not open — use
`repository health` for that condition, which names it precisely and
opens nothing.

### Step 4 — get the data out

```bash
retnd snapshot restore <source>/<set> --to /var/restore --conflict refuse
```

- Without `--snapshot` it restores the set's last known good snapshot,
  which is the only snapshot this product advertises as a restore point.
- `--snapshot` takes the **manifest** id, not a run id, on purpose: the
  repository decides whether the id names anything. That is what keeps a
  restore working when the journal has lost the row — which is exactly
  the situation somebody is restoring in.
- `--path` restores one path inside the snapshot.
- `--conflict refuse` (the default) stops rather than replacing anything.
  `skip` leaves what is there and restores the rest, which is what makes
  an interrupted restore resumable. `overwrite` replaces.

If the catalog itself is what you lost rather than the repository, the
snapshot rows are recovered by this product's own crash reconciliation
against the repository — a manifest the repository holds and the catalog
never recorded is **adopted onto the row and then verified**, never
deleted. `retnd catalog rebuild` is the artifact-side tool and
reconstructs artifact rows from sidecar recovery manifests; it is not how
snapshots come back.

### Step 5 — reclaim the storage, and only then re-enable

Retention deleting a manifest frees nothing on its own: the content
behind it stays in the packs until maintenance collects it. Check what
maintenance thinks, remembering that it opens nothing and so answers
while the repository is still unreachable:

```bash
retnd repository maintenance <domain>
```

`owner` is the load-bearing line. Exactly one instance may maintain a
domain: an unclaimed repository is claimed by the first instance that
maintains it, and one claimed by another instance is refused before the
repository is touched at all. If the owner is another instance, the
answer is that instance, not a flag.

This build schedules no maintenance and has no verb that starts one, so
there is nothing to run here and nothing to interrupt. What an
interrupted maintenance would have left is unreferenced blobs and nothing
else — it never removes referenced content. Its safety comes from time
rather than locking: content younger than 24 hours is not collected, two
garbage collections four hours apart must agree before deleted content
leaves the index, and an unreferenced pack blob is left alone for a day.
Those margins are not tunable from here and the vendor's mode that
removes them is not exposed.

When health is green and a `content_full` verification passes:

```bash
retnd backup-set enabled <source>/<set> on
```

### If the repository is genuinely unrecoverable

There is no repair tool, and this document will not invent one. The
honest sequence is:

1. Restore everything still restorable to somewhere else, snapshot by
   snapshot, holds placed first.
2. Leave the damaged repository in place, read-only, with its sets
   disabled. It costs storage and it is evidence.
3. Declare a **new** repository domain with a new id and a new
   passphrase, and create new backup sets pointing at it — the same
   procedure as [Runbook 1](#runbook-1-moving-a-source-onto-the-incremental-engine)
   from step 2. History does not migrate: the new domain starts empty,
   and the first run stores a full copy.
4. Keep the old domain until the new one has a retention window's worth
   of verified snapshots in it.

---

## Runbook 3: credential recovery

Four secrets can be in play. They fail differently and they recover
differently, so identify which one you have before doing anything.

| Symptom | Which secret |
| --- | --- |
| `repository health`: `reachable: true`, `readable: true`, `credentials_valid: false` | the **repository passphrase** |
| an imported SSH key cannot be decrypted on startup | the **`key_encryption`** key |
| `backup-set test-connection` fails at authentication | the source's **SSH key** |
| a storage medium's preflight fails to authenticate | an **S3 medium credential** |

### The repository passphrase

**If the passphrase is wrong but known, this is a configuration fix.**
The `passphrase` block is a reference, so correct the reference rather
than the secret: point `file` at the right path, fix the `env` variable
the process actually receives, or fix the `command` that prints it.
Restart the serving process (there is no reload), then:

```bash
retnd repository health
```

`credentials_valid: true` is the answer. Nothing needs rebuilding: the
repository was never damaged, it was never opened.

Common causes, in the order they actually happen: the file is outside
what the container mounts, so the process sees nothing; the file has a
trailing newline a generator added and the reference expects the byte-for-byte
content; the `command` form exits non-zero and its stdout is empty; the
secret was rotated in a secrets manager and the repository was created
with the old one.

**If the passphrase is genuinely lost, the snapshots are lost.** This is
not a limitation to be worked around and there is no procedure below this
line that recovers them. A repository this product creates is always
encrypted; there is no escrow, no recovery key, no second key slot, and
nobody holding a copy. The only remaining work is:

1. Do not delete the repository yet — if the passphrase turns up in
   somebody's password manager next week, it still opens.
2. Declare a new domain with a new id and a new passphrase, back that one
   up out of band *first*, and create new sets against it
   ([Runbook 1](#runbook-1-moving-a-source-onto-the-incremental-engine),
   from step 2).
3. Treat the artifact copies, if the source also has an artifact set, as
   the history that survived.

**Rotating a passphrase** is not something this build does. There is no
verb that re-keys an existing repository, and editing the reference to
point at a different secret does not re-key anything — it simply stops
the repository opening. If a passphrase must be considered compromised,
the procedure is a new domain and new sets, as above.

### The `key_encryption` key

`key_encryption` protects secret material this manager itself writes to
disk — today, an imported SSH private key under the configuration's
`ssh_keys/` directory. It is optional, and a deployment that never opted
in stores that key as a permission-protected plaintext file.

If the encryption key is unavailable, imported SSH keys cannot be
decrypted. The recovery is to make the key available again (fix the
`file`/`env`/`command` reference, exactly as above). If it is lost, the
imported SSH keys are unrecoverable and the remedy is to **re-import**
them: `backup-set patch <set> --ssh-key-file <path>`, which is a key
rotation, for each affected set. The far host already knows the public
key, so this is a re-import rather than an enrolment — unless the
original private key is also gone, in which case generate a new keypair
and install the public half on the source.

One placement rule is worth repeating because getting it wrong makes the
whole feature worthless: the resolved key **must live outside whatever
directory tree an SMB or AFP share exports**. See
[`docs/ssh-setup.md`](ssh-setup.md).

### A source's SSH key

```bash
retnd backup-set test-connection <source>/<set>
```

Seven named steps — `credentials`, `resolve`, `connect`, `host_key`,
`authenticate`, `list`, `write_probe` — each reported `passed`, `failed`
or `skipped`, and a non-zero exit when one of the verdict-bearing ones
fails. `skipped` is a first-class outcome and never a quiet pass: a
credential is not offered to a server whose host key did not match.
Authentication failures are a key, a username, or an `authorized_keys`
entry on the far host.

Rotate:

```bash
retnd backup-set patch <source>/<set> --ssh-key-file /path/to/new/key
```

The key is read once, validated, and copied into this deployment's own
key store; the original is left alone. A connection-changing edit is
proven before it is written and refused when it cannot be
(`--no-verify` writes it anyway and marks the set unverified).

Two things to know while you are in there:

- The **seventh** step is the write probe: it creates a uniquely named
  dotfile under the remote path, removes it, and confirms it is gone.
  Both answers pass — a read-only source is a posture, not a failure.
- If the new credentials are read-only where the old ones could write,
  `read-only off` is **refused** (`BACKUP_SET_SOURCE_NOT_WRITABLE`, HTTP
  409) rather than accepted and quietly not honoured. Turning read-only
  **on** is never refused. An incremental set is unaffected either way:
  it never deletes from the source.
- The probe's errors are dropped for their own text, so no remote path or
  probe filename reaches a log or an API response. If the one outcome
  that leaves litter occurs — written but not removed — the message names
  the filename prefix to look for.

### An S3 storage-medium credential

Storage mediums are the artifact side's destinations and are a different
thing from repository domains; see
[`docs/storage-mediums.md`](storage-mediums.md#storage-mediums-and-repository-domains-are-different-things).

```bash
retnd medium import-credentials --stdin      # reads AWS shared-credentials text, writes it 0600, prints an id
retnd medium edit <medium-id> --credentials-id <new-id>
retnd medium test-connection <medium-id>
```

There is deliberately no `--access-key-id` and no `--secret-access-key`
flag anywhere on this surface: a secret on a command line is in `ps`
output for every user on the box and in shell history. Leaving the
credential flags off an edit keeps the credential already configured —
nothing here ever reports one back to be resubmitted.

---

## Runbook 4: the engine is refusing because the gate is off

### The symptom

```text
the incremental (kopia) backup engine is disabled in this deployment: set
incremental_engine.enabled: true in config.yaml, or
RETND_INCREMENTAL_ENGINE=1 in the environment, to enable it; existing
artifact backup sets are unaffected
```

as HTTP **409** with error code `INCREMENTAL_ENGINE_DISABLED`, or as an
ordinary CLI failure (exit `1`).

### What it means, and what it does not

It means this deployment has not enabled the incremental engine, and the
thing you asked for is one of the gated ones: creating or updating a
`kopia` set, running its snapshot cycle, verifying a snapshot,
maintaining a repository, or restoring from a snapshot. A restore is
**refused rather than silently skipped**, which is the correct behaviour
and the reason you are reading a refusal rather than an empty directory.

It does **not** mean anything is wrong with your data, your repository or
your artifact sets. A configuration holding `kopia` sets still loads and
validates with the gate off, and every artifact set goes on backing up on
its schedule. That is deliberate: turning the engine off must never be a
choice between the engine and the rest of the deployment's backups.

### Check what is actually in force

The environment wins over the file, in both directions, so a deployment
that "has it enabled in `config.yaml`" can still be disabled by a
variable in whatever launched the process — a systemd unit, a compose
`environment:` entry somebody added, a wrapper script, or the `-e` on the
`docker compose exec` you are typing right now. Ask the process rather
than the deployment's files:

```bash
docker compose -p retnd ... exec retnd printenv RETND_INCREMENTAL_ENGINE
grep -n -A2 '^incremental_engine' /etc/retnd/config/config.yaml
```

- variable set to `1|true|yes|on` → enabled, whatever the file says;
- variable set to `0|false|no|off` → disabled, whatever the file says;
- variable unset or empty → the file decides; an absent block is
  disabled.

A value that is neither spelling is refused when the configuration is
validated, naming the variable and the spellings it accepts — a typo in
the switch that decides whether backups happen must not resolve to "off".

### Enabling it

Add the block and restart the serving process: there is no config watcher
and no SIGHUP reload, so a hand edit is read
at start. Then confirm against something that only works with the gate
open:

```bash
retnd repository health
```

### Deliberately keeping it off

That is a supported posture and needs no further action. Leave the
`kopia` sets configured and disable them so the refusal stops appearing
in the feed on every cycle:

```bash
retnd backup-set enabled <source>/<set> off
```

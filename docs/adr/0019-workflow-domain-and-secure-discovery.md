# ADR 0019: The workflow domain, secure script discovery and the immutable run plan

- Status: accepted
- Date: 2026-09-13
- Scope: EPIC L (#807), issue #808. The decisions #809-#814 build on and
  may not re-litigate.

## Context

EPIC L lets an operator drop shell scripts into a directory and have this
product execute them around a backup. That sentence is the entire feature
and it is also, stated plainly, a remote code execution facility with a
filesystem for an interface: a directory this daemon reads, and whose
contents it runs — some of it as root on the backup server, some of it on
the production host the backup pulls from.

Nothing about the rest of this product is a precedent for it. The
artifact pipeline copies bytes it never interprets. The incremental
engine opens repositories it created. The application validator (FR-13)
runs a command, but one an operator named explicitly in `config.yaml`,
one path, reviewed once. A workflow is the first mechanism where **what
runs is decided by what is in a directory at the moment a backup starts**.

This ADR records the decisions that make that acceptable. It deliberately
settles them before any code can execute a hook: #808 contains no `exec`
call at all, and #809 — the first issue that can run anything — inherits
these rules rather than choosing them under delivery pressure.

## Decision 1: a script's target is a property of its filename

A hook is named `NAME.local.sh` or `NAME.remote.sh`. The suffix is the
only place the execution target is stated, it is mandatory, and
`^[0-9A-Za-z][0-9A-Za-z._-]*\.(local|remote)\.sh$` is the whole rule.

A plain `backup.sh` is **refused**, not defaulted.

That refusal is the single most valuable decision in the epic and it is
the one most likely to be argued away as unfriendly, so the reasoning is
worth stating in full. The failure being prevented is not "a script did
not run". It is a script that quiesces a database running on the wrong
machine: on the backup server, where the database is not, so the quiesce
silently does nothing and the backup captures a torn state; or on the
production host, where a script written for the backup server's
filesystem does something nobody intended. Both are silent. Both produce
a green backup. A default — either default — converts a typo into one of
them.

The alternatives were considered and rejected:

- **A per-directory target** (`before_dir_local`, `before_dir_remote`).
  Rejected because the target then lives in the config file while the
  script lives in the tree, so moving a file between directories changes
  where it runs with nothing in the file to say so.
- **A shebang or in-file directive.** Rejected because it requires
  opening and parsing every candidate before deciding whether it is a
  candidate, which is a parser on untrusted input in front of the
  security check rather than behind it.
- **Defaulting to local.** Rejected: see above.

The same reasoning drives the rest of the name rule. Whitespace, control
characters, path separators and leading dots are refused because this
string travels into an audit record, a log line and a remote command; a
newline in a filename turns one audit line into two. **Non-ASCII is
refused** for a narrower reason worth naming: a Cyrillic `е` in
`quiesce.local.sh` is indistinguishable from a Latin `e` in every listing,
review and diff an operator has, so a homoglyph is the one naming attack
that survives human review.

And every entry in a hook directory is held to the rule, including files
that are obviously not scripts. A `README` is refused. The friendlier
alternative — ignore what does not match — means an operator who forgot
the target suffix gets a backup that runs no hooks and reports success,
which is exactly the class of silent failure this ADR exists to close.

## Decision 2: the custody rules are the SSH key's rules

`internal/secretref` and `internal/transport/rclone/ssh.go` already refuse
a secret file another local account can read or replace, and they refuse
it rather than warning, because a secret with somebody else's
fingerprints on it is not one this product can make a custody claim about.

A hook script is the same question with the stakes raised, so it gets the
same answer: a regular file, reached without following a symbolic link,
inside the approved root, with no directory in its ancestry that another
account can write.

One rule differs, deliberately:

| | secret file | hook script |
|---|---|---|
| mode test | `&0o077` — nobody else may **read** | `&0o022` — nobody else may **write** |

For a secret, reading is the exposure. For a program, writing is. A
world-readable hook script is therefore **accepted**, because refusing it
would refuse the ordinary `0644` file every editor produces for no
security gain, and a world-writable one is refused because it is a program
any local account can change between now and the next backup.

`firstWritableAncestor` is now a third copy of the same walk.
`internal/secretref`'s own doc argues why the mechanism is duplicated
rather than shared: unifying it with `internal/transport/rclone`'s copy is
a refactor of that package's most security-sensitive file and is tracked
separately. The rule is identical, sticky-bit exception included.

## Decision 3: the workflow root is the trust boundary, and stage dirs are constrained to it twice

`workflows.root` is the one path an operator looked at and approved. Every
stage directory must resolve inside it, and containment is checked twice
against two different forms of the root:

1. **lexically**, before any filesystem call, which catches `..`;
2. **after full symlink resolution**, against the resolved root, which
   catches the case no lexical rule can see — a symbolic link *inside* the
   tree pointing out of it.

Neither check subsumes the other. A stage directory that is *itself* a
symbolic link is refused outright rather than followed, for
`secretref`'s reason: the permissions protecting a link say nothing about
the ones protecting its target.

The root itself **may** be a symbolic link, and the asymmetry is
intentional: `/workflows -> /mnt/user/appdata/workflows` is the ordinary
shape of a NAS deployment and the operator declared it. A stage directory
is a name this product joined onto that root on their behalf, so a link
there is a path nobody approved.

## Decision 4: three states of a stage directory, and only one of them is a config error

| configuration | meaning | refused where |
|---|---|---|
| no directory set | the stage is disabled | — |
| directory exists, empty | zero steps, a legitimate work-in-progress | — |
| directory configured, missing | a mistake | **run start**, not config load |

The third row is the interesting one. `internal/config`'s validator never
opens a file — that is a standing rule of the package, and it exists
because a configuration that validates on one host and not on another is
worse than one that fails everywhere. A missing `/workflows` mount would
otherwise mean the daemon refuses to start, taking every *other* backup in
the deployment down with it, and a mount that arrives a second after the
process does would make a perfectly good config unbootable.

So the split is: `internal/config` refuses what cannot be right anywhere
(a relative root, a `..`, a reserved variable name, a `workflow` block
naming no directory), and `internal/workflow`'s `Snapshot` refuses what
depends on the disk (missing, symlinked, group-writable, oversized). The
second is a failed backup somebody sees tonight rather than a daemon that
will not come up.

## Decision 5: the plan is the only execution authority

At run start, `Snapshot` canonicalizes, discovers, validates, opens each
file **exactly once** with `O_NOFOLLOW`, checks custody **on the
descriptor**, reads, hashes, and copies the bytes into
`<state dir>/workflow-runs/<run-id>/scripts/<step-id>` — directories
`0700`, files `0600`. The resulting `Plan` is immutable and nothing in
this product ever opens a path under the workflow root again.

This is the TOCTOU decision and it is structural rather than careful.
Validating a path and then opening it is the classic check-then-use race;
opening it, reading it, then re-opening it to copy it is the same race with
an extra step. Every property that matters — regular file, mode, size,
content, hash — is answered from one descriptor and one buffer.

The consequence is the guarantee #808 was filed for: **editing or deleting
a script in `/workflows` after the snapshot cannot change what that run,
or a recovery of that run tomorrow after a restart, executes.** A recovery
is the case that makes the spool non-negotiable rather than merely tidy: it
may run hours later, on a tree that has since been redeployed, and the
script it has to finish may no longer exist anywhere else.

The spool location is **derived** from the state database's directory and
is not configurable. A configurable spool is a second path an operator can
point at an SMB export, and the spool's whole value is modes that nothing
but this process can reach — a guarantee that evaporates inside a share.

Spooled copies are not executable. Execution passes the script to an
interpreter as an argument, so the exec bit buys nothing, and a
non-executable file under the state directory is one fewer thing a mistake
elsewhere can turn into a running program.

## Decision 6: `resolved_plan_hash` fingerprints the decision, not the run

The hash covers the backup set, the configured stage list, and per step
the order, scope, phase, script name, target, size, content hash, timeout
and execution connection; then every environment variable by name, with
literals by value and secrets by **location**.

It deliberately excludes the run id, the timestamps, the spool paths and
every step's mutable state, because the question it exists to answer is
"has anything about what we execute changed since last night", and a hash
that moved every run could not answer it.

It excludes resolved secret material for a different and non-negotiable
reason: **a hash over a credential is a credential oracle.**

The encoding is a hand-written, versioned, line-oriented form rather than
JSON or a struct dump, so that what the hash covers is a decision somebody
made and can read. Adding a field to `Step` must not silently move every
deployment's plan hash.

## Decision 7: ordering is `LC_ALL=C` bytewise over the whole basename

Within a stage, scripts run in bytewise order of their full basename,
target suffix included. Between stages, the order is global-before,
set-before, *(the backup)*, set-after, global-after — the "after" stages
unwind in the reverse of the order the "before" stages were entered, which
is the nesting a shell trap, a `defer` stack and a transaction all use, and
the only order in which a global "before" hook that mounted something can
rely on the per-set hooks having finished with it.

Bytewise over the whole basename, and not the tidier-sounding "by the name
part with the target as a tiebreak", because the tidier rule would put
`10-dump.local.sh` and `10-dump.remote.sh` adjacent while separating
`10-dump.local.sh` from `20-sync.local.sh` — an order that is neither what
the operator numbered nor what their directory listing shows. What they get
instead is exactly `LC_ALL=C ls`, reproducible on their own machine without
running this product.

## Decision 8: a hook's environment is built, never inherited

A hook does not inherit this daemon's environment. It gets a sanitized
baseline (`PATH`, and nothing else), then `workflows.environment`, then the
backup set's `environment`, then the `BACKUPD_*` built-ins.

Inheriting was rejected outright, and not on style grounds. This process's
environment carries whatever the init system, the container runtime and the
operator's shell put there — *including, on a deployment using the `env`
secret resolver, the repository passphrase itself*. Handing that block to a
script somebody dropped in a directory would make every hook a credential
dump, silently.

The `BACKUPD_*` namespace is **reserved at validation time**, not
overridden at merge time. A key an operator can write and this product
silently discards is a key that looks like it works; and a hook reading
`BACKUPD_BACKUP_STATUS` has to be reading what this product observed, not
a value from a config file. The whole prefix is reserved rather than only
the names that exist today, so a built-in added in #811 cannot collide
with a variable somebody already configured.

Values are literal. `$HOME` is five characters. A config file that expanded
variables would be a config file whose meaning depends on this daemon's own
environment, which is the thing the sanitized baseline exists to sever.

## Decision 9: a secret-backed variable is a location, never material

An environment entry is either a literal or a `from_secret` reference —
the same file/env/command triple as `passphrase`, `key_encryption` and
`medium credentials`, spelled the same way, with no fourth field to paste
material into. `secretref`'s own
`TestRefFieldSetMatchesTheConfiguredOnes` now pins `config.SecretSource`
alongside the other three, so a field on one and not the others cannot
ship.

Resolution happens at execution time, through the existing custody rules,
and produces values wrapped in `obs.Secret`. A resolved value is never
part of a plan, a plan hash, a spooled file, an API response or a journal
row.

The journal half of that claim is enforced by **absence**: the schema has
no column an environment could live in, and
`TestWorkflowSchemaHasNoColumnASecretCouldLiveIn` pins the exact column
set of both tables. A write path that does not persist something today is
one somebody can extend tomorrow; a column that does not exist cannot be
filled in by accident.

## Decision 10: no `CHECK` constraint on any vocabulary column

`state`, `scope`, `phase`, `target`, the three statuses and
`recovery_state` are closed vocabularies enforced in Go, on the write
path, and stored as plain `TEXT`.

That is the opposite of what this schema does for `artifacts.state`, and
the reason is what widening *that* `CHECK` cost: migrations 0002 and 0006
each rebuilt a table, and because `DROP TABLE` runs an implicit
`DELETE FROM`, the foreign-key cascade broke every populated journal in
the field until `migrate.go`'s `suspendForeignKeys` landed (#396). Every
one of these vocabularies is going to grow as #809-#814 land. A vocabulary
whose widening is a function and a test, rather than a table rebuild, is
the only version of this that does not ship that failure again.

## Consequences

**A deployment with no workflow configuration is bit-for-bit unaffected.**
No run row, no spool directory, no environment, and — held byte for byte
by `TestMarshal_ANoWorkflowConfigGainsNoWorkflowKeys` — no new key in a
re-marshaled `config.yaml`, which under `Load`'s `KnownFields(true)` is
the difference between an upgrade and an older binary refusing the file
outright (FR-35).

**A workflow tree that was valid yesterday can be refused today.** Modes
drift, mounts move, someone adds a `README`. The refusal is a failed
backup with a sentence naming the file and the `chmod` that fixes it,
which is the direction this ADR chooses every time: a refused backup is
recoverable, and a hook that ran when it should not have is not.

**The spool grows.** Every workflow run keeps a private copy of every
script it ran, until the run is terminal *and* its recovery is settled.
`Run.SpoolRetainable` is the single expression of that pair, because a
comparison written the wrong way round at either of its two call sites
deletes the scripts a recovery was about to run. The bounded
history/spool retention policy itself is #811's.

**#809 and #810 may not re-open a path in the workflow root.** That is the
constraint this ADR exists to impose on them. A future optimisation that
"just re-reads the script" would silently undo Decision 5 while every test
about naming, custody and hashing stayed green.

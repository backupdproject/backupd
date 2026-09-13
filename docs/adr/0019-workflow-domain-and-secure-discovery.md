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

A derived fact is only worth deriving if it is also **checked**, and the
first implementation of `Step.Validate` derived the target and threw it
away: it called `ParseScriptName` for the error and discarded the target
it returned, so a record saying `quiesce.remote.sh` runs `local`
validated. That record cannot come from `Snapshot` — and a recovery pass
reads records off disk after a restart, where "the planner would never
build that" is not a guarantee. So `Validate` now requires the parsed
target to equal the stored one, and the stored step id to equal
`StepID(order, scope, phase, name)`. The journal's write path calls it
(Decision 10), and so does `RecoverPlan`.

## Decision 2: the custody rules are the SSH key's rules, minus the sticky exception

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

A second rule differs, and this one is a **correction** rather than an
adaptation. `internal/secretref` exempts a directory carrying the sticky
bit from its writable-ancestor walk, and for a secret file that is sound:
POSIX restricts `unlink` and `rename` inside a sticky directory to the
entry's owner, the directory's owner or root, so `1777` cannot be used to
**replace** an existing `0600` file.

A hook directory's exposure is the opposite operation. Discovery executes
every script it **finds**, so the attack is **creating** a new entry, and
sticky has never restricted creation. Copying secretref's rule here
accepted a `1777` hook directory — which is `/tmp`'s mode, and therefore
the mode somebody reaches for — and any local account could drop
`00-root-shell.local.sh` into tonight's backup. The ancestor walk drops
the exception for the same reason one step removed: a world-writable
ancestor is a path any account can create the missing components of, so
the stage directory it wins the race to `mkdir` is one it owns outright,
with a mode of its choosing.

**So this package refuses `&0o022` on a hook directory and on every
ancestor of one, sticky or not**, and it additionally checks **ownership**:
the directory (and the script) must be owned by this process's own user or
by root. A mode says who *may* write; it does not say whether the account
that may is one this product trusts, and a `0755` hook directory owned by
an unaudited service account is a directory that account fills with
programs this daemon runs as root. Root is accepted whatever the daemon
runs as, because root can already replace this binary.

`firstWritableAncestor` is now a third copy of the same walk.
`internal/secretref`'s own doc argues why the mechanism is duplicated
rather than shared: unifying it with `internal/transport/rclone`'s copy is
a refactor of that package's most security-sensitive file and is tracked
separately. The divergence above is a reason to keep them apart until that
refactor: the two planes genuinely want different rules.

A stage directory is also **bounded**: `MaxScriptsPerStage` (64) entries,
and `MaxPlanBytes` (8 MiB) across the whole plan. `os.ReadDir` is
unbounded and everything downstream of it is per-entry work inside the
backup window, so a directory something else filled up is a backup that
never starts. The whole directory is refused rather than the first 64
executed, for the reason an oversized script is: a prefix of a plan is a
different plan.

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

### The spool's own path is custody-checked, and created without `MkdirAll`

The spool is what actually runs, so a spool another account can relocate
or replace is the same vulnerability with the protections pointing the
wrong way. `os.MkdirAll` cannot support the claim this decision makes, for
two reasons that are easy to miss:

- it **follows symbolic links** in every component, so a pre-existing link
  at `<state dir>/workflow-runs` (or at any component above it) silently
  relocates the whole spool somewhere another account chose — and the
  `0700` applied afterwards protects *their* directory;
- it **accepts a path that already exists**, so a re-used run id writes
  into an earlier run's directory.

Instead, every component of the spool root is opened with `O_NOFOLLOW`
from the descriptor of its parent (`openat`), and each descriptor is
custody-checked — mode and owner, as in Decision 2 — before it is used as
the parent of the next. The run directory is created with `mkdirat`, which
**fails with `EEXIST`**: a second plan for one run id is refused and the
first run's captured scripts are left exactly as they are, because a
recovery of that run may be about to read them. The clean-up on a refusal
removes only a directory **this call created**, never one it found.

The spool root is held at `0700` whatever the umask and whatever it was
before; its ancestors are never chmod'ed, because they belong to the
deployment. The scripts directory, the run directory and the spool root
are `fsync`ed before `Snapshot` returns, so that the plan a caller is
about to journal is one a crash cannot take the directory entries away
from.

### A `Plan` is opaque, and a script is reached through a capability

`Plan`'s steps, its environment and its spool paths are package-private,
and `Script` does not expose the path it was discovered at. The only way
to reach a script's bytes is `Plan.OpenScript(stepID)`, which trusts
nothing but the step id: it re-derives the path from the plan, refuses a
`SpoolRef` that is not exactly `<spool>/scripts/<step id>`, re-walks the
spool's ancestry with `O_NOFOLLOW`, re-checks custody, and re-hashes the
bytes against the `sha256` recorded at snapshot time.

This is the shape the previous version documented and did not enforce:
`Steps` was a mutable slice on an exported struct and `SpoolRef` was a
string checked only for being non-empty, so #810 could — by accident or by
reading a tampered journal row — execute a path the plan never chose. A
comment is not a boundary. `RecoverPlan` produces the same opaque type
through the same validation, so the recovery path has no weaker plan to
work with.

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

The encoding is a hand-written, versioned form rather than JSON or a
struct dump, so that what the hash covers is a decision somebody made and
can read. Adding a field to `Step` must not silently move every
deployment's plan hash.

Version 1 of that encoding joined fields with tabs and terminated records
with newlines, on the argument that every value reaching it had been
validated free of control characters. That argument was wrong about
exactly the values an **operator** controls — a variable's literal value,
a secret file's path, a secret command's argv — so

```
{"A": "x\nenv\tB\tliteral\ty"}      and      {"A": "x", "B": "y"}
```

rendered identically and hashed identically: two materially different
environments, reported as "nothing about what we execute has changed".
**Version 2 length-prefixes every field and the field count**, and writes
a secret's argv as separate fields rather than joining it with a
separator no argument may contain (there is no such byte). A fingerprint's
one required property is injectivity, and that is the property it did not
have.

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

The journal half of that claim is enforced by **absence of a column a
resolved value could live in**, pinned by
`TestWorkflowSchemaHasNoColumnASecretCouldLiveIn`, which records the exact
column set of every workflow table — plus
`TestNoResolvedSecretReachesAnyPersistedByte`, which resolves a real
secret and then searches the database file, its write-ahead log and its
shared-memory index for the value, and
`TestAResolvedSecretReachesNoMarshaledConfigByte`, which does the same to
the bytes `core/service` writes back to `config.yaml` on every settings
save.

### The UNRESOLVED environment, however, is durable — and has to be

The first version of this decision read "there is no column for a hook's
environment, and that is a contract rather than an omission". Half of that
is right and the half that is wrong made the feature unrecoverable:

> a run is interrupted, the daemon restarts, an operator edits (or
> deletes) `workflows.environment` while it is down, and the recovery pass
> then has to finish a run that was planned with the **old** environment.

`resolved_plan_hash` covers the environment, which proves it changed and
cannot reconstruct it — a hash is one-way, which is the property it was
chosen for. And a `from_secret` **location** is not derivable from
anything else once the config file has moved on.

So `workflow_run_env` holds one row per variable — name, position, the
literal (`NULL` when the variable is secret-backed, so an empty literal
stays distinguishable from "no literal"), and the file / env / argv
**location** — written in the **same transaction** as the run and its
steps. What goes in those columns is exactly what `config.yaml` already
holds in the clear; what may never go in them is the value a location
resolves to. Recovery reads them back through
`workflow.NewEnvironment`, so a row set that has been tampered with is
refused by the same rules that refused it at configuration time: a journal
is not a trusted input.

### `value` and `from_secret` are pointers, because presence is the question

`from_secret: {}` decoded to the zero `SecretSource`, which is
indistinguishable from "no secret configured" — so a variable an operator
believed was secret-backed became a silently **empty literal**, and the
hook got an empty credential and failed far away from the reason.
Symmetrically, `value: ""` beside a `from_secret` is a contradiction that
was silently resolved in the secret's favour, and kept working, so nobody
found out which of the two keys was being ignored.

Both fields are therefore pointers: presence is representable, a
`from_secret` must name **exactly one** location, and declaring both keys
is a refusal in words. `value: ""` on its own remains what it looks like —
an empty variable, deliberately configured.

## Decision 10: no `CHECK` constraint on any vocabulary column

`state`, `scope`, `phase`, `target`, the three statuses and
`recovery_state` are closed vocabularies enforced in Go, on the write
path, and stored as plain `TEXT`.

"Enforced on the write path" is load-bearing and was initially untrue:
`CommitWorkflowPlan` accepted a plan of plain strings and checked only
that they were non-empty, so a state of `"whatever"`, a step whose target
disagreed with its script name, and a timeout of zero all committed — and
a recovery pass reads those rows after a restart, with no second chance to
notice. `CommitWorkflowPlan` now takes the **domain types**
(`workflow.Run`, `workflow.Step`, `workflow.Environment`) and calls their
own `Validate` before the transaction opens, and `RecoverWorkflowPlan`
re-validates on the way out through `workflow.RecoverPlan`. The
vocabulary is still `internal/workflow`'s; this journal simply refuses to
store anything that package would not have produced.

Two column decisions were corrected while 0012 was still unreleased.
`timeout_seconds` became **`timeout_nanos`**: whole seconds looked
friendlier in the `sqlite3` CLI and silently destroyed information — a
`500ms` bound became `0`, which is a bound that was configured and then
was not. And the partial recovery index now names the unsettled states
(`recovery_state IN ('required', 'in_progress')`) rather than saying
`<> 'none'`, so it keeps matching `RecoveryState.Settled` as the
vocabulary grows instead of silently enrolling every new state in the
recovery pass's scan; `TestRecoveryIndexMatchesTheUnsettledStates` fails
if the two drift.

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
No run row, no spool directory, no environment, and no new key in a
re-marshaled `config.yaml`, which under `Load`'s `KnownFields(true)` is
the difference between an upgrade and an older binary refusing the file
outright (FR-35).

That last claim is held against **bytes captured before this feature
existed**: `testdata/golden/*.premarshal.yaml` were produced by the parent
commit's `Load` + `Validate` + `yaml.Marshal`, and
`TestMarshal_ANoWorkflowConfigIsByteIdenticalToWhatItWasBeforeThisFeature`
compares today's marshal against them. The version this replaces marshaled
the already-loaded config as its own baseline, which compared the feature
against itself: anything `Validate` now does to a no-workflow config was
present on both sides and cancelled out.

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
constraint this ADR exists to impose on them, and it is now enforced by
the type rather than asked for in prose: there is no exported way to get a
workflow-root path out of this package, and `Plan.OpenScript` is the only
way to get a script's bytes. A future optimisation that "just re-reads the
script" would have to add API to do it.

**An interrupted run is recovered with the environment it was planned
with.** #811's recovery pass reads `RecoverWorkflowPlan`, not the config
file. A variable an operator removed while the daemon was down is still
resolved for the run that was planned with it — and if its location has
since become unreadable, that run fails with a sentence naming the
location, which is the honest outcome.

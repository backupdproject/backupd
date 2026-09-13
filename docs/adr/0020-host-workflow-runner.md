# ADR 0020: The host workflow runner

- Status: accepted
- Date: 2026-09-13
- Scope: EPIC L (#807), issue #809. Builds on ADR 0019 (#808) and does not
  re-litigate it. #810 executes the same envelope over SSH; #811 sequences
  the stages that call this.

## Context

ADR 0019 settled what a workflow *is*: scripts discovered under a root,
captured into a run-scoped spool, verified, and frozen into an immutable
plan that is the only authority on what a run executes. It contains no
`exec` call anywhere, on purpose.

This is the issue that runs something, and it runs it in the one place
that is hardest: **the host**. A hook named `NAME.local.sh` means "run
this on the machine backupd is installed on", and the machine backupd is
installed on is not the machine backupd is running on.

The runtime this product ships (`container/compose.yaml`,
`container/Dockerfile`, pinned by `docs/runtime-contract.md`,
`container/release-manifest.json` and a byte-for-byte compose gate) is
distroless, read-only, non-root, `cap_drop: ALL`, `no-new-privileges`,
and has **no shell at all**. It is the single most valuable security
property of the deployment, and it is enforced by tests rather than by
intention.

So `.local.sh` and the container contract are, on their face, in direct
conflict. This ADR is how that conflict was resolved without either one
giving way.

## Decision 1: a separate host process, not a weaker container

There are exactly four ways to make "run this script on the host" true,
and three of them are the same mistake wearing different clothes:

1. **Add a shell to the image.** That is the contract, deleted. Every
   other property survives and is worthless: a read-only root filesystem
   with `/bin/sh` in it is a machine an attacker can work in.
2. **Mount the Docker socket, or add `CAP_SYS_ADMIN` and `nsenter` into
   the host namespaces.** Both are root on the host for anybody who
   reaches the engine. Strictly worse than the shell, and routinely
   proposed because it is the shortest diff.
3. **Bind-mount the host root read-write and chroot.** The same, with
   extra steps and a worse failure mode, since it also makes every
   accident in the engine a host-wide accident.
4. **Run a separate, small, version-matched process on the host**, and
   let the engine ask it over one Unix-domain socket with a narrow
   vocabulary.

backupd does the fourth. `core/internal/hostrunner` is that process and
`backupd workflow-runner serve` is how it is started.

The container gains exactly two bind mounts — the hook scripts read-only
at `/workflows`, and the runner's socket directory at `/data/run` — and
nothing else. No capability, no privilege, no namespace, no Docker
socket, no shell. `distribution/compose`'s prohibition rules and the
installer's byte-for-byte compose gate pass unchanged, which is #809's
own acceptance criterion and is checked rather than asserted.

The cost is real and is accepted: a second process to install, supervise,
upgrade and roll back, and a platform-specific supervision story. Every
decision below is about making that cost bounded.

## Decision 2: bytes cross the socket, never a path

The protocol has **no field that can hold a path**, and every frame is
decoded with `DisallowUnknownFields`, so a request carrying a
`script_path` is refused as malformed rather than having the field
silently dropped.

The alternative — a runner that accepted "execute `/path/on/the/host`" —
is a general-purpose remote execution service with a socket in front of
it. Whoever reached the socket could run anything already on the host,
and ADR 0019's entire custody argument (`O_NOFOLLOW`, mode checks, a
private spool, a verified sha256) would protect nothing, because the last
hop would not be using any of it.

So the engine sends the captured bytes, their size and their sha256, and
the runner re-checks size and hash before anything is created. That is
the second verification of the same fact, deliberately: the first one
happened on the far side of a socket, and a check on the far side of a
boundary is not a check on this side of it.

A refusal here is `script_mismatch`, which is distinct from a hook that
failed. Conflating "the hook failed" with "we never ran the hook" would
take away the one distinction a workflow engine cannot do without.

## Decision 3: two independent locks, and an explicit version refusal

The socket is `0600` inside a `0700` directory owned by the service
account. That is the kernel enforcing the boundary, which is the only
enforcement that cannot be bypassed by a bug in this product's own code.

It is not the only lock. Every connection must also present an
**installation-scoped credential** written into backupd's secrets area at
install time, compared in constant time, held to mode `0600`, and at
least 32 characters.

Defense in depth reads as belt-and-braces until the ways file permissions
have actually failed on the deployments this product targets are listed:
a NAS package manager that runs everything as one account, an operator
who `chmod`'ed a parent directory while debugging something else, a
container runtime with unexpected uid mapping, a restore of `/etc` with
the wrong ownership. The credential is what turns "the permissions were
wrong for a week" into "and nothing could use it anyway".

A **version mismatch is refused explicitly**, naming both versions. The
engine and the runner are one program cut in half by a socket; a 0.4.0
engine talking to a 0.3.9 runner is two halves of two programs, and the
failure mode of guessing is a hook running with an environment or a
timeout the other half did not mean. An exact match — not a compatibility
range — because a range is a promise that every future change to the
execution envelope will be negotiated across it, and nothing in this
product is in a position to make that promise.

Authentication is checked **before** the version, so an unauthenticated
caller learns only that it is unauthenticated, while a legitimate engine
of the wrong release still gets the sentence it needs.

## Decision 4: never root, with no override

The runner refuses to serve as uid 0, and there is no flag that turns
that off.

An administrator installed this product. That says nothing whatever about
whether they meant every script in a hook directory to run as root —
including one that arrived by rsync from somewhere else. A runner
executing as root would turn "drop a file in `/workflows`" into "drop a
file in `/workflows` and own the machine".

No override, because an override is a single switch that re-privileges
every hook at once, and every deployment that ever flips it stays
flipped. An operator who genuinely needs a privileged action in a hook
has a tool for it that is per-command and auditable: `sudoers`.

The installer carries the same rule from the other end. A deployment
running as uid 0 is not given a root runner; it is told, and the
directories and credential are still staged so that fixing the uid later
is one flag rather than a re-install.

## Decision 5: the connection is a lease

An engine that crashes, is killed, or has its container recreated
mid-hook **expires the lease**: the socket read fails, the runner
terminates the hook's process group and removes its working directory.

The alternative is not "the hook finishes on its own". It is a hook that
has quiesced a database, an engine that is no longer there to run the
matching `after` hook, nothing anywhere that will ever time it out, and a
database that stays quiesced until somebody notices. A hook with no owner
is worse than a hook that was killed, so a hook with no owner does not
exist here.

Cancellation arrives on a *second* connection rather than on the one
running the step, because the running connection is busy being the lease,
and an engine needing to stop a step whose own connection has wedged
needs a door that is not that connection.

## Decision 6: one session per script, and termination is proved

Each script runs under `setsid`, so it leads its own session and process
group. Killing it is one signal to the negated process group id, which
reaches everything the hook started with no window in which a new child
appears between a walk and a kill. Without it, killing a hook that ran
`pg_dump | gzip > x` kills the hook and leaves both halves of the
pipeline running, still holding the database connection the timeout
existed to release.

Termination is `SIGTERM`, a grace period, `SIGKILL`, and then **a look**.
The look is the part that is easy to leave out and the part the journal
records (`workflow.Step.TerminationConfirmed`): a process group still
present after `SIGKILL` is a process stuck in uninterruptible I/O, which
does happen to a hook talking to a NAS share, and reporting that honestly
is the difference between "the hook was killed" and "we stopped waiting
for the hook".

The order is load-bearing and is the one thing here most likely to be
written wrong: signal, **wait for the leader to be reaped**, then probe.
A process that has exited but not been waited on is a zombie, a zombie is
still a member of its process group, and `kill(-pgid, 0)` cannot tell one
from a running process — so a probe taken before `cmd.Wait` reports
"still there" for every termination, including the ones that worked.

The per-step working directory is removed after the step **unless**
termination could not be confirmed. Removing a directory something may
still be writing into is how a cleanup turns into a half-written dump
nobody can explain.

## Decision 7: the execution envelope, agreed with #810

`core/internal/remoteexec` (#810) executes the same scripts over SSH. The
two were agreed as one contract so that a hook behaves identically
wherever it runs:

- a **fixed bash path**, resolved once by preflight, reported by
  `status`, never searched through `PATH` — because `PATH` is part of the
  hook environment and is operator-configurable per backup set, so
  resolving per execution means one set's hooks can silently run under a
  different shell;
- invoked `--noprofile --norc`, non-interactive, no pty;
- a **sanitized environment** that never inherits this daemon's own, with
  `BASH_ENV`, `ENV`, `SHELLOPTS` and `BASHOPTS` deleted unconditionally —
  each of them makes bash execute, or reinterpret, something the plan
  never captured, and all four are ordinary variable names that an
  operator can set in `workflows.environment` today;
- the environment delivered through `cmd.Env`, never as `export K=V`
  text, because text is a quoting bug waiting for a value with a quote in
  it and there is no quoting function worth trusting with a database
  password;
- `bash -n` on the captured bytes first, reported as its own refusal
  (`syntax_error`) so that `.local.sh` validation can fail **before a
  backup starts** rather than in the middle of one;
- and **nothing injected**: no `set -e`, no `-u`, no `-o pipefail`, no
  `-x`. A runner that helpfully added `set -e` would change the meaning
  of every hook already written, invisibly, since the file the author is
  reading would no longer describe what runs. `-x` additionally prints
  every expansion — including one holding a credential — into captured
  stderr this product stores.

stdout and stderr are streamed back as **separate** streams (the journal
records two references and a merged text cannot be split again), with
sequence numbers from **one shared counter** across both, so the order
they were read in is recoverable. The number records this process's read
order, which is not exactly the hook's write order; no reader can do
better without a pty, and that is written down rather than implied.

The two implementations differ in exactly one place. Here the verified
bytes are written to a runner-private file, `0500`, a *sibling* of the
working directory — bash reads a script incrementally, so a script the
hook could rewrite mid-run is a script whose second half is not the half
that was hashed — and the hook keeps its own stdin. #810 sends bytes on
stdin instead, because leaving no residue on the remote host is one of
its own acceptance criteria.

## Decision 8: the installer provisions, and the operator's answer survives

`<prefix>/workflows` and `<prefix>/run` are created `0700` by the
installer **before** any `docker compose up`, because Docker creates a
missing bind-mount source itself, as root, with a mode nobody chose.

The credential is written **once** and never rotated by an upgrade: a
rotated credential is an engine that cannot authenticate to its own
runner until both halves restart, which is a broken deployment produced
by a routine upgrade.

The runner binary is copied **out of the image being installed**, under
its version, with a stable symlink pointing at it. Version-matched has to
be a property of the bytes rather than a step somebody remembers, and the
versioned-plus-symlink shape is what makes upgrade and rollback a pair: a
rollback is a symlink flip, not a re-download on a NAS that may be
offline.

Whether the runner is supervised is `WORKFLOW_RUNNER` in the `.env` —
`auto` (start it once there are hook scripts), `on`, or `off` — rather
than a command-line flag, because the answer has to survive every later
`install`. `off` still provisions the directories and credential: they
hold an operator's scripts and any unresolved recovery spool, and a
deployment that turns the runner off for a week must not lose either.

Supervision is systemd where systemd is present and writable, and
otherwise the unit is staged under the prefix with the three commands
printed. Printing is a real outcome, not a fallback: silently doing
nothing would leave `.local.sh` validation failing with no explanation
anywhere, and escalating to root to write into `/etc` uninvited is not
something an installer gets to do on the machine an operator is most
careful about.

Uninstall takes the unit, the socket and the run directory, and
deliberately leaves `<prefix>/workflows`. Those files are the operator's,
like the backups: an uninstall that deleted them by default is a
data-loss bug with a friendly name.

## Consequences

- `.local.sh` hooks run on the host, as an unprivileged account, and the
  container security contract is byte-for-byte what it was.
- A dead engine cannot leave a runaway script on the host.
- An unpaired upgrade is a loud refusal naming both versions, rather than
  a hook that behaves subtly differently.
- Deployments get a second process to supervise, and platforms without
  systemd get instructions rather than automation. That is the price of
  not weakening the container, and it is the right way round.
- A hook that leaves a background process holding its stdout keeps the
  step open until its timeout, at which point the group is killed. Every
  shell and every CI runner behaves the same way for the same reason, and
  the alternative — returning while something is still writing output
  nobody is reading — is the runaway this design exists to prevent.

## What this ADR does not decide

Stage sequencing, cleanup-phase semantics and recovery orchestration
(#811), remote SSH execution (#810), and the CLI, API and UI surfaces
(#812-#814). This is an executor: it owns no workflow policy, and it
decides nothing about when a step should run or what should happen after
one fails.

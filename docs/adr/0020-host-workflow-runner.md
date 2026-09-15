# ADR 0020: The host workflow runner

- Status: accepted
- Date: 2026-09-13
- Scope: EPIC L (#807), issue #809, amended by issue #865 (Decision 9).
  Builds on ADR 0019 (#808) and does not re-litigate it. #810 executes the
  same envelope over SSH; #811 sequences the stages that call this.

## Context

ADR 0019 settled what a workflow *is*: scripts discovered under a root,
captured into a run-scoped spool, verified, and frozen into an immutable
plan that is the only authority on what a run executes. It contains no
`exec` call anywhere, on purpose.

This is the issue that runs something, and it runs it in the one place
that is hardest: **the host**. A hook named `NAME.local.sh` means "run
this on the machine retnd is installed on", and the machine retnd is
installed on is not the machine retnd is running on.

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

retnd does the fourth. `core/internal/hostrunner` is that process and
`retnd workflow-runner serve` is how it is started.

The container gains exactly two bind mounts — the hook scripts read-only
at `/workflows`, and the runner's socket directory at `/data/run` — and
nothing else. That second mount holds the **socket and nothing else**:
see Decision 6a. No capability, no privilege, no namespace, no Docker
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
**installation-scoped credential** written into retnd's secrets area at
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

> Amended by #865, which replaced the MECHANISM and kept every property.
> Read this decision as written -- it is the argument the container
> version inherits -- with the container's cgroup where the process group
> is, `docker kill` where the signal is, and "the daemon no longer knows
> this container" where `kill(-pgid, 0)` is. Decision 9 states the
> differences, including the one place the ORDER of the look changed.

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

`kill(-pgid, 0)` failing after the leader was reaped is not the end of
the sequence either. A hook that exits on its SIGTERM while leaving
behind a child that ignores it — any daemon that traps the signal to
finish work first does exactly that — leaves the group occupied by a
process nothing else on the host still knows the pgid of. So an
unconfirmed probe is followed by the `SIGKILL` it justifies, and only a
group that survives *that* is reported unconfirmed.

The per-step working directory is removed after the step **unless**
termination could not be confirmed. Removing a directory something may
still be writing into is how a cleanup turns into a half-written dump
nobody can explain. That holds on the paths that end in an ERROR as well
as the ones that end in a result: a step whose output stream was lost
mid-run (the engine died) still went through a kill, and "the operation
failed" is not a reason to delete the evidence of one that could not be
proved.

## Decision 6a: the socket directory and the workspace are two directories

`<prefix>/run` holds the socket. `<prefix>/workspace` holds every
per-step working directory and every runner-private copy of a captured
script. They are separate because only one of them is mounted.

Connecting to a Unix socket is a write, so `/data/run` has to be bound
read-write into the engine — which makes everything under it writable by
whatever that container runs as. The paths the runner CREATES, chmods and
later REMOVES RECURSIVELY must therefore not be under it: an engine that
was compromised, or merely authenticated, could otherwise plant a
symbolic link at `workflow/<run id>` and choose which host directory this
process writes a `0500` executable into and which one it then deletes.
Nothing mounts `<prefix>/workspace`, so that link cannot be created in
the first place.

And the runner does not rely on that alone. Every directory it creates
under the workspace, and every recursive removal, goes through an
**openat-style directory descriptor** (`os.Root`), one component at a
time, refusing a symbolic link at any of them rather than following it.
A path-based `MkdirAll`/`RemoveAll` follows whatever it meets at each
component; a final-component `Lstat` catches the last hop and nothing
before it. `Layout.Validate` refuses a layout that nests the workspace or
the secrets area inside the runtime directory, so a deployment cannot put
back by configuration what this decision took apart.

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

`<prefix>/workflows`, `<prefix>/run` and `<prefix>/workspace` are created
`0700` by the installer **before** any `docker compose up`, because
Docker creates a missing bind-mount source itself, as root, with a mode
nobody chose. The unit names the workspace too — `--workspace-dir`, and
`ReadWritePaths` lists exactly those two writable directories — so a
runner started by systemd can give a hook somewhere to write and can
write nothing else on the host.

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

Uninstall takes the unit, the socket directory and the workspace, and
deliberately leaves `<prefix>/workflows`. Those files are the operator's,
like the backups: an uninstall that deleted them by default is a
data-loss bug with a friendly name.

## Decision 9: local hooks run in ephemeral containers (#865)

A hook script is arbitrary code an operator dropped in a directory. Under
Decisions 1-8 it ran as the service account on the host, with that
account's whole filesystem in front of it and no bound on what it could
touch. #865 closes that: the runner still owns the execution, and what it
starts is one **ephemeral container per hook**.

```text
docker create --name backupd-hook-<run>-<step>-<16 hex>
           --label backupd.workflow-hook=1 (plus run, step and instance labels)
           --network none  --security-opt no-new-privileges  --cap-drop ALL
           --read-only  --tmpfs /tmp:rw,nosuid,nodev,size=64m
           --pids-limit 512  --user <the runner's own uid:gid>
           --platform <the daemon's own>  --entrypoint <bash IN the image>
           --env NAME ...            names only, never values
           -v <per-step work dir>:<itself>          read-write
           -v <captured script>:<itself>:ro         read-only
           [-v <operator-configured path>:<itself>[:ro]]
           <hook image> --noprofile --norc <the script>
docker start --attach <that name>        the hook runs, two streams back
docker inspect <that name>               what the container's process DID
docker rm --force --volumes <that name>  and the proof it is gone
```

**The lifecycle is explicit, and `docker run` is not used.** `run` is
create-and-start behind one exit status, and both halves of that cost
something this design needs. The container's name and labels do not exist
on the daemon's side until some unobservable moment inside that call, so a
cancel landing there leaves an object nothing can address -- the
termination signalled a name the daemon had not registered, got "no such
container" twice, and a container created a moment later ran on with
nothing watching it. And the client's exit status carries both "the
container could not be created" and "the hook ran and returned this",
which are different answers with the same number (125): a hook ending in
`exit 125` was recorded as a step that never ran, which is a lie about the
one fact the workflow engine branches on and a regression from the host
bash this replaced. Creating first makes the identity a fact before the
process is; asking the daemon for the status leaves the hook's own exit
code to the only party that has it.

There is no `--rm` for the same reason: a container the daemon removes the
instant it exits is a container whose exit status cannot be inspected. The
removal is this runner's own, and it happens after the status has been
read.

**The name is unique, and the unique part cannot be truncated away.** Each
launch mints 8 bytes of randomness, and the name is
`<prefix><run>-<step>-<token>` held to 200 bytes by shortening the ID
PORTION -- a hash of the id pair -- rather than the whole string. A name
cut at the end loses the token, and two concurrent steps whose ids share a
long prefix then asked for the SAME container: the second creation failed
on the name conflict and that failed attempt's cleanup removed the
container the first step was still running in.

**The envelope is unchanged.** Same fixed bash (now the one inside the
image, proven by the probe), same `--noprofile --norc`, no PTY, nothing
injected, `bash -n` as its own refusal, stdout and stderr apart with one
monotonic counter, the same `TerminationCertainty` vocabulary. #810's
contract is untouched: remote hooks stay SSH-direct, because a
third-party source host cannot be assumed to have Docker.

**Values never reach a command line.** `--env NAME` tells the client to
read the value out of its own environment, which is the block this runner
built, so the value travels over the daemon socket as data. `ps` on a NAS
is readable by every account and these values are repository passphrases.
The mirror of that: every `DOCKER_`-prefixed name is deleted from a
hook's environment, because that block IS the client's environment for
the duration of the exec, and a `DOCKER_HOST` an operator set in
`workflows.environment` would otherwise point this runner's own client at
a daemon of their choosing. The client's real settings travel as explicit
flags (`--config`, `--host`/`--context`, `--platform`), which beat the
environment in docker's own precedence.

**The engine cannot name a mount, and the operator cannot name the
socket.** The protocol still has no field that can hold a path (Decision
2), so the paths a hook may see are the RUNNER's configuration: the
per-step working directory, the script, and whatever `--hook-mount
PATH[:ro|:rw]` declares -- read-only unless the operator typed `:rw`. A
mount naming a docker socket is refused under any name, as is a socket, a
symbolic link or a relative path -- and so is any DIRECTORY the daemon
socket is in or under. `--hook-mount /run:rw` names no socket and hands
the same file to the same script, so the refusal is about the resolved
tree (both `/var/run/docker.sock` and `/run/docker.sock`, plus a
`DOCKER_HOST` unix path) rather than about the string. Mounts are IDENTITY
mounts, so `RETND_WORK_DIR` means one thing on both sides and a path in
a hook's log is a path an operator can find.

**The network can be widened but not unhinged.** `--hook-network`
accepts `none` (the default), `bridge` or a network an operator DEFINED,
and refuses `host` and `container:<id>` at startup. Those two are not a
bigger network: they put the hook inside a namespace somebody else owns,
where `host` reaches every service on this machine including the ones
bound to 127.0.0.1 -- the NAS's admin interface, this deployment's own
listeners -- and `container:<id>` makes what a hook can reach a property
of whatever that container is today, which no preflight here can
establish and none of the hardening above can constrain.

**Termination is the container's, and it is bounded.** `docker kill
--signal=TERM`, the same grace period, `docker kill`, then the look -- and
the look asks the daemon what it still has carrying this launch's INSTANCE
label (`docker ps --all --filter label=`), kills and removes whatever that
is, and reports only what it could prove. By label rather than by name,
because a removal by name is a removal of the object this runner believes
in while the label is a question about what the daemon HAS: a creation the
daemon completed after the client asking for it was killed is found here
and nowhere else.

`ps` rather than `docker inspect` for that question, which is the one
place Decision 6's ordering argument changed shape: `inspect` exits
non-zero both for "no such container" and for "no daemon", so a
termination taken during a daemon restart would be reported CONFIRMED on
the strength of an error. `ps` answers 0-with-nothing for the first and
non-zero for the second, and an unanswerable question is reported
UNCONFIRMED, which is the safe direction. Where the container's existence
itself is in doubt -- a cancelled creation -- absence is watched for the
whole confirmation window rather than believed on the first empty answer,
because such an answer proves only when it was taken.

Every call on this path has its own timeout, **including the wait for the
attached client**. `docker start --attach` talking to a daemon that has
stopped answering has no timeout of its own, so waiting for it turned a
lost lease or a shutdown into a hang with no message anywhere; when the
bound elapses the client process is killed -- safe precisely because the
client is not the container -- and the container's fate is then settled
against the daemon. A wedged daemon costs this runner certainty, never its
ability to return. The same reasoning covers the runner's OWN containers:
the capability probe and the `bash -n` syntax check are named and
labelled too, because `--rm` is a promise about an exit and a run
cancelled between creation and start leaves a container it does not cover.

This is strictly stronger than the process group it replaces. A child
that ignores `SIGTERM`, a child that changed its process group, and a
grandchild nobody knew about all go with the cgroup.

**The capability is proven once, at startup, or the runner does not
serve.** Docker present at a path fixed like bash's, the daemon reachable
as THIS account, the hook image present for this platform, and a probe
container -- started with the same hardening flags a hook gets -- that
emits the runner's own marker and reports the interpreter, the uid and
the absence of a terminal from inside. Each failure is its own sentence
with its own remedy, because "containers do not work here" sends an
operator to reinstall Docker over a group membership. The refusal carries
its own wire code (`container_unavailable`) so the engine can tell it
from a hook that failed.

There is **no fallback to host bash**, and that is structural rather than
documented: this package no longer contains the code to find a host
shell. A fallback would make every property above conditional on a daemon
nobody checked, and an operator whose hook quietly ran on the NAS itself
because the image had been garbage-collected would have no way to notice.

**The image is pinned and configurable.** `bash:5.2.37-alpine3.21` by
default -- a patch-pinned tag, not `latest`, because a floating tag is a
hook's interpreter changing under a deployment that changed nothing. The
installer fetches it and writes it into the unit; the runner refuses
rather than pulling, because a preflight that reached for a registry
would hang on a NAS with no route out. An image built for another
architecture is refused too: it would run under emulation and make the
client print a warning into every hook's own stderr.

**The one privilege this adds, and where it stops.** The runner needs to
reach the Docker daemon, which on these platforms is root-equivalent. It
is contained by belonging to the small, version-pinned, unprivileged
process that launches hook containers and to nothing else: the unit gains
`SupplementaryGroups=<the socket's group>` and the socket in
`ReadWritePaths`, and the ENGINE container gains nothing at all -- no
socket, no `group_add`, no capability, no `DOCKER_HOST`. Rootless Docker
and podman are a follow-up, not a decision made here.

**The unit names the daemon and the client the install actually used.**
The installer honours `DOCKER_HOST`, `DOCKER_CONTEXT` and `DOCKER_CONFIG`
for every check it makes, and systemd inherits none of them; the runner,
in turn, searches a fixed list of client paths rather than PATH (a
container runtime that can change with a unit-file edit is a capability
proof about a different binary). Left alone, that produced the worst kind
of success: install, preflight and pull pass against the daemon the
operator named, and then the runner probes the DEFAULT daemon through a
client it may not find, and refuses every local hook while naming an
image that was fetched. So the connection is resolved ONCE at install
time and encoded in the unit -- correctly escaped `Environment=` lines
for the winner of docker's own `DOCKER_HOST`-beats-`DOCKER_CONTEXT`
precedence, `--docker <absolute path>` on `ExecStart`, and the SAME
resolved socket in both `SupplementaryGroups` and `ReadWritePaths`.

## Consequences

- `.local.sh` hooks run on the host, as an unprivileged account, in an
  ephemeral container per hook (Decision 9), and the ENGINE's container
  security contract is byte-for-byte what it was.
- Docker is a REQUIREMENT for local hooks: a deployment with `.local.sh`
  scripts and no reachable daemon fails the installer's preflight with the
  `usermod -aG` line in it, and a runner that cannot prove the capability
  refuses to serve rather than running a hook on the host.
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

# ADR 0021: Exec-capable remote SSH execution for remote hooks

- Status: accepted
- Date: 2026-09-13
- Scope: EPIC L (#807), issue #810. Depends on the decisions ADR 0019
  (#808) settled and does not re-litigate them. Blocks #811 and #812.

## Context

A `NAME.remote.sh` hook runs on the host the backup set pulls from. That
host is reached today by exactly one mechanism: SFTP through the embedded
rclone backend, authenticated by an SSH key whose custody
`core/internal/transport/rclone` owns. The obvious implementation is
therefore to reuse that connection and ask it to run a command.

That implementation is wrong, and the reason is the posture this product
recommends. `docs/ssh-setup.md` tells an operator to give the backup
account no more than it needs, and a hardened backup source
legitimately:

- forces the account into `internal-sftp` (chrooted, no shell). Asked to
  run a command, OpenSSH answers `This service allows sftp connections
  only.` and exits 1.
- pins the key behind a forced command (`command=` in
  `authorized_keys`, or `ForceCommand` in `sshd_config`) — the rrsync
  shape. Asked to run a command, it runs somebody else's program and
  **exits 0**.

The second is the dangerous one. An executor that trusted the exit status
would report a hook as having run successfully when the hook never
existed to that server: a "quiesce the database" step that silently ran
`rrsync` and returned 0, followed by a backup of a live database.

So the question this ADR answers is not "how do we run a command over
SSH". It is "how do we know we may, and how do we know what ran".

## Decision 1: an execution connection is a first-class, separate thing

`workflows.exec_connections` is a list of named connections, each
carrying a whole `remote` block: host, port, user, the
`File`/`Env`/`Command` credential reference, `known_hosts`,
`max_connections`, `sensitive_endpoint`. A backup set's
`remote_exec_connection_ref` names one.

The reference resolves two ways, in this order:

1. a declared `workflows.exec_connections` id — an execution credential
   of its own;
2. a backup set, spelled `source/set` — reuse **that** set's source SSH
   connection.

The order is unambiguous rather than arbitrary: a declared id may not
contain `/` (refused by config validation), and `/` is the only way a
backup set is ever named.

There is deliberately **no** field for a shell to su to, a sudo user, or
a command prefix. A hook runs as the configured SSH user. This product
never escalates, and the absence of the field is how that is guaranteed
rather than promised.

### Why not simply reuse the transfer connection

Because then the SFTP-only posture above would have to be either refused
(a product that will not back up a properly hardened host) or weakened (a
product that asks an operator to give a backup account a shell in order
to use a feature). Modelling the execution connection separately is what
lets the same deployment keep an SFTP-only transfer credential **and**
run hooks, over a second account whose privileges an operator chose on
purpose.

## Decision 2: exec capability is PROVEN per run, never declared

There is no `exec_capable: true`. A configuration flag asserting a
capability is a flag that is wrong the day somebody hardens the account,
and the failure it produces is the forced-command failure above: silent.

Instead `Client.Preflight` runs, per run, before a byte of the hook is
sent, and establishes five things against the server:

1. the host identity matches the pinned `known_hosts` policy (settled
   during the handshake, recorded in the report);
2. the exec channel is permitted **and ran our bytes** — proven by
   requiring a fixed marker in the probe's own output, which neither
   `internal-sftp` nor a forced command can produce, because producing it
   requires having executed what this product sent;
3. bash is present and executable at the configured path (a missing path
   is the login shell's 127, a non-executable one its 126; both are
   reported as capability refusals naming the path);
4. the invocation works with no PTY — asserted from inside the probe,
   because "we did not ask for one" is a statement about the client and
   not about what the session got;
5. the captured bytes parse on the TARGET, with the target's own
   `bash -n`.

A refusal fails **the remote hook**, not the backup. An SFTP-only source
goes on backing up exactly as it did before this package existed, and
`core/tests/sshexecintegration` asserts both halves in one test against
one account: the artifact is transferred back and compared byte for byte
over the very credential whose exec request is refused.

### The proof is a capability, and running REQUIRES one

`Preflight` returns an opaque `Capability` bound to two things: the
connection it measured (the SSH session identifier, which a reconnect
changes) and the sha256 of the exact script bytes it measured them with.
`Run` takes one or takes the preflight itself, and refuses a capability
that matches neither — so there is no exported path that starts a hook on
a connection whose capability is merely assumed, and no way to reach one
by forgetting a call.

The alternative — a preflight a caller is trusted to have run — fails in
exactly the direction this decision exists to prevent. On a
forced-command account the exec request is ACCEPTED, somebody else's
program runs and the session exits 0, so a `Run` that began with "the
request was accepted" would record every remote hook on that account as
having completed successfully while none of them ever ran. The binding to
the script bytes is the same argument one level down: the syntax check is
part of the proof, so honouring a proof taken for other bytes would run a
script nothing has parsed on that host.

### The limit, stated

Preflight detects contamination it can see. `BASH_ENV` and `ENV` arriving
set are refused, because bash sources them at STARTUP — before any line
of the payload can unset them — so by the time a hook runs, a file of
somebody else's choosing has already run. `SHELLOPTS`/`BASHOPTS` are
refused only for the options that change what a script MEANS (`errexit`,
`nounset`, `pipefail`, `xtrace`, `posix`, …), because bash sets
`braceexpand` and `hashall` on every non-interactive shell and refusing a
non-empty `SHELLOPTS` would refuse every host on earth.

What it cannot detect is a login shell that does something invisible and
stateful before the fixed command — writes a file, opens a socket, starts
a process. The remote account's startup files are that account's trust
boundary, and this is the honest boundary of the mechanism rather than a
gap to be closed later.

## Decision 3: no PTY, and therefore a reaper

No pseudo-terminal is ever requested. A PTY would give sshd a process
group to hang up, which would make termination trivial — and it would
also merge the hook's stdout and stderr into one stream, irreversibly.
Two separate streams is the requirement that wins (#812 consumes them),
so the easier termination is given up on purpose.

The consequence is measured rather than assumed: without a PTY, closing
an SSH channel does not reliably kill what was running behind it. So
termination is a sequence, each step covering what the last cannot:

1. a **reaper** session: it finds the step's process GROUP and signals it
   `TERM` and then `KILL`. Measured on the fixture, sshd puts the exec'd
   command in its own process group, distinct from sshd's own, so this
   reaches the hook and everything it started and nothing of the
   server's. A `pgid` is only used if it parses as a number greater than
   1: `kill -TERM -1` would signal every process the account owns.
2. an exec-channel signal request (`SSH_MSG_CHANNEL_REQUEST "signal"`).
   Measured against OpenSSH 10 it is honoured and the session ends in
   milliseconds. Older and other servers do not implement it, so this is
   an optimisation, not the mechanism.
3. closing the channel, which is all that is left.

### Why the reaper goes first, and how the group is found twice

The signal request looks like the cheap step to try first, and it is the
one that destroys the mechanism behind it. OpenSSH signals the process
GROUP: bash, which does not ignore `TERM`, dies at once, while a child
that DOES ignore it carries on holding the channel's stdout. The step's
token lives on the leader's command line and nowhere else, so a reaper
running after that finds nothing, kills nothing, and reports a group that
is still running as "no process was there". Reaping first costs one short
session in the case where the signal alone would have sufficed.

The group is therefore identified two ways, because each fails where the
other works:

- the token, matched as the `-s <token>` OPERAND of the remote command
  rather than as a substring of it. Step tokens are related strings by
  construction, so a substring match lets one step's reaper kill a
  different step's process group — a hook running normally on another
  backup set — while reporting a successful termination of its own.
- the `pgid`, captured by a short scan a quarter of a second into the
  step (and once more at a second and a half if the first found nothing),
  so that a hook whose leader has EXITED — leaving a child holding the
  channel — is still reachable. The scan is skipped entirely for a step
  that ends before it, which is most of them.

## Decision 4: certainty is a fact, with one rule

`confirmed` means the session COMPLETED after termination was requested:
the exit was reported and both output streams reached end of file, so
nothing of the step still holds the channel. `unconfirmed` means it did
not. There is no third answer and deliberately no spelling of "probably
gone", because a hook still touching the thing it was quiescing while the
backup proceeds is the worst case a workflow has, and a hedge recorded
there would be read as a pass.

One rule, not a table keyed on which termination step ran. That is what
makes a **deliberately detached** descendant (`nohup`, `setsid`, a double
fork) come out right without a special case: such a child is outside the
process group the reaper can reach and it keeps the channel's stdout
open, so the session never completes and the step records `unconfirmed`.
Detached descendants are explicitly outside the guarantee, and they are
outside it in the honest direction.

## Decision 5: the envelope, and why the script is not uploaded

The remote command is FIXED:

```
exec <validated bash path> --noprofile --norc -s <step token>
```

Nothing else is ever sent as a command. The bash path must match
`^/[A-Za-z0-9._/+-]*[A-Za-z0-9._+-]$` and the token
`^[A-Za-z0-9._-]{1,120}$`; a value needing shell quoting to be safe is
refused rather than quoted, because the remote command is parsed by the
remote account's login shell.

The token is the ONLY variable part, and it is the only thing that may be
there: the remote command line is world-readable in the remote process
list, so an environment value or a secret placed there would be published
to every account on that host. A token identifies a run, not a
credential. `core/tests/sshexecintegration` samples the remote process
table while a hook is running and asserts exactly this.

The environment and the script arrive on the channel's **stdin**, as
shell text (`internal/workflowexec.StdinPayload`):

```
unset BASH_ENV ENV SHELLOPTS BASHOPTS 2>/dev/null
for __backupd_name in $(compgen -e 2>/dev/null); do ... unset -v ... done
for __backupd_name in $(compgen -A function 2>/dev/null); do unset -f ... done
export PATH='...'        # workflow.SanitizedBaseline, then the resolved plan
export NAME='...'
__backupd_script='...'
eval "$__backupd_script" 0</dev/null
```

- Every value, and the script itself, is a single-quoted literal. Inside
  single quotes a POSIX shell interprets nothing — no expansion, no
  substitution, no escapes — and the only byte needing handling is the
  apostrophe, spelled by closing the literal, escaping it outside, and
  reopening. That is the whole injection defence, and it is the reason a
  value containing `$(...)`, backticks, quotes, backslashes or newlines
  arrives byte for byte. The suite proves it with a SHA-256 round trip
  per value.
- The prologue CLEARS before it sets. An exec channel inherits whatever
  the server and the account exported — `HOME`, `USER`, `LANG`, `MAIL`,
  `SSH_CONNECTION`, any `SetEnv`, any exported function — and a hook run
  by #809 on this backup server sees none of them, because that executor
  hands execve a block built from `workflow.SanitizedBaseline`: one
  variable, `PATH`. An envelope that only layered the plan on top of the
  account's environment would make the same script mean two different
  things on the two executors, which is the one thing a shared envelope
  exists to prevent. `PWD`, `OLDPWD`, `SHLVL` and `_` are kept: bash sets
  those itself at startup whichever way it was invoked, so the hook run
  under execve has them too. The preflight refuses a bash that cannot
  enumerate its own exported variables, because a clearing loop that
  silently cleared nothing would be the environment version of the forced
  command above. The suite asserts `SSH_CONNECTION` is ABSENT from a
  hook's environment while the declared variables are present.
- The four startup variables are removed from EVERY layer, not only from
  the baseline: an operator can write `BASH_ENV` in
  `workflows.environment`, and by the time either encoding sees the
  merged environment there is nothing left to say where an entry came
  from. Stripping in the one function both encodings parse with is what
  stops the remote payload exporting one AFTER its own unset line.
- `SendEnv`/`AcceptEnv` is not used at all. A hardened sshd refuses env
  requests unless configured for those exact names, so an envelope
  depending on it would work on a laptop and not on the host that matters.
- No file is written on the remote host at any point. No upload, no
  here-document (bash implements one with a temporary file), and
  therefore no residue to clean up and nothing left if the connection
  dies mid-step. The suite asserts it by searching the remote host for
  the script's own bytes rather than by diffing a directory listing.
- NUL is refused in a name, a value and the script bytes. bash does not
  treat a NUL in a script as fatal — it warns and drops it — and "warns
  and drops" would mean the bytes that ran are not the bytes that were
  hashed, which destroys the audit trail ADR 0019 built.
- NOTHING is injected: no `set -e`, no `nounset`, no `pipefail`, no
  `xtrace`. A hook's bytes run exactly as captured, because a shell
  option this product added silently changes what somebody else's script
  means.

### The two costs of `eval`, accepted

**Why the script is a literal at all.** bash reads a script from a
non-seekable stdin one command at a time, so a `read` builtin in the
payload competes with bash's own parser for the same bytes — measured: a
`read` swallows the next line of the script as its data. A single-quoted
literal that bash PARSES (rather than data something reads) removes the
race entirely, and needs no temporary file.

**Cost 1: the hook's stdin.** `0</dev/null` gives the hook the same
closed stdin the host runner (#809) gives it. Without the redirection a
hook that read stdin would be reading the remainder of its own script.

**Cost 2: runtime line numbers.** A diagnostic bash prints while the hook
runs carries a line number offset by the payload's prologue. Syntax
errors do not, because they are caught by the `bash -n` preflight against
the captured bytes alone, where the line numbers are the script's own.
That is why the preflight check is worth its extra session.

## Decision 6: transport loss is never an exit code

`Result.ExitCode` is a `*int` and is nil unless this product actually
observed a status. A step killed on timeout, a cancelled step, a channel
that closed without an exit message and a dropped connection all leave it
nil, and the audit line prints `exit=unobserved` rather than a number.

`*ssh.ExitError` with a signal name is reported as `ErrStepSignaled` and
NOT as `128+n`: inventing that number would be this product making up a
status the hook never returned.

## Decision 7: one owner for credential custody, and the one property it loses

`internal/transport/rclone.SourceSigner` and `SourceKnownHostsFile` are
exported, and `sftpConfig`'s credential switch is extracted into
`resolveSourceCredential`, which both the SFTP transport and this
executor call. Key custody has one owner: the
exactly-one-of-three rule, the key file's mode check (#293), the whole
directory-chain check (#311), #298's at-rest decryption, and the
passphrase resolution (#269) are decisions with arguments behind them,
and a second package building its own SSH identity would re-derive them
or quietly skip some.

One property cannot be kept. `key_file`'s documented advantage is that
this process never opens it — rclone's backend reads the file, so the key
never enters this program's memory. An exec client has no external binary
doing the SSH for it: it IS the SSH client, so a signer is built here,
from bytes read here. This is a real if small widening of where key
material lives (a core dump or an attached debugger during a remote hook
can see the key, which was not true of a transfer), and it is recorded
rather than glossed. Every other control is unchanged, and a deployment
wanting the narrower property has the same options it always had for the
transfer credential — `key_env`, `key_command` — neither of which ever
had it either.

## Decision 8: the audit line holds facts and cannot hold credentials

`remoteexec.Audit` carries the backup set, the connection reference and
which resolution rule produced it, the host, the SSH user, the host key
fingerprint the server actually presented, the step id, the script name
and its sha256, start and end, the exit code when one was observed, the
signal when one killed it, the termination certainty and the outcome.

It has no field for a key, a passphrase, an environment value or a script
body, which is how "no credentials in the audit" is enforced: `String`
cannot print what the struct cannot hold. The test asserts the absence of
material that WAS in play for the step rather than of an invented string.

## Consequences

- A deployment whose source is SFTP-only must declare a separate
  execution connection to use remote hooks. That is more configuration,
  and it is the configuration that makes the privilege explicit.
- Every remote step costs three short sessions before the hook: the
  capability probe, the syntax check, and then the run. A step that
  outlives a quarter of a second costs one more, the process-group scan,
  and a step that has to be STOPPED costs a reaper session as well.
  Measured against the fixture, the two preflight sessions are ~150 ms
  together. A caller that has already run `Preflight` for the same bytes
  hands the resulting capability to `Run` and pays for them once.
- An execution connection marked `sensitive_endpoint` is redacted in logs
  and journal details exactly as a transfer remote is (#295):
  `internal/app`'s translation walks the declared exec connections as
  well as the backup sets, because the exec path names the endpoint in
  its dial errors and in every per-step audit line.
- `max_connections` is honoured by construction: one client holds exactly
  one TCP connection and opens sessions on it.
- #811 sequences these steps and owns cleanup; #812 consumes the capture
  streams; #813/#814 render what this records. None of them may
  re-decide the envelope: it is shared with #809 through
  `internal/workflowexec` so that a hook moved between the two executors
  means the same thing.

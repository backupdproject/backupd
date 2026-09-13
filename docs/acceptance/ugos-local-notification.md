# Hardware acceptance: UGOS local notification delivery

Work package B3.5 (`docs/EPIC-B-multi-nas.md` §71) picks the first of the
two mechanisms §71 offers: the platform's own local notification
capability, reached through `capabilities.Notifier`. Everything below the
notifier is covered by automated tests. The notifier itself, on real UGOS,
is not, and cannot be from CI.

§82's hardware-only template says the procedure is written first, before
the PoC exists. This is that procedure. It is deliberately written now,
while the software side is fresh, and executed later against an authorized
NAS once a UGOS `PlatformAdapter` declaring `NativeNotifications` exists.

## What is already proven without hardware

Nothing below re-tests any of this. It is listed so the hardware run stays
narrow, and so nobody executes it expecting it to prove more than it does.

- `core/internal/alert` decides which conditions alert and how often
  (`alert_test.go`, `conditions_test.go`), and cannot grow into a general
  notification framework or reach a deletion path (`mechanism_test.go`).
- A real `internal/health.Report`, computed from journal rows a cycle just
  wrote, drives an actual dispatch, and a changed host key alerts on top
  of the connection layer's refusal rather than instead of it
  (`core/internal/app/alerts_test.go`).
- The opt-in gate works from both sides: the config block
  (`core/internal/config/alerts_test.go`) and the service seam
  (`core/service/alerts_test.go`).
- The adapter hands `Alert.Title` and `Alert.Message` to
  `capabilities.Notifier` unchanged, refuses a platform that has not
  declared the capability, and surfaces a delivery failure rather than
  swallowing it (`apps/common/platform/notify/notify_test.go`).

## What this procedure decides

One question: does a UGOS `Notifier` implementation actually put a
backupd alert in front of an administrator who is not looking at
the app, and does it survive the conditions that matter (app in the
background, browser closed, container restarted)?

That question is the architecture assumption under §71's own preference
order. If the answer is no, the assumption fails and the fallback is
§71's second option, one explicit opt-in generic mechanism, which is a
separate work package.

## What this procedure cannot decide yet

Two of the four conditions are not reachable on anything this repository
ships today, and pretending otherwise would produce a false reject of the
notification mechanism itself:

- **Every condition, on the generic image.** `apps/generic`'s adapter
  declares no capability at all, so `notify.NewPlatformSink` refuses it
  and `backupd-web serve` prints "proactive alerting is off" on
  every start. The §37 headless Docker distribution therefore has no
  proactive alerting for any of the four conditions, and §71's second
  option (one explicit opt-in generic mechanism) has not been built. This
  procedure needs precondition 2, a real UGOS adapter, before any of it
  can run at all.
- **Critical storage pressure specifically.** `Service.Capacity` has no
  configuration wiring: `internal/config` has no `warning_free_bytes`,
  `critical_free_bytes` or `safety_margin_bytes` key, so every shipped
  binary runs the zero-value thresholds. With those,
  `capacity.AssessCurrent` reaches `Critical` only when the filesystem
  reports no available bytes at all, which is after the point where a
  warning would have helped. Step 4 below is blocked on FR-21's threshold
  configuration (see) and must not be executed, or marked failed,
  before it lands. `core/internal/app/alerts_test.go` pins that shipped
  default so it is a recorded fact rather than a surprise on the day.

## Preconditions

1. An authorized UGOS Pro NAS. Never someone else's production NAS, and
   never one holding backups whose loss would matter.
2. A UGOS `PlatformAdapter` (apps/ugos) whose `Capabilities` returns
   `NativeNotifications: true` and whose `Notifier` calls the real UGOS
   notification API.
3. The engine container running from a build that includes this work
   package, with `alerts.enabled: true` in its config file.
4. At least one configured backup set with a short `stale_after` (a few
   minutes), so staleness can be produced on a coffee-break timescale
   rather than a daily one.
5. An administrator account on the NAS that is NOT the one running the
   test, so "did an administrator actually see it" is answered by a person
   rather than by the person who triggered it.

## Procedure

Record, for every step: wall-clock time, the engine's own `alert` log line
(`event: "alert"`, with `alert_kind` and `backup_set`), and whether the
notification actually appeared, where, and for whom.

### 1. Stale backup

1. Let the backup set produce one good artifact, then stop the source
   producing new ones.
2. Wait past `stale_after` plus one poll interval.
3. Expect: exactly one notification, kind `STALE_BACKUP`, naming the
   backup set.
4. Wait three more poll intervals without fixing anything.
5. Expect: no further notification. This is the de-duplication contract;
   an alert per poll is a failure, not a partial pass.
6. Resume the source, let a fresh artifact land, then stop it again and
   wait past `stale_after` a second time.
7. Expect: one new notification. A condition that resolved and recurred
   must alert again.

### 2. Repeated failure

1. Make the backup set fail repeatedly (revoke read permission on the
   remote artifacts is the least destructive way).
2. Wait until `repeated_failure_threshold` artifacts are in `FAILED`, or
   until the set reads `FAILING`.
3. Expect: exactly one notification, kind `REPEATED_FAILURE`.

### 3. Changed SSH host key

1. Regenerate the source host's SSH host key, or point the backup set at a
   different host reusing the same `known_hosts` entry.
2. Wait one poll interval.
3. Expect: exactly one notification, kind `HOST_KEY_CHANGED`.
4. Expect, and confirm in the journal and the logs: the connection was
   refused and no backup ran. §77 invariant #5 means the alert reports the
   refusal, it never resolves it. If any artifact transferred, this is a
   failed run and a security finding, not a notification bug.
5. Confirm the notification does not contain key material, a path to a
   private key, or any credential.

### 4. Critical storage pressure

> **Blocked, do not execute yet.** There is no `critical_free_bytes` to
> fill down to: FR-21's thresholds are not configurable, so the shipped
> value is zero and this condition fires only at literally zero available
> bytes. Executing the steps below as written produces a false reject of
> the whole notification mechanism. Run this section once the threshold
> configuration lands, with step 1 reading the configured value.

1. Fill the destination filesystem (a large sparse file outside the
   managed backup root) until free space is at or below the configured
   `critical_free_bytes`.
2. Wait one poll interval.
3. Expect: exactly one notification, kind `CRITICAL_STORAGE_PRESSURE`.
4. Expect: nothing was deleted to make room. Compare the artifact list
   before and after. §71 and `internal/capacity` both require the alert to
   be a report, never a licence to free space, and this is the step that
   proves it on real hardware.
5. Remove the filler file, wait a poll interval, refill it.
6. Expect: one new notification.

### 5. Delivery conditions

Repeat step 1 under each of these, one at a time:

1. The backupd UI closed, no browser open to the NAS at all.
2. The administrator logged out of UGOS.
3. Immediately after an engine container restart.
4. On a second administrator account that never opened the app.

Each one either delivers or it does not. "It works when the app is open"
is not a pass: §71's whole point is the administrator who is not watching.

### 6. Opt-out

1. Set `alerts.enabled: false` and restart the engine.
2. Repeat step 1.
3. Expect: no notification, and the startup line saying alerting is off.

## Local workflow hooks are unavailable on UGOS Pro

A workflow step whose target is `local` does not run in the engine container, and since
issue #865 it does not run on a host shell either: it runs in an **ephemeral Docker
container** launched by the **Host Workflow Runner**, a small version-pinned process
systemd supervises as `backupd-workflow-runner.service`
(`docs/adr/0020-host-workflow-runner.md`, `docs/runtime-contract.md`).

UGOS Pro is a closed appliance: it offers no supported way to install a host unit or to
add an account to the Docker socket's group. The UPK that #83 will produce does not
change that — a package is not a host session — so the answer here is settled
independently of that issue.

**Local workflow hooks are unavailable on this platform.** That is a refusal with a
named mechanism rather than a gap, and it is worth being precise about which
mechanism, because two plausible ones are not it:

- the **capability contract** answers `unavailable` for this platform, with this
  reason and the alternative below
  (`apps/common/platform/capabilities`, `LocalHooks`);
- the **engine** refuses a `NAME.local.sh` step outright when a deployment has no
  host workflow runner behind it — *"this deployment has no host workflow runner,
  and a NAME.local.sh has nowhere to run"*
  (`core/internal/workflowrun/engine.go`). A hook is refused, never skipped, so a
  run cannot report success with the hook quietly missing;
- and **this procedure installs no `backupd-workflow-runner.service`**, grants no
  group and fetches no hook image.

What does **not** happen, so that nobody goes looking for it:
`scripts/install/install_docker_host.py` has no platform gate. It refuses (exit 12)
when a deployment has hook scripts and the runner's account cannot reach a Docker
daemon, and on a host with no systemd it merely stages the unit file for an operator
to install by hand. Neither of those is a refusal on this platform's grounds. The
answer here is the contract's, and the enforcement is the engine's.

**What works instead:** a remote workflow step. A step with a remote target runs over
SSH (`docs/adr/0021-remote-ssh-exec.md`) against a machine you do
administer, and needs no Docker and no host unit on this NAS. The engine's own side
of this is unchanged either way: the shipped package asks for no Docker socket, no
`group_add` and no `DOCKER_HOST`.

- [ ] No `backupd-workflow-runner.service` exists on this host, and nothing in this
      procedure created one
- [ ] No account was added to a Docker socket group for this product, and the shipped
      containers mount no socket and declare no `group_add`
- [ ] A workflow configured with a `local` hook is refused with a message naming the
      missing container runtime, and the refusal text is recorded — not a run that
      reported success with the hook skipped


## Evidence to record

- Screenshots of each notification as the administrator saw it, with the
  NAS clock visible.
- The engine's NDJSON log lines for each run, filtered to `event: "alert"`
  and `event: "error"`.
- The artifact list before and after step 4, proving nothing was deleted.
- UGOS version, model, and the image digest under test.

Store the evidence with the issue this procedure is executed for. Do not
store any credential, key, or token alongside it.

## Accept / reject

**Accept** the architecture assumption when every condition in steps 1 to
4 delivered exactly one notification, de-duplication held, recurrence
re-alerted, step 4 deleted nothing, step 5 delivered with nobody watching,
and step 6 stayed silent.

**Reject** it, and fall back to §71's second option, if native delivery
only works while the app is open, requires a session, is silently dropped,
or cannot be reached from the container at all. Record which, because that
determines what the fallback mechanism has to do differently.

A partial result is a reject. There is no useful middle state for an alert
that sometimes arrives.

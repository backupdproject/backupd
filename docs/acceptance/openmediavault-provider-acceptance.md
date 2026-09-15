# OpenMediaVault provider acceptance procedure

**Status: NOT EXECUTED. OpenMediaVault is build-supported and uncertified (§68).**

This is the procedure an operator runs on a real OMV system to certify the
deployment profile in `apps/openmediavault/`. I wrote it before the compose assets
existed, per §68, and nothing in it has been run: no OMV instance, VM or physical,
was available on the machine the profile was built on.

Required evidence per §68: a **current OMV 8.x Debian-based test system**. Record
the exact release in the evidence table.

Everything the repository itself can decide is already checked by
`distribution/packaging` on every commit. This procedure covers only what a laptop
cannot reach.

---

## Scope: no native plugin

Section 4A defers a native OMV Workbench plugin, and WP4.3 says "Do NOT implement
a native OMV plugin in v1". This procedure therefore certifies a **supported
Compose deployment** and nothing more. OMV sits in Tier C (a supported provider
deployment profile), one tier below TrueNAS and Unraid.

Concretely that means: no entry in OMV's own navigation tree, no Workbench form,
no RPC service, no `salt` state, and no `openmediavault-backupmanager` Debian
package. If any of those appear in `apps/openmediavault/`, the package has
overrun its scope and `distribution/packaging` will fail the build before anyone
gets here.

The Web UI is reached by its own published port, and step 3 covers documenting
that clearly enough that an operator finds it without a navigation entry.

---

## Step 0 — Prerequisites

### 0.1 Install the Compose plugin

```bash
omv-extras   # enable the omv-extras repository if it is not already
apt-get install openmediavault-compose
```

- [ ] **Services → Compose** appears in the OMV Workbench
- [ ] `docker --version` and `docker compose version` both work over SSH

### 0.2 Make the canonical image resolvable

`ghcr.io/backupdproject/backupd:0.4.0` is cut but not pushed yet:
`distribution/packaging/canonical.json` records `image.published: false`, and
`container/release-manifest.json` carries a `registry_digest` of `null` per
architecture. So the reference does not resolve from the registry today, and the
steps below are how you make it resolve, by pushing a build to a registry this host
can reach or building elsewhere and loading it. The previous release,
`ghcr.io/backupdproject/backupd:0.3.3`, stays published and signed if you would
rather run that. Either push to your own registry:

```bash
docker buildx build \
  --platform=linux/amd64,linux/arm64 \
  --build-arg VERSION="$(git describe --tags --always)" \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  -f container/Dockerfile \
  -t <your-registry>/backupd:<version> \
  --push .
```

or side-load and set `IMAGE` in the env file to the loaded tag:

```bash
docker save backupd:<version> | gzip > backupd.tar.gz
scp backupd.tar.gz root@<omv>:/root/
ssh root@<omv> 'gunzip -c /root/backupd.tar.gz | docker load'
```

The compose file reads the image reference from a single `IMAGE` variable in
`apps/openmediavault/compose/retnd.env`, so this is a one-line change in
one file, never an edit scattered through the compose YAML.

- [ ] Canonical image resolvable on the NAS, reference recorded

### 0.3 Resolve the real filesystem paths

The host-path defaults in `distribution/packaging/canonical.json`
(`platforms.openmediavault.hostPaths`) use `/srv/dev-disk-by-uuid/...`, which is a
**placeholder**, deliberately matching what the OMV frontend bridge already
declares. A real OMV system mounts data filesystems at
`/srv/dev-disk-by-uuid-<UUID>/`, with the UUID differing per machine, so no
checked-in default can be literally correct.

Find yours:

```bash
ls -d /srv/dev-disk-by-uuid-*
```

Set `DISK` in `retnd.env` and change nothing else. Every host path in
the compose file is written `${DISK}/...`, so the UUID appears exactly once, and
the compose file itself needs no editing. `DISK` is referenced in the
fail-closed `${DISK:?...}` form, so leaving it unset or misspelling it stops the
deployment instead of creating five directories in the wrong place.

```bash
DISK=/srv/dev-disk-by-uuid-<your-uuid>
mkdir -p "$DISK/appdata/backupd"/{state,config,secrets}
mkdir -p "$DISK/backups/backupd"
chmod 700 "$DISK/appdata/backupd/secrets"
```

The backup root is `backupd` **inside** `$DISK/backups`, not that
directory itself, which is very likely one you already use. Every step below
creates, owns and later inspects only paths this procedure created.

- [ ] Real `dev-disk-by-uuid-<UUID>` path recorded
- [ ] `appdata/backupd/{state,config,secrets}` and
      `backups/backupd` exist
- [ ] The UUID appears in exactly one place on the NAS, the env file's `DISK`
- [ ] Starting the stack with `DISK` unset fails loudly rather than creating
      paths (try it once, on purpose)

### 0.4 Own them by the uid/gid the app runs as

The runtime image is distroless: no shell, no root step, no init process, so
nothing inside the container can chown these at startup.

OMV's conventional service account is `uid 1000` for the first admin account;
check yours with `id <your-admin-user>`.

```bash
chown -R 1000:100 "$DISK/appdata/backupd"
chown 1000:100 "$DISK/backups/backupd"
```

Only paths this procedure created, and the backup root non-recursively. A
`chown -R` across `$DISK/backups` would rewrite the ownership of everything
already in it, fights the Workbench's own shared-folder ACL management, and on a
reinstall would rewrite the retained backup store.

- [ ] `PUID`/`PGID` chosen, recorded, and set in the env file
- [ ] appdata tree and `backups/backupd` owned by that uid/gid
- [ ] Nothing else under `$DISK/backups` had its ownership changed

### 0.5 Create the SSH key, the pinned known_hosts, and the config

> **This step is packaging debt now, not engine behavior.** As of issue #176 the
> engine no longer needs a configuration to start: an instance with no
> `config.yaml` serves a first-run setup flow, in the web UI, that writes one
> for you. What still blocks that here is the package. The config is
> bind-mounted as a single **read-only file**, so the container cannot create
> it, and a bind mount cannot express "this file does not exist yet" either (a
> missing source gets a directory created for it). Until the packaged config
> mount becomes a writable **directory**, this platform keeps the hand-written
> config, and this step keeps its shell commands for that reason and no other.
> That same mount is already what makes the existing create-backup-set (#146)
> and settings (#140) write paths inert in a packaged container, so it is one
> packaging fix for three things.
>
> Nothing else here survives that fix: once the mount is writable, the key is
> pasted into the setup flow's Connection test step, the host key is probed and
> confirmed on that same step, and no `config.yaml` is written by hand
> at all.

`/retnd-web serve` starts without a `config.yaml` and serves the
first-run setup flow instead (#176), but a config file that EXISTS and does not
validate is still a hard startup failure. Given the read-only mount above, create
all three before the first start.

**What still requires this step, precisely (issue #196).** The configuration mount is
now a writable directory the application owns, so the container can create and replace
`config.yaml` itself, and an empty directory is a legitimate state rather than a broken
deployment. Two things nonetheless keep this step here. The directory itself must exist
and be owned by the app's uid/gid before the first start, because a bind mount does not
create or chown its source. And `/retnd-web serve` still refuses to start
without a valid config: removing that refusal, and serving a first-run flow instead, is
#176's work and is not merged. Once it is, everything below except creating and owning
the directory becomes optional.

```bash
ssh-keygen -t ed25519 -N '' -f "$DISK/appdata/backupd/secrets/id_ed25519"
ssh-keyscan -t ed25519 <your-sftp-host> > "$DISK/appdata/backupd/secrets/known_hosts"
chmod 600 "$DISK/appdata/backupd/secrets/id_ed25519"
chown 1000:100 "$DISK/appdata/backupd/secrets/"*
```

Verify the host key fingerprint out of band. Then write
`$DISK/appdata/backupd/config/config.yaml` using the annotated example in
`apps/openmediavault/README.md`.

**Never commit the private key, the config, or any transcript containing them.**

- [ ] Key pair generated, mode 0600, owned by `PUID:PGID`
- [ ] `known_hosts` pinned, fingerprint verified out of band
- [ ] `$DISK/appdata/backupd/config` exists and is **writable** by `PUID:PGID`
- [ ] `config.yaml` written inside it and readable by `PUID:PGID`

---

## Step 1 — Install

1. **Services → Compose → Files → Add**.
2. Name: `retnd`.
3. Paste `apps/openmediavault/compose/retnd.yml` into the **File** field.
4. Paste `apps/openmediavault/compose/retnd.env`, with your step 0
   substitutions, into the **Environment** field.
5. Save, then **Up**.

- [ ] The compose file saves with no validation error
- [ ] `Up` completes and both services reach **running**
- [ ] The engine service reaches health **healthy** (it declares the
      liveness probe, `/retnd-web healthcheck --url
      http://127.0.0.1:8080/health/live`, and NOT the image's own
      `HEALTHCHECK`, `/retnd status`. The Web UI will not start until
      this reports healthy, and `status` is the backup-freshness verdict, which
      is non-zero on a fresh install that has backed nothing up)
- [ ] The Web UI service reaches health **healthy** via its own
      `/retnd-web healthcheck` override, not the image's
      `/retnd status` (which would fail: no config, no state database)
- [ ] The engine service publishes no port (`docker compose ps` shows a port
      mapping only for the Web UI service)

---

## Step 2 — Verify from the Workbench

- [ ] **Services → Compose → Files** lists `retnd` with status up
- [ ] The plugin's **Logs** action shows both services' output
- [ ] No error, warning or orphan-container notice appears in
      **System → Notifications**

---

## Step 3 — Web UI access

OMV has no navigation entry for this app, by design. Everything an operator needs
to find it must therefore be in the documentation.

1. Open `http://<omv-host>:<published port>/` in a browser.

- [ ] The shared Web UI loads
- [ ] `apps/openmediavault/README.md` states the exact URL shape, states that the
      port is set in one place in the env file, and states plainly that there is
      no OMV navigation entry and why
- [ ] The published port does not collide with OMV's own Workbench port, and the
      README says what to change if it does
- [ ] Nothing in OMV's own navigation tree, dashboard or service list was
      modified by this install

---

## Step 4 — Authentication (local-account only)

OMV gets no auth of its own. It uses the reusable local authentication the generic
Web host provides (§13A).

1. Read the one-time enrollment link out of the **engine** service's log:

   ```bash
   docker compose -p retnd logs retnd 2>&1 | grep -i enroll
   ```

2. Open it, enrol an administrator with a password you generate now, log out, log
   back in, then open the enrollment link a second time. Enrollment also asks for
   a recovery email address and the SMTP details to reach it: use a mail account
   you control, and keep that password out of this repository.
3. Submit once with a deliberately wrong SMTP port before the good attempt, and
   once signed out again, use **Forgot password** with an invented username and
   then with the real one.

- [ ] No account exists before enrollment
- [ ] The token appears only in the service log, never in any file under
      `apps/openmediavault/`
- [ ] The wrong-port attempt fails with `SMTP_SEND_FAILED`, creates no account, and
      leaves the same enrollment link usable
- [ ] A confirmation message reaches the recovery address before the account exists
- [ ] Enrollment succeeds, logout then login succeeds
- [ ] The enrollment link is refused the second time
- [ ] Forgot password answers the same for an invented username as for the real one,
      and the reset link works once and signs every session out when spent
- [ ] `GET /api/v1/system/capabilities` reports `nativeAuth: false`
- [ ] `$DISK/appdata/backupd/state/local-auth.json` holds an Argon2id
      hash, never a plaintext password, and holds the recovery address and SMTP
      settings with the SMTP password as a secret reference rather than a value:
      `grep` it for the password you typed and find nothing
- [ ] retnd's login is completely independent of the OMV Workbench
      login, and neither can log into the other

---

## Step 5 — Storage mapping and backup-root containment

Run one backup cycle to completion, then:

```bash
ls -la "$DISK/backups/backupd"
ls -la "$DISK/appdata/backupd/state"
grep -rIl 'PRIVATE KEY' "$DISK/backups/backupd" || echo "clean"
```

Then record a baseline for the removal check at the end of this procedure. The
removal criterion is that the backup root is untouched, and a criterion with
nothing to compare against is one an operator ticks off a directory listing: a
partial deletion, a truncated artifact or a silently rewritten file would all
pass it. So write a canary of known content into the backup root, and record its
hash and a full file listing **outside** the backup root, where whatever might
damage that tree cannot reach the evidence:

```bash
mkdir -p /root/backupd-acceptance
head -c 8M /dev/urandom > "$DISK/backups/backupd"/canary.bin
sha256sum "$DISK/backups/backupd"/canary.bin | tee /root/backupd-acceptance/canary.sha256
find "$DISK/backups/backupd" -type f -printf '%p %s\n' | sort > /root/backupd-acceptance/backup-root.before
```

Keep `/root/backupd-acceptance` off the repository: the listing names your own backup
sets. Record only that it was taken, and the canary's hash, in the evidence table.

- [ ] At least one completed artifact is under `$DISK/backups/backupd`
- [ ] `state.db` and `local-auth.json` are under appdata, **not** under the
      backup root
- [ ] No private key, `known_hosts`, or auth state anywhere under
      `$DISK/backups/backupd` (§19.2)
- [ ] Nothing was written anywhere else under `$DISK/backups`
- [ ] A sidecar recovery manifest sits next to the artifact and contains no
      secret material (§19.3)
- [ ] `canary.bin` written into the backup root and its hash recorded outside it
- [ ] A full `find` listing of the backup root recorded outside it

---

## Step 6 — Update

1. Capture a baseline first, over SSH to the OMV box, so the checks below are a
   comparison rather than an impression:
   ```bash
   sha256sum $DISK/appdata/backupd/state/state.db | tee /tmp/before-update.sha256
   find $DISK/backups -type f -printf '%p %s\n' | sort > /tmp/before-update.txt
   ```
2. Push or side-load a newer image tag and change `IMAGE` in the env file.
3. **Services → Compose → Files → retnd → Pull**, then **Up**.
4. Compare afterwards:
   ```bash
   find $DISK/backups -type f -printf '%p %s\n' | sort > /tmp/after-update.txt
   diff /tmp/before-update.txt /tmp/after-update.txt
   ```

- [ ] Pull and Up both complete, and both services return to healthy
- [ ] `diff` of the retained-artifact listing is empty: the update moved no
      backup data
- [ ] The administrator account still exists (no re-enrollment prompt), with its
      recovery address and SMTP settings intact
- [ ] Logging back in with the same password works
- [ ] Every backup set is still configured
- [ ] Every artifact is still present and still listed
- [ ] The old image can be pruned without affecting the running deployment

---

## Step 7 — Container replacement

Same image, destroyed and recreated containers. This is what an OMV reboot, a
`Down` then `Up`, or a `docker system prune` does.

```bash
docker compose -p retnd down
docker compose -p retnd up -d
```

- [ ] Both services come back healthy
- [ ] Retained backup data survives untouched
- [ ] The catalog survives
- [ ] The administrator account survives, recovery address and SMTP settings included

---

## Step 8 — Remove

This is the destructive-safety step. Its evidence was captured back in the
storage step, because after the removal there is nothing left to compare
against, and any deletion the comparison turns up is a release blocker rather
than a finding to triage.

1. **Services → Compose → Files → retnd → Down**.
2. Then **Delete** the file entry.

- [ ] Both containers are gone

Check the backup root against the baseline recorded in the storage step, before
looking at anything else:

```bash
sha256sum -c /root/backupd-acceptance/canary.sha256
find "$DISK/backups/backupd" -type f -printf '%p %s\n' | sort > /root/backupd-acceptance/backup-root.after
diff /root/backupd-acceptance/backup-root.before /root/backupd-acceptance/backup-root.after
```

- [ ] `sha256sum -c` reports the canary `OK`
- [ ] The `diff` against the recorded listing is empty, so the backup root is
      untouched, byte for byte, and every artifact is still readable
- [ ] `$DISK/appdata/backupd` is untouched
- [ ] Nothing elsewhere under `$DISK/backups` changed
- [ ] Nothing outside the declared host paths was touched, and no OMV
      configuration was modified
- [ ] Re-adding the same compose file with the same paths adopts the existing
      catalog rather than starting empty

Also run the destructive half on a scratch install: `docker compose down -v` and
confirm no named volume ever held retained backup data (every persistent path in
this profile is a bind mount to a host path you chose, precisely so that `-v`
cannot reach it).

- [ ] `down -v` removes nothing under `$DISK/backups/backupd`

---

## Step 9 — Local workflow hooks, and the Docker prerequisite

A workflow step whose target is `local` does not run in the engine container, and since
issue #865 it does not run on a host shell either: it runs in an **ephemeral Docker
container** launched by the **Host Workflow Runner**, a small version-pinned process
systemd supervises as `retnd-workflow-runner.service`
(`docs/adr/0020-host-workflow-runner.md`, `docs/runtime-contract.md`).

OpenMediaVault is Debian with systemd, and this deployment is an ordinary compose
project on that Debian host, so local hooks are **available**: the runner installs onto
the host beside the stack with `scripts/install/install_docker_host.py`, not through
the OMV web interface and not through `omv-compose`. OMV's own configuration database
is never written, which is the same rule step 8's removal check already applies.

Three prerequisites, all three re-proved by the runner's own startup probe, and any one
of them missing is a refusal rather than a hook that quietly does not run:

- **a daemon the runner's account can reach.** That is one supplementary group:
  `sudo usermod -aG docker <the runner's account>`, or whatever group owns the socket
  here — the installer reads the group off the socket rather than assuming `docker`. The
  unit gets `SupplementaryGroups=` and that socket in `ReadWritePaths`; nothing else
  does. The membership is root-equivalent on this host, which is exactly why it belongs
  to the runner and to nothing else: **the engine container gains nothing** — no socket,
  no `group_add`, no `DOCKER_HOST`, and `distribution/packaging`'s preflight fails the
  build if any shipped package asks for one;
- **the hook image, already on the host.** `--hook-image` defaults to the pinned
  `bash:5.2.37-alpine3.21`, `install` fetches it, and `WORKFLOW_RUNNER_HOOK_IMAGE` in the
  deployment's `.env` names a different one. The runner never pulls: an image that is not
  there is a refusal, not a download;
- **a non-root account.** The runner refuses to run as root, so a root deployment gets no
  runner and is told so.

`python3 scripts/install/install_docker_host.py preflight` refuses with exit 12, and the
refusal carries the `usermod -aG` line, when this deployment has hook scripts and the
runner's account cannot reach the daemon. A deployment with an empty workflows
directory is held to none of it, and `WORKFLOW_RUNNER=off` in the `.env` says so
explicitly.

And a fourth prerequisite, on THIS side of the socket, which was missing until
issue #921: the engine has to be able to see the runner. The compose file mounts
three paths for it, all three under the same `$DISK` prefix as everything else,
and an operator who installs the runner somewhere else gets a refusal at the
first hook rather than a hook that runs:

| Host path | In the container | Why |
|---|---|---|
| `$DISK/appdata/backupd/workflows` | `/workflows` (read-only) | the hook scripts the engine reads |
| `$DISK/appdata/backupd/run` | `/data/run` | where the runner's socket appears |
| `$DISK/appdata/backupd/secrets/workflow-runner.token` | `/etc/retnd/workflow-runner.token` (read-only) | the credential the engine presents |

So install the runner with `--workflows-dir $DISK/appdata/backupd/workflows`,
`--runtime-dir $DISK/appdata/backupd/run` and its token under
`$DISK/appdata/backupd/secrets`. None of the three reaches the Docker daemon: the
runner holds the socket's group on the host, and the engine container gains
nothing.

- [ ] `systemctl is-active retnd-workflow-runner.service` reports `active`, and the
      account it runs as is recorded in the evidence table
- [ ] That account is in the Docker socket's group (`id <account>`), and the engine
      container is **not**: `docker inspect` shows no socket mount, no `group_add` and no
      `DOCKER_HOST` on either shipped service
- [ ] The hook image is present on the host (`docker image inspect <the reference>`), and
      the reference the unit was installed with is recorded
- [ ] A workflow with one `local` hook runs, and its container is gone afterwards
      (`docker ps -a --filter label=backupd.workflow-hook=1` is empty)


## Evidence (§68)

Fill this in in the same commit that flips OpenMediaVault from uncertified to
certified.

| Field | Value |
| --- | --- |
| Provider / OS version | |
| Hardware or VM, model | |
| Architecture | |
| Package / image version | |
| Image reference used, and how it was made resolvable | |
| Real `dev-disk-by-uuid-<UUID>` path used | |
| Install result | |
| Web UI access result | |
| Auth result | |
| Storage result | |
| Update result | |
| Uninstall / removal result | |
| Retained-backup safety | |
| Confirmation that no native plugin was installed | |
| Evidence (logs, screenshots, transcripts, with secrets redacted) | |
| Executed by | |
| Date | |

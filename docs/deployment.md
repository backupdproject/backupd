# UGREEN container deployment

This documents the container packaging for `core/cmd/retnd` (A3.9): what's in
`container/`, why it's shaped the way it is, and how I verified each requirement rather
than just asserting it. It's meant to be read next to `container/Dockerfile` and
`container/compose.yaml`, which carry the same reasoning inline as comments.

## The command is `retnd`, and `/backupd-web` survives for exactly one release

EPIC R (#885) renamed the product. The engine CLI is `retnd`, the web host is
`retnd-web`, and inside the image those are the two real binaries at `/retnd` and
`/retnd-web`. Everything in this file, in `container/compose.yaml` and in every adapter
names them. This is the third name the command has had — `backup-manager` became `rbm`
in 0.3.3, and `rbm` became `backupd` in 0.4.0 — and it is the first rename that ships a
compatibility window, because by now the name is not only a printed word: it is in the
image's entrypoints, the compose service and the container's own paths.

**One name is aliased, deliberately.** `container/Dockerfile` creates `/backupd-web` in
the runtime stage as a **real hardlink** to `/retnd-web`: one inode, two names, no second
copy of a ~40 MB binary and no shell wrapper, because the distroless runtime has no shell
to resolve a name through. So a compose file, `docker run` line or unit of yours that
still spells `/backupd-web` starts on a new image instead of dying with
`exec /backupd-web: no such file or directory`. It is a shim with a closing date: #895
deletes the hardlink, so the window is one release.

**Everything else about the names is a cut.** There is no `/backupd` beside `/retnd`. A
caller that execs the engine binary by its old path stops working the moment the tag
moves — loudly, at container start — and that asymmetry is the decision rather than an
oversight: `/backupd-web` is the path operators pinned in their own compose files, and
`/backupd` is the path this project's own files pinned. The image's own `HEALTHCHECK` is
`["/retnd", "status"]`, and `scripts/install/install_docker_host.py` refuses a `--release`
older than the images that contain `/retnd-web`, for the mirror-image reason: the compose
definition it writes runs `/retnd-web`, which no image published before R1.5 (#890)
contains.

One thing to know if you script against the image rather than run it: `/retnd` and
`/retnd-web` are real files rather than links, so `docker cp` needs no `-L`, and anything
pulling them OUT of the image names `/retnd` and `/retnd-web`.
`scripts/release/record-release-hashes.sh` and
`scripts/release/verify-manifest-parity.sh` both do.

**What moved with the name, and what an upgraded deployment sees.** The compose service
is `retnd` (the UI service is still `web-ui`, because it never named the product), the
compose project is named `retnd`, so the default container names are `retnd-retnd-1` and
`retnd-web-ui-1`, and the container configuration directory is `/etc/retnd`. A script of
yours that named `backupd-backupd-1` in a `docker inspect` or `docker logs` line needs
updating; nothing else about those containers changed. The two data mounts,
`/data/state` and `/data/backups`, carry no brand and did not move, and **nothing renames
a directory on your NAS**: everything on the left of a `:` in `container/compose.yaml` is
a host path you chose. A deployment still mounted at the pre-rename container paths is
**adopted** rather than handed a first-run wizard, and warns on every start until it is
moved — [Upgrading a deployment installed before the
rename](install.md#upgrading-a-deployment-installed-before-the-rename) is the whole
procedure. In short, and in the three shapes an upgrade actually takes:

- **Through the installer**, `install_docker_host.py migrate-identity` stops the stack,
  restages it so the mounts become `/etc/retnd/...`, rewrites the absolute container
  paths in the persisted `config.yaml`, renames the three systemd units and brings the
  stack back up, restoring `config.yaml` byte for byte if anything after the rewrite
  fails. It is idempotent, and a deployment that has already moved is told so.
- **With an unedited compose file you pinned yourself**, nothing is required of you for
  one release: the old image reference resolves through the mirror, the image carries
  `/backupd-web`, and the engine adopts the state and configuration it finds at the
  pre-rename paths, warning on every start with the compose line to change and the
  command that changes it. `config.yaml` is byte-identical either way — the pre-rename
  paths are compiled-in constants and not a new configuration key — so a rollback to the
  previous build is supported for that same release.
- **The one refusal** is two *different* populated directories, one at each container
  path: the engine refuses to start and names both, because choosing a journal silently
  is the worst option available. Keep the one this deployment should serve, move the
  other aside, start again. One host directory mounted at both paths — what
  `migrate-identity` writes for the rollback window — is one device and one inode, and
  starts normally.

Two things are deliberately NOT renamed. The image reference is still
`ghcr.io/backupdproject/backupd` until the repository coordinates move (#895), because
moving it inside the old organisation first would have cost every operator two compose
edits for one rename. And `container/release-manifest.json`'s already-published entries
keep their `backupd` and `backupd-web` digest keys: they record artifacts that really
were published under those names, so rewriting them would falsify the release record.
Its consumer accepts both spellings for the release range that spans the rename.

## The authoritative runtime contract lives next door

Since issue #167, `container/compose.yaml` is not just the shape a generic Docker
deployment happens to take: it is the **authoritative runtime definition** every other
deployment artifact derives from, held to a machine-checkable contract by
`distribution/compose`. Read [`docs/runtime-contract.md`](runtime-contract.md) for the
standardised field set, the prohibition list and how each is proven, the runtime-profile
selector (`--profile=generic|ugos`) and what a profile may and may not change, the
trusted-gateway boundary, runtime-selected UI bundles (`--ui-dir` / `--ui-root`), the
digest policy, and the measured cost of the engine-plus-web-ui hop.

This file stays what it always was: the reasoning behind the container image and how each
requirement was verified rather than asserted. The two are meant to be read together.

## Status

`core/cmd/retnd` implements every execution mode this deployment shape was
originally packaged ahead of: `run`, `daemon`, `check`, `status`, `sources`, `artifacts`,
`fetch`, `retention`, `reconcile`, `validate` and `version`. `container/compose.yaml`
defaults to the real long-running process (`/retnd-web serve`, see "The generic
Web host" below) and `container/Dockerfile`'s `HEALTHCHECK` tracks `retnd
status`'s real exit code (HEALTHY vs DEGRADED/STALE/FAILING), not just process liveness
(issue #82/B4.1). Headless-only deployment (no web listener at all) is still available
by overriding `command` to `["/retnd", "daemon"]`.

## rclone is compiled in, not shelled out to

The image contains no `rclone` binary anywhere, and I checked that directly against the
built image rather than trusting the design:

```
$ docker create --platform linux/arm64 retnd:0.0.0-a3.9 version
$ docker export <container-id> | tar -tv | grep -i rclone
$ echo $?
1
```

Exit 1 means zero matches, checked case-insensitively against the full file listing of
the exported image filesystem (1447 entries: the distroless base's certs/tzdata/passwd
plus exactly one executable, the engine binary the builder stage produces; it is
`/retnd` today and has been renamed with the product every time). There's no file named `rclone`, no
`rclone` directory, nothing.

The flip side, that rclone's packages are genuinely compiled into that one binary rather
than the manager silently doing nothing useful, is also checked directly:

```
$ strings retnd | grep -c 'rclone/rclone'
2770
$ strings retnd | grep 'rclone/rclone' | sort -u | head
 github.com/rclone/rclone/fs/hash
 github.com/rclone/rclone/fs/list
 github.com/rclone/rclone/fs/walk
,github.com/rclone/rclone/fs/config/configmap
!github.com/rclone/rclone/fs/march
...
```

2770 occurrences of `rclone/rclone` import paths inside a `stripped`, `statically
linked` ELF binary. rclone is a Go module dependency (`core/go.mod` pins
`github.com/rclone/rclone v1.75.0`), imported as packages by `core/internal/transport/rclone`,
and compiled straight into `/retnd` by the builder stage. `CGO_ENABLED=0`
throughout means this holds without a C toolchain on either target architecture, which is
also why `modernc.org/sqlite` (the state package's SQLite driver, pure Go, no cgo) was
the only option that ever made sense here.

## Reproducible build

- **Base images pinned by digest**, not tag, in both stages of `container/Dockerfile`:
  the `golang:1.27-bookworm` builder and the `gcr.io/distroless/static-debian12:nonroot`
  runtime. A tag can move; a digest can't. The builder's Go version
  (`golang@sha256:ded31c68...`) matches this module's `go 1.27.0` directive exactly, so
  there's no drift between what `core/go.mod` asks for and what compiles it.
- **`GOTOOLCHAIN=local`** so `go build` never reaches out to fetch a different toolchain
  mid-build if some future `core/go.mod` bump disagreed with the pinned builder image.
- **`-trimpath`** strips the builder's absolute source paths from the binary. Checked
  directly: `strings retnd | grep -E '/Users/rom|/src/'` returns nothing.
- **`-buildvcs=false`** so the build doesn't stamp VCS state read off a `.git` directory
  that may or may not even be in the build context (`.dockerignore` excludes `.git`
  deliberately, for this exact reason).
- **Deterministic build stamps**: `VERSION` and `COMMIT` are Docker build `ARG`s supplied
  from outside (`container/compose.yaml`'s `build.args`, or `--build-arg` on a direct
  `docker buildx build`), never discovered by the Dockerfile shelling out to `git`
  itself. Same two inputs always produce the same `-ldflags -X main.version=... -X
  main.commit=...`. Compute them from a real checkout:

  ```
  VERSION=$(git describe --tags --always)
  COMMIT=$(git rev-parse HEAD)
  ```

  Left unset, they default to `dev`/`none`, which is fine for a local smoke build and not
  what a real release should ship.
- Module and build caches are BuildKit cache mounts (`--mount=type=cache`), which don't
  persist into the image or affect the compiled output's bytes, only how long a rebuild
  takes.

## Minimal runtime image

Multi-stage build: the `golang` builder is discarded, and the final image is
`gcr.io/distroless/static-debian12:nonroot` plus the one binary. No shell, no package
manager, no libc (none needed, see `CGO_ENABLED=0` above).

Built and measured directly, both architectures:

| Architecture  | Built | Ran | Image size |
|---------------|:-----:|:---:|-----------:|
| linux/arm64   | yes   | yes, natively (this is an Apple Silicon host) | 17.3 MB |
| linux/amd64   | yes   | yes, under QEMU emulation (no native amd64 host available here) | 18.5 MB |

Both were built with `docker buildx build --platform linux/<arch> ...` from
`container/Dockerfile`, and both ran `retnd version` successfully and printed
the expected version/commit/Go-version line. `docker compose build` (which does not
cross-build; see below) plus `docker compose run --rm retnd` was also exercised
end to end on linux/amd64, with the full read-only-rootfs/tmpfs/non-root/bind-mount shape
from `container/compose.yaml` in effect, not just a bare `docker run`.

To publish a genuinely multi-arch tag (a single reference that resolves to either
architecture), build both platforms in one invocation and push:

```
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg VERSION=$(git describe --tags --always) \
  --build-arg COMMIT=$(git rev-parse HEAD) \
  -f container/Dockerfile \
  -t <registry>/retnd:<version> \
  --push \
  .
```

`container/compose.yaml`'s `build:` section, by contrast, builds for whatever machine
runs `docker compose build` (i.e. it's meant to be run **on** the UGREEN NAS itself, or a
dev machine matching its architecture) — compose isn't the tool for cross-building a
multi-arch manifest, buildx directly is.

## Read-only application filesystem, and where SQLite actually needs to write

`container/compose.yaml` sets `read_only: true`. Two things have to still be writable
under that, and I checked both directly against a container built from this exact image
rather than assuming:

**The state directory, not just the database file.** `core/internal/state/state.go` opens
SQLite with `journal_mode=WAL`, which keeps a `-wal` and a `-shm` file alongside the main
`.db` file while it's open. A single-file bind mount for just the database would leave
those siblings unable to be created. `container/compose.yaml` mounts a whole directory at
`/data/state` for this reason. Verified: a small Go program using the exact same
`sql.Open` + pragma sequence as `core/internal/state/state.go`, run inside this image with
`--read-only` and only `/data/state` bind-mounted writable, inserted 20,000 rows, built an
index, and read them back successfully.

**Somewhere for temp files.** A genuinely read-only rootfs makes Go's default temp
directory (`/tmp`, when `TMPDIR` is unset) unwritable — I checked this directly with a
throwaway probe binary: `os.CreateTemp("", ...)` fails with "read-only file system" under
`--read-only` with no `/tmp` mount, and succeeds once a `tmpfs` is mounted there.
Interestingly, forcing `PRAGMA temp_store = FILE` and running `VACUUM` (a query that all
but guarantees a real temp file in the C sqlite implementation) through
`modernc.org/sqlite` v1.57.0 *didn't* fail even without a writable `/tmp` in my testing —
its temp-file path apparently doesn't always need real disk backing for a moderate
workload. That's a "didn't reproduce a failure," not a guarantee it never needs one on a
larger journal, a different query shape, or a future driver version, and provisioning a
64 MB `tmpfs` at `/tmp` costs nothing, so `container/compose.yaml` mounts one anyway and
sets `TMPDIR=/tmp` explicitly rather than relying on the fallback search order
(`SQLITE_TMPDIR`, then `TMPDIR`, then a hardcoded list ending in `/tmp`) to land somewhere
writable by luck.

Everything else under `/` is the image's own read-only content (the binary, the TLS root
store, timezone data) and was never meant to be written to.

## Non-root and the NAS uid/gid

`gcr.io/distroless/static-debian12:nonroot` already defaults to a non-root account
(uid/gid 65532, "nonroot" in its `/etc/passwd`), which would be enough in isolation. It
isn't enough here, because this container also has to write into a directory that lives
on the UGREEN NAS's actual filesystem (the state volume, and the backup storage volume),
and that directory's ownership is whatever the NAS's admin account happens to be, not
whatever uid the base image picked at build time.

**Assumption, stated explicitly**: `container/.env.example` defaults `PUID`/`PGID` to
`1000`, the common first-admin-account uid/gid on Linux-based NAS distributions (UGOS
included). This is a default to override, not a hardcoded value: `container/compose.yaml`
reads `user: "${PUID:-1000}:${PGID:-1000}"` from the environment, so a UGREEN host where
the admin account isn't 1000 just needs a different `.env`, not a different image or a
different compose file.

This image has no shell and no root-then-drop-privileges init step (that would need
`privileged`-adjacent capabilities this container deliberately doesn't have), so it
cannot `chown` the mounted directories for you at startup. **Whatever `PUID`/`PGID` you
set has to already own `STATE_DIR` and `BACKUP_DIR` on the host before the first start**,
e.g. `chown -R 1000:1000 /volume1/retnd/state /volume1/backups` on the NAS
itself, matching whichever PUID/PGID you put in `.env`.

One honest limitation: I built and ran all of this on macOS with Docker Desktop, whose
bind-mount layer does not enforce Linux file ownership/permission bits the way a native
Linux host (like the UGREEN NAS's own OS) does — a uid/gid mismatch that would fail with
"permission denied" on real Linux went through unchallenged in this sandbox. The
`PUID`/`PGID` + pre-chown guidance above is standard Linux permission semantics, not
something I was able to independently reproduce a failure for in this environment.

The image also doesn't rely on `$HOME`: `core/internal/transport/rclone/ssh.go` and
`core/internal/config` pass `known_hosts`/`key_file` paths through rclone's
`env.ShellExpand`, which only expands `~` for a path that literally starts with `~` and
otherwise leaves it alone. Keep config paths absolute (as `container/.env.example`
does) and this doesn't come up, which matters because `/home/nonroot` in the base image
is owned by uid 65532 specifically, not by whatever `PUID` you set.

## Mounted backup storage, and read-only credentials/configuration

`container/compose.yaml` bind-mounts five things:

- `/data/state` (writable): the SQLite journal directory, see above.
- `/data/backups` (writable): the NAS backup volume/share completed artifacts land on.
- `/etc/retnd/config` (writable): the DIRECTORY holding the manager's YAML
  config (FR-5), and the two stores the engine creates beside it, `ssh_keys/` and
  `known_hosts.d/`.
- `/etc/retnd/id_ed25519` (`:ro`): the SFTP client private key.
- `/etc/retnd/known_hosts` (`:ro`): the pinned host keys (FR-6).

The configuration mount is a writable directory rather than a read-only single file,
and that is issue #196 rather than a preference. Adding a backup set, saving settings
and first-run setup all replace `config.yaml` through a temp file created in its own
directory; on a single-file mount that directory is the image's read-only rootfs, so
all three failed at the write. A directory can also be empty, which is the only honest
way for a fresh install to say "not configured yet": a bind mount cannot express that
about a file, because Docker creates a directory at a source path that does not exist.

All five host paths come from environment variables (`STATE_DIR`, `BACKUP_DIR`,
`CONFIG_DIR`, `SSH_KEY_FILE`, `KNOWN_HOSTS_FILE`), documented with no real values in
`container/.env.example`. None of them have a fallback default in `compose.yaml` itself:
they're declared with `${VAR:?...}`, so `docker compose up` fails immediately with a
clear message if one is missing, rather than silently bind-mounting some made-up default
path. Nothing resembling a credential is ever written into `container/compose.yaml`,
`container/Dockerfile`, or `container/.env.example` itself; the actual key material only
ever exists at the host path the operator points `SSH_KEY_FILE` to.

## The incremental engine's gate, and where to set it in a container deployment

The incremental (kopia) backup engine is off by default and gated on one
setting, which can be given two ways. The environment wins over the file **in
both directions**: `RETND_INCREMENTAL_ENGINE` set to `1`, `true`, `yes` or
`on` enables the engine whatever the file says, `0`, `false`, `no` or `off`
disables it whatever the file says, and unset or empty defers to the file. A
value that is neither spelling is refused when the configuration is validated,
naming the variable and the accepted spellings, so a typo does not resolve to
"off".

**On the standard container deployment, set it in `config.yaml`:**

```yaml
# on the /etc/retnd/config mount
incremental_engine:
  enabled: true
```

That is the place it belongs, and on this deployment shape it is the only
place that works for the long-running processes. `container/compose.yaml`
passes environment through an explicit `environment:` block — `TMPDIR`,
`LOG_LEVEL`, `TZ` and the rest, each interpolated by name — and there is no
catch-all, so a variable dropped into `container/.env` reaches nothing unless
that file declares it. `RETND_INCREMENTAL_ENGINE` is not among them.

Where the environment override is the right tool is a process you launch
yourself, and it is worth knowing for exactly two situations — proving
something without editing a deployment's configuration, and turning the engine
off for one command:

```bash
# one command, inside the running engine container
docker compose -p retnd ... exec -e RETND_INCREMENTAL_ENGINE=1 retnd /retnd repository health

# a CLI-only install, or a systemd unit, where you own the environment
RETND_INCREMENTAL_ENGINE=0 ~/retnd/bin/retnd run
```

The variable is read by the **engine** process, the one that runs the cycle,
and not by the web host, so it only ever has to reach wherever `retnd`
itself runs.

There is no watcher and no SIGHUP reload, so a hand edit to `config.yaml` is
read when a process starts; restart the engine container after making one.
Turning the gate off never stops the daemon: a configuration that declares
incremental backup sets still loads and still backs up every `artifact` set on
schedule, and only the incremental ones refuse. See
[`docs/incremental-engine.md`](incremental-engine.md) for what the gate does
and does not cover, and
[`docs/incremental-runbooks.md`](incremental-runbooks.md#runbook-4-the-engine-is-refusing-because-the-gate-is-off)
for the refusal.

One storage note for a deployment that enables it: a repository's bytes live
under the `/data/backups` mount, in the reserved namespace
`<backup_root>/.backupd/repositories/<domain>/`. That path must be on a
filesystem the container can write to and that is not walked by anything else
&mdash; exclude `.backupd` from any SMB/AFP share, scanner or backup-of-the-backup
that covers the backup volume.

## No privileged mode

`container/compose.yaml` never sets `privileged: true` (explicitly `privileged: false`),
drops every capability (`cap_drop: [ALL]`), and sets `no-new-privileges:true`. Nothing
this process does (open files, make outbound SSH/SFTP connections, run as a fixed
non-root uid) needs any capability at all.

## Restart policy

`restart: unless-stopped`: come back after a crash or a NAS reboot, stay down if an
operator deliberately stops it. `command: ["/retnd-web", "serve"]` is a real
long-running process (the generic Web host's HTTP server plus the backup scheduler, see
below), so this policy now does what it says rather than looping a container that exits
immediately. For a one-shot check instead, use `docker compose run --rm retnd
/retnd version` (or `... check`), which bypasses `restart` entirely.

## Health check

`retnd status` (issue #26, FR-24) reports `HEALTHY`/`DEGRADED`/`STALE`/`FAILING`
per backup set and exits 0 only when every one of them is `HEALTHY`. `container/Dockerfile`'s
`HEALTHCHECK` runs exactly that:

```
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/retnd", "status"]
```

Verified directly (`apps/generic/tests/dockercli`), not just asserted: a container whose one
backup set is `DEGRADED` (no artifact ever discovered for it) reports Docker health
`unhealthy`, not `healthy`. Before this issue, `HEALTHCHECK` ran `retnd version`,
which exits 0 unconditionally and so reported `healthy` regardless of backup health — real
(if minimal) process-liveness evidence, but not what FR-24's health states are for.

Since issue #444 that verdict also covers where the backups are, not only how fresh they
are. A deployment that declares a storage medium (EPIC E, FR-27) and whose relocations to
it keep failing now reports `DEGRADED`, with the age of the oldest failing move and the
reason the engine recorded on it, and therefore reports Docker health `unhealthy`. The
backups themselves are untouched in that state, which is exactly why it needed saying out
loud: the copy the operator asked to be offsite is not offsite, the move engine has been
retrying it every cycle, and every surface that carried that fact before was describing a
single pass and was gone by the time anybody looked. A deployment that declares no
storage medium reports nothing new at all.

`container/compose.yaml` deliberately overrides that for the engine service, and asks
`/health/live` instead. The reason is `web-ui`'s `depends_on: retnd: condition:
service_healthy`: whatever the engine's healthcheck asks is what stands between an operator
and the only LAN-facing listener, and `retnd status` exits non-zero on a `DEGRADED`
or `STALE` set and on an instance with no configuration at all. Gating startup on it means a
stale backup set, or a fresh install, keeps the UI from ever coming up, which is the worst
moment to lose the page you would fix it from. Backup freshness stays what it was built to
be: the image's own `HEALTHCHECK` (so a plain `docker run` still reports it, and so does the
headless `daemon` command, which serves no HTTP and has no liveness endpoint to ask), the
alerts block, and `docker compose exec retnd /retnd status`.

Every packaged adapter declares the same start gate, and has to (issue #206). The image's
instruction and the canonical start gate are now deliberately different commands, so an
adapter that declares nothing for the engine inherits the freshness verdict rather than the
gate: `distribution/packaging`'s derivation gate allows that only where nothing waits on the
engine's health, which is the Unraid template and only that. `apps/generic/tests/dockercli`
brings every derived runtime definition up on a real fresh install and requires the Web UI to
serve, with `retnd status` non-zero inside the same stack as the control that makes
the result mean something.

## Building and running it yourself

```
# One architecture, loaded into the local Docker for testing:
docker buildx build --platform linux/arm64 \
  --build-arg VERSION=$(git describe --tags --always) \
  --build-arg COMMIT=$(git rev-parse HEAD) \
  -f container/Dockerfile -t retnd:dev --load .

docker run --rm --platform linux/arm64 retnd:dev /retnd version

# The full deployment shape, via compose (starts the generic Web host —
# see below — listening on LISTEN_PORT, default 8080):
cp container/.env.example container/.env   # then edit the paths for real
docker compose -f container/compose.yaml build
docker compose -f container/compose.yaml up -d

# A one-shot check instead of the long-running Web host:
docker compose -f container/compose.yaml run --rm retnd /retnd check
```

See "The generic Web host" below for what `serve` actually composes, and
`scripts/deploy/deploy_generic.py --help` for a scripted version of the same steps that
also renders `config.yaml`/`.env` for you from a private key and a remote host.

## The generic Web host: two containers, one image

The "generic Web App host" (issue #82/B4.1, docs/EPIC-B-multi-nas.md §9.2) is two
separate Docker containers, both running the exact same `/retnd-web` binary
from the exact same image - only `command:` differs, the same "one canonical image,
vary command" principle already applied to `/retnd` vs. `/retnd-web`
themselves. No nginx or other new runtime dependency was introduced for the split: the
UI-host container's reverse proxy is a plain `net/http/httputil.ReverseProxy`
(`apps/common/webhost/serve.NewUI`).

```text
                          published port (LISTEN_PORT)
                                    │
                                    ▼
                          ┌───────────────────┐
        LAN / operator ──▶│      web-ui        │
                          │ static UI + proxy  │
                          └─────────┬──────────┘
                                    │ internal Docker network only
                                    │ (http://retnd:8080)
                                    ▼
                          ┌───────────────────┐
                          │  retnd    │   no published port -
                          │ engine: core svc + │   reachable only from
                          │ scheduler + local  │   web-ui, over the
                          │ auth + /api/v1     │   `internal` network
                          └───────────────────┘
```

**`retnd`** (`/retnd-web serve`) is the engine: local authentication
(`apps/common/auth/local`), the versioned `/api/v1` API (`apps/common/webhost`), and the
backup scheduler (`core/service.BackupService.RunOnSchedule`, at the config file's own
`poll_interval`) - one process sharing one `*service.BackupService` and one
`signal.NotifyContext`-derived shutdown context (§9.3): both the HTTP server and the
scheduler stop because the same signal canceled that context, not because one tells the
other to, and both share `BackupService`'s existing single-flight guard so a scheduled
cycle and a future API-submitted one (`POST /api/v1/operations`) can never run
concurrently against the same backup sets. It has **no static UI and no published
port** - `container/compose.yaml` gives it no `ports:` entry at all, so it is reachable
only from `web-ui`, over the `internal` bridge network compose.yaml defines for exactly
this project (nothing external, nothing shared with any other container on the host).

**`web-ui`** (`/retnd-web serve-ui`) serves the shared static UI (`ui/shared`'s
built bundle, embedded via `apps/generic/webui`'s `go:embed`, with an SPA fallback to
`index.html` for any client-side route) and reverse-proxies `/api/v1/*` and `/health/*`
unchanged (same path, method, body, and - critically - the browser's session/CSRF
cookies) to `retnd` over that same `internal` network, by its compose service
name. This is the **only** container with a `ports:` entry - the one thing a browser or
an operator's terminal is meant to reach directly.

What this topology actually buys: even a full compromise of the UI-host process (the
one facing the LAN) reaches `retnd`'s API the exact same way a legitimate
browser would - it does not get a bind mount to `config.yaml`, the SSH key,
`known_hosts`, or either data directory, because `web-ui` never has any of those
mounted in the first place (see `container/compose.yaml`: it declares zero `volumes:`).
This is plain Docker Compose network topology, nothing more - no `internal: true`
network flag and no firewall rules block `web-ui`'s own outbound internet access, which
would be a further hardening step beyond what this issue asked for.

**First run.** With no administrator account yet, `retnd` prints a one-time
enrollment link straight to its own container log:

```
retnd-web: no administrator account exists yet. Open http://localhost:8080/enroll?token=... to create one (valid 30 minutes, single use).
```

`retnd` has no published port of its own (see above), so its own `--listen`
address is never something an operator could actually open - printing a link against
that address was a real bug fixed as part of issue #119's review: `--public-base-url`/
`$PUBLIC_BASE_URL` tells `serve` what `web-ui`'s own externally-reachable address
actually is, and `container/compose.yaml` sets it by default to
`http://localhost:${LISTEN_PORT}`, which tracks whatever host port you actually
published `web-ui` on. `localhost` only resolves correctly when you open the link on
the NAS itself; set `PUBLIC_BASE_URL` in `.env` to the NAS's real hostname/IP (see
`container/.env.example`) to get a link that also works from another machine on the
LAN. `scripts/install/install_docker_host.py` already does that for you: with no
`--public-base-url` it reads the address this machine's default route leaves by and
writes that into the `.env` it generates, because the link is opened from a different
machine on the same LAN and an address is the only form that works there with nothing
configured. A hostname is not: it resolves on the box it names and, without mDNS or a
DNS record somebody set up, nowhere else. On a host with no default route to read an
address off, it falls back to the hostname. `compose.yaml` itself cannot do any of
this, which is why its own default is still `localhost`: it has no way to ask the
kernel anything. Leaving `PUBLIC_BASE_URL` unset entirely (outside of `compose.yaml`'s own default,
e.g. when running `/retnd-web serve` directly) prints just the raw token
instead of a clickable but wrong link.

The token itself is required to complete `POST /api/v1/auth/enroll` — reaching the port
is not enough to claim the account (§49.1) — and is invalidated the moment enrollment
completes, or by the next process restart before it does. It travels as a URL query
parameter, not a form field: neither `EnrollmentPage.tsx` nor the design canvas
(`docs/design/Backup Manager.dc.html`) has one, so `ui/shared/src/api/client.ts` reads
it off `window.location.search` and attaches it as the `X-Bootstrap-Token` header
instead.

A *failed* enrollment does not spend it. The handler verifies the token, sends the
confirmation mail described below, and only then consumes it and writes the record, so a
rejected password, a malformed recovery address or an SMTP server that would not accept
the message all leave the same link usable: correct the field and submit again. That is
worth stating because it did not used to be true — a too-short password once burned the
token and left the operator restarting the engine for a fresh one.

**What enrollment asks for, and why it needs an SMTP server.** `POST
/api/v1/auth/enroll` takes a `username` and a `password`, and beside them a
`recoveryEmail` and the `smtp` block used to reach it: `host`, `port`, `security`
(`starttls`, `tls` or `none`), `username`, `password` and `from`. The username is still
the login identity; the recovery address is an additional field on the same
administrator record. Before that record is written, the handler sends a confirmation
message to the address over exactly those details, and a send that fails fails the
enrollment: `SMTP_SEND_FAILED`, no account, enrollment still open. A malformed address
is refused as `INVALID_EMAIL` before any connection is attempted. This is deliberate
and it is the whole point of asking at enrollment rather than later — a recovery address
nobody has ever delivered to is worth nothing on the day it is needed, and the day it is
needed is the day nobody can sign in to fix it.

`PUBLIC_BASE_URL` is therefore load-bearing twice over. The reset mail's link is built
from the same value as the enrollment notice above, so a deployment left on
`compose.yaml`'s default of `http://localhost:${LISTEN_PORT}` mails a link that resolves
only on the NAS itself — which, unlike the enrollment notice, nobody is watching a log
to notice.

**Losing the password.** `POST /api/v1/auth/forgot-password` takes `{username}` and
answers `204` every time, whether or not that name is the administrator's: an endpoint
that answered differently would tell an unauthenticated caller the account's name. Where
it does match and a confirmed recovery address exists, a single-use reset link valid for
30 minutes is mailed to that address, and `POST /api/v1/auth/reset-password`
`{token, newPassword}` spends it. The reset token lives in the process, like the
bootstrap one, so a restart invalidates whatever links are outstanding. Completing a
reset sets the new hash and revokes every live session, including the browser that asked:
the answer to a successful reset is the sign-in page, not a session.

**The SMTP password is a reference, not a value.** `state/local-auth.json` gains
`recovery_email`, `recovery_email_confirmed_at` and an `smtp` object beside the existing
`password_hash`, and that object's password is held in the project's secret-reference form
(`core/internal/secretref`) rather than as the secret itself. Nothing reads it back out
over the API: `GET /api/v1/auth/recovery` returns the SMTP block with no password field
at all and a `passwordSet` boolean in its place, `PATCH` leaves the stored one alone when
the field is omitted, and `POST /api/v1/auth/recovery/test` proves the credential by
using it rather than by showing it. Same rule as the SSH key: the operator supplies it,
the deployment holds it, and no response or log line ever carries it back.

**Provisioning without a browser (`/retnd-web auth create-admin`).** The other way
an administrator comes into existence is the subcommand an automated deployment runs
instead of opening a link: `--username`, `--password-stdin`, `--auth-store`. Recovery is
**optional** there — `--recovery-email`, `--smtp-host`, `--smtp-port`, `--smtp-security`,
`--smtp-username`, `--smtp-password-stdin`, `--smtp-from` — and the two stdin secrets are
read as two lines in that order: the account password first, the SMTP password second.
Given an SMTP endpoint it sends the same confirmation message and fails the command if
that send fails, exactly as the HTTP route does. Given none, it provisions the account
and leaves recovery unconfigured, which is the deliberate difference: a provisioning run
in a pipeline often has no mail credential to give, and refusing to create the account
would make that pipeline unusable to no security benefit. The operator finishes it in
Settings, and until they do, a forgotten password has no self-service route back. A
deployment provisioned this way should treat `GET /api/v1/auth/recovery` returning no
address as an open task rather than as a state to leave alone.

**Trusting `web-ui`'s reverse proxy (`TRUST_FORWARDED_HEADERS`).** `retnd`
only ever sees requests from `web-ui`'s own reverse proxy, over the `internal` network -
every request's `RemoteAddr` is `web-ui`'s own container address, never the real
external client's. Left uncorrected, that collapses per-IP rate limiting on
`/api/v1/auth/login`, `/api/v1/auth/enroll` and `/api/v1/auth/forgot-password` into one
shared bucket for every client on the internet-facing side (an attacker-usable
denial-of-service against the admin's own login), and permanently prevents the
session/CSRF cookies' `Secure` flag from ever
being `true`, regardless of TLS in front of `web-ui`'s published port (issue #119's
review, findings 1 and 4). `container/compose.yaml` sets
`TRUST_FORWARDED_HEADERS=true` for `retnd` only, which makes it trust
`X-Forwarded-For`/`X-Forwarded-Proto` from its one caller instead of its own
`RemoteAddr`/TLS state - safe specifically because network isolation guarantees
`web-ui` is the only thing that can ever be `retnd`'s direct TCP peer, and
`apps/common/webhost/serve.NewUI`'s reverse proxy always sets both headers itself, derived
from its own real connection to the browser, never copied from anything the browser
sent. This is never set for `web-ui` itself: that container IS the actual
internet-facing edge and must never trust a forwarded header from just anyone hitting
its published port.

**Two binaries, one image, no `ENTRYPOINT`.** `apps/generic` is its own Go module — it
has to be, since it imports `apps/common/webhost/serve` and `apps/common/auth/local`,
and `core/`'s own module cannot depend on `apps/` in either direction (§7.1) — so
`/retnd-web` is a second binary alongside the unchanged `/retnd`,
not a new subcommand of it. `container/Dockerfile` sets no `ENTRYPOINT` for exactly
this reason (a fixed `ENTRYPOINT` can only ever prefix one binary): every `command:` in
`container/compose.yaml`, and every example above, names its binary by full path.

**Healthchecks differ per container.** `retnd` keeps the image's own baked-in
`HEALTHCHECK` (`retnd status`, real backup-freshness evidence against the
state database it actually holds). `web-ui` has neither a config file nor a state
database, so `container/compose.yaml` overrides its `healthcheck:` to
`/retnd-web healthcheck` instead - a plain HTTP GET against its own listener,
the only question that applies to a container whose entire job is "serve static files
and proxy requests."

**Headless mode is still just the other binary.** `/retnd daemon` (or `run`,
`check`, ...) never binds a web listener at all — override `retnd`'s `command`
in `container/compose.yaml` to `["/retnd", "daemon"]` (and simply omit the
`web-ui` service, or stop it) for a deployment that should never expose the API/UI at
all. `retnd status` works identically either way, since it is always a fresh,
read-only check against the shared state database file, independent of which binary is
actually running as `retnd`'s main process.

## Storage capacity, and capping what this manager may use

By default retnd measures the filesystem your backup root is on and reports
against the whole volume: no configuration, and useful from the moment setup finishes.
If you would rather it stayed inside an allowance, set a cap:

```yaml
capacity:
  # The ceiling on how much space this manager may occupy, in BYTES.
  # 0, or no capacity block at all, means no cap: use the whole volume.
  cap_bytes: 107374182400   # 100 GiB

  # Report WARNING at or below this much remaining headroom, and REFUSE a
  # transfer at or below this much. Both default to 0, meaning no line.
  # The warning line must be at or above the critical floor.
  warning_free_bytes: 21474836480
  critical_free_bytes: 10737418240

  # Held back on top of every incoming artifact's size before a transfer is
  # admitted, for listing drift and block rounding.
  safety_margin_bytes: 1073741824
```

Everything here is bytes. The Settings page shows an MB/GB picker beside the field and
converts before it saves, so the file never carries a number whose meaning depends on a
second key.

**The cap is enforced, not displayed.** FR-21's existing guard already refuses a transfer
the disk cannot hold; with a cap set it also refuses one that would push this manager
past the ceiling, and it refuses on whichever of the two runs out first, because a cap
does not help if the volume fills first. A refused transfer leaves the remote copy
exactly where it is and is retried on a later cycle. Nothing is ever deleted to make
room: FR-21's second rule is that retention is not something a full disk gets to
trigger.

**How "how much are we using" is measured.** From the catalog, not from the disk: the
state database already records what was transferred and how big each artifact was, so
the answer is one aggregate query that counts only files this manager put there. A `du`
over the backup root would be slow on a large tree and would count everything else
sharing the mount. The dashboard reports that figure alongside the volume's own used
space, so a gap between the two, which means something else is writing into your backup
root, is visible rather than folded away.

**Which filesystem gets measured.** The one your backup root is on, as the container
sees it. That root is derived from the directory your backup sets' `local_path` values
have in common, which for the shipped layout is the `/data/backups` mount. If your sets
sit on genuinely different volumes there is nothing to derive, and the dashboard says
capacity is not known rather than measuring the container's own root filesystem and
reporting a confident number about the wrong disk. Name the one you mean if that
happens:

```yaml
capacity:
  backup_root: /data/backups
```

Every reading says which path it was taken from, so a wrong mount is something you can
see rather than something you have to suspect.

## Proactive alerting

retnd can tell an administrator that something is wrong without anyone
having to open the dashboard. It notifies on exactly four conditions
(`docs/EPIC-B-multi-nas.md` §71): a **stale backup**, **repeated failure** on a backup
set, a **changed SSH host key**, and **critical storage pressure**. That list is
deliberately closed; this is one narrow notification path, not a general alerting
framework, and it will not grow one in v1.

It is off unless you turn it on:

```yaml
alerts:
  enabled: true
  # How many artifacts must be sitting in FAILED for one backup set before
  # that counts as "repeated failure". Omit it for the default of 3.
  repeated_failure_threshold: 3
```

A config file with no `alerts:` block at all keeps working exactly as before and
notifies nobody, so turning this on is always a deliberate edit.

**Where the alert goes is not configured here.** Delivery is the platform's own local
notification capability, supplied by the provider app rather than by this file, which
is why there is no URL, command or credential to get wrong. A platform that declares
no native notification capability, and the generic Docker/Linux host is one, cannot
deliver: `/retnd-web serve` prints `proactive alerting is off` at startup and
carries on running backups normally. It never emulates delivery, so alerting is either
visibly on or visibly off, never silently swallowed.

**An alert is a notification and nothing else.** A critical-storage or repeated-failure
alert never triggers retention, never deletes anything to free space, and a changed
host key still requires you to verify the new key out of band and update `known_hosts`
yourself. The connection stays refused until you do.

**The same unresolved problem is reported once**, not once per poll. A condition that
clears and later comes back does alert again, so a recurrence is never lost behind a
notification you already dismissed.

## Metrics, and the one-release duplicate series

`core/internal/metrics` renders FR-24's health report as Prometheus text exposition
(version 0.0.4), and every series it emits is prefixed `retnd_`:
`retnd_process_info`, `retnd_backup_set_state`, `retnd_workflow_runner_reachable`,
`retnd_workflow_runs_total` and the rest. **Nothing in this build serves it yet** —
there is no subcommand and no route wired to it (issues #25, #26) — so a deployment
today has no scrape endpoint to point Prometheus at. This section is here because the
names moved and because the move has one hazard worth reading before you write a query
against them.

Those series were `backupd_*` until EPIC R (#885) renamed the product. An alert rule
whose series stopped existing does not fire, and a dashboard whose query matches nothing
is blank; both look exactly like a healthy deployment, which is the failure mode this
project will not ship. So **every gauge family is emitted a second time under the
`backupd_` prefix for one release**, derived from the bytes of the first rendering so the
two cannot disagree, with `DEPRECATED, renamed to retnd_…` in its own `# HELP` line —
so a scrape carries its own deprecation notice.

**The caveat: a query that reads both prefixes double-counts.** The duplicated samples
are the same readings under two names, not two measurements, so
`sum(retnd_backup_set_state) + sum(backupd_backup_set_state)` reports twice as many
backup sets as exist, and so does any regex matcher loose enough to catch both names
(`{__name__=~".*backup_set_state"}`). Point every rule, recording rule and dashboard at
`retnd_*` only. The old names exist so that a rule you have **not** migrated keeps
firing during the upgrade, not so that both can be read at once.

**No counter is duplicated, and that is structural rather than remembered.** The
duplication copies a family only when that family's own `# TYPE` line says `gauge`, so a
counter cannot join the duplicated set by being added to a list. Summing `rate()` across
two names of one counter doubles the rate, and nothing in the output would tell the
operator who wrote that query — which is why `retnd_workflow_runs_total` and the other
`_total` series exist under one name only.

**The window is one release.** #895 deletes the duplication along with every other
EPIC R shim, and `scripts/rename/check-brand-drift.sh`'s alias list is the ledger that
says so: it reports an alias entry that has stopped matching anything, so the window
closes because a gate noticed rather than because somebody remembered.

## Turning on diagnostics

Some faults only happen on a real deployment. Issue #730 is the example this
section exists for: one NAS whose Activity page failed with `TypeError: Failed to
fetch` — no HTTP response reaching JavaScript at all — while `curl` against the
same route answered cleanly, and a rig built from the shipped image never
reproduced it. Nothing in the default log said anything, because nothing had been
asked to.

There are **three** switches, and they are three because a request crosses three
places that each know something the other two cannot see.

**Both containers.** `LOG_LEVEL=debug` in `container/.env`, which
`container/compose.yaml` passes to `retnd` and to `web-ui` alike:

```
# container/.env
LOG_LEVEL=debug
```

```
docker compose -f container/compose.yaml up -d
docker compose -f container/compose.yaml logs -f
```

`RETND_DEBUG=1` is the same switch under a shorter name, kept because it is
the one an operator can be given over a phone call; it wins if both are set. An
unparseable value falls back to `info` rather than refusing to start — a typo in a
diagnostic knob must never take a backup host down.

`BACKUPD_DEBUG=1` and `RM_DEBUG=1` are that shortcut's two **deprecated** older
spellings, one per name this project has had: `BACKUPD_DEBUG` from before the
rename to `retnd` (EPIC R, issue #885) and `RM_DEBUG` from before the rename to
`backupd` (issue #794). Both are still read, so a deployment upgraded without its
compose file or its unit being re-derived does not go quiet in the middle of a
diagnosis, and both are removed in the release after the one that renames this
product. The current name wins when more than one is set, and the process prints
one deprecation notice per deprecated name it read, on stderr, at start — not once
per read, so a running deployment is not punished for an old compose file. Set
`RETND_DEBUG` and delete the others.

**Set it on both services or neither.** The engine records what it built and
served; `web-ui` records what the engine answered, what framing the body arrived
with, and how many bytes of it actually reached the browser. Every line on both
sides names the same `correlation_id`, which is what lets the two accounts of one
request be read as one story — and one container at `debug` gives you half of
every story with nothing to join it to.

What appears at `debug` that does not appear at `info`:

| event | container | what it answers |
| --- | --- | --- |
| `activity_debug` | `retnd` | what the activity feed actually served: how many events, how many bytes, which cursor, and the forwarded headers it was asked under |
| `proxy_upstream_headers` | `web-ui` | what the engine answered and with what framing — status, `Content-Length`, `Content-Encoding`, `Transfer-Encoding` |
| `proxy_upstream_complete` | `web-ui` | what the body turned out to be: bytes actually copied against the length declared, whether the read ended at EOF, and any read or close error. A body that ends short of its declared length is logged at `warn` |

Two events are recorded whatever the level, because a failure to answer the
browser at all is not something an operator should have to have predicted:
`proxy_error` (the reverse proxy could not produce a response — an unreachable
engine, or one that accepted the connection and never replied) and `http_refusal`
(the API returned a 500 and could say why in the log without saying it to the
client).

**The browser.** The page has its own half, and it is not an environment
variable: the two containers cannot see what `fetch` threw, and a request that got
no response carries nothing back to correlate with. Open the deployment with
`?debug=1`:

```
http://your-nas.local:8080/activity?debug=1
```

That persists (it sets `rm-debug` in `localStorage`), so a reload or a navigation
keeps logging; `?debug=0` clears it again, and so does
`localStorage.removeItem('rm-debug')` in the browser console. Filter the console
on `rm-debug` and every line this channel writes appears and nothing else. Each
line carries an `attemptId` — the browser's own name for that one attempt, sent as
`X-Client-Attempt-Id` — and the server writes the same value into its line for the
request, which is how a console screenshot of a request that produced **no
response at all** can still be matched to what the server did with it. Where a
response did arrive, both sides also name the same `correlation_id`, which every
response carries in `X-Correlation-Id`.

**Turn it back off when you are done.** At `debug` both containers write a line
per API request, and the browser writes one per `fetch`. That is a diagnostic
posture, not an operating one.

## Release hashes

`scripts/release/record-release-hashes.sh` builds `container/Dockerfile` for both
`linux/amd64` and `linux/arm64`, extracts `/retnd` and `/retnd-web`
from each built image, and writes their SHA-256 hashes (plus each build's local Docker
image ID) to `container/release-manifest.json` — the Phase 4 TDD Gate's "binary
SHA-256 and image/package digests," and §8's "release manifest SHALL prove core parity
through binary hashes and image/package digests."

```
git checkout main && git pull            # a commit that is ALREADY on main
git status --porcelain -- core apps ui   # must be empty
bash scripts/release/record-release-hashes.sh
```

**Run it on a commit that is already on `main`, from a clean tree.** This is the whole
lesson of issue #174. The manifest previously pinned `c51a07f`, recorded on a feature
branch; GitHub squash merged that branch, which rewrote the commit, and the manifest
was left describing a build no checkout could reproduce. Every parity check phrased as
"matches the release manifest" was then comparing against a fiction, and nothing
noticed for weeks. The script now refuses five ways of producing that: a `COMMIT` that
names no commit here, a `COMMIT` that is not `HEAD`, a working tree that is dirty in a
path the image is built from, a `REACHABLE_FROM` that cannot be resolved, and a commit
that is not an ancestor of `origin/main`. The last one tells "git said no" apart from
"git could not decide": a shallow clone makes `merge-base --is-ancestor` exit 128, and
that is a fact about the checkout rather than about the manifest, so it gets its own
message and its own remedy.

Those refusals are not taken on trust. `scripts/tests/record-release-hashes-guards.test.sh`
drives the real script through its `GUARDS_ONLY=1` seam in a throwaway repository per
refusal, asserting the exit code and the distinct message for each, and it runs on every
non-FAST `scripts/ci-local.sh`.

`UNSAFE_LOCAL_BUILD=1` waives all five for a throwaway local build. It is deliberately
hard to commit the result: the output path defaults to
`container/.generated/release-manifest.local.json` (already gitignored) instead of the
tracked manifest, and what it writes carries `"unsafe_local_build": true`, which
`distribution/packaging` refuses outright. Overwriting the tracked manifest with a waived
run takes an explicit `OUT=`, and the checked-in file would then fail the build.

`distribution/packaging`'s `TestReleaseManifestPinsACommitThisHistoryCanReach` and the
`release-manifest-integrity` conformance row both re-ask the ancestry question on every
run, against the strongest ref the checkout has (`origin/main`, else `main`, else
`HEAD`), so a manifest that drifts out of the history fails the build rather than being
found by hand.

The manifest checked in today names the commit it was produced at in its own `commit`
field, and a second run from that same clean checkout reproduces its binary hashes
exactly. The SHA is not repeated here on purpose: it moves with every release, and a
copy of it in prose is a copy that goes stale without anything noticing.

**What this records about the registry**: nothing yet, for the version currently cut.
`distribution/packaging/canonical.json` records `image.published: false` for
`ghcr.io/backupdproject/backupd:0.4.0`, and the manifest carries a `registry_digest` of
`null` per architecture and a null `index_digest` to say the same thing from the other
side. `TestReleaseManifestRegistryDigestTracksTheCanonicalPublishFlag` holds the two
together in both directions: a published flag with no digest and a digest with no
published flag are both half-truths.

They are filled in from a real push rather than from the push's own output. That is how
`0.3.3` was recorded: `docker buildx imagetools inspect` read each architecture's digest
back out of ghcr.io, because the push says what it believes it sent and the registry says
what it holds, and the multi-architecture index those two sat under, `sha256:bc3cbcd4`,
was keylessly signed through the release workflow's own OIDC identity with the SBOM
attested beside it. `0.4.0` gets the same treatment once the workflow pushes it.
`local_image_id_sha256` stays what it always was, the local Docker image ID the
recording build produced, which resolves nowhere but the machine that built it and is
never a stand-in for a digest.

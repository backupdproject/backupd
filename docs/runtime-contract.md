# The canonical runtime contract

Issue #167 (B6.3), EPIC B #81 Phase 6.

`container/compose.yaml` is the authoritative definition of how this product
runs. Every other deployment artifact in this repository derives from it, and
`distribution/compose` fails the build when one of them stops agreeing.

"Authoritative" here means a check, not a path. Before this issue the file was
correct, carefully reasoned and thoroughly commented, and none of that was
mechanically true: its security posture was asserted in a header comment, its
field set was whatever had accumulated, and an adapter agreeing with it was a
matter of review. `distribution/compose/runtime-contract.json` is the
contract-shaped version, and the suite next to it is what makes the file
authoritative.

## The standardised field set

Every field below must be declared. Deleting one fails
`TestCanonicalDefinitionDeclaresEveryRequiredField` by name, with the reason
the contract gives for requiring it, and
`TestRequiredFieldCheckFailsWhenAFieldIsRemoved` removes each one in turn to
prove the check can actually see it go.

| field | where | what it is |
|---|---|---|
| `image-reference` | both services | one canonical image, named identically by both, so `command` is the only difference between them |
| `command-and-runtime-profile` | both services | the command **and** `--profile=<name>`, standardised as one field |
| `listen-port` | web UI | the one published port; the engine deliberately publishes none |
| `health-check` | both services | declared here, not inherited from the image and described in a comment |
| `start-gate-liveness` | engine | the engine's healthcheck asks `/health/live`, never backup freshness: `web-ui` waits on it. Checked on every derived artifact too, not only here (issue #206) |
| `graceful-shutdown-period` | both services | `stop_grace_period`: 30s for the engine, 15s for the UI host |
| `restart-policy` | both services | `unless-stopped` |
| `ownership` | both services | explicit `user: PUID:PGID` |
| `explicit-writable-paths` | both services | `read_only: true`, which is what makes the writable paths explicit |
| `timezone` | engine | `TZ`, because retention is evaluated against calendar boundaries |
| `private-state-mount` | engine | `/data/state` |
| `backup-data-mount` | engine | `/data/backups` |
| `configuration-mount` | engine | `/etc/retnd/config`, a writable directory holding `config.yaml` (issue #196) |
| `secret-file-mount` | engine | `/etc/retnd/id_ed25519`, read-only |
| `resource-expectations` | document | `x-canonical-runtime.resources` |
| `supported-architectures` | document | `x-canonical-runtime.architectures` |
| `digest-policy` | document | `x-canonical-runtime.digest_policy` |
| `contract-version` | document | `x-canonical-runtime.contract` |
| `runtime-profiles` | document | `x-canonical-runtime.profiles` |

### The two mounts that must never contain one another

`/data/state` holds the lifecycle journal, the local-authentication
administrator record — with the account's recovery email address, whether that
address has been verified, the deadline an unverified one lapses at, the hash
(never the value) of the outstanding verification token, its SMTP settings and
the reference standing in for the SMTP password — and nothing an operator would
ever hand to somebody else.
`/data/backups` is a share people are given access to. Putting either inside
the other puts the state database, the Argon2id password hash and the
administrator's own contact details into a
directory whose whole purpose is to be shared, so
`TestPrivateStateAndBackupDataAreSeparateMounts` checks containment both ways,
and checks that its own containment helper is not vacuous.

## The prohibition list

The canonical definition and every derived artifact are checked against all of
these. None may be required:

```text
privileged: true
network_mode: host
host PID namespace
host IPC namespace
/var/run/docker.sock (or /run/docker.sock)
unbounded host filesystem access (/, /etc, /usr, /var, /boot, /proc, /sys, /root, /home)
cap_add
seccomp / AppArmor unconfined
```

An absent setting and an unnecessary setting are different claims, so the
prohibition list is checked two ways. Statically,
`TestProhibitionCheckFiresOnEveryProhibitedSetting` injects each prohibited
setting into a copy of the real definition and requires the matching rule to
fire and to name the service and the value: a rule nobody has watched fail is a
comment. And `TestProhibitionScanSeesKeysTheParserHasNoFieldFor` covers the
specific way a check like this fails open, which is parsing compose into a
struct that has no field for `privileged` on a service nobody modelled.

At runtime, the existing `apps/generic/tests/dockercli` suite runs the real
built image with none of them and completes real work.

Any future exception needs its own security review. A thin distribution adapter
cannot introduce one: the prohibition rules run against every registered
artifact, and `TestEveryComposeArtifactInTheTreeIsRegistered` fails when a
compose file exists in the tree that nothing registered.

## Local hook scripts, and why they do not touch this contract

EPIC L (#809) gives a backup set hook scripts, and a `.local.sh` hook means
"run this on the machine retnd is installed on". The engine cannot run one:
this image is distroless and has no shell, the container is read-only and
non-root, and every capability is dropped. That is not an obstacle to work
around — it is the contract above.

There are four ways to satisfy "run a script on the host", and three of them
are this contract deleted:

- add a shell to the image;
- mount the Docker socket, or add `CAP_SYS_ADMIN` and use `nsenter`;
- bind-mount the host root read-write and chroot into it;
- run a **separate, version-matched, unprivileged process on the host** and let
  the engine ask it, over one Unix-domain socket, with a narrow vocabulary.

retnd does the fourth. The engine gains exactly three bind mounts and nothing
else:

```text
${WORKFLOWS_DIR:-./workflows}:/workflows:ro    the hook scripts, READ-ONLY
${RUNTIME_DIR:-./run}:/data/run                the runner's socket directory,
                                               and nothing but the socket
${RUNNER_TOKEN_FILE:-./secrets/workflow-runner.token}:/etc/retnd/workflow-runner.token:ro
                                               the runner's credential, ONE
                                               read-only FILE
```

The credential mount is the third, and without it the other two buy nothing:
the runner authenticates every connection, and the engine reads the token from
the path `workflows.runner.token_file` names **as the engine sees it**. The
installer writes that token into `<prefix>/secrets`, which nothing else here
mounts, so the configuration a Docker Compose deployment documents is:

```yaml
workflows:
  runner:
    socket: /data/run/workflow-runner.sock
    token_file: /etc/retnd/workflow-runner.token
```

It is a single **file**, mounted read-only, exactly like the SSH key and
`known_hosts` and for the same two reasons: nothing in this container writes
it, and the rest of the secrets area — the repository passphrase, every
storage-medium credential — is not this container's business. It adds no
capability, no group, no socket and no host-root path, and the rootfs stays
read-only, so the prohibition list below is unaffected.

The scripts are read-only because the engine reads each one once, hashes it and
copies it into its own private spool under `/data/state`, and never opens the
original again (`core/internal/workflow`), so write access would buy nothing and
would let a compromised engine edit what the host is about to execute. The
runtime directory is writable because connecting to a Unix socket is a write.

That is also why it holds the socket and nothing else. A read-write mount makes
everything under it writable by this container, and the runner's per-step
working directories are the paths it creates and later removes recursively —
so they live in `<prefix>/workspace` on the host, which nothing here mounts
(ADR 0020, Decision 6a).

Neither mount uses `:?`, so a deployment whose `.env` predates EPIC L still
starts.

What is on the other end of that socket is deliberately not a shell. It is
`retnd workflow-runner serve`, built from the same commit as the engine and
extracted from the same image by the installer, and it:

- listens on a Unix socket only — there is no TCP listener and no address to
  configure — with the socket 0600 inside a 0700 directory;
- additionally requires an installation-scoped credential from retnd's
  secrets area on every connection;
- refuses an engine whose version is not exactly its own;
- accepts four operations (`syntax-check`, `execute`, `cancel`, `status`) and
  **captured script bytes, never a path**, re-checking size and SHA-256 against
  what the engine's plan recorded;
- refuses to run as root, runs each script in an **ephemeral Docker container**
  with a private 0700 working directory on the host, and stops and removes that
  container when the engine's connection — the lease — goes away.

### The runner holds Docker access; this container never does (#865)

A local hook does not run on the host's shell either. Since #865 the runner
launches one ephemeral container per hook:

```text
docker create --name backupd-hook-<run>-<step>-<16 hex>
           --label backupd.workflow-hook=1 (plus run, step and instance labels)
           --network none --security-opt no-new-privileges --cap-drop ALL
           --read-only --tmpfs /tmp --pids-limit 512 --user <uid>:<gid>
           --platform <the daemon's own> --entrypoint <bash in the image>
           --env NAME ... (names only; values travel over the daemon socket)
           -v <per-step work dir>:<itself> -v <captured script>:<itself>:ro
           <hook image> --noprofile --norc <the script>
docker start --attach <that name>        the hook runs; two streams come back
docker inspect <that name>               the hook's own exit status
docker rm --force --volumes <that name>  and the proof it is gone
```

The lifecycle is explicit rather than one `docker run`, and there is no
`--rm`: the container's name and labels exist on the daemon's side before any
process does, so a cancel mid-launch has something to address, and the hook's
exit status is read off the DAEMON — the client reports 125 both for "the
container could not be created" and for a hook that ended in `exit 125`.
Termination reconciles by the per-launch label, so a container the daemon
finished creating after the client was gone is still found and killed, and
every call on that path is bounded: a daemon that stops answering costs the
runner certainty (`unconfirmed`, which also keeps the working directory), never
its ability to return.

No Docker socket is mounted into a hook container, under any name — the runner
refuses a configured `--hook-mount` that names one, or that names a directory
the socket is IN. A hook reaches nothing else on the host unless an operator
declared it with `--hook-mount PATH[:ro|:rw]`, read-only by default; the ENGINE
cannot ask for a mount, because the protocol between them has no field that can
hold a path. `--hook-network host` and `--hook-network container:<id>` are
refused: both put the hook inside a namespace somebody else owns.

This does move one privilege onto the host: the runner needs to reach the
Docker daemon, which on a NAS is root-equivalent. That is the trade #865 makes
deliberately, and the containment is that the privilege belongs to the small
version-pinned process that launches hook containers and to nothing else:

- the unit adds `SupplementaryGroups=<the socket's group>` and that same
  socket to `ReadWritePaths`, plus the connection the install itself used —
  `Environment="DOCKER_HOST=..."` (or `DOCKER_CONTEXT`), `DOCKER_CONFIG` where
  set, and `--docker <absolute path>` on `ExecStart`, because systemd inherits
  none of the installer's environment and the runner does not search `PATH`.
  Without them an install can succeed against one daemon and the runner refuse
  to serve against another;
- **this container gains nothing.** No socket, no `group_add`, no capability,
  no `DOCKER_HOST`. It still has no shell and still cannot exec.

The capability is PROVEN at the runner's startup — docker present, daemon
reachable, hook image present for this platform, and a probe container that
emits the runner's own marker — and a host that cannot do all four refuses to
serve local hooks. There is no fall back to running a hook on the host's own
shell: a fallback would make every property above conditional on a daemon
nobody checked.

The prohibition list above is unchanged and still passes against the canonical
definition and every derived artifact: no privileged container, no Docker
socket, no host namespace, no added capability, no unbounded host mount, no
shell in the image. `docs/adr/0020-host-workflow-runner.md` carries the full
argument.

## Runtime profiles

A runtime profile is how one executable changes host-dependent behaviour
without becoming a second build.

```
retnd-web serve    --profile=generic
retnd-web serve    --profile=ugos --trusted-upstream=172.19.0.2/32
retnd-web serve-ui --profile=ugos --trusted-gateway=10.1.2.3/32 \
                            --ui-root=/usr/share/retnd/ui
```

Seven profiles exist: `generic`, `ugos`, and the five issue #169 added when it
converted the shipped platform packaging (`truenas`, `unraid`,
`openmediavault`, `proxmox`, `synology`). Those five declare no capability and
no gateway, and that is the finding rather than an omission: every one of them
uses section 13A local authentication, none has a server-side notification
channel, and the capabilities their frontend bridges declare are browser-host
capabilities this Go process cannot deliver. What a profile changes for them is
exactly what legitimately differs, which is the platform the runtime reports
itself as, how the deployment is described, and which UI bridge the Web UI host
serves. That is not nothing: before it, a user who installed through the
TrueNAS catalog was told by the running application that this was a generic
Docker Compose deployment.

A profile may change exactly four things:

- a trusted native authentication gateway,
- a provider notification bridge,
- a platform launch or navigation bridge (which UI bundle is served),
- platform capability reporting.

It may never change backup lifecycle, retention or validation semantics, and it
may never change authorization semantics either. A profile can supply an
identity; it cannot decide what that identity may do.

That is enforced two ways. Structurally, `profile.Profile` may declare no field
outside an allow-list, and the checker is proved non-vacuous against a
deliberately forked profile carrying `RetentionTiers` and `LifecycleStates`.
Behaviourally, `apps/common/webhost/serve/parity_test.go` stands up one engine
per profile over **one shared backend** and compares whole response bodies on
the lifecycle, retention, validation and storage reads. Its own positive
control is `/api/v1/system/capabilities`, which must differ, because reporting
what the host can do is one of the four things a profile is allowed to change.

Profile dispatch is startup-time configuration. There is no per-request
profile indirection to pay for, and the parity suite would show one if there
were: it drives every route through a real listener rather than calling a
handler.

Selecting an unknown profile is refused with exit code 2, before anything opens
a file or a listener. There is no fallback to `generic`: a deployment that
asked for `ugos` and silently got the generic authentication story is the
outcome that refusal exists to prevent.

### The trusted-gateway boundary

`--profile=ugos` declares that an identity header set by the platform gateway
may be believed. It is believed **only** from a network source the deployment
declared:

- a request from outside the declared range is refused with
  `ErrUntrustedPeer`, its identity header is never read, and the refused
  `AuthContext` never carries the forged username;
- a request from inside it with no identity is refused with
  `ErrNoGatewayIdentity`, which is a different error on purpose: one is an
  attack and the other is a misconfigured gateway;
- an identity that names more than one caller is refused with
  `ErrAmbiguousIdentity`, in both wire forms: a repeated header line, and a
  single value a proxy joined with a comma;
- a remote address that does not parse is untrusted, never trusted by accident;
- `--profile=ugos` with **no** declared range refuses to start at all, on
  either hop, because without a declared peer there is no gateway, only a
  header anyone on the LAN can set.

**The two hops trust different peers, and each has its own variable.**
`serve-ui --trusted-gateway` (`TRUSTED_GATEWAY_CIDRS`) names the platform
gateway, which is the only boundary the network can still answer a question
about: that container holds the one LAN-facing published port. `serve
--trusted-upstream` (`TRUSTED_UPSTREAM_CIDRS`) names the engine's own single
possible peer, the reverse proxy in front of it on the internal network. A
single variable feeding both is the configuration that cannot be correct: the
only value that lets the engine authenticate is one that also makes the edge
believe an identity header from anything on the internal bridge, which under
Docker's userland port publishing includes LAN traffic arriving at the
published port. `serve` therefore refuses `--trusted-gateway` by name rather
than silently accepting the wrong hop's variable.

Write the range as narrowly as the deployment allows, and never as a `/8`.
The values above are single addresses on purpose: the range's job is to name
the gateway, and a range wide enough to hold the whole LAN says every host on
the LAN is the gateway.

`CompiledGateway.Sanitize` strips the identity header from an untrusted
request, and it runs on the request path in both processes, so "stripped or
ignored" is literally true rather than a description of the authenticator's
behaviour. Its positive control checks that the same header from the *trusted*
peer survives, so the test is about trust and not about a function that
deletes everything it sees.

**Which hop strips.** Both, and they answer for different peers.

The engine publishes no port, so its only peer is `web-ui` over the internal
network. Its trust test can therefore only ever say "this came from `web-ui`",
which is equally true of a header the gateway set and one a client on the LAN
set against the published port and `web-ui` forwarded. `serve-ui` is the hop
that can tell them apart, because it holds the only published port and its
`RemoteAddr` is the real client's, which is why `serve-ui` takes
`--trusted-gateway` too and refuses to start a gateway profile without one.

Stripping unconditionally in the proxy would be the wrong fix, and quietly so.
On UGOS the platform gateway sits *upstream* of `serve-ui`, so a blanket delete
removes the legitimate identity and native authentication stops working
entirely. The proxy carries the same declared trust boundary the engine does
rather than a bare header name, and the engine strips as well so nothing
downstream of it, a handler or a log line or a middleware added later, can read
a value that was never trusted.

EPIC C's #92 proves the same boundary against the real UGOS gateway on real
hardware. This side needs no UGREEN device: the synthetic trusted peer is
loopback and the synthetic untrusted peer is everything else.

## Runtime-selected UI bundles

Issue #180. `serve-ui` used to embed one bundle with `go:embed` and offer no
alternative, and `ui/shared/vite.config.ts` picks the provider shell at build
time from `VITE_PLATFORM`. Together those meant shipping Synology's bridge
required compiling a Synology-specific binary, and section 3.7 requires every
provider package to carry the exact same core binary. The choice was between
the wrong bridge and a forbidden build.

The bundle is now resolved at run time, in this order:

1. `--ui-dir PATH` (`$UI_DIR`), an explicit directory. Wins outright.
2. `--ui-root PATH` (`$UI_ROOT`) plus the selected profile: the bundle served
   is `PATH/<profile>`.
3. the bundle compiled into the binary.

A configured `--ui-dir` or `--ui-root` that turns out to be unusable is a hard
start failure, never a silent fall back to the embedded bundle. "Unusable"
means missing, not a directory, or without an `index.html`. An empty directory
is exactly what a bind mount that did not mount produces, and serving it would
answer every route with 404 instead of saying what went wrong.

`ui/shared/scripts/build-bundles.mjs` (`npm run build:bundles`) builds one
bundle per provider into `dist-bundles/<provider>/`, which is what a package
ships beside the binary.

`apps/generic/tests/uibundle` proves the property that matters against a real
built artifact rather than against a function: one binary serves the embedded
bundle, two different profile-selected bundles and one package-supplied
bundle, and its sha256 is unchanged afterwards. A second test builds the same
source twice with a provider named in the environment and requires the two
digests to be identical, with a control proving the comparison can see a real
change.

### Which carrier each adapter uses

Issue #169 packaged this, and there are exactly three carriers because there
are exactly three kinds of thing that can hold a bundle.

| Carrier | Who uses it | How the bundle is selected |
|---|---|---|
| the binary | generic, Portainer, Dockge, CasaOS, ZimaOS | nothing configured; the compiled-in bundle |
| the canonical image, at `/ui/bundles` | TrueNAS, Unraid, OpenMediaVault, Proxmox, Synology's Container Manager project | `UI_ROOT=/ui/bundles` plus the adapter's own `--profile=` |
| the package's own payload | Synology's `.spk` | `--ui-dir <target>/ui-bundle` |

An adapter that is metadata and nothing else, which is what a catalog entry, a
Docker template or a compose profile is, has no payload to put a bundle in, so
the image is its only carrier. A `.spk` installs native binaries and never
pulls the image at all, so the image is no carrier for it and it carries its
own; `spk.Build` refuses a package built without one, or with one built for
another provider, because a package that installs cleanly and shows the wrong
interface is the failure mode this whole issue is about.

**Five bundles in the image, and not seven.** `generic` is already compiled
into the binary and duplicating it buys nothing. `ugos` is EPIC D's, and its
UPK carries its own. The rest is arithmetic against a gated budget: measured
on `darwin-arm64-mac17-2`, `linux/arm64`, the image goes from 43,008,762 bytes
to **44,811,244** bytes, which is +1,802,482 (+4.19%) against a ceiling of
45,159,200 (1.05x). That leaves 347,956 bytes of headroom, which is less than
one more bundle: this image can carry these five and not a sixth. Shipping all
seven, as #167 estimated, would have been roughly 2.4 MB and outside the gate.

That paragraph is #180's measurement and it is left as it was taken. Both sides
of it have since moved, and #635 re-measured them on the same host and platform
on 2026-09-08: the five bundles now hold 3,503,996 bytes rather than 1,802,482,
because #632 put 139,744 bytes of woff2 into each one and EPIC F, G and H grew
the JS chunk.

**The arithmetic no longer decides the count, and the count is unchanged for a
different reason.** #180's sum worked because the baseline it measured against
was an image carrying no bundles, so each one had to be paid for out of the
headroom. The baseline was re-captured at 69,704,266 with these five inside it,
which means charging them to the 5% counts them twice. Re-derived there it points
both ways at once: 5% is 3,485,213, the five measure 3,503,996 so charging them
puts five over by 18,783, and not charging them leaves 3,485,213 against
1,401,598 for the two missing bundles, so seven would fit with 2,083,615 spare.

What actually settles it is that `generic` is compiled into the binary and `ugos`
ships in EPIC D's UPK. Those two have a carrier, so a directory for either is
bytes nobody serves. That holds whatever the image weighs. A reader sizing a
sixth bundle should take the image size from `docs/perf/` and the count from that
sentence, and not use either as an argument for the other.

The end-to-end evidence, against the built image rather than against a
function:

```
serve-ui --profile=truenas --ui-root /ui/bundles  ->  deployment: "TrueNAS app (container)"
serve-ui --profile=generic                        ->  deployment: "Docker Compose"
serve-ui --profile=ugos    --ui-root /ui/bundles  ->  refuses to start:
    no usable UI bundle: --ui-root /ui/bundles has no usable bundle for
    profile "ugos" at /ui/bundles/ugos
```

The third line is the one worth reading twice. A missing bundle is a hard
start failure, never a silent fall back to the generic bridge, which is why
carrying too FEW bundles is a loud failure and not a quiet reappearance of
#180.

**The four adapters #170 adds share the first row with `generic`, and the
arithmetic above is why.** 347,956 bytes of headroom is less than one 352 KB
bundle, so the image that carries five cannot carry six, let alone nine.
Portainer, Dockge, CasaOS and ZimaOS therefore ship no frontend bridge at all:
they select the `generic` runtime profile, serve the bundle compiled into the
binary, and the shared UI describes them as the Docker Compose deployments they
are.

That is not only a budget decision, and it would be the same decision with room
to spare. A bridge for any of the four would need its platform id in the
`/api/v1` contract, the capability table, the profile table and the bundle list,
which is core and shared-UI code in four adapters whose own contract forbids
exactly that: #170 states it for two of them as "no CasaOS or ZimaOS import
appears in either". None of the four has host-dependent behaviour for a profile
to select, either. No native identity gateway, no notification bridge, no launch
bridge, so a profile per platform would be four rows that change nothing but the
name in a capability report, which is how a platform-specific code path starts.

`canonical.json` records the choice per platform as `uiBridge: "none"` with the
reason, and the rule on that field is two-sided: a platform that declares a
bridge has its `storageMount` pinned to what the bridge says, and a platform
that declares none must have no `frontend/` directory either, so "none" cannot
become the cheap way out of the pin.

## Deriving an adapter instead of authoring one

Issue #169. Phase 4 shipped five platforms that agree with the canonical
runtime by review: each states its own image reference, its own mounts, its
own port and its own health check, and nothing compared those statements to a
single source. Five independently authored copies of one runtime definition is
the definition of drift.

`distribution/packaging/derive.go` makes the agreement mechanical. Seven
fields, one authoritative value each, and a mismatch that names the field:

| Field | Authority |
|---|---|
| `contract-version` | each platform's `derivesFrom.contract` against the contract's own version |
| `image-reference` | `canonical.json`'s `image.reference` |
| `runtime-profile` | `x-canonical-runtime.profiles`, and the platform's own declared profile |
| `storage-mounts` | `canonical.json`'s `containerPaths`, all of them, and none besides |
| `published-port` | `canonical.json`'s `listenPort`, on the Web UI role only |
| `health-check` | `container/compose.yaml`'s per-role tests, restated in `canonical.json` so four metadata formats can be held to them |
| `supported-architectures` | the release, once; an adapter states none of its own |

`contract-version` is the one that keeps the other six honest over time. A
derivation check that only compares values keeps passing after the contract
grows a field nobody applied, because there is no value to disagree with yet.
Repeating the version in each adapter makes a contract change fail every one
of them until somebody has re-derived it and said so.

### Where the engine's health check is decided

`container/compose.yaml`, and nowhere else. `canonical.json` restates both
per-role tests so `derive.go` can hold four metadata formats to them, including
an Unraid XML template no Compose parser can read, and
`TestTheCanonicalDefinitionIsWhereTheHealthChecksAreDecided` fails the build
when the restatement stops matching. Nothing compared those two before issue
#206, which is how the canonical definition came to declare a liveness probe
while every adapter derived the backup-freshness verdict from a copy nobody
had changed, for three work packages, with every suite green.

The engine's check is a liveness question. Every adapter's web UI declares
`depends_on: <engine>: condition: service_healthy`, so whatever it asks stands
between an operator and the only LAN-facing container in the deployment.
`retnd status` is FR-24's verdict and exits non-zero on a fresh
install by design, which made "install the app" and "reach the app" mutually
exclusive on all nine adapters.

Backup freshness is unchanged and unweakened. `status` is still the
operator-facing verdict, still `container/Dockerfile`'s own baked-in
`HEALTHCHECK` (so a plain `docker run` reports it, and so does the headless
`daemon` command, which serves no HTTP and has no liveness endpoint to ask),
and still what the alerts block delivers. What no longer depends on it is
container startup ordering.

Because the image's instruction and the canonical start gate now deliberately
differ, an adapter that declares no engine health check inherits the verdict
rather than the gate. `derive.go` allows that only where nothing waits on the
engine's health, which is Unraid and only Unraid: its template schema has no
health-check seam, and it declares no start-ordering dependency either, so the
badge it produces is the freshness report it is meant to be.

Every field has a positive control that breaks it deliberately, run against
every adapter rather than against one, because the five are read out of four
different metadata formats and a rule that fires on a compose file and not on
an Unraid template is a rule Unraid does not have.

## Migrating a Phase 4 installation

The conversion changes declarations, not data. On every platform the state
directory, the backup root, the SSH key and `known_hosts` keep the host paths
they already had, so an existing installation keeps its catalog, its retained
artifacts and its enrolled administrator across the change.

One mount is redeclared, and the host side of it does not move either:

| | Phase 4 | Converted adapter |
|---|---|---|
| host path | `<appdata>/config/config.yaml` | `<appdata>/config` |
| container path | `/etc/backupd/config.yaml` | `/etc/retnd/config` |
| mode | `ro` | writable |

The file an operator already has stays exactly where it is; what the adapter
mounts is its parent directory, writable, which is issue #196. The directory
has to be writable by the container's uid/gid before the first start, because
a bind mount does not chown its source, and each platform's acceptance
procedure step 0 now says so.

What enforces the change is not the same on every platform, so it is worth
saying per platform rather than once:

| platform | what carries the old answer | what stops it |
|---|---|---|
| generic, OpenMediaVault, Proxmox | an env file the operator edits | `CONFIG_FILE` became `CONFIG_DIR`, a fail-closed `${VAR:?}` reference, so an unconverted file stops the deployment with a message |
| Synology (Container Manager) | `retnd.env` | the same, `${APPDATA:?...}/config` |
| TrueNAS (catalog) | the platform, not a file | the question was renamed `config` to `configDir`, so an upgrade has no stored answer to carry forward and the wizard asks again |
| Unraid | the operator's own template copy | nothing automatic: a changed `<Config>` Target does not retire a mapping already in the user template, so the old read-only file mapping has to be deleted by hand |
| Synology (`.spk`) | the package's own layout | the package installs the directory itself |

The `${VAR:?}` claim only ever covered the first three rows. There is no
`${VAR:?}` anywhere in a TrueNAS catalog answer or an Unraid template, and
saying otherwise made a fail-closed guarantee out of a property those two
platforms do not have. On TrueNAS the failure it hid was concrete: an upgrade
that kept a Phase 4 answer of `<pool>/backupd/config/config.yaml` bind
mounts that FILE at the container's configuration mount — `/etc/backupd/config`
as it was spelled then, `/etc/retnd/config` since issue #890 — `--config`
resolves to `config.yaml` inside it, and the engine crash-loops
on ENOTDIR with a message naming neither the mount nor the migration.

Two things close that. The TrueNAS question carries a new identifier, so there
is no answer to carry forward. And the engine now recognises the shape: when
the configuration path's parent is a file rather than a directory it says so,
names issue #196 and points here, instead of reporting "not a directory".
Unraid gets the same message, which is the only thing that can help there,
because retiring an operator's existing mapping is not something a template
can do.

## Migrating across the #890 rename

Issue #890 (R1.5, EPIC R #885) moves the deployment's own identity. It is the
second compat-breaking generation change this document records, so it gets the
same two tables as the configuration mount above: what moved, and what carries
an old answer into the new world.

Data does not move. `/data/state` and `/data/backups` carry no brand and are
untouched, so an existing installation keeps its journal, its retained
artifacts and its enrolled administrator across the change.

| | before #890 | after #890 |
|---|---|---|
| container config path | `/etc/backupd/config` | `/etc/retnd/config` |
| container key material | `/etc/backupd/id_ed25519`, `/etc/backupd/known_hosts` | `/etc/retnd/id_ed25519`, `/etc/retnd/known_hosts` |
| container runner token | `/etc/backupd/workflow-runner.token` | `/etc/retnd/workflow-runner.token` |
| image entrypoints | `/backupd`, `/backupd-web` | `/retnd`, `/retnd-web`, plus `/backupd-web` as a real hardlink for one release |
| image `HEALTHCHECK` | `/backupd status` | `/retnd status` |
| engine compose service | `backupd` | `retnd`; `web-ui` never named the product and does not move |
| compose project name | implicit, from the directory | explicit `name: retnd`, so the default containers are `retnd-retnd-1` and `retnd-web-ui-1` |
| systemd units | `backupd-bridge.service`, `backupd-bridge.timer`, `backupd-workflow-runner.service` | `retnd-bridge.service`, `retnd-bridge.timer`, `retnd-workflow-runner.service` |
| `/data/state`, `/data/backups` | unchanged | unchanged |
| image reference | `ghcr.io/backupdproject/backupd` | unchanged by this issue; #895 moves it, and pushes the old package path alongside the new one for one release because a GHCR package path is not covered by GitHub's transfer redirects |

| what carries the old answer | what stops it |
|---|---|
| an operator's pinned copy of a previously published `compose.yaml`, which this repository cannot edit | nothing has to: the image carries `/backupd-web` as a real hardlink to `/retnd-web` — one inode, two names, no second copy of the binary and no shell wrapper, because the runtime image is distroless — so an unedited pinned file still starts. Kept for exactly one release and removed by #895. There is deliberately no `/backupd` beside it, because no compose file this project has ever shipped named `/backupd` in a `command:` or a `healthcheck:` |
| a host directory bind-mounted at the old container path | FR-38's state-adoption preflight in `core/service`. For any resolved path with a path segment that is exactly `retnd` it also resolves the `backupd` counterpart, and ADOPTS the legacy location with a warning on every start rather than handing a first-run wizard to a deployment with years of journal in it. It refuses to start only on ambiguity — both populated, different device and inode — and names both paths when it does |
| a provider adapter's own `command:`, service names, file names and host paths | issue #891, which moves them as one cut. Until then the packaging gates accept both entrypoint spellings from one place, `distribution/packaging/renameoverlap.go`, rather than each gate deciding for itself; `distribution/packaging/canonical.json`'s `retainedBinaries` is the data behind it and #895 deletes both |
| the `binary_sha256` keys of an already-published release in `container/release-manifest.json` | nothing rewrites them. They record the SHA-256 of bytes that were built and pushed before the rename, which is evidence rather than a label, so every consumer accepts both spellings across the overlap release, new spelling first |
| a pre-#196 single-file configuration mount, under either spelling | `distribution/packaging`'s `legacy-config-file-mount` rule, which derives the pre-rename spellings of both file shapes rather than hardcoding them, and refuses the mount with the three write paths it breaks named |

## Digest policy

`x-canonical-runtime.digest_policy` names `container/release-manifest.json`,
which records the image digest and the binary SHA-256 per architecture. Deploy
by digest, not by tag: a tag can be moved, a digest cannot.

`TestArchitecturesAgreeAcrossTheThreePlacesTheyAreWrittenDown` holds the
architectures declared here, in `distribution/packaging/canonical.json` and in
the release manifest to one value, so "the same source revision produces
linux/amd64 and linux/arm64" is checkable rather than asserted.

## The engine plus web-ui deviation, and what it costs

`container/compose.yaml` runs two services from one image. The engine has no
published port; `web-ui` serves the static UI and reverse-proxies `/api/v1` and
`/health` to the engine over a private bridge network. It is the only service
with a LAN-facing port.

That is a project-owner requirement, adopted for network isolation, and it is
in tension with the refactor's "one production application process wherever
practical". The tension was resolved in favour of keeping it: the split is
already shipped, it is not a new proxy, it runs the same binary from the same
image with no second runtime, and it satisfies both absolute rules (one
canonical Go executable per architecture, no production Node server). The
performance contract prohibits **adding** a data-path hop, and "one process
wherever practical" is explicitly qualified.

What #167 owed was the measurement, because an already-shipped hop whose cost
nobody has measured is indistinguishable from one that is fine.

`apps/generic/tests/perfbaseline/proxycost_test.go` (`PERF_PROXY_COST=1`) runs
the same read the Phase 6 baseline times, twice against the same engine
process: once directly, once through a real `serve-ui` process proxying to it
exactly as compose wires them.

Measured on `darwin-arm64-mac17-2`, workload `phase6-baseline-v1`,
`GET /api/v1/backup-sets` (6,438-byte response, 400 timed samples after 40
warmups), five runs:

| | direct p50 | proxied p50 | direct p95 | proxied p95 | hop p50 | hop p95 |
|---|---|---|---|---|---|---|
| median of 5 | 0.062 ms | 0.087 ms | 0.082 ms | 0.128 ms | **0.025 ms** | **0.047 ms** |
| range | 0.061-0.063 | 0.085-0.088 | 0.080-0.082 | 0.128-0.135 | 0.023-0.026 | 0.046-0.055 |

So the hop costs about **25 microseconds at p50 and 47 microseconds at p95** on
this host, on a read that itself takes 62 to 82 microseconds. In relative terms
that is a real cost, roughly 40% of a very fast loopback read, and in
absolute terms it is well under a tenth of a millisecond on a request whose
end-to-end budget is dominated by the browser and the network in front of it.

Two things worth noting rather than burying:

- The number lines up with `docs/perf/gate.json`'s own reasoning. #165 set the
  API-read noise floor at 0.05 ms and justified it as "below the cost of the
  cheapest structural regression this gate exists to catch, an added loopback
  proxy hop." The measured hop is 0.047 ms, which is just under that floor. The
  floor was set from measurement and it turns out to be tight against the
  thing it was sized for, which is worth knowing before anyone adds a second
  hop.
- The harness has one assertion, and it is there because the most misleading
  result it could produce is a hop cost of roughly zero from a misconfigured
  upstream answering out of the UI host's own static handler. It fails unless
  both paths return the same number of bytes.

**The line this holds.** No further hop, no sidecar, no second runtime. A
second proxy would double a cost that is already about half the read.

## Performance evidence for this change

All seven metrics EPIC B #81's performance contract names, measured with
`python3 scripts/bdtools/perf/capture_baseline.py --repeat 5` on the designated benchmark host
`darwin-arm64-mac17-2`, workload `phase6-baseline-v1`, and compared against
#165's committed baseline with `scripts/perf/check-baseline.sh --compare`.
Nothing was re-baselined.

| metric | gated | baseline | this change | threshold | result |
|---|---|---|---|---|---|
| `api_read_p95_ms` | yes | 0.130 ms | 0.149 ms | ratio <= 1.10 **and** more than 0.05 ms above baseline | **pass**, delta 0.019 ms |
| `transfer_mb_per_second` | yes | 537.7 MB/s | 673.0 MB/s | >= 483.9 MB/s | **pass**, 1.25x |
| `idle_rss_bytes` | yes | 98,861,056 | 99,221,504 | <= 1.10x | **pass**, 1.004x |
| `config_write_p95_ms` | yes | 11.357 ms | 11.448 ms | <= 1.10x | **pass**, 1.008x |
| `image_size_bytes` | yes | 43,008,762 | 43,074,298 | <= 1.05x | **pass**, 1.0015x (+64 KiB) |
| `startup_to_healthy_ms` | no | 19.652 ms | 18.403 ms | recorded, not gated | 0.94x |
| `idle_cpu_seconds_total` | no | 0.12 s | 0.11 s | recorded, not gated | below the measurement floor on both sides |

Two of those deserve more than a tick.

**`api_read_p95_ms` moved 14.6% in ratio terms, and passed on the noise floor
rather than on the ratio.** That is the gate working as #165 designed it, not a
gate being lenient: the absolute movement is 0.019 ms, and the within-run
capture-to-capture spread of that same median was 62% of the median in this
very run. At 0.13 ms a 10% budget is smaller than the measurement's own
scatter, which is exactly why `gate.json` requires both conditions. Worth
saying out loud because the number will look like a near-miss to anyone reading
the ratio alone: it is not a near-miss, it is a metric whose ratio is not
meaningful at this magnitude, and the floor is what makes the gate real.

Note also that this is measured against the engine **directly**, so it does not
contain the reverse-proxy hop measured above. The hop is 0.047 ms, more than
twice this delta, which is a useful sanity check that the movement here is not
the two-service topology leaking into the direct path.

**`image_size_bytes` grew by 65,536 bytes.** That is the profile table, the
gateway authenticator and the bundle resolver compiled into
`/retnd-web`. It is 0.15% of the image against a 5% budget, and it is
real growth rather than noise: #165 recorded that two independent builds of the
same commit produced byte-identical image sizes, so there is no noise here to
hide in and no reason to describe 64 KiB as anything but 64 KiB.

`transfer_mb_per_second` improved by 25% and `startup_to_healthy_ms` by 6%.
Neither is attributable to this change: nothing here touches the transport or
the startup sequence, and both metrics have wide recorded spreads (27% and 15%
within this run). They are recorded because the contract names them, not
claimed as an improvement.

## Migration from the previous definition

Nothing was renamed and nothing was removed, so an existing deployment keeps
working with no change. What is new is additive:

| addition | effect on an existing deployment |
|---|---|
| `--profile=${RUNTIME_PROFILE:-generic}` on both commands | none; `generic` is what the previous build did |
| `TZ: ${TZ:-UTC}` | none; UTC is what the image defaulted to |
| `stop_grace_period` | the engine now gets 30s instead of Docker's 10s default, so a shutdown during a journal write is less likely to be killed mid-write |
| explicit `healthcheck` on the engine | the engine's compose healthcheck is now `/health/live` rather than the image's own freshness verdict, so a DEGRADED or unconfigured instance no longer keeps `web-ui` from starting. Backup freshness stays the image's own HEALTHCHECK, the alerts block, and `docker compose exec retnd /retnd status` (that command was `docker compose exec backupd /backupd status` before issue #890 moved the service name and the entrypoints) |
| `UI_DIR` / `UI_ROOT` on `web-ui` | none when unset, which is the default |
| `x-canonical-runtime` | none at runtime; compose ignores unknown `x-` keys |

No mount, environment variable or host path changed, so there is no migration
path to test and nothing to roll back.

The configuration mount is the exception, and it has its own migration table
under "One mount is redeclared" above, including what enforces the change on
each platform and what does not.

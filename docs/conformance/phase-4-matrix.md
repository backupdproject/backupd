# Phase 4 cross-provider conformance matrix

Generated. Do not edit the region between the markers by hand: it is the output
of a real run of `distribution/packaging`'s conformance suite, and the suite fails
if what is checked in differs from what a fresh run produces. To regenerate:

```bash
cd distribution && CONFORMANCE_UPDATE=1 GOWORK=off go test ./packaging/ -count=1 -run TestCrossProviderConformanceMatrix
```

`-count=1` is not decoration. The checks read files all over the tree and the test
cache keys on this module's own inputs, so a run after editing a provider can be
served from the cache and quietly regenerate nothing.

This is the record section 68's INTEGRATION step asks for, and the answer to
issue #86's fourth acceptance criterion, "unsupported capabilities are explicitly
reported". Section 63A is the reason it exists in this shape:

> The conformance suite SHALL distinguish SUPPORTED / UNSUPPORTED /
> NOT_APPLICABLE rather than silently skipping missing provider features.

## How to read an outcome

| Outcome | Means |
| --- | --- |
| `PASS` | The check ran here, on this repository, and held. |
| `FAIL` | The check ran here and did not hold. Any `FAIL` is a red build. |
| `UNSUP` | The provider genuinely does not have this capability at its section 4A support tier. Declared, with a reason, never inferred from absence. |
| `N/A` | The capability does not apply to this provider's shape, usually because the platform expresses the same guarantee somewhere else. The `verifiedBy` field says where. |
| `BLOCKED` | The check is implemented and correct and cannot conclude today, for a reason tracked in an issue. Not a pass and not a fail. |
| `OPERATOR` | Supported, and decidable only on the real platform (section 68). The automated half held: the prewritten acceptance procedure exists and covers this capability. The hardware run has not happened. |

`UNSUP`, `N/A` and `BLOCKED` are declarations in
`distribution/packaging/conformance.json`, and the suite checks them rather than
trusting them: every one of them still has its check run, and a declaration the
repository has outgrown fails the build. A provider cannot quietly drop a
capability by omitting it either, because omission is itself a failure.

## Whose gate a column counts towards

Every column also declares the EPIC whose gate consumes it. Six of them are EPIC
B's, and the Phase 4 Exit Gate is computed over those six and over nothing else:
Generic Docker, TrueNAS, Unraid, OpenMediaVault, Synology DSM and Proxmox VE, the
same six #86 and #81 name.

UGOS is EPIC D's. Its packaging is #83 (D1.2) since the UGOS split, so it cannot
be part of an EPIC B gate: an EPIC B phase that waits on a package built on
hardware nobody in this repository owns is a phase that cannot close. It is still
a column here, and still checked on exactly the same terms as every other one,
because the alternative was deleting it. A deleted column reports no blockers, it
reports nothing, and nothing reads as clean. The two-directional store-packaging
check is the concrete case: it is what caught UGOS claiming app-store packaging
with no UPK behind it, and it only works while there is a UGOS column for it to
read.

So drift in a UGOS declaration is still a red build, and it is still EPIC D's to
fix. What it is not is a Phase 4 result.

## What is blocking today

- **#174 is fixed.** `container/release-manifest.json` used to pin `c51a07f`, a
  feature-branch commit a squash merge had rewritten out of `main`, so its hashes
  described a build nobody could reproduce and `release-manifest-integrity` was
  `BLOCKED` in all seven columns. The manifest was regenerated from a real
  two-architecture build of `container/Dockerfile` at `8ad3100`, which is on
  `main`, so that row is now `PASS` everywhere. Two things keep it that way:
  `scripts/release/record-release-hashes.sh` refuses to record a commit that is
  not already an ancestor of `origin/main`, and
  `TestReleaseManifestPinsACommitThisHistoryCanReach` asks the ancestry question
  outside the declaration machinery, so the row cannot be re-declared `BLOCKED`
  back into silence. The separate per-provider row, `core-binary-hash-parity`,
  asks a different question: do the binaries THIS provider ships hash to what the
  manifest recorded? No provider checks a binary into this repository, so that row
  is `N/A` everywhere except Synology, whose `.spk` really is hashed against the
  manifest by `TestVerify_BinaryHashParity` in `apps/synology`'s own module, and
  UGOS, which is meant to ship an artifact and does not yet. That row still
  needs the comparison it names to actually happen.
- **#180** — `ui/shared/vite.config.ts` picks the frontend shell at build time
  from `VITE_PLATFORM`, defaulting to `generic`, and `serve-ui` serves one
  `go:embed`ed bundle. Nothing in the release build selects a provider, so every
  artifact anyone installs, the canonical image and the `.spk` alike, runs the
  generic bridge. A capability flag in `apps/<provider>/frontend/platform.ts` is
  therefore a statement of repository intent, not of deployed behaviour, and
  every cell resolved from one is `BLOCKED` on this rather than `PASS`.
- **#83** — work package 4.2's UGOS UPK moved out of this EPIC into EPIC D and is
  still open. `apps/ugos/` holds the frontend bridge and nothing else: no
  `project.yaml`, no Compose, no icon, no architecture image tar, so its packaging
  cells are `BLOCKED` rather than passing. Those blockers are EPIC D's, and the
  Phase 4 Exit Gate below is not computed over them.

What holds the Phase 4 Exit Gate open, then, is `#180`, and it is EPIC B's own
work. No cell of the six providers EPIC B claims fails: the suite
reddens the build if one does, so a `FAIL` in the table below cannot survive
long enough to be read here. The generated **Phase 4 Exit Gate** section states
the verdict over those six, and lists every cell holding it open with the issue
tracking it.

## Nothing here is a certification

Every `OPERATOR` row is a provider that is **build-supported and uncertified**
in section 68's own words. A green matrix proves the packaging metadata is
well-formed and mutually consistent. It proves nothing about how any of these
platforms behaves. `docs/acceptance/` is where that gets decided.

## What the review of this region found (#881)

The region below was left stale on purpose twice — at #871 and at #872 — because
a blind `CONFORMANCE_UPDATE=1` would have written a run's `FAIL` cells into a
checked-in report as though somebody had verified them, and
`docs/epic-checklist.md`'s rule is that nothing is green because nobody looked.
#881 is the review that owed. It is recorded here rather than in a commit
message because the next person to distrust this region will read this file.

Three of the four stale findings the issue named had already been fixed
underneath it by the time it was reviewed, and a run confirms it: the
`./workflows` mount now has a canonical storage role (#871 — `HostPlaneRoles`,
known and optional), and the two acceptance procedures capture the upgrade and
removal baselines they were missing (#872). Nothing was regenerated to make
those go green; they were already green and the report had not caught up.

The fourth was not stale, and it was not a product fault either. Six columns
failed `auth-mode-explicit` with "the bridge does not report auth mode
`local-account`", and the auth mode had not moved: issue #795 collapsed six
byte-identical copies of the session read into one shared module
(`ui/shared/src/platform/localSession.ts`), which took the literal out of every
bridge file while the check kept grepping the bridge for it. So the check was
fixed to follow the delegation rather than the string, and the delegated answer
is read rather than assumed — a shared reader that grew a second mode fails the
check, which is what
`TestTheAuthModeCheckFollowsTheSharedReaderRatherThanTrustingIt` watches. Six
`FAIL` cells would have been a claim about this product that is false, and
recording them would have been the same failure as rubber-stamping a `PASS`.

One finding sat outside this region and is fixed in the same pass, for the same
reason: the §170 equivalence gate demanded the workflow runner's three mounts of
CasaOS, Portainer and ZimaOS, which is the rule #871 had already decided against
for every other adapter. Host-plane mounts are optional to carry and not
optional to carry correctly, and
`TestAHostPlaneMountIsOptionalToCarryAndNotOptionalToCarryCorrectly` holds both
halves: an adapter that mounts nothing for the runner is equivalent, and one
that mounts `/workflows` writable is still reported.

Relaxing that comparison is also where the review found the one genuine product
gap, and it was filed as **#921** rather than written into this region as a
`FAIL`. Host-plane mounts being optional is right for a provider that deploys no
runner; it is wrong for one that declares `localHooks: available`, and four
profiles did both at once — OpenMediaVault, Proxmox VE, Portainer CE and CasaOS
mounted no `/workflows`, no `/data/run` and no runner token, so an operator who
followed their documents and provisioned the runner still had an engine with no
socket to dial. Three rules passed over that combination, each correctly on its
own terms, and none of them read the declaration and the mounts together. It was
not this report's `FAIL` to record: the `local-workflow-hooks` row decides
whether three declarations agree, and they did.

**#921 is now fixed**, which is why no cell here moved: all four profiles carry
the three mounts, ZimaOS still carries none because it is the one store adapter
that declares `localHooks: unavailable` and cannot host a runner at all, and the
combination has a rule of its own —
`CheckLocalHookMounts` and
`TestEveryProviderThatAdvertisesLocalHooksCanActuallyReachTheRunner`, which
redden on exactly those four profiles as they stood. A capability this report
records as `PASS` is now one an operator can reach, which is the claim the row
was always read as making.

What the regeneration itself then changed is small, which is the outcome an
honest review wants: the eight `Local workflow hooks` reason rows the #877
hand-splice could not write, and the UGOS cell count. The per-capability table
and every total came back byte-identical, so the row #877 spliced in by hand was
the row a real run produces. Its falsification still holds: flipping ZimaOS's
column to `available` fails the Go matrix and the frontend suite together, and a
document that omits any one of the three prerequisites (the unit, the group
grant, the hook image) fails
`TestTheLocalHookDocRequirementWouldNoticeASilentDocument`.

No cell in this region records a `FAIL`, and none is suppressed into one of the
softer outcomes either. The run that produced it decided every cell of every
column.

<!-- BEGIN GENERATED MATRIX -->

### Support tiers (§4A)

| Provider | Tier | Gated by | Work package | Acceptance procedure |
|---|---|---|---|---|
| UGOS Pro | A | EPIC D (reported here, gated there) | 4.2 | `docs/acceptance/ugos-local-notification.md` |
| CasaOS | B | EPIC B (Phase 6) | 6.6 | `docs/acceptance/casaos-app-store-install.md` |
| Portainer CE | B | EPIC B (Phase 6) | 6.6 | `docs/acceptance/portainer-stack-deployment.md` |
| Synology DSM | B | EPIC B (Phase 4) | 4.4 | `docs/acceptance/synology-dsm-package-lifecycle.md` |
| TrueNAS | B | EPIC B (Phase 4) | 4.3 | `docs/acceptance/truenas-provider-acceptance.md` |
| Unraid | B | EPIC B (Phase 4) | 4.3 | `docs/acceptance/unraid-provider-acceptance.md` |
| ZimaOS | B | EPIC B (Phase 6) | 6.6 | `docs/acceptance/zimaos-app-store-install.md` |
| Dockge | C | EPIC B (Phase 6) | 6.6 | `docs/acceptance/dockge-stack-import.md` |
| Generic Docker | C | EPIC B (Phase 4) | 4.1 | `none (automated instead)` |
| OpenMediaVault | C | EPIC B (Phase 4) | 4.3 | `docs/acceptance/openmediavault-provider-acceptance.md` |
| Proxmox VE | C | EPIC B (Phase 4) | 4.5 | `docs/acceptance/proxmox-ve-deployment.md` |

### Per-capability results

| Capability | UGOS Pro (EPIC D) | CasaOS (P6) | Portainer CE (P6) | Synology DSM | TrueNAS | Unraid | ZimaOS (P6) | Dockge (P6) | Generic Docker | OpenMediaVault | Proxmox VE |
|---|---|---|---|---|---|---|---|---|---|---|---|
| Provider identified correctly | PASS | N/A | N/A | PASS | PASS | PASS | N/A | N/A | PASS | PASS | PASS |
| Provider package metadata present | BLOCKED | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| Uses the exact canonical image | BLOCKED | PASS | PASS | N/A | PASS | PASS | PASS | N/A | N/A | PASS | PASS |
| Release manifest well-formed and reachable (repository-wide) | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| Core binary hash parity (this provider's own shipped bytes) | BLOCKED | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A | N/A |
| This provider's own architecture claim matches the build | BLOCKED | PASS | N/A | PASS | N/A | N/A | PASS | N/A | N/A | N/A | N/A |
| State path persists outside the container | BLOCKED | PASS | PASS | N/A | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| Backup root constrained | BLOCKED | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| Auth mode explicit and honest | PASS | N/A | N/A | PASS | PASS | PASS | N/A | N/A | PASS | PASS | PASS |
| No bundled secrets | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| No provider-specific lifecycle implementation | PASS | PASS | PASS | N/A | PASS | PASS | PASS | PASS | N/A | PASS | PASS |
| API reachable only through the intended path | BLOCKED | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| Provider removal does not alter core | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | N/A | PASS | PASS |
| Host management plane not modified | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS | PASS |
| Install / update / remove semantics | BLOCKED | OPERATOR | OPERATOR | OPERATOR | OPERATOR | OPERATOR | OPERATOR | OPERATOR | N/A | OPERATOR | OPERATOR |
| UI launches | BLOCKED | OPERATOR | OPERATOR | OPERATOR | OPERATOR | OPERATOR | OPERATOR | OPERATOR | N/A | OPERATOR | OPERATOR |
| Upgrade preserves state | BLOCKED | OPERATOR | OPERATOR | OPERATOR | OPERATOR | OPERATOR | OPERATOR | OPERATOR | N/A | OPERATOR | OPERATOR |
| Removal does not delete retained backups | BLOCKED | OPERATOR | OPERATOR | OPERATOR | OPERATOR | OPERATOR | OPERATOR | OPERATOR | N/A | OPERATOR | OPERATOR |
| Native authentication | BLOCKED | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP |
| Native notifications | BLOCKED | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP |
| Embedded window | BLOCKED | UNSUP | UNSUP | PASS | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP |
| App-store packaging | BLOCKED | N/A | N/A | PASS | PASS | PASS | N/A | UNSUP | UNSUP | UNSUP | UNSUP |
| Storage picker | BLOCKED | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP | UNSUP |
| Local workflow hooks (Host Workflow Runner + container hooks) | UNSUP | N/A | N/A | UNSUP | UNSUP | UNSUP | N/A | N/A | PASS | PASS | PASS |

### Totals

| Outcome | Cells |
|---|---|
| PASS | 122 |
| PENDING_OPERATOR | 36 |
| UNSUPPORTED | 47 |
| NOT_APPLICABLE | 43 |
| BLOCKED | 16 |
| FAIL | 0 |

### Phase 6 release qualification (issue #170)

Computed over every one of the 10 targets this refactor claims: CasaOS, Portainer CE, Synology DSM, TrueNAS, Unraid, ZimaOS, Dockge, Generic Docker, OpenMediaVault, Proxmox VE.

**Met.** Every cell of every one of those columns was decided, and none of them failed.

### Phase 4 Exit Gate

Computed over the 6 providers the §72 exit gate names, and over nothing else: Synology DSM, TrueNAS, Unraid, Generic Docker, OpenMediaVault, Proxmox VE.
The 4 target(s) Phase 6 adds (CasaOS, Portainer CE, ZimaOS, Dockge) are checked and reported here on the same
terms and are not in this verdict: a finished gate whose target set moves under it
cannot be cited afterwards.

**Met.** Every cell of every one of those columns was decided, and none of them failed.

**UGOS Pro is EPIC D's column** (work package 4.2).
All 24 of its cells are decided by the same runner, on the same terms as every
other column, and reported in full below; 16 are blocked today, on #83.
None of them is in either verdict above. A capability EPIC D owns cannot hold
EPIC B's Phase 4 or its Phase 6 release qualification open, and an EPIC D
column that goes green cannot close either of them.

### Every cell that is not a plain PASS

Section 63A's requirement in full: an unsupported capability is reported, with a
reason, rather than skipped. Every row below is a cell this run did not pass, and
why.

#### UGOS Pro (Tier A, reported here, gated by EPIC D)

| Capability | Outcome | Why |
|---|---|---|
| Provider package metadata present | BLOCKED | #83 — Work package 4.2's UPK was moved out of this EPIC into EPIC D and is still open as #83. apps/ugos/ contains the frontend bridge and nothing else: no project.yaml, no compose, no icon, no image tar. Until #83 lands, UGOS is the one Phase 4 Exit Gate provider with no package in this repository. |
| Uses the exact canonical image | BLOCKED | #83 — Nothing in apps/ugos/ references an image yet, so there is no reference to compare. |
| Core binary hash parity (this provider's own shipped bytes) | BLOCKED | #83 — The UPK is what would carry the architecture image tars (section 41), and there is no UPK, so there is no shipped byte to hash. Unlike the six providers that consume the OCI image by reference, this one is not not-applicable: UGOS is meant to ship its own artifact, so the cell stays blocked until #83 produces one. |
| This provider's own architecture claim matches the build | BLOCKED | #83 — The UPK declares the architecture image tars (section 41); no UPK, no claim of its own to check. |
| State path persists outside the container | BLOCKED | #83 — The UPK's compose declares the storage mapping (section 22); it does not exist yet. |
| Backup root constrained | BLOCKED | #83 — BACKUP_ROOT comes from the UPK's install parameters (section 20); they do not exist yet. |
| API reachable only through the intended path | BLOCKED | #83 — The UPK's compose decides which container publishes a port; it does not exist yet. |
| Install / update / remove semantics | BLOCKED | #83 — docs/acceptance/ugos-local-notification.md covers notifications only. The install/update/disable/uninstall/reinstall procedure is work package 4.2's, and belongs with #83. |
| UI launches | BLOCKED | #83 — The UI an operator would launch is the UPK's, and docs/acceptance/ugos-local-notification.md covers notifications only. Section 12's embedded provider window is delivered by the UPK too, which is why embedded-window below is blocked on the same issue rather than declared supported: both cannot be true at once. |
| Upgrade preserves state | BLOCKED | #83 — Section 46's upgrade behaviour needs the package that gets upgraded. |
| Removal does not delete retained backups | BLOCKED | #83 — Section 48's uninstall behaviour needs the package that gets uninstalled. |
| Native authentication | BLOCKED | #83 — The bridge opts in, but nothing this repository produces loads the UGOS bridge: there is no UPK (#83), and even once there is one, serve-ui embeds a single bundle chosen at build time (#180). A capability flag is a statement of intent until an installed artifact runs it. |
| Native notifications | BLOCKED | #83 — Same as native-auth: the flag is set in apps/ugos/frontend/platform.ts and no artifact loads that file. |
| Embedded window | BLOCKED | #83 — Section 12's embedded provider window is delivered by the UPK, which is exactly what ui-launch above says. Declaring this supported while blocking ui-launch on the same sentence was a contradiction: both rows are the same missing package. |
| App-store packaging | BLOCKED | #83 — The bridge claims it, and section 4A promises it, but no UPK exists in this repository yet. Passing on the bridge flag alone would be exactly the kind of claim the store-artifact half of this check exists to refuse. |
| Storage picker | BLOCKED | #83 — Same as native-auth: declared in a bridge no shipped artifact loads. |
| Local workflow hooks (Host Workflow Runner + container hooks) | UNSUP | UGOS Pro is a closed appliance: it offers no supported way to install a host unit or to add an account to the Docker socket's group. A workflow step with a remote target still runs: it goes over SSH (docs/adr/0021-remote-ssh-exec.md) and needs no Docker on the NAS. |

#### CasaOS (Tier B, gated by EPIC B's Phase 6)

| Capability | Outcome | Why |
|---|---|---|
| Provider identified correctly | N/A | This adapter ships no frontend bridge, which canonical.json records as uiBridge "none" with the reason. It is a container manager or an app store whose whole integration surface is a template or a compose file: it has no payload to carry a UI bundle in, the canonical image has 347,956 bytes of headroom against a bundle that costs roughly 352 KB, and a bridge would put this platform's id into the /api/v1 contract, the capability table, the profile table and the bundle list, which is core and shared-UI code. Identity is instead the runtime profile the stack selects, and it is generic because that is what this deployment honestly is. |
| Core binary hash parity (this provider's own shipped bytes) | N/A | This adapter consumes the canonical OCI image by reference and checks in no core binary of its own, so there is no second copy of the bytes here to hash against the release manifest. |
| Auth mode explicit and honest | N/A | The check reads the auth mode out of a frontend bridge and this adapter ships none (see provider-identity). The claim underneath it still holds and is still decided: the runtime is the canonical one, which uses section 13A local auth, and the adapter wires no authentication of its own. That half is the security-relevant half and it is checked by name rather than skipped. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/casaos-app-store-install.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/casaos-app-store-install.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/casaos-app-store-install.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/casaos-app-store-install.md, not yet executed |
| Native authentication | UNSUP | CasaOS has a user account of its own and this adapter does not borrow it: sign-in is the product's own local account over its own session cookie. A provider-native identity bridge is explicitly out of scope for issue #170. |
| Native notifications | UNSUP | CasaOS exposes no notification API a third-party compose app can post to. Webhooks instead. |
| Embedded window | UNSUP | The app tile opens the published port in a browser tab; CasaOS hosts no third-party application window. |
| App-store packaging | N/A | The store artifact exists and is real: the compose file carries the x-casaos block CasaOS builds the app tile and the install dialog from. What this capability actually decides is whether the SHIPPED BUNDLE tells the user they installed from a store, and it cannot, because this adapter ships no bridge (see provider-identity). The store metadata itself is checked by name, in both directions against the services beside it. |
| Storage picker | UNSUP | No host volume API a compose app can browse. The store install dialog shows the five fixed mounts and nothing selects among them. |
| Local workflow hooks (Host Workflow Runner + container hooks) | N/A | This provider ships no frontend bridge and no runtime profile of its own: it selects the generic profile (canonical.json's uiBridge "none"), so the running process reports generic's capabilities and cannot tell this host apart from any other Docker host. There is therefore no per-provider runtime answer for the contract to hold it to. CasaOS is an application layer installed onto an ordinary Linux distribution, which keeps its own systemd and its own docker group, so that host's administrator can run the runner installer beside the store install, and the acceptance procedure says so by name. |

#### Portainer CE (Tier B, gated by EPIC B's Phase 6)

| Capability | Outcome | Why |
|---|---|---|
| Provider identified correctly | N/A | This adapter ships no frontend bridge, which canonical.json records as uiBridge "none" with the reason. It is a container manager or an app store whose whole integration surface is a template or a compose file: it has no payload to carry a UI bundle in, the canonical image has 347,956 bytes of headroom against a bundle that costs roughly 352 KB, and a bridge would put this platform's id into the /api/v1 contract, the capability table, the profile table and the bundle list, which is core and shared-UI code. Identity is instead the runtime profile the stack selects, and it is generic because that is what this deployment honestly is. |
| Core binary hash parity (this provider's own shipped bytes) | N/A | This adapter consumes the canonical OCI image by reference and checks in no core binary of its own, so there is no second copy of the bytes here to hash against the release manifest. |
| This provider's own architecture claim matches the build | N/A | This adapter makes no architecture claim of its own: it names one multi-arch canonical image by reference and lets the runtime pick. The repository-wide claim is release-manifest-integrity's. |
| Auth mode explicit and honest | N/A | The check reads the auth mode out of a frontend bridge and this adapter ships none (see provider-identity). The claim underneath it still holds and is still decided: the runtime is the canonical one, which uses section 13A local auth, and the adapter wires no authentication of its own. That half is the security-relevant half and it is checked by name rather than skipped. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/portainer-stack-deployment.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/portainer-stack-deployment.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/portainer-stack-deployment.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/portainer-stack-deployment.md, not yet executed |
| Native authentication | UNSUP | Portainer has its own users, and this adapter does not borrow them: sign-in is the product's own local account. A provider-native identity bridge is explicitly out of scope for issue #170, and one that trusted a header with no authenticated gateway in front of it would be worse than none. |
| Native notifications | UNSUP | Portainer has no notification surface a third-party stack can post to. Webhooks instead. |
| Embedded window | UNSUP | Portainer's stack view links out to the published port; it hosts no third-party application window. |
| App-store packaging | N/A | The store artifact exists and is real: templates.json is a Portainer App Template and an operator does install this from Portainer's own catalogue. What this capability actually decides is whether the SHIPPED BUNDLE tells the user so, and it cannot, because this adapter ships no bridge (see provider-identity). Declaring it supported would be a claim about the UI that the UI does not make. The template itself is checked by name. |
| Storage picker | UNSUP | No host volume API a stack can browse. Paths are typed into the App Template's form. |
| Local workflow hooks (Host Workflow Runner + container hooks) | N/A | This provider ships no frontend bridge and no runtime profile of its own: it selects the generic profile (canonical.json's uiBridge "none"), so the running process reports generic's capabilities and cannot tell this host apart from any other Docker host. There is therefore no per-provider runtime answer for the contract to hold it to. The answer an operator needs is still decided and still written down: Portainer runs on an ordinary Docker host, so that host's administrator can run the runner installer beside the stack, and the acceptance procedure says so by name. |

#### Synology DSM (Tier B, gated by EPIC B's Phase 4)

| Capability | Outcome | Why |
|---|---|---|
| Uses the exact canonical image | N/A | Synology is the one Phase 4 provider that cannot consume the OCI image: DSM's Package Center installs a native .spk. Section 3.7 makes the SPK a sibling of the image carrying the same core binary digest, so parity here is binary parity, not image parity. |
| Core binary hash parity (this provider's own shipped bytes) | N/A | The .spk is not in this repository; cmd/spkctl builds it. The byte comparison this row demands is real and it does run: spkctl verify re-derives each binary's SHA-256 out of a finished package and compares it against container/release-manifest.json, and TestVerify_BinaryHashParity is the test that proves it, including the negative case. distribution/packaging cannot execute it without importing across the apps/synology module boundary that scripts/architecture/*.sh enforces, so this cell records where the comparison happens instead of pretending to do it here. |
| State path persists outside the container | N/A | DSM fixes the persistent location: /var/packages/<pkg>/var under the package FHS, not a bind mount this repository declares. |
| No provider-specific lifecycle implementation | N/A | DSM's package format MANDATES preinst/postinst/preuninst/postuninst/preupgrade/postupgrade and start-stop-status. Those scripts are the platform's contract, not a lifecycle engine of our own, and apps/synology holds them to wrapper-only behaviour. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/synology-dsm-package-lifecycle.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/synology-dsm-package-lifecycle.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/synology-dsm-package-lifecycle.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/synology-dsm-package-lifecycle.md, not yet executed |
| Native authentication | UNSUP | Tier B. Section 4A makes DSM SSO a follow-on capability; the initial package uses section 13A local auth. |
| Native notifications | UNSUP | Tier B. No DSM notification adapter in v1; webhooks instead. |
| Storage picker | UNSUP | Tier B. The shared folder is chosen once at install time through DSM, not browsed from inside the app. |
| Local workflow hooks (Host Workflow Runner + container hooks) | UNSUP | A DSM package cannot install a systemd unit or grant a supplementary group, and Container Manager's socket is root-owned; either step would be the host-management-plane modification §4A/§75 forbids. That holds for both ways of installing on DSM, the .spk and the Container Manager project. A workflow step with a remote target still runs: it goes over SSH (docs/adr/0021-remote-ssh-exec.md) and needs no Docker on the NAS. |

#### TrueNAS (Tier B, gated by EPIC B's Phase 4)

| Capability | Outcome | Why |
|---|---|---|
| Core binary hash parity (this provider's own shipped bytes) | N/A | This provider consumes the canonical OCI image by reference and checks in no core binary of its own, so there is no second copy of the bytes here to hash against the release manifest. Parity for a provider of this shape is the image reference it names, which canonical-image-parity decides. A cell here can only go green once a provider declares a binaryArtifacts entry and the file behind it hashes to what the manifest recorded. |
| This provider's own architecture claim matches the build | N/A | This provider makes no architecture claim of its own: it names one multi-arch canonical image and lets the runtime pick. The claim that the built architecture set matches canonical.json is repository-wide and is release-manifest-integrity's row, not seven copies of itself. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/truenas-provider-acceptance.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/truenas-provider-acceptance.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/truenas-provider-acceptance.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/truenas-provider-acceptance.md, not yet executed |
| Native authentication | UNSUP | Tier B. Section 4A gives TrueNAS the generic local auth; no middleware session adapter in v1. |
| Native notifications | UNSUP | Tier B. Webhooks instead of TrueNAS alerts. |
| Embedded window | UNSUP | Tier B. The Apps portal link opens the UI in a normal browser tab. |
| Storage picker | UNSUP | Tier B. questions.yaml asks for the dataset paths at install time; the running app does not browse pools. |
| Local workflow hooks (Host Workflow Runner + container hooks) | UNSUP | TrueNAS's host is vendor-managed: applications run under the middleware's own container runtime, a third-party systemd unit is unsupported and does not survive an upgrade, and there is no supported way to add an account to the Docker socket's group. Provisioning the Host Workflow Runner anyway would be the host-management-plane modification §4A/§75 forbids this product, and host-management-plane-untouched is a capability this same column already claims. So local hooks are refused out loud instead: the capability contract answers unavailable for this platform, and the engine refuses a NAME.local.sh step that has no host workflow runner behind it (core/internal/workflowrun/engine.go) rather than skipping it. A workflow step with a remote target still runs: it goes over SSH (docs/adr/0021-remote-ssh-exec.md) and needs no Docker on the NAS. |

#### Unraid (Tier B, gated by EPIC B's Phase 4)

| Capability | Outcome | Why |
|---|---|---|
| Core binary hash parity (this provider's own shipped bytes) | N/A | This provider consumes the canonical OCI image by reference and checks in no core binary of its own, so there is no second copy of the bytes here to hash against the release manifest. Parity for a provider of this shape is the image reference it names, which canonical-image-parity decides. A cell here can only go green once a provider declares a binaryArtifacts entry and the file behind it hashes to what the manifest recorded. |
| This provider's own architecture claim matches the build | N/A | This provider makes no architecture claim of its own: it names one multi-arch canonical image and lets the runtime pick. The claim that the built architecture set matches canonical.json is repository-wide and is release-manifest-integrity's row, not seven copies of itself. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/unraid-provider-acceptance.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/unraid-provider-acceptance.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/unraid-provider-acceptance.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/unraid-provider-acceptance.md, not yet executed |
| Native authentication | UNSUP | Tier B. Section 4A gives Unraid the generic local auth; no plugin is required for v1. |
| Native notifications | UNSUP | Tier B. Webhooks instead of Unraid notifications, which would need a plugin. |
| Embedded window | UNSUP | Tier B. The WebUI link opens a normal browser tab. |
| Storage picker | UNSUP | Tier B. Community Applications collects the paths at install time; the app does not browse shares. |
| Local workflow hooks (Host Workflow Runner + container hooks) | UNSUP | Unraid rebuilds its operating system from the flash device on every boot, so there is no persistent host unit for the runner to be, and its Docker runs everything as root while the runner refuses to run as root at all. Two independent reasons, either one sufficient. A workflow step with a remote target still runs: it goes over SSH (docs/adr/0021-remote-ssh-exec.md) and needs no Docker on the NAS. |

#### ZimaOS (Tier B, gated by EPIC B's Phase 6)

| Capability | Outcome | Why |
|---|---|---|
| Provider identified correctly | N/A | This adapter ships no frontend bridge, which canonical.json records as uiBridge "none" with the reason. It is a container manager or an app store whose whole integration surface is a template or a compose file: it has no payload to carry a UI bundle in, the canonical image has 347,956 bytes of headroom against a bundle that costs roughly 352 KB, and a bridge would put this platform's id into the /api/v1 contract, the capability table, the profile table and the bundle list, which is core and shared-UI code. Identity is instead the runtime profile the stack selects, and it is generic because that is what this deployment honestly is. |
| Core binary hash parity (this provider's own shipped bytes) | N/A | This adapter consumes the canonical OCI image by reference and checks in no core binary of its own, so there is no second copy of the bytes here to hash against the release manifest. |
| Auth mode explicit and honest | N/A | The check reads the auth mode out of a frontend bridge and this adapter ships none (see provider-identity). The claim underneath it still holds and is still decided: the runtime is the canonical one, which uses section 13A local auth, and the adapter wires no authentication of its own. That half is the security-relevant half and it is checked by name rather than skipped. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/zimaos-app-store-install.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/zimaos-app-store-install.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/zimaos-app-store-install.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/zimaos-app-store-install.md, not yet executed |
| Native authentication | UNSUP | ZimaOS has a user account of its own and this adapter does not borrow it: sign-in is the product's own local account over its own session cookie. A provider-native identity bridge is explicitly out of scope for issue #170. |
| Native notifications | UNSUP | ZimaOS exposes no notification API a third-party compose app can post to. Webhooks instead. |
| Embedded window | UNSUP | The app tile opens the published port in a browser tab; ZimaOS hosts no third-party application window. |
| App-store packaging | N/A | The store artifact exists and is real: the compose file carries the x-casaos block ZimaOS builds the app tile and the install dialog from. What this capability actually decides is whether the SHIPPED BUNDLE tells the user they installed from a store, and it cannot, because this adapter ships no bridge (see provider-identity). The store metadata itself is checked by name, in both directions against the services beside it. |
| Storage picker | UNSUP | No host volume API a compose app can browse. The store install dialog shows the five fixed mounts and nothing selects among them. |
| Local workflow hooks (Host Workflow Runner + container hooks) | N/A | This provider ships no frontend bridge and no runtime profile of its own: it selects the generic profile (canonical.json's uiBridge "none"), so the running process reports generic's capabilities and cannot tell this host apart from any other Docker host. There is therefore no per-provider runtime answer for the contract to hold it to. ZimaOS is where that matters most: it ships as a complete appliance OS rather than as a layer over a distribution the operator administers, so unlike CasaOS there is no host session in which to install a unit or grant a group, and the running process cannot tell the difference. Its acceptance procedure is the only place that answer can be given, and it gives it: local hooks are unavailable here. A workflow step with a remote target still runs: it goes over SSH (docs/adr/0021-remote-ssh-exec.md) and needs no Docker on the NAS. |

#### Dockge (Tier C, gated by EPIC B's Phase 6)

| Capability | Outcome | Why |
|---|---|---|
| Provider identified correctly | N/A | This adapter ships no frontend bridge, which canonical.json records as uiBridge "none" with the reason. It is a container manager or an app store whose whole integration surface is a template or a compose file: it has no payload to carry a UI bundle in, the canonical image has 347,956 bytes of headroom against a bundle that costs roughly 352 KB, and a bridge would put this platform's id into the /api/v1 contract, the capability table, the profile table and the bundle list, which is core and shared-UI code. Identity is instead the runtime profile the stack selects, and it is generic because that is what this deployment honestly is. |
| Uses the exact canonical image | N/A | Dockge deploys container/compose.yaml itself, and that file BUILDS the canonical image from container/Dockerfile rather than pulling a published reference: it is the source of the image, not a consumer of one. Identical to the generic column's answer, and for the identical reason, which is the point of this column reading the canonical stack rather than a copy. |
| Core binary hash parity (this provider's own shipped bytes) | N/A | This adapter consumes the canonical OCI image by reference and checks in no core binary of its own, so there is no second copy of the bytes here to hash against the release manifest. |
| This provider's own architecture claim matches the build | N/A | This adapter makes no architecture claim of its own: it names one multi-arch canonical image by reference and lets the runtime pick. The repository-wide claim is release-manifest-integrity's. |
| Auth mode explicit and honest | N/A | The check reads the auth mode out of a frontend bridge and this adapter ships none (see provider-identity). The claim underneath it still holds and is still decided: the runtime is the canonical one, which uses section 13A local auth, and the adapter wires no authentication of its own. That half is the security-relevant half and it is checked by name rather than skipped. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/dockge-stack-import.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/dockge-stack-import.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/dockge-stack-import.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/dockge-stack-import.md, not yet executed |
| Native authentication | UNSUP | Dockge is a compose manager with a single operator login of its own and no identity to federate. Section 13A local auth is the whole story. |
| Native notifications | UNSUP | Dockge has no notification surface. Webhooks instead. |
| Embedded window | UNSUP | Dockge links out to a stack's published port; it hosts no third-party application window. |
| App-store packaging | UNSUP | Dockge has no application store or catalogue at all. It manages compose stacks in a directory, which is why this adapter is supported by compatibility and ships no packaging: apps/dockge/ holds documentation and nothing else, and a runtime definition appearing there is a red test. |
| Storage picker | UNSUP | No host volume API. Paths are typed into the stack's .env. |
| Local workflow hooks (Host Workflow Runner + container hooks) | N/A | This provider ships no frontend bridge and no runtime profile of its own: it selects the generic profile (canonical.json's uiBridge "none"), so the running process reports generic's capabilities and cannot tell this host apart from any other Docker host. There is therefore no per-provider runtime answer for the contract to hold it to. Dockge imports the canonical stack onto an ordinary Docker host, so that host's administrator can run the runner installer beside it, and the acceptance procedure says so by name. |

#### Generic Docker (Tier C, gated by EPIC B's Phase 4)

| Capability | Outcome | Why |
|---|---|---|
| Uses the exact canonical image | N/A | container/compose.yaml BUILDS the canonical image from container/Dockerfile rather than pulling a published reference. It is the source of the image the other six profiles consume, so pinning it to its own output would be circular. |
| Core binary hash parity (this provider's own shipped bytes) | N/A | container/ BUILDS the canonical image from container/Dockerfile rather than consuming a published one, so its binaries are compiled during the build and nothing here is a checked-in artifact to hash. The bytes are decided by the build itself, and apps/generic/tests/dockercli drives the real image the build produces. |
| This provider's own architecture claim matches the build | N/A | This provider makes no architecture claim of its own: it names one multi-arch canonical image and lets the runtime pick. The claim that the built architecture set matches canonical.json is repository-wide and is release-manifest-integrity's row, not seven copies of itself. |
| No provider-specific lifecycle implementation | N/A | Two reasons, both structural. apps/generic IS the canonical web host (section 37), not a wrapper around it, so there is no wrapper for a second implementation to hide in. And container/compose.yaml carries a build: key, which the scanner treats as a violation everywhere else precisely because a Tier B/C package must reuse the canonical image rather than build one; here it is the file the canonical image is built FROM. |
| Provider removal does not alter core | N/A | scripts/architecture/verify-ui-shared-without-provider-sdks.sh says it directly: apps/generic is the vendor-neutral baseline the default Vite build targets, so it is not a provider SDK directory and ui/shared's own tests import its bridge on purpose. The rule applies to the six vendor providers. |
| Install / update / remove semantics | N/A | Automated rather than operator-verified: apps/generic/tests/dockercli drives the real docker CLI against the real image (section 67), so there is no hardware step to write a procedure for. |
| UI launches | N/A | Same as install-update-remove: covered by the Docker CLI suite and ui/shared's own tests, not by a hardware procedure. |
| Upgrade preserves state | N/A | No vendor updater to exercise; container replacement is what the Docker CLI suite already does. |
| Removal does not delete retained backups | N/A | No vendor uninstaller to exercise. The compose profile declares no named volume, so there is nothing for `down -v` to reach. |
| Native authentication | UNSUP | Tier C. No identity provider on a plain Docker host; section 13A local auth is the whole story. |
| Native notifications | UNSUP | Tier C. Webhook notifications instead. |
| Embedded window | UNSUP | Tier C. Opens in a standalone browser. |
| App-store packaging | UNSUP | Tier C. There is no store; this is the raw compose deployment. |
| Storage picker | UNSUP | Tier C. Manual path entry, because no host volume API exists to browse. |

#### OpenMediaVault (Tier C, gated by EPIC B's Phase 4)

| Capability | Outcome | Why |
|---|---|---|
| Core binary hash parity (this provider's own shipped bytes) | N/A | This provider consumes the canonical OCI image by reference and checks in no core binary of its own, so there is no second copy of the bytes here to hash against the release manifest. Parity for a provider of this shape is the image reference it names, which canonical-image-parity decides. A cell here can only go green once a provider declares a binaryArtifacts entry and the file behind it hashes to what the manifest recorded. |
| This provider's own architecture claim matches the build | N/A | This provider makes no architecture claim of its own: it names one multi-arch canonical image and lets the runtime pick. The claim that the built architecture set matches canonical.json is repository-wide and is release-manifest-integrity's row, not seven copies of itself. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/openmediavault-provider-acceptance.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/openmediavault-provider-acceptance.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/openmediavault-provider-acceptance.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/openmediavault-provider-acceptance.md, not yet executed |
| Native authentication | UNSUP | Tier C. A native Workbench plugin is deferred by section 4A, and auth would come with it. |
| Native notifications | UNSUP | Tier C. Webhooks; OMV notifications would need the deferred plugin. |
| Embedded window | UNSUP | Tier C. There is no Workbench navigation entry, by design; the UI is reached on its own port. |
| App-store packaging | UNSUP | Tier C. A Compose deployment profile, not an omv-extras package. Section 4A defers the Debian plugin. |
| Storage picker | UNSUP | Tier C. Paths are set once in the env file; the app does not browse OMV filesystems. |

#### Proxmox VE (Tier C, gated by EPIC B's Phase 4)

| Capability | Outcome | Why |
|---|---|---|
| Core binary hash parity (this provider's own shipped bytes) | N/A | This provider consumes the canonical OCI image by reference and checks in no core binary of its own, so there is no second copy of the bytes here to hash against the release manifest. Parity for a provider of this shape is the image reference it names, which canonical-image-parity decides. A cell here can only go green once a provider declares a binaryArtifacts entry and the file behind it hashes to what the manifest recorded. |
| This provider's own architecture claim matches the build | N/A | This provider makes no architecture claim of its own: it names one multi-arch canonical image and lets the runtime pick. The claim that the built architecture set matches canonical.json is repository-wide and is release-manifest-integrity's row, not seven copies of itself. |
| Install / update / remove semantics | OPERATOR | covered by docs/acceptance/proxmox-ve-deployment.md, not yet executed |
| UI launches | OPERATOR | covered by docs/acceptance/proxmox-ve-deployment.md, not yet executed |
| Upgrade preserves state | OPERATOR | covered by docs/acceptance/proxmox-ve-deployment.md, not yet executed |
| Removal does not delete retained backups | OPERATOR | covered by docs/acceptance/proxmox-ve-deployment.md, not yet executed |
| Native authentication | UNSUP | Tier C. No PVE realm, PAM hook or API token; section 13A local auth only. |
| Native notifications | UNSUP | Tier C. Webhooks. PVE notification targets would mean touching the host management plane. |
| Embedded window | UNSUP | Tier C. Section 4A defers the PVE Web UI plugin indefinitely, so there is no window to embed in. |
| App-store packaging | UNSUP | Tier C, and structurally so: Proxmox VE has no third-party application store to package into. That is why this provider is a deployment profile at all. |
| Storage picker | UNSUP | Tier C. One host directory is shared into the guest and the paths under it are fixed in the env file. |

<!-- END GENERATED MATRIX -->

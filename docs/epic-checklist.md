# What every EPIC has to update

A checklist, written because the expensive misses in this repository have never
been the code. They have been a surface that went out of step with the code: a
command with no row on the reference page, a route with no CLI equivalent, a
screenshot showing a screen that no longer exists, a config field the web UI
could set and the terminal could not.

Each item below says **who catches it**. That split is the point of the file:

- **Gated** means a check fails if I skip it. `scripts/ci-local.sh` or CI goes
  red and the epic cannot merge. I do not need to remember these, I need to not
  fight them.
- **Ungated** means nothing checks it and the only thing standing between the
  epic and a stale surface is this list. Every one of these has gone stale at
  least once already.

Read it twice per epic: once when writing the spec, so the work is sized with
these in it, and once at each phase exit gate.

---

## 1. The shape of the epic itself

- [ ] **Two phases, maximum. Five issues per phase, maximum.** *(Ungated.)* If it
  does not fit, cut scope and write a "What I cut to fit two phases, and why"
  section, the way `docs/EPIC-E-alternative-storage.md` does. Deferred work
  becomes the next epic, not a third phase.
- [ ] **An org-unique epic letter**, and sub-issues titled `<letter><phase>.<n>`
  (`E1.2`, `E2.4`). *(Ungated.)* A, B, D, E, K and L are taken.
- [ ] **The spec lives at `docs/EPIC-<letter>-<slug>.md`** with the Status block
  the existing three carry: type, repository, parent epics, primary
  implementation root, tracker issue with its sub-issue range, and an explicit
  FR-numbering sentence. *(Ungated.)* EPIC L's was written at the END of the
  epic, in #817, which is how long an ungated item can go missing while every
  gated one stays green.
- [ ] **FR numbers continue the global series and renumber nothing.** *(Ungated.)*
  The next free number is **FR-49**. FR-1 to FR-24 are in `docs/EPIC.md`, FR-26 is
  the `version` command, FR-27 to FR-35 are EPIC E, FR-36 to FR-48 are EPIC L.
  FR-25 is an unclaimed hole: leave it alone rather than filling it, because
  anything citing "FR-25" today is citing nothing.
- [ ] **A five-expert adversarial review section**, initial verdicts and consensus
  position, before implementation starts. *(Ungated.)*
- [ ] **Entry gate and exit gate per phase**, written as checkable claims rather
  than intentions. *(Ungated.)*
- [ ] **A conformance matrix at `docs/conformance/epic-<letter>-matrix.md`.**
  *(Ungated, and it is the one that matters most.)* One row per exit-gate line and
  per planted violation, each with a **falsification**: the mutation that must
  turn the cell red. `PASS` only once that mutation has been run and watched to
  fail. `BLOCKED` for anything whose code has not landed. No row is green because
  nobody looked.
- [ ] **Every PR closes an issue** with `Closes #N`. *(Ungated by CI, required by
  `CONTRIBUTING.md`.)*

## 2. Settings: global default plus per-set override

Every new knob gets both halves, and the global half has to have a default good
enough that nobody has to set it.

- [ ] **A global setting on `Config`** in `core/internal/config/config.go`, with a
  smart default that makes the field optional in practice. *(Ungated.)*
- [ ] **A per-backup-set override**, following the three-field pattern the file
  already uses: `XConfig *T` with `yaml:"...,omitempty"` where nil means inherit,
  a resolved `X T` with `yaml:"-"` that `Validate` fills in, and every consumer
  reading the resolved field, never the pointer. *(Ungated.)*
- [ ] **Re-resolve after any mutation of the global.** *(Ungated.)* Mutating the
  top-level value without a `Validate` pass leaves every set deciding under the
  old policy. The retention override flags were a silent no-op this exact way.
- [ ] **A compat fixture** in `core/tests/compat/testdata/configs/`, plus an
  invalid-input fixture for whatever the field refuses. *(Gated.)*
- [ ] **Say so if it is a one-way door.** *(Ungated.)* `Load` uses
  `KnownFields(true)`, so a config carrying a new key cannot be parsed at all by
  an older build. Anything an operator reaches for during an incident deserves
  that sentence in the CHANGELOG.

## 3. No functionality gap between the CLI and the web UI

- [ ] **Every new `/api/v1` route gets a `cliecho` entry** in
  `core/cliecho/routes.go`: either a builder that prints the equivalent command,
  or a `why` sentence saying what would have to be built. *(Gated:*
  `TestEveryAPIRouteNamesItsCLIEquivalentOrTheGap` in
  `apps/common/webhost/actionlog_test.go` fails on a route with neither.*)*
- [ ] **A gap sentence must not name a verb this binary ships.** *(Gated:*
  `TestNoGapClaimsAVerbThisBinaryShips` in `core/cmd/backupd/cliechogaps_test.go`.*)* Five of these once claimed a verb did
  not exist while the same tree shipped it.
- [ ] **Every new CLI verb reaches the dispatch table** in
  `core/cmd/backupd/main.go` **and gets a row in `docs/site/reference.html`.**
  *(Gated:* `distribution/packaging/site_reference_test.go` reads the dispatch
  table and the page and compares them.*)*
- [ ] **The refusals match on both routes**, same reason and same exit code.
  *(Gated:* `core/cmd/backupd/routeparity_test.go`.*)*
- [ ] **Check the gap in both directions.** *(Ungated.)* The tests above catch an
  API route with no command. Nothing catches a command with no way to do it in
  the browser, so that one is mine to check by hand.

## 4. Progress has to reach the terminal and the activity feed

Anything the new functionality does that takes time, or that an operator would
want to see happening, has to show up live.

- [ ] **An event constant and a typed method** in `core/internal/obs/events.go`,
  in the FR-23 catalog. *(Ungated.)* These strings are a wire contract: renaming
  one is a breaking change, not a cleanup.
- [ ] **Completions state an outcome**, as a `Result`, rather than leaving a
  client to infer it from a missing error field. *(Ungated.)*
- [ ] **The activity feed carries it**: `core/service/activity.go`,
  `liveactivity.go` and the handlers in `apps/common/webhost/`. *(Ungated.)*
- [ ] **The UI surfaces update**: `ActivityStrip.tsx` (the global terminal),
  `SetActivityPanel.tsx` (per set), `DashboardActivity.tsx` and
  `useActivityFeed.ts`. *(Ungated.)*
- [ ] **`CommandEcho.tsx` shows the command** the UI action is equivalent to.
  *(Ungated.)*
- [ ] **Paging still works.** *(Ungated.)* The feed is cursor-paged (`before` /
  `next_cursor`) and the record is append-only, so a new high-volume event is a
  performance decision, not just a display one.

## 5. Web UI design

- [ ] **Mock it up in Claude Designer first**, and keep it in line with what
  already ships. *(Ungated.)* `docs/design/Backup Manager.dc.html` is the
  interactive design the frontend was built from, both themes, every provider
  treatment, every risk state. `docs/deployment.md` cites it as the authority when
  the UI and the design disagree.
- [ ] **Land the mockup in `docs/design/`** as a per-issue note: the `.html` and a
  matching `.png`, the reasoning beside the picture, dated by its issue.
  *(Ungated.)* These are a record of a decision at the moment it was taken and are
  deliberately not kept in step with later work.
- [ ] **Global settings page and per-set card both get the new control**:
  `SettingsPage.tsx` and the per-set cards such as `BackupSetRetentionCard.tsx`.
  *(Ungated.)*
- [ ] **Frontend gate green**: typecheck, typecheck every provider, eslint,
  vitest, build. *(Gated.)*

### Tooltips

Every control the epic adds is a control somebody meets without having read
anything. 493 entries say this product takes that seriously, and the rules
around them are stricter than "write some help text".

- [ ] **Every new control, field and page heading that needs explaining gets a
  registry entry** in `ui/shared/src/tooltips/tooltips.json`, and the element
  names its `TooltipId` rather than carrying copy of its own. *(Ungated in the
  direction that matters, see below.)* Ids are namespaced by where they live
  (`shell.*`, `nav.*`), so a new page or panel gets its own prefix.
- [ ] **The copy stays in the registry.** *(Gated.)* What an operator reads is
  whatever the id says today, and the test asserts that through a really wired
  element rather than the component in isolation, because "is it wired" is the
  half that rots silently.
- [ ] **Only `<strong>`, `<em>`, `<code>` and `<br>`.** *(Gated.)* The copy is
  injected as HTML, so the tag list is a safety property rather than a style one.
- [ ] **No entry that nothing names.** *(Gated:* "has no entry that nothing in
  the app names".*)* Note the direction: an entry with no control fails, a
  control with no entry does not. That gap is mine to check, exactly like the
  CLI command with no browser equivalent in section 3.
- [ ] **An explained input also gets a `fieldHelpCopy.ts` entry, with all three
  parts**: what the field is for, an example of something an operator could
  type, and what typing that would actually cause. *(Gated as a compile error,*
  since it is a struct and a missing part will not build.*)* The third part is
  the whole point. "Remote path: the path on the remote host" restates the
  label and helps nobody.
- [ ] **State the effect from the code, not from the label.** *(Ungated.)* Where
  the code and the spec disagree, the code wins, because the code is what runs.
- [ ] **A field whose effect cannot be stated from the code gets no entry and no
  pop-up.** *(Ungated, and the one I would most expect an epic to get wrong.)*
  This is a decision, not a gap for somebody to fill in later. A help pop-up is
  the easiest place in the product to add an invented claim, because a plausible
  sentence about a decorative control reads exactly like a true one, and this UI
  has had to remove invented claims four times already. If a control cannot be
  explained honestly, the answer is to remove the control or make it an honest
  non-interactive statement of fact, not to write around it.
- [ ] **The opt-out preference still governs anything new**, including the
  sign-in screen's blanket suppression. *(Gated.)* Test it with the preference
  explicitly ON, so a passing case cannot be the preference doing the work.
- [ ] **The pop-up is announced with the control it wraps** and leaves that
  control's own accessible name exactly as it was. *(Gated.)*

## 6. The API contract

- [ ] **`api/v1/openapi.json` is authoritative**, and both generated bindings are
  regenerated from it: `core/apicontract/contract.gen.go` and
  `ui/shared/src/api/generated/contract.ts`. *(Gated:*
  `scripts/api/check-contract-drift.sh`.*)*
- [ ] **The shared client only builds declared paths.** *(Gated.)*
- [ ] **`ui/shared` actually consumes the generated module.** *(Gated:*
  `contract.conformance.test.ts`, which is a different question from drift.*)*
- [ ] **`docs/api/contract.md` reflects the new surface.** *(Ungated.)*

## 7. Storage, state and migrations

- [ ] **A numbered migration** in `core/migrations/`, next after `0008`. *(Gated
  by the suite, ungated as a decision.)*
- [ ] **Journal truth stays journal truth.** *(Ungated.)* Placement and lifecycle
  answers come from the journal, not from a listing.
- [ ] **Compat captures updated** where the change touches config, CLI output, the
  API, state or upgrade: `core/tests/compat/capture_*.go`. *(Gated.)*

## 8. Providers

Eleven of them: casaos, dockge, generic, openmediavault, portainer, proxmox,
synology, truenas, ugos, unraid, zimaos.

- [ ] **New platform behaviour goes through the capability contract**
  (`apps/common/platform/capabilities/`) rather than into one provider. *(Ungated.)*
- [ ] **The cross-provider conformance suite passes** (`apps/common/tests`).
  *(Gated.)*
- [ ] **Per-provider acceptance docs** in `docs/acceptance/` where the behaviour is
  provider-visible. *(Ungated.)*
- [ ] **Packaging stays consistent**: `distribution/packaging/` and its
  `submission.json`, `canonical.json`, `conformance.json`, plus
  `container/compose.yaml` as the canonical definition and the installer's own
  embedded copy (`scripts/install/embed_compose.py`). *(Gated.)*

## 9. Documentation

- [ ] **`README.md`**: the install block, the subcommand list and "What the browser
  looks like while it works". *(Ungated.)*
- [ ] **The site**, all six pages, whichever the change touches:
  `docs/site/index.html`, `web-ui.html`, `workflows.html`, `reference.html`,
  `first-run.html`, `ssh.html`. *(Partly gated:* only `reference.html`'s command
  table is held to the binary. A new page is picked up by the site-wide
  dead-fragment check in `scripts/install/test_install_docker_host.py` the
  moment it lands, so every `#anchor` it cites has to resolve.*)*
- [ ] **The site's "What has not been proven" section** (`index.html#honest`) says
  honestly what the epic did and did not demonstrate, including on hardware nobody
  here owns. *(Ungated.)*
- [ ] **The prose docs the change touches**: `install.md`, `deployment.md`,
  `runtime-contract.md`, `recovery.md`, `recovery-without-a-terminal.md`,
  `storage-mediums.md`, `ssh-setup.md`. *(Ungated.)*
- [ ] **An ADR in `docs/adr/`** for any decision a later reader would otherwise
  have to reconstruct. *(Ungated.)*
- [ ] **A CHANGELOG entry under `[Unreleased]`**, with the issue number, what
  changed, why it was needed, and what an existing deployment sees. *(Ungated.)*
- [ ] **Package doc baselines** regenerated when a package overview changes
  (`scripts/docs/package-doc.baseline`). *(Gated.)*
- [ ] **Comments the change falsified are fixed.** *(Ungated.)* Several defects
  here came from trusting a comment that had quietly stopped being true.

## 10. Screenshots and GIFs for the site

- [ ] **Regenerate `docs/site/screens/`** with the capture scripts in
  `docs/site/tools/`: `capture-first-run.mjs`, `capture-reference.mjs`,
  `capture-ssh.mjs`, `capture-web-ui.mjs`, `capture-workflows.mjs`.
  *(Ungated.)* 54 files today, 20 of them animated.
- [ ] **A new screen or interaction gets a capture step added to the right
  script**, never a picture taken by hand. *(Ungated.)* A hand-taken image is one
  nobody can reproduce after the UI moves.
- [ ] **Captures run against the mock API, never a real deployment.** *(Ungated,
  and load-bearing.)* The first-run flow claims the administrator account and burns
  the enrollment token, so a capture pointed at a real instance finishes somebody's
  setup for them. The harness starts `ui/shared`'s own dev server, where
  `createMockApi` is substituted under `import.meta.env.DEV`.
- [ ] **The clock stays pinned** (`page.clock.setFixedTime`). *(Ungated.)* Without
  it every re-record differs in every frame with a timestamp in it, and the diff
  stops telling anybody which picture actually moved.
- [ ] **Playwright is borrowed, not installed**, out of the `backupd-tests`
  checkout at the sha in `scripts/e2e/tests-repo.pin`. *(Ungated.)* If the e2e gate
  has not run on this machine, there is nothing to borrow from and the capture says
  so rather than guessing.
- [ ] **Store submission screenshots are a separate thing and cannot be
  generated.** *(Ungated.)* `docs/submission/screenshots.md` requires real hardware
  running a real installation against a real SFTP source. Never substitute a mock
  there. If the epic changes a screen a store listing shows, the row goes back to
  outstanding rather than quietly keeping the old picture.
- [ ] **Run the capture script you did not change, once, before you believe the
  ones you did.** *(Ungated, and this is the item that failed.)* EPIC L's docs
  pass (#817) found two unrelated breaks in this tooling in a single afternoon:
  `Clip.write` in `harness.mjs` named an unbound `FFMPEG` identifier, so every
  GIF encode raised `ReferenceError` and none of the four then-existing scripts
  could have re-recorded anything; and `EXAMPLE.port` was the placeholder
  string `"<your-ssh-port>"`, which #864 turned from a value the wizard coerced
  into one the wizard refuses, making every picture after the Source step
  unreachable. Neither break was visible until somebody tried to take a
  picture, and the second one was caused by a product change nobody connected
  to a capture script. Two unrelated breaks in one tool in one pass is an
  unguarded surface rather than bad luck, and this tooling reads the shipped
  UI's own rules — `isPort`, the rail labels, the field help — so a product
  change invalidates it silently. Gating it is #926.

## 11. Compliance and supply chain

- [ ] **NOTICE and the licence inventory** regenerated if the module graph or the
  frontend lockfile moved. *(Gated.)* They claim to account for everything, so
  vendoring anything a package manager cannot see means declaring it.
- [ ] **No credential reaches disk or a log.** *(Ungated as a rule, mechanised in
  places.)*
- [ ] **`docs/compliance/` and `docs/submission/`** updated where the epic changes
  what is collected, stored or sent. *(Ungated.)*

## 12. Gates

- [ ] **`scripts/ci-local.sh` green.** *(Gated.)* About 45 minutes. It is also the
  pre-commit hook, so committing with `--no-verify` and running it deliberately is
  the normal pattern.
- [ ] **Every new guard has a mutation self-test.** *(Gated for the existing ones,
  ungated for a new one until I write it.)* A guard whose only evidence is that it
  passes on the tree it ships with has proven nothing. Three checks here were found
  in one day that could not fail.
- [ ] **A new CI job goes in the release gate's needs list.** *(Gated:*
  `scripts/tests/release-gate-covers-every-job.test.sh`.*)*
- [ ] **No new `RM_` / `BM_` / `bm_` / `rbm_` identifier.** *(Gated.)*
- [ ] **Tests are red before they are green**, and pushed in that order where
  practical. *(Ungated.)*

### Fixtures

The same "can it fail" question the mutation self-test asks of a guard, asked of
the test data. All of this is ungated: nothing counts fixtures or inspects their
shape.

- [ ] **A fixture earns its place by being able to fail**, not by covering a
  feature. A fixture built out of values the defect cannot move passes whether the
  code is right or wrong, and it is worse than no fixture, because it converts
  "untested" into "covered" on whatever matrix is counting.
- [ ] **One fixture per claim the code makes**, not per feature the format has. A
  kind the epic says it handles needs one, and so does a kind it says it refuses,
  because both are claims. The cross product of every option against every other
  one is not a target, and reaching for it is how a corpus becomes too large to
  re-record and too slow to review.
- [ ] **Pick the values against the wrong implementation, not the right one.**
  *(The one I would most expect to get wrong.)* Ask what the plausible defect
  moves, and choose inputs it moves. A symmetric shape, a zero, an identity
  transform and an origin are all fixed points: the wrong answer and the right
  answer agree there. If the fixture would pass against the bug, it is testing
  nothing and needs different numbers, not more of them.
- [ ] **Richer beats more.** A fixture that leaves most fields at their zero value
  silently stops covering each new field added later, so the test decays without
  anybody touching it. Prefer extending an existing fixture to carry the new field
  over adding a thin new one beside it, and where a round-trip is asserted over a
  whole struct, require every field to be non-zero or explicitly exempt.
- [ ] **A new fixture is added when a defect is found, or when the epic states a
  property nothing existing can falsify.** Those two are the triggers. "This format
  supports it" is not one.
- [ ] **Anything generated is reproducible from its generator**, and the generator
  lands in the same commit. A committed artefact whose producer is not in the tree
  is one nobody can re-record, and a fixture nobody can re-record is one nobody
  will correct.
- [ ] **Provenance and licence are recorded** for anything not written here.

---

## The ungated items, in one place

If I only re-read one section at a phase exit gate, this is it. Nothing in the
repository will tell me I got these wrong:

1. Two phases, five issues each, and the cut list.
2. The conformance matrix, with a falsification per row that has actually been
   watched to fail.
3. A command with no equivalent in the browser (the reverse direction is gated,
   this one is not).
4. Progress reaching the activity feed, the global terminal and the per-set panel.
5. A global default good enough that nobody has to set it, plus the per-set
   override, plus the re-resolve after mutation.
6. The Claude Designer mockup, and the design note landing in `docs/design/`.
7. A new control with no tooltip entry, and the harder half: refusing to write
   one for a control whose effect the code cannot state.
8. README, the four site pages that are not `reference.html`, and the prose docs.
9. The "What has not been proven" section telling the truth about this epic.
10. Regenerated GIFs and screenshots, through the capture scripts.
11. Store submission screenshots going back to outstanding when a listed screen
    changes.
12. The CHANGELOG entry.
13. Comments the change falsified.
14. Fixtures chosen against the wrong implementation rather than the right one,
    and extended rather than multiplied.

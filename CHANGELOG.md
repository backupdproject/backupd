# Changelog

## [Unreleased]

### Added

- **Workflow hooks have a web UI: what ran, what it printed, and the hold
  that stops a set running** (EPIC L, #814, over #813's API). Five surfaces,
  all of them inside the existing shell and its design system: a workflow-run
  screen at `/workflow-runs/{run}`, that screen while a run is live, a
  Workflow panel on each backup set, a workflow environment editor with a
  merged inheritance preview, and a Workflow card in Settings.

  **The three statuses stay three, on screen as well as on the wire.** Backup,
  Workflow and Cleanup are drawn side by side and none is derived from the
  others, because "the backup succeeded and the cleanup did not" means a
  machine may be sitting quiesced with a good backup beside it, and a page
  with one verdict would make that unsayable. A failed workflow names the
  script that ended it; a run whose hooks were bypassed reports its workflow
  status as skipped and says the hooks were bypassed, rather than showing a
  green verdict for scripts nothing executed.

  **All five stages, always, including the ones with no directory.** An
  ineligible stage is drawn greyed and said out loud instead of omitted:
  "the global after stage did not run" and "this deployment has no global
  after directory" are opposite facts when a source has been left quiesced.
  The stage that is executing is ranked above its own rows by an inset rule
  and a tint as well as a badge, so which of the five is running is legible
  without reading.

  **A step killed on its timeout whose exit was never confirmed gets a
  page-level warning**, naming the script, because the consequence is a
  process that may still be running on a machine this product cannot reach.

  **`recovery_required` blocks the per-set Run control** and offers exactly
  two ways out, both of them the API's: resume the cleanup, which runs the
  hooks that run still owes from its own retained spool, or acknowledge it in
  words that are recorded. There is no dismiss. An unreadable hold list is
  reported and also blocks the control, since treating it as "no holds" would
  offer a run the engine is about to refuse.

  **A `.local.sh` hook is "Local Host", executed by the Host Workflow
  Runner**, on every surface that names one. Nothing implies the engine's own
  container grew a shell.

  **No surface can show a secret, and none has a reveal control.** The
  environment editor shows a variable's name and the LOCATION its value comes
  from — a file, an environment variable, an argv — because that is all the
  contract carries; the merged preview shows precedence (deployment < backup
  set < reserved `BACKUPD_*` built-ins, which are read-only) and says where a
  secret will be read from rather than what it is. An empty literal stays
  distinct from no literal at all in both directions.

  **A backup set that configures no hooks shows one sentence and nothing
  else** — no empty table, no findings, no run history. The hook check is a
  button and never a poll, because it hashes every script and opens both a
  runner socket and an SSH connection to the source; a check that could not
  run reports "not examined" rather than a green tick, and the two verdicts
  (`valid_for_backup`, `workflow_valid`) stay apart. Settings reports the
  runner's socket and credential path as read-only deployment facts and says
  whether that address is CONFIGURED — never that the runner is answering,
  because nothing on that read contacts it; liveness, its build and the
  account it executes a hook as come from a set's own hook check, which opens
  the socket. There is no elevation control of any kind.

- **A repository domain can be declared from the UI, the API and a terminal**
  (#862). `POST /repositories` persists a new `repository_domains:` entry —
  id, isolation, passphrase REFERENCE, description — into `config.yaml`
  atomically and hot-reloads, through the same door `POST /backup-sets`
  writes through, and `backupd repository create <domain> --isolation
  shared|isolated --passphrase-file F` does the same from a shell. The
  Repositories → "Define a repository domain" screen is a working wizard
  rather than a read-only explanation of one.

  **Declaring is not creating.** What this writes is the DECLARATION; the
  store underneath is still realized lazily by the first backup run that puts
  a snapshot in the domain, exactly as a domain named on the add-backup-set
  wizard's repository step already was. So the create opens no storage and
  resolves no passphrase reference — not even to describe what it just wrote:
  the 201 reports the domain from the declaration, `degraded`, with a detail
  saying the store is written by the first run, rather than a green row nobody
  measured. `GET /repositories` probes it from then on, where a domain nothing
  has run into yet answers reachable and not yet readable — the storage
  answers and holds no repository.

  **`maintenance_owner` is a gate on this one write and records nothing.**
  `this` comes back `REPOSITORY_DOMAIN_MAINTAINED_ELSEWHERE` when a
  maintenance record for that id already names somebody else, because ADR 0017
  moves ownership by transfer only; `another-instance` declares the boundary
  without claiming it. Neither answer is persisted anywhere, and which
  deployment maintains a store stays a matter of that durable record.

  **The passphrase is a reference in every spelling** — a file, an environment
  variable, or a command whose stdout is the secret — and there is no field,
  flag or input anywhere on this path to type one into. The write is gated by
  the incremental engine (`409 INCREMENTAL_ENGINE_DISABLED`, which the screen
  renders as the sentence naming the config key), and refuses a duplicate id
  with `REPOSITORY_DOMAIN_EXISTS` without touching the file.

- **A second backup engine: incremental snapshots, off by default** (EPIC K,
  #779, issues #780-#789). A backup set can now run `engine: kopia` instead of
  `artifact`: rather than pulling a producer's finished file whole, it
  snapshots a source *tree* into an encrypted, content-addressed repository
  and stores only content that repository does not already hold. A snapshot of
  a 100 GB tree that changed by 1% is a manifest plus roughly the changed
  content, not a second 100 GB copy.

  It is **gated and off by default**: `incremental_engine.enabled: true` in
  `config.yaml`, or `BACKUPD_INCREMENTAL_ENGINE=1` in the environment, which
  overrides the file in both directions. With the gate off, a configuration
  that declares incremental sets still loads and still backs up every artifact
  set on schedule — the incremental ones refuse with one sentence naming both
  ways to enable it (`409 INCREMENTAL_ENGINE_DISABLED`, CLI exit 1), and a
  gated restore refuses rather than succeeding having written nothing.
  **Existing artifact backup sets are unchanged in either position, and
  nothing ever converts one engine into the other**: no auto-migration on
  upgrade, on reload or as a convenience, and no `--engine` on
  `backup-set patch`. Moving a source onto the incremental engine is a new
  backup set created beside the old one, which is a documented procedure.

  **Scanning is not incremental storage**, and the reports say so. Every run
  walks the whole source tree — there is no incremental scan, no change
  journal and no watcher — in bounded memory: a million entries in one flat
  directory cost 4.2 MiB of peak heap streaming against 493.4 MiB for the
  slice-shaped listing this product used to build, with the first entry
  arriving in 12 ms rather than after 102 seconds. The whole tree is also
  READ every run: content is reused, files are not, because skipping a file's
  bytes because its size and modification time look familiar is how a
  rewritten file silently keeps its old content in every later snapshot. What
  is incremental is the STORAGE, so a run reports five separate measurements
  and deliberately no total: entries scanned, logical size, read
  from source, written to repository, reused. Only the fourth is storage
  growth. Every counter is nullable and null prints `not measured`, never `0`.

  What a run is allowed to claim about a source is declared per set and
  recorded per run: `live_best_effort` (the default and the weakest claim),
  `externally_quiesced`, or `external_snapshot`, and only the last is ever
  rendered as "consistent". Under the stronger two, a mutation observed during
  the run is reported as a contract violation rather than passing unnoticed.
  Content reuse never becomes permanent trust: every path in every source is
  re-read on a bounded cadence the policy names.

  The operator surface is `backupd snapshot` (`list`, `show`, `holds`,
  `retention`, `verify`, `restore`, `hold`, `unhold`), `backupd repository`
  (`health`, `maintenance`), four
  repository-domain screens and four per-set snapshot screens in the web
  interface, and six new `GET` routes plus four actions on `POST /operations`
  (`restore_snapshot`, `verify_snapshot`, `hold_snapshot`,
  `release_snapshot_hold`) — no new mutating route, so the CLI and the browser
  submit the same durable, idempotency-keyed operation. Snapshots are retained
  under the deployment's existing GFS chain rather than a second policy, may
  be protected by holds that require a reason, and are verified at four
  depths with the level *achieved* reported separately from the level asked
  for.

  Three limits ship documented rather than discovered. An incremental set's
  source must be a backend whose directory listing can be bounded —
  `local_volume` or `s3`; **`sftp` cannot be**, and a `kopia` set pointed at
  an SFTP source is refused at run time, because pulling a remote producer's
  finished files over SSH is the `artifact` engine's job. Every repository
  domain's storage is local, under the backup root, inside the reserved
  `.backupd` namespace that artifact discovery, retention and prune may not
  enter. And no verb or schedule runs repository maintenance yet: the
  surfaces report its decision, its owner and what it has reclaimed.

  New documentation: [`docs/incremental-engine.md`](docs/incremental-engine.md),
  [`docs/incremental-runbooks.md`](docs/incremental-runbooks.md) and
  [ADR 0018](docs/adr/0018-shipping-the-incremental-engine-behind-a-gate.md),
  plus the command table, the nine new screens and the gate on the reference
  page.

- **The connection test proves write permission, and read-only is derived from
  it** (#852). A seventh step, `write_probe`, creates a uniquely named dotfile
  under the configured remote path, removes it, and confirms it is gone. Both
  answers **pass** — a read-only source account is an ordinary, supported and
  frequently recommended posture rather than a misconfiguration — so what the
  answer decides is `writable`, not the verdict.

  What that arms is deleting the remote original after a verified backup, and
  until now the only proof was the first cycle that tried: an account that
  could read every byte and unlink nothing looked identical to one that could
  do both. Enabling delete-from-source against a source just proven
  non-writable is now **refused rather than coerced** (`409
  BACKUP_SET_SOURCE_NOT_WRITABLE`), from one place, covering create, first-run
  create, a connection-changing edit and `backup-set read-only off`. A `200`
  for "back up and delete the originals" that quietly did not delete is the
  failure this ends. Turning read-only **on** is never gated: making a set
  safer does not need the source's permission. `writable` is a required field
  on the wire and never omitted, because absent and false must not be the same
  thing to a client, and nothing may enable a delete on an absence of
  evidence. The probe's errors are classified and then dropped for their own
  text, so no remote path, chroot home or probe filename can reach a log, a
  feed or an API response.

- **How often a source is checked is a setting, at two scopes** (#845). Settings
  gains a **Service behaviour** card holding the deployment-wide polling
  interval, and a backup set's own Edit form can override it for that set alone;
  an empty box there means "follow the deployment's", which is what the set does
  unless it says otherwise. Both scopes are refused below one minute, wherever
  they are written — a web form, the HTTP API or a hand-edited `config.yaml` —
  because below that a poll stops being a schedule and becomes pressure on
  somebody's NAS.

  The scheduling loop sleeps until the earliest moment any enabled set is
  actually due, rather than at a fixed granularity: a set asking for seven
  minutes beside one asking for five is polled every seven, not rounded up to
  ten. A set is timed from when it was last ATTEMPTED, so a source that is down
  is retried on its own interval instead of on every wake. A save takes effect
  on the running process immediately, including on a loop already asleep in a
  long interval, and it carries the existing schedule across rather than
  restarting it — saving a setting is not a reason to go and knock on every
  backup source in the deployment.

- **The administrator account is recoverable, and enrolment is where that is
  arranged** (#830). The one-time enrolment form now also takes a recovery email
  address and the SMTP details to reach it, and the server sends a confirmation
  message to that address, over exactly those details, *before* it writes the
  administrator record: a send the mail server will not accept answers
  `SMTP_SEND_FAILED`, creates no account, and leaves enrolment open. A recovery
  address nobody has ever delivered to is worth nothing on the day it is needed,
  and the day it is needed is the day nobody can sign in to configure it, so it is
  verified on the day it is given. The username is unchanged as the login
  identity; the address is an additional field on the same record.

  What it buys is the sign-in page's new **Forgot password?**: it takes the
  username alone and answers identically whether or not that name is the
  administrator's — an endpoint that answered differently would tell an
  unauthenticated caller what the account is called — and where it does match, a
  single-use link valid for 30 minutes is mailed to the recovery address.
  Completing the reset sets the new password and revokes every live session,
  including the browser that asked, so the answer to a successful reset is the
  sign-in page rather than a dashboard; a reset is also what an operator does when
  they think somebody else holds a session, and one that left sessions running
  would not be a recovery. The link is built from the same `PUBLIC_BASE_URL` the
  enrolment notice already uses, which is worth setting properly at install time:
  unlike the enrolment notice, nobody is reading a log to notice it points at
  `localhost`.

  The SMTP password is never persisted, logged or returned. `local-auth.json`
  gains `recovery_email`, `recovery_email_confirmed_at` and an `smtp` object whose
  password is held as a secret reference in the same form every other secret in
  this project uses, and `GET /api/v1/auth/recovery` answers with no password
  property at all and a `passwordSet` boolean in its place — absent rather than
  masked, because a form that round-trips what it was served would otherwise write
  the mask in as the new password. Both the address and the SMTP details are
  editable afterwards from Settings' **Account recovery** card, with a test send
  beside them, and changing the address re-verifies it by confirmation message
  rather than trusting the new value.

  Two smaller consequences. A *failed* enrolment no longer spends the bootstrap
  token: it is consumed only on success, so a rejected password, a malformed
  address or an SMTP server that would not accept the message all leave the same
  `/enroll?token=…` link usable — correct the field and submit again, where a
  too-short password used to burn the link and leave the operator restarting the
  engine for a fresh one. And `backupd-web auth create-admin` takes the same
  details as optional flags (`--recovery-email`, `--smtp-host`, `--smtp-port`,
  `--smtp-security`, `--smtp-username`, `--smtp-password-stdin`, `--smtp-from`),
  because a provisioning run in a pipeline often has no mail credential to give
  and refusing to create the account there would buy no security; given an
  endpoint it sends the same confirmation and fails the command if that send
  fails, and given none it leaves recovery unconfigured for the operator to finish
  in Settings.

  **The address is not merely mailed to — it is verified, and an account whose
  address nobody verifies is deleted** (#830, scope additions 8-9). The one
  message enrolment sends now IS the verification: it carries a single-use link
  (`PUBLIC_BASE_URL` + `/verify-email?token=…`, expiring in 30 minutes) and the
  account it creates is **provisional** — `local-auth.json` gains
  `recovery_email_verified_at`, a `verification_deadline`, and the SHA-256 of the
  outstanding token, never the token itself. `POST /api/v1/auth/verify-email`
  redeems the link (single-use, expiring, and one `VERIFY_TOKEN_INVALID` refusal
  for expired, spent, unknown and no-such-account alike, so nothing can be probed
  with it); `POST /api/v1/auth/verify-email/resend` mails a fresh one to whoever
  can still sign in.

  If the address is never verified, a reaper **deletes the administrator record**,
  revokes its sessions and reopens enrolment with a fresh bootstrap token. The
  deadline is `max(enrolment-link window end, created_at + 30 minutes)`, fixed at
  creation and moved by nothing afterwards — a resend that extended it would be no
  deadline at all. It is enforced on a timer *and* at service start, so a
  deployment that was shut down through its whole window still cleans up on its
  next start. That is deliberately harsher than a warning: an SMTP server
  accepting a message proves the endpoint works and nothing more, since a typo
  that lands in the neighbouring domain is accepted just as happily as the right
  address, and an administrator nobody can mail is already lost — 30 seconds of
  re-enrolment now is cheaper than discovering it the day a password is forgotten.
  Verifying clears the deadline for good, so an established administrator who
  later edits the address gets an unverified address and a nudge, never a deleted
  account, and `auth create-admin` run with no SMTP endpoint at all never gets a
  deadline, because no link was ever mailed for anybody to open.

  While the address is unverified the console carries a banner that cannot be
  dismissed, naming the address, the deadline and a **Resend link** action, plus a
  `/verify-email` page that reports what the link did. `auth create-admin` prints
  the same warning to stdout and takes `--public-base-url` for the link it mails.

  **Changing where recovery mail goes now asks for the password** (#830, security
  review). `PATCH /api/v1/auth/recovery` takes `currentPassword` and re-checks it
  before it resolves the SMTP credential, sends anything or writes anything,
  refusing with the same 401 `UNAUTHENTICATED` that `POST /auth/password` gives.
  The recovery address and the SMTP endpoint decide where a reset link is
  delivered, so a caller holding a live session but not the password could
  otherwise repoint them, press **Forgot password?**, receive the link and take
  the account over for good — a stolen cookie turning into permanent ownership.
  Settings' **Account recovery** card grows an **Administrator password** field
  to match; **Send test email** does not ask for one, because it changes nothing.

  Three narrower holes in the same routes are closed with it. A change to the
  **SMTP endpoint** is now proven like a change of address: the verification (or,
  on an already-verified mailbox, the test message) goes out over the endpoint
  the request establishes and the whole update is refused with
  `SMTP_SEND_FAILED` if it cannot be delivered, so a new host can no longer be
  stored beside an untouched `recoveryEmailConfirmed: true` and fail silently at
  the one moment it is needed. Redeeming a verification link now spends the
  **exact challenge** it matched, re-compared under the store's own lock, so an
  address change or a resend landing in between can no longer leave the NEW
  address verified on the OLD address's token. And the reaper's decision and its
  deletion happen in one critical section (`Store.DeleteUnverifiedAdmin`), so a
  verification committing at the deadline instant can no longer be answered 204
  by a process that deletes the account a moment later.

  A reap that reopens enrolment **at runtime** also prints the fresh enrolment
  notice, to the same stream the startup one goes to. A host prints that notice
  once, at startup, so a token minted an hour into a process's life used to
  expire in 30 minutes with nothing having shown it to anybody — a deployment
  locked out by the mechanism that exists to prevent lockouts.

  Two contract corrections: `PATCH /auth/recovery` answers 400 `INVALID_REQUEST`
  rather than 500 when no SMTP endpoint is configured to prove an address over
  (the answer its siblings already gave, and the one the contract already
  declared), and `RecoverySettingsResponse`'s `smtp` and `verificationDeadline`
  are optional members that are OMITTED when there is nothing to report, instead
  of a null against a non-nullable schema and an empty-string sentinel.

- **A backup set can name a subtree discovery must not walk into** (#737).
  `exclude_paths` on a backup set lists directories, relative to
  `remote_path`, that the listing skips: "recurse into `uploads/`, never into
  `uploads/tiles/`". It is a path list and FR-5's `include` is a basename
  pattern list, deliberately two fields — an include pattern is matched
  against a candidate's basename wherever it turned up, so it can say nothing
  about *where* to look, and teaching it to would change what every pattern
  already written means.

  The point is the cost, not the result set. Discovery's listing is fully
  recursive by design, so a set pointed at a directory an application also
  caches under walks the cache too — 65k files across 1.6k subdirectories in
  the deployment that reported this, which against a remote with no native
  recursive listing is 1.6k round trips per poll for artifacts nobody wants,
  and a pass that did not finish. An entry becomes an rclone directory
  filter, so the walk declines to descend rather than walking and discarding:
  the excluded directories are never listed at all. Each entry is a literal
  relative path, root-anchored under `remote_path` (an optional leading or
  trailing `/` is tolerated and ignored, and does not make the value
  filesystem-absolute); a traversal segment, a backslash, surrounding
  whitespace or a glob metacharacter is refused rather than half-honoured — a
  `tiles ` would become a filter matching nothing and quietly resume the full
  walk. Writing none of them is every configuration that exists today,
  unchanged.
- **The activity feed is read a page at a time** (#730). `GET /api/v1/activity`
  takes a `before` cursor and answers with a `next_cursor`, so the Activity
  page asks for a bounded first page and offers "Load older events" instead of
  loading a deployment's entire lifecycle record on open. The record is
  append-only and nothing prunes it, so the previous shape got slower every
  week a deployment stayed up, and clamping alone would have left everything
  older than the newest thousand events unreachable. The cursor is a row
  position echoed straight back from `next_cursor` — described that way rather
  than as "opaque", because a token a client is told to hand back unchanged is
  the honest description of what it is — it pages on the journal's own ordering
  rather than on an offset, so a transition recorded between two requests
  cannot make a page repeat itself, and any value the feed could not have
  issued (unparseable, negative, zero, too large to name a row) is ignored and
  answered with the newest page rather than refused, the same way an
  unparseable `limit` already was. The filters above the list apply to every
  page loaded, not only the newest one.

- **`LOG_LEVEL` is the diagnostics switch, and it reaches every process**
  (#730). It was already read by the web host's own surfaces while the engine
  built its log sink at a hard-coded `info`, so an operator who set it got the
  reverse-proxy trace and nothing from the process the trace describes.
  `core/service.Open` and the CLI's own sink now take the level from the
  environment (`obs.LevelFromEnv`), and `container/compose.yaml` passes
  `LOG_LEVEL` to BOTH services — with `container/.env.example`, the provider
  adapters and the Portainer template carrying it too — because the two halves
  of one request are recorded in two containers and one of them at `debug`
  gives half of every story. `BACKUPD_DEBUG=1` stays as the shortcut, and
  `docs/deployment.md`'s new "Turning on diagnostics" covers all three
  switches, the browser's `?debug=1`/`?debug=0` included.

- **The proxy reports what the body turned out to be, not only what it
  declared** (#730). At `debug`, `web-ui` wraps the upstream body in a
  transparent observer and emits `proxy_upstream_complete` once per request:
  bytes actually copied against the `Content-Length` declared, whether the read
  ended at EOF, and any read or close error, at `warn` when the two disagree.
  This is the shape the reported fault takes from JavaScript — a well-formed
  status line, a body that stops early, a browser that refuses the response
  before `fetch` sees any of it — and until now nothing in the container
  recorded that the transfer came apart. The header-phase line is renamed
  `proxy_upstream_headers`, since that is what it is.

- **Every response carries a correlation id, and the browser names its own
  attempt** (#730). The id is minted in one middleware at the web host's edge,
  before authentication and routing, and set on every response rather than only
  on refusals: a 200 whose body the browser could not read used to carry none,
  so "the browser could not read this" and "here is what was sent" were two
  records with nothing in common. `serve-ui` forwards it to the engine, which
  adopts a valid inbound id instead of minting a second, so the two containers'
  lines join. And because a request that gets no response carries nothing back,
  the browser now sends `X-Client-Attempt-Id` and logs it in the `[rm-debug]`
  line, which the server writes down (bounded and validated) — so a console
  screenshot of a failed `fetch` can still be matched to the server's record of
  the same request.

### Changed

- **The debug shortcut is `BACKUPD_DEBUG`, and `RM_DEBUG` is deprecated**
  (#794). The one-variable diagnostics switch still carried the project's
  old `RM_` prefix, from before the rename to backupd, which is the wrong
  name to read out over the phone call this knob exists for. The engine
  (`obs.LevelFromEnv`) and the web host (`webhost.envLogLevel`) now both
  accept `BACKUPD_DEBUG=1`, and `container/compose.yaml`,
  `container/.env.example`, every provider adapter's compose file and
  `docs/deployment.md` name that spelling.

  `RM_DEBUG=1` keeps working, as a deprecated alias, for one release. The
  two are OR'd rather than ranked because neither has ever had an "off"
  value — only the documented `1` means anything — so a deployment that
  upgrades one container before the other, or that still has the old name
  in a compose file nobody re-derived, does not go quiet in the middle of
  a diagnosis. Set `BACKUPD_DEBUG` on new deployments; `RM_DEBUG` will be
  removed a release after this one.

- **The session and CSRF cookies are named `backupd_session` and
  `backupd_csrf`** (#794). They were `bm_session` and `bm_csrf`, named for
  a brand two renames ago. Both new names are the only ones the runtime
  WRITES; both old names are still READ for one release, so upgrading in
  place does not sign every open console out and does not break a client
  mid-session — a rename is not a reason to invalidate a credential. The
  CSRF cookie needed more than a fallback: its client half is page
  JavaScript that reads the cookie by name, so an upgrade is guaranteed to
  have already-cached bundles in the field echoing whatever they found
  under the old name. A request carrying only the old name therefore has
  that exact token re-issued under the new one rather than being handed a
  second, different token — two names holding two values would mean
  whichever one the double-submit check preferred would refuse the other
  with 403 `CSRF_TOKEN_MISMATCH`. Nothing an operator configures changes,
  and the old names disappear from the wire on their own as each client is
  issued the current one.

### Fixed

- **`scripts/api/check-client-paths.sh` runs again** (#730). The diagnostics
  commit on this branch changed `ui/shared/src/api/client.ts` to fetch through
  a local `const url = BASE + path`, and that gate refuses to trust the paths
  it reduced unless the client's single `fetch` literally reads `fetch(BASE +
  path` — a URL built any other way is one the gate never saw. It had been
  failing for every path in the file, which is a CI step red for reasons that
  have nothing to do with the change being checked.

- **A deployment that asked for no diagnostics pays for none** (#730). The
  activity feed's debug line reports the bytes a counting response writer
  actually wrote, instead of marshalling the whole payload a second time to
  guess the size; the browser client resolves the toggle once per request and
  builds its detail objects through a thunk, so nothing is constructed on the
  path every API call in the bundle takes; and the reverse proxy no longer
  allocates a per-request context value of its own for the clock, riding along
  in the one the correlation id already needs — which is also what keeps
  `proxy_error` reporting how long the browser waited on a default `info`
  deployment, where the failure it describes actually happens.
- **SSH (SFTP) destinations are described, and say they are not ready yet**
  (#731). `sftp` is a registered destination backend now, with its own bundled
  manifest: a host, a port that defaults to 22, a user, a `known_hosts` file,
  the directory to write into, an optional subdirectory, and an SSH private
  key stored the way every other credential is. `known_hosts` is required, not
  optional, for the reason the source side already refuses to connect without
  one: unset, an SSH client accepts any key from any server that answers.
  This cost no new dependency and no binary growth — the sftp backend has been
  linked since FR-4, because a backup source is read over it — so what it cost
  instead is written down in `docs/adr/0005-destination-backend-decision.md`:
  a destination backend is its own decision, taken separately from the source
  backend that shares its implementation, and a fourth one fails a named test.

  Declaring one in `config.yaml` or saving one from the wizard is not wired
  yet (#235), and the catalogue says so rather than leaving an operator to
  find out at the end of a form: a manifest carries a new `configurable` flag,
  `sftp` reports `false`, `GET /api/v1/backends` serves it, and the
  add-a-destination picker renders that row disabled with the reason. Searching
  for "sftp" still finds it — that is the point of describing it at all — and
  nothing about it can be submitted. `configurable` defaults to true, so an
  ordinary manifest says nothing and behaves as it always has.

- **A deployment whose engine is unreachable says so, on every surface that
  meets it** (#795). The web-ui container on a reported NAS could not resolve
  the engine (`dial tcp: lookup rclone-manager: no such host`), and the
  Activity page showed nothing. One fault, and three places turned it into
  something other than what it was.

  The reverse proxy inside `serve-ui` answers **502 with no body** when it
  cannot reach the engine, and the browser client read that as a refusal it
  could not parse: "The backup service returned an unexpected response.", with
  no remediation under it. The backup service returned nothing — it was never
  spoken to — so the sentence named the wrong machine and pointed away from
  the container an operator has to go and look at. A refusal now carries the
  response status, and — because a status cannot say which hop wrote it —
  `serve-ui` now marks the responses its own proxy writes
  (`X-Backupd-Proxy-Error`, deleted from any upstream response so it can only
  ever mean this hop). A refusal identified that way is described as what it
  is: Backupd's web interface could not reach the Backupd service, with the
  correlation id that 502 really carried (it is inherited from the request
  scope, so it matches the `proxy_error` line in the same container's log). A
  5xx that carries its own **typed** reason is untouched, whether or not this
  build recognises its code, because the service's own sentence says more than
  anything written here could.

  `/auth/session` is proxied down the same hop, and six provider bridges held
  a byte-identical copy of a session read that treated **any** refusal as "not
  signed in". So an operator who reloaded during the outage — the first thing
  anybody does — was told they had been signed out and handed a sign-in form
  that posts back down the connection that is failing. There is one reader
  now, and it answers that question only when the service answered it: **401**
  means signed out, and everything else is a failure to ask. A **403** is a
  policy denial that no sign-in can lift, so it goes down the typed path with
  every other refusal. The app records the failure and replaces the login form
  with a page that says only what is known: "Backupd is not answering" when
  nothing came back, and "Backupd could not check your session" — over the
  service's own words — when something did. Both offer Try again.

  And there was no reproduction. `scripts/e2e/three-machine-web-ui.sh
  --break-engine` takes the engine away from a browser that is already signed
  in and holding the app — mid-session, because a stack that starts broken
  never gets a browser past the login page — leaving `serve-ui` up in front of
  it. The client container has no Docker socket, so the break is driven
  through files in a directory it already has, the harness acknowledges only
  states it verified (and says so explicitly when it could not reach one,
  rather than letting a suite read its own timeout), and the mode rehearses
  the fault before handing the stack over rather than letting a suite pass
  against a stack that was never broken.

- **A wizard's step rail cannot be used to skip a step, and does not tick a
  step nobody filled in** (#864). Two guided flows drew the shared rail
  (`components/WizardStep.tsx`) with every step clickable at any time and a
  green check on every step behind the cursor. Both halves of that were
  wrong, and the second one worse than the first: an operator could click
  from "Source" to "Review" in the add-backup-set wizard — past the
  connection test whose verdict steps 3 to 7 read, including the write probe
  that decides whether deleting from the source may be offered at all — and
  arrive at a page drawing seven ticks for seven steps they had never seen.
  A tick that means "the cursor went past this" is the product telling
  somebody their configuration is done.

  The rail now takes what is finished and what is reachable from the flow
  that owns the answers: a step is ticked for its OWN inputs, independently
  of where the cursor is, and it is reachable only once every step before it
  is finished — so the first unfinished step, and every step already
  answered, stays open (going back to check or fix an answer is what a rail
  is for) while nothing past it is. A locked step is a genuinely disabled
  button, which is also what takes it out of the tab order, so there is no
  keyboard route around it; Continue refuses on an unfinished step, because a
  footer that walks past one is the same hole in the same gate. A step that
  does not apply to the engine chosen — a repository domain for an artifact
  set — is finished rather than blocking, exactly as it already renders "not
  for this engine" rather than an empty form.

  In the add-backup-set wizard the load-bearing gate is the connection test:
  steps 3 onwards stay shut until it passes for the values on the form now,
  and editing the host, the port or the directory to back up puts them back
  out of reach, because a result that outlived the values it was about is a
  green check standing for a connection nobody made.

  The restore flow gates the same way: Confirm — and so Start restore — is
  out of reach until a snapshot is chosen, the scope is actually answered
  ("one path inside it" with no path typed is not an answer) and a
  destination is named. Its last step is ticked by the submission rather
  than by arriving at it, and a submitted restore lifts the gate, so editing
  a field afterwards cannot hide the operation that is already running.

  Every Save refusal these flows already had (#146, #624, #852) is still
  underneath: a handler reachable by any other route must not save a set
  whose connection nothing proved.

## [0.4.0] - 2026-09-09

### Added

- **A destination is an instance of a registered backend** (#664, #665). What
  a backend is — its identity, the fields an instance of it declares, each
  field's rules, and the probe steps that prove one — is written in a manifest
  the engine reads, rather than in a schema per backend spread through Go.
  `core/internal/backend` is that registry, standard library only, and exactly
  two manifests are bundled: `core/internal/backend/bundled/local_volume.json`
  and `s3.json`. A malformed manifest is refused at load rather than skipped, a
  manifest naming an rclone backend this build cannot dial is refused, and none
  of them may claim the reserved id `local`. Field values are judged by shape
  and a refusal never echoes what was typed into a field, so a credential
  pasted into the wrong box cannot come back out in a message.

- **S3 is described by its manifest rather than by `config.Validate`** (#667).
  A bucket's shape, the closed sets for storage class and upload verification
  and the four rules on a key prefix belong to the manifest now, and the legal
  set of destination types is whatever the registry can express instead of a
  list written out in Go. A `config.yaml` frozen from before any registry code
  existed still loads, validates and re-marshals byte for byte, which is the
  test that would have caught this delegation drifting from the schema it
  replaced.

- **A directory on a second disk can be a destination of its own, and there
  can be more than one** (#666). A `local_volume` destination takes a `path`,
  and the local probe gains a fourth check beside reach, deliverable and
  space: `distinct_volume` compares device ids and refuses two declared local
  destinations that turn out to share a filesystem, naming the one it collides
  with — two names for one disk is two copies that are one copy.

- **`local` is a declared destination, seeded at first run** (#666, #670). The
  local hard drive was an implicit special case every layer had to remember.
  A fresh install now writes it out as instance zero of the `local_volume`
  backend — `id: local`, with the backup root it just established as its path
  — after probing that root, so it is verified without anybody pressing
  anything. Its undeletability stopped being a rule of its own: what refuses a
  removal is that the destination is in use, or holds the default, or is the
  last one left, exactly as for any other. Hand the default to something else
  and the seeded entry becomes an ordinary, removable destination.

- **`GET /api/v1/backends`** (#664, #668). The catalogue an operator chooses
  from, served read-only, carrying the two naming rules with it — the
  instance-id pattern and the reserved id — for the reason
  `tier_name_pattern` already established: a form has to refuse exactly what a
  hand-edited configuration file would be refused for, and a client holding
  its own copy of the rule goes stale in one direction only, silently
  accepting a name the engine then rejects. Storage shapes this build can dial
  that no manifest declares are served as `unregistered` — today that is
  `sftp` — which is a subtraction from what the engine requires rather than a
  second list somebody has to remember to update.

- **The add-a-destination wizard: choose a backend, then name the instance**
  (#668). Two steps, deliberately kept apart even though this release ships
  two backends and it therefore looks like ceremony. Several instances of one
  backend is the normal case — two local volumes, a hot bucket and a cold one
  — and a screen that collapses "pick S3" and "call it `cold_archive`" into
  one act has quietly re-asserted that a destination *is* a backend type. It
  also puts the uniqueness rule where it applies: step 2 lists the instances
  that already exist on the chosen backend, so an operator sees why a name is
  taken instead of meeting a validation error about it afterwards. There is no
  list of backend names in the component and no branch on which one was
  chosen; the rows come from the registry, and nothing is written by any of
  the three panes.

- **The configure-a-destination wizard is a renderer for a manifest, not a
  form per backend** (#669). One renderer per declared field kind — string,
  path, url, enum, bool, credential, key_prefix, and no eighth — with the
  per-kind rules transcribed from the engine's own validator, the probe drawn
  as one step list in three states, and a review pane. Which fields appear, in
  what order, which are required and what an unset optional one resolves to
  are the manifest's answers; local volume renders a directory and a
  subdirectory, S3 renders six fields and a credential pair, and neither list
  appears in the code. Nothing is written until the probe passes, and what is
  written is what was proven: the proof is keyed to the values it ran against,
  so editing a field afterwards withdraws it rather than carrying it over. A
  credential is written to the credential store and never read back, and the
  review pane says so where the value would otherwise be.

- **`install --cli-only`** (#689). A deployment with the scheduler and the
  command line and no browser, instead of one shape and an operator who wanted
  the other writing their own Compose file and owning it forever. The engine
  runs as `/rbm daemon`, no `web-ui` service is started, and no port is
  published, so nothing in the deployment serves HTTP. The interface is a
  wrapper at `<prefix>/bin/rbm` over `docker compose run --rm --no-deps`, not
  `exec`, because the first command a fresh host needs runs before anything is
  started. Such an install stages everything, starts nothing, exits 0 and
  prints the command that writes the first configuration — a daemon with no
  `config.yaml` is refused rather than started, so starting it would only
  produce a crash loop and an installer that claimed success over it. The
  shape is recorded in `.env` and adopted on a bare re-run, because an upgrade
  is not the place to start publishing a Web UI on the LAN of a host somebody
  deliberately installed without one; `--no-cli-only` converts it back and
  says so out loud.

- **`install_docker_host.py enroll-link` mints a fresh enrolment link and
  prints it** (#714). The link an install prints lasts thirty minutes and
  works once, and nothing reissues it while the engine keeps running, so the
  documented way back from a lapsed one was raw Docker commands. The
  subcommand restarts the engine, which is what mints a token, waits for the
  new notice, and prints the **last** one in the log — the last, because a
  container keeps its log across a restart and the dead notice is the one
  above, which is how somebody pastes an invalidated link into a browser and
  reads a refusal that does not explain itself. It says in as many words that
  every link printed before it, including any still in the scrollback, is now
  dead.

- **A licence that names this product and its copyright holder, and a
  contributor agreement** (#685). `LICENSE`'s appendix, `NOTICE` and
  `compliance.json` all said *Backup Manager*, in the one file a redistributor
  reads to find out what they have; the product is rclone-manager and the
  holder is Roman Goldmann. Both artifacts are regenerated from
  `compliance.json` rather than hand-edited, and the Apache-2.0 body itself is
  untouched — only the appendix's copyright line was ever ours to write.
  `CONTRIBUTOR-LICENSE-AGREEMENT.md` did not exist at all; it is adapted from
  the Apache ICLA and signed once, by adding `contributors/<username>.md` in a
  first pull request rather than on every one, with one clause written for
  this project: a contribution quietly carrying somebody else's code makes the
  generated licence inventory and the NOTICE false, and those are exactly the
  documents a recipient relies on. `CONTRIBUTING.md` covers the rest.

- **The site is published, and it covers the rest of the product** (#673).
  `docs/site` had been in the tree with nowhere to be read; it is deployed to
  Pages by a workflow of its own, because the branch setting can only publish
  the repository root or `/docs` and moving the site there would mix the pages
  written for people using the product in with the acceptance procedures and
  EPIC documents written for people working on it. `web-ui.html` and
  `ssh.html` are new, `index.html` and `first-run.html` are rewritten, and ten
  of the captures are moving pictures rather than stills, because most of what
  the last two epics added is a thing that happens rather than a thing that
  sits still. A clip is a list of frames with a hold time each, encoded
  through ffmpeg's concat demuxer, so the same script produces the same file on
  any machine and a diff of the output still means something. Every one of
  them was re-shot after the rename, so no capture on the site shows a command
  name this release does not have (#651).

- **A reference page that enumerates instead of walking** (#693). The site had
  a home page for the shape of the product and a tutorial for the one path a
  new install takes, and nothing that answered "what does this button do":
  three surfaces, three different non-answers. `docs/site/reference.html` lists
  every screen, control, command and installer flag, with what pressing one
  costs — the distinctions you cannot get by looking, like revalidate moving
  nothing where retry ingestion re-transfers, reinstate forfeiting the deletion
  of the remote original for good, or an emptied retention chain reinstating
  the default one rather than switching retention off. The command table is
  held to the binary's own dispatch table and the flag table to the installer's
  own parser, both by tests with positive controls, so a page that claims to
  list everything cannot quietly stop doing so.

- **`bash scripts/ci-docker.sh` runs the whole gate in a container, on a
  toolchain nothing can change under it** (#703). `scripts/ci-local.sh` is not
  modified — it is still the gate, its steps are still the steps and its
  verdict is still the verdict; what moves is what it runs on. Four red runs
  in one evening were none of them about anything in this repository: a
  Command Line Tools update landed mid-session and every cgo link on the
  machine started failing, in every repository. A red run has to mean the code
  is wrong, otherwise the reasonable response to one becomes "run it again",
  and that is the end of a gate as evidence. The container uses the host's
  Docker daemon through a mounted socket rather than starting one inside, so
  the two-machine proof creates exactly the sibling containers it always did;
  the repository is mounted at its own host path, or every path the gate hands
  to `docker run -v` names a directory that does not exist out there; and each
  JS workspace's `node_modules` is masked by a volume, because a macOS install
  of esbuild cannot execute in a linux container.

### Fixed

- **`backup-set create --no-verify` can write a first configuration again**
  (#670). Seeding the local hard drive as a declared destination at first boot
  proves the backup root first, which is right — but the probe was wired as a
  refusal rather than as a verdict, so it failed the whole first-run write even
  when the operator had explicitly asked not to verify. On a host whose backup
  root is not mounted yet, that made the first configuration unwritable by the
  one flag that exists for the situation: the same command printed
  &ldquo;`--no-verify` was given, so nothing was resolved&rdquo; for the SSH half and
  then refused for this one. The probe now runs either way and its answer is
  recorded either way — the seeded destination carries
  `connection_unverified: true` when it did not pass, which the destinations
  card already shows as **never proven** and which a passing test connection
  clears. Only the refusal is conditional: an install nobody opted out of still
  has to prove its backup root before anything is written.

- **`rbm medium remove local` explains itself with the reason it actually
  has** (#670). The CLI test still demanded the words &ldquo;cannot be
  un-declared&rdquo;, the local-specific refusal deleted when `local` became a
  declared destination; the engine had been answering the generic
  is-default refusal, correctly, and the assertion had outlived the
  behaviour. Retargeted, and strengthened to prove the refusal is about the
  mark rather than the name: move the default elsewhere and the reason goes
  away.

- **A completed local copy is no longer recorded as zero bytes** (#662). The
  durable commit measured the file it had just linked into place and then wrote
  the transfer's own reported numbers instead, so a local read-back that came
  back empty became a permanent, content-verified record of an empty backup
  over a file that was 294 bytes and perfectly good — in the journal placement
  and in the sidecar recovery manifest alike. Both are now written from the
  measurement, `verification_class: "content"` is only ever claimed over a
  digest of the bytes at the location, and a transfer report that disagrees
  with the committed file is reported as a fault on the transition an operator
  reads. The copy step no longer trusts the backend's byte count either: it
  measures the `.partial` it just wrote and refuses to record a copy shorter
  than the remote object the journal already recorded.

- **Reconciliation no longer condemns a durable copy it never opened** (#662).
  A disagreement between the recorded remote size and the recorded transfer
  size — two fields of the same journal row — quarantined an intact backup. It
  is now treated as a fault in the row: the file is checked against the remote
  identity captured at discovery, the one figure a bad local read-back cannot
  have written, and a refusal names the digest it measured. A copy that is
  genuinely wrong is still quarantined.

- **A settled record fault (#662) reached no operator, and its own content
  check was disabled forever** (#663). Reconciliation's fix for a
  self-contradictory row (recorded remote size disagreeing with recorded
  transfer size) rode a `noAction` finding that `rbm reconcile` never
  printed, and the branch it took stopped at the size question, so the
  content check FR-17 also runs never ran again on that row, on any later
  pass. A settled record fault is now flagged `NeedsInvestigation`, so it
  prints alongside the finding's reason, and `rbm reconcile`'s own summary
  line no longer says "no unresolved findings" over a run that just printed
  one. The row's durable local copy is also now hashed against the
  discovery-time remote digest (when one was recorded, and this build can
  compute it) rather than only re-checking its size, and the reason says so
  either way — content-verified, or explicitly not, never silently one or
  the other. Exit status is unchanged either way.

- **A FAILED backup has a way out** (#662). `retry` on an artifact whose own
  durable local copy occupies its final name used to loop forever against
  FR-12's collision refusal, and no other verb accepted the artifact. `rbm
  retry` now completes that ingestion in place: it compares the copy with the
  remote object, and on a byte-identical match records the measurement and
  returns the artifact to the durable state it held, through the same
  reinstatement path an operator's own `quarantine reinstate` takes (so the
  remote source is preserved from then on, permanently). `quarantine
  revalidate` and `quarantine reinstate` now also accept a FAILED artifact, so
  `retry` can no longer take away a recovery option and leave the artifact as
  stuck as before. The FR-12 collision refusal itself is unchanged.

- **A converged commit no longer certifies a damaged file, and no longer
  risks leaving an artifact without a recovery manifest to make that
  refusal** (#662, #663). Re-running `Commit` against an artifact already
  `COMMITTED` re-measures the file and rewrites the sidecar recovery
  manifest so a crash between the `COMMITTED` journal write and that write
  can never leave the manifest missing forever. When the file no longer
  matches the record, the manifest is now written from the record instead
  of from the disagreeing measurement — the record was itself measured
  from the file at commit time, so it is the trustworthy side of a
  convergence-time disagreement, not the file underneath it — and the
  disagreement is still reported as an error rather than being discarded.
  `measureCommitted`'s corrective re-hash also now refuses to pair a size
  from one read with a digest from a shorter one, instead of stamping
  `verification_class: "content"` on a hash that describes fewer bytes
  than the size beside it.
- **The browser offers a stuck backup a way out, on the backup it is actually
  stuck on** (#662). A backup's detail page had no control that reached any
  recovery verb, and the card added to answer that was gated on a combination
  no backend produces: a backup that failed an attempt carries no validation
  verdict at all, so the API reports it as `pending`, and a FAILED row is not
  quarantined either — so the card rendered for nothing real. It is gated on
  the lifecycle state now, which the API already reported and the client
  simply dropped. Pressing it re-enters the pipeline and the page re-reads the
  backup, because the verb answers before the pipeline has run: it says the
  request was accepted, never that the backup is recovered. A refused press
  carries the service's own words, what to do next, and the correlation id an
  operator can quote, instead of a bare error. Note that reaching this page at
  all still needs #677: the route matches one path segment and an artifact id
  has three.

- **The docked terminal no longer contradicts its own advice on screen**
  (#662). The suppression of a *"no rbm equivalent yet"* line printed under a
  remedy that names a command was applied to what Copy and Save produce and,
  separately, to what the panel draws — and only the first was ever exercised,
  so the window an operator reads could disagree with the file they exported
  from it. Both are now asserted against the rendered panel.

- **An artifact id can reach its own page** (#677). `App.tsx` declared
  `/backups/:artifactId`, one path segment, and a real artifact id is
  `source/set/name` — three. Nothing matched, the catch-all took it, and the
  operator landed on the Dashboard with no error, which left #662's own
  browser-side remedy — the Recovery card on the backup detail page —
  unreachable in a browser on every real deployment, even with #663 merged.
  The route takes three segments now and the page reassembles the id from
  them, exactly as `/sets/:source/:set` has done since #285, whose explanation
  was sitting four lines above the line that still had the bug. The URL is
  built by `artifactPath()`, escaping each part on its own, which is the half
  concatenation cannot do: a filename may contain a `#`, and a raw `#` in a
  path starts a fragment and takes the rest of the id out of the URL
  entirely. Nothing caught it because the fixture's artifact ids are
  slash-free, so every fixture-driven test navigated a URL a real id can never
  produce, and the detail page's own test declared the broken route itself.

- **A first configuration can be written on a host whose backup root does not
  exist yet** (#670). The seeded local destination's probe ran before the
  directory this deployment owns had been created, and `reach` correctly
  reported nothing there — so a create was refused although every connection
  step had just passed, and the three-container end-to-end rig could not stand
  a deployment up at all. The leaf is created first, with `Mkdir` and
  deliberately not `MkdirAll`: `reach` is the only check that catches a volume
  that never mounted, and building the whole chain would put it on the system
  disk and let every later check pass against an empty directory behind an
  empty mount point.

- **The enrolment link names the machine the deployment is on** (#688).
  `http://localhost:8080/enroll?token=…` is read on a different machine from
  the one it names: the install is run over SSH from a laptop, the browser is
  on the laptop, and `localhost` there is the laptop. The token is single use
  and dies in thirty minutes, so this was the first thing anybody did with a
  fresh install, failing. The hostname was not the fix and was already the
  default — it resolves on the box it names and, without mDNS or a record
  somebody set up, nowhere else. What works from anywhere on the LAN with
  nothing configured is an address, so the installer prints one: the local end
  of a socket connected to TEST-NET-1, which is the interface the default
  route would leave by, found without sending a packet. A loopback answer is
  treated as no answer, no default route falls back to the hostname rather
  than refusing, and `--public-base-url` still wins when it is given.

- **The add-a-destination wizard no longer prints a command that cannot be
  run** (#668). Its confirm step creates nothing — the single create happens
  in the configure step, once the probe passes — and it was echoing `medium
  add <id> --backend <backend>` at that moment anyway, advertising a write
  that does not happen with a flag the CLI does not take. The line is gone and
  its absence is stated where a reader looks for the command: a recorded gap
  with its reason beats a plausible line that fails on execution.

- **The configure wizard's echoed `medium add --type` names what the CLI
  wants** (#81). It passed the manifest's rclone backend where `--type` takes
  the registry key, so the one destination a fresh install has came out as
  `--type local`.

- **The installer stops telling operators that the version it installs cannot
  be proven** (#681). `preflight` and `install` both reported that 0.3.3 "is
  cut and not pushed" and that whatever the tag resolves to "goes in on the
  registry's word" — false since publication, from the one message whose whole
  job is to say whether the thing being installed can be proven, and the first
  thing a new operator meets. The carried pin and the recorded manifest are
  filled in together; one present and the other missing is exactly what the
  test beside them catches.

- **The reference page describes the product this release ships** (#664, #706,
  #707). Its four drift checks were green, which is the more interesting
  failure mode: EPIC I added no route and no command, so nothing structural
  was missing and everything substantive was. The Settings table still called
  `local` "the drive backups already land on and is always there", which is
  what a destinations list looked like when there could only ever be one of
  them; the list, the add flow and the manifest renderer now have sections of
  their own, with the costs of each control. The page's route map had also
  gone stale against #677's three-segment route, and two of its own
  enumerations against the sections added with it. Three more holes of the
  same shape closed with the install output (#714): the page said the
  installer has "six subcommands" and listed six, where `enroll-link` is the
  seventh — every drift check was green, because the flag table is held to the
  parser's options and the command table to the CLI's dispatch table and
  nothing had an opinion about subcommands, which is now checked in both
  directions *and* against the number in the heading; the exit-code table was
  missing `53`, under a sentence promising every refusal has its own code; and
  one in-site link pointed at a fragment that did not exist, which is now
  walked for every page.

- **The compliance documents name the application id this project declares**
  (#687). `privacy-policy.md`, `source-offer.md` and `support.md` said
  `com.iasbuilt.backupmanager` where `compliance.json` says
  `com.iasbuilt.rclonemanager`. The check that should have caught it only
  asked whether a phrase appeared somewhere in a document, and a stale id
  sitting beside nothing that contradicts it has nothing to disagree with; the
  new one reads every `com.iasbuilt.*` id a document names and refuses one
  that is not the declared id.

- **The web host introduces itself by the name everything else calls it**
  (#652). The image ships `/rbm-web`, every compose file runs it, the packaging
  manifest names it and the CLI's own exit-code table sends an operator to
  `rbm-web serve`, while the binary went on saying `backup-manager-web` in 37
  places, including `usage: backup-manager-web <command> [flags]`. So a fresh
  install answered `docker compose logs` with a name no compose file, no
  document and no other binary in the image still uses. The first-run
  enrolment notice — usually the first line a new deployment prints — had
  drifted further: it claimed the CLI's name for a line the CLI has never
  printed.

### Changed

- `rbm status` names the artifacts that need intervention and the commands
  that act on them, instead of only reporting that intervention is needed
  (#662).
- `rbm fetch --dry-run` marks an already-known object whose artifact a cycle
  will not attempt again, and says how many there are, instead of printing an
  empty plan that read exactly like a settled backup set (#662).
- `rbm retry` opens a transport, because completing an ingestion in place has
  to ask the remote for its hash.
- The Web UI's backup detail page offers a FAILED backup a **Recovery** control
  wired to the retry verb; the docked terminal no longer prints "no rbm
  equivalent yet" about an operation the same window has just handed the
  operator a command for (#662).
- The Web UI's storage destinations list can hand the **default** from one
  destination to another, and says what that costs before it does it (#671).
  Every destination now carries **Make default**; the one that holds the mark
  keeps the control and its **Remove**, both disabled, each pointing at the
  sentence that says why it is off and what would turn it back on — a control
  that is simply missing sends an operator hunting for a screen that does not
  exist. The confirmation names both halves of what one click does, because
  only one of them was asked for: the destination picked takes the mark and
  stops being removable, and the one that had it becomes removable. It also
  says what does not change — no tier is rewritten and no copy already
  written is moved or deleted. A destination nobody has proven cannot take
  the mark, and says so before the click rather than failing after it.
- Removing the local destination is now reachable from the browser once
  another destination holds the default (#670, #671). The list withheld
  **Remove** from any entry flagged `isLocal`, which stopped being a proxy
  for "not declared" when #670 made `local` a declared destination the engine
  removes like any other; the button now follows the engine's own rule, which
  is the default mark and nothing else.
- The machine-tier end-to-end case for #662 (`--case empty-record`) asserts the
  fix instead of the defect, and its own `--help` no longer tells operators it
  is red on purpose. It was written to fail and said so in four places,
  including the rendered help and the golden that is compared to it byte for
  byte; with #662 fixed, the first person to see it fail would have read that
  text and dismissed a regression as expected. It now requires the planted
  empty record to leave the copy at a durable restore point, the recorded fault
  to still reach an operator in words, and the file to be untouched. The
  dead-end walk it replaced is retained rather than deleted, and runs if
  reconciliation ever leaves the artifact outside a durable state.
- "Installing it" opens with the two commands that install the product, and
  both now carry the output they really produce, under an **Output** label so
  it cannot be mistaken for something to type (#683, #714). The enrolment link
  is highlighted in the block and annotated where it is: single use, thirty
  minutes, and an address that is this machine's own rather than `localhost`.
  Being honest about it took one extra step — the installer's success epilog
  does not carry the link at all, it prints the command that greps it out of
  the engine's log — so the page shows that command and its result rather than
  a line the installer never emits. The `--cli-only` block deliberately has no
  link, because there is no Web UI to enrol into, which is the whole point of
  the flag. None of those blocks is typed out any more: each one is rendered
  from the epilog the installer itself prints and compared line for line, and
  the enrolment sentence inside them is read out of the engine's own format
  string rather than a constant in a test, so a page still showing
  `http://localhost:8080/enroll?token=…` fails a check instead of waiting to
  be noticed. Three of the hand-typed blocks were already wrong when they
  landed — an elided Compose argv and an invented line continuation — and an
  abbreviation under an **Output** label is read character by character
  against a real screen, which is the whole reason the page shows output
  rather than prose.
- Sixty-odd shell scripts begin becoming one Python package, `scripts/rcmtools`
  (#672). The `perf` domain is ported in full and no longer needs `jq` to read
  a baseline record; the two entry points a gate test fabricates a stand-in for
  keep real shims at their old paths, and the one script that is sourced rather
  than executed is untouched, so there is no third copy of the host-id rule.
  The port closed a collision it found on the way: a malformed baseline record
  made `jq`'s exit status the script's own, which this repository elsewhere
  reserves for "this machine could not perform the proof" — a corrupt
  committed record is a real defect and has to fail the gate rather than be
  ledgered as incomplete.
- The product end-to-end gate stands up the three-container rig and drives it
  over `RM_BASE_URL`, instead of starting the UI workspace's own dev server
  against a mock API (#687). The tests-repo pin moves with it, read off that
  repository's merged `main` rather than a branch, and the eight browser cases
  that were required to fail on the route #677 fixed now run for real: 191
  passed, 69 skipped, nothing failed and nothing expected-failed.

### Removed

- `rclone_backend` is off the `/api/v1` contract (#81). EPIC I's backend
  catalogue put it on the wire twice, as a property of a manifest and as the
  whole of an unregistered entry, and the contract-drift gate was right to
  refuse it: what a public schema may not be made of is the implementation
  vocabulary a client codes against. The manifest no longer carries it, and an
  unregistered entry names its `transport`, which is a protocol and can
  therefore be spelled at product level at all. Prose is still allowed to tell
  the truth about rclone — a description may say what a thing is not — but a
  property may not be named for it. Internally the manifest keeps its own
  `RcloneBackend`, which is what the engine dials.
- `RemoveStorageMedium`'s local-specific refusal, and the browser's `isLocal`
  gate on **Remove** (#670, #671). Both said the local drive "was never
  declared", which stopped being true.
- The first-run page's opening note about the development mock, its
  "Re-shooting these" section, and the reference page's screenshot-provenance
  callout (#675, #678, #706). All three were true and written for the wrong
  audience — the first thing a reader met on a page they came to in order to
  look something up, or instructions for somebody with the repository checked
  out. The footer of every page still carries the provenance claim, the
  capture scripts keep their own header comments, and the notes that survive
  are the ones where believing the data is real would actually mislead: the
  dashboard that shows five backup sets where a real first run has one, the
  host fingerprint that is not yours, and the line saying no screenshot here
  is evidence about a running engine.
- `scripts/perf/capture-baseline.sh` (#672), replaced by `python3
  scripts/rcmtools/perf/capture_baseline.py`. Nothing fabricates a stand-in
  for it and no gate calls it — it is a by-hand driver on a dedicated
  benchmark host — and every caller and both documents now name the Python
  one.

### What you may need to do

Nothing is required, and nothing already written has to be changed. If a backup
set is already stuck in the state #662 describes — `rbm status` reporting
FAILING with a FAILED artifact whose local copy is intact — run `rbm retry
<artifact-id>`; it now resolves that state instead of looping. Note that an
artifact recovered this way keeps its remote source for good: this manager
never deletes the original of a backup it trusted on evidence rather than
re-fetched.

EPIC I asks nothing of a deployment that already exists. A `config.yaml`
written before this release never declared `local`, and it still loads and
resolves exactly as it always did: the local hard drive is synthesised on
every read rather than materialised, the same decision the effective default
destination and a tier's effective medium already make, so there is no
migration and no rewrite nobody asked for. Only a fresh install writes `local`
out as a declared destination, and the reserved id stays shut on every
operator-facing path — the API, the CLI and the wizard cannot manufacture it.

If you install with `--cli-only`, nothing is running when the installer
finishes, and that is what the flag means: use the `rbm` wrapper it stages at
`<prefix>/bin/rbm` to write the first configuration, and the deployment starts
serving from there. There is no Web UI on such a host and therefore no
enrolment link to open.

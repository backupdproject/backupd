# Privacy disclosure

Written to be pasted into a store's own privacy field, and short because there is very
little to disclose.

## Does this application collect personal data?

No. retnd collects no personal data of any kind, from the administrator or from
anyone else. It has no account system of ours, no registration, no licence check and no
account linking.

The one contact detail it holds is one the administrator gives to themselves: a recovery
email address, typed into the setup form, used only to mail that administrator a
password-reset link for their own installation. It is stored on the NAS, it is sent only
to the mail server the administrator nominated, and nobody else ever receives it.

## Does it send telemetry?

No. There is no usage reporting, no analytics, no crash reporting and no version-check
call. There is no opt-out, because there is nothing switched on to opt out of, and there is
no opt-in either: this release ships no telemetry mechanism at all, which is what
§45.5 of the design requires and what the packaging preflight verifies against the shipped
artifact rather than against this sentence. A telemetry endpoint appearing in any packaged
file fails the build.

## What does it connect to?

Two classes of destination, and the administrator chooses every member of both: the
SFTP sources configured as backup sources, and — if they configured account recovery —
the SMTP server they nominated for the account's own mail. Nothing else. The application
makes no outbound connection that an administrator did not configure, and every host it
will ever reach is visible in its own configuration.

The published URLs in this package's metadata point at the project's own source repository,
its issue tracker and its container registry. Those are for a human reading the listing;
the application itself never fetches them.

## What data does it store, and where?

On the NAS, in locations the administrator picks at install time:

- The catalog: a SQLite database recording each artifact's source, name, path, state,
  hashes and timestamps. It holds metadata about backup artifacts, not their contents.
- The retained artifacts themselves, in the backup root the administrator chose, which is
  kept a separate directory from the application's own state.
- Authentication state: one local administrator record holding an Argon2id password hash.
  Never a plaintext password.
- The account's recovery details: the administrator's recovery email address, and the
  SMTP settings used to reach it. The SMTP password is held as a secret reference rather
  than a value, so the record itself never contains it and no log or API response carries
  it.
- Credentials for the configured sources: an SSH private key and a pinned `known_hosts`
  file, both supplied by the administrator, both mounted read-only, and neither ever
  written into the package, the image or any log.

## Does any of it leave the NAS?

Only the account's own mail, and only where the administrator asked for it: an enrollment
confirmation, a password-reset link, or a test message, sent to their own address through
their own mail server. Nothing else. No backup artifact, no catalog, no configuration and
no credential is uploaded, mirrored, synchronised or reported anywhere; all of it stays on
the storage the administrator nominated.

## Data retention and deletion

Retained artifacts are deleted only by the retention policy the administrator configured,
and only after a plan they confirmed. Removing the application does not delete retained
artifacts: they live outside the application's own state on purpose, and the removal
procedure for every target states that explicitly.

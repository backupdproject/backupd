/**
 * Everything an operator can configure from the UI, plus what this build
 * and this platform are.
 *
 * Most of the page's history is subtraction. Several cards here used to be
 * controls that rendered a value and saved nowhere: a polling interval in
 * the wrong unit, a log level with no config key behind it, storage
 * thresholds with no handler. Each was either removed or replaced with the
 * real thing, and the notes in the JSX say which, because a decorative
 * control is worse than a missing one. It teaches an operator that they
 * have configured something.
 *
 * What is left splits three ways: cards that own a real config block and
 * write it, capability copy that reports what this platform can do without
 * ever claiming more, and build information. The version reads the shared
 * node rather than fetching again, so this page and the compatibility
 * banner above it cannot name two different versions.
 */
import { useId, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { usePlatform } from "@shared/platform/PlatformContext";
import type { RecoverySettings } from "@shared/api/contracts";
import { useAsync } from "@shared/hooks/useAsync";
import { notificationCopy } from "@shared/platform/capabilities";
import { useCausl } from "@shared/state/graph";
import { configuredNode, publishRecoverySettings, versionNode } from "@shared/state/appNodes";
import { Banner } from "@shared/components/Banner";
import { PageHeader } from "@shared/components/PageHeader";
import { PlatformBadge } from "@shared/components/PlatformBadge";
import { ErrorState } from "@shared/components/EmptyState";
import { apiErrorOf, describeFailure } from "@shared/api/failure";
import type { OperatorFailure } from "@shared/api/failure";
import { RetentionPolicyCard } from "@shared/pages/RetentionPolicyCard";
import { CapacityCard } from "@shared/pages/CapacityCard";
import { StorageDestinationsCard } from "@shared/pages/StorageDestinationsCard";
import { HelpField } from "@shared/components/FieldHelp";
import { PasswordInput } from "@shared/components/PasswordInput";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import {
  DEFAULT_SMTP,
  RecoveryAttention,
  SmtpFields,
  looksLikeEmail,
  smtpFieldsComplete,
  smtpInput
} from "@shared/components/RecoveryFields";
import type { SmtpFieldValues } from "@shared/components/RecoveryFields";
import { useTooltipsEnabled } from "@shared/hooks/useTooltips";
import { setTooltipsEnabled } from "@shared/state/tooltipNodes";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import type { TooltipId } from "@shared/tooltips/tooltips";

export function SettingsPage({ readOnly }: { readOnly: boolean }) {
  const navigate = useNavigate();
  const { bridge, capabilityCopy } = usePlatform();
  // Reads the same shared node App.tsx already fetches once for its own
  // readOnly derivation (#103), instead of running a second independent
  // getVersion() here — the two could otherwise briefly disagree about
  // which version is current.
  const version = useCausl(versionNode);
  const configured = useCausl(configuredNode);
  // #829's toggle reads the PREFERENCE, not `useTooltipsVisible()`: a
  // checkbox has to show what is stored, and a surface that suppressed
  // tooltips locally would otherwise draw this control as "off" while the
  // stored answer was on.
  const tooltipsEnabled = useTooltipsEnabled();

  // The two cards below read the same destinations twice, from two
  // endpoints, into two independent fetches: the destinations card lists
  // them, and the retention card needs them for its per-tier picker and
  // for which one a NEW tier starts on. Neither can reload the other, so
  // the page holds the one fact they have to agree about (#634).
  //
  // A revision rather than lifting the fetch itself, deliberately. The
  // two reads answer different questions of different endpoints, and
  // hoisting them into one would put a settings fetch in a page that has
  // no other use for it and give the destinations card a shape nothing
  // else on this page wants. What actually has to travel between them is
  // one bit, "what you read is now out of date", and this is that bit.
  const [destinationsRevision, setDestinationsRevision] = useState(0);

  return (
    <>
      <PageHeader
        title="Settings"
        tip="nav.settings"
        subtitle="Service behaviour, platform integration and build information"
      />

      <div style={{ display: "grid", gridTemplateColumns: "minmax(0, 1.3fr) minmax(0, 1fr)", gap: 14, alignItems: "start" }}>
        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          {/* Issue #299: this used to be a "Service" card holding two
              decorative controls, "Polling interval" (15/30/60 SECONDS)
              and "Log level" — both `defaultValue`, no `onChange`, nothing
              saved. Removed rather than wired: the real `poll_interval`
              config key is a duration (minutes, defaults to 15m) so this
              control was even answering the wrong unit, and there is no
              log-level concept anywhere in config.Config to wire the
              second one to. `poll_interval` is still real and still
              editable — just directly in config.yaml, not here. */}

          {/* Issue #286: the storage cap and its two FR-21 thresholds
              used to sit here as three decorative controls that saved
              nowhere ("Storage warning threshold"/"Storage critical
              threshold" carried defaultValue="80%"/"92%" and no handler
              at all). They are now the real thing, reading from and
              writing to internal/config's capacity block, same as
              RetentionPolicyCard below did for retention under #140. */}
          <CapacityCard readOnly={readOnly} />

          {/* B3.7 (#140). This used to be a static row of badges reading
              "7 daily / 13 weekly / 12 monthly / protect known-good": a
              picture of a policy, wired to nothing, and wrong twice over
              once #156 generalized the chain (13 weekly was never a
              default, and the chain is not three fixed tiers). It is now
              the real thing, read from and written to the running
              config. */}
          <RetentionPolicyCard readOnly={readOnly} destinationsRevision={destinationsRevision} />

          {/* G2.2 (#594). Beside the retention plan deliberately: a
              retention tier's Medium field is a picker over exactly this
              list, so the place where a destination is declared and the
              place where a tier is pointed at one belong on the same
              screen. Before this card there was no such place at all, and
              declaring a destination meant editing config.yaml by hand,
              which is the one thing EPIC G is about not having to do. */}
          <StorageDestinationsCard
            readOnly={readOnly}
            onChanged={() => setDestinationsRevision((n) => n + 1)}
          />

          <section className="card">
            <div className="card__header">
              <InfoTooltip id="settings.notifications">
                <h2 className="eyebrow">Notifications</h2>
              </InfoTooltip>
            </div>
            <div className="card__body" style={{ display: "flex", flexDirection: "column", gap: 12 }}>
              {/* Honest capability copy — never present a fallback as native (§22). */}
              <Banner tone="info" style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                <span aria-hidden="true" style={{ color: "var(--text-3)" }}>i</span>
                <span>{notificationCopy(bridge.capabilities(), bridge.name)}</span>
              </Banner>
              {/* Issue #299: this row used to present
                  "https://hooks.internal/bm" as a live webhook delivery
                  target. config.Alerts' own doc comment says where an
                  alert goes is deliberately not configurable in this
                  product — there was never a URL for this row to
                  validate, save, or actually deliver to. Removed rather
                  than wired, since wiring it would mean reversing that
                  design decision, which this issue does not do. This was
                  the most urgent item in #299: not a control that merely
                  failed to save, a specific fake fact stated as true. */}
            </div>
          </section>

          {/* Issue #829. Deliberately NOT disabled by `readOnly`: §38
              disables management actions because this build and the
              service disagree about the /api/v1 contract, and this
              preference is not a management action. It is stored in this
              browser, it is never sent anywhere, and the dialog that
              offers the opt-out points every operator who takes it at
              exactly this control — a version mismatch leaving it
              unusable would mean an operator who turned tooltips off
              having no way to turn them back on. */}
          <section className="card">
            <div className="card__header">
              <InfoTooltip id="settings.interface">
                <h2 className="eyebrow">Interface</h2>
              </InfoTooltip>
            </div>
            <div className="card__body" style={{ display: "flex", flexDirection: "column", gap: 12 }}>
              <InfoTooltip id="settings.tooltips-toggle" block>
                <label
                  style={{
                    display: "flex", alignItems: "center", gap: 10, padding: "11px 13px",
                    border: "1px solid var(--border)", borderRadius: 7, fontSize: 13,
                    cursor: "pointer"
                  }}
                >
                  <input
                    type="checkbox"
                    checked={tooltipsEnabled}
                    style={{ accentColor: "var(--accent)" }}
                    onChange={(e) => setTooltipsEnabled(e.target.checked)}
                  />
                  <span style={{ flex: 1 }}>Show tooltips on hover</span>
                  <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
                    {tooltipsEnabled ? "on" : "off"}
                  </span>
                </label>
              </InfoTooltip>
              <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "74ch" }}>
                Tooltips explain what a field does and what it changes. With this off,
                no tooltip appears on hover anywhere in this interface. The setting is
                stored in this browser only, so it does not affect anyone else using
                this instance.
              </p>
            </div>
          </section>

          <ChangePasswordCard readOnly={readOnly} />

          {/* Issue #830. Beside the password card deliberately: the two
              are one subject from an operator's side, since the recovery
              endpoint below is what stands in for the password above when
              it is lost. */}
          <AccountRecoveryCard readOnly={readOnly} />

          {/* #275: on an instance with no configuration there is no
              storage location, so there is nothing this card could
              truthfully claim was found in one. */}
          {configured === false ? null : (
            <section className="card" style={{ borderColor: "var(--warn)" }}>
              <div className="card__header">
                <InfoTooltip id="settings.catalog-recovery">
                  <h2 className="eyebrow">Catalog recovery</h2>
                </InfoTooltip>
              </div>
              <div className="card__body" style={{ display: "flex", flexDirection: "column", gap: 12 }}>
                <div style={{ fontSize: 13.5, fontWeight: 600 }}>Existing backup data detected</div>
                <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "74ch" }}>
                  Backup files were found in the configured storage location, but they are
                  not currently present in the Backupd catalog. Scanning is
                  read-only — no files will be deleted.
                </p>
                <div>
                  <InfoTooltip id="settings.catalog-scan">
                    <button className="btn btn--primary" disabled={readOnly} onClick={() => navigate("/catalog-recovery")}>
                      Scan backup storage
                    </button>
                  </InfoTooltip>
                </div>
              </div>
            </section>
          )}
        </div>

        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          <section className="card">
            <div className="card__header">
              <InfoTooltip id="settings.platform">
                <h2 className="eyebrow">Platform</h2>
              </InfoTooltip>
            </div>
            <div className="card__body">
              <PlatformBadge />
              <InfoTooltip id="settings.capabilities" block>
                <div className="eyebrow" style={{ fontSize: 10.5, margin: "16px 0 8px" }}>Capabilities</div>
              </InfoTooltip>
              <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
                {capabilityCopy.map((c) => (
                  <InfoTooltip key={c.label} id="settings.capability" block>
                    <div style={{ display: "flex", alignItems: "center", gap: 9, fontSize: "var(--text-sm)" }}>
                      <span
                        aria-hidden="true"
                        style={{ width: 12, textAlign: "center", color: c.supported ? "var(--ok)" : "var(--text-3)" }}
                      >
                        {c.supported ? "\u2713" : "\u2013"}
                      </span>
                      <span style={{ flex: 1 }}>{c.label}</span>
                      <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
                        {c.detail}
                      </span>
                    </div>
                  </InfoTooltip>
                ))}
              </div>
            </div>
          </section>

          <section className="card">
            <div className="card__header">
              <InfoTooltip id="settings.system-information">
                <h2 className="eyebrow">System information</h2>
              </InfoTooltip>
            </div>
            <div className="card__body">
              {version.data ? (
                <dl style={{ margin: 0, display: "grid", gridTemplateColumns: "1fr auto", gap: "10px 14px", fontSize: "var(--text-sm)" }}>
                  <Row label="Service version" tip="settings.version.service" value={version.data.service} />
                  <Row label="API contract" tip="settings.version.api" value={version.data.api} />
                  <Row label="Backup engine" tip="settings.version.engine" value={version.data.engine} />
                  <Row label="Go toolchain" tip="settings.version.go" value={version.data.goVersion} />
                  <Row
                    label="Configuration revision"
                    tip="settings.version.config-revision"
                    value={version.data.configRevision}
                  />
                  <Row
                    label="Platform adapter"
                    tip="settings.version.adapter"
                    value={bridge.deployment.adapterVersion}
                  />
                  <Row label="Build commit" tip="settings.version.build-commit" value={version.data.buildCommit} />
                </dl>
              ) : version.error ? (
                // versionNode's one fetch is owned by App.tsx, not this page,
                // so there is nothing here to retry (mirrors BackupSetsPage's
                // operations.error inline notice, same reasoning).
                <Banner
                  tone="danger"
                  style={{ fontSize: "var(--text-sm)" }}
                  // The sentence names the failure it is reporting, so the
                  // dismissal is scoped to THAT failure: a different one
                  // puts the banner back without waiting for a remount
                  // (#620).
                  dismissKey={version.error.message}
                >
                  {"Version information is unavailable (" + version.error.message + ") — details below may be out of date."}
                </Banner>
              ) : (
                <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>Loading version information…</p>
              )}
            </div>
          </section>
        </div>
      </div>
    </>
  );
}

/** One row of the build-information list.
 *
 *  `tip` is required rather than optional (issue #834): every one of these
 *  values is a version string or a hash, which is exactly the kind of
 *  thing an operator is asked to quote and given no way to interpret. A
 *  row that cannot say what its own value means should not be here. */
function Row({ label, tip, value }: { label: string; tip: TooltipId; value: string }) {
  return (
    <>
      <dt style={{ color: "var(--text-2)" }}>
        {/* The term keeps an element of its own, so something on the page
            still reads exactly "Build commit" for anything looking for it,
            and the icon beside it stays out of that text. */}
        <span>{label}</span>
        <InfoTooltip id={tip} />
      </dt>
      <dd className="mono" style={{ margin: 0 }}>{value}</dd>
    </>
  );
}

const MIN_PASSWORD_LENGTH = 12;

/** §13A password rotation (issue #128) - reuses EnrollmentPage.tsx's own
 *  validation shape (minimum length, confirm-match) since it is the same
 *  "pick a new password" moment, just for an existing account instead of
 *  a first-run one. A successful rotation signs out every OTHER session
 *  for this administrator (apps/common/auth/local's handleRotatePassword);
 *  this tab's own session is reissued, so no redirect/sign-out happens
 *  here.
 *
 *  #274: a rejected rotation used to read as a wrong current password
 *  whatever the service said, under the literal `cid_rotate_password`. A
 *  rate-limited address and a session that expired while this tab sat open
 *  are both reachable here and neither is a wrong password. */
function describeRotationFailure(e: unknown): OperatorFailure {
  const api = apiErrorOf(e);
  if (api?.code === "UNAUTHENTICATED") {
    // handleRotatePassword answers UNAUTHENTICATED both for a wrong current
    // password and for a session that is no longer valid, and does not say
    // which. Naming both beats naming the wrong one.
    return {
      message: "That current password was not accepted.",
      remediation: "If it is definitely right, this tab's session may have expired: reload the page, sign in again and retry.",
      correlationId: api.correlationId
    };
  }
  return describeFailure(e, "The password was not changed.");
}

function ChangePasswordCard({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<OperatorFailure | null>(null);
  const [success, setSuccess] = useState(false);

  const tooShort = next.length > 0 && next.length < MIN_PASSWORD_LENGTH;
  const mismatch = confirm.length > 0 && confirm !== next;
  // Referenced rather than left inside the field's <label> to be swept into
  // its name; see the matching note on EnrollmentPage and PasswordInput's
  // doc for why the name no longer picks them up.
  const tooShortId = useId();
  const mismatchId = useId();
  const valid = current.length > 0 && next.length >= MIN_PASSWORD_LENGTH && confirm === next;

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!valid || readOnly) return;
    setBusy(true);
    setFailure(null);
    setSuccess(false);
    api
      .rotatePassword(current, next)
      .then(() => {
        setSuccess(true);
        setCurrent("");
        setNext("");
        setConfirm("");
      })
      .catch((e: unknown) => setFailure(describeRotationFailure(e)))
      .finally(() => setBusy(false));
  };

  return (
    <section className="card">
      <div className="card__header">
        <InfoTooltip id="settings.password">
          <h2 className="eyebrow">Administrator password</h2>
        </InfoTooltip>
      </div>
      <div className="card__body">
        <form onSubmit={submit} style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          <HelpField label="Current password" help={FIELD_HELP.currentPassword}>
            {(helpId, field) => (
              <PasswordInput
                label={field.label}
                labelledBy={field.id}
                autoComplete="current-password"
                describedBy={helpId}
                value={current}
                onChange={setCurrent}
                disabled={readOnly}
                required
              />
            )}
          </HelpField>
          <HelpField label="New password" help={FIELD_HELP.newPassword}>
            {(helpId, field) => (
              <>
                <PasswordInput
                  label={field.label}
                  labelledBy={field.id}
                  autoComplete="new-password"
                  describedBy={tooShort ? helpId + " " + tooShortId : helpId}
                  value={next}
                  onChange={setNext}
                  disabled={readOnly}
                  required
                />
                {tooShort ? (
                  <span id={tooShortId} style={{ fontSize: "var(--text-sm)", color: "var(--danger)" }}>
                    {"Minimum " + MIN_PASSWORD_LENGTH + " characters."}
                  </span>
                ) : null}
              </>
            )}
          </HelpField>
          <HelpField label="Confirm new password" help={FIELD_HELP.confirmNewPassword}>
            {(helpId, field) => (
              <>
                <PasswordInput
                  label={field.label}
                  labelledBy={field.id}
                  autoComplete="new-password"
                  describedBy={mismatch ? helpId + " " + mismatchId : helpId}
                  value={confirm}
                  onChange={setConfirm}
                  disabled={readOnly}
                  required
                />
                {mismatch ? (
                  <span id={mismatchId} style={{ fontSize: "var(--text-sm)", color: "var(--danger)" }}>
                    Passwords do not match.
                  </span>
                ) : null}
              </>
            )}
          </HelpField>
          {success ? (
            <Banner tone="ok" style={{ fontSize: "var(--text-sm)" }}>
              Password changed. Other signed-in sessions have been signed out.
            </Banner>
          ) : null}
          {failure ? (
            <ErrorState
              message={failure.message}
              remediation={failure.remediation}
              correlationId={failure.correlationId}
            />
          ) : null}
          <div>
            <InfoTooltip id="settings.change-password">
              <button
                className="btn btn--primary"
                type="submit"
                disabled={!valid || busy || readOnly}
                style={{ height: 40 }}
              >
                {busy ? "Changing…" : "Change password"}
              </button>
            </InfoTooltip>
          </div>
        </form>
      </div>
    </section>
  );
}

/**
 * Issue #830: the account's way back in, after enrolment has set it up.
 *
 * The card exists because a mail endpoint that worked on the day it was
 * configured is not a mail endpoint that works: providers withdraw
 * credentials, ports change, a relay is decommissioned, and the moment
 * this is needed is the one moment nobody can find out by trying. So it
 * offers both halves — edit the configuration, and prove it still sends —
 * rather than only the first.
 *
 * Two properties of the write are worth knowing before reading the
 * handler. A blank SMTP password means KEEP the stored one, because no
 * read of this configuration can ever return a password to prefill the
 * field with (SmtpSettingsView), so blank is the only thing an unedited
 * field could be. And a changed recovery address is re-verified by the
 * service as part of the same request: it sends a confirmation over the
 * endpoint this request establishes and refuses the whole update if that
 * send fails, so `recoveryEmailConfirmed` coming back true is a message
 * having been delivered rather than a request having succeeded.
 */
function AccountRecoveryCard({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const recovery = useAsync<RecoverySettings>(() => api.getRecoverySettings(), [api]);

  return (
    <section className="card">
      <div className="card__header"><h2 className="eyebrow">Account recovery</h2></div>
      <div className="card__body">
        {recovery.error ? (
          <ErrorState
            message={recovery.error.message}
            remediation="The recovery settings could not be read, so they cannot be edited here yet."
            correlationId={recovery.error.correlationId}
            onRetry={recovery.reload}
          />
        ) : recovery.data ? (
          <RecoveryEditor loaded={recovery.data} readOnly={readOnly} />
        ) : (
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
            Loading recovery settings…
          </p>
        )}
      </div>
    </section>
  );
}

/** A refusal from either recovery write, in the words that separate them.
 *  `sendFailed` is what was NOT done when the mail server refused, and it
 *  differs between the two callers: a failed save changed nothing, while a
 *  failed test was never going to change anything. Quoting the server's
 *  own message is the point of both — "connection refused" and "535
 *  authentication failed" are the two different problems this card
 *  exists to surface, and no sentence written here could tell them
 *  apart. */
function describeRecoveryFailure(
  e: unknown,
  { fallback, sendFailed }: { fallback: string; sendFailed: string }
): OperatorFailure {
  const api = apiErrorOf(e);
  if (api?.code === "SMTP_SEND_FAILED") {
    return {
      message: sendFailed,
      remediation: "The mail server said: " + api.message,
      correlationId: api.correlationId
    };
  }
  if (api?.code === "INVALID_EMAIL") {
    return {
      message: "That address was not accepted as an email address.",
      remediation:
        "Backupd checks the recovery address and the From address against the mail standard rather than against a rough pattern. Check both for a missing domain, a stray space or a trailing comma.",
      correlationId: api.correlationId
    };
  }
  if (api?.code === "UNAUTHENTICATED") {
    // The one refusal on this card that is not about mail. It covers a
    // wrong password and a session that has since lapsed, because the
    // service deliberately does not distinguish them, and saying so is
    // more useful than picking one.
    return {
      message: "That password was not accepted, so nothing was changed.",
      remediation:
        "Re-type the administrator password. If it is definitely right, the session has expired instead — sign in again and repeat the change.",
      correlationId: api.correlationId
    };
  }
  return describeFailure(e, fallback);
}

function RecoveryEditor({ loaded, readOnly }: { loaded: RecoverySettings; readOnly: boolean }) {
  const api = useApi();
  // What the service last told us it holds. Replaced by the answer to a
  // save rather than by a guess at it, which is what makes the confirmed
  // badge below report a delivered message instead of a successful
  // request.
  const [current, setCurrent] = useState(loaded);
  const [email, setEmail] = useState(loaded.recoveryEmail);
  const [smtp, setSmtp] = useState<SmtpFieldValues>(() => smtpFieldsOf(loaded.smtp));
  // The re-authentication this write requires (#830 security review).
  // Held here rather than in the SMTP block because it is not part of
  // the configuration at all: it proves who is changing it, and it is
  // cleared the moment the change lands.
  const [password, setPassword] = useState("");
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [saved, setSaved] = useState<string | null>(null);
  const [tested, setTested] = useState<string | null>(null);
  const [failure, setFailure] = useState<OperatorFailure | null>(null);

  const badEmailId = useId();
  const badEmail = email.length > 0 && !looksLikeEmail(email);
  const valid = looksLikeEmail(email) && smtpFieldsComplete(smtp) && password.length > 0;

  const save = (e: React.FormEvent) => {
    e.preventDefault();
    if (!valid || readOnly) return;
    const addressChanged = email.trim() !== current.recoveryEmail;
    setSaving(true);
    setFailure(null);
    setSaved(null);
    setTested(null);
    api
      .updateRecoverySettings({ currentPassword: password, recoveryEmail: email.trim(), smtp: smtpInput(smtp) })
      .then((next) => {
        // Re-rendered from the answer, including the password field, which
        // goes back to blank: whatever was typed is stored now and there
        // is nothing to show in its place.
        setCurrent(next);
        setEmail(next.recoveryEmail);
        setSmtp(smtpFieldsOf(next.smtp));
        // The administrator's own password is not kept for a second
        // save: it is a credential, this form has no reason to hold one
        // after the write it authorised, and re-typing it is the point
        // of asking.
        setPassword("");
        // And published, because this answer is also what the unverified
        // banner above every page is drawn from (#830 §§8-9). A changed
        // address comes back UNVERIFIED - a verification link was just
        // mailed to it and nobody has opened it - so without this the
        // banner would keep reporting the previous address's proof until
        // the next full page load.
        publishRecoverySettings(next);
        setSaved(
          addressChanged
            ? "Saved. A verification link has been delivered to " + next.recoveryEmail + "."
            : "Saved."
        );
      })
      .catch((e: unknown) =>
        setFailure(
          describeRecoveryFailure(e, {
            fallback: "The recovery settings were not saved.",
            sendFailed: "The confirmation email could not be sent, so nothing was saved."
          })
        )
      )
      .finally(() => setSaving(false));
  };

  const sendTest = () => {
    if (readOnly) return;
    setTesting(true);
    setFailure(null);
    setSaved(null);
    setTested(null);
    api
      .sendRecoveryTestEmail()
      .then(() => setTested("Test message sent to " + current.recoveryEmail + "."))
      .catch((e: unknown) =>
        setFailure(
          describeRecoveryFailure(e, {
            fallback: "The test message could not be sent.",
            sendFailed: "The mail server refused the test message, so recovery mail would not arrive either."
          })
        )
      )
      .finally(() => setTesting(false));
  };

  return (
    <form onSubmit={save} style={{ display: "flex", flexDirection: "column", gap: 14 }}>
      <RecoveryAttention />
      {current.smtp === null ? (
        // A real state, not an empty form: `auth create-admin` leaves
        // recovery optional, so a headlessly provisioned administrator has
        // no endpoint at all and no way back until one is filled in here.
        <Banner tone="warn" dismissible={false} style={{ fontSize: "var(--text-sm)" }}>
          No SMTP endpoint is configured on this instance, so no reset link can
          be sent. Until one is saved below, a forgotten password is recovered
          at the host or not at all.
        </Banner>
      ) : null}
      <HelpField label="Recovery email" help={FIELD_HELP.recoveryEmail}>
        {/* Named by its label id rather than by the label's text, the same
            way the wizard's copy of this field is: the validation message
            below sits inside the label and would otherwise be swept into
            the field's own name. */}
        {(helpId, field) => (
          <>
            <input
              className="input input--mono"
              type="email"
              aria-labelledby={field.id}
              aria-describedby={badEmail ? helpId + " " + badEmailId : helpId}
              autoComplete="email"
              disabled={readOnly}
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              required
            />
            {badEmail ? (
              <span id={badEmailId} style={{ fontSize: "var(--text-sm)", color: "var(--danger)" }}>
                Enter an email address, such as ops@example.com.
              </span>
            ) : null}
          </>
        )}
      </HelpField>
      {/* Typed and reachable are different facts, and only the second one
          means recovery works, so the card reports which it has. */}
      <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)" }}>
        {current.recoveryEmailConfirmed
          ? "A confirmation message has been delivered to " + current.recoveryEmail + ", so this address is known to be reachable."
          : "No message has reached " + (current.recoveryEmail || "this address") + " yet, so it is not known to be reachable. Saving sends a confirmation to it."}
      </p>
      <SmtpFields
        values={smtp}
        onChange={(patch) => setSmtp((currentValues) => ({ ...currentValues, ...patch }))}
        disabled={readOnly}
        passwordAutoComplete="off"
        passwordNote={
          <span style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
            {current.smtp?.passwordSet
              ? "A password is stored. Leave this blank to keep it, or type a new one to replace it."
              : "No password is stored for this endpoint yet."}
          </span>
        }
      />
      {/* Last, immediately above the button it authorises, and only on
          the save path: the test send changes nothing and is not gated
          by it. Whoever controls this address and this endpoint controls
          where a password reset link is delivered, so this write asks
          for the password itself rather than accepting a session cookie
          - the same re-authentication the password-change card above
          performs, for a change of the same weight. */}
      <HelpField label="Administrator password" help={FIELD_HELP.recoveryCurrentPassword}>
        {(helpId, field) => (
          <PasswordInput
            label={field.label}
            labelledBy={field.id}
            autoComplete="current-password"
            describedBy={helpId}
            value={password}
            onChange={setPassword}
            disabled={readOnly}
            required
          />
        )}
      </HelpField>
      {saved ? (
        <Banner tone="ok" style={{ fontSize: "var(--text-sm)" }}>{saved}</Banner>
      ) : null}
      {tested ? (
        <Banner tone="ok" style={{ fontSize: "var(--text-sm)" }}>{tested}</Banner>
      ) : null}
      {failure ? (
        <ErrorState
          message={failure.message}
          remediation={failure.remediation}
          correlationId={failure.correlationId}
          detail={failure.detail}
        />
      ) : null}
      <div style={{ display: "flex", gap: 10 }}>
        <button
          className="btn btn--primary"
          type="submit"
          disabled={!valid || saving || readOnly}
          style={{ height: 40 }}
        >
          {saving ? "Saving…" : "Save recovery settings"}
        </button>
        {/* Sends over what is STORED, not over what is in the form, which
            is why it is a plain button rather than a second submit: a test
            that quietly saved the fields first would report on a
            configuration the operator had not agreed to keep. */}
        <button
          className="btn"
          type="button"
          onClick={sendTest}
          disabled={testing || readOnly || current.smtp === null}
          style={{ height: 40 }}
        >
          {testing ? "Sending…" : "Send test email"}
        </button>
      </div>
    </form>
  );
}

/** The stored endpoint as this form holds it: every field except the
 *  password, which no read returns and which therefore starts blank,
 *  meaning "keep whatever is stored". An instance with no endpoint gets
 *  the same defaults the enrolment wizard starts from. */
function smtpFieldsOf(view: RecoverySettings["smtp"]): SmtpFieldValues {
  if (view === null) return DEFAULT_SMTP;
  return {
    host: view.host,
    port: String(view.port),
    security: view.security,
    username: view.username,
    password: "",
    from: view.from
  };
}

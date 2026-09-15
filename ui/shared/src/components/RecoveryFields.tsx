/**
 * Issue #830: the recovery block, drawn once for the two screens that
 * collect it.
 *
 * First-run enrolment and Settings' "Account recovery" card ask for the
 * same six SMTP values and carry the same warning about what they are
 * for, and the two are not allowed to drift: an operator who configures
 * mail in the wizard and later corrects a port on Settings has to be
 * looking at the same fields with the same names, and the warning has to
 * say the same thing in both places, because it is the only sentence that
 * explains why any of this is mandatory.
 *
 * What is NOT shared is the meaning of a blank password, which is the one
 * place the two screens genuinely differ: enrolment has nothing stored so
 * blank means no password, while an update leaves the stored one alone.
 * That is why the password's note is a prop rather than a string here —
 * the difference is real and stating it wrongly on either screen is worse
 * than any duplication saved.
 */
import type { ReactNode } from "react";
import { Banner } from "@shared/components/Banner";
import { HelpField } from "@shared/components/FieldHelp";
import { PasswordInput } from "@shared/components/PasswordInput";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import type { SmtpSecurity, SmtpSettingsInput } from "@shared/api/contracts";

/** The SMTP block as a FORM holds it: the port is a string because that is
 *  what an <input> has, and "" is a state an operator passes through while
 *  retyping it. It becomes a number at the boundary (see `smtpInput`), not
 *  before, so a half-typed port cannot be rounded into something the
 *  server would accept. */
export interface SmtpFieldValues {
  host: string;
  port: string;
  security: SmtpSecurity;
  username: string;
  password: string;
  from: string;
}

/** Submission with STARTTLS, which is what nearly every provider wants and
 *  the one default that is neither unencrypted nor a legacy port. */
export const DEFAULT_SMTP: SmtpFieldValues = {
  host: "",
  port: "587",
  security: "starttls",
  username: "",
  password: "",
  from: ""
};

/**
 * Deliberately weaker than the server, which parses the address per RFC
 * 5322. A browser-side check exists to catch the mistakes a person makes
 * while typing — no @ at all, a trailing space, the whole thing left
 * blank — and a stricter rule here would refuse addresses the service
 * accepts, which is the failure mode that cannot be recovered from the
 * form: the operator would have a valid address and a button that will
 * not submit it. The service stays the one that decides, and it answers
 * INVALID_EMAIL when it disagrees.
 */
const EMAIL = /^[^\s@]+@[^\s@]+$/;

export function looksLikeEmail(value: string): boolean {
  return EMAIL.test(value);
}

/** A port an operator has finished typing. Absent, non-numeric, zero and
 *  above 65535 are all "not yet", which is what the submit gate reads. */
export function portNumber(value: string): number | null {
  if (!/^\d+$/.test(value)) return null;
  const port = Number(value);
  return port >= 1 && port <= 65535 ? port : null;
}

/** Whether this block is complete enough to send. The two credential
 *  fields are not gated: a relay on the same host can legitimately need
 *  neither, and the service refuses the combinations it cannot use. */
export function smtpFieldsComplete(values: SmtpFieldValues): boolean {
  return values.host.trim() !== "" && portNumber(values.port) !== null && looksLikeEmail(values.from);
}

/** The form's values as the contract's own request type. Only ever called
 *  once `smtpFieldsComplete` holds, so the port cannot be null here; it is
 *  still handled rather than asserted, because a caller that gets the
 *  order wrong should send a port the server refuses rather than NaN. */
export function smtpInput(values: SmtpFieldValues): SmtpSettingsInput {
  return {
    host: values.host.trim(),
    port: portNumber(values.port) ?? 0,
    security: values.security,
    username: values.username.trim(),
    password: values.password,
    from: values.from.trim()
  };
}

/**
 * The warning both screens carry, and the reason the fields beside it are
 * not optional.
 *
 * Not dismissible, by the rule in Banner's own doc: the words are the only
 * thing standing between an operator and the belief that mail settings are
 * a convenience. They are not. This product stores a password verifier and
 * nothing else about the person who owns the account, so a forgotten
 * password with no working mail endpoint is recovered at the host or not
 * at all.
 *
 * The two links are the fastest route to a working endpoint for somebody
 * who has never set one up, and they are named rather than described so
 * they can be recognised: a free relay, and the one every home operator
 * already has an account with.
 */
export function RecoveryAttention() {
  return (
    <Banner tone="warn" dismissible={false} style={{ fontSize: "var(--text-sm)", display: "block" }}>
      <strong style={{ display: "block", marginBottom: 4 }}>ATTENTION</strong>
      <span>
        This mail connection is the only way back into this account. retnd
        keeps no copy of the administrator password and has no other channel
        to reach you on, so if the password is ever lost, a reset link sent
        over this SMTP server is the one thing that can restore access. An
        endpoint that has stopped working is discovered at the worst possible
        moment, so keep it current and use Send test email after any change.
      </span>
      <span style={{ display: "block", marginTop: 6 }}>
        No mail server of your own? A free relay such as{" "}
        <a href="https://www.smtp2go.com/" target="_blank" rel="noreferrer">SMTP2go</a>{" "}
        works, and so does an ordinary mailbox: see{" "}
        <a href="https://support.google.com/mail/answer/81126" target="_blank" rel="noreferrer">Gmail SMTP setup</a>.
      </span>
    </Banner>
  );
}

/**
 * The six fields themselves.
 *
 * `onChange` takes a patch rather than a whole value so a caller holds one
 * piece of state for the block instead of six setters, and so adding a
 * field here does not touch either call site's handler.
 */
export function SmtpFields({
  values,
  onChange,
  disabled = false,
  passwordAutoComplete,
  passwordNote
}: {
  values: SmtpFieldValues;
  onChange(patch: Partial<SmtpFieldValues>): void;
  disabled?: boolean;
  /** "new-password" while enrolling, "off" on a field whose blank state
   *  means "keep the stored one": offering to save an empty value into a
   *  password manager is offering to save nothing. */
  passwordAutoComplete: string;
  /** What blank means on this screen. Rendered under the field, because
   *  it is the one thing about this block that differs between them. */
  passwordNote?: ReactNode;
}) {
  return (
    <>
      <HelpField label="SMTP host" help={FIELD_HELP.smtpHost}>
        {(helpId) => (
          <input
            className="input input--mono"
            aria-describedby={helpId}
            autoComplete="off"
            disabled={disabled}
            value={values.host}
            onChange={(e) => onChange({ host: e.target.value })}
          />
        )}
      </HelpField>
      <div style={{ display: "grid", gridTemplateColumns: "minmax(0, 1fr) minmax(0, 1.2fr)", gap: 14 }}>
        <HelpField label="Port" help={FIELD_HELP.smtpPort}>
          {(helpId) => (
            <input
              className="input input--mono"
              type="number"
              min={1}
              max={65535}
              aria-describedby={helpId}
              autoComplete="off"
              disabled={disabled}
              value={values.port}
              onChange={(e) => onChange({ port: e.target.value })}
            />
          )}
        </HelpField>
        <HelpField label="Security" help={FIELD_HELP.smtpSecurity}>
          {(helpId) => (
            <select
              className="input"
              aria-describedby={helpId}
              disabled={disabled}
              value={values.security}
              onChange={(e) => onChange({ security: e.target.value as SmtpSecurity })}
            >
              <option value="starttls">STARTTLS (587)</option>
              <option value="tls">TLS (465)</option>
              <option value="none">None</option>
            </select>
          )}
        </HelpField>
      </div>
      <HelpField label="SMTP username" help={FIELD_HELP.smtpUsername}>
        {(helpId) => (
          <input
            className="input input--mono"
            aria-describedby={helpId}
            autoComplete="off"
            disabled={disabled}
            value={values.username}
            onChange={(e) => onChange({ username: e.target.value })}
          />
        )}
      </HelpField>
      <HelpField label="SMTP password" help={FIELD_HELP.smtpPassword}>
        {(helpId, field) => (
          <>
            <PasswordInput
              label={field.label}
              labelledBy={field.id}
              autoComplete={passwordAutoComplete}
              describedBy={helpId}
              value={values.password}
              onChange={(password) => onChange({ password })}
              disabled={disabled}
            />
            {passwordNote}
          </>
        )}
      </HelpField>
      <HelpField label="From address" help={FIELD_HELP.smtpFrom}>
        {(helpId) => (
          <input
            className="input input--mono"
            type="email"
            aria-describedby={helpId}
            autoComplete="off"
            disabled={disabled}
            value={values.from}
            onChange={(e) => onChange({ from: e.target.value })}
          />
        )}
      </HelpField>
    </>
  );
}

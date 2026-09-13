/**
 * Asking for a reset link (issue #830).
 *
 * The whole screen is one field and one button, and the only difficult
 * part is what it is allowed to say afterwards.
 *
 * POST /auth/forgot-password answers 204 for every input: for an
 * unenrolled deployment, for a username that is not the administrator's,
 * for an administrator with no recovery address, and for an SMTP endpoint
 * that refused the message. It answers BEFORE any mail is attempted, so
 * the response time does not vary either. That is deliberate — this route
 * is unauthenticated and rate limited, and an answer that varied would
 * let anybody on the network confirm an administrator's name — and it
 * means this page genuinely does not know whether anything was sent.
 *
 * So the confirmation says so, in as many words, rather than claiming a
 * message is on its way. An operator who typed the wrong username and
 * then waits an hour for mail that was never sent has been misled by a
 * page that was trying to be reassuring, and the honest sentence costs
 * nothing: the condition under which a link arrives is stated, and the
 * fact that the answer is the same either way is stated beside it.
 *
 * A transport failure is a different matter and is still reported: "the
 * service did not answer" is not an answer about an account, so there is
 * nothing to leak in saying it.
 */
import { useState } from "react";
import { Link } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { describeFailure } from "@shared/api/failure";
import type { OperatorFailure } from "@shared/api/failure";
import { AuthFrame } from "./LoginPage";
import { Banner } from "@shared/components/Banner";
import { ErrorState } from "@shared/components/EmptyState";
import { HelpField } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";

export function ForgotPasswordPage() {
  const api = useApi();
  const [username, setUsername] = useState("");
  const [busy, setBusy] = useState(false);
  const [asked, setAsked] = useState(false);
  const [failure, setFailure] = useState<OperatorFailure | null>(null);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (username.length === 0) return;
    setBusy(true);
    setFailure(null);
    // Cleared before the attempt, not only on failure: a confirmation left
    // standing over a request that never got out tells an operator mail is
    // coming that nobody was ever asked to send.
    setAsked(false);
    api
      .requestPasswordReset(username)
      .then(() => setAsked(true))
      .catch((e: unknown) => setFailure(describeFailure(e, "The reset request could not be sent.")))
      .finally(() => setBusy(false));
  };

  return (
    <AuthFrame>
      <h1 style={{ margin: "0 0 6px", fontSize: 21 }}>Reset your password</h1>
      <p style={{ margin: "0 0 22px", color: "var(--text-2)", fontSize: 13 }}>
        Backupd emails a one-time link to the recovery address stored for the
        administrator account.
      </p>
      <form onSubmit={submit} style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <HelpField label="Username" help={FIELD_HELP.forgotUsername}>
          {(helpId) => (
            <input
              className="input input--mono"
              aria-describedby={helpId}
              autoComplete="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              required
            />
          )}
        </HelpField>
        {asked ? (
          // Not dismissible: it is the answer to the action just taken, and
          // it is also the only thing on screen saying what was and was not
          // established. Dismissed, the page looks like nothing happened.
          <Banner tone="ok" dismissible={false} style={{ fontSize: "var(--text-sm)", display: "block" }}>
            <span>
              If that is the administrator account and it has a recovery address
              configured, a reset link is on its way to it. The link works once
              and expires 30 minutes after it was issued.
            </span>
            <span style={{ display: "block", marginTop: 6, color: "var(--text-2)" }}>
              This page answers exactly the same way whether or not that account
              exists, so it cannot tell you which of the two happened. If no mail
              arrives, check the username and the recovery address configured for
              it, under Settings, from a signed-in session.
            </span>
          </Banner>
        ) : null}
        {failure ? (
          <ErrorState
            message={failure.message}
            remediation={failure.remediation}
            correlationId={failure.correlationId}
            detail={failure.detail}
          />
        ) : null}
        <button className="btn btn--primary" type="submit" disabled={busy || username.length === 0} style={{ height: 40 }}>
          {busy ? "Sending…" : "Email a reset link"}
        </button>
      </form>
      <p style={{ margin: "18px 0 0", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
        Remembered it? <Link to="/">Go to sign in</Link>.
      </p>
    </AuthFrame>
  );
}

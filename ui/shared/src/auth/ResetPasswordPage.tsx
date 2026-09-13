/**
 * Redeeming a reset link (issue #830).
 *
 * Three states, and two of them are about the link rather than about the
 * password:
 *
 *   - No token in the URL. Somebody reached this route by typing it, or
 *     by following a link their mail client truncated. There is nothing
 *     to submit and no field worth showing, so the page says which link
 *     to use instead rather than presenting a form that cannot work.
 *   - A token the service will not take. RESET_TOKEN_INVALID covers
 *     expired, already-spent and never-issued alike, deliberately: all
 *     three are recovered the same way, by asking for another link, and
 *     that is the one thing this page offers on that refusal.
 *   - A password set. Redeeming the token revokes EVERY session and
 *     issues none, so this browser is signed out whatever it was before.
 *     Saying so matters: without it, an operator who was signed in
 *     elsewhere discovers it as a mysterious sign-out later, and one who
 *     expects to land in the application sits on a page that appears to
 *     have stalled.
 *
 * The token is read from the query string rather than taken as a prop,
 * because the link in the mail is what carries it and this page is
 * mounted by the unauthenticated router with no one to pass it down.
 */
import { useId, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { apiErrorOf, describeFailure } from "@shared/api/failure";
import type { OperatorFailure } from "@shared/api/failure";
import { AuthFrame } from "./LoginPage";
// The same floor first-run enrolment applies and the same one the service
// enforces. Imported rather than restated: a reset page that accepted a
// shorter password would have the button and the server disagreeing, and
// the operator would spend their single-use link finding out.
import { MIN_LENGTH } from "./EnrollmentPage";
import { Banner } from "@shared/components/Banner";
import { ErrorState } from "@shared/components/EmptyState";
import { HelpField } from "@shared/components/FieldHelp";
import { PasswordInput } from "@shared/components/PasswordInput";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";

/** Set on the one refusal with somewhere to send the operator, the same
 *  shape EnrollmentPage uses for ENROLLMENT_CLOSED. */
export interface ResetFailure extends OperatorFailure {
  offerNewLink?: boolean;
}

export function describeResetFailure(e: unknown): ResetFailure {
  const api = apiErrorOf(e);
  if (api?.code === "RESET_TOKEN_INVALID") {
    return {
      message: "This link has expired or has already been used.",
      remediation:
        "A reset link works once and lapses 30 minutes after it was issued, and the password was not changed. Ask for a fresh link and open the newest mail, since an older link in the same thread is one of the ones that no longer works.",
      correlationId: api.correlationId,
      offerNewLink: true
    };
  }
  return describeFailure(e, "The password was not changed.");
}

export function ResetPasswordPage() {
  const api = useApi();
  const [params] = useSearchParams();
  const token = params.get("token") ?? "";

  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  const [failure, setFailure] = useState<ResetFailure | null>(null);

  const tooShort = password.length > 0 && password.length < MIN_LENGTH;
  const mismatch = confirm.length > 0 && confirm !== password;
  // Referenced from the field rather than left loose inside its <label>,
  // for the reason EnrollmentPage's own note gives: PasswordInput carries
  // an explicit aria-labelledby, so nothing sweeps these into the name and
  // a description is where a validation message belongs anyway.
  const tooShortId = useId();
  const mismatchId = useId();
  const valid = password.length >= MIN_LENGTH && confirm === password;

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!valid || token === "") return;
    setBusy(true);
    setFailure(null);
    api
      .resetPassword(token, password)
      .then(() => {
        setDone(true);
        setPassword("");
        setConfirm("");
      })
      .catch((e: unknown) => setFailure(describeResetFailure(e)))
      .finally(() => setBusy(false));
  };

  return (
    <AuthFrame>
      <h1 style={{ margin: "0 0 6px", fontSize: 21 }}>Choose a new password</h1>
      {token === "" ? (
        // No form at all. A password typed here could not be submitted, and
        // a disabled button over two filled fields explains nothing.
        <>
          <p style={{ margin: "0 0 14px", color: "var(--text-2)", fontSize: 13 }}>
            This page needs the one-time token from the emailed link, and the
            address it was opened with carries none.
          </p>
          <Banner tone="warn" dismissible={false} style={{ fontSize: "var(--text-sm)" }}>
            <span>
              Open the link in the reset email itself, which ends in ?token= and
              a long value. Copying only part of it, or retyping this address by
              hand, leaves the token behind.
            </span>
          </Banner>
          <p style={{ margin: "18px 0 0", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
            No link to hand? <Link to="/forgot-password">Ask for a new one</Link>.
          </p>
        </>
      ) : done ? (
        <>
          <Banner tone="ok" dismissible={false} style={{ fontSize: "var(--text-sm)", display: "block" }}>
            <span>
              The password has been changed, and every session has been signed
              out: this browser, and anywhere else this account was left signed
              in. That is what makes a reset safe to use on an account somebody
              else may have had access to.
            </span>
          </Banner>
          <p style={{ margin: "18px 0 0", fontSize: "var(--text-sm)" }}>
            <Link to="/">Go to sign in</Link> with the new password.
          </p>
        </>
      ) : (
        <>
          <p style={{ margin: "0 0 22px", color: "var(--text-2)", fontSize: 13 }}>
            {"Minimum " + MIN_LENGTH + " characters. This link can be used once, and setting a password signs out every session."}
          </p>
          <form onSubmit={submit} style={{ display: "flex", flexDirection: "column", gap: 14 }}>
            <HelpField label="New password" help={FIELD_HELP.resetNewPassword}>
              {(helpId, field) => (
                <>
                  <PasswordInput
                    label={field.label}
                    labelledBy={field.id}
                    autoComplete="new-password"
                    describedBy={tooShort ? helpId + " " + tooShortId : helpId}
                    value={password}
                    onChange={setPassword}
                    required
                  />
                  {tooShort ? (
                    <span id={tooShortId} style={{ fontSize: "var(--text-sm)", color: "var(--danger)" }}>
                      {"Minimum " + MIN_LENGTH + " characters."}
                    </span>
                  ) : null}
                </>
              )}
            </HelpField>
            <HelpField label="Confirm new password" help={FIELD_HELP.resetConfirmPassword}>
              {(helpId, field) => (
                <>
                  <PasswordInput
                    label={field.label}
                    labelledBy={field.id}
                    autoComplete="new-password"
                    describedBy={mismatch ? helpId + " " + mismatchId : helpId}
                    value={confirm}
                    onChange={setConfirm}
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
            {failure ? (
              <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
                <ErrorState
                  message={failure.message}
                  remediation={failure.remediation}
                  correlationId={failure.correlationId}
                  detail={failure.detail}
                />
                {failure.offerNewLink ? (
                  <Link to="/forgot-password" style={{ fontSize: "var(--text-sm)" }}>
                    Email a new reset link
                  </Link>
                ) : null}
              </div>
            ) : null}
            <button className="btn btn--primary" type="submit" disabled={!valid || busy} style={{ height: 40 }}>
              {busy ? "Setting…" : "Set password"}
            </button>
          </form>
        </>
      )}
    </AuthFrame>
  );
}

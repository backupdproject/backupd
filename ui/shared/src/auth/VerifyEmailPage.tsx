/**
 * Redeeming a recovery-email verification link (issue #830 §8).
 *
 * The page exists because the link in the mail has to land somewhere, and
 * what it lands on has three states worth distinguishing:
 *
 *   - No token in the URL. Somebody typed the address, or their mail
 *     client truncated the link. There is nothing to submit, so the page
 *     says which link to use instead of showing a control that cannot
 *     work.
 *   - Verified. The address is proved, the account is no longer
 *     provisional, and saying so explicitly matters more here than on
 *     most success screens: the message that sent the operator here told
 *     them the account would be REMOVED if they did nothing, so the one
 *     thing they need to read is that it will not be.
 *   - Refused. VERIFY_TOKEN_INVALID covers expired, already-used,
 *     never-issued and "there is no account any more" alike, and all four
 *     are recovered the same way - sign in and ask for a fresh link from
 *     the banner, or, if the account has already lapsed, enroll again.
 *     Both routes are offered, because from here it is genuinely not
 *     knowable which of the two happened.
 *
 * It submits on mount rather than behind a button. The operator has
 * already made the decision this page is about, in their mail client, by
 * clicking; a button here would be a second confirmation of the same
 * intent, and one more thing to abandon halfway. The POST still carries
 * the CSRF header every other write does (the api client attaches it), so
 * nothing about this is a cross-site-writable route.
 *
 * It is reachable signed IN and signed OUT, and is mounted in both of
 * App's routers for that reason: the link is opened from whatever device
 * is holding the mailbox, which is frequently not the one the console is
 * open on, and requiring a session would mean proving you hold the
 * account in order to prove you can read its recovery address.
 */
import { useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { apiErrorOf, describeFailure } from "@shared/api/failure";
import type { OperatorFailure } from "@shared/api/failure";
import { AuthFrame } from "./LoginPage";
import { graph } from "@shared/state/graph";
import { publishRecoverySettings, recoveryVerificationNode } from "@shared/state/appNodes";
import { Banner } from "@shared/components/Banner";
import { ErrorState } from "@shared/components/EmptyState";

/** Set on the one refusal that has somewhere to send the operator, the
 *  same shape ResetPasswordPage uses for a dead reset link. Unexported,
 *  both of them: nothing outside this page has a use for either, and a
 *  non-component export here costs the file its fast-refresh boundary. */
interface VerifyFailure extends OperatorFailure {
  offerNewLink?: boolean;
}

function describeVerifyFailure(e: unknown): VerifyFailure {
  const api = apiErrorOf(e);
  if (api?.code === "VERIFY_TOKEN_INVALID") {
    return {
      message: "This verification link has expired or has already been used.",
      remediation:
        "A verification link works once and lapses 30 minutes after it was issued. Sign in and use the “Resend link” action on the banner to get a fresh one — and open the newest mail, since an older link in the same thread is one of the ones that no longer works. If the account was removed for not being verified in time, enrollment is open again and the container log carries a new enrollment link.",
      correlationId: api.correlationId,
      offerNewLink: true
    };
  }
  return describeFailure(e, "The recovery email was not verified.");
}

export function VerifyEmailPage() {
  const api = useApi();
  const [params] = useSearchParams();
  const token = params.get("token") ?? "";

  const [busy, setBusy] = useState(token !== "");
  const [done, setDone] = useState(false);
  const [failure, setFailure] = useState<VerifyFailure | null>(null);
  // React 18's development StrictMode mounts effects twice, and this one
  // spends a single-use token: without the guard the second pass reports
  // the operator's own successful verification back to them as an
  // already-used link.
  const submitted = useRef(false);

  useEffect(() => {
    if (token === "" || submitted.current) return;
    submitted.current = true;
    api
      .verifyRecoveryEmail(token)
      .then(() => {
        setDone(true);
        // The banner above this page (RecoveryVerificationBanner) sits
        // above the router and is never unmounted by arriving here, so
        // without the two writes below it would go on saying the account
        // is about to be removed directly above the sentence saying it
        // is verified.
        //
        // First, optimistically, from what this response already proves,
        // so the banner goes at once rather than after another round
        // trip. It can only run when the banner's own read has already
        // landed - that is where the address comes from - which is why it
        // is not the whole of the fix.
        const current = graph.read(recoveryVerificationNode);
        if (current) {
          publishRecoverySettings({
            ...current,
            recoveryEmailVerified: true,
            verificationDeadline: ""
          });
        }
        // Then authoritatively. The banner's read and this POST are
        // issued in the same instant when the link is opened in a tab
        // that is already signed in, and the service may well answer the
        // read first, with the state as it was before the click; this
        // re-read is taken AFTER the verification and is what the banner
        // ends up drawn from (its own stale-read guard discards the older
        // answer if that arrives last). Refused for an unauthenticated
        // browser, which is the common case - the link is usually opened
        // on a phone - and there is nothing to update there, so the
        // refusal is ignored rather than reported.
        api
          .getRecoverySettings()
          .then(publishRecoverySettings)
          .catch(() => undefined);
      })
      .catch((e: unknown) => setFailure(describeVerifyFailure(e)))
      .finally(() => setBusy(false));
  }, [api, token]);

  return (
    <AuthFrame>
      <h1 style={{ margin: "0 0 6px", fontSize: 21 }}>Verify your recovery email</h1>
      {token === "" ? (
        <>
          <p style={{ margin: "0 0 14px", color: "var(--text-2)", fontSize: 13 }}>
            This page needs the one-time token from the emailed link, and the
            address it was opened with carries none.
          </p>
          <Banner tone="warn" dismissible={false} style={{ fontSize: "var(--text-sm)" }}>
            <span>
              Open the link in the verification email itself, which ends in
              ?token= and a long value. Copying only part of it, or retyping this
              address by hand, leaves the token behind.
            </span>
          </Banner>
          <p style={{ margin: "18px 0 0", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
            <Link to="/">Go to sign in</Link> — once signed in, the banner at the
            top of the page can send a fresh link.
          </p>
        </>
      ) : busy ? (
        <p style={{ margin: 0, color: "var(--text-2)", fontSize: 13 }}>Verifying…</p>
      ) : done ? (
        <>
          <Banner tone="ok" dismissible={false} style={{ fontSize: "var(--text-sm)", display: "block" }}>
            <span>
              Your recovery email is verified. The administrator account is no
              longer provisional and will not be removed, and a forgotten password
              can now be reset over this address.
            </span>
          </Banner>
          <p style={{ margin: "18px 0 0", fontSize: "var(--text-sm)" }}>
            <Link to="/">Continue to Backupd</Link>.
          </p>
        </>
      ) : failure ? (
        <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
          <ErrorState
            message={failure.message}
            remediation={failure.remediation}
            correlationId={failure.correlationId}
            detail={failure.detail}
          />
          {failure.offerNewLink ? (
            <Link to="/" style={{ fontSize: "var(--text-sm)" }}>
              Go to sign in
            </Link>
          ) : null}
        </div>
      ) : null}
    </AuthFrame>
  );
}

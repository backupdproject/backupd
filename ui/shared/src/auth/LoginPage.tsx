/**
 * The sign-in screen, and the frame both pre-auth screens share.
 *
 * Two constraints shape it. It must not look like the NAS operating
 * system's own login, because an operator who mistakes it for one has just
 * typed their OS credentials into an application that never wanted them,
 * so the page says what account it is asking for in as many words. And a
 * refusal has to be reported as the refusal it was: a rate-limited address
 * and a wrong password are different problems with different answers, and
 * reading both as a wrong password sends someone to retype something that
 * was never wrong.
 *
 * `AuthFrame` lives here rather than in components because it exists for
 * exactly these two screens, and putting it next to the first one that
 * needs it keeps the shared-with-whom question answerable by looking.
 */
import { useState } from "react";
import { Link } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { apiErrorOf, describeFailure } from "@shared/api/failure";
import type { OperatorFailure } from "@shared/api/failure";
import { Logo, Wordmark } from "@shared/components/Logo";
import { ErrorState } from "@shared/components/EmptyState";
import { HelpField } from "@shared/components/FieldHelp";
import { PasswordInput } from "@shared/components/PasswordInput";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import { TooltipsSuppressed } from "@shared/hooks/useTooltips";

/** Issue #274, one view over from the enrolment page it was filed against:
 *  a wrong password and a rate-limited address are different problems with
 *  different answers, and both used to read as a wrong password under a
 *  correlation id, the literal `cid_login`, that no log has ever held. */
export function describeLoginFailure(e: unknown): OperatorFailure {
  const api = apiErrorOf(e);
  if (api?.code === "UNAUTHENTICATED") {
    // The service's own words for this are already exact, and this is the
    // one refusal where saying less is the point: which half was wrong is
    // deliberately not distinguished, by the handler or here.
    return {
      message: "That username and password combination was not accepted.",
      correlationId: api.correlationId
    };
  }
  return describeFailure(e, "Signing in did not succeed.");
}

/** §30 — deliberately NOT styled like a NAS system login. An operator must never
 *  believe they are handing NAS OS credentials to this app.
 *
 *  Issue #829's requirement 2: this screen carries no hover tooltips at
 *  all, whatever the global preference says, so it is wrapped in the
 *  suppression scope rather than reading the preference. It is a screen
 *  with two fields and one button, shown to somebody who has not signed in
 *  yet and whose first job is to not mistake it for their NAS login (see
 *  above): pop-ups opening over that are noise on the one screen that can
 *  least afford any. The fields keep their aria-describedby copy, which is
 *  a description and not a pop-up, so the sign-in form stays as explained
 *  to a screen reader as it ever was.
 *
 *  A wrapper rather than a provider around the body, so the scope covers
 *  the whole screen and not only the fields that happen to have help
 *  today. */
export function LoginPage({ onSignedIn }: { onSignedIn(): void }) {
  return (
    <TooltipsSuppressed.Provider value={true}>
      {/* The same fact twice, because since issue #874 two layers ask it
          and only one of them can read React context. A host inside this
          subtree reads the provider; the delegated `data-tip` layer is
          mounted at the app root and, by the time it has a trigger, has
          an element and not a position in the tree — so the DOM states
          it too. `display: contents` because this screen's layout is not
          this wrapper's business. */}
      <div data-tips="off" style={{ display: "contents" }}>
        <SignInForm onSignedIn={onSignedIn} />
      </div>
    </TooltipsSuppressed.Provider>
  );
}

function SignInForm({ onSignedIn }: { onSignedIn(): void }) {
  const api = useApi();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [failure, setFailure] = useState<OperatorFailure | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setFailure(null);
    api
      .login(username, password)
      .then(onSignedIn)
      .catch((e: unknown) => setFailure(describeLoginFailure(e)))
      .finally(() => setBusy(false));
  };

  return (
    <AuthFrame>
      <h1 style={{ margin: "0 0 6px", fontSize: 21 }}>Sign in</h1>
      <p style={{ margin: "0 0 22px", color: "var(--text-2)", fontSize: 13 }}>
        retnd local account — <strong style={{ color: "var(--text)" }}>not</strong> your
        NAS operating-system login.
      </p>
      <form onSubmit={submit} style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <HelpField label="Username" help={FIELD_HELP.loginUsername}>
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
        <HelpField label="Password" help={FIELD_HELP.loginPassword}>
          {(helpId, field) => (
            <PasswordInput
              label={field.label}
              labelledBy={field.id}
              autoComplete="current-password"
              describedBy={helpId}
              value={password}
              onChange={setPassword}
              required
            />
          )}
        </HelpField>
        {failure ? (
          <ErrorState
            message={failure.message}
            remediation={failure.remediation}
            correlationId={failure.correlationId}
          />
        ) : null}
        <button className="btn btn--primary" type="submit" disabled={busy} style={{ height: 40 }}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
      <p style={{ margin: "18px 0 0", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
        First time here? <Link to="/enroll">Create the administrator account</Link>.
      </p>
      {/* Issue #830. Below the enrolment line rather than beside the
          password field: the moment somebody needs this they have already
          tried the password and failed, so it belongs where the other
          "this is not working" route out of the screen already is. */}
      <p style={{ margin: "6px 0 0", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
        <Link to="/forgot-password">Forgot password?</Link>
      </p>
    </AuthFrame>
  );
}

/** The centred, branded card both pre-auth screens sit in. It carries the
 *  product's own identity deliberately, because the one thing an operator
 *  must not conclude on either of these screens is that they are looking
 *  at their NAS operating system asking for its own password. */
export function AuthFrame({ children }: { children: React.ReactNode }) {
  return (
    <div style={{ minHeight: "100vh", display: "grid", placeItems: "center", padding: "48px 24px", background: "var(--bg)" }}>
      <div style={{ width: "100%", maxWidth: 452, display: "flex", flexDirection: "column", gap: 20 }}>
        <div style={{ display: "flex", alignItems: "center", gap: 11 }}>
          <Logo size={27} title="retnd" />
          <Wordmark size={15} />
        </div>
        <div className="card" style={{ padding: 28 }}>{children}</div>
      </div>
    </div>
  );
}

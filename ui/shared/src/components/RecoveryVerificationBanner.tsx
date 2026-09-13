/**
 * The one thing standing between an operator and losing their account
 * (issue #830 §9).
 *
 * An administrator whose recovery address has not been verified is
 * PROVISIONAL: the service deletes the record at a deadline fixed when
 * the account was created, revokes its sessions and reopens enrollment.
 * That is the right behaviour - an account whose recovery address nobody
 * can read is already lost, and finding out at creation time is far
 * cheaper than finding out the day a password is forgotten - but it is
 * only defensible if the operator is TOLD, while they can still act.
 * This banner is that telling.
 *
 * Three decisions are deliberate.
 *
 * It is not dismissible, and this is the strongest case for that in the
 * application. It sits above the router, mounted once per session, so a
 * dismissal would last until a hard reload; and what it says is that the
 * account disappears. #620's rule ("a banner that is merely serious is
 * still a banner somebody has finished reading") is about notices whose
 * consequence survives being closed unread. This one's consequence is
 * that the console the operator is reading it in stops having an
 * administrator.
 *
 * It owns the read of GET /auth/recovery rather than taking the block as
 * a prop, and publishes what comes back into recoveryVerificationNode
 * (state/appNodes.ts) rather than into local state. Both halves are
 * deliberate. App fetches the five app-wide resources and passes them
 * down, and this is not one of them: it is authenticated auth state
 * rather than backup state, and one more resource in App's fan-out would
 * make every page depend on a read only this banner uses. But the answer
 * cannot be private either - VerifyEmailPage redeems the link that makes
 * this banner's sentence false, and a banner mounted above the router is
 * never unmounted by navigating to that page, so the two have to be
 * looking at one value or they contradict each other on screen.
 *
 * The tone follows the CONSEQUENCE, not the severity of the words.
 * "danger" while a deadline is running, because something is about to be
 * deleted; "warn" for an established account whose address was changed
 * and not re-verified, where nothing lapses and what is at stake is only
 * that recovery would mail to an unproven mailbox.
 */
import { useCallback, useEffect, useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { describeFailure } from "@shared/api/failure";
import type { OperatorFailure } from "@shared/api/failure";
import { useCausl } from "@shared/state/graph";
import { publishRecoverySettings, recoveryVerificationNode } from "@shared/state/appNodes";
import { WarningBanner } from "./WarningBanner";

/** The deadline as an operator reads a clock, with the machine-readable
 *  instant kept on the element for anything that wants it exactly.
 *
 *  A value the browser cannot parse is rendered verbatim rather than as
 *  "Invalid Date": the service sends RFC3339, and if that ever changes
 *  the honest failure is to show what arrived instead of a word that
 *  looks like a bug in the account. */
function DeadlineText({ deadline }: { deadline: string }) {
  const parsed = new Date(deadline);
  const text = Number.isNaN(parsed.getTime()) ? deadline : parsed.toLocaleString();
  return <time dateTime={deadline}>{text}</time>;
}

export function RecoveryVerificationBanner() {
  const api = useApi();
  const recovery = useCausl(recoveryVerificationNode);
  const [resending, setResending] = useState(false);
  const [resent, setResent] = useState(false);
  const [failure, setFailure] = useState<OperatorFailure | null>(null);

  const load = useCallback(() => {
    let cancelled = false;
    api
      .getRecoverySettings()
      .then((r) => {
        // Through the shared publisher, which is what stops this read -
        // frequently taken before a verification that has already
        // happened - from overwriting it (publishRecoverySettings).
        if (!cancelled) publishRecoverySettings(r);
      })
      .catch(() => {
        // Silent, and that is the one place in this component where
        // silence is right. A deployment whose auth service cannot
        // answer this read has a bigger problem than an unverified
        // address, every other panel on the page is reporting it, and a
        // second red banner saying so would push the actual state off
        // the screen. The verified case renders nothing either, so
        // "could not ask" and "nothing to say" look the same here on
        // purpose.
      });
    return () => {
      cancelled = true;
    };
  }, [api]);

  useEffect(load, [load]);

  const resend = () => {
    setResending(true);
    setFailure(null);
    api
      .resendRecoveryEmailVerification()
      .then(() => {
        setResent(true);
        // Re-read rather than assume: the send is what the service just
        // did, and the deadline (and, if somebody verified in another
        // tab meanwhile, the whole banner) comes from its answer.
        load();
      })
      .catch((e: unknown) =>
        setFailure(describeFailure(e, "The verification email was not sent."))
      )
      .finally(() => setResending(false));
  };

  // Nothing to say: verified, not read yet, or an administrator with no
  // recovery address at all - the last of those is `auth create-admin`'s
  // headless state, which the Settings page reports as unconfigured and
  // which has no link to verify because none was ever mailed.
  if (!recovery || recovery.recoveryEmailVerified || recovery.recoveryEmail === "") return null;

  const lapses = recovery.verificationDeadline !== "";
  return (
    <WarningBanner
      tone={lapses ? "danger" : "warn"}
      eyebrow="Recovery email"
      title={"Unverified — verify " + recovery.recoveryEmail}
      dismissible={false}
      dismissKey="recovery-unverified"
      actions={
        <button
          className="btn"
          type="button"
          onClick={resend}
          disabled={resending}
          style={{ height: 32 }}
        >
          {resending ? "Sending…" : "Resend link"}
        </button>
      }
    >
      <span>
        {lapses ? (
          <>
            Open the link in the verification email sent to{" "}
            <strong>{recovery.recoveryEmail}</strong>. This account is provisional:
            if the address is not verified by{" "}
            <DeadlineText deadline={recovery.verificationDeadline} /> it will be
            removed, every session will be signed out, and enrollment will reopen.
          </>
        ) : (
          <>
            Open the link in the verification email sent to{" "}
            <strong>{recovery.recoveryEmail}</strong>. Until it is used, nobody has
            proved this mailbox can be read, so a forgotten password would send a
            reset link somewhere that may not arrive.
          </>
        )}
        {resent ? " A fresh link has just been sent." : ""}
        {failure ? " " + failure.message : ""}
      </span>
    </WarningBanner>
  );
}

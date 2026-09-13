/**
 * The hold that stops a backup set running, and the two ways out of it
 * (issue #814).
 *
 * # What a hold IS
 *
 * A workflow run whose cleanup this product could not finish. Its
 * "after" hooks may never have run, which means the source machine may
 * still be quiesced, mounted or paused. The engine refuses the next run
 * of that backup set until somebody settles it, and this banner is the
 * only surface that says so and the only one that offers a way out.
 *
 * # Why the gate reads the HOLD list and not the run's own state
 *
 * Because the holds are what the next run is actually refused against.
 * A run row carries `recovery_state`, which is a description of that
 * run; `GET /workflow-recovery` is the engine's own hold set, and a
 * screen that gated on the former would be predicting a refusal rather
 * than reporting one — and would keep predicting it after somebody
 * resumed the cleanup from the CLI.
 *
 * # Two ways out, and deliberately no third
 *
 * `Resume cleanup` executes the hooks that are already owed, from that
 * run's own retained spool, each one re-verified against the sha256
 * recorded when the run was planned — so nothing an operator edited in
 * the meantime decides what executes.
 *
 * `Acknowledge` records a person taking responsibility, in words that are
 * kept. It is a dialog rather than a button because the reason is
 * required by the service and by the product: there is no "clear this"
 * and no "ignore this", because the alternative to both of these is a
 * source machine left in a state this product cannot see.
 *
 * The resume is offered FIRST and as the primary action, because it is
 * the one that actually unwinds the hook. An acknowledgement unblocks the
 * set and changes nothing on the machine.
 */
import { useCallback, useState } from "react";

import { useApi } from "@shared/api/ApiContext";
import { WarningBanner } from "@shared/components/WarningBanner";
import { describeFailure } from "@shared/api/failure";
import { useAsync } from "@shared/hooks/useAsync";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import { stamp } from "@shared/utilities/format";
import type { WorkflowRecoveryHold } from "@shared/api/contracts";

/** What a page needs to know about this set's hold: whether there is one,
 *  whether the read failed, and how to re-ask once it is settled. */
export interface WorkflowHoldState {
  hold: WorkflowRecoveryHold | null;
  /** True while the first read is in flight. A page must not draw its Run
   *  control as available on the strength of a hold list it has not
   *  received: the gate is "no hold has been reported", and before the
   *  first answer nothing has been reported either way. */
  loading: boolean;
  /** Why the hold list could not be read, when it could not be. A page
   *  says so rather than silently treating an unreadable hold list as
   *  "no holds", which is the failure mode that would offer a Run the
   *  engine is about to refuse. */
  error: string | null;
  reload(): void;
}

/**
 * This set's outstanding hold, if it has one.
 *
 * One read of the whole hold set, filtered here, because that is the
 * route the API offers and it is the right one: an operator asking "what
 * is stuck in this deployment" gets one answer, and the per-set question
 * is a filter over it.
 */
export function useWorkflowHold(backupSetId: string): WorkflowHoldState {
  const api = useApi();
  const holds = useAsync(() => api.workflowRecovery(), [api]);
  return {
    hold: (holds.data ?? []).find((entry) => entry.backupSetId === backupSetId) ?? null,
    loading: holds.loading,
    error: holds.error?.message ?? null,
    reload: holds.reload
  };
}

export function WorkflowRecoveryBanner({
  hold,
  readOnly,
  onSettled
}: {
  hold: WorkflowRecoveryHold;
  readOnly: boolean;
  /** Called once the hold is gone, so the page re-reads the gate and the
   *  run beside it rather than guessing that the action worked. */
  onSettled(): void;
}) {
  const api = useApi();
  const [busy, setBusy] = useState<"resume" | "acknowledge" | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const [acknowledging, setAcknowledging] = useState(false);
  const [reason, setReason] = useState("");

  const resume = useCallback(() => {
    setBusy("resume");
    setFailure(null);
    api
      .resumeWorkflowCleanup(hold.runId)
      .then(() => {
        setBusy(null);
        onSettled();
      })
      .catch((e: unknown) => {
        setBusy(null);
        // The service's own sentence, which for this route is the
        // interesting half: a resume can be refused because the spool no
        // longer verifies, and that is a different problem from a dead
        // engine. describeFailure is what keeps an exception that never
        // reached the service from being reported as a refusal.
        setFailure(describeFailure(e, "the cleanup could not be resumed").message);
      });
  }, [api, hold.runId, onSettled]);

  const acknowledge = useCallback(() => {
    setBusy("acknowledge");
    setFailure(null);
    api
      .acknowledgeWorkflowRecovery(hold.runId, reason)
      .then(() => {
        setBusy(null);
        setAcknowledging(false);
        setReason("");
        onSettled();
      })
      .catch((e: unknown) => {
        setBusy(null);
        setFailure(describeFailure(e, "the acknowledgement was not recorded").message);
      });
  }, [api, hold.runId, onSettled, reason]);

  return (
    <WarningBanner
      tone="danger"
      eyebrow="Workflow recovery"
      title="This backup set will not run until a workflow run is accounted for"
      tip="workflow.recovery.hold"
      // Not dismissible: this banner is the only thing on screen
      // explaining why the Run control is unavailable, and the refusal
      // stays in force whether or not it is on screen.
      dismissible={false}
      actions={
        <>
          <InfoTooltip id="workflow.recovery.resume">
            <button
              className="btn btn--sm btn--primary"
              disabled={readOnly || busy !== null}
              onClick={resume}
            >
              {busy === "resume" ? "Resuming\u2026" : "Resume cleanup"}
            </button>
          </InfoTooltip>
          <InfoTooltip id="workflow.recovery.acknowledge" alignEnd>
            <button
              className="btn btn--sm btn--caution"
              disabled={readOnly || busy !== null}
              onClick={() => setAcknowledging((open) => !open)}
            >
              {acknowledging ? "Cancel acknowledgement" : "Acknowledge\u2026"}
            </button>
          </InfoTooltip>
        </>
      }
    >
      <span>
        {"Run " + hold.runId + " stopped before its \u201cafter\u201d hooks finished, so this source " +
          "may still be quiesced, mounted or paused. Resume the cleanup to run the hooks that run " +
          "is still owed, from its own retained scripts, or acknowledge that you have dealt with " +
          "it by hand."}
      </span>
      <div className="mono" style={{ marginTop: 6, fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
        {"held since " + stamp(hold.enteredAt) + " \u00b7 scripts retained at " + hold.spoolRef}
      </div>

      {failure ? (
        <div style={{ marginTop: 8, fontSize: "var(--text-sm)", color: "var(--danger)" }}>
          {"That did not work: " + failure}
        </div>
      ) : null}

      {acknowledging ? (
        <div
          style={{
            marginTop: 10,
            display: "flex",
            flexDirection: "column",
            gap: 7,
            maxWidth: "76ch"
          }}
        >
          <label
            htmlFor="workflow-acknowledge-reason"
            className="eyebrow"
            style={{ fontSize: 10.5 }}
          >
            What was done about this run
          </label>
          <textarea
            id="workflow-acknowledge-reason"
            value={reason}
            rows={3}
            onChange={(e) => setReason(e.target.value)}
            placeholder="e.g. thawed the database and unmounted the scratch volume by hand"
            style={{
              font: "inherit",
              fontSize: 13,
              padding: "8px 9px",
              border: "1px solid var(--border-strong)",
              borderRadius: "var(--radius-md)",
              background: "var(--surface)",
              color: "var(--text)",
              resize: "vertical"
            }}
          />
          <p style={{ margin: 0, fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
            {"This is recorded against the run, with the account that wrote it. It unblocks the " +
              "backup set and changes nothing on the source machine."}
          </p>
          <div>
            <InfoTooltip id="workflow.recovery.acknowledge-confirm">
              <button
                className="btn btn--sm btn--caution"
                // The service requires a reason, so an empty one is
                // refused here rather than sent: a request that cannot
                // succeed should not leave the browser.
                disabled={readOnly || busy !== null || reason.trim() === ""}
                onClick={acknowledge}
              >
                {busy === "acknowledge" ? "Recording\u2026" : "Record acknowledgement"}
              </button>
            </InfoTooltip>
          </div>
        </div>
      ) : null}
    </WarningBanner>
  );
}

/**
 * Submitting one of EPIC K's four snapshot acts, and then watching it
 * (issue #788).
 *
 * Restore, verify, hold and release are one `POST /operations` each,
 * carrying a configuration revision and an `Idempotency-Key`, followed by
 * polling the operation that comes back. Four screens do that and they do
 * it identically, so it is one hook: a second copy of this would be a
 * second chance to mint a fresh key per retry, which is the mistake that
 * turns "the reply was lost" into "a second restore started".
 *
 * # The key describes the RETRY of one act on one TARGET
 *
 * A key is minted when the operator asks for something and the SAME key
 * is sent again if they press the button again after a refusal. That is
 * what lets the service recognise a dropped response as the same intent.
 * It is cleared on success, so the next press is a new logical
 * submission. `useRunControls` holds exactly this rule for backup runs
 * and its module doc carries the long version.
 *
 * What that rule needs, and did not have, is a TARGET. One hook instance
 * serves every row of a table (SnapshotRetentionPage holds a single
 * `hold` and a single `release` for all of them), so a key kept after a
 * refusal was kept for the page rather than for the snapshot the
 * operator was acting on: a hold that failed on one run, followed by a
 * hold on a different run, re-sent the first run's key. The service is
 * entitled to read that as the same submission and replay the earlier —
 * failed — one, against the wrong snapshot. So the pending key is
 * remembered WITH the target it belongs to, and a submission naming a
 * different target mints a new one. Dismissing the outcome clears it
 * too: the banner is gone, the operator has moved on, and the next press
 * is not a retry of something no longer on screen.
 *
 * # Why the configuration revision gates the button
 *
 * The service refuses an act submitted against a revision other than the
 * one it is serving, which is what stops a screen that has been open
 * while somebody edited the configuration from acting on a setup nobody
 * looking at it has seen. A page that has not yet loaded the version read
 * therefore cannot submit at all, and says so rather than sending an
 * empty string the service will refuse (issue #597's defect, in the
 * shape EPIC K would have repeated it).
 *
 * # Watching
 *
 * A restore and a verify run for minutes; a hold and a release are
 * finished by the time they answer. Both come back as an operation and
 * both are polled the same way, because the DIFFERENCE is in the
 * operation's status and not in the call: a poll that stops on a terminal
 * status stops immediately for a hold and keeps a restore's progress bar
 * live. Polling stops when the record reaches `completed` or `failed`,
 * and never runs at all before one has been submitted.
 */
import { useCallback, useEffect, useRef, useState } from "react";

import { useApi } from "@shared/api/ApiContext";
import { newIdempotencyKey } from "@shared/api/client";
import { describeFailure } from "@shared/api/failure";
import type { OperatorFailure } from "@shared/api/failure";
import type { SnapshotOperationResult } from "@shared/api/contracts";
import { useCausl } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import type { Operation } from "@shared/types/operation";

/** How often a submitted operation is re-read while it is still running.
 *  Two seconds is the cadence the activity surfaces already use, and a
 *  restore's progress reads as live at it without a poll per frame. */
const WATCH_INTERVAL_MS = 2_000;

/** What the act is doing, as a screen renders it. */
export interface SnapshotAction {
  /** True once the configuration revision is known. False disables every
   *  submit control, and the page says why. */
  ready: boolean;
  /** A submission is in flight. Not the same as a running operation: this
   *  is the POST, and it clears the moment the operation record
   *  arrives. */
  busy: boolean;
  /** The durable record, re-read while it runs. */
  operation: Operation | null;
  /** What came back from the act's own request, translated. Null once a
   *  later attempt succeeds. */
  failure: OperatorFailure | null;
  /**
   * Submits one act on one target and starts watching it.
   *
   * `target` names what is being acted on — a run id, a hold id, the
   * backup set for an act that takes no row — and it is what the pending
   * idempotency key is filed under. Pressing the same button again after
   * a refusal is a retry and re-sends the key; acting on a different
   * target is a new submission and mints a new one. It is a required
   * parameter rather than an optional refinement because a caller that
   * forgot it would be the defect this exists to stop.
   *
   * `act` is handed the two things every one of the four requests needs
   * and nothing else, so a caller writes its own parameter object and
   * cannot forget either. `onDone` receives the whole result — the
   * operation AND the snapshots it changed — which is what lets a hold
   * dialog close onto the new truth rather than onto a re-read that may
   * not have landed.
   */
  submit(
    target: string,
    act: (credentials: { configRevision: string; idempotencyKey: string }) => Promise<SnapshotOperationResult>,
    onDone?: (result: SnapshotOperationResult) => void
  ): void;
  /** Clears the last outcome: the failure, the operation being watched,
   *  and the pending idempotency key. The key goes with them: a retry is
   *  a second press against a refusal that is still on screen, and once
   *  the operator has dismissed it the next press is a new intent. */
  dismiss(): void;
}

/**
 * `fallbackMessage` is what this act is called, in the words its failure
 * should open with — "Could not start the restore." A failure this
 * frontend has no type for still reads as a sentence about the thing the
 * operator pressed.
 */
export function useSnapshotOperation(fallbackMessage: string): SnapshotAction {
  const api = useApi();
  const version = useCausl(versionNode);
  const [busy, setBusy] = useState(false);
  const [operation, setOperation] = useState<Operation | null>(null);
  const [failure, setFailure] = useState<OperatorFailure | null>(null);

  // Survives renders on purpose: pressing the button again after a
  // refusal is the same logical submission, and the header is what lets
  // the service see that. The TARGET travels with it, because one
  // instance of this hook serves every row of a table and a key without
  // the row it belongs to is a key the next row would inherit.
  const pending = useRef<{ target: string; key: string } | null>(null);

  const configRevision = version.data?.configRevision;
  const ready = typeof configRevision === "string" && configRevision !== "";

  const submit = useCallback<SnapshotAction["submit"]>(
    (target, act, onDone) => {
      if (!ready) {
        setFailure({
          message: "This page has not finished loading.",
          remediation:
            "The configuration revision this request is checked against has not arrived yet, and a request submitted without one is refused. Wait a moment, then ask again."
        });
        return;
      }

      // A pending key is reused only for the target it was minted for.
      const held = pending.current;
      const idempotencyKey = held !== null && held.target === target ? held.key : newIdempotencyKey();
      pending.current = { target, key: idempotencyKey };

      setBusy(true);
      setFailure(null);
      act({ configRevision, idempotencyKey }).then(
        (result) => {
          // Done with this submission, so the next press is a new one.
          pending.current = null;
          setBusy(false);
          setOperation(result.operation);
          onDone?.(result);
        },
        (e: unknown) => {
          // The key is deliberately KEPT. Pressing the button again is
          // the same act retried, which is the whole point of the header.
          setBusy(false);
          setFailure(describeFailure(e, fallbackMessage));
        }
      );
    },
    [configRevision, fallbackMessage, ready]
  );

  const watchedId = operation === null ? null : operation.id;
  const settled = operation !== null && (operation.status === "completed" || operation.status === "failed");

  useEffect(() => {
    if (watchedId === null || settled) return;
    const timer = window.setInterval(() => {
      api.getOperation(watchedId).then(
        (latest) => setOperation(latest),
        // A poll that fails leaves the last good reading on screen. The
        // operation is durable and is still running; a page that blanked
        // itself because one re-read was refused would be reporting the
        // poll rather than the restore.
        () => undefined
      );
    }, WATCH_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [api, watchedId, settled]);

  return {
    ready,
    busy,
    operation,
    failure,
    submit,
    dismiss: useCallback(() => {
      setFailure(null);
      setOperation(null);
      // The refusal this key belonged to is off the screen, so the next
      // press is a new submission rather than a retry of something the
      // operator can no longer see.
      pending.current = null;
    }, [])
  };
}

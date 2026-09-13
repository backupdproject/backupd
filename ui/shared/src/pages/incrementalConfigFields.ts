/**
 * The editable half of an incremental set's configuration, as data
 * (EPIC K, issue #788).
 *
 * # Why this is a module and not five pieces of component state
 *
 * UpdateBackupSetRequest declares four incremental fields and the create
 * request declares six, and the two the patch does NOT declare are the
 * load-bearing part: a set's history belongs to its engine and lives in
 * the repository domain it was written to, so `engine` and
 * `repositoryDomain` are create-only and the configuration card draws them
 * as facts rather than as controls. Everything here is therefore about the
 * FIVE the operator can still change — the consistency declaration and the
 * four-part verification budget — and the one rule that matters about
 * them: a save sends only what actually changed.
 *
 * That rule is what makes this a pure module. A card with five boxes and a
 * hand-written save handler sends whatever the five boxes hold, which
 * quietly rewrites the four the operator never touched with values that
 * merely round-tripped through a text input. `incrementalPatch` compares
 * against the values LOADED and emits one key per genuine change, so a
 * save that changed the sample share is a patch with one field in it.
 *
 * # Everything is text
 *
 * For the same reason backupSetEditFields.ts holds strings: the dirty
 * check is against what was loaded rather than against the last keystroke,
 * and comparing strings is the only comparison that answers "typed a
 * character and deleted it" the same way as "never touched it".
 *
 * # Null is inherited, 0 is never, and they are different answers
 *
 * A cadence of null inherits the deployment's; a cadence of 0 is an
 * explicit "never raise the level on a schedule". The patch type carries
 * `number | undefined` and has no way to spell null, so a box that HAS a
 * value cannot be returned to inheritance from here — clearing it is
 * refused with that sentence rather than accepted and silently sent as a
 * 0, which would turn "use the deployment's cadence" into "never".
 */
import type { BackupSetPatch } from "@shared/api/contracts";
import type { IncrementalSettings } from "@shared/types/backup";
import type { BackupEngine, SourceConsistency, VerificationLevel } from "@shared/types/snapshot";

/** One day, in the seconds the wire carries. Both cadences are read and
 *  written in days because that is the unit the mock-up states them in
 *  and the unit an operator picks them in ("read every file every 7
 *  days"); ServiceBehaviourCard does the same thing with minutes. */
export const DAY_SECONDS = 86_400;

/**
 * The answers a NEW backup set starts from.
 *
 * There is no deployment-wide default for any of these on this contract:
 * GET /settings declares retention, capacity and service behaviour, and
 * nothing else, so what a new set starts with is what the wizard opens
 * with. Two screens state that — the wizard, which asks, and the
 * deployment's Backup defaults page, which reports what asking will
 * start from — so it is one constant rather than one constant and one
 * page repeating it from memory.
 *
 * `engine` is "artifact" deliberately and not by oversight:
 * BackupSetSpec.engine defaults to artifact server-side, and a UI that
 * pre-selected the other one would make its default and the contract's
 * disagree on the single choice a set can never change.
 */
export const NEW_SET_DEFAULTS: {
  engine: BackupEngine;
  sourceConsistency: SourceConsistency;
  verificationLevel: VerificationLevel;
  samplePercent: string;
  fullEveryDays: string;
  drillEveryDays: string;
} = {
  engine: "artifact",
  sourceConsistency: "live_best_effort",
  verificationLevel: "content_sample",
  samplePercent: "5",
  fullEveryDays: "7",
  drillEveryDays: "30"
};

/** The five editable fields, as an edit form holds them. `""` means the
 *  set inherits the deployment's answer for that field, which is a
 *  different state from any value it could hold. */
export interface IncrementalDraft {
  sourceConsistency: SourceConsistency | "";
  verificationLevel: VerificationLevel | "";
  /** The share of files a sampled verification reads, 1..100. */
  samplePercent: string;
  /** How often a run reads every file, in days. "0" is never. */
  fullEveryDays: string;
  /** How often a run performs a restore drill, in days. "0" is never. */
  drillEveryDays: string;
}

export type IncrementalFieldKey = keyof IncrementalDraft;

/** A stored cadence as its box holds it. It DIVIDES rather than rounds:
 *  the wire is seconds and a hand-written configuration may legally say
 *  thirty-six hours, and a box that rounded that to "2" would be dirty the
 *  moment it loaded and would write two days over a cadence nobody
 *  touched. */
export function daysDraft(seconds: number | null): string {
  return seconds === null ? "" : String(seconds / DAY_SECONDS);
}

/** What the five boxes hold when the card opens. */
export function readIncrementalDraft(settings: IncrementalSettings): IncrementalDraft {
  return {
    sourceConsistency: settings.sourceConsistency ?? "",
    verificationLevel: settings.verificationLevel ?? "",
    samplePercent:
      settings.verificationSamplePercent === null ? "" : String(settings.verificationSamplePercent),
    fullEveryDays: daysDraft(settings.verificationFullEverySeconds),
    drillEveryDays: daysDraft(settings.verificationRestoreDrillEverySeconds)
  };
}

/** The sentence a box that cannot be saved carries, or nothing. Compared
 *  against the baseline as well as parsed, because "left inherited" is
 *  legal and "cleared what was set" is not. */
export function incrementalFieldErrors(
  baseline: IncrementalDraft,
  draft: IncrementalDraft
): Partial<Record<IncrementalFieldKey, string>> {
  const errors: Partial<Record<IncrementalFieldKey, string>> = {};

  const cleared = (key: IncrementalFieldKey) =>
    draft[key].trim() === "" && baseline[key].trim() !== "";
  const INHERIT_IS_ONE_WAY =
    "This is set on this backup set, and the update route has no way to say" +
    " \u201cinherit again\u201d. Enter a value, or 0 for never.";

  if (draft.samplePercent.trim() !== "") {
    const percent = Number(draft.samplePercent.trim());
    if (!Number.isInteger(percent) || percent < 1 || percent > 100) {
      errors.samplePercent = "Enter a whole percentage between 1 and 100.";
    }
  } else if (cleared("samplePercent")) {
    errors.samplePercent = INHERIT_IS_ONE_WAY;
  }

  for (const key of ["fullEveryDays", "drillEveryDays"] as const) {
    const typed = draft[key].trim();
    if (typed === "") {
      if (cleared(key)) errors[key] = INHERIT_IS_ONE_WAY;
      continue;
    }
    const days = Number(typed);
    const exact = days * DAY_SECONDS;
    if (!Number.isFinite(days) || days < 0 || Math.abs(exact - Math.round(exact)) > 1e-6) {
      errors[key] = "Enter a number of days that lands on whole seconds, or 0 for never.";
    }
  }

  return errors;
}

/**
 * The patch a Save sends: one key per field that genuinely changed, and
 * nothing else.
 *
 * Fields still holding what they loaded are absent, so a save cannot
 * rewrite a budget the operator did not edit; a field still inherited is
 * absent for the same reason. The caller gates on
 * `incrementalFieldErrors` first — a draft that does not parse has no
 * business reaching the wire, and this function does not invent a value
 * for one that does not.
 */
export function incrementalPatch(
  baseline: IncrementalDraft,
  draft: IncrementalDraft
): BackupSetPatch {
  const patch: BackupSetPatch = {};
  const changed = (key: IncrementalFieldKey) => draft[key].trim() !== baseline[key].trim();

  if (changed("sourceConsistency") && draft.sourceConsistency !== "") {
    patch.sourceConsistency = draft.sourceConsistency;
  }
  if (changed("verificationLevel") && draft.verificationLevel !== "") {
    patch.verificationLevel = draft.verificationLevel;
  }
  if (changed("samplePercent") && draft.samplePercent.trim() !== "") {
    patch.verificationSamplePercent = Number(draft.samplePercent.trim());
  }
  if (changed("fullEveryDays") && draft.fullEveryDays.trim() !== "") {
    patch.verificationFullEverySeconds = Math.round(Number(draft.fullEveryDays.trim()) * DAY_SECONDS);
  }
  if (changed("drillEveryDays") && draft.drillEveryDays.trim() !== "") {
    patch.verificationRestoreDrillEverySeconds = Math.round(
      Number(draft.drillEveryDays.trim()) * DAY_SECONDS
    );
  }
  return patch;
}

/** Whether a Save would send anything at all. */
export function incrementalDirty(baseline: IncrementalDraft, draft: IncrementalDraft): boolean {
  return Object.keys(incrementalPatch(baseline, draft)).length > 0;
}

/** A cadence as a sentence, for the read-only summary above the form.
 *  Three answers, and the point of the function is that they are three:
 *  inherited, never, and a number of days. */
export function cadenceSummary(seconds: number | null): string {
  if (seconds === null) return "inherited from the deployment";
  if (seconds === 0) return "never on a cadence";
  const days = seconds / DAY_SECONDS;
  if (days >= 1 && Number.isInteger(days)) return "every " + days + (days === 1 ? " day" : " days");
  const hours = seconds / 3600;
  if (Number.isInteger(hours)) return "every " + hours + (hours === 1 ? " hour" : " hours");
  return "every " + seconds + " seconds";
}

/**
 * How a workflow run reads: the presentation tables, the five-stage
 * ladder, and the environment merge (issue #814).
 *
 * # Why this is a module and not three components
 *
 * Every rule in here is a decision about MEANING that more than one
 * surface has to make the same way. A step's state is drawn on the run
 * page and summarised on the backup set's panel; the stage ladder is the
 * run page's spine and the set panel's "what would run" list; the
 * environment merge is rendered at the deployment scope and at one set's,
 * from two different pairs of lists. A copy of any of those is a second
 * answer to a question an operator asks once.
 *
 * It also makes the rules assertable without a DOM, which is the same
 * reason LifecycleTimeline exports `buildPhases` separately from its
 * rendering.
 *
 * # Colour is never the message
 *
 * Every presentation entry pairs a tone with an ICON and a WORD, because
 * §9 forbids status carried by hue alone and because the word is the only
 * thing a screen reader gets. The tones come from the shared StatusBadge
 * vocabulary rather than from new CSS.
 *
 * # The one thing this module refuses to do
 *
 * It does not derive one verdict from another. A run carries three
 * statuses and they stay three: "the backup succeeded and the cleanup did
 * not" is the most operationally important thing this feature can report,
 * because it means a machine may be sitting quiesced with a good backup
 * beside it. Nothing here computes an overall verdict, and nothing should
 * be added that does.
 */
import type {
  BackupSetWorkflow,
  WorkflowEnvVariable,
  WorkflowPhase,
  WorkflowRun,
  WorkflowRunState,
  WorkflowScope,
  WorkflowStage,
  WorkflowStatus,
  WorkflowStepState,
  WorkflowStepSummary
} from "@shared/api/contracts";
import type { StatusTone } from "@shared/components/StatusBadge";
import type { IconName } from "@shared/design-system/icons";

/** One state's appearance: the tone, the icon that carries the meaning
 *  without colour, and the word a screen reader hears. */
export interface StatePresentation {
  tone: StatusTone;
  icon: IconName;
  label: string;
}

/**
 * The eight step states.
 *
 * "skipped" and "pending" are NEUTRAL rather than amber, and that is the
 * decision worth naming: a step that was never reached because an earlier
 * one failed is not itself a problem, and drawing a column of amber
 * behind one red failure buries the one row an operator has to read.
 * "interrupted" is amber because nobody knows what it did.
 */
export const STEP_PRESENTATION: Record<WorkflowStepState, StatePresentation> = {
  pending: { tone: "neutral", icon: "status-idle", label: "Pending" },
  running: { tone: "accent", icon: "status-active", label: "Running" },
  success: { tone: "ok", icon: "success", label: "Success" },
  failed: { tone: "danger", icon: "failure", label: "Failed" },
  timed_out: { tone: "danger", icon: "failure", label: "Timed out" },
  canceled: { tone: "warn", icon: "warning", label: "Canceled" },
  skipped: { tone: "neutral", icon: "status-idle", label: "Skipped" },
  interrupted: { tone: "warn", icon: "warning", label: "Interrupted" }
};

/**
 * The run states: the step vocabulary plus the four only a run can be in.
 *
 * `recovery_required` and `cleanup_failed` are the two that hold a backup
 * set, and both are drawn danger rather than warn: the consequence is a
 * source machine that may still be quiesced, which is worse than a failed
 * backup.
 */
export const RUN_PRESENTATION: Record<WorkflowRunState, StatePresentation> = {
  ...STEP_PRESENTATION,
  recovery_required: { tone: "danger", icon: "failure", label: "Recovery required" },
  cleanup_running: { tone: "accent", icon: "status-active", label: "Cleanup running" },
  cleanup_failed: { tone: "danger", icon: "failure", label: "Cleanup failed" },
  recovered: { tone: "ok", icon: "success", label: "Recovered" }
};

/**
 * The three verdicts' own vocabulary.
 *
 * "unknown" is NEUTRAL and says "Not reported". It is not an error and it
 * is not a success: a run this build received no verdict for is one
 * nobody should draw a conclusion from, which is the same rule the
 * client's mappers follow when they narrow an unrecognised value onto it.
 */
export const STATUS_PRESENTATION: Record<WorkflowStatus, StatePresentation> = {
  unknown: { tone: "neutral", icon: "status-idle", label: "Not reported" },
  running: { tone: "accent", icon: "status-active", label: "Running" },
  success: { tone: "ok", icon: "success", label: "Success" },
  failed: { tone: "danger", icon: "failure", label: "Failed" },
  skipped: { tone: "warn", icon: "warning", label: "Skipped" }
};

/** One of the five stages a run executes, in order. */
export interface StageKey {
  scope: WorkflowScope;
  phase: WorkflowPhase;
}

/**
 * The five stages, in the fixed order a run executes them.
 *
 * The backup itself is one of the five and is not a hook stage, which is
 * why it has no scope: leaving it out would draw an "after" stage
 * immediately below a "before" one and hide the thing the hooks are
 * wrapped around.
 */
export const WORKFLOW_STAGE_ORDER: ReadonlyArray<StageKey | "backup"> = [
  { scope: "global", phase: "before" },
  { scope: "set", phase: "before" },
  "backup",
  { scope: "set", phase: "after" },
  { scope: "global", phase: "after" }
];

export const STAGE_LABELS: Record<string, string> = {
  "global:before": "Global before",
  "set:before": "Backup set before",
  backup: "Backup",
  "set:after": "Backup set after",
  "global:after": "Global after"
};

/** One stage as a surface draws it. */
export interface StageView {
  /** "global:before", "backup", … — stable, and what a React key and a
   *  test both name a stage by. */
  key: string;
  label: string;
  /** Absent for the backup stage, which no scope configures. */
  scope?: WorkflowScope;
  phase?: WorkflowPhase;
  /** The directory this stage runs from, or "" when the configuration
   *  gives it none. */
  dir: string;
  /**
   * Whether this stage can run at all.
   *
   * A stage with no directory is drawn greyed and SAID rather than
   * omitted, because "global after did not run" and "this deployment has
   * no global after directory" are opposite facts when a source has been
   * left quiesced, and a ladder that hid the ineligible ones makes them
   * the same picture.
   */
  eligible: boolean;
  steps: WorkflowStepSummary[];
}

/**
 * The ladder one run is drawn as.
 *
 * `stages` is the configuration's answer about which stages have a
 * directory, and `steps` is what the run actually planned. Both, because
 * neither alone is enough: a run carries no record of a stage that was
 * configured and empty, and a configuration cannot say what a run from
 * last night did.
 *
 * Steps are ordered by their own `order` within a stage, which is the
 * engine's plan order and the only order a hook author can rely on.
 */
export function buildStageViews(
  steps: WorkflowStepSummary[],
  stages: WorkflowStage[]
): StageView[] {
  return WORKFLOW_STAGE_ORDER.map((entry) => {
    if (entry === "backup") {
      return { key: "backup", label: STAGE_LABELS.backup, dir: "", eligible: true, steps: [] };
    }
    const key = entry.scope + ":" + entry.phase;
    const configured = stages.find((s) => s.scope === entry.scope && s.phase === entry.phase);
    const own = steps
      .filter((step) => step.scope === entry.scope && step.phase === entry.phase)
      .sort((a, b) => a.order - b.order);
    return {
      key,
      label: STAGE_LABELS[key],
      scope: entry.scope,
      phase: entry.phase,
      dir: configured?.dir ?? "",
      // A stage the configuration did not report is still eligible when
      // the RUN has steps in it, which is not a contradiction: a run
      // outlives the configuration that produced it, so a stage whose
      // directory has since been cleared must still be drawn as the
      // stage that ran.
      eligible: configured !== undefined || own.length > 0,
      steps: own
    };
  });
}

/**
 * One stage's own verdict, for the badge on its head.
 *
 * This exists because of what a stage list looks like without it. The
 * requirement on the live screen is that an operator can tell which of
 * the five stages is executing AT A GLANCE, and a ladder whose stage
 * heads carry nothing forces them to scan every script row to find the
 * one that is running — which is exactly the reading the screen is meant
 * to save them. The badge answers "which stage" before the rows answer
 * "which script".
 *
 * The precedence is deliberate and is not "worst wins": RUNNING beats a
 * failure, because a stage with a failed step and another still executing
 * is a stage that has not finished, and telling an operator it failed
 * while it is still doing something is worse than telling them it is
 * busy. Everything below that is worst-first, since a stage that
 * succeeded three scripts and failed one did not succeed.
 */
export function stageStatus(stage: StageView): StatePresentation {
  if (!stage.eligible) {
    return { tone: "neutral", icon: "status-idle", label: "No directory configured" };
  }
  if (stage.steps.length === 0) {
    return { tone: "neutral", icon: "status-idle", label: "No scripts" };
  }
  const states = stage.steps.map((step) => step.state);
  if (states.includes("running")) return { tone: "accent", icon: "status-active", label: "Running now" };
  if (states.includes("failed") || states.includes("timed_out")) {
    return { tone: "danger", icon: "failure", label: "Failed" };
  }
  if (states.includes("interrupted")) return { tone: "warn", icon: "warning", label: "Interrupted" };
  if (states.includes("canceled")) return { tone: "warn", icon: "warning", label: "Canceled" };
  if (states.every((state) => state === "pending")) {
    return { tone: "neutral", icon: "status-idle", label: "Pending" };
  }
  if (states.includes("pending")) return { tone: "accent", icon: "status-active", label: "In progress" };
  if (states.every((state) => state === "skipped")) {
    return { tone: "neutral", icon: "status-idle", label: "Skipped" };
  }
  return { tone: "ok", icon: "success", label: "Done" };
}

/**
 * How long a step has been going, for a step that is still going.
 *
 * A running step carries no duration — the engine records one when it
 * finishes — so a page that only rendered `durationMs` shows an em dash
 * beside a spinner and gives an operator no idea whether a hook has been
 * hanging for three seconds or three minutes. The elapsed figure is
 * labelled "so far" because that is what it is: measured against this
 * browser's clock and not a figure the engine reported.
 */
export function elapsedLabel(step: WorkflowStepSummary, now = Date.now()): string {
  if (step.durationMs !== undefined && step.durationMs !== null) return durationLabel(step.durationMs);
  if (step.state !== "running" || !step.startedAt) return "\u2014";
  const started = Date.parse(step.startedAt);
  if (Number.isNaN(started)) return "\u2014";
  const elapsed = now - started;
  // The plausibility guard, and it is not defensive padding: this figure
  // is the difference between the SERVICE's clock and the BROWSER's, so a
  // machine whose clock is minutes out produces a number that is not a
  // measurement of anything. A negative one is that case outright, and
  // one far past the bound this step would have been killed at is the
  // same case in the other direction — the engine would have signalled
  // it. Neither is shown, because a wrong duration beside a spinner is
  // worse than no duration at all.
  const implausible = step.timeoutMs !== undefined && step.timeoutMs > 0 ? step.timeoutMs * 2 : DAY_MS;
  if (elapsed < 0 || elapsed > implausible) return "\u2014";
  return durationLabel(elapsed) + " so far";
}

const DAY_MS = 86_400_000;

/**
 * Which identity a step's row prints, and it is the wording rule for the
 * whole feature.
 *
 * A local step runs on the machine retnd is installed on, executed by
 * the HOST WORKFLOW RUNNER. It does not run "in the engine container",
 * and no surface may imply the container grew a shell: the runner is a
 * separate component precisely because the container has no shell for a
 * hook, and an operator told otherwise will write a hook against a
 * filesystem that is not there.
 *
 * A remote step's identity is the host when the client could name one and
 * the execution connection's reference otherwise — never a guess.
 */
export function describeStepExecutor(step: WorkflowStepSummary): string {
  if (step.target === "local") return "Local Host \u00b7 Host Workflow Runner";
  return step.remoteHost ?? step.executionConnectionRef ?? "an execution connection";
}

/** The badge word for where a step runs. Two values and no third: a
 *  reader deciding whether a hook can see a path needs this answer in one
 *  glance. */
export function targetLabel(target: "local" | "remote"): string {
  return target === "local" ? "Local Host" : "Remote";
}

/**
 * The first seven hex digits of a script hash, or an em dash.
 *
 * An em dash and never a truncated empty string, because the absence is
 * meaningful on a run: the journal does not carry the hash a run was
 * planned against, so a run's rows have none to print, and rendering the
 * validation report's hash there instead would be a claim about what
 * executed that nothing supports.
 */
export function shortSha(sha: string | undefined): string {
  return sha && sha.length >= 7 ? sha.slice(0, 7) : "\u2014";
}

/** A step's exit code as a cell. `null` is NOT zero: a step that produced
 *  no code either never ran or was signalled and never reported one, and
 *  "0" there would read as a clean success. */
export function exitCodeLabel(exitCode: number | null | undefined): string {
  return exitCode === null || exitCode === undefined ? "\u2014" : String(exitCode);
}

/** A measured duration in the unit an operator reads. Sub-second work is
 *  shown in milliseconds rather than as "0s", which is the same rule the
 *  API applies when it reports run-time facts in milliseconds and
 *  configured bounds in seconds. */
export function durationLabel(ms: number | null | undefined): string {
  if (ms === null || ms === undefined) return "\u2014";
  if (ms < 1_000) return ms + "ms";
  if (ms < 60_000) return (ms / 1_000).toFixed(1) + "s";
  const minutes = Math.floor(ms / 60_000);
  const seconds = Math.round((ms % 60_000) / 1_000);
  return minutes + "m " + seconds + "s";
}

/**
 * Whether a run is still going, which decides whether a surface polls.
 *
 * `cleanup_running` counts, and that is the whole reason this is a
 * function rather than a check against "running": a canceled run whose
 * after hooks are still executing is a live run, and a page that stopped
 * polling on cancellation would freeze at the exact moment an operator
 * is watching to see whether their source gets un-quiesced.
 */
export function isRunLive(run: WorkflowRun): boolean {
  return run.state === "running" || run.state === "cleanup_running" || run.state === "pending";
}

/**
 * Every step whose termination this product could not confirm.
 *
 * `terminationConfirmed === false` only: an ABSENT flag is a run the
 * engine reported nothing about, which is not the same as an unconfirmed
 * kill and must not raise a banner saying a script may still be running.
 */
export function unconfirmedTerminations(steps: WorkflowStepSummary[]): WorkflowStepSummary[] {
  return steps.filter((step) => step.terminationConfirmed === false);
}

/** Which scope an effective environment value came from. "builtin" is
 *  this product's own injection and is not a scope an operator can
 *  write. */
export type EnvSource = "global" | "set" | "builtin";

export const ENV_SOURCE_LABELS: Record<EnvSource, string> = {
  global: "Global",
  set: "Backup Set",
  builtin: "Built-in"
};

/**
 * The variables this product injects into every hook's environment, in
 * documented order (core/internal/workflow/env.go's `builtinEnvNames`).
 *
 * Listed rather than derived, because the enumeration IS the contract a
 * hook author writes against: a variable that is set but not listed is
 * one nobody can rely on. They are reserved — the whole `RETND_` prefix
 * is — so an operator cannot configure one, which is why they are
 * read-only on every surface: a hook reading `RETND_BACKUP_STATUS` has
 * to be reading what this product observed rather than a value somebody
 * wrote into a config file.
 *
 * Each one is ALSO exported under its previous `BACKUPD_` spelling for
 * one release (EPIC R, #885, FR-37), and that compat block is
 * deliberately absent from this list: it is what a hook written against
 * the old name still reads, not something a new hook should be written
 * against, and a surface that offered both would be documenting the name
 * that is going away.
 */
export const BUILTIN_ENV_NAMES: readonly string[] = [
  "RETND",
  "RETND_RUN_ID",
  "RETND_BACKUP_SET_ID",
  "RETND_BACKUP_SET_NAME",
  "RETND_PHASE",
  "RETND_STEP_ID",
  "RETND_STEP_NAME",
  "RETND_STEP_TARGET",
  "RETND_SOURCE_HOST",
  "RETND_SOURCE_PATH",
  "RETND_DESTINATION",
  "RETND_WORK_DIR",
  "RETND_BACKUP_STATUS",
  "RETND_WORKFLOW_STATUS",
  "RETND_CLEANUP_STATUS",
  "RETND_BACKUP_ERROR_CODE",
  "RETND_STARTED_AT",
  "RETND_RECOVERY"
];

/** The prefix this product reserves, and the bare name beside it.
 *
 *  A name matching either is refused by the service, so the editor
 *  refuses it in front of the request rather than sending one that
 *  cannot succeed. The previous `BACKUPD_` prefix and its bare
 *  `BACKUPD` are refused on the same terms while this release still
 *  exports them (core/internal/workflow.IsReservedEnvName is the rule
 *  this mirrors): a variable an operator could save under a name the
 *  engine overwrites per run is one that looks like it works. */
export function isReservedEnvName(name: string): boolean {
  return (
    name === "RETND" ||
    name.startsWith("RETND_") ||
    name === "BACKUPD" ||
    name.startsWith("BACKUPD_")
  );
}

/** One row of the merged preview: what a hook will actually receive. */
export interface MergedEnvVariable {
  name: string;
  /** The winning entry, or undefined for a built-in, whose value this
   *  product supplies per run and which no read can report. */
  entry?: WorkflowEnvVariable;
  source: EnvSource;
  /** Every scope that ALSO set this name and lost. Rendered, because an
   *  operator who cannot see that a set-scope value is hiding the
   *  deployment's has no way to explain a hook reaching the wrong
   *  host. */
  shadowed: EnvSource[];
  /** Whether the winning value comes from a secret reference, in which
   *  case no surface may print a value at all. */
  secret: boolean;
}

/**
 * The merge, in the engine's own precedence order.
 *
 * `sanitized baseline < workflows.environment < backup-set environment <
 * RETND_* built-ins` (core/internal/workflow's package doc). The
 * baseline is not represented here because it is not configuration and no
 * read reports it; the other three are, and the built-ins win outright,
 * which is what makes them read-only rather than merely discouraged.
 *
 * Sorted by name, with the built-ins last: an operator scanning for a
 * variable they wrote should not have to read past eighteen of ours.
 */
export function mergeWorkflowEnvironment(
  global: WorkflowEnvVariable[],
  set: WorkflowEnvVariable[]
): MergedEnvVariable[] {
  const rows = new Map<string, MergedEnvVariable>();

  for (const entry of global) {
    rows.set(entry.name, {
      name: entry.name,
      entry,
      source: "global",
      shadowed: [],
      secret: entry.secret !== undefined
    });
  }
  for (const entry of set) {
    const existing = rows.get(entry.name);
    rows.set(entry.name, {
      name: entry.name,
      entry,
      source: "set",
      // The deployment's entry is what this one is hiding, which is the
      // fact the preview exists to show.
      shadowed: existing ? [existing.source, ...existing.shadowed] : [],
      secret: entry.secret !== undefined
    });
  }

  // A configured name inside the reserved namespace is dropped from the
  // configured half rather than listed twice: the built-in below carries
  // the collision, and two rows with the same NAME would be two React
  // children with the same key in every table that renders this.
  const configured = [...rows.values()]
    .filter((row) => !isReservedEnvName(row.name))
    .sort((a, b) => a.name.localeCompare(b.name));

  const builtins: MergedEnvVariable[] = BUILTIN_ENV_NAMES.map((name) => {
    // A built-in cannot be shadowed, and the reverse is what is recorded:
    // anything an operator managed to configure under this name is what
    // gets hidden. The service refuses such a write, so this is a
    // defensive reading of a configuration written before the
    // reservation, not an expected state.
    const collision = rows.get(name);
    return {
      name,
      source: "builtin",
      shadowed: collision ? [collision.source] : [],
      secret: false
    };
  });

  return [...configured, ...builtins];
}

/**
 * Whether a backup set has anything workflow-shaped to show at all.
 *
 * The gate on every new surface this issue adds to an existing page. A
 * set that configures no hooks must show no panel, no empty table and no
 * findings — one line saying so, and nothing else. New noise on every
 * existing set is the failure mode this whole wave is one edit away from,
 * and `configured` alone is not enough: a set can have `configured` false
 * and still inherit the deployment's global stages, which ARE hooks that
 * will run against it.
 */
export function hasWorkflowSurface(
  workflow: BackupSetWorkflow | null,
  globalStages: WorkflowStage[]
): boolean {
  if (workflow === null) return false;
  return workflow.configured || workflow.stages.length > 0 || globalStages.length > 0;
}

/**
 * What a backup set is configured to do, drawn for the engine it actually
 * runs (EPIC K, issue #788).
 *
 * # The contrast is the feature
 *
 * The mock-up's two detail screens are the same page for an incremental
 * set and for an artifact set, and the whole epic turns on a reader never
 * mistaking one for the other. So this card has two branches and they do
 * not converge: an artifact set's repository domain, consistency mode and
 * verification level are ABSENT here rather than drawn empty, because they
 * are not properties an artifact set has. A row reading "Repository
 * domain: —" would say this set has one and it is unset.
 *
 * # Two of the six are facts, not controls
 *
 * `engine` and `repository_domain` are create-only:
 * UpdateBackupSetRequest declares neither, because a set's history belongs
 * to its engine and lives in the domain it was written to, and changing
 * either would leave everything already collected behind and start a
 * second, unrelated lineage. They are stated as cells with the wire field
 * beside them, and the sentence under them says why there is no control.
 *
 * # Why this saves without taking the edit hold
 *
 * The hold (issue #350) stops a cycle running against a set while its
 * DEFINITION is being edited in place, box by box, and it is what the
 * inline edit list above this card enters. This card follows
 * BackupSetRetentionCard instead, which writes its own policy the same
 * way: one form, one Save, one patch containing only what changed. What it
 * writes is a statement about how hard FUTURE runs are checked, so a run
 * in flight neither invalidates the edit nor is invalidated by it.
 */
import { useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { Cell, CellGrid, Note, Row, Rows } from "@shared/components/Definitions";
import { Choice } from "@shared/components/Choice";
import { CONSISTENCY_COPY, ENGINE_COPY, VERIFICATION_COPY } from "@shared/components/EngineBadge";
import { ErrorState } from "@shared/components/EmptyState";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import { describeFailure } from "@shared/api/failure";
import type { BackupSet, IncrementalSettings } from "@shared/types/backup";
import type { SourceConsistency, VerificationLevel } from "@shared/types/snapshot";
import {
  cadenceSummary,
  incrementalDirty,
  incrementalFieldErrors,
  incrementalPatch,
  readIncrementalDraft
} from "@shared/pages/incrementalConfigFields";
import type { IncrementalDraft } from "@shared/pages/incrementalConfigFields";

/** The order the two closed questions are asked in, everywhere. The
 *  wizard walks the same sequences, so an operator returning to this card
 *  finds the options where the wizard left them. */
const CONSISTENCY_ORDER: SourceConsistency[] = [
  "live_best_effort",
  "externally_quiesced",
  "external_snapshot"
];
const VERIFICATION_ORDER: VerificationLevel[] = [
  "structural",
  "content_sample",
  "content_full",
  "restore_drill"
];

export function BackupSetConfigurationCard({
  set,
  readOnly,
  onSaved
}: {
  set: BackupSet;
  readOnly: boolean;
  /** Re-read the set after a successful save, so this card and everything
   *  else on the page are showing one moment. */
  onSaved(): void;
}) {
  const settings = set.incremental;
  if (settings === null) return <ArtifactConfiguration set={set} />;
  // No "saved" banner here, and that is a finding rather than an
  // omission: a successful save reloads the set, and the detail page
  // renders NOTHING at all while its own read is in flight
  // (BackupSetDetailPage: `if (!set.data || set.loading) return null`), so
  // this whole card unmounts and remounts on every save. Any confirmation
  // held in its state would be discarded before it could be read. What
  // reports the save instead is the card itself: it comes back showing the
  // values the service now holds, which is the evidence rather than a
  // claim about it.
  return (
    <IncrementalConfiguration
      // Remounts when the persisted settings change, so the form's
      // baseline is always what the service last said rather than what
      // this component loaded the first time it rendered.
      key={JSON.stringify(settings)}
      set={set}
      settings={settings}
      readOnly={readOnly}
      onSaved={onSaved}
    />
  );
}

/** The artifact branch. Deliberately the same shape and size as the
 *  incremental one: an artifact set is not a lesser case of an incremental
 *  set, and a panel that shrank to two rows would read as one. */
function ArtifactConfiguration({ set }: { set: BackupSet }) {
  return (
    <>
      <CellGrid>
        <Cell label="Engine" value={ENGINE_COPY.artifact.name} wire="engine=artifact" />
        <Cell label="Completion method" value={COMPLETION_LABEL[set.completionMethod]} />
        <Cell
          label="Validation"
          value={
            set.validations
              .map((v) =>
                v === "transfer"
                  ? "Transfer"
                  : v === "checksum"
                    ? "Checksum (SHA-256)"
                    : "Application"
              )
              .join(" \u00b7 ") || "Transfer only"
          }
        />
        <Cell
          label="Schedule"
          value={"every " + Math.round(set.effectivePollIntervalSeconds / 60) + " min"}
          wire="poll_interval_seconds"
          mono
        />
      </CellGrid>
      <Note>
        {ENGINE_COPY.artifact.summary +
          " The fields an incremental set carries are absent here rather than shown empty: a" +
          " repository domain, a source-consistency declaration and a verification level are not" +
          " part of what an artifact set is."}
      </Note>
    </>
  );
}

const COMPLETION_LABEL: Record<BackupSet["completionMethod"], string> = {
  "completion-marker": "Completion marker",
  "atomic-rename": "Atomic rename",
  "stable-size": "Stable size"
};

function IncrementalConfiguration({
  set,
  settings,
  readOnly,
  onSaved
}: {
  set: BackupSet;
  settings: IncrementalSettings;
  readOnly: boolean;
  onSaved(): void;
}) {
  const api = useApi();
  const [baseline] = useState<IncrementalDraft>(() => readIncrementalDraft(settings));
  const [draft, setDraft] = useState<IncrementalDraft>(baseline);
  const [editing, setEditing] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<{ message: string; correlationId?: string } | null>(
    null
  );

  const errors = incrementalFieldErrors(baseline, draft);
  const invalid = Object.keys(errors).length > 0;
  const dirty = incrementalDirty(baseline, draft);

  const save = async () => {
    if (readOnly || invalid || !dirty || saving) return;
    setSaving(true);
    setSaveError(null);
    try {
      await api.updateBackupSet(set.source, set.set, incrementalPatch(baseline, draft));
      setEditing(false);
      onSaved();
    } catch (e) {
      const failure = describeFailure(e, "Backupd could not save this set's incremental settings.");
      setSaveError({
        message: failure.message,
        ...(failure.correlationId ? { correlationId: failure.correlationId } : {})
      });
    } finally {
      setSaving(false);
    }
  };

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      {/* The two answers that were settled when the set was created. They
          are cells rather than controls because the update route has no
          field for either, and a disabled <select> would still be a
          control an operator expects to become usable. */}
      <div>
        <CellGrid>
          <Cell
            label="Engine"
            value={ENGINE_COPY.kopia.name}
            wire="engine=kopia"
            tip="wizard.incremental.engine"
          />
          <Cell
            label="Repository domain"
            value={settings.repositoryDomain ?? "not declared"}
            wire="repository_domain"
            mono
            tip="wizard.incremental.domain"
          />
          <Cell label="Set identity in the repository" value={set.id} wire="backup_set_id" mono />
        </CellGrid>
        <Note>
          Both are fixed after creation. Snapshots live in the domain they were written to and
          belong to the engine that wrote them, so pointing this set at either somewhere else
          would leave its whole history behind and start again from nothing.
        </Note>
      </div>

      {editing ? (
        <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
          <fieldset style={FIELDSET} disabled={saving}>
            <legend style={LEGEND}>
              <InfoTooltip id="wizard.incremental.consistency">
                <span>Source consistency</span>
              </InfoTooltip>
            </legend>
            <div style={CHOICE_GRID}>
              {CONSISTENCY_ORDER.map((mode) => (
                <Choice
                  key={mode}
                  name="set-consistency"
                  title={CONSISTENCY_COPY[mode].name}
                  wire={"source_consistency=" + mode}
                  detail={CONSISTENCY_COPY[mode].summary}
                  checked={draft.sourceConsistency === mode}
                  onChange={() => setDraft({ ...draft, sourceConsistency: mode })}
                >
                  <span style={CHOICE_FOOT}>{"Point in time: " + CONSISTENCY_COPY[mode].pointInTime}</span>
                </Choice>
              ))}
            </div>
            <Note>
              Backupd records what you arranged and reports a run that contradicts it. It never
              guesses which mode a source is in.
            </Note>
          </fieldset>

          <fieldset style={FIELDSET} disabled={saving}>
            <legend style={LEGEND}>
              <InfoTooltip id="wizard.incremental.verification">
                <span>Verification</span>
              </InfoTooltip>
            </legend>
            <div style={CHOICE_GRID}>
              {VERIFICATION_ORDER.map((level) => (
                <Choice
                  key={level}
                  name="set-verification"
                  title={VERIFICATION_COPY[level].name}
                  wire={"verification_level=" + level}
                  detail={VERIFICATION_COPY[level].finds}
                  checked={draft.verificationLevel === level}
                  onChange={() => setDraft({ ...draft, verificationLevel: level })}
                >
                  <span style={CHOICE_FOOT}>{VERIFICATION_COPY[level].cost}</span>
                </Choice>
              ))}
            </div>
            <div
              style={{
                marginTop: 14,
                display: "grid",
                gridTemplateColumns: "repeat(auto-fit, minmax(228px, 1fr))",
                gap: "15px 18px"
              }}
            >
              <BudgetField
                label="Sampled share of files (%)"
                wire="verification_sample_percent"
                value={draft.samplePercent}
                error={errors.samplePercent}
                onChange={(next) => setDraft({ ...draft, samplePercent: next })}
              />
              <BudgetField
                label="Read every file every (days)"
                wire="verification_full_every_seconds"
                value={draft.fullEveryDays}
                error={errors.fullEveryDays}
                onChange={(next) => setDraft({ ...draft, fullEveryDays: next })}
              />
              <BudgetField
                label="Restore drill every (days)"
                wire="verification_restore_drill_every_seconds"
                value={draft.drillEveryDays}
                error={errors.drillEveryDays}
                onChange={(next) => setDraft({ ...draft, drillEveryDays: next })}
              />
            </div>
            <Note>
              A run that proves less than the level above fails, and only a run that proves it can
              become this set&rsquo;s newest known-good restore point. The two cadences may raise the
              bar for one run; nothing lowers it. An empty box is inherited from the deployment, and
              0 is never.
            </Note>
          </fieldset>

          {saveError ? (
            <ErrorState
              message={saveError.message}
              remediation="Nothing was saved. This set is still running the settings above."
              {...(saveError.correlationId ? { correlationId: saveError.correlationId } : {})}
            />
          ) : null}

          <div style={{ display: "flex", gap: 9, flexWrap: "wrap" }}>
            <button
              className="btn btn--primary"
              type="button"
              disabled={readOnly || invalid || !dirty || saving}
              onClick={() => void save()}
            >
              {saving ? "Saving…" : "Save incremental settings"}
            </button>
            <button
              className="btn"
              type="button"
              disabled={saving}
              onClick={() => {
                setDraft(baseline);
                setSaveError(null);
                setEditing(false);
              }}
            >
              Cancel
            </button>
            {dirty ? null : (
              <span style={{ alignSelf: "center", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                Nothing has changed yet.
              </span>
            )}
          </div>
        </div>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
          <Rows>
            <Row
              label="Source consistency"
              wire="source_consistency"
              tip="wizard.incremental.consistency"
              value={
                settings.sourceConsistency === null
                  ? "Inherited from the deployment"
                  : CONSISTENCY_COPY[settings.sourceConsistency].name
              }
            />
            <Row
              label="Verification level"
              wire="verification_level"
              tip="wizard.incremental.verification"
              value={
                settings.verificationLevel === null
                  ? "Inherited from the deployment"
                  : VERIFICATION_COPY[settings.verificationLevel].name
              }
            />
            <Row
              label="Sampled share of files"
              wire="verification_sample_percent"
              value={
                settings.verificationSamplePercent === null
                  ? "Inherited from the deployment"
                  : settings.verificationSamplePercent + "%"
              }
            />
            <Row
              label="Read every file"
              wire="verification_full_every_seconds"
              value={cadenceSummary(settings.verificationFullEverySeconds)}
            />
            <Row
              label="Restore drill"
              wire="verification_restore_drill_every_seconds"
              value={cadenceSummary(settings.verificationRestoreDrillEverySeconds)}
            />
          </Rows>

          <div>
            <button
              className="btn btn--sm"
              type="button"
              disabled={readOnly}
              onClick={() => setEditing(true)}
            >
              Edit incremental settings
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

/** One number in the verification budget, with the sentence that refuses
 *  it underneath. A number input, because these three are numbers and a
 *  NAS console is frequently driven on a tablet where that is the
 *  difference between a keypad and a keyboard. */
function BudgetField({
  label,
  wire,
  value,
  error,
  onChange
}: {
  label: string;
  wire: string;
  value: string;
  error?: string;
  onChange(next: string): void;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 5 }}>
      <label className="field">
        <span className="field__label">{label}</span>
        <input
          className="input input--mono"
          type="number"
          min={0}
          step="any"
          value={value}
          placeholder="inherited"
          aria-invalid={error ? true : undefined}
          onChange={(e) => onChange(e.target.value)}
        />
      </label>
      <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
        {wire}
      </span>
      {error ? (
        <span style={{ fontSize: "var(--text-sm)", color: "var(--danger)" }}>{error}</span>
      ) : null}
    </div>
  );
}

const FIELDSET: React.CSSProperties = { margin: 0, padding: 0, border: "none" };
const LEGEND: React.CSSProperties = {
  padding: 0,
  marginBottom: 10,
  fontSize: 13,
  fontWeight: 600
};
const CHOICE_GRID: React.CSSProperties = {
  display: "grid",
  gridTemplateColumns: "repeat(auto-fit, minmax(250px, 1fr))",
  gap: 10
};
const CHOICE_FOOT: React.CSSProperties = {
  display: "block",
  marginTop: 6,
  fontSize: "var(--text-xs)",
  color: "var(--text-3)"
};

/**
 * The step rail, the step body and the Back/Continue pair every guided
 * flow in this product draws, promoted out of `BackupSetWizardPage` for
 * EPIC K (issue #788).
 *
 * Three flows use them now — adding a backup set, restoring from a
 * snapshot, and whatever comes next — and the promotion is what keeps the
 * three looking like one product. The mock-up copied these shapes into
 * `src/mockup/parts.tsx` and its design doc records the intended fix as
 * promoting them here instead of copying them again; this is that.
 *
 * The one real change on the way out of the wizard is that the rail takes
 * its step count from its own list rather than hardcoding six columns.
 * That was the single line standing between "the wizard's rail" and "a
 * rail", and it is why the restore flow can be four steps and the add
 * flow eight without either of them drawing its own.
 */
import type { ReactNode } from "react";
import { Icon } from "@shared/design-system/icons";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import type { TooltipId } from "@shared/tooltips/tooltips";

/** The heading and lede a step opens with. */
export function StepBody({
  title,
  lede,
  children
}: {
  title: string;
  lede: string;
  children: ReactNode;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 18 }}>
      <div>
        <h2>{title}</h2>
        <p style={{ margin: "5px 0 0", fontSize: 13, color: "var(--text-2)" }}>{lede}</p>
      </div>
      <div>{children}</div>
    </div>
  );
}

/**
 * The numbered rail, one column per step, every one of them a button.
 *
 * Every step is reachable at any time and that is deliberate: a rail
 * whose later steps are disabled until the earlier ones are "done" cannot
 * be used to go back and check an answer, which is the thing operators
 * actually do with it. What refuses is the SAVE, and it says why.
 *
 * `tip` is one registry id for the whole rail rather than one per step:
 * what a step is, and that any of them can be revisited, is one
 * explanation (#834).
 */
export function StepRail({
  steps,
  step,
  onSelect,
  tip
}: {
  steps: readonly string[];
  step: number;
  onSelect(n: number): void;
  tip: TooltipId;
}) {
  return (
    <ol
      style={{
        margin: 0,
        padding: 0,
        listStyle: "none",
        display: "grid",
        gridTemplateColumns: "repeat(" + steps.length + ", 1fr)",
        gap: 8
      }}
    >
      {steps.map((label, i) => {
        const n = i + 1;
        const active = step === n;
        const done = step > n;
        return (
          <li key={label}>
            {/* The host wraps the button, so the button keeps its own
                accessible name. */}
            <InfoTooltip id={tip} block>
              <button
                onClick={() => onSelect(n)}
                // Without this the accessible name is "01 Engine",
                // because the step-number span is part of the button. A
                // screen reader should hear the step, not the numeral
                // glued to the label.
                aria-label={label}
                aria-current={active ? "step" : undefined}
                style={{
                  display: "flex",
                  flexDirection: "column",
                  gap: 5,
                  width: "100%",
                  height: "100%",
                  padding: "9px 10px",
                  borderRadius: "var(--radius-lg)",
                  textAlign: "left",
                  border: "1px solid " + (active ? "var(--accent)" : "var(--border)"),
                  background: active ? "var(--accent-quiet)" : "var(--surface)",
                  color: active ? "var(--text)" : "var(--text-2)",
                  font: "inherit",
                  cursor: "pointer"
                }}
              >
                <span
                  className="mono"
                  style={{ display: "flex", alignItems: "center", gap: 7, fontSize: "var(--text-xs)" }}
                >
                  {(n < 10 ? "0" : "") + n}
                  <span
                    aria-hidden="true"
                    style={{ color: "var(--ok)", opacity: done ? 1 : 0, display: "inline-flex" }}
                  >
                    <Icon name="success" size={11} />
                  </span>
                </span>
                <span style={{ fontSize: "var(--text-sm)", fontWeight: 500 }}>{label}</span>
              </button>
            </InfoTooltip>
          </li>
        );
      })}
    </ol>
  );
}

/**
 * The footer a step ends with: Back, where you are, and Continue.
 *
 * On the last step Continue becomes the flow's own commit label, and
 * `onFinish` is a separate callback rather than `onNext` with a branch
 * inside it, because the two do genuinely different things and a caller
 * that forgot the branch would advance past the end of its own rail.
 */
export function StepControls({
  step,
  total,
  onBack,
  onNext,
  finishLabel,
  onFinish,
  finishDisabled,
  finishHint
}: {
  step: number;
  total: number;
  onBack(): void;
  onNext(): void;
  finishLabel: string;
  onFinish(): void;
  finishDisabled?: boolean;
  /** Why the commit is refused, in words, beside the disabled button. A
   *  disabled control announces nothing about why it is disabled, so a
   *  flow that has one owes this sentence. */
  finishHint?: string;
}) {
  const last = step === total;
  return (
    <div
      style={{
        display: "flex",
        alignItems: "center",
        gap: 10,
        padding: "14px 22px",
        borderTop: "1px solid var(--border)",
        background: "var(--surface-2)",
        flexWrap: "wrap"
      }}
    >
      <button className="btn" disabled={step === 1} onClick={onBack}>
        Back
      </button>
      <div style={{ flex: 1, fontSize: "var(--text-sm)", color: "var(--text-3)", minWidth: 120 }}>
        {last && finishHint ? finishHint : "Step " + step + " of " + total}
      </div>
      {last ? (
        <button className="btn btn--primary" disabled={finishDisabled} onClick={onFinish}>
          {finishLabel}
        </button>
      ) : (
        <button className="btn btn--primary" onClick={onNext}>
          Continue
        </button>
      )}
    </div>
  );
}

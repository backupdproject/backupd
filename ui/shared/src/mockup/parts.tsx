/**
 * The pieces every mock-up screen is built out of (issue #788).
 *
 * Nothing here is a new design. Each one is the shape an existing page
 * already draws — `BackupSetDetailPage`'s card-with-an-eyebrow-heading and
 * its `Cell`/`Row` definition pairs, `BackupSetWizardPage`'s step rail,
 * `Choice` and `Toggle`, `DashboardPage`'s metric strip — lifted here so
 * the mock-up cannot drift from the product it is proposing an addition
 * to. Where a shape is copied rather than imported it is because the
 * original is a private function inside a page; the production UI wave
 * should promote those instead of copying them again, and the design doc
 * says so.
 *
 * The one thing that is genuinely new is `WireName`: the api/v1 field
 * behind an operator-facing label, in monospace. It exists because this
 * artifact is reviewed by people checking two different things at once —
 * that the words make sense to an operator, and that the screen is
 * spending the contract #788 defines rather than inventing one.
 */
import type { CSSProperties, ReactNode } from "react";
import { StatusBadge } from "@shared/components/StatusBadge";
import type { StatusTone } from "@shared/components/StatusBadge";
import { Icon } from "@shared/design-system/icons";
import type { IconName } from "@shared/design-system/icons";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import type { TooltipId } from "@shared/tooltips/tooltips";
import { ENGINE_COPY } from "@shared/mockup/data";
import type { MockEngine } from "@shared/mockup/data";

/** The separator every list in this app uses between a value and its
 *  unit, or one chip and the next. Braced rather than written as JSX text
 *  so it is a JavaScript string literal in both positions. */
export const DOT = " \u00b7 ";

/** A card with the eyebrow heading `BackupSetDetailPage.Section` draws. */
export function MockCard({
  title,
  tip,
  actions,
  children
}: {
  title: string;
  tip?: TooltipId;
  actions?: ReactNode;
  children: ReactNode;
}) {
  const heading = <h2 className="eyebrow">{title}</h2>;
  return (
    <section className="card">
      <div className="card__header">
        {tip ? <InfoTooltip id={tip}>{heading}</InfoTooltip> : heading}
        {actions ? <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>{actions}</div> : null}
      </div>
      <div className="card__body">{children}</div>
    </section>
  );
}

/** The api/v1 field a value comes from, for the reviewer rather than the
 *  operator. Quiet enough to read past. */
export function WireName({ name }: { name: string }) {
  return (
    <span
      className="mono"
      style={{ fontSize: "var(--text-xs)", color: "var(--text-3)", whiteSpace: "nowrap" }}
    >
      {name}
    </span>
  );
}

/** A definition row: label on the left, value on the right, inside a
 *  two-column `<dl>` the caller owns. */
export function Row({
  label,
  value,
  wire,
  mono,
  tip
}: {
  label: string;
  value: ReactNode;
  wire?: string;
  mono?: boolean;
  tip?: TooltipId;
}) {
  const term = (
    <span style={{ display: "inline-flex", alignItems: "baseline", gap: 8, flexWrap: "wrap" }}>
      {label}
      {wire ? <WireName name={wire} /> : null}
    </span>
  );
  return (
    <>
      <dt style={{ color: "var(--text-2)" }}>{tip ? <InfoTooltip id={tip}>{term}</InfoTooltip> : term}</dt>
      <dd className={mono ? "mono" : undefined} style={{ margin: 0 }}>
        {value}
      </dd>
    </>
  );
}

/** The `<dl>` the rows above sit in. */
export function Rows({ children, columns = 2 }: { children: ReactNode; columns?: number }) {
  return (
    <dl
      style={{
        margin: 0,
        display: "grid",
        gridTemplateColumns: columns === 1 ? "1fr" : "minmax(180px, max-content) 1fr",
        gap: "10px 20px",
        fontSize: 13,
        alignItems: "baseline"
      }}
    >
      {children}
    </dl>
  );
}

/** A boxed summary tile, the shape the detail page's `Cell` draws. */
export function Cell({
  label,
  value,
  wire,
  mono,
  tone
}: {
  label: string;
  value: ReactNode;
  wire?: string;
  mono?: boolean;
  tone?: "quiet";
}) {
  return (
    <div
      style={{
        padding: "12px 14px",
        border: "1px solid var(--border)",
        borderRadius: "var(--radius-lg)",
        background: tone === "quiet" ? "var(--surface-2)" : "var(--surface)"
      }}
    >
      <div className="eyebrow" style={{ fontSize: 10.5, letterSpacing: "0.06em" }}>
        {label}
      </div>
      <div
        style={{
          marginTop: 5,
          fontSize: 14,
          fontFamily: mono ? "var(--font-mono)" : undefined,
          wordBreak: mono ? "break-all" : undefined
        }}
      >
        {value}
      </div>
      {wire ? (
        <div style={{ marginTop: 4 }}>
          <WireName name={wire} />
        </div>
      ) : null}
    </div>
  );
}

/** A responsive grid of `Cell`s. */
export function CellGrid({ children, min = 200 }: { children: ReactNode; min?: number }) {
  return (
    <div
      style={{
        display: "grid",
        gridTemplateColumns: "repeat(auto-fit, minmax(" + min + "px, 1fr))",
        gap: 10
      }}
    >
      {children}
    </div>
  );
}

/** Which engine a set runs, stated the same way everywhere it appears.
 *  Artifact is neutral and incremental is accented, because the accent is
 *  what the eye finds in a list where most rows are the old kind. */
export function EngineBadge({ engine }: { engine: MockEngine }) {
  const copy = ENGINE_COPY[engine];
  return (
    <StatusBadge
      tone={engine === "kopia" ? "accent" : "neutral"}
      icon={engine === "kopia" ? "backups" : "backup-sets"}
    >
      {copy.name}
    </StatusBadge>
  );
}

const CHECK_TONE: Record<"ok" | "warn" | "danger", { tone: StatusTone; icon: IconName; word: string }> = {
  ok: { tone: "ok", icon: "success", word: "Pass" },
  warn: { tone: "warn", icon: "warning", word: "Attention" },
  danger: { tone: "danger", icon: "failure", word: "Fail" }
};

/** A list of checks with a verdict and a sentence each: the connection
 *  test, and the repository health panel, which are the same shape. */
export function CheckList({
  checks
}: {
  checks: { label: string; state: "ok" | "warn" | "danger"; detail: string }[];
}) {
  return (
    <ul style={{ margin: 0, padding: 0, listStyle: "none", display: "flex", flexDirection: "column", gap: 2 }}>
      {checks.map((check) => {
        const presentation = CHECK_TONE[check.state];
        return (
          <li
            key={check.label}
            style={{
              display: "grid",
              gridTemplateColumns: "minmax(190px, max-content) 1fr",
              gap: "4px 16px",
              alignItems: "baseline",
              padding: "9px 2px",
              borderBottom: "1px solid var(--border)"
            }}
          >
            <span style={{ display: "inline-flex", alignItems: "center", gap: 8, fontSize: 13 }}>
              {/* The tone names a status token directly: "ok", "warn" and
                  "danger" ARE the token names (design-system/tokens.css). */}
              <span aria-hidden="true" style={{ color: "var(--" + check.state + ")", display: "inline-flex" }}>
                <Icon name={presentation.icon} />
              </span>
              {check.label}
              {/* Colour is never the message (§9): the verdict is a word
                  as well as a tint, and it is what a reader hears. */}
              <span className="visually-hidden">{presentation.word}</span>
            </span>
            <span style={{ fontSize: 13, color: "var(--text-2)" }}>{check.detail}</span>
          </li>
        );
      })}
    </ul>
  );
}

/** A radio card, the wizard's own `Choice`. */
export function Choice({
  name,
  title,
  detail,
  wire,
  checked,
  onChange,
  children
}: {
  name: string;
  title: string;
  detail: string;
  wire?: string;
  checked: boolean;
  onChange(): void;
  children?: ReactNode;
}) {
  return (
    <label
      style={{
        display: "flex",
        gap: 10,
        padding: "13px 14px",
        border: checked ? "1.5px solid var(--accent)" : "1px solid var(--border-strong)",
        borderRadius: "var(--radius-lg)",
        background: checked ? "var(--accent-quiet)" : "var(--surface-2)",
        cursor: "pointer"
      }}
    >
      <input
        type="radio"
        name={name}
        checked={checked}
        onChange={onChange}
        style={{ marginTop: 2, accentColor: "var(--accent)" }}
      />
      <span style={{ flex: 1 }}>
        <span style={{ display: "flex", alignItems: "baseline", gap: 8, flexWrap: "wrap" }}>
          <span style={{ fontSize: 13, fontWeight: 600 }}>{title}</span>
          {wire ? <WireName name={wire} /> : null}
        </span>
        <span style={{ display: "block", marginTop: 3, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
          {detail}
        </span>
        {children}
      </span>
    </label>
  );
}

/**
 * A checkbox row, the wizard's own `Toggle`, plus the state #852 added:
 * refused, with the reason attached as a registry tooltip rather than
 * left to the operator to work out.
 *
 * A disabled control with no explanation is the failure mode this shape
 * exists to avoid — the operator's next move is to go looking for the
 * setting somewhere else, or to conclude the product cannot do it at all.
 * The tooltip host wraps the whole row, so the pop-up is reachable by
 * hover, by focus and by the "i" trigger even though the input inside it
 * takes no pointer events.
 */
export function Toggle({
  label,
  note,
  checked,
  onChange,
  disabled,
  tip
}: {
  label: string;
  note: string;
  checked: boolean;
  onChange?(): void;
  disabled?: boolean;
  tip?: TooltipId;
}) {
  const row = (
    <label
      style={{
        display: "flex",
        alignItems: "center",
        gap: 10,
        padding: "11px 13px",
        border: "1px solid " + (disabled ? "var(--border)" : "var(--border-strong)"),
        borderRadius: 7,
        background: disabled ? "var(--surface-3)" : "var(--surface-2)",
        fontSize: 13,
        color: disabled ? "var(--text-3)" : "var(--text)",
        cursor: disabled ? "not-allowed" : "pointer"
      }}
    >
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        onChange={() => onChange?.()}
        style={{ accentColor: "var(--accent)" }}
      />
      <span style={{ flex: 1 }}>{label}</span>
      <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)", textAlign: "right" }}>{note}</span>
    </label>
  );
  return tip ? (
    <InfoTooltip id={tip} block>
      {row}
    </InfoTooltip>
  ) : (
    row
  );
}

/** A labelled input, inert. The mock-up shows what a field looks like and
 *  what it would hold; it is not collecting anything. */
export function Field({
  label,
  value,
  onChange,
  wire,
  mono,
  style
}: {
  label: string;
  value: string;
  onChange?(next: string): void;
  wire?: string;
  mono?: boolean;
  style?: CSSProperties;
}) {
  return (
    <label className="field" style={style}>
      <span className="field__label" style={{ display: "inline-flex", alignItems: "baseline", gap: 8 }}>
        {label}
        {wire ? <WireName name={wire} /> : null}
      </span>
      <input
        className={"input" + (mono ? " input--mono" : "")}
        value={value}
        onChange={(e) => onChange?.(e.target.value)}
        readOnly={!onChange}
      />
    </label>
  );
}

/** A labelled select. */
export function Select({
  label,
  value,
  options,
  onChange,
  wire,
  tip
}: {
  label: string;
  value: string;
  options: { value: string; label: string }[];
  onChange(next: string): void;
  wire?: string;
  tip?: TooltipId;
}) {
  const field = (
    <label className="field">
      <span className="field__label" style={{ display: "inline-flex", alignItems: "baseline", gap: 8 }}>
        {label}
        {wire ? <WireName name={wire} /> : null}
      </span>
      <select className="select" value={value} onChange={(e) => onChange(e.target.value)}>
        {options.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </select>
    </label>
  );
  return tip ? (
    <InfoTooltip id={tip} block>
      {field}
    </InfoTooltip>
  ) : (
    field
  );
}

/** A responsive form grid, the spacing the wizard's own steps use. */
export function FormGrid({ children, min = 228 }: { children: ReactNode; min?: number }) {
  return (
    <div
      style={{
        display: "grid",
        gridTemplateColumns: "repeat(auto-fit, minmax(" + min + "px, 1fr))",
        gap: "15px 18px"
      }}
    >
      {children}
    </div>
  );
}

/** The heading-and-lede a wizard step opens with. */
export function StepBody({ title, lede, children }: { title: string; lede: string; children: ReactNode }) {
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

/** The numbered step rail, the wizard's own, widened to take a step count
 *  the caller chooses rather than the six it hardcodes. */
export function StepRail({
  steps,
  step,
  onSelect
}: {
  steps: readonly string[];
  step: number;
  onSelect(n: number): void;
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
            <InfoTooltip id="wizard.incremental.step" block>
              <button
                onClick={() => onSelect(n)}
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
                  {"0" + n}
                  <span aria-hidden="true" style={{ color: "var(--ok)", opacity: done ? 1 : 0, display: "inline-flex" }}>
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

/** The Back / Next pair a wizard step ends with. */
export function StepControls({
  step,
  total,
  onBack,
  onNext,
  finishLabel
}: {
  step: number;
  total: number;
  onBack(): void;
  onNext(): void;
  finishLabel: string;
}) {
  return (
    <div
      style={{
        display: "flex",
        alignItems: "center",
        gap: 10,
        padding: "14px 22px",
        borderTop: "1px solid var(--border)",
        background: "var(--surface-2)"
      }}
    >
      <button className="btn" disabled={step === 1} onClick={onBack}>
        Back
      </button>
      <div style={{ flex: 1, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
        {"Step " + step + " of " + total}
      </div>
      <button className="btn btn--primary" onClick={onNext}>
        {step === total ? finishLabel : "Continue"}
      </button>
    </div>
  );
}

/** A quiet explanatory line under a control or a table. */
export function Note({ children }: { children: ReactNode }) {
  return <p style={{ margin: "10px 0 0", fontSize: "var(--text-sm)", color: "var(--text-2)" }}>{children}</p>;
}

/** The tone a verification verdict is drawn in, in one place so the list,
 *  the inspect panel and the retention view cannot disagree. */
export function VerificationBadge({
  status,
  achieved
}: {
  status: "passed" | "failed" | "pending";
  achieved: string | null;
}) {
  if (status === "failed") {
    return (
      <StatusBadge tone="danger" icon="failure">
        Failed
      </StatusBadge>
    );
  }
  if (status === "pending") {
    return (
      <StatusBadge tone="neutral" icon="status-idle">
        Not checked yet
      </StatusBadge>
    );
  }
  return (
    <StatusBadge tone="ok" icon="success">
      {achieved ?? "Passed"}
    </StatusBadge>
  );
}

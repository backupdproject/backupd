/**
 * A radio card and a checkbox row: the two shapes this product asks a
 * closed question with, promoted out of `BackupSetWizardPage` for EPIC K
 * (issue #788).
 *
 * `Choice` was already page-private and copied into #788's mock-up;
 * `Toggle` was too, and it comes out with one addition the product
 * genuinely needed: a REFUSED state that carries its reason.
 */
import type { ReactNode } from "react";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import type { TooltipId } from "@shared/tooltips/tooltips";
import { WireField } from "@shared/components/Definitions";

/**
 * One option among several, drawn as a card so the explanation fits.
 *
 * `detail` is required, not optional, and that is the component making an
 * argument: every choice this product offers an operator is between
 * things with consequences, and a radio with a bare label is one they
 * will pick by guessing.
 */
export function Choice({
  name,
  title,
  detail,
  wire,
  checked,
  defaultChecked,
  onChange,
  disabled,
  children
}: {
  name: string;
  title: string;
  detail: string;
  /** The api/v1 field and value this option sends, for the screens an
   *  operator reads beside a contract or a CLI transcript. */
  wire?: string;
  checked?: boolean;
  defaultChecked?: boolean;
  onChange?(): void;
  disabled?: boolean;
  children?: ReactNode;
}) {
  const selected = checked ?? defaultChecked ?? false;
  return (
    <label
      style={{
        display: "flex",
        gap: 10,
        padding: "13px 14px",
        border: selected ? "1.5px solid var(--accent)" : "1px solid var(--border-strong)",
        borderRadius: "var(--radius-lg)",
        background: selected ? "var(--accent-quiet)" : "var(--surface-2)",
        opacity: disabled ? 0.6 : 1,
        cursor: disabled ? "not-allowed" : "pointer"
      }}
    >
      <input
        type="radio"
        name={name}
        checked={checked}
        defaultChecked={checked === undefined ? defaultChecked : undefined}
        disabled={disabled}
        onChange={onChange}
        style={{ marginTop: 2, accentColor: "var(--accent)" }}
      />
      <span style={{ flex: 1 }}>
        <span style={{ display: "flex", alignItems: "baseline", gap: 8, flexWrap: "wrap" }}>
          <span style={{ fontSize: 13, fontWeight: 600 }}>{title}</span>
          {wire ? <WireField name={wire} /> : null}
        </span>
        <span
          style={{ display: "block", marginTop: 3, fontSize: "var(--text-sm)", color: "var(--text-2)" }}
        >
          {detail}
        </span>
        {children}
      </span>
    </label>
  );
}

/**
 * A checkbox row with its current value stated on the right, plus the
 * state issue #852 made necessary: refused, with the reason attached.
 *
 * A disabled control with no explanation is the failure this shape exists
 * to avoid — the operator's next move is to go looking for the setting
 * somewhere else, or to conclude the product cannot do it at all. So
 * `disabled` comes with `tip`, the registry id of the sentence that says
 * why and what to do about it, and the tooltip host wraps the whole row
 * so the pop-up is reachable by hover, by focus and by the "i" trigger
 * even though the input inside it takes no pointer events.
 *
 * `describedBy` is the other half of the same problem: a disabled control
 * announces nothing about why it is disabled, so a caller that has the
 * reason on screen as text points at it here.
 */
export function Toggle({
  label,
  note,
  checked,
  onChange,
  disabled,
  tip,
  describedBy
}: {
  label: string;
  note: string;
  checked: boolean;
  onChange?(): void;
  disabled?: boolean;
  tip?: TooltipId;
  describedBy?: string;
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
        aria-describedby={describedBy}
        onChange={() => onChange?.()}
        style={{ accentColor: "var(--accent)" }}
      />
      <span style={{ flex: 1 }}>{label}</span>
      <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)", textAlign: "right" }}>
        {note}
      </span>
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

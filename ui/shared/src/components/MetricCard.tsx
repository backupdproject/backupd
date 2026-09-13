/**
 * One number with its label, sized to be read across a room.
 *
 * A right border rather than a gap between cells is what makes a row of
 * these read as one instrument panel: the enclosing `.card` hides its
 * overflow, so the last divider lands on the card's edge and vanishes,
 * and adding or removing a metric needs no change here. `children` is for
 * the metrics that carry a gauge or a badge under the figure, which is
 * why this is a component rather than a helper returning a string.
 *
 * `tip` is named by the CALLER rather than fixed here (issue #834): what
 * a figure means is a property of the metric the page chose to draw, not
 * of the box it is drawn in, and one hardcoded id would explain every
 * metric in the product identically. It covers the label, the figure and
 * the detail line, and deliberately NOT `children` — a gauge given to
 * this card explains itself, and a host nested inside this one would
 * open two overlapping pop-ups from a single hover.
 */
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import type { TooltipId } from "@shared/tooltips/tooltips";

export function MetricCard({
  label,
  value,
  detail,
  children,
  tip
}: {
  label: string;
  value: string;
  detail?: string;
  children?: React.ReactNode;
  tip?: TooltipId;
}) {
  const figure = (
    <div>
      <div className="eyebrow" style={{ fontSize: "var(--text-xs)" }}>{label}</div>
      <div style={{ marginTop: 6, fontSize: 24, fontWeight: 600, letterSpacing: "-0.02em" }}>
        {value}
      </div>
      {detail ? (
        <div style={{ marginTop: 4, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
          {detail}
        </div>
      ) : null}
    </div>
  );

  return (
    <div style={{ padding: "15px 20px", borderRight: "1px solid var(--border)" }}>
      {tip ? <InfoTooltip id={tip} block>{figure}</InfoTooltip> : figure}
      {children}
    </div>
  );
}

/**
 * The two label-and-value shapes this product states a fact in, promoted
 * out of `BackupSetDetailPage` so that the screens EPIC K adds (issue
 * #788) reuse them instead of copying them a third time.
 *
 * `Cell` is a boxed summary tile and `Row` is a definition pair in a
 * two-column grid; both live inside a `<dl>`, which is what
 * `CellGrid`/`Rows` are. That is a semantic decision rather than a
 * styling one: a page full of labelled values IS a description list, and
 * a `<div>` grid of `<span>`s reads to anything navigating by structure
 * as an undifferentiated wall of text.
 *
 * # Why they were private, and why that stopped working
 *
 * They began as functions inside the detail page, which was right while
 * exactly one page drew them. #788's mock-up then needed the same shapes
 * on eight more screens and copied them into `src/mockup/parts.tsx`; the
 * design doc (docs/design/788-incremental-ui-mockup.md) records the
 * intended fix as promoting them here and deleting the copy, which is
 * what this file is.
 *
 * # The accessible-name gap, carried over deliberately
 *
 * Neither gives its `dt` or its `dd` an accessible name. `<dt>` is role
 * `term` and `<dd>` is role `definition`, and both take their name from
 * the author rather than from their content, so a row here is not
 * reachable by anything navigating by role. The fix is one line in each
 * (an id on the `dt`, an `aria-labelledby` on the `dd`) and it was
 * written, measured and taken back out on the detail page: four of that
 * page's read-only labels are also the labels of its inline EDIT fields,
 * so naming the values puts two elements with the same accessible name on
 * screen whenever edit mode is open. Promoting the shape does not change
 * that argument, and quietly fixing it here would reintroduce the
 * ambiguity on the page the decision was made for.
 */
import type { ReactNode } from "react";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import type { TooltipId } from "@shared/tooltips/tooltips";

/**
 * A boxed summary tile: a small eyebrow label over one value.
 *
 * `value` is a `ReactNode` rather than a string because half the tiles
 * EPIC K adds hold a badge, and the alternative — a second component for
 * the badge case — is how two tiles that should look identical stop
 * looking identical.
 *
 * `wire` is the api/v1 field the value came from, in monospace under it.
 * It is off by default and the screens that pass it are the ones an
 * operator reads next to a contract: the repository health panel, the
 * snapshot inspector and the wizard's review step, where "which field is
 * this" is a question somebody genuinely asks while comparing a screen
 * with `backupd`'s own output.
 */
export function Cell({
  label,
  value,
  wire,
  mono,
  boxed,
  tip
}: {
  label: string;
  value: ReactNode;
  wire?: string;
  mono?: boolean;
  /** Draws the tile with a border and a quiet background. The detail
   *  page's own cells sit in a bordered card already, so they stay
   *  unboxed; the new screens put cells directly on the page. */
  boxed?: boolean;
  tip?: TooltipId;
}) {
  return (
    <div
      style={
        boxed
          ? {
              padding: "12px 14px",
              border: "1px solid var(--border)",
              borderRadius: "var(--radius-lg)",
              background: "var(--surface-2)"
            }
          : undefined
      }
    >
      <dt className="eyebrow" style={{ fontSize: 10.5, letterSpacing: "0.06em" }}>
        {label}
      </dt>
      {/* The host goes round the VALUE, inside the <dd>. A <dl> pairs its
          terms with its definitions through its own children, so a
          wrapper between them would be one an assistive technology has to
          walk past to find the row. */}
      <dd style={{ margin: "4px 0 0", fontFamily: mono ? "var(--font-mono)" : undefined, wordBreak: mono ? "break-all" : undefined }}>
        {tip ? <InfoTooltip id={tip}><span>{value}</span></InfoTooltip> : value}
      </dd>
      {wire ? (
        <div style={{ marginTop: 4 }}>
          <WireField name={wire} />
        </div>
      ) : null}
    </div>
  );
}

/** A responsive grid of `Cell`s, and the `<dl>` they need to be inside. */
export function CellGrid({ children, min = 200 }: { children: ReactNode; min?: number }) {
  return (
    <dl
      style={{
        margin: 0,
        display: "grid",
        gridTemplateColumns: "repeat(auto-fit, minmax(" + min + "px, 1fr))",
        gap: 10
      }}
    >
      {children}
    </dl>
  );
}

/**
 * A definition pair: label on the left, value on the right.
 *
 * Returns a fragment, so its `dt` and `dd` are direct children of the
 * `<dl>` `Rows` draws and the grid can lay them out as two columns. It
 * must therefore never be wrapped in anything.
 */
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
  const term = wire ? (
    <span style={{ display: "inline-flex", alignItems: "baseline", gap: 8, flexWrap: "wrap" }}>
      {label}
      <WireField name={wire} />
    </span>
  ) : (
    label
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

/** The `<dl>` `Row` lives in. One column collapses the pair onto two
 *  lines, which is what a narrow card wants. */
export function Rows({ children, columns = 2 }: { children: ReactNode; columns?: 1 | 2 }) {
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

/**
 * The api/v1 field a value came from, quiet enough to read past.
 *
 * It came out of the mock-up, where it existed for reviewers checking the
 * copy and the contract in one pass, and it earns a place in the product
 * for a different reason: EPIC K's screens are the ones an operator
 * compares with `backupd snapshot list` output and with a support
 * engineer's questions, and "source_bytes_read" is the word both of those
 * conversations use.
 *
 * That it SHIPS, rather than going with the mock-up, is a decision taken
 * on #788's review and recorded in the design doc's Vocabulary section
 * (docs/design/788-incremental-ui-mockup.md), which used to say the
 * opposite. The rule that comes with it: an annotation carries the
 * contract's spelling and never a label. `kopia` may appear inside one
 * of these; no operator is ever asked to read it as the name of an
 * engine, which is "Incremental".
 */
export function WireField({ name }: { name: string }) {
  return (
    <span
      className="mono"
      style={{ fontSize: "var(--text-xs)", color: "var(--text-3)", whiteSpace: "nowrap" }}
    >
      {name}
    </span>
  );
}

/** A quiet explanatory line under a control, a table or a card. The
 *  product had this shape inline in a dozen places; EPIC K's screens lean
 *  on it hard enough (every table that reports a nullable counter owes a
 *  sentence saying why) that it is worth one name. */
export function Note({ children }: { children: ReactNode }) {
  return (
    <p style={{ margin: "10px 0 0", fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
      {children}
    </p>
  );
}

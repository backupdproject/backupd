/**
 * The title block every page opens with, including its way back and its
 * page-level actions.
 *
 * Actions belong up here rather than beside the thing they act on
 * whenever their scope is the whole page, which is a distinction this
 * product has already got wrong once: a control's placement says what it
 * acts on, louder than its label does, so a deployment-wide pass drawn
 * inside a per-set card reads as a per-set run (see BackupSetCard's own
 * note). Anything handed to `actions` here is claiming page scope.
 *
 * `title` and `subtitle` take nodes rather than strings so a page can put
 * an identity in mono or a badge beside the name without this component
 * growing a prop per case.
 */
import type { ReactNode } from "react";
import { Icon } from "@shared/design-system/icons";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import type { TooltipId } from "@shared/tooltips/tooltips";

export function PageHeader({
  title,
  subtitle,
  back,
  actions,
  tip
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  back?: { label: string; onClick(): void };
  actions?: ReactNode;
  /** What this page is, in the tooltip registry (issue #834). Drawn as
   *  the "i" beside the title, because a page heading has no control of
   *  its own to hang an explanation off. */
  tip?: TooltipId;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
      {back ? (
        <InfoTooltip id="page.back" style={{ alignSelf: "flex-start" }}>
          <button
            className="btn btn--quiet"
            onClick={back.onClick}
            style={{
              alignSelf: "flex-start", height: "auto", padding: 0, border: "none",
              background: "none", color: "var(--accent)", fontSize: "var(--text-sm)",
              display: "inline-flex", alignItems: "center", gap: 7
            }}
          >
            {/* The arrow is a picture now rather than a character in the
                label (#621), which also fixes something that was wrong
                before it: a button's accessible name is its text, so this
                control used to be announced as "left arrow Backups". It is
                named by its words alone now. */}
            <Icon name="arrow-left" />
            {back.label}
          </button>
        </InfoTooltip>
      ) : null}
      <div
        style={{
          display: "flex", alignItems: "flex-end", justifyContent: "space-between",
          gap: 16, flexWrap: "wrap"
        }}
      >
        <div>
          {/* The "i" is a SIBLING of the heading, never a child of it: a
              control inside a heading contributes its own name to the
              heading's, and every page in this app is found by that name
              (issue #834, and see InfoTooltip's module doc). */}
          <div style={{ display: "flex", alignItems: "center" }}>
            <h1>{title}</h1>
            {tip ? <InfoTooltip id={tip} /> : null}
          </div>
          {subtitle ? (
            <p style={{ margin: "4px 0 0", color: "var(--text-2)", fontSize: 13 }}>{subtitle}</p>
          ) : null}
        </div>
        {actions ? (
          <div style={{ display: "flex", gap: 9, flexWrap: "wrap" }}>{actions}</div>
        ) : null}
      </div>
    </div>
  );
}

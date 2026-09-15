/**
 * A list of checks with a verdict and a sentence each (EPIC K, issue
 * #788).
 *
 * Two surfaces are the same shape and this is it: the connection test's
 * per-step report, and the repository health panel's probes. Both answer
 * "here is a thing retnd TRIED, and here is what happened", which is
 * the distinction the whole panel exists for — a repository handle
 * survives a network partition and every call on it would fail, so
 * reachability has to be proved by a read and a write rather than
 * inferred from an open connection.
 *
 * Colour is never the message (§9): every row carries an icon, a word for
 * assistive technology, and the sentence. A verdict that can only be told
 * apart by hue is a verdict that is not being communicated.
 */
import { Icon } from "@shared/design-system/icons";
import type { IconName } from "@shared/design-system/icons";
import type { StatusTone } from "@shared/components/StatusBadge";

/** Three outcomes and not two. "Attention" is the one that matters: a
 *  repository that is readable but not writable, or a maintenance window
 *  that has been missed, is neither a pass nor a failure, and folding it
 *  either way loses the only rows an operator has to act on. */
export type CheckState = "ok" | "warn" | "danger";

export interface Check {
  label: string;
  state: CheckState;
  detail: string;
  /** The api/v1 field this row reports, shown in monospace beside the
   *  label on the screens an operator reads next to a contract. */
  wire?: string;
}

const PRESENTATION: Record<CheckState, { tone: StatusTone; icon: IconName; word: string }> = {
  ok: { tone: "ok", icon: "success", word: "Pass" },
  warn: { tone: "warn", icon: "warning", word: "Attention" },
  danger: { tone: "danger", icon: "failure", word: "Fail" }
};

export function CheckList({ checks }: { checks: Check[] }) {
  return (
    <ul
      style={{
        margin: 0,
        padding: 0,
        listStyle: "none",
        display: "flex",
        flexDirection: "column",
        gap: 2
      }}
    >
      {checks.map((check) => {
        const presentation = PRESENTATION[check.state];
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
              {/* The state names a status token directly: "ok", "warn"
                  and "danger" ARE the token names
                  (design-system/tokens.css). */}
              <span
                aria-hidden="true"
                style={{ color: "var(--" + check.state + ")", display: "inline-flex" }}
              >
                <Icon name={presentation.icon} />
              </span>
              {check.label}
              {/* The verdict as a word as well as a tint, and it is what a
                  reader hears. */}
              <span className="visually-hidden">{presentation.word}</span>
            </span>
            <span style={{ fontSize: 13, color: "var(--text-2)" }}>
              {check.detail}
              {check.wire ? (
                <span
                  className="mono"
                  style={{ marginLeft: 8, fontSize: "var(--text-xs)", color: "var(--text-3)" }}
                >
                  {check.wire}
                </span>
              ) : null}
            </span>
          </li>
        );
      })}
    </ul>
  );
}

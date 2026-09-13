/**
 * The one rule this mock-up applies to a number before printing it
 * (issue #788).
 *
 * It is a module of its own rather than a helper inside parts.tsx because
 * that file exports components and this is not one — the workspace's Fast
 * Refresh rule is the immediate reason, and the better one is that the
 * decision here is about the CONTRACT rather than about any screen.
 */

/**
 * A counter the wire may not carry, rendered.
 *
 * Every snapshot figure in #788's contract is nullable, and absent means
 * NOBODY MEASURED IT. This is the single place that turns that into
 * words, so no screen can decide on its own that an unmeasured number is
 * a zero — which is the specific misreport the four byte counts exist to
 * prevent: a zero is a measurement, and "reused 0 bytes" sends an
 * operator hunting a fault in a backup that is working perfectly well.
 */
export function measured(value: number | null, render: (n: number) => string): string {
  return value === null ? "not measured" : render(value);
}

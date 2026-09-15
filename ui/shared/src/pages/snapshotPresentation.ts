/**
 * The words EPIC K's operational screens report a snapshot, a repository
 * and a retention verdict in (issue #788).
 *
 * It is a module rather than five private helpers because the same facts
 * are stated on more than one screen and the product's rule is that they
 * are stated the same way. The snapshot list, the inspector and the
 * retention view all badge a phase; the health page and the maintenance
 * page both report a clock; and the two of those are exactly the places an
 * operator compares two screens against each other.
 *
 * Two rules are enforced here rather than left to each call site:
 *
 *   - **Absent is never zero.** `utilities/format.measured` does the
 *     rendering; what this file adds is the SIGNED, nullable clock skew,
 *     where a null drawn as "0 s" would read as a perfectly synchronised
 *     repository and a magnitude drawn without its sign would hide the one
 *     direction that matters (a clock behind the repository dates a new
 *     snapshot before one already stored).
 *   - **A phase is never shown raw.** The state machine's own words
 *     (core/internal/state/snapshots.go) are SNAPSHOT_WRITE and
 *     CATALOG_COMMIT; an operator reads "Writing to the repository" and
 *     "Recording in the catalog". An unknown phase still renders — a
 *     newer service is allowed to have a state this build has not heard
 *     of, and dropping it would leave a row with no state at all.
 */
import type { Check } from "@shared/components/CheckList";
import type { StatusTone } from "@shared/components/StatusBadge";
import type { IconName } from "@shared/design-system/icons";
import type { RetentionTierPlacement, RetentionTierSelection } from "@shared/types/backup";
import type {
  RepositoryHealth,
  RepositoryState,
  SnapshotRetentionTier
} from "@shared/types/snapshot";

/** How one phase of the snapshot state machine reads, and how it is
 *  badged. Every terminal state that is not SUCCESS is a state somebody
 *  has to act on, so none of them are drawn quietly. */
export interface PhasePresentation {
  word: string;
  tone: StatusTone;
  icon: IconName;
}

/**
 * The snapshot state machine's states, in the operator's words.
 *
 * DELETED and LOST are not the same event and are not badged the same
 * way: a deleted snapshot is retention doing its job, and a lost one is a
 * manifest the repository no longer has, which is a restore point that
 * evaporated.
 */
export const SNAPSHOT_PHASE_COPY: Record<string, PhasePresentation> = {
  PENDING: { word: "Waiting to start", tone: "neutral", icon: "status-idle" },
  SOURCE_SCAN: { word: "Scanning the source", tone: "accent", icon: "status-active" },
  SNAPSHOT_WRITE: { word: "Writing to the repository", tone: "accent", icon: "status-active" },
  MANIFEST_COMMITTED: { word: "Stored, not yet verified", tone: "warn", icon: "warning" },
  VERIFICATION: { word: "Verifying", tone: "accent", icon: "status-active" },
  CATALOG_COMMIT: { word: "Recording in the catalog", tone: "accent", icon: "status-active" },
  // NOT "stored and verified". SUCCESS is the state machine reaching its
  // end, and a run can reach it with a verification that FAILED (the
  // check ran, proved nothing, and the run still finished). A phase word
  // that claimed verification would contradict the verification badge on
  // the same row, and the badge is the one that is about verification.
  SUCCESS: { word: "Run complete", tone: "ok", icon: "success" },
  FAILED: { word: "Failed", tone: "danger", icon: "failure" },
  LOST: { word: "Lost from the repository", tone: "danger", icon: "failure" },
  DELETED: { word: "Deleted by retention", tone: "neutral", icon: "status-idle" },
  QUARANTINED: { word: "Quarantined", tone: "danger", icon: "quarantine" }
};

/**
 * A phase, presentable.
 *
 * An unrecognised phase keeps its own word rather than becoming
 * "Unknown": a service newer than this build may have a state this table
 * has never heard of, and the raw token is at least the thing the CLI and
 * the logs print. It is badged neutral, because this build cannot claim
 * to know whether it is good news.
 */
export function phasePresentation(phase: string): PhasePresentation {
  return SNAPSHOT_PHASE_COPY[phase] ?? { word: phase, tone: "neutral", icon: "status-idle" };
}

/**
 * A repository's clock, in words, with the sign preserved.
 *
 * Null is "not measured" and never a zero: a zero is a measurement, and
 * an unmeasured clock drawn as a perfect one is the single most
 * reassuring thing this page could say falsely. Negative is the dangerous
 * direction and says so in words rather than with a minus sign an eye
 * skips over.
 */
export function clockSkewSentence(seconds: number | null): string {
  if (seconds === null) return "not measured";
  if (seconds < 0) return Math.abs(seconds) + " s behind the repository";
  if (seconds === 0) return "in step with the repository";
  return "within " + seconds + " s";
}

/** What a run reported about its own walk. Three answers, not two:
 *  `null` is "this run did not report", which is a different claim from
 *  `false` and must not be collapsed into it. */
export function sourceCompleteSentence(complete: boolean | null): string {
  if (complete === null)
    return "not measured \u2014 this run did not report whether the walk captured everything";
  return complete
    ? "Yes \u2014 every entry the walk found was captured"
    : "No \u2014 the walk did not capture every entry it found";
}

/** FR-18's placements, which are the only values `RetentionBadge` draws
 *  in brackets. A snapshot verdict's `selected_by` is an open string, so
 *  anything else it carries is named in words beside the badge instead of
 *  being forced into this vocabulary. */
const PLACEMENTS: Record<string, RetentionTierPlacement> = {
  DISCOVERY: "DISCOVERY",
  PRODUCER: "PRODUCER",
  BOTH: "BOTH",
  PROTECTION: "PROTECTION"
};

/**
 * A snapshot verdict's tiers, as the product's own `RetentionTierBadges`
 * takes them.
 *
 * Two conversions happen here and both are load-bearing:
 *
 *   - the tier name is upper-cased and hyphens become underscores, so the
 *     wire's `last-known-good` reaches `RetentionBadge`'s LAST_KNOWN_GOOD
 *     and is drawn as **Protected** rather than as a tier this build has
 *     never heard of;
 *   - a null `selectedBy` becomes PROTECTION, which is the one placement
 *     that draws NO bracket. Last-known-good protection has no placement
 *     to name, and a parenthesised word after "Protected" would read as
 *     one.
 */
export function snapshotTierSelections(tiers: SnapshotRetentionTier[]): RetentionTierSelection[] {
  return tiers.map((tier) => ({
    tier: tier.tier.toUpperCase().replace(/-/g, "_"),
    selectedBy:
      tier.selectedBy === null ? "PROTECTION" : PLACEMENTS[tier.selectedBy.toUpperCase()] ?? "PROTECTION"
  }));
}

/**
 * The `selected_by` values a badge cannot carry, in the order they
 * arrived, so a screen can state them beside the badges.
 *
 * Snapshot retention's `selected_by` is not FR-18's closed placement
 * vocabulary — it is "what about the snapshot the tier selected it for",
 * which a service may answer with a timestamp or a rule name. Those are
 * real information and are shown rather than discarded, but they are not
 * squeezed into a bracket that means something else.
 */
export function unplacedSelections(tiers: SnapshotRetentionTier[]): string[] {
  return tiers
    .filter((tier) => tier.selectedBy !== null && PLACEMENTS[tier.selectedBy.toUpperCase()] === undefined)
    .map((tier) => tier.tier + ": " + (tier.selectedBy ?? ""));
}

/** How a domain's own verdict is badged. The same three words, the same
 *  severity scale and the same colours a backup set's health uses, which
 *  is what stops one dashboard carrying two vocabularies. */
export const REPOSITORY_STATE_PRESENTATION: Record<
  RepositoryState,
  { word: string; tone: StatusTone; icon: IconName }
> = {
  HEALTHY: { word: "Healthy", tone: "ok", icon: "status-active" },
  DEGRADED: { word: "Degraded", tone: "warn", icon: "warning" },
  FAILING: { word: "Failing", tone: "danger", icon: "failure" }
};

/**
 * One repository domain's probes, as check rows.
 *
 * Each probe is carried separately rather than reduced to one boolean
 * because the REMEDIES differ, and the detail line on each row is what
 * says which remedy this is: unreachable is a mount, unwritable is a
 * permission, invalid credentials is a passphrase, and overdue
 * maintenance is a schedule.
 *
 * Readable-but-not-writable is `warn` rather than `danger` on purpose:
 * every snapshot already in that store can still be restored, and the
 * thing that has stopped is new writes. Calling that a failure would
 * send an operator to a restore they do not need.
 */
export function repositoryChecks(health: RepositoryHealth): Check[] {
  return [
    {
      label: "Reachable",
      wire: "reachable",
      state: health.reachable ? "ok" : "danger",
      detail: health.reachable
        ? "The store answered."
        : "Nothing answered at this domain's location. Check the mount or the network path before anything else here."
    },
    {
      label: "Readable",
      wire: "readable",
      state: health.readable ? "ok" : "danger",
      detail: health.readable
        ? "A read probe returned what it wrote earlier, so restores from this store can be served."
        : "A read probe failed, so nothing stored here can be restored right now."
    },
    {
      label: "Writable",
      wire: "writable",
      state: health.writable ? "ok" : "warn",
      detail: health.writable
        ? "A write probe created an object and removed it again."
        : "The store refused a write probe. Snapshots already here can still be restored; no new one can be written until the permission is granted."
    },
    {
      label: "Credentials",
      wire: "credentials_valid",
      state: health.credentialsValid ? "ok" : "danger",
      detail: health.credentialsValid
        ? "The configured credentials opened the store."
        : "The configured credentials were rejected. This is a passphrase or a key, not a network fault."
    },
    {
      label: "Clock",
      wire: "clock_skew_seconds",
      state: health.clockSane ? "ok" : "warn",
      // Three sentences, not two. A sane clock whose skew was never
      // measured must not claim manifests order correctly BECAUSE of a
      // figure nobody took: what is known is that the service did not
      // report a drift, and the page says exactly that much.
      detail:
        clockSkewSentence(health.clockSkewSeconds) +
        (!health.clockSane
          ? ". A clock behind the repository dates a new snapshot before one already stored, which mis-orders manifests."
          : health.clockSkewSeconds === null
            ? ". retnd reports no drift against this store, but the skew itself was never measured."
            : ". Manifests written here order correctly against the ones already stored.")
    },
    {
      label: "Maintenance",
      wire: "maintenance_overdue",
      state: health.maintenanceOverdue ? "warn" : "ok",
      detail: health.maintenanceOverdue
        ? "Full maintenance has not run inside its window. Snapshots are still written and still restorable; what is not happening is reclamation, so the store keeps paying for content nothing references."
        : "Maintenance has run inside its window."
    }
  ];
}

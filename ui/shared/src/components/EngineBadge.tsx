/**
 * How this interface tells an Artifact set from an Incremental one, and
 * how it reports what a verification actually proved (EPIC K, issue
 * #788).
 *
 * # Why a badge and not a different layout
 *
 * Same page, same cards, one accented badge and a different set of
 * metrics. An operator who runs both engines should not have to learn two
 * interfaces, and an operator who runs one should never wonder which they
 * are looking at. That is the decision #788's mock-up asked for a verdict
 * on and got; this component is where it lives, so no screen can make it
 * again differently.
 *
 * Incremental is the accented tone and Artifact is neutral, because the
 * accent is what an eye finds in a list where most rows are the older
 * kind.
 *
 * # No vendor words
 *
 * The configured value is `kopia` and the word an operator reads is
 * "Incremental". The contract's spelling appears only where a screen
 * shows a wire field on purpose (`WireField`), never as a label.
 */
import { StatusBadge } from "@shared/components/StatusBadge";
import type { BackupEngine, SnapshotVerificationStatus, VerificationLevel } from "@shared/types/snapshot";

/** The operator-facing name of each engine, and the sentence that says
 *  what choosing it means. Exported because the wizard, the per-set
 *  configuration page and the deployment overview all state it, and three
 *  copies of a product decision is three chances to describe it
 *  differently. */
export const ENGINE_COPY: Record<BackupEngine, { name: string; summary: string; wire: string }> = {
  artifact: {
    name: "Artifact",
    summary:
      "Pulls finished files a producer leaves for you and keeps each one whole, verified on arrival.",
    wire: "artifact"
  },
  kopia: {
    name: "Incremental",
    summary:
      "Snapshots a whole directory tree every run and stores only content the repository does not already hold.",
    wire: "kopia"
  }
};

/** What each verification level proves, what it costs, and the contract's
 *  word for it. The ladder is a floor rather than a target: a run that
 *  proves LESS than the level asked for fails. */
export const VERIFICATION_COPY: Record<
  VerificationLevel,
  { name: string; finds: string; cost: string }
> = {
  structural: {
    name: "Structure",
    finds: "Proves the snapshot's manifest and directory tree are complete and readable.",
    cost: "Seconds. Reads no file content."
  },
  content_sample: {
    name: "Sampled content",
    finds: "Reads and hash-checks a share of the files, chosen fresh each run.",
    cost: "Minutes, in proportion to the share."
  },
  content_full: {
    name: "Every byte",
    finds: "Reads and hash-checks every file in the snapshot.",
    cost: "As long as reading the whole tree takes."
  },
  restore_drill: {
    name: "Restore drill",
    finds: "Restores the snapshot to scratch space and compares what lands against the manifest.",
    cost: "The longest, and the only level that proves a restore works."
  }
};

/** What the operator arranged on the source, in their words rather than
 *  the contract's, plus the one claim each mode is allowed to make about
 *  being a point in time. */
export const CONSISTENCY_COPY: Record<
  "live_best_effort" | "externally_quiesced" | "external_snapshot",
  { name: string; summary: string; pointInTime: string }
> = {
  live_best_effort: {
    name: "Live",
    summary: "Nothing was arranged: the tree may change while a run walks it.",
    pointInTime: "no — files may be from different moments"
  },
  externally_quiesced: {
    name: "Quiesced",
    summary: "You stop or pause whatever writes to this source for the duration of a run.",
    pointInTime: "only as far as the pause held"
  },
  external_snapshot: {
    name: "Frozen image",
    summary: "The run reads a filesystem or volume snapshot that cannot change under it.",
    pointInTime: "yes — the whole tree is one moment"
  }
};

export function EngineBadge({ engine }: { engine: BackupEngine }) {
  return (
    <StatusBadge
      tone={engine === "kopia" ? "accent" : "neutral"}
      icon={engine === "kopia" ? "backups" : "backup-sets"}
    >
      {ENGINE_COPY[engine].name}
    </StatusBadge>
  );
}

/**
 * A verification verdict, in one component so the snapshot list, the
 * inspector and the retention view cannot disagree about it.
 *
 * The badge reports the ACHIEVED level, because that is the answer (ADR
 * 0014): a failed check proves nothing and carries no level at all, and a
 * run nothing has looked at yet is neither a pass nor a failure. The
 * level a run was asked for is a different claim and belongs beside this,
 * not inside it.
 */
export function VerificationBadge({
  status,
  achieved
}: {
  status: SnapshotVerificationStatus;
  achieved: VerificationLevel | null;
}) {
  if (status === "failed") {
    return (
      <StatusBadge tone="danger" icon="failure">
        Failed
      </StatusBadge>
    );
  }
  if (status === "unchecked") {
    return (
      <StatusBadge tone="neutral" icon="status-idle">
        Not checked yet
      </StatusBadge>
    );
  }
  return (
    <StatusBadge tone="ok" icon="success">
      {achieved ? VERIFICATION_COPY[achieved].name : "Passed"}
    </StatusBadge>
  );
}

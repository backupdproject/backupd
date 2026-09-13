/**
 * The incremental engine's operator vocabulary (EPIC K, issue #788).
 *
 * These are the shapes every incremental screen renders: a snapshot run,
 * a hold, what snapshot retention would decide, a repository domain's
 * health and its maintenance state. They are this UI's camelCase model of
 * api/v1's snake_case bodies, mapped once in api/client.ts, for the same
 * reason types/backup.ts exists: a page reads a domain object, never a
 * wire body.
 *
 * # Why every counter is `number | null`
 *
 * Because the wire's are, and the distinction is the whole point of EPIC
 * K's five figures. A run that died before its manifest was recorded, and
 * a snapshot adopted from the repository by crash reconciliation, have
 * counters nobody ever took; a run over an empty tree has counters that
 * are genuinely zero. Collapsing the two reports "not measured" as
 * "measured nothing" — and for `contentReusedBytes` that is an active
 * misreport: a zero says the repository deduplicated nothing, which sends
 * an operator hunting a fault in a backup that is working perfectly well.
 * `measured()` (utilities/format.ts) is the one place a null becomes
 * words, and every surface goes through it.
 *
 * # Why the two verification levels are separate fields
 *
 * ADR 0014: what a run was ASKED to prove and what it actually PROVED are
 * different claims, and a failed check proves nothing at all, so it
 * carries no achieved level. A single field would have to choose which of
 * the two to lose.
 */

/** Which engine a backup set runs. `kopia` is the contract's word for the
 *  incremental engine and appears nowhere an operator reads: the
 *  interface's nouns are "Artifact" and "Incremental". */
export type BackupEngine = "artifact" | "kopia";

/** What the operator arranged on the source for the duration of a run.
 *  Backupd records this rather than detecting it, and reports a run that
 *  contradicts it. */
export type SourceConsistency = "live_best_effort" | "externally_quiesced" | "external_snapshot";

/** How far a backup is checked before it counts as a restore point. A
 *  floor, not a target: a run that proves less than the configured level
 *  fails. */
export type VerificationLevel = "structural" | "content_sample" | "content_full" | "restore_drill";

/**
 * Where a verification got to.
 *
 * "unchecked" is the wire's empty string and its "pending": nothing has
 * looked at this snapshot yet, which is neither a pass nor a failure and
 * must never be drawn as one.
 */
export type SnapshotVerificationStatus = "passed" | "failed" | "unchecked";

/** What retention would do about one snapshot. REFUSE is not a third
 *  shade of DELETE: it means the snapshot was a delete candidate and
 *  something stopped it, which is the only one of the three that needs
 *  somebody to look at it. */
export type SnapshotRetentionAction = "KEEP" | "DELETE" | "REFUSE";

/** What happens to a file already sitting where a restore would write.
 *  "refuse" is the default everywhere, because it is the only one of the
 *  three that cannot lose data. */
export type RestoreConflictPolicy = "refuse" | "skip" | "overwrite";

/**
 * One durable statement that a snapshot must not be deleted, whoever's
 * retention policy says otherwise.
 *
 * It names who placed it and why, because a hold nobody can attribute is
 * one nobody dares release. Several may sit on one snapshot, and the
 * snapshot survives until the last of them is released.
 */
export interface SnapshotHold {
  holdId: string;
  runId: string;
  backupSetId: string;
  reason: string;
  placedAt: string;
  placedBy: string;
  /** Null while the hold is in force. */
  releasedAt: string | null;
  releasedBy: string | null;
  active: boolean;
}

/**
 * One pass of the incremental engine over one backup set's source.
 *
 * `runId` exists from the moment the pass starts; `snapshotId` is the
 * engine's manifest id and exists only once a manifest was committed, so
 * a failed run has the first and not the second. Nothing in this product
 * addresses a snapshot by manifest id for that reason: the run is the
 * stable name, and it is what the API's own `.../snapshots/{run}` route
 * takes.
 */
export interface Snapshot {
  runId: string;
  backupSetId: string;
  snapshotId: string | null;
  operationId: string | null;
  engine: string;
  /** The snapshot state machine's own word: PENDING, SOURCE_SCAN,
   *  SNAPSHOT_WRITE, MANIFEST_COMMITTED, VERIFICATION, CATALOG_COMMIT,
   *  SUCCESS, FAILED, LOST, DELETED or QUARANTINED. Rendered through
   *  SNAPSHOT_PHASE_COPY rather than shown raw. */
  phase: string;
  repositoryDomain: string | null;
  consistencyMode: SourceConsistency | null;
  verificationLevel: VerificationLevel | null;
  /** What the check actually performed. Null for a run that failed
   *  verification and for one nothing has checked yet. */
  verificationLevelAchieved: VerificationLevel | null;
  verificationStatus: SnapshotVerificationStatus;
  entriesScanned: number | null;
  files: number | null;
  directories: number | null;
  logicalBytes: number | null;
  sourceBytesRead: number | null;
  repositoryBytesWritten: number | null;
  contentReusedBytes: number | null;
  /** Whether the walk captured every entry it found. Null is "this run
   *  did not report", which is not the same claim as `false`. */
  sourceComplete: boolean | null;
  lastKnownGood: boolean;
  reason: string;
  startedAt: string;
  completedAt: string | null;
  durationSeconds: number | null;
  deleteRequestedAt: string | null;
  holds: SnapshotHold[];
}

/** One edge of the snapshot state machine, as it actually happened. The
 *  run record is overwritten by every advance, so it can say what a run
 *  IS and never how it got there; this is what tells a run verified twice
 *  from one verified once. */
export interface SnapshotTransition {
  from: string | null;
  to: string;
  at: string;
  detail: string;
}

/** One run plus the transition log that says how it got where it is. */
export interface SnapshotDetail {
  snapshot: Snapshot;
  transitions: SnapshotTransition[];
}

/** One reason a snapshot survived: which tier selected it, and what about
 *  the snapshot the tier selected it for. */
export interface SnapshotRetentionTier {
  tier: string;
  /** Absent for last-known-good protection, which selects a snapshot
   *  without a placement to name. */
  selectedBy: string | null;
}

/** What snapshot retention would do about one snapshot, and why. A
 *  PREVIEW: nothing is removed until a pass is applied, and a hold placed
 *  before then changes the answer. */
export interface SnapshotRetentionVerdict {
  runId: string;
  snapshotId: string | null;
  action: SnapshotRetentionAction;
  startedAt: string;
  tiers: SnapshotRetentionTier[];
  holds: SnapshotHold[];
  reason: string;
  holdReason: string | null;
}

export interface SnapshotRetentionPreview {
  generatedAt: string;
  verdicts: SnapshotRetentionVerdict[];
}

/** The three words a repository domain's health is reported in — the same
 *  three a backup set's health uses, because one severity scale per
 *  dashboard is what stops an operator working out which "degraded" is
 *  the worse one. */
export type RepositoryState = "HEALTHY" | "DEGRADED" | "FAILING";

/**
 * One repository domain's own health, which is a different question from
 * any backup set's: a set can be perfectly fresh while the repository
 * holding its snapshots is unwritable, out of maintenance, or reachable
 * only by a process whose clock has drifted far enough to mis-order
 * manifests.
 *
 * Every probe is carried separately rather than reduced to one boolean,
 * because the remedies differ: unreachable is a mount, unwritable is a
 * permission, invalid credentials is a passphrase, and overdue
 * maintenance is a schedule.
 */
export interface RepositoryHealth {
  domain: string;
  /** Whether a second backup set may join this domain. False is an
   *  isolated store, and a second set pointed at it is refused rather
   *  than quietly admitted. */
  mayShare: boolean;
  state: RepositoryState;
  reachable: boolean;
  readable: boolean;
  writable: boolean;
  credentialsValid: boolean;
  clockSane: boolean;
  /** Whole seconds, SIGNED, and nullable. Positive is a clock ahead of
   *  this deployment's newest durable timestamp; negative is one behind,
   *  which is the dangerous direction because it dates a new snapshot
   *  before one already stored. Null is "not measured" and is never drawn
   *  as a perfect zero. */
  clockSkewSeconds: number | null;
  maintenanceOverdue: boolean;
  lastMaintenanceAt: string | null;
  lastMaintenanceResult: string;
  lastSnapshotAt: string | null;
  lastSnapshotStatus: string;
  lastVerificationAt: string | null;
  lastVerificationStatus: string;
  /** The backup sets storing snapshots here, by their full source/set id. */
  backupSets: string[];
  detail: string;
}

export interface RepositoryFleet {
  generatedAt: string;
  repositories: RepositoryHealth[];
}

/**
 * Who owns one repository's maintenance, when it last ran and when it is
 * next eligible.
 *
 * Ownership is the load-bearing part. Several deployments may share one
 * repository and exactly one of them may maintain it, so an operator
 * looking at a repository that is not being maintained has to be able to
 * tell "nobody owns it" from "the owner is somebody else" — which is also
 * what decides whether any control on the screen can be pressed.
 */
export interface RepositoryMaintenance {
  domain: string;
  /** The instance that owns maintenance. "" is "nobody has claimed it".
   *
   *  There is no expiry beside it, and that absence is deliberate:
   *  ownership moves by an explicit transfer and never by a clock, so a
   *  "claim expires" field could only ever be some other timestamp
   *  wearing that label. The wire dropped `owned_until` for exactly that
   *  reason. */
  owner: string;
  lastQuickAt: string | null;
  lastFullAt: string | null;
  nextEligibleAt: string | null;
  due: boolean;
  dueMode: string;
  dueReason: string;
  overdue: boolean;
  runs: number;
  failures: number;
  reclaimedBytes: number;
  failing: boolean;
}

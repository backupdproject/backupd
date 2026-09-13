/**
 * The placeholder deployment every mock-up screen is drawn against
 * (issue #788, the design gate).
 *
 * It is one coherent fictional site rather than per-screen sample values,
 * because the thing being reviewed is a WORKFLOW: the domain an operator
 * picks in the wizard has to be the domain the snapshot list attributes a
 * snapshot to, and the snapshot the restore flow restores has to be one
 * the retention view can put a hold on. Numbers that only agree within one
 * screenshot hide exactly the mistakes a design review is for.
 *
 * Every field name here is the vocabulary the domain model already uses —
 * `docs/adr/0009` (source-consistency modes), `0010` (engine, repository
 * domain), `0014` (verification levels and the ACHIEVED level), `0015`
 * (retention, holds, last-known-good), `0017` (maintenance ownership) and
 * `core/internal/backupengine`'s `SnapshotInfo` / `TreeSnapshotInfo` (the
 * four byte counts that must never be presented as one another). The
 * mock-up shows an operator's words on screen and the wire's word in
 * monospace beside it, so the review can check both at once.
 *
 * Nothing here is fetched and nothing here is written. This module is
 * dev-only: see mockup/MockupApp.tsx.
 */

/** Which engine a backup set runs. `model.BackupEngine`'s two values. */
export type MockEngine = "artifact" | "kopia";

/** `model.RepositoryIsolation`. */
export type MockIsolation = "shared" | "isolated";

/** ADR 0009 §1. Declared by the operator, never detected. */
export type MockConsistencyMode = "live_best_effort" | "externally_quiesced" | "external_snapshot";

/** ADR 0014 §1, deepest last. */
export type MockVerificationLevel = "structural" | "content_sample" | "content_full" | "restore_drill";

/** How each engine is named to an operator, and what it does to a source.
 *  The mock-up never shows one without the other: "which engine" is the
 *  only choice in this workflow that changes what a run does to somebody's
 *  data. */
export const ENGINE_COPY: Record<MockEngine, { name: string; wire: string; summary: string }> = {
  artifact: {
    name: "Artifact",
    wire: "artifact",
    summary:
      "Pulls finished files a producer has dropped somewhere, whole, and keeps each one as its own backup."
  },
  kopia: {
    name: "Incremental",
    wire: "kopia",
    summary:
      "Snapshots a whole source tree into an encrypted repository, storing only content the repository does not already hold."
  }
};

export const CONSISTENCY_COPY: Record<
  MockConsistencyMode,
  { name: string; wire: string; summary: string; pointInTime: string }
> = {
  live_best_effort: {
    name: "Live, best effort",
    wire: "live_best_effort",
    summary:
      "Read while whatever writes to the source keeps writing. Each file is captured coherently or reported; the set of files is not a single moment.",
    pointInTime: "No"
  },
  externally_quiesced: {
    name: "Quiesced by you",
    wire: "externally_quiesced",
    summary:
      "You stop, flush or lock the writers for the run. A change seen during the run is reported as a broken arrangement, not shrugged off.",
    pointInTime: "No"
  },
  external_snapshot: {
    name: "Frozen image",
    wire: "external_snapshot",
    summary:
      "The run reads a frozen copy you made first (LVM, ZFS, VSS, a read-only clone). The only mode a backup may be called consistent under.",
    pointInTime: "Yes"
  }
};

export const VERIFICATION_COPY: Record<
  MockVerificationLevel,
  { name: string; wire: string; finds: string; cost: string }
> = {
  structural: {
    name: "Structure",
    wire: "structural",
    finds: "A snapshot whose tree or content references no longer resolve.",
    cost: "Runs on every backup, no extra reads"
  },
  content_sample: {
    name: "Structure + sampled content",
    wire: "content_sample",
    finds: "Content the repository still references and no longer has, without reading the whole tree.",
    cost: "Reads a fixed share of files"
  },
  content_full: {
    name: "Structure + all content",
    wire: "content_full",
    finds: "Damage inside stored data that is still present.",
    cost: "Reads every file in the snapshot"
  },
  restore_drill: {
    name: "Restore drill",
    wire: "restore_drill",
    finds: "A restore that truncates, misplaces, stubs or invents files.",
    cost: "Restores the snapshot to a scratch directory"
  }
};

export interface MockDomain {
  id: string;
  description: string;
  isolation: MockIsolation;
  location: string;
  /** Backup sets whose snapshots live here. */
  sets: string[];
  snapshots: number;
  physicalBytes: number;
  /** ADR 0017 §1: exactly one instance owns maintenance for a domain. */
  maintenanceOwner: string;
  ownedHere: boolean;
  lastQuickMaintenance: string;
  lastFullMaintenance: string;
  fullMaintenanceDue: string;
  health: "ok" | "warn" | "danger";
  healthNote: string;
}

export const DOMAINS: MockDomain[] = [
  {
    id: "primary-nas",
    description: "Everything backed up to the local array",
    isolation: "shared",
    location: "/data/backups/repositories/primary-nas",
    sets: ["production/file-server", "production/mail-store", "staging/build-cache"],
    snapshots: 1284,
    physicalBytes: 812 * 1024 ** 3,
    maintenanceOwner: "this instance (nas-01)",
    ownedHere: true,
    lastQuickMaintenance: "2026-09-13T02:14:00Z",
    lastFullMaintenance: "2026-09-07T02:31:00Z",
    fullMaintenanceDue: "2026-09-14T02:00:00Z",
    health: "ok",
    healthNote: "Reachable, writable, credentials valid, clock within tolerance."
  },
  {
    id: "offsite-b2",
    description: "Second copy, off site",
    isolation: "shared",
    location: "b2://acme-backups/primary",
    sets: ["production/file-server", "production/mail-store"],
    snapshots: 486,
    physicalBytes: 731 * 1024 ** 3,
    maintenanceOwner: "this instance (nas-01)",
    ownedHere: true,
    lastQuickMaintenance: "2026-09-12T03:05:00Z",
    lastFullMaintenance: "2026-08-24T03:40:00Z",
    fullMaintenanceDue: "2026-09-10T03:00:00Z",
    health: "warn",
    healthNote: "Full maintenance is 3 days overdue. Nothing is at risk yet; storage is not being reclaimed."
  },
  {
    id: "finance-isolated",
    description: "Finance records; shares nothing with any other set",
    isolation: "isolated",
    location: "/data/backups/repositories/finance-isolated",
    sets: ["production/finance-db"],
    snapshots: 213,
    physicalBytes: 96 * 1024 ** 3,
    maintenanceOwner: "nas-02",
    ownedHere: false,
    lastQuickMaintenance: "2026-09-13T01:02:00Z",
    lastFullMaintenance: "2026-09-06T01:20:00Z",
    fullMaintenanceDue: "2026-09-13T01:00:00Z",
    health: "ok",
    healthNote: "Maintained by nas-02. This instance reads and writes snapshots but will not maintain it."
  }
];

/** What co-tenancy in one domain shares, all of it or none of it
 *  (`model.RepositoryBoundaries`). The wizard and the domain form both
 *  state this before an operator picks `shared`. */
export const DOMAIN_BOUNDARIES: { title: string; detail: string }[] = [
  { title: "Encryption", detail: "One key opens every snapshot in the domain." },
  { title: "Credential", detail: "One credential reaches the storage behind it." },
  { title: "Failure and corruption", detail: "Damage to the store is damage to every set in it." },
  { title: "Maintenance", detail: "One instance owns maintenance for the whole domain." },
  { title: "Deduplication", detail: "Content shared between sets is stored once — the reason to share." },
  { title: "Administrative trust", detail: "Anyone who can restore one set here can read them all." }
];

export interface MockSet {
  source: string;
  set: string;
  engine: MockEngine;
  domain: string | null;
  root: string;
  consistency: MockConsistencyMode;
  verification: MockVerificationLevel;
  fullEvery: string;
  drillEvery: string;
  pollInterval: string;
  state: "healthy" | "degraded" | "stale";
  lastRunAt: string;
  nextRunAt: string;
  snapshots: number;
  holds: number;
  lastSnapshotId: string;
}

export const SETS: MockSet[] = [
  {
    source: "production",
    set: "file-server",
    engine: "kopia",
    domain: "primary-nas",
    root: "/srv/shares",
    consistency: "external_snapshot",
    verification: "content_sample",
    fullEvery: "7 days",
    drillEvery: "30 days",
    pollInterval: "every 6h",
    state: "healthy",
    lastRunAt: "2026-09-13T04:00:00Z",
    nextRunAt: "2026-09-13T10:00:00Z",
    snapshots: 412,
    holds: 2,
    lastSnapshotId: "9f2a1c7e40b83d55"
  },
  {
    source: "production",
    set: "mail-store",
    engine: "kopia",
    domain: "primary-nas",
    root: "/var/vmail",
    consistency: "externally_quiesced",
    verification: "content_full",
    fullEvery: "every run",
    drillEvery: "14 days",
    pollInterval: "every 12h",
    state: "degraded",
    lastRunAt: "2026-09-13T01:30:00Z",
    nextRunAt: "2026-09-13T13:30:00Z",
    snapshots: 388,
    holds: 0,
    lastSnapshotId: "c31d09b4e7a25f80"
  },
  {
    source: "production",
    set: "finance-db",
    engine: "kopia",
    domain: "finance-isolated",
    root: "/srv/finance/dumps",
    consistency: "external_snapshot",
    verification: "restore_drill",
    fullEvery: "7 days",
    drillEvery: "7 days",
    pollInterval: "every 24h",
    state: "healthy",
    lastRunAt: "2026-09-13T02:10:00Z",
    nextRunAt: "2026-09-14T02:10:00Z",
    snapshots: 213,
    holds: 1,
    lastSnapshotId: "6b70ff1c2d94a3e1"
  },
  {
    source: "production",
    set: "api-server",
    engine: "artifact",
    domain: null,
    root: "/var/backups/api-server",
    consistency: "live_best_effort",
    verification: "structural",
    fullEvery: "not applicable",
    drillEvery: "not applicable",
    pollInterval: "every 30m",
    state: "healthy",
    lastRunAt: "2026-09-13T05:30:00Z",
    nextRunAt: "2026-09-13T06:00:00Z",
    snapshots: 0,
    holds: 0,
    lastSnapshotId: ""
  }
];

export const INCREMENTAL_SET = SETS[0];
export const ARTIFACT_SET = SETS[3];

export interface MockSnapshot {
  id: string;
  startedAt: string;
  durationSeconds: number;
  /** SnapshotInfo.Files + Directories: what the walk visited. */
  entries: number;
  files: number;
  directories: number;
  /** SnapshotInfo.Bytes — the logical size of the tree as the source described it. */
  logicalBytes: number;
  /** TreeSnapshotInfo.SourceBytesRead — what this run pulled off the source. */
  sourceReadBytes: number;
  /** TreeSnapshotInfo.RepositoryBytesWritten — what landed in storage. */
  writtenBytes: number;
  /** TreeSnapshotInfo.ContentReusedBytes, meaningless unless measured. */
  reusedBytes: number;
  reuseMeasured: boolean;
  /** ADR 0014: the level the verification ACTUALLY performed. */
  achievedLevel: MockVerificationLevel | null;
  verification: "passed" | "failed" | "pending";
  verifiedAt: string | null;
  lastKnownGood: boolean;
  holds: MockHold[];
  incompleteReason: string;
  consistencyViolation: string;
}

export const SNAPSHOTS: MockSnapshot[] = [
  {
    id: "9f2a1c7e40b83d55",
    startedAt: "2026-09-13T04:00:00Z",
    durationSeconds: 214,
    entries: 148_902,
    files: 141_286,
    directories: 7_616,
    logicalBytes: 1_412 * 1024 ** 3,
    sourceReadBytes: 1_412 * 1024 ** 3,
    writtenBytes: 3.4 * 1024 ** 3,
    reusedBytes: 1_402 * 1024 ** 3,
    reuseMeasured: true,
    achievedLevel: "content_sample",
    verification: "passed",
    verifiedAt: "2026-09-13T04:05:00Z",
    lastKnownGood: true,
    holds: [],
    incompleteReason: "",
    consistencyViolation: ""
  },
  {
    id: "41c8b0d5e9376a2f",
    startedAt: "2026-09-12T22:00:00Z",
    durationSeconds: 233,
    entries: 148_744,
    files: 141_140,
    directories: 7_604,
    logicalBytes: 1_409 * 1024 ** 3,
    sourceReadBytes: 1_409 * 1024 ** 3,
    writtenBytes: 5.1 * 1024 ** 3,
    reusedBytes: 1_396 * 1024 ** 3,
    reuseMeasured: true,
    achievedLevel: "content_full",
    verification: "passed",
    verifiedAt: "2026-09-12T22:41:00Z",
    lastKnownGood: false,
    holds: [
      {
        id: "hold-3f21",
        reason: "Kept for the 2026 Q3 audit",
        placedBy: "r.okonkwo",
        placedAt: "2026-09-12T23:10:00Z"
      }
    ],
    incompleteReason: "",
    consistencyViolation: ""
  },
  {
    id: "8d5310af6c2be974",
    startedAt: "2026-09-12T16:00:00Z",
    durationSeconds: 198,
    entries: 148_502,
    files: 140_922,
    directories: 7_580,
    logicalBytes: 1_407 * 1024 ** 3,
    sourceReadBytes: 1_407 * 1024 ** 3,
    writtenBytes: 2.8 * 1024 ** 3,
    reusedBytes: 1_398 * 1024 ** 3,
    reuseMeasured: true,
    achievedLevel: "content_sample",
    verification: "passed",
    verifiedAt: "2026-09-12T16:04:00Z",
    lastKnownGood: false,
    holds: [],
    incompleteReason: "",
    consistencyViolation: ""
  },
  {
    id: "2e97c604ba1d8f33",
    startedAt: "2026-09-12T10:00:00Z",
    durationSeconds: 402,
    entries: 148_310,
    files: 140_760,
    directories: 7_550,
    logicalBytes: 1_404 * 1024 ** 3,
    sourceReadBytes: 1_404 * 1024 ** 3,
    writtenBytes: 6.9 * 1024 ** 3,
    reusedBytes: 0,
    reuseMeasured: false,
    achievedLevel: null,
    verification: "failed",
    verifiedAt: "2026-09-12T10:11:00Z",
    lastKnownGood: false,
    holds: [],
    incompleteReason: "",
    consistencyViolation:
      "Six files under /srv/shares/finance changed while the run was reading them, and this set is declared as a frozen image."
  },
  {
    id: "b0447e91cd3a625f",
    startedAt: "2026-09-12T04:00:00Z",
    durationSeconds: 221,
    entries: 148_120,
    files: 140_590,
    directories: 7_530,
    logicalBytes: 1_401 * 1024 ** 3,
    sourceReadBytes: 1_401 * 1024 ** 3,
    writtenBytes: 4.2 * 1024 ** 3,
    reusedBytes: 1_390 * 1024 ** 3,
    reuseMeasured: true,
    achievedLevel: "restore_drill",
    verification: "passed",
    verifiedAt: "2026-09-12T05:02:00Z",
    lastKnownGood: false,
    holds: [
      {
        id: "hold-91ab",
        reason: "Legal hold — matter 2026-114",
        placedBy: "legal.ops",
        placedAt: "2026-09-12T09:00:00Z"
      }
    ],
    incompleteReason: "",
    consistencyViolation: ""
  }
];

/**
 * The connection test, and the line on it that decides whether this
 * product is ever allowed to delete anything on somebody's server
 * (issue #852).
 *
 * Reachability and authentication are the checks an operator expects. The
 * write probe is the one that carries a safety consequence: Backupd
 * writes a scratch file under the source path and deletes it again, and
 * the ANSWER to that — not an operator's belief about their own sudoers
 * file — is what arms or refuses "delete from source after backup". A
 * credential that cannot prove it can write is never offered a control
 * that deletes.
 */
export interface MockConnectionCheck {
  label: string;
  state: "ok" | "warn" | "danger";
  detail: string;
}

export const CONNECTION_CHECKS_WRITABLE: MockConnectionCheck[] = [
  { label: "Server reachable", state: "ok", detail: "prod-files-01.internal:22 answered in 41 ms." },
  { label: "Host key", state: "ok", detail: "Matches the key you trusted on the previous step." },
  { label: "Authentication", state: "ok", detail: "Signed in as backup-agent with the key for this set." },
  { label: "Source path readable", state: "ok", detail: "/srv/shares listed 6 entries." },
  {
    label: "Write permission",
    state: "ok",
    detail:
      "Wrote and deleted a scratch file under /srv/shares. Backupd can remove files there when a backup set asks it to."
  },
  { label: "Clock", state: "ok", detail: "The server and this NAS agree to within 1 second." }
];

export const CONNECTION_CHECKS_READ_ONLY: MockConnectionCheck[] = [
  { label: "Server reachable", state: "ok", detail: "prod-files-01.internal:22 answered in 39 ms." },
  { label: "Host key", state: "ok", detail: "Matches the key you trusted on the previous step." },
  { label: "Authentication", state: "ok", detail: "Signed in as backup-reader with the key for this set." },
  { label: "Source path readable", state: "ok", detail: "/srv/shares listed 6 entries." },
  {
    label: "Write permission",
    state: "warn",
    detail:
      "The scratch file could not be created under /srv/shares: permission denied. Backups still run; deleting from the source is switched off and cannot be turned on."
  },
  { label: "Clock", state: "ok", detail: "The server and this NAS agree to within 1 second." }
];

/** The copy the disabled delete control carries, in one place, because the
 *  wizard and the per-set configuration screen must not word the same
 *  refusal differently (#852). */
export const SOURCE_DELETE_COPY = {
  label: "Delete from source after a verified backup",
  note: "Frees space on the server once Backupd holds a verified copy.",
  readOnlyNote: "Unavailable: these credentials cannot write to the source."
};

export interface MockHold {
  id: string;
  reason: string;
  placedBy: string;
  placedAt: string;
}

/** ADR 0015 §1: the retention chain a set is governed by, and what each
 *  tier keeps. Named tiers, operator-defined, so the mock-up shows one
 *  that is not the documented default. */
export const RETENTION_TIERS: { name: string; keep: string; kept: number; nextExpiry: string }[] = [
  { name: "Daily", keep: "7 days", kept: 7, nextExpiry: "2026-09-14 04:00" },
  { name: "Weekly", keep: "13 weeks", kept: 13, nextExpiry: "2026-09-15 04:00" },
  { name: "Monthly", keep: "12 months", kept: 12, nextExpiry: "2026-10-01 04:00" }
];

/** What a retention pass would do to each snapshot, and why. The "why" is
 *  the whole panel: an operator reading a delete row has to see what
 *  stopped, or failed to stop, the deletion. */
export const RETENTION_PROJECTION: {
  id: string;
  takenAt: string;
  verdict: "keep" | "delete";
  reason: string;
}[] = [
  { id: "9f2a1c7e40b83d55", takenAt: "2026-09-13 04:00", verdict: "keep", reason: "Daily · newest known-good" },
  { id: "41c8b0d5e9376a2f", takenAt: "2026-09-12 22:00", verdict: "keep", reason: "Hold: 2026 Q3 audit" },
  { id: "8d5310af6c2be974", takenAt: "2026-09-12 16:00", verdict: "delete", reason: "No tier selects it, no hold" },
  { id: "2e97c604ba1d8f33", takenAt: "2026-09-12 10:00", verdict: "delete", reason: "Verification failed · no tier selects it" },
  { id: "b0447e91cd3a625f", takenAt: "2026-09-12 04:00", verdict: "keep", reason: "Daily · legal hold 2026-114" },
  { id: "77ce2a0148bd93f6", takenAt: "2026-09-11 04:00", verdict: "keep", reason: "Weekly" },
  { id: "d4b1908f35e6ac27", takenAt: "2026-09-01 04:00", verdict: "keep", reason: "Monthly" }
];

/** ADR 0014's health inputs, as an operator reads them: what was checked,
 *  what it found, and what to do about it. */
export const HEALTH_CHECKS: {
  label: string;
  state: "ok" | "warn" | "danger";
  detail: string;
}[] = [
  { label: "Storage answers a read", state: "ok", detail: "Proved 2 minutes ago, not assumed from an open handle." },
  { label: "Storage answers a write", state: "ok", detail: "Proved 2 minutes ago against a scratch object." },
  { label: "Encryption credentials", state: "ok", detail: "The stored passphrase opens the repository." },
  {
    label: "Maintenance not overdue",
    state: "warn",
    detail: "Full maintenance last completed 20 days ago on offsite-b2, 3 days past its window."
  },
  {
    label: "Clock sanity",
    state: "ok",
    detail: "This machine and the storage agree to within 2 seconds."
  },
  { label: "Last snapshot", state: "ok", detail: "production/file-server, 2 hours ago, completed." },
  {
    label: "Last verification",
    state: "warn",
    detail: "production/mail-store proved structure only on its last run; the set asks for all content."
  }
];

export interface MockMaintenanceRow {
  domain: string;
  owner: string;
  ownedHere: boolean;
  lastQuick: string;
  lastFull: string;
  due: string;
  state: "idle" | "running" | "overdue" | "refused";
  note: string;
}

export const MAINTENANCE: MockMaintenanceRow[] = [
  {
    domain: "primary-nas",
    owner: "nas-01 (this instance)",
    ownedHere: true,
    lastQuick: "11 hours ago",
    lastFull: "6 days ago",
    due: "tomorrow, 02:00",
    state: "idle",
    note: "Nothing due. Quick maintenance runs after each backup pass."
  },
  {
    domain: "offsite-b2",
    owner: "nas-01 (this instance)",
    ownedHere: true,
    lastQuick: "1 day ago",
    lastFull: "20 days ago",
    due: "3 days ago",
    state: "overdue",
    note: "Full maintenance has not completed inside its window. Storage is not being reclaimed."
  },
  {
    domain: "finance-isolated",
    owner: "nas-02",
    ownedHere: false,
    lastQuick: "12 hours ago",
    lastFull: "7 days ago",
    due: "today, 01:00",
    state: "refused",
    note: "This instance will not maintain a domain another instance owns. Transfer ownership to change that."
  }
];

/** The restore flow's file picker. A tree deep enough to show selection
 *  state and a size column, and no deeper. */
export const RESTORE_TREE: { path: string; depth: number; kind: "dir" | "file"; size: number; selected: boolean }[] = [
  { path: "srv/shares", depth: 0, kind: "dir", size: 1_412 * 1024 ** 3, selected: true },
  { path: "finance", depth: 1, kind: "dir", size: 84 * 1024 ** 3, selected: true },
  { path: "ledger-2026.sqlite", depth: 2, kind: "file", size: 12 * 1024 ** 3, selected: true },
  { path: "invoices", depth: 2, kind: "dir", size: 72 * 1024 ** 3, selected: true },
  { path: "engineering", depth: 1, kind: "dir", size: 902 * 1024 ** 3, selected: false },
  { path: "marketing", depth: 1, kind: "dir", size: 426 * 1024 ** 3, selected: false }
];

/** Sizes and counts the mock-up quotes more than once. Held here so two
 *  screens cannot disagree about the same figure. */
export const TOTALS = {
  domains: DOMAINS.length,
  incrementalSets: SETS.filter((s) => s.engine === "kopia").length,
  artifactSets: SETS.filter((s) => s.engine === "artifact").length,
  snapshots: DOMAINS.reduce((n, d) => n + d.snapshots, 0),
  physicalBytes: DOMAINS.reduce((n, d) => n + d.physicalBytes, 0)
};

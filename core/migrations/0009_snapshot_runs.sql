-- 0009_snapshot_runs: the durable catalog of snapshot RUNS for the
-- incremental (kopia) engine (EPIC K, issue #783).
--
-- WHY THIS EXISTS. Everything this journal records so far is about
-- artifacts: one row per file this product moved, with a lifecycle it
-- drove step by step. The incremental engine does not produce artifacts.
-- It produces one snapshot per run of a whole source tree, inside a
-- repository that keeps its own manifests, and the only durable trace of
-- a run is whatever this journal writes down about it.
--
-- That makes this table the answer to three questions nothing else can
-- answer after a crash:
--
--   1. "What was in flight when this process died?" A snapshot run has
--      phases with genuinely different recovery rules. A run killed
--      during the scan has written nothing to the repository; one killed
--      after the manifest was committed has a real, durable snapshot in
--      the repository that nothing has verified or catalogued yet.
--      Guessing between those means either abandoning a good snapshot or
--      advertising an unverified one, so the phase is written down before
--      each step rather than reconstructed afterwards.
--
--   2. "What may I offer to restore?" Exactly one run per backup set is
--      the last-known-good one, and it is marked here rather than derived
--      by "the newest successful row", because derivation is what gets
--      this wrong: the code that computes it lives one layer above the
--      durable record, and a newer run that failed must not be able to
--      displace an older restore point by existing. The flag moves only
--      when a run reaches SUCCESS, in the same transaction, and the
--      partial unique index below is the schema's own insistence that a
--      set can never have two.
--
--   3. "Is this repository snapshot one of ours?" A repository contains
--      manifests, not backup sets. Reconciliation walks them and has to
--      attribute each one to a run of ours or quarantine it, and that
--      attribution is a lookup on (domain, snapshot_id) here.
--
-- WHY A SEPARATE TABLE AND NOT MORE COLUMNS ON operations. An operation
-- row is a receipt held by an API client (see internal/state/operations.go
-- for the full argument): it records that a request was made and how it
-- ended. A snapshot run is the manager's own account of a piece of work
-- with an internal state machine, and most runs have no client at all
-- because a scheduled cycle started them. operation_id below is a
-- back-reference for the ones that do, and it is empty for the ones that
-- do not.
--
-- WHO OWNS THE PHASE VOCABULARY. Go does, in
-- core/internal/state/snapshots.go, exactly as internal/lifecycle owns
-- artifacts.state and internal/placement owns placements.verification_class.
-- The CHECK constraint below is a backstop against a write that bypasses
-- that package, not the definition: what a phase MEANS, which moves
-- between them are legal, and which of them are terminal is decided in Go
-- and pinned against this list by a test. A phase added here without
-- being added there would be a value nothing can read back.
--
-- WHAT MUST NEVER BE WRITTEN HERE. No secrets and no full source paths.
-- source_identity is already a digest computed by internal/model, never a
-- path or a credential, and it is stored as the opaque string it is.
-- reason is one sentence of operator prose written by the caller (why a
-- run failed, why a snapshot was quarantined, why it is lost) and it goes
-- through the same redactor state_transitions.detail does before it is
-- written (issue #295). Nothing in this table holds a file name from
-- inside the source tree, a repository password, or a connection string,
-- and a future column that would is a column that belongs somewhere else.
--
-- TWO TABLES, FOR THE REASON artifacts AND state_transitions ARE TWO. The
-- run row is overwritten by every advance, so it can say what a run IS
-- and never how it got there. A run that was verified twice because a
-- crash interrupted the first attempt, and a run that was verified once,
-- read identically on the row; only the append-only log tells them apart,
-- and that is exactly the question an operator asks when a snapshot is
-- being investigated.

CREATE TABLE snapshot_runs (
    id                  INTEGER PRIMARY KEY,

    -- run_id is the caller's own identifier for this attempt, and
    -- idempotency_key is what makes a resubmitted attempt resolve to the
    -- row that already exists. They are separate for the reason
    -- operations.operation_id and operations.idempotency_key are: one is
    -- what a later caller looks the run up by, the other is a promise
    -- about one logical piece of work. Both are UNIQUE, so a replay is
    -- decided by the database rather than by a check-then-insert race.
    run_id              TEXT NOT NULL UNIQUE,
    idempotency_key     TEXT NOT NULL UNIQUE,

    -- model.BackupSetID's two halves, stored split the way artifacts,
    -- backup_set_halts and backup_set_addresses already store them, so
    -- nothing has to take a joined "source/set" string back apart to ask
    -- a per-set question. Every question this table answers is per set.
    source              TEXT NOT NULL,
    backup_set          TEXT NOT NULL,

    -- The operations row this run belongs to, or '' for a scheduled cycle
    -- that no client asked for. It is deliberately not a foreign key:
    -- most runs have no operation, and a text column that is empty for
    -- them says that more honestly than a nullable reference nobody can
    -- join on anyway.
    operation_id        TEXT NOT NULL DEFAULT '',

    -- engine is model.BackupEngine's string and domain is
    -- model.RepositoryDomainID's. They are stored as the strings they are
    -- and not constrained to a closed set here, for the reason
    -- placements.medium is not: the set of configured domains lives in
    -- the operator's configuration, which this schema cannot see and must
    -- not freeze a copy of.
    engine              TEXT NOT NULL,
    domain              TEXT NOT NULL,

    -- source_identity is model.SourceIdentity's string: a digest of what
    -- was backed up, never the path. It is here because it is half of
    -- what makes a replayed idempotency key legitimate (a key presented
    -- for a different source is a different piece of work) and because
    -- reconciliation attributes a repository snapshot by it.
    source_identity     TEXT NOT NULL,

    consistency_mode    TEXT NOT NULL DEFAULT '',

    -- The two verification claims, kept in two columns because they are
    -- two different facts and a row that mixed them would advertise a
    -- verification that never ran. verification_level is the level this
    -- run was CONFIGURED for, which is what the operator asked for and
    -- does not change. verification_level_achieved is the level a
    -- verification actually PROVED, and '' means nothing was proven. A
    -- run configured for a full content verification whose verification
    -- only managed a structural one must read as exactly that, which is
    -- impossible to express in one column.
    verification_level          TEXT NOT NULL DEFAULT '',
    verification_level_achieved TEXT NOT NULL DEFAULT '',

    -- phase is the run's state machine. The vocabulary is owned by Go
    -- (see this file's preamble) and this CHECK is the schema's backstop.
    --
    -- The nominal path is PENDING -> SOURCE_SCAN -> SNAPSHOT_WRITE ->
    -- MANIFEST_COMMITTED -> VERIFICATION -> CATALOG_COMMIT -> SUCCESS,
    -- and the split at MANIFEST_COMMITTED is the one that matters: before
    -- it a crash has left nothing durable in the repository, after it the
    -- snapshot exists whatever happens to this process.
    --
    -- The other four are ends rather than steps. FAILED is a run that
    -- stopped. LOST is a run that succeeded once and whose snapshot is no
    -- longer in the repository, which is a fact about the repository and
    -- not a failure of the run. DELETED is a delete this product intended
    -- and completed. QUARANTINED is a repository snapshot nothing can
    -- attribute to any configured set, recorded so it is accounted for
    -- and never deleted.
    phase               TEXT NOT NULL DEFAULT 'PENDING'
                        CHECK (phase IN (
                            'PENDING',
                            'SOURCE_SCAN',
                            'SNAPSHOT_WRITE',
                            'MANIFEST_COMMITTED',
                            'VERIFICATION',
                            'CATALOG_COMMIT',
                            'SUCCESS',
                            'FAILED',
                            'LOST',
                            'DELETED',
                            'QUARANTINED'
                        )),

    -- snapshot_id is the engine's own opaque manifest id, empty until the
    -- manifest is committed. Only the engine that wrote it knows how to
    -- interpret it, which is placements.location's contract for the same
    -- reason.
    snapshot_id         TEXT NOT NULL DEFAULT '',

    -- What the run measured. Every one of these is nullable rather than
    -- defaulting to 0, for placements.size_bytes' reason: a source really
    -- can contain zero files and a run really can write zero new bytes to
    -- the repository (a snapshot of unchanged data does), so a zero must
    -- not double as "nobody recorded this". A reconciler deciding whether
    -- a run got far enough to have measured anything reads the difference.
    --
    -- logical_bytes is the SCANNED logical size of the source, which is
    -- not what was read and not what was written: an incremental snapshot
    -- of ten gigabytes may read ten gigabytes, write four megabytes, and
    -- reuse the rest. All three are recorded because a report that
    -- conflates them either overstates what the repository costs or
    -- understates what the run did.
    files                       INTEGER,
    directories                 INTEGER,
    logical_bytes               INTEGER,
    source_bytes_read           INTEGER,
    repository_bytes_written    INTEGER,
    content_reused_bytes        INTEGER,

    -- Whether a verification has run and what it concluded, kept apart
    -- from the phase because a run can be at VERIFICATION with nothing
    -- concluded yet, and a run can be at SUCCESS having passed a
    -- verification that a later re-verification would change. '' means
    -- nothing has been asked of this snapshot.
    verification_status TEXT NOT NULL DEFAULT ''
                        CHECK (verification_status IN ('', 'pending', 'passed', 'failed')),

    -- One sentence from the caller: why it failed, why it was
    -- quarantined, why it is lost. Operator prose, redacted on the way in,
    -- never a stack trace and never a path from inside the source.
    reason              TEXT NOT NULL DEFAULT '',

    -- last_known_good marks the one run per backup set whose snapshot may
    -- be offered as a restore point. Nothing but reaching SUCCESS sets it
    -- and nothing but another run reaching SUCCESS clears it, so a newer
    -- run that failed, was lost, was deleted or was quarantined cannot
    -- displace an older restore point by existing.
    last_known_good     INTEGER NOT NULL DEFAULT 0
                        CHECK (last_known_good IN (0, 1)),

    started_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,

    -- completed_at is when the run came to rest, NULL while it is still
    -- in flight. It is written once: SUCCESS -> LOST and SUCCESS ->
    -- DELETED are facts about the snapshot recorded after the run itself
    -- finished, and they must not move the time the run finished.
    completed_at        TEXT,

    -- delete_requested_at is the durable INTENT to delete this snapshot,
    -- written BEFORE the repository is asked to delete anything so that a
    -- crash in between is a decidable state rather than a guess: a row
    -- with an intent and a snapshot still in the repository is a delete to
    -- resume, and a row without one whose snapshot has gone is a loss to
    -- investigate. It is not a phase, because the phase still describes
    -- the run, and the delete has not happened yet.
    delete_requested_at TEXT
);

-- At most one last-known-good run per backup set, enforced rather than
-- intended. The whole read surface above this table asks "the" restore
-- point for a set, and two rows claiming it would make that answer depend
-- on the query plan. A partial index is what expresses it: any number of
-- rows may have the flag clear.
CREATE UNIQUE INDEX idx_snapshot_runs_last_known_good
    ON snapshot_runs (source, backup_set) WHERE last_known_good = 1;

-- The crash reconciler's worklist is "every run not in a terminal phase,
-- oldest first", read once on startup across all sets.
CREATE INDEX idx_snapshot_runs_phase_started ON snapshot_runs (phase, started_at);

-- Per-set history, newest first, which is what an operator's listing of a
-- set's snapshots reads.
CREATE INDEX idx_snapshot_runs_set_started ON snapshot_runs (source, backup_set, started_at);

-- "What did the request I am polling actually do", read once per operation
-- read on the API's durable-operation surface. It is partial because
-- operation_id is empty for every scheduled cycle, which is most rows in a
-- healthy deployment: those are not a group anybody looks up, they are the
-- absence of one, and indexing them would be paying to keep the one key
-- nothing ever queries.
CREATE INDEX idx_snapshot_runs_operation
    ON snapshot_runs (operation_id, started_at) WHERE operation_id <> '';

-- "Is this repository manifest one of ours" is a lookup on (domain,
-- snapshot_id), and it is UNIQUE rather than merely indexed because the
-- question has exactly one right answer. Two runs claiming one manifest
-- would mean this journal believed two runs produced the same snapshot,
-- and whichever row a reader happened to get would decide whether that
-- snapshot is advertised, retained or deleted. The index is partial
-- because '' is not a manifest id: every run carries it until its manifest
-- is committed.
CREATE UNIQUE INDEX idx_snapshot_runs_domain_snapshot
    ON snapshot_runs (domain, snapshot_id) WHERE snapshot_id <> '';

-- The append-only log of phase changes, one row per edge actually taken.
--
-- It carries no CHECK on its phases on purpose, unlike the run row above.
-- This table records what happened, and a build that retired a phase its
-- predecessor wrote must still be able to hold the history that mentions
-- it. The run row is a live value and is constrained; the log is evidence
-- and is not.
CREATE TABLE snapshot_run_transitions (
    id           INTEGER PRIMARY KEY,
    run_id       TEXT NOT NULL REFERENCES snapshot_runs (run_id),
    from_phase   TEXT NOT NULL,
    to_phase     TEXT NOT NULL,
    occurred_at  TEXT NOT NULL,

    -- The caller's sentence for this edge, redacted on the way in, or ''.
    detail       TEXT NOT NULL DEFAULT ''
);

-- The log is read per run, in the order it was written. The rowid is the
-- tiebreak for two edges recorded inside one clock tick, which is the same
-- ordering state.LastTransition uses over state_transitions.
CREATE INDEX idx_snapshot_run_transitions_run ON snapshot_run_transitions (run_id, id);

-- 0011_snapshot_holds: the durable record of a snapshot a person has said
-- must not be deleted, and of the person who later said it may be (EPIC K,
-- issue #785).
--
-- WHY THIS EXISTS. Retention is a calculation over a calendar
-- (internal/retention's GFS tiers) plus one protected restore point
-- (last-known-good). Both of those are derived: run the same policy over
-- the same history tomorrow and they can name different snapshots, which
-- is exactly what a retention policy is for. A hold is the opposite kind
-- of fact. It is somebody's decision, about one specific snapshot, for a
-- reason no policy can compute -- a legal matter, an incident under
-- investigation, a migration nobody wants to redo -- and it has to
-- survive every recalculation, every restart, and every operator who was
-- not in the room when it was placed.
--
-- So it is a row, not a flag on the run and not a field in the config
-- file. A flag could not say who placed it or why, and a config file is
-- the wrong custody model for a fact that outlives an edit: the whole
-- value of a hold is that it is still there when nobody remembers it.
--
-- WHY RELEASING IS A COLUMN AND NOT A DELETE. "Who released the hold on
-- the snapshot that was then deleted, and when" is the first question
-- asked after a deletion somebody disputes, and a DELETE from this table
-- answers it with silence. A released hold is history, and history in
-- this journal is append-only for the same reason state_transitions is:
-- the run row says what is, the log says what happened, and only the
-- second one can be audited.
--
-- WHY SEVERAL HOLDS MAY SIT ON ONE SNAPSHOT. A legal hold and an
-- operator's "do not touch this until the migration is signed off" are
-- two decisions by two people, and releasing one must not release the
-- other. One row each is what makes that true without anybody having to
-- remember it; the snapshot is protected while ANY row over it is
-- unreleased.
--
-- WHAT A HOLD DOES NOT DO. It does not pin repository storage, and
-- nothing here reaches into the repository at all. A held snapshot's
-- manifest is simply never removed, and its content therefore stays
-- referenced, so the engine's own maintenance (issue #786) will not
-- reclaim it -- which is the whole mechanism, and the reason this product
-- must never enable a competing retention schedule inside the engine: a
-- hold this table records is invisible to it.

CREATE TABLE snapshot_holds (
    id          INTEGER PRIMARY KEY,

    -- hold_id is the caller's own identifier, and it is UNIQUE so that a
    -- caller which crashed between placing a hold and observing it
    -- resolves to the hold it already placed rather than stacking a
    -- second one over the same snapshot. It is the same contract
    -- snapshot_runs.idempotency_key has, for the same reason.
    hold_id     TEXT NOT NULL UNIQUE,

    -- The run whose snapshot is held. A real reference, unlike
    -- snapshot_runs.operation_id: a hold over a run this journal does not
    -- have is a hold over nothing, and the write path refuses it in a
    -- sentence before the constraint ever has to.
    run_id      TEXT NOT NULL REFERENCES snapshot_runs (run_id),

    -- The lineage the held run belongs to, copied off the run row at
    -- placement time and never supplied by the caller. It is denormalized
    -- deliberately: the retention pass asks "every active hold in this
    -- backup set" once per pass, and that read must be an index scan on
    -- this table rather than a join against the whole run history.
    set_uuid    TEXT NOT NULL,

    -- Why, and who. Both are required by the write path rather than by a
    -- CHECK here, so the refusal can say what a hold with neither costs:
    -- an operator finding it in six months cannot release it with any
    -- confidence, so it becomes permanent by accident. It is operator
    -- prose and it is redacted on the way in, exactly like
    -- state_transitions.detail and snapshot_runs.reason (issue #295).
    reason      TEXT NOT NULL DEFAULT '',
    placed_by   TEXT NOT NULL DEFAULT '',
    placed_at   TEXT NOT NULL,

    -- NULL means active. It is written once: a repeat release keeps the
    -- first instant, because that is when the protection actually ended
    -- and a retry that rewrote it would move the one timestamp an
    -- investigation reads.
    released_at TEXT,
    released_by TEXT NOT NULL DEFAULT ''
);

-- The retention pass's own read: every unreleased hold in one lineage,
-- once per pass. Partial on released_at because a deployment that has been
-- running for years is mostly released holds, and those are not a group
-- anybody looks up by lineage -- they are read per run, by the index
-- below, when somebody is investigating one snapshot.
CREATE INDEX idx_snapshot_holds_active
    ON snapshot_holds (set_uuid, placed_at) WHERE released_at IS NULL;

-- One snapshot's whole hold history, released rows included: the audit
-- read.
CREATE INDEX idx_snapshot_holds_run ON snapshot_holds (run_id, id);

-- 0013_workflow_lifecycle: the two durable records the five-stage
-- workflow lifecycle needs and 0012 does not have (EPIC L, issue #811).
--
-- WHY THE OBLIGATION IS A ROW AND NOT A DERIVATION OVER THE STEPS. This
-- is the whole of the crash-safety argument, so it is worth stating
-- before the table. A run that dies between its "before" stage and its
-- "after" stage leaves step rows that all read 'pending' -- which is
-- byte-for-byte what a run that died BEFORE its first "before" step
-- leaves behind. Those two situations are a machine that has been
-- quiesced and a machine that has not, and no query over the steps can
-- tell them apart, because the difference is not in the steps: it is in
-- whether this product had already decided to enter the scope.
--
-- So entering a scope is its own durable write, made before the first
-- side-effecting command in that scope, and it is what a restart reads.
-- The states are internal/workflow's ObligationState vocabulary, and the
-- one rule behind them is that every unsettled value can move to
-- 'recovery_required' and nothing moves out of a settled one.
--
-- WHY THERE IS NO CHECK CONSTRAINT ON scope OR state. 0012's own header
-- carries this argument in full and it has not changed: 0002 and 0006 are
-- what widening a CHECK costs in this schema (a new table, a copy of
-- every row, a drop, a rename, and the foreign-key cascade that broke
-- every populated journal in the field until migrate.go's
-- suspendForeignKeys landed). The vocabulary is enforced in Go, on the
-- write path, where widening it is a function and a test.
--
-- WHAT IS DELIBERATELY NOT IN THE OBLIGATION TABLE: any environment, any
-- secret location, any script. An obligation says a scope is owed its
-- cleanup; WHAT that cleanup runs is the run's plan (0012), read back
-- through internal/workflow's RecoverPlan, whose bytes are verified
-- against the sha256 the plan recorded. An obligation that carried its
-- own copy of what to run would be a second execution authority, and the
-- spool is the only one.

-- The bypass flag, which is a HISTORY field rather than a control.
--
-- #811 requires a run record to say whether the hooks were skipped, and
-- #811 also requires that skipping them can never clear an outstanding
-- recovery. Those two are only compatible if the flag is an INPUT to the
-- run: it is written once, by CommitWorkflowPlan, as part of the decision
-- that created the run, and there is no write anywhere in this journal
-- that turns it on afterwards. So a later release's --skip-workflow-scripts
-- (L6) can record that it bypassed the hooks and cannot use that record to
-- reach the recovery axis, because the axis is only settled by
-- ResolveWorkflowRecovery, which reads the obligations and not this
-- column.
ALTER TABLE workflow_runs ADD COLUMN bypassed INTEGER NOT NULL DEFAULT 0;

CREATE TABLE workflow_cleanup_obligations (
    id                 INTEGER PRIMARY KEY,

    -- A real reference, for the reason workflow_steps.run_id is one: an
    -- obligation whose run this journal does not have is a promise
    -- nothing can attribute, and the pass that reads these is the one
    -- running after a restart with no other memory.
    run_id             TEXT NOT NULL REFERENCES workflow_runs (run_id),

    -- Which of the two nested scopes: 'global' or 'set'. One row per
    -- scope per run, which is what UNIQUE below says, because a scope
    -- with two obligations is a scope with two answers to "is the
    -- cleanup owed".
    scope              TEXT NOT NULL,

    -- The backup set, denormalised from workflow_runs on purpose. The
    -- refusal this table drives is per SET -- a run requiring recovery
    -- blocks its set's next run -- and the query that answers "may this
    -- set run" is on the startup path of every daemon. It must not need
    -- a join to find out which set an outstanding promise is about.
    backup_set_id      TEXT NOT NULL,

    state              TEXT NOT NULL,

    -- When the scope was ENTERED: NULL only for a scope that never was.
    -- It is the timestamp an operator reads to answer "how long has this
    -- database been left quiesced", which is the first question a
    -- recovery raises and one no other column here can answer.
    entered_at         TEXT,

    -- The cleanup itself. NULL is "has not happened", never the zero
    -- time, for the reason every other nullable timestamp in this schema
    -- is nullable.
    started_at         TEXT,
    finished_at        TEXT,

    -- The audit record behind 'manually_acknowledged', which is the only
    -- exit from 'recovery_required' that is not a cleanup run.
    --
    -- All three are required for that state and refused for every other
    -- one, in Go, on the write path. An acknowledgement with no reason is
    -- a cleared alarm nobody can explain, and the reason this is a column
    -- rather than a log line is that the thing it explains is a durable
    -- refusal being lifted.
    --
    -- acknowledged_by is an actor name, not a credential. It is whatever
    -- the surface that took the acknowledgement knows about who asked.
    acknowledged_at    TEXT,
    acknowledged_by    TEXT NOT NULL DEFAULT '',
    acknowledge_reason TEXT NOT NULL DEFAULT '',

    UNIQUE (run_id, scope)
);

-- The refusal's own read: every scope anywhere that still needs a person,
-- by set. Partial, for the reason 0012's unsettled index is partial: a
-- deployment that has been running for years is overwhelmingly scopes
-- with nothing outstanding, and those are never looked up this way.
--
-- The predicate names the state rather than saying "not settled", so that
-- it keeps matching ObligationState.RequiresRecovery in Go as the
-- vocabulary grows; internal/state's
-- TestTheRecoveryIndexMatchesTheStateThatBlocksASet is what fails if the
-- two drift.
CREATE INDEX idx_workflow_obligations_recovery
    ON workflow_cleanup_obligations (backup_set_id, run_id)
    WHERE state = 'recovery_required';

-- One run's obligations, in a fixed order: the engine's and the recovery
-- pass's read.
CREATE INDEX idx_workflow_obligations_run ON workflow_cleanup_obligations (run_id, scope);

-- One run's captured hook output, as an ordered sequence a follower can
-- resume from.
--
-- WHY THE OUTPUT IS HERE AT ALL, given 0012's header says a hook's output
-- is not in the journal and only referenced from it. That paragraph was
-- about the STEP row, and it is still true of it: workflow_steps has
-- stdout_log_ref and stderr_log_ref and no payload, because a step row is
-- read by every history query and must not carry a megabyte. What #811
-- needs is the thing those references point AT, and it needs two
-- properties a file cannot give cheaply:
--
--   * a cursor. "What has happened since sequence 4197" is the question a
--     reconnecting Web or CLI follower asks, and answering it from a file
--     means every follower learning a byte offset into something that is
--     being appended to;
--   * a bound that is enforced without truncating a file somebody is
--     reading.
--
-- WHY THE PAYLOAD IS A BLOB AND NOT TEXT. A hook's output is not required
-- to be UTF-8: a tar progress line, a binary tool's stderr, a locale that
-- emits Latin-1. TEXT in SQLite means "valid UTF-8 by convention", and a
-- store that re-encoded or refused those bytes would be editing evidence.
--
-- WHAT HAS ALREADY HAPPENED TO THESE BYTES, and it is the one thing to
-- understand before reading them: they are REDACTED. A hook can print its
-- own credential -- `set -x` around a psql invocation is the ordinary way
-- it happens -- so every chunk passes obs.StreamFilter, which holds back
-- enough of the tail to catch a needle split across two reads, BEFORE any
-- of it reaches this table. That is why the guard that pins this schema's
-- columns (TestWorkflowSchemaHasNoColumnASecretCouldLiveIn) admits a
-- payload column at all: the column holds what a hook chose to print,
-- after this product has removed everything it knows to be secret. It is
-- not a column this product ever writes a resolved environment value into.
CREATE TABLE workflow_step_logs (
    id          INTEGER PRIMARY KEY,

    run_id      TEXT NOT NULL REFERENCES workflow_runs (run_id),

    -- The step these bytes came out of. Not a foreign key to
    -- workflow_steps: that table's identity is (run_id, step_id) with no
    -- unique constraint this could reference, and the run reference above
    -- is what keeps a record attributable.
    step_id     TEXT NOT NULL,

    -- The cursor, monotonic across the whole RUN and therefore also
    -- within a step, since steps execute serially in plan order. UNIQUE
    -- per run, because a follower's cursor is only a cursor if one number
    -- names one record. Numbered from 1: zero is what an uninitialised
    -- counter reads as, and "I have seen nothing" has to be spellable.
    seq         INTEGER NOT NULL,

    -- 'output' or 'truncated'. The truncation marker is IN the sequence
    -- rather than a flag on the step, because a bound reached mid-hook is
    -- a fact a follower has to see in the position it happened at -- a
    -- flag read at the end would put it after the missing bytes.
    kind        TEXT NOT NULL,

    -- 'stdout' or 'stderr'. The streams stay separate because merging
    -- them is irreversible; seq is what keeps "which came first"
    -- answerable anyway.
    stream      TEXT NOT NULL,

    -- When this product READ the bytes, which is not when the hook wrote
    -- them and does not claim to be. It is what makes a hook that went
    -- quiet for four minutes visible.
    captured_at TEXT NOT NULL,

    payload     BLOB NOT NULL,

    UNIQUE (run_id, seq)
);

-- The follower's read and the operator's read: one run's log from a
-- cursor, and one step's log within it.
CREATE INDEX idx_workflow_step_logs_step ON workflow_step_logs (run_id, step_id, seq);

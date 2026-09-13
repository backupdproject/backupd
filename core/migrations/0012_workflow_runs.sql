-- 0012_workflow_runs: the durable record of the scripts a backup cycle
-- decided to run, and of what happened to each one (EPIC L, issues #807
-- and #808).
--
-- WHY THIS EXISTS. A workflow run is not derived from anything. Retention
-- is a calculation over a calendar and a snapshot's phase is a projection
-- of its manifest, so both can be recomputed; a hook that ran is an event
-- in the world. It mounted something, it quiesced a database, it wrote a
-- file on a NAS. The only account of it is the one written down at the
-- time, and the run that has to read that account is often not this one:
-- a daemon that died mid-hook comes back with side effects applied and no
-- memory of applying them.
--
-- WHY THE PLAN IS A ROW SET AND NOT A BLOB. A serialized plan would be
-- simpler to write and useless to everything that has to read it: the
-- recovery pass asks "which step in this run was running when we died",
-- the spool retention pass asks "which runs are terminal with their
-- recovery settled", and the operator surfaces ask "what did last night's
-- hooks do". All three are queries over steps, and none of them is a
-- query a caller should answer by parsing a blob in application code.
--
-- WHY THERE IS NO CHECK CONSTRAINT ON ANY VOCABULARY COLUMN. state,
-- scope, phase, target, the three statuses and recovery_state are all
-- closed vocabularies, and every one of them is going to GROW as issues
-- #809 to #814 land. 0002 and 0006 are what widening a CHECK costs in
-- this schema: a new table, a copy of every row, a drop and a rename,
-- and -- because DROP TABLE runs an implicit DELETE FROM -- a foreign-key
-- cascade that broke every populated journal in the field until
-- migrate.go's suspendForeignKeys landed. So the vocabulary is enforced
-- in Go, on the write path, where widening it is a function and a test
-- rather than a table rebuild. internal/workflow's states.go is the
-- authority and says the same thing from the other side.
--
-- WHAT IS DELIBERATELY NOT HERE. There is no column for a hook's
-- environment, and that is a contract rather than an omission. An
-- environment variable can be secret-backed, secret-backed resolves to
-- material, and #808's rule is that no resolved value is ever persisted
-- anywhere -- not in the plan, not in the spool, not here. What is
-- durable is the variable's NAME and the LOCATION its value comes from,
-- both of which live in the config file, and both of which are covered by
-- resolved_plan_hash. A column that does not exist cannot be filled in by
-- accident, which is why the absence is pinned by
-- TestWorkflowSchemaHasNoColumnASecretCouldLiveIn rather than left to
-- reviewers.
--
-- There is also no column for a hook's OUTPUT, only references to it
-- (stdout_log_ref, stderr_log_ref). A script's output is unbounded by
-- construction -- a `set -x` in a loop is a megabyte a second -- and a
-- journal that grew with it would be a journal an incident fills up.

CREATE TABLE workflow_runs (
    id                 INTEGER PRIMARY KEY,

    -- run_id is the caller's own identifier and it is UNIQUE, which is
    -- what makes a re-commit of one run a refusal rather than a second
    -- plan for the same spool directory. It is also the spool's directory
    -- name, so internal/workflow holds it to the rules a path component
    -- has to obey.
    run_id             TEXT NOT NULL UNIQUE,

    -- The backup set, as its rendered model.BackupSetID ("source/set").
    -- A string rather than two columns because that is how this journal
    -- already carries a set identity everywhere else, and because
    -- model.ParseBackupSetID reads the rendering back unambiguously.
    backup_set_id      TEXT NOT NULL,

    state              TEXT NOT NULL,
    started_at         TEXT NOT NULL,

    -- NULL until the run reaches a terminal state. NULL is "still open",
    -- never "finished at the zero time": a recovery pass reads exactly
    -- this column to decide whether anybody is still waiting.
    finished_at        TEXT,

    -- The three statuses a hook is told about (BACKUPD_BACKUP_STATUS and
    -- friends). They default to 'unknown' rather than to '' because an
    -- unset variable and a variable saying "nobody knows yet" read
    -- identically in `test -z` and only one of them is true; a "before"
    -- hook legitimately sees 'unknown' for the backup that has not run.
    backup_status      TEXT NOT NULL DEFAULT 'unknown',
    cleanup_status     TEXT NOT NULL DEFAULT 'unknown',
    workflow_status    TEXT NOT NULL DEFAULT 'unknown',

    -- The second axis: whether an interruption in this run has been dealt
    -- with. Separate from state because a run can be terminal with a
    -- recovery still outstanding, and that pair is exactly the case the
    -- script spool must not be reclaimed under.
    recovery_state     TEXT NOT NULL DEFAULT 'none',

    -- The fingerprint of the decision this run executes, deterministic
    -- over the same workflow tree. It is what answers "did anything about
    -- what we run change since last night" without diffing a directory.
    resolved_plan_hash TEXT NOT NULL,

    -- The run-scoped directory holding the captured script bytes: the
    -- only place execution ever reads a script from.
    script_spool_ref   TEXT NOT NULL
);

-- One backup set's workflow history, newest last: the operator surface's
-- read and the retention pass's scan.
CREATE INDEX idx_workflow_runs_set ON workflow_runs (backup_set_id, started_at);

-- The recovery pass's own read: every run that is not settled. Partial,
-- because a deployment that has been running for years is overwhelmingly
-- runs with nothing outstanding, and those are never looked up this way --
-- they are read per set, by the index above.
--
-- The predicate names the two unsettled states rather than saying "not
-- none", so that it keeps matching RecoveryState.Settled in Go as the
-- vocabulary grows: a state added later is settled or unsettled by
-- somebody's decision, and <> 'none' would silently enrol it in this index
-- and in the recovery pass's scan. internal/workflow's
-- TestRecoveryIndexMatchesTheUnsettledStates is what fails if the two
-- drift.
CREATE INDEX idx_workflow_runs_unsettled
    ON workflow_runs (recovery_state, started_at)
    WHERE recovery_state IN ('required', 'in_progress');

CREATE TABLE workflow_steps (
    id                       INTEGER PRIMARY KEY,

    -- A real reference, unlike snapshot_runs.operation_id: a step whose
    -- run this journal does not have is a step nothing can attribute, and
    -- a recovery pass finding one would have a side effect it cannot
    -- place. The write path commits both in one transaction so the
    -- constraint is defence in depth rather than the mechanism.
    run_id                   TEXT NOT NULL REFERENCES workflow_runs (run_id),

    -- step_id is derived by internal/workflow from the order, scope,
    -- phase and script name, and it is the spooled file's name. UNIQUE
    -- per run, because two steps sharing one id would share one spool
    -- file.
    step_id                  TEXT NOT NULL,

    -- step_order, not "order": ORDER is a SQL keyword and a column that
    -- has to be quoted in every statement is a column somebody
    -- eventually does not quote. It is UNIQUE per run because the order
    -- IS the plan: two steps claiming one position is a plan with no
    -- defined execution sequence.
    step_order               INTEGER NOT NULL,

    scope                    TEXT NOT NULL,
    phase                    TEXT NOT NULL,

    -- The script as it was at snapshot time: its name, the sha256 of the
    -- bytes that were captured, and how many there were. Together with
    -- spool_ref they are what lets an operator prove that what ran is
    -- what passed validation.
    script_name              TEXT NOT NULL,
    script_sha256            TEXT NOT NULL,
    script_size              INTEGER NOT NULL,

    -- Where the script runs, read off its own basename and never
    -- configured: see internal/workflow's Target for why that is the most
    -- important decision in the feature.
    target                   TEXT NOT NULL,

    -- The connection a remote step runs over. '' for a local step, and
    -- the Go write path refuses the two mismatched combinations.
    execution_connection_ref TEXT NOT NULL DEFAULT '',

    state                    TEXT NOT NULL,

    -- Resolved at snapshot time, so a config edit mid-run cannot change
    -- the bound a running step is held to.
    --
    -- NANOSECONDS, which is what time.Duration is, rather than the
    -- seconds this column first held. Seconds looked friendlier in the
    -- sqlite3 CLI and silently destroyed information: 500ms became 0, and
    -- a step recovered from that row would be a step with no bound at all
    -- -- or, once Step.Validate refuses a zero timeout, a run that cannot
    -- be recovered. The configuration accepts any Go duration, so the
    -- column stores any Go duration.
    timeout_nanos            INTEGER NOT NULL,

    started_at               TEXT,
    finished_at              TEXT,

    -- NULL unless a process actually exited and this product observed the
    -- status. A killed step, an interrupted step and a step that never
    -- started all leave it NULL; NULL and 0 are emphatically not the same
    -- answer, which is why there is no DEFAULT here.
    exit_code                INTEGER,

    -- Whether this product PROVED the process it killed is gone, rather
    -- than having sent a signal and moved on. A script still running
    -- after this product stopped waiting for it is the worst case a
    -- workflow has: the backup proceeds while a hook is still touching
    -- the thing it was meant to quiesce.
    termination_confirmed    INTEGER NOT NULL DEFAULT 0,

    -- References to captured output, never the output itself: see this
    -- file's header.
    stdout_log_ref           TEXT NOT NULL DEFAULT '',
    stderr_log_ref           TEXT NOT NULL DEFAULT '',

    -- The captured copy of the script. The only path execution opens.
    spool_ref                TEXT NOT NULL,

    UNIQUE (run_id, step_id),
    UNIQUE (run_id, step_order)
);

-- One run's plan, in execution order: the read every consumer makes.
CREATE INDEX idx_workflow_steps_run ON workflow_steps (run_id, step_order);

-- The run's UNRESOLVED environment: one row per variable, holding the
-- literal an operator wrote or the LOCATION a secret comes from.
--
-- WHY THIS EXISTS AT ALL, given the header above says there is no column
-- for a hook's environment. That paragraph was half right and the half it
-- got wrong made the feature unrecoverable. It is true that no RESOLVED
-- value may be persisted: a secret's material is read at execution time,
-- used, and dropped. It does not follow that the environment itself is
-- derivable, and the recovery case is where that breaks:
--
--   a run is interrupted, the daemon restarts, an operator edits (or
--   deletes) the workflow environment while it is down, and the recovery
--   pass then has to finish a run that was planned with the OLD
--   environment. resolved_plan_hash covers the environment, which proves
--   it changed and cannot reconstruct it -- a hash is one-way, which is
--   the property it was chosen for.
--
-- So the plan's environment is durable for exactly the reason the plan's
-- scripts are: what a run executes is decided once, and a config edit
-- mid-run must not change it. The columns hold what the config file holds
-- and nothing more.
--
-- WHAT MAY GO IN THESE COLUMNS. literal_value is a literal an operator
-- typed into config.yaml, which is a file on this host in the clear;
-- persisting it exposes nothing the config file did not. The three secret_
-- columns are a LOCATION: a path, a variable name, an argv. They are what
-- secretref.Ref carries and what secretref documents as safe to log.
--
-- WHAT MUST NEVER GO IN THEM is the value a location resolves to.
-- TestWorkflowSchemaHasNoColumnASecretCouldLiveIn pins the column set, and
-- TestNoResolvedSecretReachesAnyPersistedByte resolves a real secret and
-- searches the database file for it.
CREATE TABLE workflow_run_env (
    id             INTEGER PRIMARY KEY,

    run_id         TEXT NOT NULL REFERENCES workflow_runs (run_id),

    -- The variable's place in the plan's own ordering (by name). It is
    -- stored rather than re-derived so that a recovered environment is
    -- byte-identical to the planned one, including its order, which the
    -- plan hash covers.
    position       INTEGER NOT NULL,

    name           TEXT NOT NULL,

    -- NULL when this variable is secret-backed. NULL rather than '',
    -- because '' is a literal an operator can legitimately configure and
    -- a column that could not tell the two apart would turn an empty
    -- literal into a secret with no source.
    literal_value  TEXT,

    -- WHERE a secret-backed value comes from: exactly one of these is
    -- set, and all three are empty for a literal. secret_command is the
    -- argv as a JSON array, because an argument may contain any byte and
    -- a separator-joined form would be one two different argvs could
    -- share.
    secret_file    TEXT NOT NULL DEFAULT '',
    secret_env     TEXT NOT NULL DEFAULT '',
    secret_command TEXT NOT NULL DEFAULT '',

    UNIQUE (run_id, name),
    UNIQUE (run_id, position)
);

-- One run's environment, in plan order: the recovery pass's read.
CREATE INDEX idx_workflow_run_env_run ON workflow_run_env (run_id, position);

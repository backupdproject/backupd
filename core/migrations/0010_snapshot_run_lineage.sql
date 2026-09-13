-- 0010_snapshot_run_lineage: key a snapshot lineage to the backup set's
-- DURABLE identifier, and record the two verdicts 0009 measured and threw
-- away.
--
-- WHY THE LINEAGE KEY CHANGES. 0009 keys everything a snapshot run is
-- looked up by on (source, backup_set): the two halves of
-- model.BackupSetID, which are the NAMES an operator writes in
-- configuration and can edit at any time. Every question this table
-- exists to answer then has a name in the middle of it, and a rename
-- silently changes the answer:
--
--   * The partial unique index that guarantees ONE last-known-good run
--     per set is per name pair. Rename the set and the old holder keeps
--     its flag while the next success sets a second one, so "the" restore
--     point becomes two rows and which one a caller gets depends on the
--     query plan -- the exact failure that index was written to make
--     impossible.
--
--   * Per-set history is per name pair. Rename the set and its whole
--     snapshot history disappears from the listing, from the crash
--     reconciler's view of what it may compare against the repository,
--     and from the guard that refuses to create a second repository over
--     a set that already has one.
--
-- The backup set's uuid is the value that does not move: internal/config
-- requires one on every incremental set for precisely this reason (see
-- config.BackupSet.UUID), it is already what model.NewSourceIdentity
-- digests, and nothing an operator edits in a name can change it. So
-- set_uuid becomes the lineage key here, and source/backup_set stay as
-- what they always were in this table -- display metadata, the names to
-- render beside the rows.
--
-- WHY THE BACKFILL IS THE NAME PAIR. A row written before this column
-- existed carries no uuid and this schema cannot invent one: the uuid
-- lives in the operator's configuration, which the database cannot see.
-- What it CAN do is keep those rows' existing lineage intact, which is
-- exactly what "source/backup_set" is -- the key they were already
-- grouped by. A legacy row therefore stays in one lineage with its
-- siblings, the unique last-known-good index can be created without
-- colliding two old sets into one key, and the first run after this
-- migration writes the real uuid. The two value spaces cannot collide: a
-- uuid contains no '/' and this backfill always does.
--
-- WHY source_complete. A well-formed snapshot of a tree with a hole in it
-- is still a well-formed snapshot. 0009 recorded the manifest before the
-- run had checked whether the source pass covered everything, so a
-- process that died in that window left a row that reconciliation could
-- verify and promote to SUCCESS -- advertising, as a restore point, a
-- snapshot known to omit source data. The source side's own verdict is
-- therefore written durably in the same statement that records the
-- manifest, and reconciliation refuses to promote a row that does not
-- carry it. NULL is a third value and it is the honest one: nobody
-- recorded a verdict for this row (it died before the manifest was
-- recorded, or its manifest was adopted from the repository by crash
-- reconciliation), which is not the same claim as "the pass was
-- incomplete" and is equally not a restore point.
--
-- WHY entries_scanned. The source side counts every entry a pass
-- considered, of every kind, including the ones it deliberately skipped.
-- 0009 had nowhere to put that number, so the reports above this table
-- reconstructed it as files + directories, which silently reads as zero
-- for an adopted snapshot and understates every pass that skipped
-- anything. It is nullable for the reason every counter in 0009 is: a
-- pass can genuinely consider zero entries, so zero must not double as
-- "nobody measured this".

ALTER TABLE snapshot_runs ADD COLUMN set_uuid TEXT NOT NULL DEFAULT '';

ALTER TABLE snapshot_runs ADD COLUMN source_complete INTEGER
    CHECK (source_complete IN (0, 1));

ALTER TABLE snapshot_runs ADD COLUMN entries_scanned INTEGER;

UPDATE snapshot_runs SET set_uuid = source || '/' || backup_set WHERE set_uuid = '';

-- At most one last-known-good run per LINEAGE, which is what the 0009
-- index meant to say. Dropped and recreated rather than added beside the
-- old one: two indexes each enforcing "one holder" under a different
-- definition of "one set" would both be satisfiable at once by exactly
-- the rename this migration exists to survive.
DROP INDEX idx_snapshot_runs_last_known_good;
CREATE UNIQUE INDEX idx_snapshot_runs_last_known_good
    ON snapshot_runs (set_uuid) WHERE last_known_good = 1;

-- Per-lineage history, newest first: an operator's listing of a set's
-- snapshots, the crash reconciler's window onto what it may compare
-- against the repository, and the guard that refuses to create a second
-- repository over a set that already has one.
DROP INDEX idx_snapshot_runs_set_started;
CREATE INDEX idx_snapshot_runs_lineage_started ON snapshot_runs (set_uuid, started_at);

-- "Does this repository domain hold any snapshot of ours at all", and
-- "which manifests in it are ours", are both answered by the existing
-- partial unique index on (domain, snapshot_id) WHERE snapshot_id <> '':
-- domain leads it, so an EXISTS over a domain and a bulk read of that
-- domain's manifest ids are both index scans and neither needs a new
-- index here. They are named in this comment because the read they
-- replaced -- one lookup per repository snapshot, over a bounded window
-- of one set's newest rows -- looked like it needed one.

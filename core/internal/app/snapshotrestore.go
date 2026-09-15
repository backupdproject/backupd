package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/retnd/retnd/core/internal/backupengine"
	"github.com/retnd/retnd/core/internal/config"
	"github.com/retnd/retnd/core/internal/model"
	"github.com/retnd/retnd/core/internal/snapshotlifecycle"
	"github.com/retnd/retnd/core/internal/state"
)

// This file is the engine half of a snapshot restore (#787): an operator
// names a backup set, a restore point and a place on this machine, and
// gets the tree, the directory or the file back.
//
// # Why it lives here rather than in core/service
//
// Because everything it needs to resolve the request is already here.
// Which repository a set's snapshots live in is derived from the
// configuration this Service is serving (repositoryLocation), which
// restore point is the good one is a catalog read this Service's journal
// answers, and the engine boundary is this Service's own field. A
// restore assembled one layer up would have to re-derive all three from
// outside, which is how two answers to "where is this set's repository"
// come to exist.
//
// # What it deliberately does not do
//
// It does not create a repository. openRepository (snapshotcycle.go)
// does, because a BACKUP of a set that has never run has to put its first
// snapshot somewhere; a RESTORE from a repository that is not there is a
// missing repository, and creating an empty one would turn "your backups
// are unreachable, check the mount" into "there is nothing to restore",
// which is the same sentence an operator reads as "the backups are gone".
//
// It does not restore to a remote either. That is deliberate and it is
// stated in backupengine.RestoreRequest: streaming a restore back to the
// endpoint the data came from is separate work with its own partial-write
// story, and this delivers the local case that every operator needs
// first.

// ErrBackupSetNotConfigured is returned when a restore names a backup set
// this configuration does not hold.
//
// Its own sentinel because the caller's remedy is specific: the set was
// renamed, removed, or the request came off a stale screen, and none of
// those is a reason to go looking at the repository.
var ErrBackupSetNotConfigured = errors.New("app: no backup set of that name is configured")

// ErrSetHasNoSnapshots is returned when a restore was asked for without
// naming a snapshot and the set has no last-known-good restore point.
//
// It is not an error about the repository. A set whose runs have all
// failed, or that has never run, genuinely has nothing to restore, and
// saying so is a different message from "the repository is unreachable".
var ErrSetHasNoSnapshots = errors.New("app: this backup set has no restore point to restore from")

// ErrNotAnIncrementalSet is returned when a snapshot restore names a set
// that does not use the incremental engine.
//
// An artifact set's restore is a different act against a different store
// (internal/archive, service.SubmitRestorePlacement), and answering this
// request with that one would hand somebody a storage-provider retrieval
// when they asked for a file out of a snapshot.
var ErrNotAnIncrementalSet = errors.New("app: this backup set does not store snapshots")

// SnapshotRestoreRequest is one restore of one restore point to one local
// directory.
type SnapshotRestoreRequest struct {
	// SourceName and SetName address the backup set, spelled the way
	// every surface in this product spells one.
	SourceName string
	SetName    string

	// SnapshotID names the restore point. Empty means the set's
	// last-known-good snapshot, which is the answer to "just give me the
	// newest one that is known to be good" and is a catalog fact rather
	// than a guess about recency.
	SnapshotID string

	// SourcePath selects what inside the snapshot to restore, as a
	// slash-separated path. Empty means the whole snapshot.
	SourcePath string

	// TargetPath is the local directory to restore into.
	TargetPath string

	// Conflict is what happens when the destination already holds
	// something. The zero value refuses.
	Conflict backupengine.RestoreConflict

	// PreserveOwners asks for the snapshot's uid/gid to be restored,
	// which only a privileged process can do. The default is not to,
	// because a restore that failed on ownership an operator never asked
	// about is a restore that did not happen.
	PreserveOwners bool

	// Progress, when set, is called once per restored entry.
	Progress func(backupengine.RestoreProgress)
}

// SnapshotRestoreResult is what a restore did, plus which restore point
// it actually used.
//
// SnapshotID is reported back even when the caller named it, because the
// interesting case is the caller who did not: "the last known good one"
// is a question answered at the moment of the restore, and the answer is
// what an operator has to be able to write down afterwards.
type SnapshotRestoreResult struct {
	SnapshotID string
	Report     backupengine.RestoreReport
}

// RestoreSnapshot writes a stored snapshot, or one directory or file
// inside it, to a local directory.
//
// The repository is opened for this restore and closed on the way out.
// Content verification is always on: this path is reached from a durable
// operation whose completion is recorded and read later as evidence, and
// a restore's own statistics are exactly what a broken restore path
// reports correctly.
func (s *Service) RestoreSnapshot(ctx context.Context, req SnapshotRestoreRequest) (result SnapshotRestoreResult, err error) {
	bs, err := s.incrementalBackupSet(req.SourceName, req.SetName)
	if err != nil {
		return SnapshotRestoreResult{}, err
	}

	catalog, ok := s.Journal.(snapshotlifecycle.Catalog)
	if !ok {
		return SnapshotRestoreResult{}, ErrNoSnapshotCatalog
	}

	if s.Repositories == nil {
		return SnapshotRestoreResult{}, ErrNoIncrementalEngine
	}

	snapshotID, err := s.restorePoint(ctx, catalog, bs, req.SnapshotID)
	if err != nil {
		return SnapshotRestoreResult{}, err
	}

	loc, err := s.repositoryLocation(bs)
	if err != nil {
		return SnapshotRestoreResult{}, err
	}

	repo, err := s.Repositories.OpenRepository(ctx, loc)
	if err != nil {
		return SnapshotRestoreResult{}, fmt.Errorf("opening repository %s: %w", loc.Domain, err)
	}

	result.SnapshotID = snapshotID

	// Named returns, because this is the one error a restore can hit
	// after its work is finished: a repository that would not close has
	// not necessarily written everything it was holding, and swallowing
	// that would report a restore as complete on the strength of a
	// handle that failed to let go.
	defer func() {
		if cerr := repo.Close(ctx); cerr != nil && err == nil {
			err = fmt.Errorf("closing repository %s: %w", loc.Domain, cerr)
		}
	}()

	result.Report, err = repo.Restore(ctx, backupengine.SnapshotID(snapshotID), backupengine.RestoreRequest{
		SourcePath:    req.SourcePath,
		TargetPath:    req.TargetPath,
		Conflict:      req.Conflict,
		SkipOwners:    !req.PreserveOwners,
		VerifyContent: true,
		Progress:      req.Progress,
	})
	if err != nil {
		return result, err
	}

	return result, nil
}

// incrementalBackupSet finds the configured set a restore names, and
// refuses one that does not store snapshots.
//
// It is also where EPIC K's production gate (#789) is enforced for every
// per-set incremental surface, because it is the one door all of them go
// through: snapshotSurface calls it, and so does every caller of that.
// The gate is checked BEFORE the set is looked up, deliberately -- with
// the engine disabled the whole surface is unavailable, and answering
// "no such backup set" to a request about a set that is right there in
// the configuration would send an operator looking for the wrong
// mistake. See incrementalgate.go.
func (s *Service) incrementalBackupSet(sourceName, setName string) (config.BackupSet, error) {
	if err := s.incrementalEngineGate(); err != nil {
		return config.BackupSet{}, err
	}

	if s.Config == nil {
		return config.BackupSet{}, ErrBackupSetNotConfigured
	}

	for _, src := range s.Config.Sources {
		if src.Name != sourceName {
			continue
		}

		for _, bs := range src.BackupSets {
			if bs.Name != setName {
				continue
			}

			if bs.Engine != model.EngineKopia {
				return config.BackupSet{}, fmt.Errorf("%w: %s/%s stores artifacts", ErrNotAnIncrementalSet, sourceName, setName)
			}

			return bs, nil
		}
	}

	return config.BackupSet{}, fmt.Errorf("%w: %s/%s", ErrBackupSetNotConfigured, sourceName, setName)
}

// restorePoint resolves which snapshot a restore reads from.
//
// A named snapshot is taken as given and is NOT checked against the
// catalog first. That is deliberate: the catalog is this deployment's
// account of its own runs, and a restore point that the repository holds
// and the catalog has forgotten -- a journal restored from an older
// backup, a row a migration lost -- is exactly the situation somebody is
// restoring in. The repository decides whether the id names anything, and
// says ErrSnapshotNotFound when it does not.
func (s *Service) restorePoint(
	ctx context.Context,
	catalog snapshotlifecycle.Catalog,
	bs config.BackupSet,
	requested string,
) (string, error) {
	if requested != "" {
		return requested, nil
	}

	run, err := catalog.LastKnownGoodSnapshot(ctx, snapshotLineage(bs))
	if err != nil {
		if errors.Is(err, state.ErrSnapshotRunNotFound) {
			return "", fmt.Errorf("%w: %s", ErrSetHasNoSnapshots, bs.ID)
		}

		return "", fmt.Errorf("reading the last known good snapshot of %s: %w", bs.ID, err)
	}

	if run.SnapshotID == "" {
		return "", fmt.Errorf("%w: %s", ErrSetHasNoSnapshots, bs.ID)
	}

	return run.SnapshotID, nil
}

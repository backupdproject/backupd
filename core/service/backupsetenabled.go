package service

import (
	"context"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/retnd/retnd/core/internal/config"
)

// This file is the three per-set switches an operator flips from a
// screen: run this set or leave it alone, delete its remote sources or
// never touch them, and does the thing still answer.
//
// The two toggles look symmetric and are not, in the same direction.
// Turning a set off stops new restore points being made and touches
// nothing already made; turning read-only off stops future artifacts
// being retained and reaches back to authorise nothing. Both are
// deliberately one-way about artifacts that already exist, which is the
// shape #227's reinstatement established for this codebase: a change of
// mind may alter what happens next, never what was already promised about
// a copy somebody else's system is still holding.
//
// That asymmetry is why neither is a destructive operation in §50's
// terms, despite one of them being spelled "read-only" and the other
// stopping backups. Neither can delete anything, and the cost of the
// scary-sounding one, a set going stale because nobody turned it back on,
// is reported by FR-24 rather than hidden.
//
// Every write here runs the same sequence, which is written out once in
// SetBackupSetEnabled and referred to from the others rather than
// re-argued: re-read the file from disk, edit, encode BEFORE validation
// resolves defaults in place, then persist and adopt. The encode ordering
// is the subtle one. Validate fills in this release's defaults, so
// encoding after it would freeze today's values into an operator's file
// as though they had chosen them, and a toggle of one set would silently
// pin the defaults of every other.

// connectionTestTimeout bounds one reachability check. It is the same
// ten seconds TestConnection uses for a candidate source: a test that can
// hang indefinitely is a request an operator cannot cancel.
const connectionTestTimeout = 10 * time.Second

// SetBackupSetEnabled turns one configured backup set on or off, persists
// the change to the configuration file this BackupService was opened from,
// and hot-reloads so it takes effect immediately.
//
// # What "disabled" means, and what it does not
//
// A disabled backup set is excluded from every run cycle: nothing is
// discovered, transferred, verified, committed or retained for it while it
// stays off. Nothing already backed up is touched. Turning a set off
// deletes no artifact, releases no remote source, and does not run
// retention; turning it back on resumes the ordinary pipeline from
// whatever the journal already holds. That is why this is a
// state-changing but NON-destructive operation in
// docs/EPIC-B-multi-nas.md §50's terms, in the same bucket as
// create-backup-set, and why the API layer wraps it in CSRF protection but
// not the destructive-operations gate.
//
// It is worth being explicit about the direction that sounds dangerous:
// turning a set OFF stops new restore points being made, which degrades
// freshness over time, and FR-24's health computation reports that
// honestly as the set goes stale. It is not hidden, and it is reversible
// by the same call.
//
// # Persist, then reload
//
// This follows CreateBackupSet's sequence exactly, for the same reasons
// recorded there: re-read the file fresh rather than trusting the running
// in-memory copy, encode the bytes BEFORE config.Validate resolves
// defaults in place (so an unrelated toggle does not freeze this
// release's defaults into the operator's file), resolve the validator
// catalog before the write so the only step after it cannot fail, then
// one atomic state.Store so no concurrent reader ever sees a torn
// {inner, revision} pair.
func (b *BackupService) SetBackupSetEnabled(_ context.Context, id string, enabled bool) (BackupSet, error) {
	if b.configPath == "" {
		return BackupSet{}, ErrConfigNotFileBacked
	}
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return BackupSet{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	cfg, err := config.Load(b.configPath)
	if err != nil {
		return BackupSet{}, fmt.Errorf("service: re-reading configuration: %w", err)
	}

	found := false
	for i := range cfg.Sources {
		if cfg.Sources[i].Name != sourceName {
			continue
		}
		for j := range cfg.Sources[i].BackupSets {
			if cfg.Sources[i].BackupSets[j].Name != setName {
				continue
			}
			cfg.Sources[i].BackupSets[j].Disabled = !enabled
			found = true
		}
	}
	if !found {
		return BackupSet{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	// Encoded before Validate, which resolves defaults in place; see
	// UpdateSettings' own comment for the full reasoning and for what an
	// unrelated edit would otherwise silently freeze into the file.
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		return BackupSet{}, fmt.Errorf("service: encoding configuration: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return BackupSet{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	applyValidators, err := planValidatorCatalog(cfg)
	if err != nil {
		return BackupSet{}, err
	}

	if err := writeConfigBytesAtomically(b.configPath, encoded); err != nil {
		return BackupSet{}, fmt.Errorf("service: persisting configuration: %w", err)
	}

	applyValidators()

	b.adoptConfig(cfg)

	return toServiceBackupSet(cfg, b.configPath, sourceName, findBackupSet(cfg, sourceName, setName)), nil
}

// SetBackupSetReadOnly turns issue #282's read-only declaration on or off
// for one already-persisted backup set, through the API/wizard rather
// than by hand-editing config.yaml (issue #316). It persists the change
// to the configuration file this BackupService was opened from and
// hot-reloads, following exactly the sequence SetBackupSetEnabled above
// documents in full and for the same reasons.
//
// # What this always sets, and what it never touches
//
// This always writes this ONE backup set's own explicit override
// (config.BackupSet.ReadOnlyConfig), never its source's ReadOnly default:
// an API caller names one backup set, and there is no "every set under
// this source" concept anywhere in the CRUD surface for it to mean
// instead. A source-level default set by hand in config.yaml is left
// exactly as it is; this only ever adds or changes THIS set's own
// override on top of it, the same way a hand-edited per-set `read_only:`
// line would.
//
// # What "read-only" means, and what turning it off does not do
//
// See config.BackupSet.ReadOnly's own doc for the full contract: while
// true, FR-15's delete step is structurally never reached for this set
// (core/internal/app's pipeline routes it to lifecycle.RetainRemote
// instead of lifecycle.DeleteRemote), and an artifact that already
// reached REMOTE_RETAINED under it stays retained — turning this back off
// does not reach back and make an already-retained artifact eligible for
// deletion again, the same one-way-per-artifact shape #227's
// reinstatement already established for this codebase. It only changes
// what happens to artifacts THIS backup set commits from here on.
//
// # Turning it OFF has to be earned (issue #852)
//
// Read-only OFF means "delete from the source after backup", and backupd
// may only promise that when the source's own credentials can actually do
// it. So this call runs the connection test's write probe first and
// refuses with ErrSourceNotWritable when that source is proven
// non-writable, which is the same refusal a create or an edit asking for
// the same thing gets. Read-only ON is never gated: making a set safer
// asks no permission of anybody.
func (b *BackupService) SetBackupSetReadOnly(ctx context.Context, id string, readOnly bool) (BackupSet, error) {
	if b.configPath == "" {
		return BackupSet{}, ErrConfigNotFileBacked
	}
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return BackupSet{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	// Issue #852: turning read-only OFF is asking this manager to delete
	// from somebody else's machine, so it is the one direction that has
	// to be earned. The connection check proves whether these
	// credentials may write there, and a set whose source refused the
	// write probe is refused here with ErrSourceNotWritable — the same
	// refusal, from the same function, that the create and edit paths
	// make, so the `backup-set read-only <set> off` verb and a wizard
	// save cannot disagree about what this source can do.
	//
	// Turning read-only ON proves nothing and asks nothing: a set that
	// never deletes needs no permission to delete, and a source that has
	// gone unreachable must not block an operator from making a set
	// SAFER.
	//
	// It runs BEFORE configMu is taken, exactly like CreateBackupSet's
	// check and unlike UpdateBackupSet's, because nothing about it
	// depends on the configuration the lock protects: it reads the
	// persisted set through the same snapshot TestBackupSetConnection
	// does. Holding a process-wide configuration lock across network I/O
	// is what PR #628's review found on the edit path, and there is no
	// reason to repeat it where it is avoidable.
	if !readOnly {
		result, err := b.TestBackupSetConnection(ctx, id)
		if err != nil {
			return BackupSet{}, err
		}
		if !result.OK {
			return BackupSet{}, fmt.Errorf("%w: %s", ErrConnectionNotProven, result.Message)
		}
		if err := refuseDeleteOnUnwritableSource(result, false); err != nil {
			return BackupSet{}, err
		}
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	cfg, err := config.Load(b.configPath)
	if err != nil {
		return BackupSet{}, fmt.Errorf("service: re-reading configuration: %w", err)
	}

	found := false
	for i := range cfg.Sources {
		if cfg.Sources[i].Name != sourceName {
			continue
		}
		for j := range cfg.Sources[i].BackupSets {
			if cfg.Sources[i].BackupSets[j].Name != setName {
				continue
			}
			// A pointer to a fresh local, per iteration: reusing one
			// variable's address across the loop (or across calls) would
			// have every backup set's ReadOnlyConfig alias the same bool,
			// silently rewriting an earlier match's answer whenever a
			// later one is set.
			ro := readOnly
			cfg.Sources[i].BackupSets[j].ReadOnlyConfig = &ro
			found = true
		}
	}
	if !found {
		return BackupSet{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	// Encoded before Validate, which resolves defaults in place; see
	// SetBackupSetEnabled's own comment above for the full reasoning.
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		return BackupSet{}, fmt.Errorf("service: encoding configuration: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return BackupSet{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	applyValidators, err := planValidatorCatalog(cfg)
	if err != nil {
		return BackupSet{}, err
	}

	if err := writeConfigBytesAtomically(b.configPath, encoded); err != nil {
		return BackupSet{}, fmt.Errorf("service: persisting configuration: %w", err)
	}

	applyValidators()

	b.adoptConfig(cfg)

	return toServiceBackupSet(cfg, b.configPath, sourceName, findBackupSet(cfg, sourceName, setName)), nil
}

// TestBackupSetConnection runs the same non-destructive reachability and
// authentication check TestConnection performs, against an ALREADY
// PERSISTED backup set rather than a candidate a caller is still filling
// in.
//
// The two share one route (POST /api/v1/backup-sets/test-connection) and
// differ only in where the connection details come from: a candidate
// carries its own, and a persisted set has them in the configuration
// already. That matters for more than tidiness. A client asking to test
// set "nas-a/photos" does not know, and must never have to send back, the
// key reference and known-hosts line that set is configured with; making
// it echo them would turn a read-only "does this still work" button into
// a request that could quietly test something else.
//
// The check itself is read-only: it lists the configured remote path over
// the transport this service already uses and discards the result.
// Nothing is written remotely, and no trust decision is made or revised.
//
// What it CAN write, since issue #624, is this deployment's own
// config.yaml, and that is worth stating precisely rather than leaving
// under "read-only" (PR #628 review). A check that passes against a set
// marked ConnectionUnverified takes the mark off, through
// clearConnectionUnverified, which is the same path every configuration
// write in this package takes: under configMu, re-reading the file from
// disk, encoding, validating, writing atomically and hot-reloading through
// adoptConfig. Two things follow for a control shaped like a read. A set
// that carries no mark still writes nothing, so the ordinary "does this
// still work" press is a read. And on a marked set, a passing press
// hot-reloads whatever hand edit was made to config.yaml since this
// process last loaded it, exactly as an edit to any other set would; that
// is the house rule for every configuration write here rather than
// something this button chose. backupsetverified.go's own doc carries the
// argument for why that one transition is a write a check may make, and
// why a failing check makes none.
func (b *BackupService) TestBackupSetConnection(ctx context.Context, id string) (ConnectionTestResult, error) {
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return ConnectionTestResult{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	st := b.state.Load()
	var found *config.BackupSet
	for _, src := range st.inner.Config.Sources {
		if src.Name != sourceName {
			continue
		}
		for i := range src.BackupSets {
			if src.BackupSets[i].Name == setName {
				found = &src.BackupSets[i]
			}
		}
	}
	if found == nil {
		return ConnectionTestResult{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	testCtx, cancel := context.WithTimeout(ctx, connectionTestTimeout)
	defer cancel()

	// Built by connectionSourceFor rather than here, so this button and
	// the check UpdateBackupSet runs in front of a connection-changing
	// edit are asking about the same remote in the same words (#624).
	// Two constructions of one transport.Source is how a check quietly
	// starts proving a slightly different connection from the one that
	// runs.
	src := connectionSourceFor(*found, st.inner.Config.KeyEncryption)

	// Issue #596: six named steps rather than one boolean. The error is
	// still never returned to the caller, for exactly the reason
	// TestConnection gives (err's own text can embed transport internals
	// a caller must not have to treat as safe to render); what changed is
	// that DNS, the connect, the host key, the credential, the
	// authentication and the listing are asked and answered separately,
	// so a typo'd hostname, an unauthorised key, a rotated host key and a
	// missing path stop reading identically. See connectiontest.go.
	result := b.runConnectionTest(testCtx, id, found, src)

	// Issue #624: a check that PASSES against a set created without one
	// takes the mark off. It is the transition and nothing else, so a set
	// that carries no mark still reads no files and writes none here, and
	// a check that failed leaves the mark exactly where it was, because
	// "somebody pressed the button" is not the claim the mark makes.
	//
	// The ctx passed on is the caller's rather than testCtx: the ten
	// seconds above bound the reachability check, and cancelling a
	// configuration write halfway through because a network timeout
	// elapsed would be the wrong deadline on the wrong operation.
	if result.OK {
		b.clearConnectionUnverified(ctx, id)
	}
	return result, nil
}

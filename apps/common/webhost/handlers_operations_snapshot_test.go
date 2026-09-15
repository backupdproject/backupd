package webhost

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/retnd/retnd/core/service"
)

// EPIC K's four mutating acts over real HTTP (#788).
//
// Everything here goes through the router rather than into the handler,
// because what is under test is the part only the router has: the CSRF
// pair and the destructive gate in front of the route, the Idempotency-Key
// HEADER becoming a field of the service request, and the JSON body
// arriving as the one parameter object its action owns.
//
// The idempotency key is the assertion worth stating plainly. These four
// are retried by clients and by people: a hold taken twice is two holds
// on one snapshot, and a verify submitted twice is a second full read of
// a repository. The key is what makes a retry the same act, and a handler
// that dropped it would answer 202 to every retry and look correct on
// every screen.

// snapshotAction is one of the four, with a body that names it and the
// assertion that its parameters crossed the boundary intact.
type snapshotAction struct {
	name string
	// body is the whole submission, parameters included.
	body string
	// carried reports what the fake was handed, so each row can check
	// the fields its own action owns.
	carried func(*snapshotFixture) (backupSetID, key string, extra map[string]string)
	// missing is the same submission with its parameter object left out,
	// which every one of the four has to refuse.
	missing string
}

var snapshotActions = []snapshotAction{
	{
		name: "restore_snapshot",
		body: `{"action":"restore_snapshot","config_revision":"rev-1","snapshot_restore":` +
			`{"backup_set_id":"production/uploads-tree","snapshot_id":"snap_7","source_path":"/srv/uploads/a","target_path":"/restore/here","conflict":"overwrite"}}`,
		carried: func(fx *snapshotFixture) (string, string, map[string]string) {
			return fx.lastRestore.BackupSetID, fx.lastRestore.IdempotencyKey, map[string]string{
				"snapshot_id": fx.lastRestore.SnapshotID,
				"source_path": fx.lastRestore.SourcePath,
				"target_path": fx.lastRestore.TargetPath,
				"conflict":    fx.lastRestore.Conflict,
			}
		},
		missing: `{"action":"restore_snapshot","config_revision":"rev-1"}`,
	},
	{
		name: "verify_snapshot",
		body: `{"action":"verify_snapshot","config_revision":"rev-1","snapshot_verify":` +
			`{"backup_set_id":"production/uploads-tree","run_id":"run_9","level":"content_sample","sample_percent":25}}`,
		carried: func(fx *snapshotFixture) (string, string, map[string]string) {
			return fx.lastVerify.BackupSetID, fx.lastVerify.IdempotencyKey, map[string]string{
				"run_id":         fx.lastVerify.RunID,
				"level":          fx.lastVerify.Level,
				"sample_percent": fmt.Sprint(fx.lastVerify.SamplePercent),
			}
		},
		missing: `{"action":"verify_snapshot","config_revision":"rev-1"}`,
	},
	{
		name: "hold_snapshot",
		body: `{"action":"hold_snapshot","config_revision":"rev-1","snapshot_hold":` +
			`{"backup_set_id":"production/uploads-tree","run_id":"run_9","reason":"incident 4711"}}`,
		carried: func(fx *snapshotFixture) (string, string, map[string]string) {
			return fx.lastHold.BackupSetID, fx.lastHold.IdempotencyKey, map[string]string{
				"run_id": fx.lastHold.RunID,
				"reason": fx.lastHold.Reason,
			}
		},
		missing: `{"action":"hold_snapshot","config_revision":"rev-1"}`,
	},
	{
		name: "release_snapshot_hold",
		body: `{"action":"release_snapshot_hold","config_revision":"rev-1","snapshot_hold_release":` +
			`{"backup_set_id":"production/uploads-tree","hold_id":"hold_3"}}`,
		carried: func(fx *snapshotFixture) (string, string, map[string]string) {
			return fx.lastHoldRelease.BackupSetID, fx.lastHoldRelease.IdempotencyKey, map[string]string{
				"hold_id": fx.lastHoldRelease.HoldID,
			}
		},
		missing: `{"action":"release_snapshot_hold","config_revision":"rev-1"}`,
	},
}

// expectedSnapshotParameters is what each body above says, by the same
// names, so a handler that put a value in the wrong field is caught
// rather than a handler that merely filled something in.
var expectedSnapshotParameters = map[string]map[string]string{
	"restore_snapshot": {
		"snapshot_id": "snap_7",
		"source_path": "/srv/uploads/a",
		"target_path": "/restore/here",
		"conflict":    "overwrite",
	},
	"verify_snapshot": {
		"run_id":         "run_9",
		"level":          "content_sample",
		"sample_percent": "25",
	},
	"hold_snapshot": {
		"run_id": "run_9",
		"reason": "incident 4711",
	},
	"release_snapshot_hold": {
		"hold_id": "hold_3",
	},
}

func TestSnapshotActions_ParametersAndTheIdempotencyKeyReachTheService(t *testing.T) {
	for _, action := range snapshotActions {
		t.Run(action.name, func(t *testing.T) {
			tr := newOperationsTestRouter(t, alwaysPassGate{})
			const key = "idem-snapshot-1"

			rec := submitOperation(t, tr.router, key, action.body)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
			}

			setID, gotKey, extra := action.carried(snapshotsOf(tr.backend))
			if setID != "production/uploads-tree" {
				t.Errorf("the service was handed backup set %q, want %q", setID, "production/uploads-tree")
			}
			// The header, not a body field. A submission retried by a
			// client that sent the same key has to be the same act, and
			// this is the only thing that makes it one.
			if gotKey != key {
				t.Errorf("the service was handed idempotency key %q, want %q; without it every retry of this act is a second one", gotKey, key)
			}
			for field, want := range expectedSnapshotParameters[action.name] {
				if extra[field] != want {
					t.Errorf("%s reached the service as %q, want %q", field, extra[field], want)
				}
			}
		})
	}
}

// A submission naming an action and carrying none of its parameters is
// refused rather than submitted with an empty object: a hold with no
// reason, or a restore with no target, is not a request anybody meant to
// make, and the four refusals are the same refusal so a client handles
// one case.
func TestSnapshotActions_AMissingParameterObjectIsRefused(t *testing.T) {
	for _, action := range snapshotActions {
		t.Run(action.name, func(t *testing.T) {
			tr := newOperationsTestRouter(t, alwaysPassGate{})

			rec := submitOperation(t, tr.router, "idem-missing", action.missing)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if code := errorCodeOf(t, rec); code != "INVALID_REQUEST" {
				t.Errorf("code = %q, want INVALID_REQUEST", code)
			}
		})
	}
}

// The refusals the four share, each mapped onto the status and code the
// contract declares for submitOperation. Every one of these is reached by
// an operator clicking a control on a screen that has moved on -- a
// snapshot pruned since the page loaded, a hold somebody else released --
// so each has to be told apart from the others by code alone.
func TestSnapshotActions_TypedRefusalsCarryTheirDeclaredCode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"the snapshot is gone", service.ErrSnapshotNotFound, http.StatusNotFound, "SNAPSHOT_NOT_FOUND"},
		{"the hold is gone", service.ErrSnapshotHoldNotFound, http.StatusNotFound, "SNAPSHOT_HOLD_NOT_FOUND"},
		{"the snapshot cannot be held", service.ErrSnapshotNotHoldable, http.StatusConflict, "SNAPSHOT_NOT_HOLDABLE"},
		{"the set is not incremental", service.ErrSnapshotRestoreUnsupported, http.StatusBadRequest, "BACKUP_SET_NOT_INCREMENTAL"},
		// EPIC K's production gate (#789). Its own code and a conflict
		// rather than the 400 above it, because the two offer an operator
		// different things: that one says ask about a different set, this
		// one says this deployment does not run the engine at all, and a
		// client that could not tell them apart would offer the wrong way
		// out for both.
		{"the incremental engine is gated off", service.ErrIncrementalEngineDisabled, http.StatusConflict, "INCREMENTAL_ENGINE_DISABLED"},
		{"the set is gone", service.ErrBackupSetNotFound, http.StatusNotFound, "BACKUP_SET_NOT_FOUND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newOperationsTestRouter(t, alwaysPassGate{})
			snapshotsOf(tr.backend).errOnSubmit = fmt.Errorf("%w: the fixture's own sentence", tc.err)

			rec := submitOperation(t, tr.router, "idem-refused", snapshotActions[2].body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if code := errorCodeOf(t, rec); code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// A body carrying one action's parameters under another action is
// refused, over HTTP, for every pairing this route can be asked.
//
// The flat backup_set_id row is the one that was actually served: it was
// refused for run_cycle alone, so a hold_snapshot carrying a top-level
// backup_set_id went through with the field ignored -- and the two sets
// in that body can be DIFFERENT, which is a caller holding a snapshot in
// one set while believing it held one in another.
func TestSnapshotActions_ForeignParametersAreRefusedOverHTTP(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "a hold carrying a flat backup_set_id",
			body: `{"action":"hold_snapshot","config_revision":"rev-1","backup_set_id":"production/other-set",` +
				`"snapshot_hold":{"backup_set_id":"production/uploads-tree","reason":"incident 4711"}}`,
		},
		{
			name: "a hold carrying verify parameters",
			body: `{"action":"hold_snapshot","config_revision":"rev-1","snapshot_hold":{"backup_set_id":"production/uploads-tree","reason":"r"},` +
				`"snapshot_verify":{"backup_set_id":"production/uploads-tree"}}`,
		},
		{
			name: "a verify carrying hold parameters",
			body: `{"action":"verify_snapshot","config_revision":"rev-1","snapshot_verify":{"backup_set_id":"production/uploads-tree"},` +
				`"snapshot_hold":{"backup_set_id":"production/uploads-tree","reason":"r"}}`,
		},
		{
			name: "a run_cycle carrying snapshot restore parameters",
			body: `{"action":"run_cycle","config_revision":"rev-1","snapshot_restore":{"backup_set_id":"production/uploads-tree","target_path":"/restore/here"}}`,
		},
		{
			name: "a restore_snapshot carrying the placement restore's parameters",
			body: `{"action":"restore_snapshot","config_revision":"rev-1","snapshot_restore":{"backup_set_id":"production/uploads-tree","target_path":"/restore/here"},` +
				`"restore":{"artifact_id":"production/uploads-tree/a.dump","medium_id":"offsite"}}`,
		},
		{
			name: "a release carrying the hold's parameters",
			body: `{"action":"release_snapshot_hold","config_revision":"rev-1","snapshot_hold_release":{"backup_set_id":"production/uploads-tree","hold_id":"hold_3"},` +
				`"snapshot_hold":{"backup_set_id":"production/uploads-tree","reason":"r"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newOperationsTestRouter(t, alwaysPassGate{})

			rec := submitOperation(t, tr.router, "idem-foreign", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; a body that has confused two operations must not be served, body: %s",
					rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if code := errorCodeOf(t, rec); code != "INVALID_REQUEST" {
				t.Errorf("code = %q, want INVALID_REQUEST", code)
			}

			fx := snapshotsOf(tr.backend)
			if fx.lastHold.BackupSetID != "" || fx.lastVerify.BackupSetID != "" ||
				fx.lastRestore.BackupSetID != "" || fx.lastHoldRelease.BackupSetID != "" {
				t.Error("a refused submission still reached the service; a refusal must run no work at all")
			}
		})
	}
}

// Issue #852's refusal on the three backup-set routes that can meet it: a
// source that answered and cannot be written to.
//
// All three are ways of asking this manager to start deleting a source's
// originals -- creating a writable set, editing one to be writable, and
// turning read-only off -- and all three have to answer with the code
// that says "the source will not have it", rather than with the one that
// says the form was wrong or the server was down. The way out is granting
// write permission or keeping the set read-only, and
// skip_connection_check is deliberately not one.
func TestBackupSetWrites_ASourceThatCannotBeWrittenToIsRefusedAsDeclared(t *testing.T) {
	const refusal = "the source answered and these credentials cannot write there"
	sourceNotWritable := func() error {
		return fmt.Errorf("%w: "+refusal, service.ErrSourceNotWritable)
	}

	for _, tc := range []struct {
		name string
		// submit arms one fake and makes the request, so each row uses
		// the fixture its own route is already tested through rather
		// than a fourth one written for this table.
		submit func(t *testing.T) *httptest.ResponseRecorder
	}{
		{
			name: "create",
			submit: func(t *testing.T) *httptest.ResponseRecorder {
				tr := newBackupSetsTestRouter(t)
				tr.backend.errOnCreate = sourceNotWritable()

				return postBackupSet(t, tr.router, validCreateBody, true)
			},
		},
		{
			name: "patch",
			submit: func(t *testing.T) *httptest.ResponseRecorder {
				tr := newBackupSetsTestRouter(t)
				seedSet(t, tr, "api/postgres-primary")
				tr.backend.errOnUpdate = sourceNotWritable()

				return patchBackupSet(t, tr.router, "api/postgres-primary", `{"local_path":"/data/backups/production/postgres"}`, true)
			},
		},
		{
			name: "read-only off",
			submit: func(t *testing.T) *httptest.ResponseRecorder {
				rt := newReadSurfaceRouter(t)
				rt.backend.errOnSetReadOnly = sourceNotWritable()

				return rt.post(t, "/api/v1/backup-sets/production/postgres/read-only", `{"read_only":false}`)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.submit(t)

			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusConflict, rec.Body.String())
			}
			if code := errorCodeOf(t, rec); code != "BACKUP_SET_SOURCE_NOT_WRITABLE" {
				t.Fatalf("code = %q, want BACKUP_SET_SOURCE_NOT_WRITABLE; a client that read this as INVALID_REQUEST would tell an operator their form was wrong when their source was", code)
			}
			if body := rec.Body.String(); !strings.Contains(body, refusal) {
				t.Errorf("the refusal does not carry the service's own sentence, so an operator is told nothing about what to do:\n%s", body)
			}
		})
	}
}

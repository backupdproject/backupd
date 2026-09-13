package apiclient

import (
	"context"
	"net/url"
	"strconv"

	"github.com/backupdproject/backupd/core/apicontract"
)

// The typed calls.
//
// Each one names a contract operation id and hands over its path
// parameters in the order the contract declares them; none of them builds
// a URL, and none of them declares a request or response type. That is the
// point: the path, the method, whether a session is required, whether the
// double-submit token is required, which status counts as success and
// which error codes are legal all come from core/apicontract, so an
// operation that changes in api/v1/openapi.json changes here by
// regeneration rather than by somebody noticing.
//
// The set below is the surface the commands issue #543 and #544 route need
// - the mutating backup-set verbs, the two steps a create takes before
// them, and the five operations the reads `sources`, `status`, `artifacts`
// and the retention preview put their questions to - plus the
// session verbs. It is deliberately not all forty-six operations: an
// untested wrapper around an endpoint no command calls is a claim that
// this client works against it, and nothing here has watched that claim
// fail. Adding one is three lines and a contract id.
//
// setBackupSetEnabled and setBackupSetReadOnly were absent under that
// same rule and have a caller now (#788). `backup-set enabled` and
// `backup-set read-only` are the two post-creation toggles a terminal
// could not reach at all, and both rewrite config.yaml, so beside a
// serving engine they were refused with nothing on the other side of the
// refusal. These are that other side.

// ListBackupSets is GET /backup-sets: the configuration the ENGINE holds,
// which is the whole reason a CLI would ask over HTTP rather than read the
// file itself.
func (c *Client) ListBackupSets(ctx context.Context) (apicontract.ListBackupSetsResponse, error) {
	var out apicontract.ListBackupSetsResponse
	err := c.call(ctx, "listBackupSets", nil, nil, &out)
	return out, err
}

// GetBackupSet is GET /backup-sets/{source}/{set}.
//
// Two arguments rather than one composite id, because that is what the
// contract publishes and what the engine routes. It used to be one, back
// when the document spelled this path "/backup-sets/{id}": fillPath
// escapes a parameter, as it must, so "production/postgres" went out as
// "production%2Fpostgres" and every backup set that existed came back
// BACKUP_SET_NOT_FOUND (PR #546 review).
func (c *Client) GetBackupSet(ctx context.Context, source, set string) (apicontract.BackupSet, error) {
	var out apicontract.BackupSet
	err := c.call(ctx, "getBackupSet", []string{source, set}, nil, &out)
	return out, err
}

// CreateBackupSet is POST /backup-sets, the operation whose CLI twin
// wrote past a running engine in issue #535.
func (c *Client) CreateBackupSet(ctx context.Context, req apicontract.CreateBackupSetRequest) (apicontract.CreateBackupSetResponse, error) {
	var out apicontract.CreateBackupSetResponse
	err := c.call(ctx, "createBackupSet", nil, req, &out)
	return out, err
}

// ImportSSHKey is POST /ssh-keys: the private key this deployment will
// use to reach a source, handed over once so the engine can keep its own
// copy.
//
// It is here because `backup-set create --ssh-key-file` has to import
// before it can name a key id, and importing into whatever filesystem the
// CLI happens to be running on is importing into the wrong place when the
// key store belongs to the process being routed to. The request carries
// key material, which is the only body in this package that does; it is
// built by the caller from a file it read, sent once, and held nowhere
// else. Nothing here logs it, and the response deliberately carries a
// reference and a fingerprint rather than the key.
func (c *Client) ImportSSHKey(ctx context.Context, req apicontract.ImportSSHKeyRequest) (apicontract.ImportSSHKeyResponse, error) {
	var out apicontract.ImportSSHKeyResponse
	err := c.call(ctx, "importSSHKey", nil, req, &out)
	return out, err
}

// ProbeHostKey is POST /ssh/host-key-probe: open a connection to a source
// from where the ENGINE is, and report the host key that answered.
//
// Which end probes is the whole point. `--trust-host-key` is trust on
// first use, and the machine that has to be able to reach the source is
// the one that will be pulling backups off it. A CLI that probed from its
// own host would trust a key seen from somewhere the backups never travel,
// and would fail outright wherever the source is only reachable from the
// engine's network.
func (c *Client) ProbeHostKey(ctx context.Context, req apicontract.HostKeyProbeRequest) (apicontract.HostKeyProbeResponse, error) {
	var out apicontract.HostKeyProbeResponse
	err := c.call(ctx, "probeHostKey", nil, req, &out)
	return out, err
}

// TestConnection is POST /backup-sets/test-connection, in either of its
// two modes: name BackupSetID to re-check a set that already exists, or
// fill in the connection details to check a candidate before it is saved
// (issue #624 gave the CLI a caller for both).
//
// Which end runs the check is ProbeHostKey's point restated: the machine
// that has to be able to reach the source is the one that will be pulling
// backups off it, and it is also the one whose live feed the six steps
// belong on. A check run from this host would prove a route the backups
// never travel and would leave its result in a terminal nobody else can
// see.
//
// One method for both modes because it is one route and one response
// shape. The contract's own rule, exactly one mode per request, is the
// caller's to keep: engineroute.go builds one or the other and never both.
func (c *Client) TestConnection(ctx context.Context, req apicontract.TestConnectionRequest) (apicontract.TestConnectionResponse, error) {
	var out apicontract.TestConnectionResponse
	err := c.call(ctx, "testCandidateConnection", nil, req, &out)
	return out, err
}

// UpdateBackupSet is PATCH /backup-sets/{source}/{set}.
func (c *Client) UpdateBackupSet(ctx context.Context, source, set string, req apicontract.UpdateBackupSetRequest) (apicontract.BackupSet, error) {
	var out apicontract.BackupSet
	err := c.call(ctx, "updateBackupSet", []string{source, set}, req, &out)
	return out, err
}

// RemoveBackupSet is DELETE /backup-sets/{source}/{set}.
func (c *Client) RemoveBackupSet(ctx context.Context, source, set string) error {
	return c.call(ctx, "removeBackupSet", []string{source, set}, nil, nil)
}

// SetBackupSetEnabled is POST /backup-sets/{source}/{set}/enabled, and
// SetBackupSetReadOnly is POST /backup-sets/{source}/{set}/read-only.
//
// Both answer with the WHOLE backup set rather than with an
// acknowledgement, which is what lets the command print the posture the
// engine actually holds instead of the one it asked for. That
// distinction is the verb's own rule (backupsettoggle.go): a write that
// was coerced and a write that did exactly what was asked are different
// outcomes, and a caller that echoed its own request would report the
// second for both.
func (c *Client) SetBackupSetEnabled(ctx context.Context, source, set string, req apicontract.SetEnabledRequest) (apicontract.BackupSet, error) {
	var out apicontract.BackupSet
	err := c.call(ctx, "setBackupSetEnabled", []string{source, set}, req, &out)
	return out, err
}

func (c *Client) SetBackupSetReadOnly(ctx context.Context, source, set string, req apicontract.SetReadOnlyRequest) (apicontract.BackupSet, error) {
	var out apicontract.BackupSet
	err := c.call(ctx, "setBackupSetReadOnly", []string{source, set}, req, &out)
	return out, err
}

// GetBackupSetRetention is GET /backup-sets/{source}/{set}/retention.
func (c *Client) GetBackupSetRetention(ctx context.Context, source, set string) (apicontract.BackupSetRetention, error) {
	var out apicontract.BackupSetRetention
	err := c.call(ctx, "getBackupSetRetention", []string{source, set}, nil, &out)
	return out, err
}

// SetBackupSetRetention is PUT /backup-sets/{source}/{set}/retention.
func (c *Client) SetBackupSetRetention(ctx context.Context, source, set string, req apicontract.RetentionOverride) (apicontract.BackupSetRetention, error) {
	var out apicontract.BackupSetRetention
	err := c.call(ctx, "setBackupSetRetention", []string{source, set}, req, &out)
	return out, err
}

// ClearBackupSetRetention is DELETE /backup-sets/{source}/{set}/retention,
// which answers with the policy the set falls back to rather than with
// nothing.
func (c *Client) ClearBackupSetRetention(ctx context.Context, source, set string) (apicontract.BackupSetRetention, error) {
	var out apicontract.BackupSetRetention
	err := c.call(ctx, "clearBackupSetRetention", []string{source, set}, nil, &out)
	return out, err
}

// GetSettings is GET /settings: the retention and capacity policy the
// ENGINE is deciding with, resolved, which is a different fact from what
// this host's config.yaml says and the whole reason to ask over HTTP.
func (c *Client) GetSettings(ctx context.Context) (apicontract.SettingsResponse, error) {
	var out apicontract.SettingsResponse
	err := c.call(ctx, "getSettings", nil, nil, &out)
	return out, err
}

// UpdateSettings is PATCH /settings, the other configuration write a
// terminal can make. It is here rather than left for later because
// `settings patch` is refused beside a running engine (#538) and this is
// the only thing that can replace that refusal with a change that lands.
func (c *Client) UpdateSettings(ctx context.Context, req apicontract.UpdateSettingsRequest) (apicontract.SettingsResponse, error) {
	var out apicontract.SettingsResponse
	err := c.call(ctx, "updateSettings", nil, req, &out)
	return out, err
}

// SystemHealth is GET /system/health, half of what `status` reports.
func (c *Client) SystemHealth(ctx context.Context) (apicontract.HealthResponse, error) {
	var out apicontract.HealthResponse
	err := c.call(ctx, "getSystemHealth", nil, nil, &out)
	return out, err
}

// SystemVersion is GET /system/version. It carries the engine's own
// config_revision, which is what lets a command say WHICH configuration
// the answer came from rather than only what it said.
func (c *Client) SystemVersion(ctx context.Context) (apicontract.VersionResponse, error) {
	var out apicontract.VersionResponse
	err := c.call(ctx, "getSystemVersion", nil, nil, &out)
	return out, err
}

// StorageStatus is GET /system/storage, the other half of `status`.
func (c *Client) StorageStatus(ctx context.Context) (apicontract.ListStorageStatusResponse, error) {
	var out apicontract.ListStorageStatusResponse
	err := c.call(ctx, "listStorageStatus", nil, nil, &out)
	return out, err
}

// ListArtifacts is GET /backups: every artifact the ENGINE's journal holds
// at the moment it is asked, optionally narrowed to one backup set.
//
// setID is a "source/set" id and goes in the QUERY rather than the path,
// which is why this is the one call in this package that sends one at all.
// An empty setID sends no query, and that is not the same request as
// ?setId=: the contract refuses an id naming no configured backup set
// rather than answering it with an empty list, so an empty filter sent as
// a filter would turn "show me everything" into a 404.
//
// The unfiltered listing includes the artifacts of backup sets whose
// configuration was removed (issue #391), and every artifact carries
// retention_policy so a caller can tell those apart (issue #523).
func (c *Client) ListArtifacts(ctx context.Context, setID string) (apicontract.ListArtifactsResponse, error) {
	var query url.Values
	if setID != "" {
		query = url.Values{"setId": []string{setID}}
	}
	var out apicontract.ListArtifactsResponse
	err := c.callQuery(ctx, "listArtifacts", nil, query, nil, &out)
	return out, err
}

// GetArtifact is GET /backups/{source}/{set}/{name}.
//
// Three arguments rather than one composite id, for GetBackupSet's reason
// one level deeper: the contract used to spell this "/backups/{id}", and
// an artifact id is three segments, so fillPath escaped two slashes into
// one unroutable parameter and this operation could not be called at all
// (PR #546 review).
func (c *Client) GetArtifact(ctx context.Context, source, set, name string) (apicontract.Artifact, error) {
	var out apicontract.Artifact
	err := c.call(ctx, "getArtifact", []string{source, set, name}, nil, &out)
	return out, err
}

// PreviewRetention is GET /backup-sets/{source}/{set}/retention/preview:
// the plan the ENGINE would apply, derived from the configuration that
// engine holds rather than from the file on disk.
//
// It issues a plan_id and deletes nothing. FR-20's deletion runs through
// applyRetention, which refuses unless the plan it re-derives still
// fingerprints as the one that id was issued for, and this package
// deliberately has no method for that: `backupd retention` is a
// preview in both its modes (retention.go's own doc) and a CLI apply would
// be a second authorisation path beside the one an administrator reviews.
func (c *Client) PreviewRetention(ctx context.Context, source, set string) (apicontract.RetentionPlan, error) {
	var out apicontract.RetentionPlan
	err := c.call(ctx, "previewRetention", []string{source, set}, nil, &out)
	return out, err
}

// The storage-destination surface (G2.2, issue #594), which is what makes
// `backupd medium add|edit|remove|import-credentials` work beside a
// running engine instead of being refused.
//
// They are here under this file's own rule and not in spite of it: each
// one has a command that drives it (core/cmd/backupd/medium.go),
// so none of them is an untested wrapper claiming this client works
// against a route nothing calls.

// ImportStorageCredentials is POST /storage-credentials: the one call on
// this client that ever carries S3 credential material, and it carries it
// once, in one direction.
//
// The material arrives on this process's STDIN (`medium
// import-credentials --stdin`) and leaves in a request body over the
// session this client already holds. It is never a command-line argument,
// so it is not in the process table and not in shell history, and the
// response is an id, so there is nothing to redact in anything printed
// afterwards.
func (c *Client) ImportStorageCredentials(ctx context.Context, req apicontract.ImportStorageCredentialsRequest) (apicontract.ImportStorageCredentialsResponse, error) {
	var out apicontract.ImportStorageCredentialsResponse
	err := c.call(ctx, "importStorageCredentials", nil, req, &out)
	return out, err
}

// ListStorageMediums is GET /storage-mediums: the destinations the ENGINE
// holds, which is why a terminal would ask over HTTP rather than read the
// file itself.
func (c *Client) ListStorageMediums(ctx context.Context) (apicontract.ListStorageMediumsResponse, error) {
	var out apicontract.ListStorageMediumsResponse
	err := c.call(ctx, "listStorageMediums", nil, nil, &out)
	return out, err
}

// GetStorageMedium is GET /storage-mediums/{id}.
func (c *Client) GetStorageMedium(ctx context.Context, id string) (apicontract.StorageMediumSummary, error) {
	var out apicontract.StorageMediumSummary
	err := c.call(ctx, "getStorageMedium", []string{id}, nil, &out)
	return out, err
}

// PreflightStorageMedium is POST /storage-mediums/{id}/preflight: the
// check by id, against a destination this deployment already declares.
//
// The candidate form beside it has been here since #594 because `medium
// add` verifies before it writes and the check and the write have to
// happen in one world. This one is here for a reason of its own, and it
// arrived with #636: a check that PASSES now clears that destination's
// unverified mark, which is a configuration write, so it belongs on the
// door configuration writes go through rather than beside the reads. The
// process that clears the mark has to be the process whose configuration
// the mark is in.
//
// "local" is a legal id: the engine answers it with the local hard
// drive's own check (#622), which is why nothing here filters the id.
func (c *Client) PreflightStorageMedium(ctx context.Context, id string) (apicontract.MediumPreflightResponse, error) {
	var out apicontract.MediumPreflightResponse
	err := c.call(ctx, "preflightStorageMedium", []string{id}, nil, &out)
	return out, err
}

// StorageMediumUsage is GET /storage-mediums/{id}/usage: FR-30's report of
// what is actually on a destination, per backup set.
func (c *Client) StorageMediumUsage(ctx context.Context, id string) (apicontract.StorageMediumUsageResponse, error) {
	var out apicontract.StorageMediumUsageResponse
	err := c.call(ctx, "getStorageMediumUsage", []string{id}, nil, &out)
	return out, err
}

// PreflightStorageMediumCandidate is POST /storage-mediums/preflight:
// prove a destination that has not been saved. It writes nothing whatever
// the report says.
func (c *Client) PreflightStorageMediumCandidate(ctx context.Context, req apicontract.StorageMediumRequest) (apicontract.MediumPreflightResponse, error) {
	var out apicontract.MediumPreflightResponse
	err := c.call(ctx, "preflightStorageMediumCandidate", nil, req, &out)
	return out, err
}

// CreateStorageMedium is POST /storage-mediums.
func (c *Client) CreateStorageMedium(ctx context.Context, req apicontract.StorageMediumRequest) (apicontract.StorageMediumSummary, error) {
	var out apicontract.StorageMediumSummary
	err := c.call(ctx, "createStorageMedium", nil, req, &out)
	return out, err
}

// UpdateStorageMedium is PUT /storage-mediums/{id}.
func (c *Client) UpdateStorageMedium(ctx context.Context, id string, req apicontract.StorageMediumRequest) (apicontract.StorageMediumSummary, error) {
	var out apicontract.StorageMediumSummary
	err := c.call(ctx, "updateStorageMedium", []string{id}, req, &out)
	return out, err
}

// RemoveStorageMedium is DELETE /storage-mediums/{id}, which the engine
// refuses while any copy names the destination (FR-30), and since H2.2
// (#622) also when the destination is the local hard drive or is the one
// a newly created retention tier starts on.
func (c *Client) RemoveStorageMedium(ctx context.Context, id string) error {
	return c.call(ctx, "removeStorageMedium", []string{id}, nil, nil)
}

// SetDefaultStorageMedium is PUT /storage-mediums/{id}/default: make this
// the destination a NEWLY CREATED retention tier starts on (H2.2, #622).
//
// It carries no body. The whole content of the request is which
// destination, and that is in the path, so a body would be a second place
// for the same fact to be written and a second thing for the engine to
// have to reconcile against the path.
func (c *Client) SetDefaultStorageMedium(ctx context.Context, id string) (apicontract.StorageMediumSummary, error) {
	var out apicontract.StorageMediumSummary
	err := c.call(ctx, "setDefaultStorageMedium", []string{id}, nil, &out)
	return out, err
}

// CreateRepositoryDomain is POST /repositories (issue #862): declare a
// repository security boundary.
//
// It answers with the domain's HEALTH rather than with an echo of the
// declaration, because that is the shape the route answers with -- but
// NOT a probe: declaring opens no storage and resolves no passphrase
// reference, so what comes back is built from the declaration (the id,
// the co-tenancy posture, DEGRADED, and a detail saying the store is
// written by the first backup run into the domain) with every access
// boolean false because nothing was measured. GET /repositories is what
// probes, and a domain nothing has run into yet reads there as reachable
// and not yet readable.
func (c *Client) CreateRepositoryDomain(ctx context.Context, req apicontract.CreateRepositoryDomainRequest) (apicontract.RepositoryHealth, error) {
	var out apicontract.RepositoryHealth
	err := c.call(ctx, "createRepositoryDomain", nil, req, &out)
	return out, err
}

// ListActivity is GET /activity: the deployment-wide lifecycle feed, newest
// first.
//
// limit is advisory in both directions and matches the route's own
// handling: zero or less sends no query at all and takes the engine's
// default, and a number above the engine's maximum is clamped there rather
// than refused. A caller asking for a feed gets a feed.
//
// This is the one read in this package whose answer is RENDERED rather
// than compared. readmode.go's doc explains why the other four are
// comparisons: each of those commands prints something the contract cannot
// express, so re-rendering from the wire would print less than the direct
// route does. This feed has no such gap. Every column `activity` shows is
// a field of ActivityEvent, so beside a live engine the honest thing is to
// print the engine's own answer, and a set comparison would be worse than
// useless here anyway: the log grows while the two reads happen, so two
// truthful answers taken a round trip apart legitimately differ at the
// newest end.
func (c *Client) ListActivity(ctx context.Context, limit int) (apicontract.ListActivityResponse, error) {
	var query url.Values
	if limit > 0 {
		query = url.Values{"limit": []string{strconv.Itoa(limit)}}
	}
	var out apicontract.ListActivityResponse
	err := c.callQuery(ctx, "listActivity", nil, query, nil, &out)
	return out, err
}

// LiveActivity is GET /activity/live: what every configured backup set is
// doing right now, plus the tail of events behind it.
//
// A different feed from ListActivity above and deliberately not a
// variation on it. That one reads the durable, append-only transition log
// and survives a restart; this one is a bounded in-memory tail of the
// SERVING PROCESS's own event stream, so it exists only where that process
// does. A caller with no route to it has nothing to read, which is why
// `backupd activity --follow` refuses rather than falling back to
// the journal: the two feeds answer different questions and quietly
// swapping one for the other would be this repository's own recurring
// defect, two surfaces telling an operator different things.
//
// since is the highest sequence the caller has already seen, and zero
// means "whatever is still held". It travels in the query rather than a
// body because this is a GET, and it is what keeps polling cheap.
//
// The response's Epoch names the process that answered. A caller MUST drop
// its cursor and everything it is holding when that changes, or after a
// restart it goes on showing a dead process's log with a cursor the new
// one cannot honour (#573).
//
// scope is the half of the set/deployment split that backupSetID cannot
// express, because naming no set already means "every set" (#593). The
// contract takes exactly one value, "deployment", and narrows the reading
// to the log that belongs to no single backup set: the reading a terminal
// following the deployment's own log wants, and the only reading a fresh
// install with nothing configured has. Sent together with a backupSetID
// the narrower question wins and the answer is about that set alone, so
// the two are never a contradiction the engine has to refuse. Empty means
// the whole reading.
func (c *Client) LiveActivity(ctx context.Context, backupSetID, scope string, since int64, limit int) (apicontract.LiveActivityResponse, error) {
	query := url.Values{}
	if backupSetID != "" {
		query.Set("backup_set", backupSetID)
	}
	if scope != "" {
		query.Set("scope", scope)
	}
	if since > 0 {
		query.Set("since", strconv.FormatInt(since, 10))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if len(query) == 0 {
		query = nil
	}
	var out apicontract.LiveActivityResponse
	err := c.callQuery(ctx, "getLiveActivity", nil, query, nil, &out)
	return out, err
}

// GetBackupSetEditHold is GET /backup-sets/{source}/{set}/edit-hold: what
// entering edit mode for this set would interrupt, and whether a hold is
// already in force.
//
// Two arguments rather than a composite id, for GetBackupSet's reason: the
// contract routes source and set as separate path parameters, and an id
// pasted whole would be escaped into one unroutable segment.
func (c *Client) GetBackupSetEditHold(ctx context.Context, source, set string) (apicontract.BackupSetEditHoldState, error) {
	var out apicontract.BackupSetEditHoldState
	err := c.call(ctx, "getBackupSetEditHold", []string{source, set}, nil, &out)
	return out, err
}

// ReleaseBackupSetEditHold is POST
// /backup-sets/{source}/{set}/edit-hold/release: give the lease back, so
// the scheduler may run this set again without waiting for the hold to
// lapse.
//
// takeBackupSetEditHold is deliberately NOT here, under this file's own
// rule about wrappers nothing calls. A hold exists to protect an edit
// session from a cycle running underneath it, and a CLI edit is one
// `backup-set patch` that either runs or does not: there is no session to
// protect, so a CLI that could TAKE a hold would only be able to pause a
// backup set with no way for anything to notice it meant to. Releasing one
// somebody else left behind is the operation an operator actually needs
// from a terminal.
func (c *Client) ReleaseBackupSetEditHold(ctx context.Context, source, set string) error {
	return c.call(ctx, "releaseBackupSetEditHold", []string{source, set}, nil, nil)
}

// EPIC L's four routed workflow operations (#813), and the reason they
// are four rather than eighteen.
//
// This file's rule is that a method exists because a command drives it,
// and for the workflow surface the line between what a terminal can
// answer on its own and what only a serving engine can is sharp. A CLI
// process opens the configuration and the journal, so it answers the
// configuration reads, the run reads and `validate workflow` in its own
// process, and its configuration WRITES go through the same
// *BackupService door every other configuration write does. Those
// therefore have no wrapper here, deliberately, and adding one would be
// a claim that this client works against a route nothing calls.
//
// These four are the ones that structurally cannot be answered anywhere
// but in the process that holds the workflow engine. core/service reports
// ErrWorkflowsNotWired for every one of them when there is no engine, and
// a CLI process never builds one: the step-log tail needs the engine's
// broker to wait on, the recovery holds are the engine's own set, and a
// resume or an acknowledgement is a state transition the engine owns. So
// beside a serving engine these have to travel over HTTP, which is what
// this client is for.

// WorkflowStepLogs is GET /workflow-runs/{run}/steps/{step}/logs: one
// page of one step's captured output, from a cursor.
//
// The cursor is the CALLER's, which is the whole protocol: `after` is the
// last sequence this caller PROCESSED, and the page's own cursor is what
// to send next time. That makes resume after a dropped connection the
// ordinary read rather than a special case, and it makes every page a
// separately authenticated request -- so a session that has expired is
// refused on the next page instead of a stream outliving its
// authorization.
//
// waitSeconds asks the engine to hold the request briefly for output
// newer than the cursor, which is what keeps a follow of a quiet hook
// from being a poll loop choosing between latency and load. It is
// BOUNDED: the engine clamps it to its own ceiling, so a caller asking
// for an hour gets an answer in seconds. Zero never waits.
//
// after is uint64 because the sequence is: it is run-monotonic, never
// negative and never zero for a real record, so a signed cursor would
// have a range of values that cannot name a position.
func (c *Client) WorkflowStepLogs(ctx context.Context, runID, stepID string, after uint64, limit, waitSeconds int) (apicontract.WorkflowStepLogPage, error) {
	query := url.Values{}
	if after > 0 {
		query.Set("after", strconv.FormatUint(after, 10))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if waitSeconds > 0 {
		query.Set("wait", strconv.Itoa(waitSeconds))
	}
	if len(query) == 0 {
		// No query at all rather than three empty parameters, matching
		// ListActivity's own handling: the route reads an absent value as
		// its own default, and sending "after=0" would be naming a
		// position rather than declining to.
		query = nil
	}

	var out apicontract.WorkflowStepLogPage
	err := c.callQuery(ctx, "getWorkflowStepLogs", []string{runID, stepID}, query, nil, &out)
	return out, err
}

// WorkflowRecovery is GET /workflow-recovery: every run whose cleanup
// this deployment could not finish, and which is therefore holding its
// backup set.
//
// It asks the ENGINE rather than reading the journal for rows that look
// blocking, because the holds are what the next run will actually be
// refused against, and a second derivation of "is this set blocked" would
// be a second answer that can disagree with the one the scheduler acts
// on.
func (c *Client) WorkflowRecovery(ctx context.Context) (apicontract.WorkflowRecoveryResponse, error) {
	var out apicontract.WorkflowRecoveryResponse
	err := c.call(ctx, "listWorkflowRecovery", nil, nil, &out)
	return out, err
}

// ResumeWorkflowCleanup is POST
// /workflow-recovery/{run}/resume-cleanup: run the "after" hooks an
// interrupted run still owes, out of that run's own captured bytes.
//
// It answers with the run as the journal holds it AFTERWARDS, which is
// what a caller has to print: a resume that left the run still blocked
// has to say so from the row the next backup will be refused against,
// rather than from this call's own idea of how it went.
func (c *Client) ResumeWorkflowCleanup(ctx context.Context, runID string) (apicontract.WorkflowRun, error) {
	var out apicontract.WorkflowRun
	err := c.call(ctx, "resumeWorkflowCleanup", []string{runID}, nil, &out)
	return out, err
}

// AcknowledgeWorkflowRecovery is POST
// /workflow-recovery/{run}/acknowledge: record that a person dealt with
// an interrupted run by hand, and unblock its backup set.
//
// The request carries a reason and nothing else. The ACTOR is the
// engine's answer rather than this caller's claim -- it comes from the
// authenticated session on the far side -- which is what makes the record
// worth having six months later.
func (c *Client) AcknowledgeWorkflowRecovery(ctx context.Context, runID string, req apicontract.WorkflowAcknowledgementRequest) error {
	return c.call(ctx, "acknowledgeWorkflowRecovery", []string{runID}, req, nil)
}

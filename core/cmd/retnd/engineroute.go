package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/retnd/retnd/core/apicontract"
	"github.com/retnd/retnd/core/cliecho"
	"github.com/retnd/retnd/core/internal/apiclient"
	"github.com/retnd/retnd/core/service"
)

// The one place core/service's vocabulary and core/apicontract's meet.
//
// Everything the two spell differently is in this file and nowhere else,
// which is the property route.go's interface exists to make possible and
// the reason this file is worth reading top to bottom rather than
// dipping into. There are four differences and each one is a decision:
//
// A duration on one side is a whole number of SECONDS on the other
// (stable_for_seconds, stale_after_seconds). Truncating quietly would let
// `--stable-for 1500ms` be persisted as one second by the engine and as a
// second and a half by a direct write, which is two deployments disagreeing
// about a completion rule. So a value the wire cannot carry is refused
// here, before anything is sent.
//
// An ACTOR is a field of the request on one side and a fact about the
// session on the other. service.CreateBackupSetRequest carries
// Actor: cliActor, and the engine records whoever is signed in. The field
// is dropped rather than sent, and that is the routing working rather than
// something lost in it: the whole argument for going through the engine is
// that the engine decides who acted, from a session it minted, instead of
// believing a client that told it.
//
// An IDENTITY is one string on one side and two path segments on the
// other. "source/name" is split here, because api/v1/openapi.json publishes
// /backup-sets/{source}/{set} as two parameters and the client escapes each
// one (PR #546's review found what a single composite parameter cost: every
// real backup set came back not-found).
//
// There used to be a fourth difference and there is not any more. The
// API's BackupSet carried no stale_after at all, so the engine could not
// report what it had just persisted for a field it had accepted on the way
// in, and a routed create printed "not reported" for a value the operator
// had typed on that very command line. #555 put stale_after_seconds on the
// wire, so both windows now cross in both directions and this adapter
// carries no gap at all.

// engineRoute is backupSetRoute over the running engine.
type engineRoute struct {
	client *apiclient.Client
}

// ImportSSHKey hands the key material to the engine's own key store.
func (r *engineRoute) ImportSSHKey(ctx context.Context, raw []byte, passphrase string) (service.SSHKeyRef, error) {
	resp, err := r.client.ImportSSHKey(ctx, apicontract.ImportSSHKeyRequest{
		PrivateKeyPEM: string(raw),
		Passphrase:    passphrase,
	})
	if err != nil {
		return service.SSHKeyRef{}, err
	}
	// KeyFile is deliberately left empty. It is the server-side path the
	// id resolves to, the API does not publish it (SSHKeyRef's own doc
	// gives the rule), and it is not this deployment's filesystem anyway.
	return service.SSHKeyRef{
		ID:          resp.ID,
		Algorithm:   resp.Algorithm,
		Fingerprint: resp.Fingerprint,
	}, nil
}

// ProbeHostKey asks the ENGINE to dial the source and report what
// answered, which is the only end whose answer is worth trusting: the
// engine is the process that will be pulling backups over that connection.
func (r *engineRoute) ProbeHostKey(ctx context.Context, host string, port int) (service.HostKeyProbe, error) {
	resp, err := r.client.ProbeHostKey(ctx, apicontract.HostKeyProbeRequest{Host: host, Port: port})
	if err != nil {
		return service.HostKeyProbe{}, err
	}
	return service.HostKeyProbe{
		Algorithm:      resp.Algorithm,
		Fingerprint:    resp.Fingerprint,
		KnownHostsLine: resp.KnownHostsLine,
	}, nil
}

// CreateBackupSet is POST /backup-sets.
func (r *engineRoute) CreateBackupSet(ctx context.Context, req service.CreateBackupSetRequest) (service.CreateBackupSetResult, error) {
	stableFor, err := wireSeconds(req.StableFor, "--stable-for")
	if err != nil {
		return service.CreateBackupSetResult{}, err
	}
	staleAfter, err := wireSeconds(req.StaleAfter, "--stale-after")
	if err != nil {
		return service.CreateBackupSetResult{}, err
	}
	verificationFullEvery, err := wireSeconds(req.VerificationFullEvery, "--verification-full-every")
	if err != nil {
		return service.CreateBackupSetResult{}, err
	}
	verificationRestoreDrill, err := wireSeconds(req.VerificationRestoreDrillEvery, "--verification-restore-drill-every")
	if err != nil {
		return service.CreateBackupSetResult{}, err
	}

	resp, err := r.client.CreateBackupSet(ctx, apicontract.CreateBackupSetRequest{
		BackupSetSpec: apicontract.BackupSetSpec{
			SourceName:         req.SourceName,
			Name:               req.Name,
			Host:               req.Host,
			Port:               req.Port,
			User:               req.User,
			SSHKeyID:           req.SSHKeyID,
			KnownHostsLine:     req.KnownHostsLine,
			RemotePath:         req.RemotePath,
			LocalPath:          req.LocalPath,
			Include:            req.Include,
			CompletionStrategy: req.CompletionStrategy,
			StableForSeconds:   stableFor,
			StaleAfterSeconds:  staleAfter,
			ValidatorID:        string(req.ValidatorID),
			Disabled:           req.Disabled,
			ReadOnly:           req.ReadOnly,
			// Issue #624's opt-out, carried across for the reason the
			// patch route carries its own: the engine runs the check in
			// front of the write and marks the set when told not to, so a
			// routed --no-verify that dropped this flag would be refused
			// where the direct one writes.
			SkipConnectionCheck: req.SkipConnectionCheck,

			// EPIC K's engine seam (#788), carried across for the reason
			// every other field here is: a routed create that dropped
			// these would write an ARTIFACT set on the engine while the
			// operator's command line asked for snapshots, which is the
			// one divergence between the two routes that nothing
			// downstream could ever notice. The two cadences are whole
			// SECONDS on the wire, refused rather than truncated,
			// exactly as the two completion windows above are.
			Engine:                               req.Engine,
			RepositoryDomain:                     req.RepositoryDomain,
			SourceConsistency:                    req.SourceConsistency,
			VerificationLevel:                    req.VerificationLevel,
			VerificationSamplePercent:            req.VerificationSamplePercent,
			VerificationFullEverySeconds:         int64(verificationFullEvery),
			VerificationRestoreDrillEverySeconds: int64(verificationRestoreDrill),
		},
		RunImmediately:     req.RunImmediately,
		AcknowledgeRepoint: req.AcknowledgeRepoint,
		// req.Actor is NOT sent. See this file's own doc: the engine
		// records the session it minted, and a body that named an actor
		// would be a client asserting an identity.
	})
	if err != nil {
		return service.CreateBackupSetResult{}, err
	}

	result := service.CreateBackupSetResult{Set: backupSetFromWire(resp.BackupSet)}
	if resp.Operation != nil {
		result.Operation = &service.Operation{
			ID:             resp.Operation.OperationID,
			Actor:          resp.Operation.Actor,
			BackupSetID:    resp.Operation.BackupSetID,
			ConfigRevision: resp.Operation.ConfigRevision,
			Action:         resp.Operation.Action,
			Status:         resp.Operation.Status,
			Result:         resp.Operation.Result,
			Error:          resp.Operation.Error,
		}
	}
	if resp.RunError != "" {
		// The set exists and the run the operator asked for did not start.
		// The direct route reports that by returning a nil Operation from
		// a successful create and nothing else, so this says the part the
		// direct route cannot: which is more than it could, not less.
		fmt.Fprintf(os.Stderr, cliecho.Binary+": the set was created and the run it asked for did not start: %s\n", resp.RunError)
	}
	return result, nil
}

// UpdateBackupSet is PATCH /backup-sets/{source}/{set}.
func (r *engineRoute) UpdateBackupSet(ctx context.Context, id string, req service.UpdateBackupSetRequest) (service.BackupSet, error) {
	source, set, ok := splitBackupSetID(id)
	if !ok {
		return service.BackupSet{}, fmt.Errorf("%q is not a backup set id; a backup set id is exactly source/name", id)
	}

	body := apicontract.UpdateBackupSetRequest{
		Host:               req.Host,
		Port:               req.Port,
		User:               req.User,
		RemotePath:         req.RemotePath,
		LocalPath:          req.LocalPath,
		Include:            req.Include,
		CompletionStrategy: req.CompletionStrategy,
		// Issue #572's two, carried across with the same nil/non-nil
		// distinction everything else on this body keeps: a routed patch
		// that dropped them would report success for a rotation the
		// engine never heard about.
		SSHKeyID:       req.SSHKeyID,
		KnownHostsLine: req.KnownHostsLine,

		AcknowledgeRepoint:       req.AcknowledgeRepoint,
		AcknowledgeHostKeyChange: req.AcknowledgeHostKeyChange,
		// Issue #624's opt-out, carried across for the same reason the
		// two acknowledgements are: an edit refused by the engine for a
		// connection it could not prove has one way past it, and a
		// routed --no-verify that dropped this flag would be refused
		// where the direct one succeeds.
		SkipConnectionCheck: req.SkipConnectionCheck,

		// EPIC K's editable verification budget (#788). The three that
		// are not durations cross as the pointers they already are, so
		// "leave this alone" and "set this to none" stay the opposite
		// requests they are on the direct route.
		SourceConsistency:         req.SourceConsistency,
		VerificationLevel:         req.VerificationLevel,
		VerificationSamplePercent: req.VerificationSamplePercent,
	}
	if req.ValidatorID != nil {
		v := string(*req.ValidatorID)
		body.ValidatorID = &v
	}
	// Both windows keep the nil/non-nil distinction the whole way across,
	// because that is what a sparse edit means on both sides: "leave this
	// alone" and "set this to zero" are opposite requests, and a mapping
	// that collapsed them would silently clear a field nobody named.
	if req.StableFor != nil {
		seconds, err := wireSeconds(*req.StableFor, "--stable-for")
		if err != nil {
			return service.BackupSet{}, err
		}
		body.StableForSeconds = &seconds
	}
	if req.StaleAfter != nil {
		seconds, err := wireSeconds(*req.StaleAfter, "--stale-after")
		if err != nil {
			return service.BackupSet{}, err
		}
		body.StaleAfterSeconds = &seconds
	}
	// Issue #845's per-set cadence, with the same whole-seconds refusal
	// the two completion windows get and the same nil/non-nil meaning:
	// absent leaves the set's cadence alone, and an explicit ZERO is how
	// "inherit the deployment's again" is spelled, so it has to cross as
	// a zero rather than be mistaken for nothing to say.
	if req.PollInterval != nil {
		seconds, err := wireSeconds(*req.PollInterval, "--poll-interval")
		if err != nil {
			return service.BackupSet{}, err
		}
		body.PollIntervalSeconds = &seconds
	}
	// The two verification cadences, with the whole-seconds refusal every
	// other duration on this body gets. An explicit ZERO is "never do
	// this any more", which is the request an operator makes when a
	// nightly restore drill turns out to cost more than it is worth, so
	// it crosses as a zero rather than as nothing to say.
	if req.VerificationFullEvery != nil {
		seconds, err := wireSeconds(*req.VerificationFullEvery, "--verification-full-every")
		if err != nil {
			return service.BackupSet{}, err
		}
		full := int64(seconds)
		body.VerificationFullEverySeconds = &full
	}
	if req.VerificationRestoreDrillEvery != nil {
		seconds, err := wireSeconds(*req.VerificationRestoreDrillEvery, "--verification-restore-drill-every")
		if err != nil {
			return service.BackupSet{}, err
		}
		drill := int64(seconds)
		body.VerificationRestoreDrillEverySeconds = &drill
	}

	updated, err := r.client.UpdateBackupSet(ctx, source, set, body)
	if err != nil {
		return service.BackupSet{}, err
	}
	return backupSetFromWire(updated), nil
}

// SetBackupSetEnabled is POST /backup-sets/{source}/{set}/enabled and
// SetBackupSetReadOnly is POST /backup-sets/{source}/{set}/read-only
// (#788), the two post-creation toggles.
//
// Neither is a patch and neither may become one. The service has a named
// method for each because each is a decision with its own consequence
// (backupsettoggle.go's own doc), and folding them into the sparse edit
// body would put a set's safety declaration where a stray field could
// carry it. The same argument holds on the wire, where the contract
// gives each its own route and its own one-field request.
//
// Both read the posture back out of the SET the engine answered with,
// exactly as the direct route reads it out of what it persisted, so a
// write the engine coerced is reported as what it became.
func (r *engineRoute) SetBackupSetEnabled(ctx context.Context, id string, enabled bool) (service.BackupSet, error) {
	source, set, ok := splitBackupSetID(id)
	if !ok {
		return service.BackupSet{}, fmt.Errorf("%q is not a backup set id; a backup set id is exactly source/name", id)
	}
	updated, err := r.client.SetBackupSetEnabled(ctx, source, set, apicontract.SetEnabledRequest{Enabled: enabled})
	if err != nil {
		return service.BackupSet{}, err
	}
	return backupSetFromWire(updated), nil
}

func (r *engineRoute) SetBackupSetReadOnly(ctx context.Context, id string, readOnly bool) (service.BackupSet, error) {
	source, set, ok := splitBackupSetID(id)
	if !ok {
		return service.BackupSet{}, fmt.Errorf("%q is not a backup set id; a backup set id is exactly source/name", id)
	}
	updated, err := r.client.SetBackupSetReadOnly(ctx, source, set, apicontract.SetReadOnlyRequest{ReadOnly: readOnly})
	if err != nil {
		return service.BackupSet{}, err
	}
	return backupSetFromWire(updated), nil
}

// TestConnection is POST /backup-sets/test-connection in its CANDIDATE
// mode: prove a source described by a request, before any set exists for
// it (issue #624).
//
// Made by the ENGINE rather than here, for ProbeHostKey's reason restated
// with a sharper edge: `backup-set create` verifies before it writes, so
// the check and the write have to happen in the same world. A route proven
// from this shell and a set declared in a process on the other side of a
// container boundary would be two different claims about two different
// networks, reported as one.
func (r *engineRoute) TestConnection(ctx context.Context, req service.ConnectionTestRequest) (service.ConnectionTestResult, error) {
	return connectionTestFromWire(r.client.TestConnection(ctx, apicontract.TestConnectionRequest{
		Host:           req.Host,
		Port:           req.Port,
		User:           req.User,
		SSHKeyID:       req.SSHKeyID,
		KnownHostsLine: req.KnownHostsLine,
		RemotePath:     req.RemotePath,
	}))
}

// TestBackupSetConnection is the same route in its PERSISTED mode: prove
// the set this id names, by id alone.
//
// The connection details come off the engine's own configuration, which is
// the half that makes this mode worth having: a caller asking whether
// "nas-a/photos" still works neither knows nor has to echo back that set's
// key reference and trusted line, so a read-only check cannot be turned
// into a check of something else.
//
// This is also where a passing check clears issue #624's unverified mark,
// in the process that holds the configuration the mark is in.
func (r *engineRoute) TestBackupSetConnection(ctx context.Context, id string) (service.ConnectionTestResult, error) {
	if !isBackupSetID(id) {
		return service.ConnectionTestResult{}, fmt.Errorf("%q is not a backup set id; a backup set id is exactly source/name", id)
	}
	return connectionTestFromWire(r.client.TestConnection(ctx, apicontract.TestConnectionRequest{BackupSetID: id}))
}

// connectionTestFromWire is the one translation both modes come back
// through, so the routed answer and the direct one cannot describe the
// same six steps differently.
func connectionTestFromWire(resp apicontract.TestConnectionResponse, err error) (service.ConnectionTestResult, error) {
	if err != nil {
		return service.ConnectionTestResult{}, err
	}
	// Writable travels with OK and Message rather than being re-derived
	// from the write_probe row: it is the field the contract carries
	// (issue #852), and a CLI that computed it from a sentence would be
	// a second answer to the question the engine already answered.
	result := service.ConnectionTestResult{OK: resp.OK, Message: resp.Message, Writable: resp.Writable}
	for _, c := range resp.Checks {
		result.Checks = append(result.Checks, service.ConnectionCheck{
			Step:       c.Step,
			Outcome:    c.Outcome,
			Category:   c.Category,
			Detail:     c.Detail,
			DurationMs: c.DurationMs,
		})
	}
	return result, nil
}

// RemoveBackupSet is DELETE /backup-sets/{source}/{set}.
func (r *engineRoute) RemoveBackupSet(ctx context.Context, id string) error {
	source, set, ok := splitBackupSetID(id)
	if !ok {
		return fmt.Errorf("%q is not a backup set id; a backup set id is exactly source/name", id)
	}
	return r.client.RemoveBackupSet(ctx, source, set)
}

// ErrArtifactsNotRouted is this route saying it cannot answer a question
// rather than answering it wrongly.
//
// `backup-set remove` counts what stays on storage as a courtesy, off the
// journal, and over this route that count is GET /backups with the set in
// a query parameter. The client can make that call, since #544 added
// listArtifacts and the query support it needs. What nothing has written
// is the other half, an apicontract.Artifact turned back into a
// service.Artifact, and #544 did not need one: it asks the engine whether
// it holds the same backups and compares ids (readagreement.go), rather
// than asking it to render the rows this command prints.
//
// So the count is refused, not faked, and backupSetRemoveWith already
// knows what to do with a count it could not take: it says so, in place of
// printing a reassuring 0 about something nothing looked at. The removal
// itself is unaffected, which is the half the operator asked for.
var ErrArtifactsNotRouted = errors.New("this build does not count what stays on storage when a removal goes through a running engine")

// ListArtifacts is the read `backup-set remove` uses for its count.
func (r *engineRoute) ListArtifacts(_ context.Context, _ service.ArtifactFilter) ([]service.Artifact, error) {
	return nil, ErrArtifactsNotRouted
}

// backupSetFromWire turns the engine's answer back into the shape every
// command in this package already prints.
//
// Going back through core/service's own type rather than printing the wire
// shape is what keeps the two routes' output one thing: printBackupSet has
// one input and cannot grow a second rendering for the engine-attached
// case.
func backupSetFromWire(s apicontract.BackupSet) service.BackupSet {
	return service.BackupSet{
		ID:                  s.ID,
		SourceName:          s.SourceName,
		Name:                s.Name,
		Host:                s.Host,
		Port:                s.Port,
		User:                s.User,
		RemotePath:          s.RemotePath,
		LocalPath:           s.LocalPath,
		Include:             s.Include,
		CompletionStrategy:  s.CompletionStrategy,
		StableFor:           time.Duration(s.StableForSeconds) * time.Second,
		StaleAfter:          time.Duration(s.StaleAfterSeconds) * time.Second,
		ValidatorID:         service.ValidatorID(s.ValidatorID),
		Disabled:            s.Disabled,
		ReadOnly:            s.ReadOnly,
		RetentionIsOverride: s.RetentionIsOverride,
		// Issue #624. An engine older than this field answers false,
		// which reads as "nothing here says this set's connection was
		// skipped" rather than as a claim that it was proven, and that
		// is the same reading the configuration file's own absent key
		// gets.
		ConnectionUnverified: s.ConnectionUnverified,

		// Issue #845's two cadence fields, and the pair matters: the
		// override is nullable and the effective value is not, so a
		// route that carried only the second would report every set as
		// pinning an interval, and the next routed edit would write
		// that back as an explicit override.
		PollInterval:          durationPointerFromWireSeconds(s.PollIntervalSeconds),
		EffectivePollInterval: time.Duration(s.EffectivePollIntervalSeconds) * time.Second,
	}
}

// durationPointerFromWireSeconds keeps the contract's null ("this set
// inherits the deployment's cadence") distinct from a zero, which on this
// field is a request rather than a value.
func durationPointerFromWireSeconds(seconds *int) *time.Duration {
	if seconds == nil {
		return nil
	}
	d := time.Duration(*seconds) * time.Second
	return &d
}

// wireSeconds converts a duration to the whole seconds the contract
// carries, refusing anything that would not survive the trip.
//
// Refusing rather than rounding, and refusing HERE rather than letting the
// engine decide, because the failure this prevents is silent: a create with
// a sub-second window would persist one value through the engine and a
// different one directly, and both routes would report success.
func wireSeconds(d time.Duration, flag string) (int, error) {
	if d < 0 {
		return 0, fmt.Errorf("%s is negative, and the engine's API carries it as a whole number of seconds", flag)
	}
	if d%time.Second != 0 {
		return 0, fmt.Errorf("%s is %s, and the engine's API carries it as a whole number of seconds, so this would reach the engine as a different value from the one you typed; give it in whole seconds", flag, d)
	}
	return int(d / time.Second), nil
}

// Settings is GET /settings: the retention and capacity policy the engine
// is actually deciding with, resolved.
func (r *engineRoute) Settings(ctx context.Context) (service.Settings, error) {
	resp, err := r.client.GetSettings(ctx)
	if err != nil {
		return service.Settings{}, err
	}
	return settingsFromWire(resp), nil
}

// UpdateSettings is PATCH /settings.
//
// Every field crosses as a pointer, on both sides, and that is the load
// bearing part rather than a style. Zero is a MEANING in three of the four
// capacity fields ("no cap", "no warning line", "no critical line") and
// --protect-last-known-good's zero value is already true, so a mapping
// that read values instead of pointers would turn "leave this alone" into
// "set it to zero" and "turn FR-19 protection off" into "leave it on",
// both while reporting success. The CLI goes to the trouble of telling
// those apart through fs.Visit; this is where that would be thrown away.
func (r *engineRoute) UpdateSettings(ctx context.Context, req service.UpdateSettingsRequest) (service.Settings, error) {
	body := apicontract.UpdateSettingsRequest{
		AcknowledgeMediumDisclosure: req.AcknowledgeMediumDisclosure,
	}
	if req.Retention != nil {
		retention := apicontract.UpdateRetentionSettings{
			Timezone:             req.Retention.Timezone,
			WeekStartsOn:         req.Retention.WeekStartsOn,
			ProtectLastKnownGood: req.Retention.ProtectLastKnownGood,
		}
		// Tiers used to be nil from this CLI, which did not expose a
		// whole-chain replacement, and was mapped anyway so that the one
		// type doing the translating would not have a hole in it the day
		// something else filled that field in. That day is #595:
		// `settings patch --policy-file` fills it, and this line is why
		// the routed half of it needed nothing.
		for _, t := range req.Retention.Tiers {
			retention.Tiers = append(retention.Tiers, retentionTierToWire(t))
		}
		body.Retention = &retention
	}
	if req.Capacity != nil {
		body.Capacity = &apicontract.UpdateCapacitySettings{
			CapBytes:          req.Capacity.CapBytes,
			WarningFreeBytes:  req.Capacity.WarningFreeBytes,
			CriticalFreeBytes: req.Capacity.CriticalFreeBytes,
			SafetyMarginBytes: req.Capacity.SafetyMarginBytes,
		}
	}
	// Issue #845's service-behaviour section. A cadence is carried as
	// whole seconds like every other duration on this API, and one that
	// would not survive that is refused here rather than rounded: an
	// interval persisted as one value through the engine and another
	// directly is two deployments running different schedules while both
	// reported success.
	if req.Service != nil && req.Service.PollInterval != nil {
		seconds, err := wireSeconds(*req.Service.PollInterval, "--poll-interval")
		if err != nil {
			return service.Settings{}, err
		}
		body.Service = &apicontract.UpdateServiceSettings{PollIntervalSeconds: &seconds}
	}

	resp, err := r.client.UpdateSettings(ctx, body)
	if err != nil {
		return service.Settings{}, err
	}
	return settingsFromWire(resp), nil
}

// settingsFromWire turns the engine's answer back into the shape
// printSettings already renders, so the two routes cannot grow two
// renderings of one policy.
//
// SettingsResponse's schema block is deliberately dropped: service.Settings
// carries no equivalent, nothing this command prints reads it, and
// inventing a field to hold it would be this adapter deciding what
// core/service's type is for.
func settingsFromWire(s apicontract.SettingsResponse) service.Settings {
	out := service.Settings{
		Retention: service.RetentionSettings{
			Timezone:             s.Retention.Timezone,
			WeekStartsOn:         s.Retention.WeekStartsOn,
			ProtectLastKnownGood: s.Retention.ProtectLastKnownGood,
		},
		Capacity: service.CapacitySettings{
			CapBytes:             s.Capacity.CapBytes,
			WarningFreeBytes:     s.Capacity.WarningFreeBytes,
			CriticalFreeBytes:    s.Capacity.CriticalFreeBytes,
			SafetyMarginBytes:    s.Capacity.SafetyMarginBytes,
			BackupRoot:           s.Capacity.BackupRoot,
			BackupRootConfigured: s.Capacity.BackupRootConfigured,
		},
		// The deployment's own cadence, so a routed `settings show`
		// reports what the engine is actually running rather than a
		// zero this adapter invented by not mentioning the field.
		Service: service.ServiceSettings{
			PollInterval: time.Duration(s.Service.PollIntervalSeconds) * time.Second,
		},
	}
	for _, t := range s.Retention.Tiers {
		out.Retention.Tiers = append(out.Retention.Tiers, service.RetentionTier{
			Name:        t.Name,
			Granularity: t.Granularity,
			PeriodDays:  t.PeriodDays,
			Keep:        t.Keep,
			WindowUnit:  t.WindowUnit,
			Medium:      t.Medium,
		})
	}
	for _, m := range s.Mediums {
		out.Mediums = append(out.Mediums, storageMediumFromWire(m))
	}
	return out
}

func retentionTierToWire(t service.RetentionTier) apicontract.RetentionTier {
	return apicontract.RetentionTier{
		Name:        t.Name,
		Granularity: t.Granularity,
		PeriodDays:  t.PeriodDays,
		Keep:        t.Keep,
		WindowUnit:  t.WindowUnit,
		Medium:      t.Medium,
	}
}

// The storage-destination half of this route (G2.2, issue #594).
//
// It exists for the reason settingsRoute exists: `medium add`, `edit` and
// `remove` write configuration, and a configuration write left in the file
// beside a running engine is a change that process would never read
// (#538/#543). Without these, those three verbs would be permanently
// refused next to a live deployment, which is the position `settings
// patch` was in before #543.
//
// The import is the one that carries a secret, and it is the reason the
// CLI has no flag that takes one. The material is read from this
// process's stdin and put straight into a request body over the session
// this client already holds: never an argument, so never in the process
// table and never in shell history, and the response is an id, so nothing
// printed afterwards has anything to redact.

func (r *engineRoute) ImportStorageCredentials(ctx context.Context, accessKeyID, secretAccessKey, sessionToken string) (service.MediumCredentialRef, error) {
	resp, err := r.client.ImportStorageCredentials(ctx, apicontract.ImportStorageCredentialsRequest{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
		SessionToken:    sessionToken,
	})
	if err != nil {
		return service.MediumCredentialRef{}, err
	}
	// File is deliberately left empty. The engine wrote that path on its
	// own host, which may not be this one, and the API does not report it
	// for exactly that reason: an id is the whole of what a caller may
	// hold.
	return service.MediumCredentialRef{ID: resp.ID}, nil
}

func (r *engineRoute) ListStorageMediums(ctx context.Context) ([]service.StorageMediumSummary, error) {
	resp, err := r.client.ListStorageMediums(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.StorageMediumSummary, 0, len(resp.Mediums))
	for _, m := range resp.Mediums {
		out = append(out, storageMediumFromWire(m))
	}
	return out, nil
}

func (r *engineRoute) GetStorageMedium(ctx context.Context, id string) (service.StorageMediumSummary, error) {
	resp, err := r.client.GetStorageMedium(ctx, id)
	if err != nil {
		return service.StorageMediumSummary{}, err
	}
	return storageMediumFromWire(resp), nil
}

func (r *engineRoute) StorageMediumUsage(ctx context.Context, id string) (service.StorageMediumUsage, error) {
	resp, err := r.client.StorageMediumUsage(ctx, id)
	if err != nil {
		return service.StorageMediumUsage{}, err
	}
	out := service.StorageMediumUsage{Medium: resp.Medium, Placements: resp.Placements}
	for _, s := range resp.BackupSets {
		out.BackupSets = append(out.BackupSets, service.StorageMediumUsageBySet{
			Set: s.Set, Placements: s.Placements, OnlyCopyHere: s.OnlyCopyHere,
		})
	}
	return out, nil
}

func (r *engineRoute) PreflightStorageMediumCandidate(ctx context.Context, spec service.StorageMediumSpec) (service.MediumPreflight, error) {
	resp, err := r.client.PreflightStorageMediumCandidate(ctx, storageMediumToWire(spec))
	if err != nil {
		return service.MediumPreflight{}, err
	}
	return mediumPreflightFromWire(resp), nil
}

func (r *engineRoute) PreflightStorageMedium(ctx context.Context, id string) (service.MediumPreflight, error) {
	resp, err := r.client.PreflightStorageMedium(ctx, id)
	if err != nil {
		return service.MediumPreflight{}, err
	}
	return mediumPreflightFromWire(resp), nil
}

func (r *engineRoute) CreateStorageMedium(ctx context.Context, spec service.StorageMediumSpec) (service.StorageMediumSummary, error) {
	resp, err := r.client.CreateStorageMedium(ctx, storageMediumToWire(spec))
	if err != nil {
		return service.StorageMediumSummary{}, err
	}
	return storageMediumFromWire(resp), nil
}

func (r *engineRoute) UpdateStorageMedium(ctx context.Context, spec service.StorageMediumSpec) (service.StorageMediumSummary, error) {
	resp, err := r.client.UpdateStorageMedium(ctx, spec.ID, storageMediumToWire(spec))
	if err != nil {
		return service.StorageMediumSummary{}, err
	}
	return storageMediumFromWire(resp), nil
}

func (r *engineRoute) RemoveStorageMedium(ctx context.Context, id string) error {
	return r.client.RemoveStorageMedium(ctx, id)
}

func (r *engineRoute) SetDefaultStorageMedium(ctx context.Context, id string) (service.StorageMediumSummary, error) {
	resp, err := r.client.SetDefaultStorageMedium(ctx, id)
	if err != nil {
		return service.StorageMediumSummary{}, err
	}
	return storageMediumFromWire(resp), nil
}

// storageMediumFromWire is the one place the engine's answer becomes this
// binary's shape, shared by the settings read and by every medium verb, so
// the two cannot come to disagree about a destination.
func storageMediumFromWire(m apicontract.StorageMediumSummary) service.StorageMediumSummary {
	return service.StorageMediumSummary{
		ID:                  m.ID,
		Type:                m.Type,
		Bucket:              m.Bucket,
		Region:              m.Region,
		Endpoint:            m.Endpoint,
		Prefix:              m.Prefix,
		StorageClass:        m.StorageClass,
		UploadVerification:  m.UploadVerification,
		ReadsRequireRestore: m.ReadsRequireRestore,
		// H2.2's three (#622). Carried across rather than dropped for the
		// reason this function exists at all: `medium list` beside a
		// serving engine has to print what the engine's own settings page
		// shows, and a mapping that lost is_default would print a list
		// with no default in it, which is a list no deployment can
		// actually be in.
		Path:      m.Path,
		IsLocal:   m.IsLocal,
		IsDefault: m.IsDefault,
		// #636's mark, carried for the identical reason: `medium show`
		// beside a serving engine has to say what that engine's own
		// destinations card says, and a mapping that lost this would
		// print a destination as though it had been proven.
		ConnectionUnverified: m.ConnectionUnverified,
	}
}

// storageMediumToWire is the write direction. The credentials block is
// sent as the caller spelled it, EMPTY INCLUDED: on an edit the engine
// reads an unnamed credential as "keep the one already configured", and a
// mapping that invented a value here would rotate a credential nobody
// asked to rotate.
func storageMediumToWire(spec service.StorageMediumSpec) apicontract.StorageMediumRequest {
	return apicontract.StorageMediumRequest{
		ID:                 spec.ID,
		Type:               spec.Type,
		Region:             spec.Region,
		Endpoint:           spec.Endpoint,
		Bucket:             spec.Bucket,
		Prefix:             spec.Prefix,
		StorageClass:       spec.StorageClass,
		UploadVerification: spec.UploadVerification,
		Credentials: apicontract.StorageMediumCredentialsReference{
			CredentialsID: spec.Credentials.ID,
			File:          spec.Credentials.File,
			Env:           spec.Credentials.Env,
			Command:       spec.Credentials.Command,
		},
		// Issue #636's instruction, carried rather than dropped. A route
		// that lost it would send `medium add --no-verify` to an engine
		// as an ordinary create, which either refuses against a bucket
		// this host cannot reach or writes an unmarked destination: two
		// ways for one flag to mean nothing in the one mode where an
		// operator most needs it, which is the fleet.
		SkipConnectionCheck: spec.SkipConnectionCheck,
	}
}

// mediumPreflightFromWire carries every check through, skipped ones
// included. A route that dropped them would print a shorter list on a
// failure than on a success, which is the one moment the full list matters
// most.
func mediumPreflightFromWire(r apicontract.MediumPreflightResponse) service.MediumPreflight {
	out := service.MediumPreflight{Medium: r.Medium, OK: r.OK, Checks: make([]service.MediumPreflightCheck, 0, len(r.Checks))}
	for _, c := range r.Checks {
		out.Checks = append(out.Checks, service.MediumPreflightCheck{
			Step: c.Step, Outcome: c.Outcome, Category: c.Category, Detail: c.Detail,
		})
	}
	return out
}

// CreateRepositoryDomain is `repository create` routed at the engine
// (#862). The declaration crosses in the contract's spelling and the
// answer comes back as the domain's health, which is the shape the direct
// route answers with too, so the same printer renders both modes.
func (r *engineRoute) CreateRepositoryDomain(ctx context.Context, req service.CreateRepositoryDomainRequest) (service.RepositoryHealth, error) {
	resp, err := r.client.CreateRepositoryDomain(ctx, apicontract.CreateRepositoryDomainRequest{
		ID:          req.ID,
		Description: req.Description,
		Isolation:   req.Isolation,
		Passphrase: apicontract.RepositoryPassphraseReference{
			File:    req.Passphrase.File,
			Env:     req.Passphrase.Env,
			Command: req.Passphrase.Command,
		},
		Location:         req.Location,
		MaintenanceOwner: req.MaintenanceOwner,
	})
	if err != nil {
		return service.RepositoryHealth{}, err
	}
	return repositoryHealthFromWire(resp), nil
}

// repositoryHealthFromWire is the engine's verdict in this binary's own
// shape.
//
// The nullable skew keeps its two states, which is the whole reason it is
// nullable: null is "there is no durable timestamp to compare against
// yet", and zero is a clock that agrees exactly. Flattening them would
// make a brand-new deployment print a perfectly synchronised clock it has
// never measured.
func repositoryHealthFromWire(r apicontract.RepositoryHealth) service.RepositoryHealth {
	out := service.RepositoryHealth{
		Domain:                 r.Domain,
		MayShare:               r.MayShare,
		BackupSets:             r.BackupSets,
		State:                  r.State,
		Reachable:              r.Reachable,
		Readable:               r.Readable,
		Writable:               r.Writable,
		CredentialsValid:       r.CredentialsValid,
		ClockSane:              r.ClockSane,
		MaintenanceOverdue:     r.MaintenanceOverdue,
		LastMaintenanceAt:      wireInstant(r.LastMaintenanceAt),
		LastMaintenanceResult:  r.LastMaintenanceResult,
		LastSnapshotAt:         wireInstant(r.LastSnapshotAt),
		LastSnapshotStatus:     r.LastSnapshotStatus,
		LastVerificationAt:     wireInstant(r.LastVerificationAt),
		LastVerificationStatus: r.LastVerificationStatus,
		Detail:                 r.Detail,
	}
	if r.ClockSkewSeconds != nil {
		skew := time.Duration(*r.ClockSkewSeconds) * time.Second
		out.ClockSkew = &skew
	}
	return out
}

// wireInstant reads one RFC3339 timestamp, and reads an absent or
// unparseable one as the zero time -- which is exactly what every printer
// in this binary already renders as "never" or "none". A route that
// errored here would fail a create that had already succeeded.
func wireInstant(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// --- EPIC L's recovery and log reads over the wire (#813) -------------
//
// The four operations below are why apiclient carries workflow calls at
// all. `workflow recovery show`, `resume-cleanup`, `acknowledge` and
// `run log` used to open the LOCAL journal unconditionally, and for the
// two mutations that is not a slower path, it is a different one: a
// resume or an acknowledgement performed in a second process writes the
// journal and leaves the SERVING engine's in-memory refusal set
// untouched (workflowrun/recovery.go says in as many words that the
// refusal is in memory and the truth is on disk), so the backup set
// stayed blocked until somebody restarted the engine -- and two
// processes could resume the same run at once. `run log --follow` had
// the same shape one layer down: the broker that wakes a follower
// belongs to the process executing the run, so a local follow polled a
// journal nobody was writing.

// WorkflowRecovery is GET /workflow-recovery: the SERVING engine's own
// refusal set, which is what the next run will really be refused
// against.
func (r *engineRoute) WorkflowRecovery(ctx context.Context) ([]service.WorkflowRecoveryHold, error) {
	resp, err := r.client.WorkflowRecovery(ctx)
	if err != nil {
		return nil, err
	}

	holds := make([]service.WorkflowRecoveryHold, 0, len(resp.Holds))
	for _, h := range resp.Holds {
		holds = append(holds, service.WorkflowRecoveryHold{
			RunID:       h.RunID,
			BackupSetID: h.BackupSetID,
			Scope:       h.Scope,
			EnteredAt:   wireTime(h.EnteredAt),
			SpoolRef:    h.SpoolRef,
		})
	}

	return holds, nil
}

// ResumeWorkflowCleanup is POST /workflow-recovery/{run}/resume-cleanup.
func (r *engineRoute) ResumeWorkflowCleanup(ctx context.Context, runID string) (service.WorkflowRunDetail, error) {
	resp, err := r.client.ResumeWorkflowCleanup(ctx, runID)
	if err != nil {
		return service.WorkflowRunDetail{}, err
	}

	return workflowRunFromWire(resp), nil
}

// AcknowledgeWorkflowRecovery is POST
// /workflow-recovery/{run}/acknowledge.
//
// The ACTOR is dropped rather than sent, which is the routing working
// rather than something lost in it: the engine records whoever is signed
// in, and an acknowledgement's whole value six months later is that the
// name on it was established by the process that accepted it.
func (r *engineRoute) AcknowledgeWorkflowRecovery(ctx context.Context, runID string, ack service.WorkflowAcknowledgement) error {
	return r.client.AcknowledgeWorkflowRecovery(ctx, runID, apicontract.WorkflowAcknowledgementRequest{Reason: ack.Reason})
}

// WorkflowStepLogs is GET /workflow-runs/{run}/steps/{step}/logs.
//
// The wait is sent in whole SECONDS because that is what the route
// publishes, and a sub-second wait is rounded UP rather than truncated
// to zero: a zero never waits, which would silently turn a follow into a
// poll loop.
func (r *engineRoute) WorkflowStepLogs(ctx context.Context, req service.WorkflowStepLogRequest) (service.WorkflowStepLogPage, error) {
	wait := 0
	if req.Wait > 0 {
		wait = int((req.Wait + time.Second - 1) / time.Second)
	}

	page, err := r.client.WorkflowStepLogs(ctx, req.RunID, req.StepID, req.After, req.Limit, wait)
	if err != nil {
		return service.WorkflowStepLogPage{}, err
	}

	out := service.WorkflowStepLogPage{
		RunID:     page.RunID,
		StepID:    page.StepID,
		Cursor:    page.Cursor,
		Truncated: page.Truncated,
		Complete:  page.Complete,
		StepState: page.StepState,
	}
	for _, rec := range page.Records {
		out.Records = append(out.Records, service.WorkflowStepLogRecord{
			Seq:    rec.Seq,
			StepID: rec.StepID,
			Stream: rec.Stream,
			Kind:   rec.Kind,
			At:     wireTime(rec.At),
			Text:   rec.Text,
		})
	}

	return out, nil
}

// workflowRunFromWire translates one run row.
//
// Only the fields a CLI verb prints are carried, and the steps with it,
// because `resume-cleanup` prints the run in full afterwards: a resume
// that ran every hook and still could not account for a scope has to be
// readable from the row the next backup will be refused against.
func workflowRunFromWire(run apicontract.WorkflowRun) service.WorkflowRunDetail {
	out := service.WorkflowRunDetail{
		RunID:          run.RunID,
		BackupSetID:    run.BackupSetID,
		State:          run.State,
		BackupStatus:   run.BackupStatus,
		CleanupStatus:  run.CleanupStatus,
		WorkflowStatus: run.WorkflowStatus,
		RecoveryState:  run.RecoveryState,
		Bypassed:       run.Bypassed,
		FailedStep:     run.FailedStep,
		FailedScript:   run.FailedScript,
		StartedAt:      wireTime(run.StartedAt),
		DurationMillis: run.DurationMs,
		ScriptCount:    run.ScriptCount,
	}
	if finished := wireTime(run.FinishedAt); !finished.IsZero() {
		out.FinishedAt = &finished
	}
	for _, s := range run.Steps {
		out.Steps = append(out.Steps, service.WorkflowStepDetail{
			StepID:                 s.StepID,
			Order:                  s.Order,
			ScriptName:             s.ScriptName,
			Scope:                  s.Scope,
			Phase:                  s.Phase,
			Target:                 s.Target,
			State:                  s.State,
			ExecutionConnectionRef: s.ExecutionConnectionRef,
			ExitCode:               s.ExitCode,
			TerminationConfirmed:   s.TerminationConfirmed,
			DurationMillis:         s.DurationMs,
			TimeoutMillis:          s.TimeoutMs,
		})
	}

	return out
}

// wireTime parses an RFC 3339 instant the API published, and reports an
// unparseable or absent one as the zero time.
//
// Zero rather than an error, because every caller here is printing: a
// "finished: not yet" is exactly what a run with no finish time means,
// and a verb that refused to print a recovery hold because one timestamp
// was odd would be withholding the list an operator is trying to act on.
func wireTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}

	return at
}

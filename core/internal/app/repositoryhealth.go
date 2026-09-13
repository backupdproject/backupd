package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/internal/alert"
	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/health"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/repomaintenance"
	"github.com/backupdproject/backupd/core/internal/state"
)

// EPIC K's repository health (#788): the six questions an operator has
// about the store their snapshots live in, answered together.
//
// # Why this is not part of BuildHealthReport
//
// Because it costs a repository open per declared domain, and
// BuildHealthReport is what GET /api/v1/system/health, `backupd
// status` and every alerting tick are built on. Loading format blobs and
// an index cache on a surface a dashboard polls would turn a page refresh
// into storage traffic, and would do it on the deployments that have the
// most snapshots to index.
//
// So the probe is its own call, reached by the two surfaces that mean it
// -- GET /api/v1/repositories and `backupd repository health` -- and by
// the alerting pass, which runs on the poll cadence rather than on a page
// load. Callers that want the verdict inside a health.Report assign it
// there (health.Report.Repositories); nothing does it behind their back.
//
// # Why the probes are three questions and not one
//
// health.RepositoryHealth's own doc argues it: unreachable is a mount,
// unreadable is a wrong or empty mount, unwritable is a permission, and
// invalid credentials is a passphrase. One boolean would send an operator
// looking for all four.

// clockSkewTolerance is how far this process's clock may sit BEHIND the
// newest durable timestamp this deployment has already written before the
// clock is called insane.
//
// Behind and not ahead, and the asymmetry is the finding. A clock that is
// ahead writes timestamps into the future, which is ugly and orders
// correctly. A clock that is behind its own history re-uses instants that
// are already spent: two snapshots minutes apart get timestamps in the
// wrong order, retention buckets them into the wrong calendar periods,
// and the wrong one gets deleted. Five minutes is wide enough to absorb
// an ordinary NTP correction and narrow enough to catch a container that
// came up with no clock at all.
const clockSkewTolerance = 5 * time.Minute

// maintenanceOverdueAfter is how long a repository may go with no
// successful maintenance before the state says so.
//
// It is deliberately much longer than the engine's own quick cadence: a
// window that was skipped once is not an operator's problem, and a
// product that said so would train people to ignore it. A fortnight is
// long enough that every reasonable schedule has had many chances and
// short enough that the storage growth is still worth catching.
const maintenanceOverdueAfter = 14 * 24 * time.Hour

// RepositoryHealth probes every repository domain this configuration
// declares and reports each one's verdict, in declaration order.
//
// It never fails as a whole for one bad repository: a domain that could
// not be reached is a RESULT, and returning an error instead would let
// one unplugged NAS hide the health of every other repository in the
// deployment. The only error it returns is one that makes the whole
// question unanswerable.
func (s *Service) RepositoryHealth(ctx context.Context) ([]health.RepositoryHealth, error) {
	if s.Config == nil {
		return nil, errors.New("app: this service holds no configuration, so it can name no repository")
	}

	sets := s.setsByDomain()

	out := make([]health.RepositoryHealth, 0, len(s.Config.RepositoryDomains))
	for i := range s.Config.RepositoryDomains {
		declared := s.Config.RepositoryDomains[i]
		out = append(out, s.repositoryHealthOf(ctx, declared, sets[declared.Domain.ID]))
	}

	return out, nil
}

// RepositoryMaintenanceState is one repository's maintenance state, read
// from the durable ownership record and weighed against the schedule.
//
// It is the read half of #786 and it opens nothing: the record is a file
// this deployment writes beside its own state, so answering "when did
// maintenance last run" never needs the repository that maintenance is
// about -- which is exactly the case an operator asks in, because the
// repository is usually the thing that is not answering.
type RepositoryMaintenanceState struct {
	Metrics repomaintenance.Metrics

	// Decision is what the schedule says about right now.
	Decision repomaintenance.Decision

	// Overdue is the stronger claim: long enough with no successful
	// maintenance to be worth an operator's attention, rather than merely
	// eligible to run.
	Overdue bool

	// Recorded is whether any maintenance record exists at all. False is
	// a repository nothing has ever maintained, which reads as every
	// timestamp zero and must not be rendered as the epoch.
	Recorded bool
}

// ErrRepositoryDomainNotDeclared is what a maintenance read reports for a
// domain this configuration does not declare.
var ErrRepositoryDomainNotDeclared = errors.New("app: this configuration declares no repository domain of that id")

// RepositoryMaintenance reports one declared domain's maintenance state.
func (s *Service) RepositoryMaintenance(ctx context.Context, domain string) (RepositoryMaintenanceState, error) {
	if s.Config == nil {
		return RepositoryMaintenanceState{}, ErrRepositoryDomainNotDeclared
	}

	id, err := model.NewRepositoryDomainID(domain)
	if err != nil {
		return RepositoryMaintenanceState{}, fmt.Errorf("%w: %q", ErrRepositoryDomainNotDeclared, domain)
	}

	found := false
	for i := range s.Config.RepositoryDomains {
		if s.Config.RepositoryDomains[i].Domain.ID == id {
			found = true
			break
		}
	}
	if !found {
		return RepositoryMaintenanceState{}, fmt.Errorf("%w: %q", ErrRepositoryDomainNotDeclared, domain)
	}

	return s.maintenanceState(ctx, id), nil
}

// maintenanceState reads one domain's record and weighs it.
func (s *Service) maintenanceState(ctx context.Context, domain model.RepositoryDomainID) RepositoryMaintenanceState {
	record := s.maintenanceRecord(ctx, domain)
	if record == nil {
		return RepositoryMaintenanceState{
			Metrics:  repomaintenance.Metrics{Domain: domain},
			Decision: repomaintenance.Decision{Due: true, Reason: "no maintenance has ever been recorded for this repository"},
			Overdue:  false,
		}
	}

	now := s.now()
	state := RepositoryMaintenanceState{
		Metrics:  repomaintenance.Measure(*record),
		Decision: repomaintenance.Due(*record, repomaintenance.Intervals{}, now),
		Recorded: true,
	}
	state.Overdue = maintenanceOverdue(*record, now)

	return state
}

// maintenanceOverdue is the "worth telling somebody" line, which is not
// the schedule's own "may run now".
//
// A repository that has never had a FULL maintenance is overdue once it
// is old enough, because full maintenance is the only mode that reclaims
// storage: a repository with recent quick runs and no full run is growing
// and nothing in the quick cadence will ever stop it.
func maintenanceOverdue(record backupengine.MaintenanceOwnership, now time.Time) bool {
	last := record.LastFull
	if last.IsZero() {
		// Nothing has ever reclaimed anything here. That is overdue only
		// once the repository has existed long enough for it to matter,
		// and the newest evidence of that is the last quick run.
		if record.LastQuick.IsZero() {
			return false
		}

		return now.Sub(record.LastQuick) >= 0 && now.Sub(record.LastQuick) < maintenanceOverdueAfter
	}

	return now.Sub(last) >= maintenanceOverdueAfter
}

// repositoryHealthOf probes one declared domain.
func (s *Service) repositoryHealthOf(
	ctx context.Context,
	declared config.RepositoryDomainConfig,
	sets []config.BackupSet,
) health.RepositoryHealth {
	out := health.RepositoryHealth{
		Domain:   declared.Domain.ID.String(),
		MayShare: declared.Domain.Isolation != model.RepositoryIsolated,
		State:    health.Healthy,
	}
	for _, bs := range sets {
		out.BackupSets = append(out.BackupSets, bs.ID.String())
	}
	sort.Strings(out.BackupSets)

	s.attachCatalogFacts(ctx, &out, sets)

	maintenance := s.maintenanceState(ctx, declared.Domain.ID)
	out.MaintenanceOverdue = maintenance.Overdue
	out.LastMaintenanceAt = newestOf(maintenance.Metrics.LastFull, maintenance.Metrics.LastQuick)
	if maintenance.Recorded {
		out.LastMaintenanceResult = maintenanceResultWord(maintenance)
	}

	s.probeRepository(ctx, &out, declared, sets)
	decideRepositoryState(&out)

	return out
}

// maintenanceResultWord is the last window's outcome in one word an
// operator can scan a column of.
func maintenanceResultWord(state RepositoryMaintenanceState) string {
	if state.Metrics.Failing {
		return "failed"
	}
	if state.Metrics.Runs == 0 {
		return ""
	}

	return "succeeded"
}

// attachCatalogFacts fills in the two "what has actually happened here"
// answers and the clock comparison, all from the journal.
//
// They are journal reads rather than repository reads on purpose: they
// are true whether or not the repository is answering right now, which is
// exactly the moment somebody needs them.
func (s *Service) attachCatalogFacts(ctx context.Context, out *health.RepositoryHealth, sets []config.BackupSet) {
	catalog, ok := s.Journal.(snapshotCatalog)
	if !ok {
		return
	}

	var newest, newestVerified time.Time
	for _, bs := range sets {
		runs, err := catalog.ListSnapshotRuns(ctx, snapshotLineage(bs), maxSnapshotListing)
		if err != nil {
			continue
		}

		for _, run := range runs {
			at := run.UpdatedAt
			if at.IsZero() {
				at = run.StartedAt
			}

			if at.After(newest) {
				newest = at
				out.LastSnapshotAt = at
				out.LastSnapshotStatus = string(run.Phase)
			}

			if run.VerificationStatus != "" && at.After(newestVerified) {
				newestVerified = at
				out.LastVerificationAt = at
				out.LastVerificationStatus = run.VerificationStatus
			}
		}
	}

	out.ClockSane = true
	if newest.IsZero() {
		return
	}

	out.ClockSkewKnown = true
	out.ClockSkew = s.now().Sub(newest)
	if out.ClockSkew < -clockSkewTolerance {
		out.ClockSane = false
		out.Detail = fmt.Sprintf(
			"this machine's clock reads %s earlier than the newest snapshot this deployment has already recorded, so new snapshots would be timestamped before ones that already exist and retention would bucket them into the wrong periods",
			(-out.ClockSkew).Round(time.Second))
	}
}

// probeRepository asks the storage the three access questions.
//
// The order is the order the answers become available, and each failure
// stops the ladder rather than being guessed at: a location that will not
// open has not proved it is readable, and one that is not readable cannot
// have proved it is writable.
func (s *Service) probeRepository(
	ctx context.Context,
	out *health.RepositoryHealth,
	declared config.RepositoryDomainConfig,
	sets []config.BackupSet,
) {
	if s.Repositories == nil {
		out.Detail = appendDetail(out.Detail, "this build has no incremental backup engine wired, so no repository could be opened")
		return
	}

	loc, err := s.domainLocation(declared, sets)
	if err != nil {
		out.Detail = appendDetail(out.Detail, "this build cannot place this repository domain's storage, so nothing could be opened")
		return
	}

	repo, err := s.Repositories.OpenRepository(ctx, loc)
	if err != nil {
		switch {
		case errors.Is(err, backupengine.ErrPassphrase):
			// The storage answered; the secret did not fit. Reachable and
			// readable are both true of a repository whose format blob
			// was read and whose key was refused, and reporting them as
			// false would send an operator to check a mount that is fine.
			out.Reachable, out.Readable = true, false
			out.Detail = appendDetail(out.Detail, "this repository's declared passphrase does not open it, so no snapshot here can be read or written until the secret it references is corrected")
		case errors.Is(err, backupengine.ErrRepositoryNotFound):
			out.Reachable = true
			out.Detail = appendDetail(out.Detail, "the storage answered and holds no repository, which is what an empty or wrongly-mounted location looks like; nothing has been created here, deliberately")
		default:
			out.Detail = appendDetail(out.Detail, "this repository's storage could not be reached")
		}

		return
	}
	defer func() { _ = repo.Close(ctx) }()

	out.Reachable, out.Readable, out.CredentialsValid = true, true, true

	report, err := repo.Health(ctx)
	if err != nil {
		out.Detail = appendDetail(out.Detail, "this repository opened but its own health check did not complete, so nothing has proved it can still be written to")
		return
	}

	out.Writable = report.Reachable
	if !out.Writable {
		out.Detail = appendDetail(out.Detail, "this repository can be read and could not be written to, so the next backup into it will fail even though restores from it still work")
	}

	for _, w := range report.Warnings {
		switch w.Kind {
		case backupengine.HealthWarningClockSkew:
			out.ClockSane = false
			out.Detail = appendDetail(out.Detail, w.Detail)
		case backupengine.HealthWarningUnreachable:
			out.Reachable = false
			out.Detail = appendDetail(out.Detail, w.Detail)
		default:
			out.Detail = appendDetail(out.Detail, w.Detail)
		}
	}
}

// domainLocation is where one declared domain's repository lives.
//
// It is derived from a backup set that uses the domain when there is one,
// so that the one place a repository's location is computed stays
// repositoryLocation. A domain with no sets yet is placed the same way
// from the declaration alone: an operator who has declared a domain and
// not yet pointed anything at it should still be told whether its storage
// answers.
func (s *Service) domainLocation(declared config.RepositoryDomainConfig, sets []config.BackupSet) (backupengine.RepositoryLocation, error) {
	if len(sets) > 0 {
		return s.repositoryLocation(sets[0])
	}

	root := s.Config.EffectiveBackupRoot()
	if root == "" {
		return backupengine.RepositoryLocation{}, ErrRepositoryStorageUnsupported
	}

	return backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     declared.Domain.ID,
		Root:       root,
		Passphrase: declared.PassphraseRef,
	}, nil
}

// decideRepositoryState turns the probes into the one verdict a surface
// colours a row with.
//
// Failing is reserved for a repository that cannot take a backup at all,
// because that is the state that needs somebody now. Everything that
// still works but is worth attention -- overdue maintenance, a clock that
// has drifted, a verification that failed -- is Degraded, which is the
// same line internal/health already draws between the two.
func decideRepositoryState(out *health.RepositoryHealth) {
	switch {
	case !out.Reachable, !out.Readable, !out.CredentialsValid, !out.Writable:
		out.State = health.Failing
	case !out.ClockSane, out.MaintenanceOverdue, out.LastVerificationStatus == "failed":
		out.State = health.Degraded
	default:
		out.State = health.Healthy
	}

	if out.State == health.Degraded && out.Detail == "" {
		switch {
		case out.MaintenanceOverdue:
			out.Detail = "this repository has gone too long without a full maintenance window, so storage freed by deleted snapshots is still occupied. No restore point is affected."
		case out.LastVerificationStatus == "failed":
			out.Detail = "the newest verification of a snapshot in this repository failed, so at least one restore point could not be proven readable."
		}
	}
}

// appendDetail joins one more sentence onto a detail line.
func appendDetail(existing, add string) string {
	if strings.TrimSpace(add) == "" {
		return existing
	}
	if existing == "" {
		return add
	}

	return existing + " " + add
}

// newestOf is the later of two instants, treating zero as "never".
func newestOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}

	return b
}

// setsByDomain indexes every incremental backup set by the domain it
// stores snapshots in.
func (s *Service) setsByDomain() map[model.RepositoryDomainID][]config.BackupSet {
	out := make(map[model.RepositoryDomainID][]config.BackupSet)
	if s.Config == nil {
		return out
	}

	for _, src := range s.Config.Sources {
		for _, bs := range src.BackupSets {
			if bs.Engine != model.EngineKopia {
				continue
			}

			domain := bs.Repository.Domain
			if domain.IsZero() {
				continue
			}

			out[domain] = append(out[domain], bs)
		}
	}

	return out
}

// RepositoryAlertConditions is what this deployment's repositories mean
// to the notification path, in internal/alert's own vocabulary.
//
// Only conditions an operator can act on, and only ones the existing
// alert kinds already name: a repository whose maintenance is failing is
// alert.MaintenanceFailed, which repomaintenance already composes the
// sentence for. Nothing here invents a kind, because a kind is something
// an operator configures around and adding one silently is a change to
// their notification rules.
//
// A repository that is merely unreachable raises nothing here on purpose.
// The backup sets inside it go stale, which is the condition that already
// exists and already says the thing an operator has to do something
// about; a second alert for the same outage is how a product teaches
// people to filter its notifications.
func (s *Service) RepositoryAlertConditions(ctx context.Context) []alert.Condition {
	if s.Config == nil {
		return nil
	}

	var out []alert.Condition
	for i := range s.Config.RepositoryDomains {
		record := s.maintenanceRecord(ctx, s.Config.RepositoryDomains[i].Domain.ID)
		if record == nil {
			continue
		}

		out = append(out, repomaintenance.AlertConditions(*record)...)
	}

	return out
}

// snapshotRunsForDomain is every run this deployment recorded against one
// domain, newest first, bounded per set.
//
// Unused by the health probe itself, which walks sets directly; it is
// here because the metrics renderer and the CLI both want the same bound
// and a second spelling of it would drift.
func snapshotRunsForDomain(ctx context.Context, catalog snapshotCatalog, sets []config.BackupSet) []state.SnapshotRun {
	var out []state.SnapshotRun
	for _, bs := range sets {
		runs, err := catalog.ListSnapshotRuns(ctx, snapshotLineage(bs), maxSnapshotListing)
		if err != nil {
			continue
		}

		out = append(out, runs...)
	}

	return out
}

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
// load. A caller that wants the verdict inside a health.Report has to
// hold it itself: health.Report carries a process and its backup sets
// and no repository field at all, deliberately, because a repository
// does not go stale and a backup set's freshness verdict must not move
// because a NAS was asleep when somebody loaded a page (health.Report's
// own doc).
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

// repositoryProbeTimeout bounds one domain's storage probe.
//
// It exists because the failure this surface is FOR is the one that
// hangs. An unplugged NAS, a stale NFS mount and a bucket behind a
// dropped route do not refuse a connection; they accept one and never
// answer, and GET /repositories walks every declared domain in turn. So
// the read an operator opens to find out which repository is broken is
// exactly the read one broken repository would hold open forever.
//
// Thirty seconds rather than a few: opening a repository reads a format
// blob and can warm an index cache over a link somebody's backups
// travel, so a tight budget would report a working remote repository as
// unreachable, which is the same wrong answer in the other direction.
const repositoryProbeTimeout = 30 * time.Second

// RepositoryHealth probes every repository domain this configuration
// declares and reports each one's verdict, in declaration order.
//
// It never fails as a whole for one bad repository: a domain that could
// not be reached is a RESULT, and returning an error instead would let
// one unplugged NAS hide the health of every other repository in the
// deployment. The only errors it returns are the two that make the whole
// question unanswerable: no configuration at all, and EPIC K's
// production gate being shut (#789).
//
// The gate is an error rather than a row per domain with a "disabled"
// verdict, and that is the honest shape. health.RepositoryHealth's
// states say whether a repository can take a backup; with the engine
// disabled nothing may open one, so every field would be a guess. An
// operator asking this question gets the one answer that is true and
// actionable -- the engine is off, here is the flag -- rather than six
// booleans nobody measured.
func (s *Service) RepositoryHealth(ctx context.Context) ([]health.RepositoryHealth, error) {
	if s.Config == nil {
		return nil, errors.New("app: this service holds no configuration, so it can name no repository")
	}

	if err := s.incrementalEngineGate(); err != nil {
		return nil, err
	}

	sets := s.setsByDomain()

	out := make([]health.RepositoryHealth, 0, len(s.Config.RepositoryDomains))
	for i := range s.Config.RepositoryDomains {
		declared := s.Config.RepositoryDomains[i]
		out = append(out, s.repositoryHealthOf(ctx, declared, sets[declared.Domain.ID]))
	}

	return out, nil
}

// MaintenanceOwnerOf reports which instance the durable record says
// maintains one repository, and "" when nothing has ever maintained it.
//
// Unlike RepositoryMaintenance below, it does NOT require the domain to
// be declared, and that is the whole point of it: the caller is the
// create route, which asks before the declaration exists (#862). A
// record for an undeclared id is not a contradiction -- it is what a
// re-declared id, or a state directory shared with a second instance,
// leaves behind, and it is exactly the case ADR 0017 says a declaration
// must not quietly overwrite by becoming a claim.
//
// It opens no repository: the record is a file this deployment writes
// beside its own state. An unreadable record reads as no record, on the
// same terms maintenanceRecord already sets, and the create's refusal is
// therefore never raised on the strength of something this deployment
// could not read.
func (s *Service) MaintenanceOwnerOf(ctx context.Context, domain string) string {
	if s.Config == nil {
		return ""
	}

	id, err := model.NewRepositoryDomainID(domain)
	if err != nil {
		return ""
	}

	record := s.maintenanceRecord(ctx, id)
	if record == nil {
		return ""
	}

	return record.Owner.String()
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
		// and the newest evidence of that is the last quick run: a quick
		// cadence that has been running for longer than the budget is a
		// repository that has had every chance at a full window and has
		// not taken one.
		//
		// The comparison is deliberately this way round. Reading it as
		// "the last quick run is RECENT" would report a repository whose
		// maintenance is working perfectly as overdue, and a repository
		// nobody has touched for a month as fine, which is the one
		// reading that is wrong in both directions at once.
		if record.LastQuick.IsZero() {
			return false
		}

		return now.Sub(record.LastQuick) >= maintenanceOverdueAfter
	}

	return now.Sub(last) >= maintenanceOverdueAfter
}

// repositoryHealthOf probes one declared domain.
func (s *Service) repositoryHealthOf(
	ctx context.Context,
	declared config.RepositoryDomainConfig,
	sets []config.BackupSet,
) health.RepositoryHealth {
	// One bounded window per domain, because the probe below opens real
	// storage and GET /repositories walks every declared domain in
	// sequence. A mount that has gone away does not refuse a connection,
	// it hangs: without this, one asleep NAS holds the whole response
	// open, and the surface an operator loads to find out WHICH
	// repository is broken is the one surface that never answers. The
	// deadline is per domain rather than over the whole walk so that a
	// slow first domain cannot consume every later domain's budget and
	// report a shelf of healthy repositories as unreachable.
	ctx, cancel := context.WithTimeout(ctx, repositoryProbeTimeout)
	defer cancel()

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
			out.Reachable, out.Readable = true, true
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
		case backupengine.HealthWarningUnreachable:
			out.Reachable = false
		}

		out.Detail = appendDetail(out.Detail, repositoryWarningSentence(w.Kind))
	}
}

// repositoryWarningSentence is this product's own sentence for one engine
// warning kind.
//
// The engine's own Detail is deliberately NOT carried through, and that
// is the whole point of this function rather than a stylistic
// preference. backupengine.HealthWarning.Detail is composed by an
// adapter, and one of the three kinds passes the underlying error's text
// straight through (kopia's repository.go hands over
// r.engineRetention.Error()), which names the repository's own
// filesystem path. health.RepositoryHealth.Detail is rendered on a
// dashboard and pasted into support conversations, and its own doc
// promises it carries no path, endpoint or credential -- a promise
// nothing kept while the engine's string was echoed.
//
// A kind with no sentence here still says something rather than nothing:
// dropping a condition the engine went to the trouble of reporting would
// be worse than naming it generically, and the closed kind set means a
// new kind arriving without a sentence is a one-line change rather than a
// silent leak.
func repositoryWarningSentence(kind backupengine.HealthWarningKind) string {
	switch kind {
	case backupengine.HealthWarningClockSkew:
		return "this machine's clock and this repository's storage disagree by more than a repository tolerates, so maintenance locks and content-retention windows are being reasoned about with two different ideas of the time; fix time synchronisation on this host"
	case backupengine.HealthWarningUnreachable:
		return "this repository's storage stopped answering while it was being checked, so nothing here has been proven readable or writable"
	case backupengine.HealthWarningEngineRetention:
		return "this repository's own stored retention policy still expires snapshots and this deployment could not correct it, which is what a read-only mount, an object lock or a legal hold looks like. Every snapshot this product writes here is pinned and safe; one written into the same repository by other software is not"
	default:
		return "this repository reported a condition this build has no sentence for, which is worth an operator's attention and is recorded in this deployment's log"
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
// Two conditions, from two different sources, and the split is the point.
// A repository whose maintenance is failing is alert.MaintenanceFailed,
// read from the durable ownership record -- a file beside this
// deployment's own state, so that question is answerable while the
// repository is the very thing not answering. A repository that cannot
// take a backup at all is alert.RepositoryUnavailable, and that one can
// only come from a probe.
//
// # Why the probe runs here, on the alerting cadence
//
// #788 left this pass record-only and said so: "the probes that DO open
// a repository deliberately raise nothing here", on the argument that a
// repository that has gone away makes its backup sets go stale and
// StaleBackup already covers it. #789 closes that: staleness is a
// days-long window, RepeatedFailure's count arm counts FAILED artifacts
// an incremental set never produces, and so the fast signal for "every
// backup into this repository is failing right now" was missing
// entirely. alert.RepositoryUnavailable's own doc has the full argument.
//
// The cost is one repository open per declared domain per alerting pass,
// and it is affordable for a reason worth writing down: this deployment
// ALREADY opens each of those repositories on that same cadence, once
// per incremental set, because that is what a processing cycle does. One
// extra open per domain is the same order as the work already happening,
// and it is what buys an alert that arrives before the next cycle rather
// than a day later.
//
// A gated deployment is silent here (#789's production flag): with the
// engine disabled nothing may open a repository, so there is nothing to
// probe and nothing that could be failing. That silence is correct
// rather than convenient -- an operator who turned the engine off must
// not be notified about the store it is not using -- and it is the
// reason this returns nil rather than passing the gate's error up: a
// gate is not an incident.
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

	// RepositoryHealth refuses as a whole exactly twice: no
	// configuration, and the gate. Both are "there is nothing to alert
	// about", never a condition of their own, so the maintenance
	// conditions above stand and this half adds nothing.
	reports, err := s.RepositoryHealth(ctx)
	if err != nil {
		return out
	}

	for _, report := range reports {
		out = append(out, alert.RepositoryConditions(report.Domain, report)...)
	}

	return out
}

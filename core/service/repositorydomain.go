package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/health"
	"github.com/backupdproject/backupd/core/internal/model"
)

// Declaring a repository domain (issue #862): the write half of EPIC K's
// repository surface, which until now had only reads.
//
// # What a create actually creates
//
// A declaration, and nothing else. A repository domain is a boundary --
// one encryption key, one credential, one maintenance owner, one
// deduplication pool, one corruption blast radius -- and the kopia store
// underneath it is realized LAZILY, by the first backup run that stores a
// snapshot in it. That is not a shortcut taken here: it is exactly what
// already happens for a domain named on the add-backup-set wizard's
// repository step, and making this route eager would give the product two
// different lifecycles for one noun, one of which can fail halfway and
// leave a repository nothing declares.
//
// So this method opens no storage, resolves no passphrase reference and
// runs no probe -- not even to describe what it just wrote. It answers
// with the domain as the DECLARATION describes it: an unrealized
// repository, reported honestly rather than dressed up, and with no
// claim about storage nothing has looked at. See
// declaredRepositoryHealth for why a probing read-back would have been
// the route granting itself the two capabilities it is exempt from the
// destructive gate for not having.
//
// # Why it is a *BackupService method
//
// Because it rewrites config.yaml, and core/service/setup.go's rule is
// that every route that rewrites config.yaml is one of these: the file is
// re-read fresh under configMu, the new entry is folded in, and the
// service hot-reloads through the same encode / validate / write /
// adopt tail every other configuration write in this package shares
// (persistConfig, mediums.go). A second door would be a second idea of
// what the file currently says.
//
// # What it refuses, and what refuses it
//
// The gate (#789) and the duplicate id are asked HERE, against the file
// this method just re-read, because both are facts about the
// configuration the write is based on rather than about the request. The
// shape of the domain itself -- the id, the isolation word, the
// passphrase reference naming exactly one source -- is refused by
// config.Validate inside persistConfig, so this method holds no second
// copy of rules that live in internal/config. The one rule that is
// neither is ADR 0017's: see the ownership check below.

// RepositoryPassphraseRef names where a repository domain's encryption
// passphrase comes from: a file, an environment variable, or a command
// whose stdout is the secret.
//
// It is a reference in all three spellings and there is deliberately no
// field for the passphrase itself. A domain's passphrase is the only
// thing standing between its storage and everything this product holds;
// a value that could be submitted here would be one sitting in the clear
// in config.yaml beside the hostnames it protects, which is the exposure
// the SSH key's own reference-only rule (#298) exists to close.
type RepositoryPassphraseRef struct {
	File    string
	Env     string
	Command []string
}

func (p RepositoryPassphraseRef) isZero() bool {
	return p.File == "" && p.Env == "" && len(p.Command) == 0
}

// CreateRepositoryDomainRequest is one repository domain as a caller
// declares it.
type CreateRepositoryDomainRequest struct {
	// ID is how a backup set will name this domain. Unique across the
	// domains this deployment declares.
	ID string

	// Description is the operator's own sentence about what this domain
	// holds. Never interpreted.
	Description string

	// Isolation is "shared" or "isolated", and it is required: neither
	// default is acceptable, for the reason config's own field says at
	// length.
	Isolation string

	// Passphrase is where the key that opens this repository comes from.
	Passphrase RepositoryPassphraseRef

	// Location is where the repository is stored. Empty means this
	// deployment's own storage location, which is the only place this
	// build can put one; anything else is refused rather than ignored.
	Location string

	// MaintenanceOwner is "this" or "another-instance", and empty means
	// "this". It declares nothing durable -- see the ADR 0017 check in
	// CreateRepositoryDomain for what it does and why it cannot do more.
	MaintenanceOwner string
}

// The two maintenance-owner answers this request understands.
const (
	maintenanceOwnerThis    = "this"
	maintenanceOwnerAnother = "another-instance"
)

// ErrRepositoryDomainExists is what a create reports for an id this
// configuration already declares.
//
// Its own sentinel rather than ErrInvalidRequest, on the same reasoning
// ErrStorageMediumExists is: the request is well-formed and the only
// thing wrong with it is the state of this deployment, so a client
// reading INVALID_REQUEST would send an operator back to a form that is
// already correct. The remedy is a different id, or the existing domain.
var ErrRepositoryDomainExists = errors.New("service: this deployment already declares a repository domain of that id")

// ErrRepositoryDomainMaintainedElsewhere is ADR 0017's refusal at the one
// moment a declaration could break it.
//
// Maintenance ownership is a durable record: it is taken by whichever
// instance first maintains an unclaimed repository, and after that it
// moves only through Transfer, which takes the owner the administrator
// believes holds it and refuses if the record says otherwise. A
// declaration naming THIS deployment as the maintainer of a repository
// whose record already names somebody else is precisely the "claim this
// for me" operation that design exists to make impossible, so it is
// refused here rather than silently written and discovered when two
// instances compact one store.
var ErrRepositoryDomainMaintainedElsewhere = errors.New("service: another instance already holds maintenance for that repository")

// CreateRepositoryDomain declares a repository domain in the
// configuration file this BackupService was opened from and hot-reloads
// this service, so the domain is immediately nameable by a backup set.
//
// It creates no store. See this file's own doc for why that is the same
// lifecycle an implicitly named domain already has, and what each refusal
// below is about.
func (b *BackupService) CreateRepositoryDomain(ctx context.Context, req CreateRepositoryDomainRequest) (RepositoryHealth, error) {
	if b.configPath == "" {
		return RepositoryHealth{}, ErrConfigNotFileBacked
	}

	owner, err := maintenanceOwnerOf(req.MaintenanceOwner)
	if err != nil {
		return RepositoryHealth{}, err
	}
	if err := repositoryDomainIDIsWellFormed(req.ID); err != nil {
		return RepositoryHealth{}, err
	}
	// Asked here rather than left to config.Validate because config has
	// no field for it: a domain declared with no passphrase at all is a
	// legal configuration (an operator building a boundary up before
	// pointing anything at it), and the refusal an API caller would
	// otherwise meet is the one raised about the BACKUP SET that names
	// the domain, later, somewhere else. A create that cannot say "this
	// domain needs a passphrase" would be a create whose result is a
	// domain nothing may use.
	if req.Passphrase.isZero() {
		return RepositoryHealth{}, fmt.Errorf(
			"%w: a repository domain needs a passphrase reference (file, env or command); a repository this product creates is always encrypted",
			ErrInvalidRequest)
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	// Re-read from disk rather than from b.state, the same "always read
	// fresh" discipline CreateBackupSet and every medium write document:
	// the write below is based on the file's actual current content, so
	// a domain added by hand, or by a second process, since this service
	// loaded is not lost by a create that does not touch it.
	cfg, err := config.Load(b.configPath)
	if err != nil {
		return RepositoryHealth{}, fmt.Errorf("service: re-reading configuration: %w", err)
	}

	// EPIC K's production gate (#789), read from the revision this write
	// is based on. A domain declared into a deployment that does not run
	// the incremental engine is a boundary nothing could ever open, and
	// the operator would have configuration they cannot use and no
	// sentence saying why.
	if err := refuseGatedIncrementalEngine(cfg); err != nil {
		return RepositoryHealth{}, err
	}

	id := strings.TrimSpace(req.ID)
	for i := range cfg.RepositoryDomains {
		if cfg.RepositoryDomains[i].ID == id {
			return RepositoryHealth{}, fmt.Errorf("%w: %s", ErrRepositoryDomainExists, id)
		}
	}

	if err := refuseForeignLocation(cfg, req.Location); err != nil {
		return RepositoryHealth{}, err
	}

	// ADR 0017. Asked before anything is written, and only for the
	// answer that would be a claim: an operator who has said another
	// instance maintains this store is telling this deployment exactly
	// what the record says, and declaring the boundary anyway is the
	// supported way to point a backup set at a repository somebody else
	// looks after.
	if owner == maintenanceOwnerThis {
		if held := b.state.Load().inner.MaintenanceOwnerOf(ctx, id); held != "" {
			return RepositoryHealth{}, fmt.Errorf(
				"%w: %s holds maintenance for %s. Ownership moves by transfer, never by declaration (ADR 0017), so declaring this domain cannot make this deployment its maintainer; re-send it with maintenance_owner=%s to declare the boundary without claiming it",
				ErrRepositoryDomainMaintainedElsewhere, held, id, maintenanceOwnerAnother)
		}
	}

	cfg.RepositoryDomains = append(cfg.RepositoryDomains, config.RepositoryDomainConfig{
		ID:          id,
		Description: strings.TrimSpace(req.Description),
		Isolation:   strings.TrimSpace(req.Isolation),
		Passphrase: config.Passphrase{
			File:    req.Passphrase.File,
			Env:     req.Passphrase.Env,
			Command: req.Passphrase.Command,
		},
	})

	// The shared encode / validate / write / hot-reload tail. Everything
	// about the domain's own shape -- the id, the isolation word, the
	// passphrase naming exactly one source -- is config.Validate's to
	// refuse, and it comes back as ErrInvalidRequest with that package's
	// own sentence, which is built from its field descriptions and the
	// caller's own submitted values (a passphrase REFERENCE is a
	// submitted value; the passphrase is not, and there is no field here
	// one could have arrived in).
	if err := b.persistConfig(cfg); err != nil {
		return RepositoryHealth{}, err
	}

	return declaredRepositoryHealth(id, strings.TrimSpace(req.Isolation)), nil
}

// declaredRepositoryHealth is the 201's body: one repository domain as
// the declaration that just landed describes it, and nothing else.
//
// # Why a create does not probe
//
// Because probing would make the declaration do the two things this
// route promises not to do. RepositoryHealth is normally produced by
// opening the store (internal/app.probeRepository), and opening a store
// resolves the domain's passphrase reference -- which for the `command`
// spelling means EXECUTING a program the request named -- and then
// connects to storage. On a route that is CSRF-checked but deliberately
// exempt from the destructive gate, on the stated grounds that declaring
// a boundary "opens no storage, proves no passphrase" and cannot reach a
// backup datum, that is not a detail: it is the route quietly acquiring
// the two capabilities its exemption was granted for not having.
//
// It is also a liveness problem. The probe would run under configMu,
// bounded only by the repository probe timeout, so one unreachable store
// or one passphrase command that hangs would stall every other
// configuration write in this process for as long as it took.
//
// # What the answer therefore says
//
// The two facts the declaration itself establishes -- the id and the
// co-tenancy posture, MayShare derived exactly as internal/app derives
// it -- and no claim about storage at all. Every probe boolean stays
// false because nothing was probed, which is what false means here, and
// the verdict is DEGRADED: a domain with no store yet is not HEALTHY,
// and FAILING is reserved for a repository something has actually found
// to be unusable. The detail says which of the two it is, because
// "declared, nothing realized" and "probed and broken" have completely
// different remedies and only the fleet read can report the second.
func declaredRepositoryHealth(id, isolation string) RepositoryHealth {
	return RepositoryHealth{
		Domain:   id,
		MayShare: model.RepositoryIsolation(isolation) != model.RepositoryIsolated,
		State:    health.Degraded.String(),
		Detail: "this repository domain is declared and its store has not been created yet: it is written by the first backup run that stores a snapshot here, " +
			"so nothing above is a reading of storage. GET /repositories probes it from then on",
	}
}

// maintenanceOwnerOf resolves the request's owner word, defaulting an
// empty one to this deployment.
//
// Empty means "this" rather than being refused, unlike isolation, and the
// asymmetry is deliberate: co-tenancy has no safe default, while "the
// instance that declares a repository maintains it" is what the product
// already does with an unclaimed repository, so silence there has a true
// meaning. A word that is neither is refused rather than read as either.
func maintenanceOwnerOf(word string) (string, error) {
	switch strings.TrimSpace(word) {
	case "":
		return maintenanceOwnerThis, nil
	case maintenanceOwnerThis:
		return maintenanceOwnerThis, nil
	case maintenanceOwnerAnother:
		return maintenanceOwnerAnother, nil
	default:
		return "", fmt.Errorf("%w: maintenance owner %q is neither %q nor %q",
			ErrInvalidRequest, word, maintenanceOwnerThis, maintenanceOwnerAnother)
	}
}

// refuseForeignLocation refuses a location that is not this deployment's
// own storage location.
//
// The configuration has no per-domain location key: where a domain's
// bytes live is derived from the deployment's backup root, for every
// domain it declares. A request naming somewhere else is therefore
// something this build cannot honour, and it is refused rather than
// accepted and dropped -- a field that silently did nothing is how an
// operator ends up believing their second copy is off site.
func refuseForeignLocation(cfg *config.Config, location string) error {
	wanted := strings.TrimSpace(location)
	if wanted == "" {
		return nil
	}

	root := cfg.EffectiveBackupRoot()
	if wanted == root {
		return nil
	}

	if root == "" {
		return fmt.Errorf(
			"%w: this deployment cannot yet say where a repository would be stored, so it cannot honour a location; leave it empty and the domain is stored under this deployment's own storage location once there is one",
			ErrInvalidRequest)
	}

	return fmt.Errorf(
		"%w: this deployment stores every repository domain it declares under %s, and a per-domain storage location is not part of its configuration; leave the location empty to declare the domain there",
		ErrInvalidRequest, root)
}

// repositoryDomainIDIsWellFormed is model's rule, asked before the file
// is touched so a malformed id is refused as a request rather than as a
// validation failure over a configuration nobody submitted.
//
// It is not a second copy of the rule: it calls the same constructor
// config.Validate does.
func repositoryDomainIDIsWellFormed(id string) error {
	if _, err := model.NewRepositoryDomainID(id); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	return nil
}

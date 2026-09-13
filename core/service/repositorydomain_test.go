// Declaring a repository domain from a request rather than by hand
// editing config.yaml (issue #862).
//
// Every persistence assertion here reads the domain back through a
// SECOND, independently opened service, and that is the whole discipline
// of this file. A create that returned the right struct and never
// reached the disk passes any test that only looks at what it returned,
// and the failure it hides is the one that matters: a domain an operator
// declared, saw on screen, pointed a backup set at, and lost on the next
// restart.
//
// The passphrase case is a canary case for the same reason the storage
// medium's credential one is. A domain's passphrase is the only thing
// standing between its storage and everything this product holds, so the
// test reads the written bytes and searches them for the secret itself,
// rather than asserting that the field the service happened to fill in
// looks like a reference.

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/model"
)

// testRepositoryPassphrase is obviously fake and is written only into a
// file the test's own temp directory owns. Nothing may copy it into
// config.yaml: the declaration names the FILE.
const testRepositoryPassphrase = "EXAMPLE-REPOSITORY-PASSPHRASE-NOT-A-REAL-ONE"

// passphraseFile writes the canary somewhere this deployment can read it
// and returns the path the declaration will reference.
func passphraseFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "domain.passphrase")
	if err := os.WriteFile(path, []byte(testRepositoryPassphrase), 0o600); err != nil {
		t.Fatalf("writing the passphrase file: %v", err)
	}
	return path
}

// reopen is the second, independent load. It opens the same config file
// in a new service, so what it reports came off the disk rather than out
// of the service that wrote it.
func reopen(t *testing.T, configPath string) *BackupService {
	t.Helper()
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("re-opening %s: %v", configPath, err)
	}
	t.Cleanup(func() { _ = cleanup() })
	return svc
}

// declaredDomains is every domain a service reports on the fleet read,
// by id.
func declaredDomains(t *testing.T, svc *BackupService) []string {
	t.Helper()
	report, err := svc.ListRepositories(context.Background())
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	out := make([]string, 0, len(report.Repositories))
	for _, r := range report.Repositories {
		out = append(out, r.Domain)
	}
	return out
}

func validDomainRequest(t *testing.T) CreateRepositoryDomainRequest {
	t.Helper()
	return CreateRepositoryDomainRequest{
		ID:          "offsite-b2",
		Description: "Second copy, off site",
		Isolation:   "isolated",
		Passphrase:  RepositoryPassphraseRef{File: passphraseFile(t)},
	}
}

// The acceptance criterion: the declaration is on disk, the service that
// wrote it has hot-reloaded, and a process that has never seen this
// request reports the domain.
func TestCreateRepositoryDomain_PersistsTheDeclarationASecondLoadCanSee(t *testing.T) {
	svc := openRestoreTestService(t)
	configPath := svc.configPath

	created, err := svc.CreateRepositoryDomain(context.Background(), validDomainRequest(t))
	if err != nil {
		t.Fatalf("CreateRepositoryDomain: %v", err)
	}
	if created.Domain != "offsite-b2" {
		t.Errorf("the created domain reports id %q, want offsite-b2", created.Domain)
	}
	if created.MayShare {
		t.Error("an isolated domain reports may_share true, so the co-tenancy posture did not survive the write")
	}

	// The write itself.
	raw, err := os.ReadFile(configPath) //nolint:gosec // a path this test created.
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "id: offsite-b2") {
		t.Fatalf("config.yaml does not declare the domain:\n%s", raw)
	}

	// The hot reload, in the service that did the write.
	if !contains(declaredDomains(t, svc), "offsite-b2") {
		t.Error("the writing service does not report the new domain, so nothing hot-reloaded")
	}

	// And the second, independent load, which is what makes this a
	// persistence test rather than an echo test.
	if got := declaredDomains(t, reopen(t, configPath)); !contains(got, "offsite-b2") {
		t.Errorf("a second service opened on the same file reports %v, which does not include the created domain", got)
	}
}

// FR-33's rule for the one secret this noun has: the file is referenced,
// never read into the configuration.
func TestCreateRepositoryDomain_PersistsAPassphraseReferenceAndNotTheSecret(t *testing.T) {
	svc := openRestoreTestService(t)
	configPath := svc.configPath

	req := validDomainRequest(t)
	if _, err := svc.CreateRepositoryDomain(context.Background(), req); err != nil {
		t.Fatalf("CreateRepositoryDomain: %v", err)
	}

	raw, err := os.ReadFile(configPath) //nolint:gosec // a path this test created.
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, testRepositoryPassphrase) {
		t.Fatalf("the passphrase itself is in config.yaml:\n%s", text)
	}
	if !strings.Contains(text, "file: "+req.Passphrase.File) {
		t.Errorf("config.yaml carries no passphrase.file reference to %s:\n%s", req.Passphrase.File, text)
	}
}

// EPIC K's production gate (#789). A domain declared on a deployment
// that does not run the engine is a boundary nothing could ever open.
func TestCreateRepositoryDomain_IsRefusedWhenTheIncrementalEngineIsGatedOff(t *testing.T) {
	svc := openRestoreTestService(t)
	configPath := svc.configPath
	before := readFileForTest(t, configPath)

	t.Setenv(config.IncrementalEngineEnvVar, "0")

	_, err := svc.CreateRepositoryDomain(context.Background(), validDomainRequest(t))
	if !errors.Is(err, ErrIncrementalEngineDisabled) {
		t.Fatalf("CreateRepositoryDomain on a gated deployment = %v, want ErrIncrementalEngineDisabled", err)
	}
	if after := readFileForTest(t, configPath); after != before {
		t.Error("a refused create changed config.yaml")
	}
}

// Two entries claiming one id are two boundaries with one name, and
// whichever a backup set means, the other is silently not in force.
func TestCreateRepositoryDomain_RefusesADuplicateIdAndWritesNothing(t *testing.T) {
	svc := openRestoreTestService(t)
	configPath := svc.configPath
	before := readFileForTest(t, configPath)

	req := validDomainRequest(t)
	// "production" is the domain openRestoreTestService's fixture
	// already declares and its incremental set already stores in.
	req.ID = "production"

	_, err := svc.CreateRepositoryDomain(context.Background(), req)
	if !errors.Is(err, ErrRepositoryDomainExists) {
		t.Fatalf("CreateRepositoryDomain over a declared id = %v, want ErrRepositoryDomainExists", err)
	}
	if after := readFileForTest(t, configPath); after != before {
		t.Fatal("a refused duplicate rewrote config.yaml, so the file an operator depends on was touched by a request that failed")
	}
	// And the set that stores in the existing domain is still stored in
	// a domain that opens: a corrupted file would show up here first.
	if got := declaredDomains(t, reopen(t, configPath)); len(got) != 1 || got[0] != "production" {
		t.Errorf("a second load reports domains %v, want exactly [production]", got)
	}
}

// The shape refusals, each one a request an operator can fix, and none
// of them a 500.
func TestCreateRepositoryDomain_RefusesRequestsConfigWouldNotAccept(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutum func(*CreateRepositoryDomainRequest)
	}{
		{"no id at all", func(r *CreateRepositoryDomainRequest) { r.ID = "" }},
		{"an id carrying a separator", func(r *CreateRepositoryDomainRequest) { r.ID = "prod/main" }},
		{"an isolation outside the two words", func(r *CreateRepositoryDomainRequest) { r.Isolation = "private" }},
		{"no isolation, which is not a default", func(r *CreateRepositoryDomainRequest) { r.Isolation = "" }},
		{"no passphrase source at all", func(r *CreateRepositoryDomainRequest) { r.Passphrase = RepositoryPassphraseRef{} }},
		{"two passphrase sources", func(r *CreateRepositoryDomainRequest) { r.Passphrase.Env = "BACKUPD_DOMAIN_PASSPHRASE" }},
		{"a maintenance owner outside the two words", func(r *CreateRepositoryDomainRequest) { r.MaintenanceOwner = "somebody" }},
		{"a storage location this deployment cannot honour", func(r *CreateRepositoryDomainRequest) {
			r.Location = "b2://acme-backups/primary"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := openRestoreTestService(t)
			configPath := svc.configPath
			before := readFileForTest(t, configPath)

			req := validDomainRequest(t)
			tc.mutum(&req)

			_, err := svc.CreateRepositoryDomain(context.Background(), req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("CreateRepositoryDomain(%s) = %v, want ErrInvalidRequest", tc.name, err)
			}
			if after := readFileForTest(t, configPath); after != before {
				t.Error("a refused create changed config.yaml")
			}
		})
	}
}

// ADR 0017's rule at the moment a declaration would break it: maintenance
// ownership is taken by whoever first maintains an unclaimed repository
// and afterwards moves only by transfer, so a create that named this
// deployment as the maintainer of a repository somebody else already
// holds would be exactly the claim that design forbids.
func TestCreateRepositoryDomain_WillNotClaimMaintenanceAnotherInstanceHolds(t *testing.T) {
	svc := openRestoreTestService(t)
	configPath := svc.configPath
	plantMaintenanceRecord(t, configPath, "offsite-b2", "nas-two")
	before := readFileForTest(t, configPath)

	req := validDomainRequest(t)
	req.MaintenanceOwner = "this"

	_, err := svc.CreateRepositoryDomain(context.Background(), req)
	if !errors.Is(err, ErrRepositoryDomainMaintainedElsewhere) {
		t.Fatalf("declaring a domain another instance maintains = %v, want ErrRepositoryDomainMaintainedElsewhere", err)
	}
	if !strings.Contains(err.Error(), "nas-two") {
		t.Errorf("the refusal does not name the owner an operator has to go and ask: %v", err)
	}
	if after := readFileForTest(t, configPath); after != before {
		t.Error("a refused create changed config.yaml")
	}

	// And the escape the refusal names actually works: the operator who
	// knows another instance maintains this store declares the boundary
	// without claiming it.
	req.MaintenanceOwner = "another-instance"
	if _, err := svc.CreateRepositoryDomain(context.Background(), req); err != nil {
		t.Fatalf("declaring the same domain as maintained elsewhere: %v", err)
	}
	if got := declaredDomains(t, reopen(t, configPath)); !contains(got, "offsite-b2") {
		t.Errorf("a second load reports %v, which does not include the domain that was accepted", got)
	}
}

// plantMaintenanceRecord writes a durable ownership record for a domain
// nothing declares yet, which is what a re-declared id or a state
// directory shared with a second instance leaves behind.
func plantMaintenanceRecord(t *testing.T, configPath, domain, owner string) {
	t.Helper()

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("loading the fixture configuration: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validating the fixture configuration: %v", err)
	}
	dir, err := backupengine.ReservedLocalStateDir(cfg.EffectiveBackupRoot())
	if err != nil {
		t.Fatalf("ReservedLocalStateDir(%q): %v", cfg.EffectiveBackupRoot(), err)
	}
	store, err := backupengine.NewFileMaintenanceOwnershipStore(dir)
	if err != nil {
		t.Fatalf("NewFileMaintenanceOwnershipStore: %v", err)
	}
	id, err := model.NewRepositoryDomainID(domain)
	if err != nil {
		t.Fatalf("NewRepositoryDomainID(%q): %v", domain, err)
	}
	if _, err := store.Create(context.Background(), backupengine.MaintenanceOwnership{
		Domain: id,
		Owner:  backupengine.MaintenanceOwner(owner),
	}); err != nil {
		t.Fatalf("planting the ownership record: %v", err)
	}
}

func readFileForTest(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a path this test created.
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(raw)
}

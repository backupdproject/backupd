// POST /api/v1/repositories as a client sees it (issue #862).
//
// Two things are worth pinning at this boundary and nothing else is. The
// first is that the passphrase crosses it as a REFERENCE: the body names
// a file, an environment variable or a command, the service is handed
// exactly that, and there is no field a secret could have arrived in. The
// second is that the three refusals this route has are told APART -- the
// engine gate, a duplicate id and a repository somebody else maintains
// are all 409s, and a client that could not tell them apart would offer
// "pick another id" to an operator whose deployment simply has the engine
// switched off.

package webhost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/service"
)

const validDomainBody = `{
  "id": "offsite-b2",
  "description": "Second copy, off site",
  "isolation": "isolated",
  "passphrase": {"file": "/etc/backupd/offsite-b2.passphrase"},
  "maintenance_owner": "this"
}`

func postRepositoryDomain(t *testing.T, router http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repositories", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestCreateRepositoryDomain_PassesAReferenceThroughAndAnswersWithTheCreatedDomain(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	snapshotsOf(tr.backend).created = service.RepositoryHealth{
		Domain: "offsite-b2", State: "FAILING", MayShare: false,
	}

	rec := postRepositoryDomain(t, tr.router, validDomainBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rec.Code, rec.Body.String())
	}

	var body repositoryHealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the response: %v (%s)", err, rec.Body.String())
	}
	if body.Domain != "offsite-b2" {
		t.Errorf("the response names domain %q, want offsite-b2", body.Domain)
	}
	// The created domain is reported exactly as the fleet read reports
	// one, which for a store nothing has run into yet is not healthy. A
	// handler that invented a green row here would teach a surface to
	// show one.
	if body.State != "FAILING" {
		t.Errorf("the response reports state %q, and the service said FAILING", body.State)
	}

	got := snapshotsOf(tr.backend).lastCreate
	if got.ID != "offsite-b2" || got.Isolation != "isolated" {
		t.Errorf("the service was handed %+v, which is not what the body said", got)
	}
	if got.Passphrase.File != "/etc/backupd/offsite-b2.passphrase" {
		t.Errorf("the passphrase reference did not cross the boundary: %+v", got.Passphrase)
	}
	if got.MaintenanceOwner != "this" {
		t.Errorf("maintenance owner = %q, want this", got.MaintenanceOwner)
	}
}

// The three 409s, each with its own code. Same status deliberately: what
// makes them usable is that the code differs.
func TestCreateRepositoryDomain_TellsItsRefusalsApart(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "the deployment does not run the incremental engine",
			err:        fmt.Errorf("%w", service.ErrIncrementalEngineDisabled),
			wantStatus: http.StatusConflict,
			wantCode:   "INCREMENTAL_ENGINE_DISABLED",
		},
		{
			name:       "the id is already declared",
			err:        fmt.Errorf("%w: offsite-b2", service.ErrRepositoryDomainExists),
			wantStatus: http.StatusConflict,
			wantCode:   "REPOSITORY_DOMAIN_EXISTS",
		},
		{
			name:       "another instance maintains that repository",
			err:        fmt.Errorf("%w: nas-two holds maintenance for offsite-b2", service.ErrRepositoryDomainMaintainedElsewhere),
			wantStatus: http.StatusConflict,
			wantCode:   "REPOSITORY_DOMAIN_MAINTAINED_ELSEWHERE",
		},
		{
			name:       "the declaration is one config would not accept",
			err:        fmt.Errorf("%w: isolation: %q is neither shared nor isolated", service.ErrInvalidRequest, "private"),
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_REQUEST",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newOperationsTestRouter(t, alwaysPassGate{})
			snapshotsOf(tr.backend).errOnCreate = tc.err

			rec := postRepositoryDomain(t, tr.router, validDomainBody)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if code := errorCodeOf(t, rec); code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}

			declared := contractEndpoints()["createRepositoryDomain"].ErrorCodes[tc.wantStatus]
			found := false
			for _, c := range declared {
				if string(c) == tc.wantCode {
					found = true
				}
			}
			if !found {
				t.Errorf("the handler answered %d %q, which api/v1/openapi.json does not declare for createRepositoryDomain at that status (it declares %v)", tc.wantStatus, tc.wantCode, declared)
			}
		})
	}
}

// The gate refusal keeps its sentence, which is the one place an operator
// is told which key to set.
func TestCreateRepositoryDomain_TheGateRefusalNamesTheKeyToSet(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	snapshotsOf(tr.backend).errOnCreate = fmt.Errorf("%w", service.ErrIncrementalEngineDisabled)

	rec := postRepositoryDomain(t, tr.router, validDomainBody)
	if body := rec.Body.String(); !strings.Contains(body, "incremental_engine.enabled: true") {
		t.Errorf("the 409 body does not name the config key to set: %s", body)
	}
}

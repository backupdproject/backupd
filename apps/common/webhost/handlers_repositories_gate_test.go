// EPIC K's production feature gate (#789) as an API consumer sees it on
// the repository read.
//
// GET /api/v1/repositories is the one read whose whole answer disappears
// when the gate is shut: with the engine disabled nothing may open a
// repository, so there is no verdict to serve. What must NOT happen is
// the default for an error on that path -- a 500 INTERNAL, which tells a
// dashboard "something went wrong" about a deployment where nothing is
// wrong and one config key is the whole story.

package webhost

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/service"
)

func TestListRepositories_TheProductionGateIsAConflictAndNotAnInternalError(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	snapshotsOf(tr.backend).err = fmt.Errorf("%w", service.ErrIncrementalEngineDisabled)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repositories", nil)
	rec := httptest.NewRecorder()
	tr.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != "INCREMENTAL_ENGINE_DISABLED" {
		t.Errorf("code = %q, want INCREMENTAL_ENGINE_DISABLED", code)
	}

	// The message reaches the client intact: it is the one place an
	// operator is told which key to set, and a generic body would make
	// the code the only usable part of the answer.
	if body := rec.Body.String(); !strings.Contains(body, "incremental_engine.enabled: true") {
		t.Errorf("the 409 body does not name the config key to set: %s", body)
	}
}

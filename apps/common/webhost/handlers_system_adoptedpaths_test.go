package webhost

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/retnd/retnd/apps/common/platform/capabilities"
	"github.com/retnd/retnd/core/legacypath"
)

// The third of FR-38's three reporting surfaces (matrix row V.5).
//
// EPIC R (#890) renamed this deployment's container-internal
// configuration directory, and a deployment still mounted at the
// pre-rename path is served from that path instead (core/legacypath). The
// warning that says so is printed once per start, into a log an operator
// scrolls past; `retnd check` answers it in a terminal on the host. This
// route is the answer a client, a support transcript or a dashboard can
// read without a shell on the box at all, which is the case the other two
// surfaces do not cover.
//
// It lives on GET /api/v1/system/version rather than on a route of its
// own because that is the route the deployment check already reads
// (core/cmd/retnd/deploymentcheck.go compares the deployment_id it
// carries): "which deployment am I talking to" and "which of its
// directories is actually live" are the same question one level down, and
// a client that already fetches this response should not need a second
// round trip to find out that the answer came from a pre-rename path.

// TestSystemVersion_ReportsNoAdoptedPathsOnANormalDeployment is the
// control for the test below, and it is the case that is true of every
// deployment once the migration has been run.
//
// The field is required by the contract, so it has to be PRESENT and
// EMPTY rather than absent: a client must never have to tell "no
// adoptions" apart from "this build does not report them", because those
// two answers differ by exactly the release this shim closes in.
func TestSystemVersion_ReportsNoAdoptedPathsOnANormalDeployment(t *testing.T) {
	router := newTestRouterForSystem(t, capabilities.PlatformCapabilities{})
	rec := doGet(t, router, "/api/v1/system/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	body, ok := raw["adopted_paths"]
	if !ok {
		t.Fatalf("adopted_paths is absent from the response; the contract declares it required, so a client cannot tell an unadopted deployment from a build that does not report adoption\nbody: %s", rec.Body.String())
	}
	if string(body) != "[]" {
		t.Errorf("adopted_paths = %s on a deployment with no adoptions, want []", body)
	}
}

// TestSystemVersion_ReportsTheAdoptedPathAndTheRenamedOne is V.5 itself.
//
// Both paths, not just the live one. The path in use answers "where is my
// data", and the renamed path answers "where does it have to go", which
// is the question the operator actually has to act on; a response
// carrying only one of the two would leave them reading the release notes
// to work out the other.
func TestSystemVersion_ReportsTheAdoptedPathAndTheRenamedOne(t *testing.T) {
	router := NewRouter(RouterConfig{
		Platform:      fakePlatformAdapter{caps: capabilities.PlatformCapabilities{}, auth: fakeAuthenticator{authenticated: true, username: "alice"}},
		Backend:       newSyncFakeBackend(),
		Gate:          alwaysPassGate{},
		BinaryVersion: "9.9.9",
		Commit:        "deadbeef",
		// Exactly what core/legacypath hands the host at startup: the
		// configuration half adopted, the state half not. Both halves
		// are reported separately because different code resolves them
		// and a deployment can have adopted one and not the other.
		AdoptedPaths: []legacypath.Adoption{{
			What:    "configuration",
			Renamed: "/etc/retnd/config/config.yaml",
			Legacy:  "/etc/backupd/config/config.yaml",
			Path:    "/etc/backupd/config/config.yaml",
			Outcome: legacypath.AdoptLegacy,
		}},
	})

	rec := doGet(t, router, "/api/v1/system/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		AdoptedPaths []struct {
			What    string `json:"what"`
			Serving string `json:"serving"`
			Renamed string `json:"renamed"`
		} `json:"adopted_paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.AdoptedPaths) != 1 {
		t.Fatalf("adopted_paths has %d entries, want 1\nbody: %s", len(body.AdoptedPaths), rec.Body.String())
	}
	got := body.AdoptedPaths[0]
	if got.What != "configuration" {
		t.Errorf("what = %q, want %q; an operator with both halves adopted has to be able to tell which is which", got.What, "configuration")
	}
	if got.Serving != "/etc/backupd/config/config.yaml" {
		t.Errorf("serving = %q, want the pre-rename path that is actually live", got.Serving)
	}
	if got.Renamed != "/etc/retnd/config/config.yaml" {
		t.Errorf("renamed = %q, want the path the deployment has to be moved to", got.Renamed)
	}
}

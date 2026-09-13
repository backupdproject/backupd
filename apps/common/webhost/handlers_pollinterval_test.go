package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Issue #845 on the wire: the deployment-wide poll interval as a settings
// section, and the per-set override as a nullable field on a backup set.
//
// The interesting assertions are about NULL. "This set inherits" and
// "this set polls every 15 minutes, which happens to be what the
// deployment does today" are different configurations, and a wire that
// could not tell them apart would make the next save pin every set to the
// default it was tracking.

func TestGetSettings_CarriesTheServiceBehaviourSection(t *testing.T) {
	tr := newSettingsTestRouter(t)
	tr.backend.settings.Service.PollInterval = 15 * time.Minute

	got := decodeSettings(t, tr.get(t))
	if got.Service.PollIntervalSeconds != 900 {
		t.Errorf("service.poll_interval_seconds = %d, want 900", got.Service.PollIntervalSeconds)
	}
	// The floor is served rather than restated in the form, exactly as
	// the retention bounds are: a client that kept its own copy would
	// eventually refuse a value the engine accepts, or accept one it
	// refuses.
	if got.Schema.Service.MinPollIntervalSeconds != 60 {
		t.Errorf("schema.service.min_poll_interval_seconds = %d, want 60", got.Schema.Service.MinPollIntervalSeconds)
	}
}

func TestPatchSettings_WritesTheGlobalPollInterval(t *testing.T) {
	tr := newSettingsTestRouter(t)

	rec := tr.patch(t, `{"service":{"poll_interval_seconds":2700}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if tr.backend.lastUpdate.Service == nil || tr.backend.lastUpdate.Service.PollInterval == nil {
		t.Fatal("the backend received no service.poll_interval")
	}
	if got := *tr.backend.lastUpdate.Service.PollInterval; got != 45*time.Minute {
		t.Errorf("PollInterval = %s, want 45m0s", got)
	}
	if tr.backend.lastUpdate.Retention != nil || tr.backend.lastUpdate.Capacity != nil {
		t.Error("a service-only write carried another section; an omitted section must reach the backend as nil")
	}
}

// TestPatchSettings_AnEmptyServiceSectionIsRefused holds the new section
// to the rule every other one already follows: a section that names no
// field asks for nothing, and answering 200 for it would rewrite the
// operator's file and move the config revision for no change.
func TestPatchSettings_AnEmptyServiceSectionIsRefused(t *testing.T) {
	tr := newSettingsTestRouter(t)

	rec := tr.patch(t, `{"service":{}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
	if tr.backend.updateCalls != 0 {
		t.Error("an empty service section reached the backend")
	}
}

// TestGetBackupSet_ReportsThePollIntervalOverrideAndTheEffectiveValue is
// the read a config form fills itself from: null means inherit, and the
// effective number is served beside it so the form can say what
// inheriting currently gets you without a second request.
func TestGetBackupSet_ReportsThePollIntervalOverrideAndTheEffectiveValue(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	seedSet(t, tr, "api/postgres-primary")

	tr.backend.setPollInterval(t, "api/postgres-primary", nil, 15*time.Minute)
	inherited := getBackupSetBody(t, tr, "api/postgres-primary")
	if inherited.PollIntervalSeconds != nil {
		t.Errorf("poll_interval_seconds = %v for an inheriting set, want null", *inherited.PollIntervalSeconds)
	}
	if inherited.EffectivePollIntervalSeconds != 900 {
		t.Errorf("effective_poll_interval_seconds = %d, want 900", inherited.EffectivePollIntervalSeconds)
	}

	override := 5 * time.Minute
	tr.backend.setPollInterval(t, "api/postgres-primary", &override, override)
	overridden := getBackupSetBody(t, tr, "api/postgres-primary")
	if overridden.PollIntervalSeconds == nil || *overridden.PollIntervalSeconds != 300 {
		t.Errorf("poll_interval_seconds = %v, want 300", overridden.PollIntervalSeconds)
	}
	if overridden.EffectivePollIntervalSeconds != 300 {
		t.Errorf("effective_poll_interval_seconds = %d, want 300", overridden.EffectivePollIntervalSeconds)
	}
}

// TestPatchBackupSet_CarriesThePollIntervalOverride covers all three
// requests this field can carry: absent (leave alone), a value, and the
// explicit zero that means "inherit the deployment's again".
func TestPatchBackupSet_CarriesThePollIntervalOverride(t *testing.T) {
	t.Run("a value", func(t *testing.T) {
		tr := newBackupSetsTestRouter(t)
		seedSet(t, tr, "api/postgres-primary")
		rec := patchBackupSet(t, tr.router, "api/postgres-primary", `{"poll_interval_seconds":300}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
		}
		got := tr.backend.lastUpdate().PollInterval
		if got == nil || *got != 5*time.Minute {
			t.Errorf("PollInterval = %v, want 5m0s", got)
		}
	})

	t.Run("an explicit zero clears the override", func(t *testing.T) {
		tr := newBackupSetsTestRouter(t)
		seedSet(t, tr, "api/postgres-primary")
		rec := patchBackupSet(t, tr.router, "api/postgres-primary", `{"poll_interval_seconds":0}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
		}
		got := tr.backend.lastUpdate().PollInterval
		if got == nil {
			t.Fatal("an explicit zero arrived as an omission; a set would have no way to inherit again")
		}
		if *got != 0 {
			t.Errorf("PollInterval = %s, want 0", *got)
		}
	})

	t.Run("absent leaves it alone", func(t *testing.T) {
		tr := newBackupSetsTestRouter(t)
		seedSet(t, tr, "api/postgres-primary")
		rec := patchBackupSet(t, tr.router, "api/postgres-primary", `{"user":"backup"}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
		}
		if got := tr.backend.lastUpdate().PollInterval; got != nil {
			t.Errorf("PollInterval = %v for a body that never mentioned it, want nil", got)
		}
	})
}

// getBackupSetBody drives GET /api/v1/backup-sets/{id} and decodes it.
func getBackupSetBody(t *testing.T, tr backupSetsTestRouter, id string) backupSetResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backup-sets/"+id, nil)
	rec := httptest.NewRecorder()
	tr.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body: %s", id, rec.Code, rec.Body.String())
	}
	var got backupSetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v, body: %s", err, rec.Body.String())
	}
	return got
}

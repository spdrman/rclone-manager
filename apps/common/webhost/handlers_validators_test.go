// This file is issue #162's contract suite for GET /api/v1/validators,
// the read-only route that turns the wizard's decorative step 5 picklist
// into a real one. Contract tests come before the handler exists
// (docs/EPIC-B-multi-nas.md §4C): request shape, response shape, error
// behaviour and auth.
package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

func getValidators(t *testing.T, router http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/validators", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestListValidators_ReturnsTheRegisteredCatalog is the response
// contract: an object with one array field (matching
// listBackupSetsResponse and listStorageStatusResponse, so a future
// field can be added without breaking a client parsing a bare top-level
// array), one entry per core/service.RegisteredValidators entry, each
// carrying the id a create request may send and a human summary to
// render beside it.
func TestListValidators_ReturnsTheRegisteredCatalog(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := getValidators(t, tr.router)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body struct {
		Validators []struct {
			ID      string `json:"id"`
			Summary string `json:"summary"`
		} `json:"validators"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	catalog := service.RegisteredValidators()
	if len(body.Validators) != len(catalog) {
		t.Fatalf("returned %d validators, want %d (core/service.RegisteredValidators is the only source)", len(body.Validators), len(catalog))
	}
	for i, want := range catalog {
		if body.Validators[i].ID != string(want.ID) {
			t.Errorf("validators[%d].id = %q, want %q", i, body.Validators[i].ID, want.ID)
		}
		if body.Validators[i].Summary == "" {
			t.Errorf("validators[%d].summary is empty; the picklist has nothing to label %q with", i, want.ID)
		}
	}
}

// TestListValidators_NeverExposesAnExecutablePath is §26 Step 5 from the
// response side. The whole point of selecting a validator by id is that
// no path crosses this boundary in either direction: a client that
// learned where the scripts are materialized would learn this
// deployment's filesystem layout, and the next author would be one field
// away from sending one back.
func TestListValidators_NeverExposesAnExecutablePath(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := getValidators(t, tr.router)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	for _, needle := range []string{".sh", "/tmp/", "executable", "command", "/usr/", "validators/"} {
		if strings.Contains(body, needle) {
			t.Errorf("the catalog response contains %q, which is a path or an executable-shaped field:\n%s", needle, body)
		}
	}

	t.Run("the check catches what it is looking for", func(t *testing.T) {
		leaky := `{"validators":[{"id":"trailer-marker","executable":"/tmp/x/trailer-marker.sh"}]}`
		caught := 0
		for _, needle := range []string{".sh", "/tmp/", "executable", "command", "/usr/", "validators/"} {
			if strings.Contains(leaky, needle) {
				caught++
			}
		}
		if caught == 0 {
			t.Fatal("the needle list matched nothing in an obviously leaky body, so the assertion above proves nothing")
		}
	})
}

// TestListValidators_RequiresAuthentication keeps this route inside the
// same fail-closed group as every other /api/v1 route.
// TestNoAPIRouteBypassesAuthentication already walks the whole table, but
// this states it for the one route this file introduces, so a failure
// points here rather than at a generic walk.
func TestListValidators_RequiresAuthentication(t *testing.T) {
	router := NewRouter(RouterConfig{
		Platform: noAuthWiredAdapter{},
		Backend:  newBackupSetFakeBackend(),
		Gate:     alwaysPassGate{},
	})
	if rec := getValidators(t, router); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d for an unauthenticated request", rec.Code, http.StatusUnauthorized)
	}
}

// TestListValidators_IsReadOnly proves the route is a GET and nothing
// else: a POST to the same path must not be routed to this handler (405
// or 404 from chi, never 200), since a "catalog" a client could write to
// is precisely the arbitrary-command surface §26 Step 5 forbids.
func TestListValidators_IsReadOnly(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/validators", strings.NewReader(`{"id":"anything"}`))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	tr.router.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK || rec.Code == http.StatusCreated {
		t.Fatalf("POST /api/v1/validators returned %d; the catalog must be read-only", rec.Code)
	}
}

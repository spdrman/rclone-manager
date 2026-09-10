package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

// This file is EPIC I's (#664) contract suite for GET /api/v1/backends,
// the read-only route that makes the backend registry reachable from a
// browser. Without it #668's add-a-destination wizard has no source for
// its backend list but a literal of its own, which is the assumption the
// epic exists to remove.
//
// Contract tests come before the handler exists
// (docs/EPIC-B-multi-nas.md §4C): response shape, auth, read-onlyness,
// and — the one that is specific to this route — that a catalogue of
// manifests carries SHAPE and never a value.

func getBackends(t *testing.T, router http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backends", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// backendsBody is the response as a client parses it, spelled out here
// rather than borrowed from the handler so the test reads the wire and not
// the implementation.
type backendsBody struct {
	Backends []struct {
		ID            string `json:"id"`
		Label         string `json:"label"`
		Summary       string `json:"summary"`
		Role          string `json:"role"`
		RcloneBackend string `json:"rclone_backend"`
		Fields        []struct {
			ID         string `json:"id"`
			Label      string `json:"label"`
			Help       string `json:"help"`
			Kind       string `json:"kind"`
			Required   bool   `json:"required"`
			Pattern    string `json:"pattern"`
			UnsetMeans string `json:"unset_means"`
			Values     []struct {
				Value string `json:"value"`
				Label string `json:"label"`
			} `json:"values"`
		} `json:"fields"`
		Probe struct {
			Steps []struct {
				Step   string `json:"step"`
				Run    bool   `json:"run"`
				Reason string `json:"reason"`
			} `json:"steps"`
		} `json:"probe"`
	} `json:"backends"`
	Unregistered []struct {
		RcloneBackend string `json:"rclone_backend"`
	} `json:"unregistered"`
	InstanceIDPattern  string `json:"instance_id_pattern"`
	ReservedInstanceID string `json:"reserved_instance_id"`
}

func decodeBackends(t *testing.T, rec *httptest.ResponseRecorder) backendsBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body backendsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

// TestListBackends_ReturnsTheRegistry is the response contract: one entry
// per registered manifest, in registry order, projected field for field
// from core/service.RegisteredBackends, which is the only source.
func TestListBackends_ReturnsTheRegistry(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	body := decodeBackends(t, getBackends(t, tr.router))

	catalog, err := service.RegisteredBackends()
	if err != nil {
		t.Fatalf("service.RegisteredBackends: %v", err)
	}
	if len(body.Backends) != len(catalog.Backends) {
		t.Fatalf("returned %d backends, want %d (core/service.RegisteredBackends is the only source)", len(body.Backends), len(catalog.Backends))
	}
	for i, want := range catalog.Backends {
		got := body.Backends[i]
		if got.ID != want.ID {
			t.Errorf("backends[%d].id = %q, want %q", i, got.ID, want.ID)
		}
		if got.Label == "" || got.Summary == "" {
			t.Errorf("backends[%d] (%s) has an empty label or summary; the picker has nothing to render", i, want.ID)
		}
		if got.Role != want.Role {
			t.Errorf("backends[%d].role = %q, want %q", i, got.Role, want.Role)
		}
		if got.RcloneBackend != want.RcloneBackend {
			t.Errorf("backends[%d].rclone_backend = %q, want %q", i, got.RcloneBackend, want.RcloneBackend)
		}
		if len(got.Fields) != len(want.Fields) {
			t.Fatalf("backends[%d] (%s) declares %d fields, want %d; a projection that drops one is a manifest format with two spellings", i, want.ID, len(got.Fields), len(want.Fields))
		}
		for j, wf := range want.Fields {
			gf := got.Fields[j]
			if gf.ID != wf.ID || gf.Kind != wf.Kind || gf.Required != wf.Required {
				t.Errorf("backends[%d].fields[%d] = {%q %q %v}, want {%q %q %v}", i, j, gf.ID, gf.Kind, gf.Required, wf.ID, wf.Kind, wf.Required)
			}
		}
		if len(got.Probe.Steps) != len(want.Probe) {
			t.Errorf("backends[%d] (%s) declares %d probe steps, want %d; a surface renders a fixed list and a short one would hide a step", i, want.ID, len(got.Probe.Steps), len(want.Probe))
		}
		for j, ws := range want.Probe {
			gs := got.Probe.Steps[j]
			if gs.Step != ws.Step || gs.Run != ws.Run {
				t.Errorf("backends[%d].probe.steps[%d] = {%q %v}, want {%q %v}", i, j, gs.Step, gs.Run, ws.Step, ws.Run)
			}
			// A skipped step is a first-class outcome, not a quiet pass,
			// so the sentence explaining it has to survive the wire.
			if !ws.Run && gs.Reason == "" {
				t.Errorf("backends[%d].probe.steps[%d] (%s) does not run and carries no reason", i, j, ws.Step)
			}
		}
	}
}

// TestListBackends_CarriesTheNamingRules is the half of this response the
// add-a-destination form validates against. Both rules come from the
// engine's own constants: a client holding its own copy goes stale in one
// direction only, silently accepting a name the engine then rejects.
func TestListBackends_CarriesTheNamingRules(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	body := decodeBackends(t, getBackends(t, tr.router))

	catalog, err := service.RegisteredBackends()
	if err != nil {
		t.Fatalf("service.RegisteredBackends: %v", err)
	}
	if body.InstanceIDPattern != catalog.InstanceIDPattern {
		t.Errorf("instance_id_pattern = %q, want %q", body.InstanceIDPattern, catalog.InstanceIDPattern)
	}
	if body.ReservedInstanceID != catalog.ReservedInstanceID {
		t.Errorf("reserved_instance_id = %q, want %q", body.ReservedInstanceID, catalog.ReservedInstanceID)
	}
	if body.InstanceIDPattern == "" || body.ReservedInstanceID == "" {
		t.Fatal("a form cannot refuse a name it has no rule for; both fields are required")
	}
}

// TestListBackends_ReportsAnUnderstoodButUnregisteredBackend is the
// dimmed half of #668's picker. Hiding it answers an operator worse:
// somebody who came looking for SFTP learns nothing from a menu that
// never mentions it, whereas a row saying the shape is understood and is
// not registered is a real answer.
func TestListBackends_ReportsAnUnderstoodButUnregisteredBackend(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	body := decodeBackends(t, getBackends(t, tr.router))

	catalog, err := service.RegisteredBackends()
	if err != nil {
		t.Fatalf("service.RegisteredBackends: %v", err)
	}
	if len(body.Unregistered) != len(catalog.Unregistered) {
		t.Fatalf("returned %d unregistered backends, want %d", len(body.Unregistered), len(catalog.Unregistered))
	}
	// The set is a subtraction, so no name may appear on both sides: a
	// backend that is both registered and not is a catalogue an operator
	// cannot read.
	registered := map[string]bool{}
	for _, b := range body.Backends {
		registered[b.RcloneBackend] = true
	}
	for i, u := range body.Unregistered {
		if u.RcloneBackend == "" {
			t.Errorf("unregistered[%d] has no name", i)
		}
		if registered[u.RcloneBackend] {
			t.Errorf("unregistered[%d] = %q, which a manifest already claims", i, u.RcloneBackend)
		}
	}
}

// TestListBackends_DeclaresShapeAndNeverAValue is this route's own
// version of the rule #665's C1-C5 establish for credentials, and it is
// the reason a catalogue of manifests can be served to a browser at all.
//
// A manifest says a credential is NEEDED here. It never says what one is.
// The assertion is structural rather than a search for today's secrets: a
// credential-kind field may carry an id, a label and a required flag, and
// there is no field on this wire shape that could hold material even if a
// future manifest tried to.
func TestListBackends_DeclaresShapeAndNeverAValue(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := getBackends(t, tr.router)
	body := decodeBackends(t, rec)

	credentialFields := 0
	for _, b := range body.Backends {
		for _, f := range b.Fields {
			if f.Kind != "credential" {
				continue
			}
			credentialFields++
			// A credential field declares itself and nothing else. Values
			// is the enum choice set and a credential has no choices; a
			// pattern would be a claim about the shape of a secret, and
			// unset_means would be a default credential.
			if len(f.Values) != 0 {
				t.Errorf("backend %q field %q is a credential and declares %d choices", b.ID, f.ID, len(f.Values))
			}
			if f.Pattern != "" {
				t.Errorf("backend %q field %q is a credential and declares a pattern", b.ID, f.ID)
			}
			if f.UnsetMeans != "" {
				t.Errorf("backend %q field %q is a credential and declares what unset means, which would be a default credential", b.ID, f.ID)
			}
		}
	}
	// The positive control: without it this test passes on a response
	// that contains no credential field at all, which proves nothing.
	if credentialFields == 0 {
		t.Fatal("no bundled manifest declares a credential field, so the assertions above are vacuous")
	}

	// And no key anywhere in the raw body is value-shaped, whatever a
	// manifest might one day put in one. Read as bytes rather than
	// through backendsBody above, so a field added to the response
	// without being added to that struct is still caught.
	//
	// An enum choice's own `value` is deliberately NOT on this list: a
	// declared choice ("STANDARD_IA") is part of the shape, it is the
	// same for every deployment, and it is exactly what a picker has to
	// render. What must never appear is anything an operator supplied.
	forbidden := []string{"secret", "access_key", "password", "token", "credentials_id"}
	raw := rec.Body.String()
	for _, needle := range forbidden {
		if strings.Contains(raw, needle) {
			t.Errorf("the backend catalogue contains %q, which is value-shaped", needle)
		}
	}
	t.Run("the check catches what it is looking for", func(t *testing.T) {
		leaky := `{"backends":[{"id":"s3","fields":[{"id":"credentials","kind":"credential","secret":"x"}]}]}`
		caught := 0
		for _, needle := range forbidden {
			if strings.Contains(leaky, needle) {
				caught++
			}
		}
		if caught == 0 {
			t.Fatal("the needle list matched nothing in an obviously leaky body, so the assertion above proves nothing")
		}
	})
}

// TestListBackends_RequiresAuthentication keeps this route inside the
// same fail-closed group as every other /api/v1 route.
// TestNoAPIRouteBypassesAuthentication already walks the whole table;
// this states it for the one route this file introduces, so a failure
// points here rather than at a generic walk.
func TestListBackends_RequiresAuthentication(t *testing.T) {
	router := NewRouter(RouterConfig{
		Platform: noAuthWiredAdapter{},
		Backend:  newBackupSetFakeBackend(),
		Gate:     alwaysPassGate{},
	})
	if rec := getBackends(t, router); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d for an unauthenticated request", rec.Code, http.StatusUnauthorized)
	}
}

// TestListBackends_IsReadOnly proves the route is a GET and nothing else.
// A backend catalogue a client could write to would put FR-4's gate on
// the far side of the network from the binary it constrains, and a
// manifest accepted over HTTP could name an rclone backend this build
// does not carry.
func TestListBackends_IsReadOnly(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends", strings.NewReader(`{"id":"anything"}`))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	tr.router.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK || rec.Code == http.StatusCreated {
		t.Fatalf("POST /api/v1/backends returned %d; the catalogue must be read-only", rec.Code)
	}
}

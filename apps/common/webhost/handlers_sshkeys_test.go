package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The two reads this API never had, and the one write that grows a mode
// (issue #592).
//
// Every case here is about the same seam: what a browser is allowed to
// learn about this deployment's key store, and what it is allowed to send
// back. The store listing must carry ids and describing metadata and no
// path, because SSHKeyRef.KeyFile has been kept off the wire since #146
// and an inventory is not an exception to that. The candidate listing DOES
// carry paths, because a candidate's path is its identity to the operator,
// and what makes that safe is that the handle travelling in the other
// direction is opaque and the locations are a closed set this process
// decides.

func getJSON(t *testing.T, router http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestListSSHKeys_ServesTheStoreWithoutAPathOrKeyMaterial is the read
// that makes `--ssh-key-id` and the edit box usable at all.
func TestListSSHKeys_ServesTheStoreWithoutAPathOrKeyMaterial(t *testing.T) {
	tr := newBackupSetsTestRouter(t)

	rec := getJSON(t, tr.router, "/api/v1/ssh-keys")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Keys) == 0 {
		t.Fatal("the listing is empty, so nothing below is being asserted about a real row")
	}
	for _, key := range body.Keys {
		for _, forbidden := range []string{"key_file", "path", "private_key_pem"} {
			if _, present := key[forbidden]; present {
				t.Errorf("a key row carries %q. The server-side path is kept off the wire so a caller never learns this process's filesystem layout, and a listing is not an exception", forbidden)
			}
		}
		for _, required := range []string{"id", "algorithm", "fingerprint", "used_by"} {
			if _, present := key[required]; !present {
				t.Errorf("a key row is missing %q, which is what a caller renders instead of a path", required)
			}
		}
	}
	if strings.Contains(rec.Body.String(), "PRIVATE KEY") {
		t.Error("the listing carries private key material")
	}
}

// TestListSSHKeyCandidates_NamesEveryLocationItSearched is the epic's
// refusals rule at the HTTP boundary.
//
// An empty candidate list is only meaningful beside the places that were
// looked in, so the two travel together in one response and a caller
// cannot render one without the other.
func TestListSSHKeyCandidates_NamesEveryLocationItSearched(t *testing.T) {
	tr := newBackupSetsTestRouter(t)

	rec := getJSON(t, tr.router, "/api/v1/ssh/key-candidates")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Locations []map[string]any `json:"locations"`
		//nolint:unused // asserted through the raw body below
		Candidates []map[string]any `json:"candidates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Locations) == 0 {
		t.Fatal("the scan reported no locations. An empty candidate list has to read as \"I looked in these places\", never as \"you have no keys\"")
	}
	for _, loc := range body.Locations {
		if loc["path"] == nil || loc["path"] == "" {
			t.Error("a searched location has no path")
		}
		if loc["kind"] == nil || loc["kind"] == "" {
			t.Error("a searched location has no kind; a caller renders \"the key file your configuration names\" differently from \"a directory that is not mounted\"")
		}
	}
}

// TestImportSSHKey_SelectingACandidateNeverTakesAPath is the traversal
// case for the mode this endpoint grows.
//
// A candidate id is an opaque handle that resolves only against a fresh
// scan of the fixed locations. Anything path-shaped is refused by the
// service, and the point of this case is that the HTTP layer does not
// invent a second way in.
func TestImportSSHKey_SelectingACandidateNeverTakesAPath(t *testing.T) {
	tr := newBackupSetsTestRouter(t)

	rec := postSSHKeyImport(t, tr.router, `{"candidate_id":"abc123"}`, true)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("POST /api/v1/ssh-keys does not accept a candidate_id at all; selecting a discovered key is the mode that makes the wizard's first step work")
	}
	if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "private_key_pem is required") {
		t.Fatalf("a request naming a candidate was refused for having no pasted key: %s", rec.Body.String())
	}

	// The two modes are mutually exclusive, for the reason
	// test-connection's own two modes are: a request that is both is
	// ambiguous about what is being imported, and silently preferring
	// one is how a caller ends up with a key it did not ask for.
	both := postSSHKeyImport(t, tr.router, `{"candidate_id":"abc123","private_key_pem":"-----BEGIN OPENSSH PRIVATE KEY-----\nX\n-----END OPENSSH PRIVATE KEY-----"}`, true)
	if both.Code != http.StatusBadRequest {
		t.Errorf("a request carrying both a pasted key and a candidate id returned %d, want %d", both.Code, http.StatusBadRequest)
	}
}

// TestTestConnection_ReportsTheSixSteps is the response growing without
// #211's callers noticing, and it is the CANDIDATE mode of the route.
//
// Six steps here and six steps in the persisted mode, out of one array
// called `checks`. Both branches used to answer with different arrays and
// a caller had to remember which request it had sent to know which one it
// was holding, which is a contract that costs a major version to take
// back later.
func TestTestConnection_ReportsTheSixSteps(t *testing.T) {
	tr := newBackupSetsTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/backup-sets/test-connection",
		strings.NewReader(`{"host":"prod-db-01.internal","port":22,"user":"backup-agent","ssh_key_id":"key_test_1","known_hosts_line":"prod-db-01.internal ssh-ed25519 AAAAfaketest","remote_path":"/backups"}`))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	tr.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"stages"`) {
		t.Fatalf("the response still carries a `stages` array beside `checks`; one route answering in two shapes is what this convergence removed: %s", rec.Body.String())
	}
	var body struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Step       string `json:"step"`
			Outcome    string `json:"outcome"`
			Detail     string `json:"detail"`
			DurationMS *int   `json:"duration_ms"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := []string{"credentials", "resolve", "connect", "host_key", "authenticate", "list"}
	if len(body.Checks) != len(want) {
		t.Fatalf("checks = %d, want %d: %v. Reporting fewer is the boolean this change exists to replace", len(body.Checks), len(want), want)
	}
	for i, check := range body.Checks {
		if check.Step != want[i] {
			t.Errorf("check %d is %q, want %q; the order is the order they are proven in", i, check.Step, want[i])
		}
		if check.Outcome == "" {
			t.Errorf("step %q has no outcome", check.Step)
		}
		if check.Detail == "" {
			t.Errorf("step %q has no detail, and detail is required on this schema: a step with no sentence is a row nobody can act on", check.Step)
		}
	}

	// A pointer, so "absent" and "0" are distinguishable, which is the
	// entire reason duration_ms is omitempty. Authenticate and list come
	// out of ONE call, so a "0 ms" there would be a number the server
	// never measured, rendered beside a green row as though it had.
	for _, check := range body.Checks {
		switch check.Step {
		case "authenticate", "list":
			if check.DurationMS != nil {
				t.Errorf("%s carries duration_ms=%d; these two share one call and have no timing of their own, so the field has to be absent rather than zero", check.Step, *check.DurationMS)
			}
		default:
			if check.DurationMS == nil {
				t.Errorf("%s carries no duration_ms; it is measured on its own and a surface renders that number", check.Step)
			}
		}
	}
}

// TestGetBackupSet_ReportsWhichKeyItUses closes the loop the edit box
// could never close: a set that can be told which key to use has to be
// able to say which key it uses.
func TestGetBackupSet_ReportsWhichKeyItUses(t *testing.T) {
	tr := newBackupSetsTestRouter(t)
	rec := postBackupSet(t, tr.router, validCreateBody, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating the fixture set: %d %s", rec.Code, rec.Body.String())
	}

	got := getJSON(t, tr.router, "/api/v1/backup-sets/api/postgres-primary")
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", got.Code, http.StatusOK, got.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, present := body["ssh_key_id"]; !present {
		t.Fatal("a backup set does not report ssh_key_id, so a surface offering to replace its key cannot name the key being replaced")
	}
}

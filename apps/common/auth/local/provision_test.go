package local

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// TestCreateAdmin_ProvisionsAnAdminRecordWithNoHTTPInvolvedAtAll is issue
// #322's RED case made concrete: before this package had a CreateAdmin
// entry point, the ONLY way to put an AdminRecord into a store was
// through Service.Handler's POST /enroll - a running HTTP server, a CSRF
// cookie, and bootstrap.go's in-memory, single-use, network-reachable
// token, none of which exist yet for a store nobody has opened with New.
//
// CreateAdmin is a second, genuinely different entry point into the same
// store.go this test then reads back through - not a client of the HTTP
// handler, not a bypass of its checks, a direct call into the on-disk
// format itself, exactly the way every other CLI command in this project
// already reads config.yaml/state.db directly instead of calling an API
// over HTTP.
func TestCreateAdmin_ProvisionsAnAdminRecordWithNoHTTPInvolvedAtAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")

	admin, err := CreateAdmin(CreateAdminConfig{
		StorePath: path,
		Username:  "bm-admin",
		Password:  "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	if admin.Username != "bm-admin" {
		t.Errorf("admin.Username = %q, want %q", admin.Username, "bm-admin")
	}
	if admin.PasswordHash == "" || admin.PasswordHash == "correct-horse-battery-staple" {
		t.Errorf("admin.PasswordHash = %q, must be hashed and non-empty, never the plaintext", admin.PasswordHash)
	}

	// Read the record back through a completely independent Store value,
	// the same way store_test.go's own
	// TestStore_PersistsAcrossANewStoreInstance proves a normal HTTP
	// enrollment persisted: this has to be indistinguishable from that
	// path to anything that later reads the store.
	reopened := NewStore(path)
	persisted, err := reopened.Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if persisted == nil || persisted.Username != "bm-admin" {
		t.Fatalf("Admin() after CreateAdmin = %+v, want the provisioned administrator", persisted)
	}
	if persisted.PasswordHash != admin.PasswordHash {
		t.Errorf("persisted.PasswordHash = %q, want %q (the same hash CreateAdmin returned)", persisted.PasswordHash, admin.PasswordHash)
	}
	if persisted.CreatedAt.IsZero() {
		t.Error("persisted.CreatedAt is zero, want a real timestamp")
	}
}

func TestCreateAdmin_HashUsesTheSameArgon2idFormatAsHTTPEnrollment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	admin, err := CreateAdmin(CreateAdminConfig{StorePath: path, Username: "bm-admin", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	// verifyPassword is the exact function handleLogin calls against a
	// record it read from the store (handler.go); if CreateAdmin ever
	// hashed with anything else, this is where that would show up.
	if err := verifyPassword(admin.PasswordHash, "correct-horse-battery-staple"); err != nil {
		t.Errorf("verifyPassword against a CreateAdmin-produced hash: %v", err)
	}
	if err := verifyPassword(admin.PasswordHash, "wrong-password"); !errors.Is(err, ErrPasswordMismatch) {
		t.Errorf("verifyPassword(wrong password) = %v, want ErrPasswordMismatch", err)
	}
}

func TestCreateAdmin_RefusesAnEmptyUsername(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if _, err := CreateAdmin(CreateAdminConfig{StorePath: path, Username: "", Password: "correct-horse-battery-staple"}); err == nil {
		t.Fatal("CreateAdmin with an empty username = nil error, want a refusal")
	}
}

func TestCreateAdmin_RefusesAPasswordShorterThanTheHTTPEnrollmentMinimum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	// minPasswordLength (handler.go) is 12; this deliberately stays one
	// character under it so a change to that constant cannot silently
	// make this test meaningless.
	tooShort := "short-pw-11"
	if len(tooShort) >= minPasswordLength {
		t.Fatalf("test fixture password %q is %d chars, want fewer than minPasswordLength (%d)", tooShort, len(tooShort), minPasswordLength)
	}
	if _, err := CreateAdmin(CreateAdminConfig{StorePath: path, Username: "bm-admin", Password: tooShort}); err == nil {
		t.Fatal("CreateAdmin with a too-short password = nil error, want a refusal")
	}
}

// TestCreateAdmin_IsSingleShotJustLikeHTTPEnrollment proves CreateAdmin
// funnels through store.go's own Store.Enroll guard (§49.1: "single-shot
// and irreversible") rather than reimplementing that rule - a second call
// must fail with the exact same ErrAlreadyEnrolled a second HTTP /enroll
// would.
func TestCreateAdmin_IsSingleShotJustLikeHTTPEnrollment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if _, err := CreateAdmin(CreateAdminConfig{StorePath: path, Username: "bm-admin", Password: "correct-horse-battery-staple"}); err != nil {
		t.Fatalf("first CreateAdmin: %v", err)
	}
	_, err := CreateAdmin(CreateAdminConfig{StorePath: path, Username: "someone-else", Password: "another-long-enough-password"})
	if !errors.Is(err, ErrAlreadyEnrolled) {
		t.Fatalf("second CreateAdmin error = %v, want ErrAlreadyEnrolled", err)
	}

	// And the first administrator must survive untouched, exactly as
	// store_test.go's TestStore_RejectsASecondEnrollAndKeepsTheFirstAdmin
	// already requires of the HTTP path.
	admin, err := NewStore(path).Admin()
	if err != nil {
		t.Fatalf("Admin: %v", err)
	}
	if admin.Username != "bm-admin" {
		t.Errorf("Admin().Username after a rejected second CreateAdmin = %q, want the original %q", admin.Username, "bm-admin")
	}
}

// TestCreateAdmin_RefusesWhileARunningServiceHoldsTheStore is this
// issue's concurrency-safety requirement: CreateAdmin writes directly to
// the same on-disk file a live Service's Store.Enroll/Store.SetPassword
// read-modify-write cycle also touches. Running it while a server is up
// is exactly the unsafe case (docs/EPIC-B-multi-nas.md §49.1 says nothing
// about this directly, but store.go's own doc already assumes "a single
// process owns path" - this is that assumption enforced rather than
// hoped for), so it must be refused, not raced.
func TestCreateAdmin_RefusesWhileARunningServiceHoldsTheStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")

	// Stands in for `serve` already running against this exact store -
	// New acquires the store's exclusive lock and (deliberately) never
	// releases it for as long as this Service value is reachable,
	// exactly like the real long-lived server process.
	svc, err := New(Config{StorePath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = CreateAdmin(CreateAdminConfig{StorePath: path, Username: "bm-admin", Password: "correct-horse-battery-staple"})
	if !errors.Is(err, ErrStoreLocked) {
		t.Fatalf("CreateAdmin while a Service holds the store: error = %v, want ErrStoreLocked", err)
	}

	// The running server's own view of the store must be unaffected by
	// the refused attempt: still no administrator, still able to enroll
	// normally through the HTTP path once this test proves that below.
	needsEnrollment, err := svc.NeedsEnrollment()
	if err != nil {
		t.Fatalf("NeedsEnrollment: %v", err)
	}
	if !needsEnrollment {
		t.Error("NeedsEnrollment() after a refused CreateAdmin = false, want true (nothing should have been written)")
	}
}

// TestCreateAdmin_ThenTheSameCredentialsLogInThroughTheNormalHTTPFlow is
// the critical proof this issue asks for: an administrator CreateAdmin
// provisions has to be indistinguishable, to the HTTP layer, from one who
// enrolled through a browser. This runs CreateAdmin the way an operator
// actually would - BEFORE any Service exists for this store, i.e. before
// the server has ever started - and only then brings up a real
// Service+http.Handler and drives POST /login exactly the way
// handler_test.go's own testServer helper does.
func TestCreateAdmin_ThenTheSameCredentialsLogInThroughTheNormalHTTPFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	const username = "bm-admin"
	const password = "correct-horse-battery-staple"

	if _, err := CreateAdmin(CreateAdminConfig{StorePath: path, Username: username, Password: password}); err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}

	// Only now does the server start, against the store CreateAdmin just
	// wrote to - CreateAdmin's own lock was released when it returned
	// above, so this must succeed rather than hit ErrStoreLocked.
	svc, err := New(Config{StorePath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	needsEnrollment, err := svc.NeedsEnrollment()
	if err != nil {
		t.Fatalf("NeedsEnrollment: %v", err)
	}
	if needsEnrollment {
		t.Fatal("NeedsEnrollment() = true after CreateAdmin, want false: the server should see an administrator already exists and never print/accept a bootstrap token")
	}

	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", http.StripPrefix("/api/v1/auth", svc.Handler()))
	server := httptest.NewServer(EnsureCSRFCookie(false)(mux))
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar}

	// Seed the CSRF cookie the same way a real browser's first page load
	// would, and read it back the same way handler_test.go's own
	// seedCSRFCookie/csrfTokenFromJar helpers do (this file is in the
	// same package, so it reuses them rather than re-deriving the same
	// choreography).
	seedCSRFCookie(t, client, server)
	csrfToken := csrfTokenFromJar(t, client, server)

	loginResp := postJSON(t, client, server.URL+"/api/v1/auth/login",
		credentialsRequest{Username: username, Password: password},
		map[string]string{CSRFHeaderName: csrfToken})
	defer loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /login with CreateAdmin-provisioned credentials: status = %d, want %d", loginResp.StatusCode, http.StatusNoContent)
	}

	// GET /session must now report the same session cookie /login just
	// set is authenticated as this exact administrator - proof the two
	// entry points produced the one same, compatible piece of state.
	sessionResp, err := client.Get(server.URL + "/api/v1/auth/session")
	if err != nil {
		t.Fatalf("GET session: %v", err)
	}
	defer sessionResp.Body.Close()
	if sessionResp.StatusCode != http.StatusOK {
		t.Fatalf("GET session after login: status = %d, want %d", sessionResp.StatusCode, http.StatusOK)
	}
	var got sessionResponse
	if err := json.NewDecoder(sessionResp.Body).Decode(&got); err != nil {
		t.Fatalf("decode session response: %v", err)
	}
	if got.Username != username {
		t.Errorf("session username = %q, want %q", got.Username, username)
	}
}

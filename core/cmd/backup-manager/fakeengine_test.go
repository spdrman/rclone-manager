package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/apicontract"
	"github.com/spdrman/rclone-manager/core/service"
)

// A stand-in for the engine, with the real service layer behind it.
//
// It exists because of what #543 has to prove. The claim is not "the CLI
// sends some JSON somewhere"; it is that the same input gets the same
// answer whichever route carries it, and an answer invented by a test
// double proves nothing about that. So this serves the contract's own
// operations over a REAL core/service.BackupService, opened on a
// configuration of its own. A refusal that comes back over this wire is
// the refusal core/service produced, not a sentence somebody typed into a
// fixture, which is what makes the parity test able to fail.
//
// What is hand-written here is the part core cannot borrow: the mapping
// from a service error to a status and a contract error code lives in
// apps/common/webhost, and core may not import apps (the dependency rule
// scripts/architecture/check-core-dependency-rule.sh enforces). So this
// mirrors it, and the mirroring is deliberately narrow: only the codes the
// contract declares for each operation, so a mistake here is caught by the
// client's own contract check rather than passed off as an engine answer.
//
// It also carries the two things every /api/v1 response really carries and
// this client really needs: the double-submit CSRF cookie issued in front
// of every response, and a session cookie that a login mints and an
// authenticated route requires.

// fakeEngine is one engine: a real BackupService, an HTTP surface over it,
// and the session and CSRF machinery the client has to satisfy.
type fakeEngine struct {
	t   *testing.T
	svc *service.BackupService

	// configPath is the configuration THIS engine serves, which is
	// deliberately not the one the CLI under test was pointed at: a write
	// that lands here and not there is the whole of what routing means.
	configPath string

	username string
	password string

	server *httptest.Server

	mu       sync.Mutex
	sessions map[string]bool
	csrf     map[string]bool
	seen     []string
}

// startFakeEngine opens a BackupService over configPath and serves it.
func startFakeEngine(t *testing.T, configPath string) *fakeEngine {
	t.Helper()
	svc, closeFn, err := service.Open(t.Context(), configPath)
	if err != nil {
		t.Fatalf("opening the engine's own service over %s: %v", configPath, err)
	}
	t.Cleanup(func() {
		if err := closeFn(); err != nil {
			t.Errorf("closing the engine's service: %v", err)
		}
	})

	e := &fakeEngine{
		t:          t,
		svc:        svc,
		configPath: configPath,
		username:   "operator",
		// Never a real credential, and never written anywhere: this pair
		// only ever exists in this process's memory and in the fake
		// engine's own comparison.
		password: "not-a-real-password",
		sessions: map[string]bool{},
		csrf:     map[string]bool{},
	}
	e.server = httptest.NewServer(e)
	t.Cleanup(e.server.Close)
	return e
}

// baseURL is the address the CLI is told to reach this engine at.
func (e *fakeEngine) baseURL() string { return e.server.URL }

// requests is every request this engine received, as "METHOD path".
func (e *fakeEngine) requests() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.seen...)
}

// attach points the CLI under test at this engine, through the same
// environment an operator would set.
//
// The names are spelled out here rather than read from the constants the
// command uses, deliberately. They are an operator-facing contract: a
// rename is something a deployment feels, so this has to notice one rather
// than follow it.
func (e *fakeEngine) attach(t *testing.T) {
	t.Helper()
	t.Setenv("BACKUP_MANAGER_API_URL", e.baseURL())
	t.Setenv("BACKUP_MANAGER_API_USERNAME", e.username)
	t.Setenv("BACKUP_MANAGER_API_PASSWORD", e.password)
}

func (e *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, apicontract.BasePath)
	e.mu.Lock()
	e.seen = append(e.seen, r.Method+" "+path)
	e.mu.Unlock()

	// The runtime issues this in front of every response it serves
	// (apps/common/csrf's EnsureCSRFCookie), which is what a
	// double-submit scheme means: the client can only echo a value the
	// server gave it.
	token := e.issueCSRF(w, r)

	switch {
	case r.Method == http.MethodGet && path == "/auth/session":
		e.session(w, r)
	case r.Method == http.MethodPost && path == "/auth/login":
		e.login(w, r)
	case r.Method == http.MethodPost && path == "/auth/logout":
		w.WriteHeader(http.StatusNoContent)
	default:
		e.api(w, r, path, token)
	}
}

// issueCSRF hands out the double-submit cookie, reusing whatever the
// caller already carries so a second request echoes a value this engine
// still recognises.
func (e *fakeEngine) issueCSRF(w http.ResponseWriter, r *http.Request) string {
	if cookie, err := r.Cookie("bm_csrf"); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	token := "csrf-" + time.Now().Format("150405.000000000")
	e.mu.Lock()
	e.csrf[token] = true
	e.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "bm_csrf", Value: token, Path: "/"})
	return token
}

func (e *fakeEngine) signedIn(r *http.Request) bool {
	cookie, err := r.Cookie("bm_session")
	if err != nil || cookie.Value == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sessions[cookie.Value]
}

// session is GET /auth/session, which answers the flat envelope
// apps/common/auth/local answers with rather than the nested one the rest
// of /api/v1 uses.
func (e *fakeEngine) session(w http.ResponseWriter, r *http.Request) {
	if !e.signedIn(r) {
		writeJSON(w, http.StatusUnauthorized, apicontract.AuthErrorResponse{
			Code:    apicontract.ErrorCodeUnauthenticated,
			Message: "no session",
		})
		return
	}
	writeJSON(w, http.StatusOK, apicontract.SessionResponse{Username: e.username})
}

func (e *fakeEngine) login(w http.ResponseWriter, r *http.Request) {
	var body apicontract.CredentialsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, apicontract.AuthErrorResponse{
			Code: apicontract.ErrorCodeInvalidRequest, Message: "unreadable body",
		})
		return
	}
	if body.Username != e.username || body.Password != e.password {
		writeJSON(w, http.StatusUnauthorized, apicontract.AuthErrorResponse{
			Code: apicontract.ErrorCodeUnauthenticated, Message: "wrong credentials",
		})
		return
	}
	token := "session-" + time.Now().Format("150405.000000000")
	e.mu.Lock()
	e.sessions[token] = true
	e.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "bm_session", Value: token, Path: "/"})
	w.WriteHeader(http.StatusNoContent)
}

// api is every route that goes through the engine's own middleware:
// authenticated, and for a mutation, double-submit checked.
func (e *fakeEngine) api(w http.ResponseWriter, r *http.Request, path, token string) {
	if !e.signedIn(r) {
		refuse(w, http.StatusUnauthorized, apicontract.ErrorCodeUnauthenticated, "no session")
		return
	}
	if r.Method != http.MethodGet {
		if got := r.Header.Get("X-CSRF-Token"); got == "" {
			refuse(w, http.StatusForbidden, apicontract.ErrorCodeCSRFTokenMissing, "no double-submit token")
			return
		} else if got != token {
			refuse(w, http.StatusForbidden, apicontract.ErrorCodeCSRFTokenMismatch, "the double-submit token does not match the cookie")
			return
		}
	}

	switch {
	case r.Method == http.MethodPost && path == "/ssh-keys":
		e.importKey(w, r)
	case r.Method == http.MethodPost && path == "/ssh/host-key-probe":
		e.probeHostKey(w, r)
	case r.Method == http.MethodPost && path == "/backup-sets":
		e.createBackupSet(w, r)
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/backup-sets/"):
		e.updateBackupSet(w, r, strings.TrimPrefix(path, "/backup-sets/"))
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/backup-sets/"):
		e.removeBackupSet(w, r, strings.TrimPrefix(path, "/backup-sets/"))
	case r.Method == http.MethodGet && path == "/settings":
		e.getSettings(w, r)
	case r.Method == http.MethodPatch && path == "/settings":
		e.updateSettings(w, r)
	default:
		refuse(w, http.StatusNotFound, apicontract.ErrorCodeInternal, "this fake engine serves no "+r.Method+" "+path)
	}
}

func (e *fakeEngine) importKey(w http.ResponseWriter, r *http.Request) {
	var body apicontract.ImportSSHKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		refuse(w, http.StatusBadRequest, apicontract.ErrorCodeInvalidRequest, err.Error())
		return
	}
	ref, err := e.svc.ImportSSHKey(r.Context(), []byte(body.PrivateKeyPEM), body.Passphrase)
	if err != nil {
		refuse(w, http.StatusBadRequest, apicontract.ErrorCodeInvalidRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, apicontract.ImportSSHKeyResponse{
		ID: ref.ID, Algorithm: ref.Algorithm, Fingerprint: ref.Fingerprint,
	})
}

func (e *fakeEngine) probeHostKey(w http.ResponseWriter, r *http.Request) {
	var body apicontract.HostKeyProbeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		refuse(w, http.StatusBadRequest, apicontract.ErrorCodeInvalidRequest, err.Error())
		return
	}
	probe, err := e.svc.ProbeHostKey(r.Context(), body.Host, body.Port)
	if err != nil {
		refuse(w, http.StatusBadRequest, apicontract.ErrorCodeHostKeyProbeFailed, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, apicontract.HostKeyProbeResponse{
		Algorithm: probe.Algorithm, Fingerprint: probe.Fingerprint, KnownHostsLine: probe.KnownHostsLine,
	})
}

func (e *fakeEngine) createBackupSet(w http.ResponseWriter, r *http.Request) {
	var body apicontract.CreateBackupSetRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		refuse(w, http.StatusBadRequest, apicontract.ErrorCodeInvalidRequest, err.Error())
		return
	}
	req := service.CreateBackupSetRequest{
		SourceName:         body.SourceName,
		Name:               body.Name,
		Host:               body.Host,
		Port:               body.Port,
		User:               body.User,
		SSHKeyID:           body.SSHKeyID,
		KnownHostsLine:     body.KnownHostsLine,
		RemotePath:         body.RemotePath,
		LocalPath:          body.LocalPath,
		Include:            body.Include,
		CompletionStrategy: body.CompletionStrategy,
		StableFor:          time.Duration(body.StableForSeconds) * time.Second,
		StaleAfter:         time.Duration(body.StaleAfterSeconds) * time.Second,
		ValidatorID:        service.ValidatorID(body.ValidatorID),
		Disabled:           body.Disabled,
		ReadOnly:           body.ReadOnly,
		RunImmediately:     body.RunImmediately,
		// The actor is the SESSION's, never the request's. That is the
		// whole authorization argument for routing: an engine records who
		// asked, and the caller does not get to say.
		Actor:              e.username,
		AcknowledgeRepoint: body.AcknowledgeRepoint,
	}
	result, err := e.svc.CreateBackupSet(r.Context(), req)
	if err != nil {
		refuseServiceError(w, "createBackupSet", err)
		return
	}
	writeJSON(w, http.StatusCreated, apicontract.CreateBackupSetResponse{
		BackupSet: toContractBackupSet(result.Set),
	})
}

func (e *fakeEngine) updateBackupSet(w http.ResponseWriter, r *http.Request, id string) {
	var body apicontract.UpdateBackupSetRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		refuse(w, http.StatusBadRequest, apicontract.ErrorCodeInvalidRequest, err.Error())
		return
	}
	req := service.UpdateBackupSetRequest{
		Host:               body.Host,
		Port:               body.Port,
		User:               body.User,
		RemotePath:         body.RemotePath,
		LocalPath:          body.LocalPath,
		Include:            body.Include,
		CompletionStrategy: body.CompletionStrategy,
		AcknowledgeRepoint: body.AcknowledgeRepoint,
	}
	if body.StableForSeconds != nil {
		d := time.Duration(*body.StableForSeconds) * time.Second
		req.StableFor = &d
	}
	if body.StaleAfterSeconds != nil {
		d := time.Duration(*body.StaleAfterSeconds) * time.Second
		req.StaleAfter = &d
	}
	if body.ValidatorID != nil {
		v := service.ValidatorID(*body.ValidatorID)
		req.ValidatorID = &v
	}
	updated, err := e.svc.UpdateBackupSet(r.Context(), id, req)
	if err != nil {
		refuseServiceError(w, "updateBackupSet", err)
		return
	}
	writeJSON(w, http.StatusOK, toContractBackupSet(updated))
}

func (e *fakeEngine) removeBackupSet(w http.ResponseWriter, r *http.Request, id string) {
	if err := e.svc.RemoveBackupSet(r.Context(), id); err != nil {
		refuseServiceError(w, "removeBackupSet", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (e *fakeEngine) getSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := e.svc.Settings(r.Context())
	if err != nil {
		refuseServiceError(w, "getSettings", err)
		return
	}
	writeJSON(w, http.StatusOK, toContractSettings(settings))
}

func (e *fakeEngine) updateSettings(w http.ResponseWriter, r *http.Request) {
	var body apicontract.UpdateSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		refuse(w, http.StatusBadRequest, apicontract.ErrorCodeInvalidRequest, err.Error())
		return
	}
	req := service.UpdateSettingsRequest{AcknowledgeMediumDisclosure: body.AcknowledgeMediumDisclosure}
	if body.Retention != nil {
		retention := service.RetentionUpdate{
			Timezone:             body.Retention.Timezone,
			WeekStartsOn:         body.Retention.WeekStartsOn,
			ProtectLastKnownGood: body.Retention.ProtectLastKnownGood,
		}
		for _, t := range body.Retention.Tiers {
			retention.Tiers = append(retention.Tiers, service.RetentionTier{
				Name: t.Name, Granularity: t.Granularity, PeriodDays: t.PeriodDays,
				Keep: t.Keep, WindowUnit: t.WindowUnit, Medium: t.Medium,
			})
		}
		req.Retention = &retention
	}
	if body.Capacity != nil {
		req.Capacity = &service.CapacityUpdate{
			CapBytes:          body.Capacity.CapBytes,
			WarningFreeBytes:  body.Capacity.WarningFreeBytes,
			CriticalFreeBytes: body.Capacity.CriticalFreeBytes,
			SafetyMarginBytes: body.Capacity.SafetyMarginBytes,
		}
	}
	settings, err := e.svc.UpdateSettings(r.Context(), req)
	if err != nil {
		refuseServiceError(w, "updateSettings", err)
		return
	}
	writeJSON(w, http.StatusOK, toContractSettings(settings))
}

// toContractSettings mirrors apps/common/webhost's own settings response,
// minus the schema block, which service.Settings does not carry and
// nothing the CLI prints reads.
func toContractSettings(s service.Settings) apicontract.SettingsResponse {
	out := apicontract.SettingsResponse{
		Retention: apicontract.RetentionSettings{
			Timezone:             s.Retention.Timezone,
			WeekStartsOn:         s.Retention.WeekStartsOn,
			ProtectLastKnownGood: s.Retention.ProtectLastKnownGood,
		},
		Capacity: apicontract.CapacitySettings{
			CapBytes:             s.Capacity.CapBytes,
			WarningFreeBytes:     s.Capacity.WarningFreeBytes,
			CriticalFreeBytes:    s.Capacity.CriticalFreeBytes,
			SafetyMarginBytes:    s.Capacity.SafetyMarginBytes,
			BackupRoot:           s.Capacity.BackupRoot,
			BackupRootConfigured: s.Capacity.BackupRootConfigured,
		},
	}
	for _, t := range s.Retention.Tiers {
		out.Retention.Tiers = append(out.Retention.Tiers, apicontract.RetentionTier{
			Name: t.Name, Granularity: t.Granularity, PeriodDays: t.PeriodDays,
			Keep: t.Keep, WindowUnit: t.WindowUnit, Medium: t.Medium,
		})
	}
	for _, m := range s.Mediums {
		out.Mediums = append(out.Mediums, apicontract.StorageMediumSummary{
			ID: m.ID, Type: m.Type, Bucket: m.Bucket, Region: m.Region,
			StorageClass: m.StorageClass, ReadsRequireRestore: m.ReadsRequireRestore,
		})
	}
	return out
}

// toContractBackupSet mirrors apps/common/webhost's toBackupSetResponse,
// including the field it does NOT carry: the API reports no stale_after at
// all, which is a real gap and not something a fixture may paper over.
func toContractBackupSet(s service.BackupSet) apicontract.BackupSet {
	return apicontract.BackupSet{
		ID:                  s.ID,
		SourceName:          s.SourceName,
		Name:                s.Name,
		Host:                s.Host,
		Port:                s.Port,
		User:                s.User,
		RemotePath:          s.RemotePath,
		LocalPath:           s.LocalPath,
		Include:             s.Include,
		CompletionStrategy:  s.CompletionStrategy,
		StableForSeconds:    int(s.StableFor / time.Second),
		ValidatorID:         string(s.ValidatorID),
		Disabled:            s.Disabled,
		ReadOnly:            s.ReadOnly,
		RetentionIsOverride: s.RetentionIsOverride,
	}
}

// refuseServiceError maps a core/service refusal onto the status and code
// the contract declares for it, which is what apps/common/webhost does for
// real. Only codes the operation declares: a code outside them is refused
// by the client as a contract violation, which would report this fixture's
// mistake as the engine's.
func refuseServiceError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidRequest):
		refuse(w, http.StatusBadRequest, apicontract.ErrorCodeInvalidRequest, err.Error())
	case errors.Is(err, service.ErrBackupSetNotFound):
		refuse(w, http.StatusNotFound, apicontract.ErrorCodeBackupSetNotFound, err.Error())
	case errors.Is(err, service.ErrRepointNotAcknowledged):
		refuse(w, http.StatusConflict, apicontract.ErrorCodeBackupSetRepointNotAcknowledged, err.Error())
	case errors.Is(err, service.ErrHistoryRepointNotAcknowledged):
		refuse(w, http.StatusConflict, apicontract.ErrorCodeBackupSetHistoryRepointNotAcknowledged, err.Error())
	default:
		refuse(w, http.StatusInternalServerError, apicontract.ErrorCodeInternal, err.Error())
	}
}

func refuse(w http.ResponseWriter, status int, code apicontract.ErrorCode, message string) {
	writeJSON(w, status, apicontract.ErrorResponse{Error: apicontract.ErrorBody{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status == http.StatusNoContent {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

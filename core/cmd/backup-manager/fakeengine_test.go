package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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

	// anonymous makes GET /system/version answer with no deployment_id,
	// which is what every engine older than #555 does, what an engine
	// that could not read its own identity file does, and what an engine
	// holding no deployment at all does. The client cannot tell those
	// apart and must not read any of them as agreement: two processes
	// that both name nothing are not thereby one deployment.
	anonymous bool

	server *httptest.Server

	mu       sync.Mutex
	sessions map[string]bool
	csrf     map[string]bool
	seen     []string
}

// startFakeEngine announces that this process serves the deployment
// configPath names, opens a BackupService over it, and serves it.
//
// Announce first, then open, which is the order backup-manager-web and
// `backup-manager daemon` use and which is now load-bearing rather than
// tidy: core/service mints a deployment identity in AnnounceServing and
// nowhere else (#555 as #559 left it), so an engine that opened first
// would cache an empty identity and every routed write against it would
// be refused for naming no deployment.
//
// It is also what makes this fixture an engine rather than a web server
// with a BackupService behind it. A CLI decides engine-attached mode from
// the serving lock, so a test that wanted one used to announce separately
// beside this; the announcement belongs to the thing that serves.
func startFakeEngine(t *testing.T, configPath string) *fakeEngine {
	t.Helper()
	release, err := service.AnnounceServing(configPath)
	if err != nil {
		t.Fatalf("announcing this process as serving %s: %v", configPath, err)
	}
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("releasing the serving announcement: %v", err)
		}
	})

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

// startFakeEngineFor stands an engine up over a configuration file of its
// own that names the SAME deployment as cliConfig.
//
// Two files, one deployment, and both halves matter. Two files is what
// lets a test assert that a routed write changed the engine's
// configuration and left the CLI's alone, which is the whole of what
// routing means. One deployment is what #555 made necessary: a routed
// write now asks the engine which deployment it serves and refuses when
// it is not the one the command was typed at, so a fixture with two
// journals would drive that refusal on every row instead of the route the
// row is about. It is also the truthful arrangement, since a real CLI and
// a real engine share one config.yaml and one journal.
func startFakeEngineFor(t *testing.T, cliConfig string) *fakeEngine {
	t.Helper()
	return startFakeEngine(t, writeTestConfigFor(t, cliConfig))
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
	case r.Method == http.MethodGet && path == "/system/version":
		e.systemVersion(w, r)
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

// systemVersion is GET /system/version, and it is the call a routed write
// makes before it sends anything (#555).
//
// Every value comes from the real BackupService behind this engine, the
// deployment identity included. That is what makes the check under test
// able to fail: an identity typed into a fixture would agree with whatever
// the test wanted it to agree with, and the whole question is whether the
// engine at the other end is the deployment the command was typed at.
func (e *fakeEngine) systemVersion(w http.ResponseWriter, _ *http.Request) {
	v := service.BuildVersion("dev", "none")
	deploymentID := e.svc.DeploymentID()
	if e.anonymous {
		deploymentID = ""
	}
	writeJSON(w, http.StatusOK, apicontract.VersionResponse{
		APIVersion:     apicontract.Version,
		CoreVersion:    v.CoreVersion,
		Commit:         v.Commit,
		GoVersion:      v.GoVersion,
		EngineVersion:  v.EngineVersion,
		ConfigRevision: e.svc.ConfigRevision(),
		DeploymentID:   deploymentID,
		Ready:          e.svc.Ready(),
		Configured:     true,
	})
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
// field for field.
//
// Field for field is the whole contract of this function, and it was
// broken in the commit that made stale_after reportable: the API grew
// stale_after_seconds and this stayed as it was, so every routed test in
// this package went on exercising the legacy branch, a routed create went
// on printing "stale_after: not reported", and the sentence #555 says it
// removed was still being produced under a green suite. A fixture that
// carries less than production is not a smaller fixture, it is a
// different product.
//
// TestTheFixtureCarriesEveryFieldTheContractHas is the control that keeps
// this honest, because a missing field is invisible here: the zero value
// is a legal value for every one of them.
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
		StaleAfterSeconds:   int(s.StaleAfter / time.Second),
		ValidatorID:         string(s.ValidatorID),
		Disabled:            s.Disabled,
		ReadOnly:            s.ReadOnly,
		RetentionIsOverride: s.RetentionIsOverride,
		// Issue #572: what the set's known_hosts actually pins. Carried
		// for the reason the guard below states, and the guard is what
		// made me carry it: a fixture engine that reports no host key is
		// one no routed test can catch dropping it.
		TrustedHostKeys:          toContractTrustedHostKeys(s.TrustedHostKeys),
		TrustedHostKeyRecordedAt: contractTimeOrEmpty(s.TrustedHostKeyRecordedAt),
	}
}

// toContractTrustedHostKeys mirrors toBackupSetResponse's own loop,
// including its nil-for-none: absent on the wire is "this deployment could
// not report what this set trusts", and a fixture that sent [] instead
// would be answering a different question from production.
func toContractTrustedHostKeys(keys []service.TrustedHostKey) []apicontract.TrustedHostKey {
	var out []apicontract.TrustedHostKey
	for _, k := range keys {
		out = append(out, apicontract.TrustedHostKey{Algorithm: k.Algorithm, Fingerprint: k.Fingerprint})
	}
	return out
}

// contractTimeOrEmpty renders a moment the way toBackupSetResponse does,
// and the zero time as the empty string the omitempty tag drops.
func contractTimeOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// TestTheFixtureCarriesEveryFieldTheContractHas is the control that makes
// a sparse fixture visible.
//
// toContractBackupSet's whole job is to mirror apps/common/webhost's
// toBackupSetResponse, and a field it drops is invisible in every
// assertion written over it: the zero value is a legal value for all
// sixteen, so a routed test asserting on a set the engine returned passes
// whether or not the field made the trip. That is how stale_after_seconds
// went missing for a whole commit while a routed create went on printing
// "stale_after: not reported" under a green suite.
//
// So the mirroring is driven from the other end. Every field of
// service.BackupSet is given a value nothing here would produce by
// accident, and every field of the apicontract.BackupSet that comes out
// has to be non-zero. A field that legitimately cannot carry one belongs
// in exemptFromTheFixture, named, with the reason beside it, which makes
// leaving one out a decision somebody wrote down.
func TestTheFixtureCarriesEveryFieldTheContractHas(t *testing.T) {
	// Deliberately unlike anything the rest of this package builds: this
	// is about whether a value survives the mapping, not about whether it
	// is a plausible backup set.
	full := service.BackupSet{
		ID:                  "control/every-field",
		SourceName:          "control",
		Name:                "every-field",
		Host:                "control.example.internal",
		Port:                2201,
		User:                "controluser",
		RemotePath:          "/srv/control",
		LocalPath:           "/data/control",
		Include:             []string{"*.control"},
		CompletionStrategy:  "stable",
		StableFor:           90 * time.Second,
		StaleAfter:          36 * time.Hour,
		ValidatorID:         service.ValidatorID("control-validator"),
		Disabled:            true,
		ReadOnly:            true,
		RetentionIsOverride: true,
		TrustedHostKeys: []service.TrustedHostKey{
			{Algorithm: "control-algorithm", Fingerprint: "SHA256:controlfingerprint"},
		},
		TrustedHostKeyRecordedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
	}

	// Nothing is exempt today, and that is the point of writing the list
	// out: a field added to the contract that this fixture cannot carry
	// has to be argued for here rather than quietly left at zero.
	exemptFromTheFixture := map[string]string{}

	got := toContractBackupSet(full)
	v := reflect.ValueOf(got)
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		if reason, exempt := exemptFromTheFixture[name]; exempt {
			if !v.Field(i).IsZero() {
				t.Errorf("%s is listed as a field this fixture cannot carry (%s) and it carried one anyway; the list is out of date", name, reason)
			}
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("toContractBackupSet drops %s, so every routed test in this package exercises an engine that does not report it and none exercises one that does. apps/common/webhost's toBackupSetResponse carries it, and a fixture that carries less than production is a different product", name)
		}
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

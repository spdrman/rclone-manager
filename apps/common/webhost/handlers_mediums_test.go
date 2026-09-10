package webhost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

// The medium preflight, whose most important test is about what the
// response does not contain.
//
// A preflight talks to real storage with real credentials, and the natural
// way to report a failure is to pass the provider's own error text
// through. That text can carry a signed URL, a bucket listing or an
// account identifier, so one case here reads the whole response body and
// asserts no credential and no hint of where the credential came from
// appears anywhere in it.
//
// The other shape worth stating is that a medium which fails its checks is
// a successful request. The route's job is to report what happened, and
// answering 500 would conflate "this medium is misconfigured", which the
// operator can fix, with "this endpoint is broken", which they cannot.

// mediumPreflightCanary is a value that exists nowhere else in this
// repository, so finding it in a response is proof of where it came from.
// The E1.3 shape, reused against the surface issue #443 adds.
const mediumPreflightCanary = "CANARY-443-web-3ae91f7c05d6-DO-NOT-SERVE"

func workingPreflight() service.MediumPreflight {
	return service.MediumPreflight{
		Medium: "offsite_s3",
		OK:     true,
		Checks: []service.MediumPreflightCheck{
			{Step: "credentials", Outcome: "passed", Detail: "the credential was obtained and the endpoint accepted it"},
			{Step: "reach", Outcome: "passed", Detail: `the endpoint answered and holds bucket "nas-backups"`},
			{Step: "deliverable", Outcome: "passed", Detail: "storage class STANDARD_IA reads on demand"},
			{Step: "write", Outcome: "passed", Detail: "an object was written"},
			{Step: "read_back", Outcome: "passed", Detail: "the object was read back and is byte for byte what was written"},
			{Step: "storage_class", Outcome: "passed", Detail: "the endpoint stored the object as STANDARD_IA"},
			{Step: "verification", Outcome: "passed", Detail: "the content class is what the read-back step just did"},
			{Step: "delete", Outcome: "passed", Detail: "the probe object was deleted, and the endpoint confirms it is gone"},
		},
	}
}

func TestPreflightStorageMedium_ReportsEveryStepAndNamesTheMedium(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.mediumPreflight = workingPreflight()

	rec := rt.post(t, "/api/v1/storage-mediums/offsite_s3/preflight", "")
	mustStatus(t, rec, http.StatusOK)

	if got := rt.backend.lastPreflightedMedium; got != "offsite_s3" {
		t.Errorf("the handler asked about %q, want offsite_s3", got)
	}

	var body mediumPreflightResponse
	decodeInto(t, rec, &body)
	if body.Medium != "offsite_s3" || !body.OK {
		t.Fatalf("body = %+v", body)
	}
	if len(body.Checks) != 8 {
		t.Fatalf("body carries %d checks, want the engine's full list of 8: %+v", len(body.Checks), body.Checks)
	}
	for _, c := range body.Checks {
		if c.Step == "" || c.Outcome == "" || c.Detail == "" {
			t.Errorf("a check reached the wire with a hole in it: %+v", c)
		}
	}
}

// TestPreflightStorageMedium_AFailingMediumIs200AndNot500 is the rule this
// surface shares with the backup-set connection test: a bucket that is not
// there is what an operator did, not what broke, and a 500 would put it on
// the "something is wrong with your manager" pile.
func TestPreflightStorageMedium_AFailingMediumIs200AndNot500(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.mediumPreflight = service.MediumPreflight{
		Medium: "offsite_s3",
		OK:     false,
		Checks: []service.MediumPreflightCheck{
			{Step: "credentials", Outcome: "passed", Detail: "the credential was obtained"},
			{Step: "reach", Outcome: "failed", Category: "configuration", Detail: `the endpoint answered and does not have bucket "nas-backups"`},
			{Step: "deliverable", Outcome: "skipped", Detail: "the endpoint could not be reached"},
			{Step: "write", Outcome: "skipped", Detail: "nothing was written"},
			{Step: "read_back", Outcome: "skipped", Detail: "nothing was written"},
			{Step: "storage_class", Outcome: "skipped", Detail: "nothing was written"},
			{Step: "verification", Outcome: "skipped", Detail: "nothing was written"},
			{Step: "delete", Outcome: "skipped", Detail: "nothing was written"},
		},
	}

	rec := rt.post(t, "/api/v1/storage-mediums/offsite_s3/preflight", "")
	mustStatus(t, rec, http.StatusOK)

	var body mediumPreflightResponse
	decodeInto(t, rec, &body)
	if body.OK {
		t.Fatal("a medium naming a bucket that is not there came back ok")
	}
	// The category survives to the wire, because it is the machine-readable
	// half a client branches on. Without it a client is left parsing an
	// English sentence to decide what to say.
	var reach mediumPreflightCheck
	for _, c := range body.Checks {
		if c.Step == "reach" {
			reach = c
		}
	}
	if reach.Outcome != "failed" || reach.Category != "configuration" {
		t.Fatalf("reach check = %+v, want a failed configuration verdict", reach)
	}
}

func TestPreflightStorageMedium_AnUndeclaredMediumIs404AndNamed(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.errOnMediumPreflight = fmt.Errorf("%w: typo_s3", service.ErrMediumNotFound)

	rec := rt.post(t, "/api/v1/storage-mediums/typo_s3/preflight", "")
	mustStatus(t, rec, http.StatusNotFound)
	if got := responseErrorCode(rec.Body.String()); got != "MEDIUM_NOT_FOUND" {
		t.Fatalf("error code = %q, want MEDIUM_NOT_FOUND", got)
	}
}

// TestPreflightStorageMedium_NeverReturnsACredentialOrWhereItCameFrom is
// #443's own acceptance line: the E1.3 redaction canary, reused against
// this response.
//
// It plants the canary in the one place a leak would realistically come
// from, which is a check detail carrying something the transport said, and
// then proves it does not reach the wire. The engine is what makes that
// true structurally (core/internal/mediumcheck never copies an underlying
// error's text into a Report), and this asserts the handler did not
// reintroduce it: a projection that helpfully appended err.Error() to a
// detail would pass every test above and fail this one.
func TestPreflightStorageMedium_NeverReturnsACredentialOrWhereItCameFrom(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.errOnMediumPreflight = fmt.Errorf(
		`service: preflighting storage medium offsite_s3: medium "offsite_s3": resolving credentials from environment variable "BACKUP_S3_%s": not set`,
		mediumPreflightCanary)

	rec := rt.post(t, "/api/v1/storage-mediums/offsite_s3/preflight", "")
	mustStatus(t, rec, http.StatusInternalServerError)

	// The positive control first: this test is only meaningful if the
	// canary really was in play, and an error whose text never reached the
	// handler would make the assertion below pass for the wrong reason.
	if !strings.Contains(rt.backend.errOnMediumPreflight.Error(), mediumPreflightCanary) {
		t.Fatal("the fixture no longer carries the canary, so this test proves nothing")
	}
	body := rec.Body.String()
	if strings.Contains(body, mediumPreflightCanary) {
		t.Fatalf("the preflight response carries the canary:\n%s", body)
	}
	for _, forbidden := range []string{"BACKUP_S3", "environment variable", "credentials"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the preflight response carries %q:\n%s", forbidden, body)
		}
	}
}

// ------------------------------------------- declaring one (G2.2, #594) ---

// storageCredentialCanary is a value that exists nowhere else in this
// repository, so finding it in a response, in a log line or in an echoed
// command is proof of where it came from. Same shape as
// mediumPreflightCanary above, pointed at the one route in this whole API
// that ever holds S3 credential material.
const (
	storageCredentialCanary   = "CANARY-594-web-6d20af31c9b7-DO-NOT-SERVE"
	storageCredentialCanaryID = "AKIAEXAMPLE594NOTREAL"
)

// TestImportStorageCredentials_AnswersWithAReferenceAndNeverTheMaterial
// is the one-way-door assertion. The material really does reach the
// backend, which the positive control proves, and none of it comes back.
func TestImportStorageCredentials_AnswersWithAReferenceAndNeverTheMaterial(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	rec := rt.post(t, "/api/v1/storage-credentials", fmt.Sprintf(
		`{"access_key_id":%q,"secret_access_key":%q}`, storageCredentialCanaryID, storageCredentialCanary))
	mustStatus(t, rec, http.StatusCreated)

	// The positive control: without it, an empty response body would pass
	// the leak assertion below for the wrong reason.
	if rt.backend.lastImportedSecret != storageCredentialCanary {
		t.Fatalf("the backend was handed %q, not the canary, so this test proves nothing", rt.backend.lastImportedSecret)
	}
	if rt.backend.lastImportedAccessKeyID != storageCredentialCanaryID {
		t.Fatalf("the backend was handed access key id %q, not the canary", rt.backend.lastImportedAccessKeyID)
	}

	body := rec.Body.String()
	for _, forbidden := range []string{storageCredentialCanary, storageCredentialCanaryID, "access_key", "secret"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the import response carries %q:\n%s", forbidden, body)
		}
	}

	var got struct {
		ID string `json:"id"`
	}
	decodeInto(t, rec, &got)
	if got.ID == "" {
		t.Fatal("the import returned no id, so nothing can reference the credential it wrote")
	}
	// The server-side path the id resolves to is not on the wire either.
	// A path is a fact about this host's filesystem an API caller has no
	// use for, exactly as importSSHKeyResponse declines to carry KeyFile.
	if strings.Contains(body, "s3_credentials") || strings.Contains(body, "/srv") {
		t.Fatalf("the import response carries the server-side path:\n%s", body)
	}
}

func TestImportStorageCredentials_RefusesAnEmptyKey(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rec := rt.post(t, "/api/v1/storage-credentials", `{"access_key_id":"","secret_access_key":"x"}`)
	mustStatus(t, rec, http.StatusBadRequest)
	if got := responseErrorCode(rec.Body.String()); got != "INVALID_REQUEST" {
		t.Errorf("error code = %q, want INVALID_REQUEST", got)
	}
}

// candidateBody is the wizard's step 3 request: a whole destination
// described in one go, with a credential REFERENCE and no material.
const candidateBody = `{"id":"offsite_s3","type":"s3","region":"us-east-1","bucket":"nas-backups",` +
	`"prefix":"monthly","storage_class":"STANDARD_IA","credentials":{"credentials_id":"9b41c7e2"}}`

// TestPreflightStorageMediumCandidate_ChecksSomethingNotYetDeclared is
// the route that did not exist and that made a wizard impossible: the
// by-id preflight can only check a destination already written into the
// operator's configuration.
func TestPreflightStorageMediumCandidate_ChecksSomethingNotYetDeclared(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.mediumPreflight = workingPreflight()

	rec := rt.post(t, "/api/v1/storage-mediums/preflight", candidateBody)
	mustStatus(t, rec, http.StatusOK)

	// Nothing was declared by checking it. This is the property the whole
	// verify-before-save ordering rests on.
	if len(rt.backend.mediums) != 0 {
		t.Fatalf("a candidate preflight declared %d medium(s)", len(rt.backend.mediums))
	}
	if rt.backend.lastCandidate.ID != "offsite_s3" || rt.backend.lastCandidate.Bucket != "nas-backups" {
		t.Fatalf("the candidate reached the backend as %+v", rt.backend.lastCandidate)
	}
	if rt.backend.lastCandidate.Credentials.ID != "9b41c7e2" {
		t.Errorf("the credentials_id did not cross the boundary: %+v", rt.backend.lastCandidate.Credentials)
	}

	var got mediumPreflightResponse
	decodeInto(t, rec, &got)
	if len(got.Checks) != 8 {
		t.Fatalf("the candidate report carries %d checks, want all 8; a surface that drops the skipped ones shows a shorter list on a failure than on a success", len(got.Checks))
	}
}

// TestPreflightStorageMediumCandidate_RendersEverySkippedStep is the
// rendering rule this issue asks for in as many words: never a single OK
// or FAILED, always the eight steps with their categories.
func TestPreflightStorageMediumCandidate_RendersEverySkippedStep(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	report := workingPreflight()
	report.OK = false
	report.Checks[1] = service.MediumPreflightCheck{
		Step: "reach", Outcome: "failed", Category: "configuration",
		Detail: "the endpoint answered but does not hold this bucket",
	}
	for i := 2; i < len(report.Checks); i++ {
		report.Checks[i] = service.MediumPreflightCheck{
			Step: report.Checks[i].Step, Outcome: "skipped", Detail: "this check did not run",
		}
	}
	rt.backend.mediumPreflight = report

	rec := rt.post(t, "/api/v1/storage-mediums/preflight", candidateBody)
	mustStatus(t, rec, http.StatusOK)

	var got mediumPreflightResponse
	decodeInto(t, rec, &got)
	if got.OK {
		t.Fatal("a failing candidate came back ok")
	}
	skipped := 0
	for _, c := range got.Checks {
		if c.Outcome == "skipped" {
			skipped++
		}
	}
	if skipped != 6 {
		t.Errorf("%d checks came back skipped, want 6; a skipped write rendered as anything but 'never tried' tells an operator their bucket is writable", skipped)
	}
	if got.Checks[1].Category != "configuration" {
		t.Errorf("the failing check lost its category (%q); a client branches on the category and never on the detail", got.Checks[1].Category)
	}
}

func TestCreateStorageMedium_DeclaresItAndCarriesNoCredentialBack(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	rec := rt.post(t, "/api/v1/storage-mediums", candidateBody)
	mustStatus(t, rec, http.StatusCreated)

	if rt.backend.lastMediumSpec.Credentials.ID != "9b41c7e2" {
		t.Fatalf("the credential reference did not reach the backend: %+v", rt.backend.lastMediumSpec.Credentials)
	}
	body := rec.Body.String()
	for _, forbidden := range []string{"9b41c7e2", "credential"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(forbidden)) {
			t.Errorf("the create response carries %q:\n%s", forbidden, body)
		}
	}
}

// TestListAndGetStorageMedium_NeverCarryTheCredentialReferenceEither is
// issue #665's C3, extending TestCreateStorageMedium_DeclaresItAndCarriesNoCredentialBack's
// coverage to every other route a client can read rather than writing a
// second copy of its check: a medium created from the canary reference
// above must not come back carrying it, or the word "credential", from
// GET /api/v1/storage-mediums (the list) or GET
// /api/v1/storage-mediums/{id} (one medium) either.
// StorageMediumSummary has structurally no field for a secret
// (service/mediums.go's own doc), so this proves that absence holds
// through the real JSON encoding this handler uses, not only through the
// struct's shape.
//
// # Why the backend catalogue is checked here and by a different rule
//
// EPIC I (#664) added GET /api/v1/backends, which is a new API response
// and therefore inside C3's scope rather than beside it. It is also the
// one response in this product where the WORD "credential" legitimately
// appears: a manifest DECLARES that a credential is needed, which is a
// statement of shape and is exactly what #669's form renders a control
// from. So the blanket word ban above cannot be the rule there, and the
// rule that replaces it is the structural one — a credential-kind field
// may declare its id, its label and its required-ness, and no field on
// that shape can hold material. Anything an operator supplied, the
// canary included, must be absent from it as absolutely as from the two
// reads above.
func TestListAndGetStorageMedium_NeverCarryTheCredentialReferenceEither(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	mustStatus(t, rt.post(t, "/api/v1/storage-mediums", candidateBody), http.StatusCreated)
	if rt.backend.lastMediumSpec.Credentials.ID != "9b41c7e2" {
		t.Fatalf("the credential reference did not reach the backend: %+v", rt.backend.lastMediumSpec.Credentials)
	}
	rt.backend.mediums = []service.StorageMediumSummary{
		{ID: "offsite_s3", Type: "s3", Bucket: "nas-backups", StorageClass: "STANDARD_IA", UploadVerification: "readback"},
	}

	for _, route := range []string{"/api/v1/storage-mediums", "/api/v1/storage-mediums/offsite_s3"} {
		rec := rt.get(t, route)
		mustStatus(t, rec, http.StatusOK)
		body := rec.Body.String()
		for _, forbidden := range []string{"9b41c7e2", "credential"} {
			if strings.Contains(strings.ToLower(body), strings.ToLower(forbidden)) {
				t.Errorf("GET %s carries %q:\n%s", route, forbidden, body)
			}
		}
	}

	// The backend catalogue, by the structural rule its docblock above
	// explains. Read after the create, so the canary really is in play
	// in this process when the catalogue is served.
	rec := rt.get(t, "/api/v1/backends")
	mustStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "9b41c7e2") {
		t.Errorf("GET /api/v1/backends carries the credential reference:\n%s", rec.Body.String())
	}
	var catalogue backendsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &catalogue); err != nil {
		t.Fatalf("decode the backend catalogue: %v", err)
	}
	declared := 0
	for _, b := range catalogue.Backends {
		for _, f := range b.Fields {
			if f.Kind != "credential" {
				continue
			}
			declared++
			// Shape only. A pattern would be a claim about what a
			// secret looks like, unset_means would be a default
			// credential, and a choice set would be a list of them.
			if f.Pattern != "" || f.UnsetMeans != "" || len(f.Values) != 0 {
				t.Errorf("backend %q declares credential field %q with a value-shaped attribute: pattern=%q unset_means=%q values=%d",
					b.ID, f.ID, f.Pattern, f.UnsetMeans, len(f.Values))
			}
		}
	}
	if declared == 0 {
		t.Fatal("no bundled manifest declares a credential field, so the structural check above is vacuous")
	}
}

// TestRemoveStorageMedium_RefusesWithAConflictWhileCopiesNameIt is FR-30
// on the wire. A 409 rather than a 400 because the request was understood
// perfectly and is being declined on the state of the deployment; a 400
// would send an operator off to check their JSON.
func TestRemoveStorageMedium_RefusesWithAConflictWhileCopiesNameIt(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.errOnMediumWrite = fmt.Errorf(
		"%w: 148 copies on storage medium \"offsite_s3\", across api-server/var-backups (96), nas-media/photos (52)",
		service.ErrStorageMediumInUse)

	rec := rt.delete(t, "/api/v1/storage-mediums/offsite_s3")
	mustStatus(t, rec, http.StatusConflict)
	if got := responseErrorCode(rec.Body.String()); got != "MEDIUM_IN_USE" {
		t.Fatalf("error code = %q, want MEDIUM_IN_USE", got)
	}
	// The refusal is only useful if it names what is affected. "148
	// copies affected" with nothing listed is a number, not a report.
	body := rec.Body.String()
	for _, want := range []string{"148", "api-server/var-backups", "nas-media/photos"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, body)
		}
	}
}

func TestGetStorageMediumUsage_ListsTheSetsRatherThanOnlyCounting(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.mediumUsage = service.StorageMediumUsage{
		Placements: 148,
		BackupSets: []service.StorageMediumUsageBySet{
			{Set: "api-server/var-backups", Placements: 96, OnlyCopyHere: 96},
			{Set: "nas-media/photos", Placements: 52, OnlyCopyHere: 52},
		},
	}

	rec := rt.get(t, "/api/v1/storage-mediums/offsite_s3/usage")
	mustStatus(t, rec, http.StatusOK)

	var got storageMediumUsageResponse
	decodeInto(t, rec, &got)
	if got.Medium != "offsite_s3" || got.Placements != 148 {
		t.Fatalf("usage = %+v", got)
	}
	if len(got.BackupSets) != 2 || got.BackupSets[0].OnlyCopyHere != 96 {
		t.Fatalf("the affected sets are not listed: %+v", got.BackupSets)
	}
}

// TestUpdateStorageMedium_RefusesTwoIdsRatherThanPreferringOne is
// testConnection's own rule applied here: a request that says two things
// about which destination it is editing is ambiguous, and silently
// preferring either is how a caller is shown a success for a change to
// something else.
func TestUpdateStorageMedium_RefusesTwoIdsRatherThanPreferringOne(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rec := rt.put(t, "/api/v1/storage-mediums/offsite_s3",
		`{"id":"cold_vault","type":"s3","bucket":"nas-archive","credentials":{"credentials_id":"9b41c7e2"}}`)
	mustStatus(t, rec, http.StatusBadRequest)
	if rt.backend.lastMediumSpec.ID != "" {
		t.Errorf("the ambiguous request reached the backend as %q", rt.backend.lastMediumSpec.ID)
	}
}

// TestUpdateStorageMedium_MayOmitTheCredentialBlock is the shape an edit
// form actually sends: this API never reports a medium's credential, so a
// form cannot resubmit one it never received.
func TestUpdateStorageMedium_MayOmitTheCredentialBlock(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.mediums = []service.StorageMediumSummary{
		{ID: "offsite_s3", Type: "s3", Bucket: "nas-backups", StorageClass: "STANDARD", UploadVerification: "readback"},
	}

	rec := rt.put(t, "/api/v1/storage-mediums/offsite_s3",
		`{"type":"s3","region":"eu-west-1","bucket":"nas-backups","storage_class":"STANDARD_IA"}`)
	mustStatus(t, rec, http.StatusOK)
	if got := rt.backend.lastMediumSpec; got.ID != "offsite_s3" || got.Region != "eu-west-1" {
		t.Fatalf("the edit reached the backend as %+v", got)
	}
	if c := rt.backend.lastMediumSpec.Credentials; c.ID != "" || c.File != "" || c.Env != "" || len(c.Command) != 0 {
		t.Errorf("an edit that named no credential arrived carrying one: %+v", c)
	}
}

// H2.2 (#622) on the wire: the local hard drive in the list, the default
// and how it moves, and the second removal refusal.

// TestListStorageMediums_CarriesTheLocalEntryWithItsDriveAndItsDefaultMark
// pins the three fields #622 adds, together, because they are only useful
// together: an entry saying "local" with no path does not tell an
// operator which drive their backups land on, and a list with no default
// mark does not tell them where the next tier would start.
func TestListStorageMediums_CarriesTheLocalEntryWithItsDriveAndItsDefaultMark(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.mediums = []service.StorageMediumSummary{
		{ID: "local", Type: "local", Path: "/srv/backups", UploadVerification: "readback", IsLocal: true, IsDefault: true},
		{ID: "offsite_s3", Type: "s3", Bucket: "nas-backups", StorageClass: "STANDARD", UploadVerification: "readback"},
	}

	rec := rt.get(t, "/api/v1/storage-mediums")
	mustStatus(t, rec, http.StatusOK)

	var got listStorageMediumsResponse
	decodeInto(t, rec, &got)
	if len(got.Mediums) != 2 {
		t.Fatalf("mediums = %+v, want the local hard drive and the declared one", got.Mediums)
	}
	local := got.Mediums[0]
	if !local.IsLocal || local.ID != "local" {
		t.Fatalf("the first entry is not the local hard drive: %+v", local)
	}
	if local.Path != "/srv/backups" {
		t.Errorf("the local entry does not name the drive it writes to: %+v", local)
	}
	if !local.IsDefault {
		t.Errorf("the local entry is not marked as the destination a new tier starts on: %+v", local)
	}
	if got.Mediums[1].IsLocal || got.Mediums[1].IsDefault {
		t.Errorf("the declared destination is marked local or default: %+v", got.Mediums[1])
	}
}

// TestSetDefaultStorageMedium_MovesItAndAnswersWithTheDestination: the
// response is the destination that is now the default, so a caller
// re-renders from the answer rather than from its own optimistic guess.
func TestSetDefaultStorageMedium_MovesItAndAnswersWithTheDestination(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.mediums = []service.StorageMediumSummary{
		{ID: "local", Type: "local", Path: "/srv/backups", IsLocal: true, IsDefault: true},
		{ID: "offsite_s3", Type: "s3", Bucket: "nas-backups", StorageClass: "STANDARD", UploadVerification: "readback"},
	}

	rec := rt.put(t, "/api/v1/storage-mediums/offsite_s3/default", "")
	mustStatus(t, rec, http.StatusOK)

	var got storageMediumBody
	decodeInto(t, rec, &got)
	if got.ID != "offsite_s3" || !got.IsDefault {
		t.Fatalf("the answer is not the destination that is now the default: %+v", got)
	}
	// Exactly one, which is the invariant the list has to keep. A fake
	// that set a flag without clearing the others would pass a check on
	// the answer alone and produce a list no deployment can be in.
	defaults := 0
	for _, m := range rt.backend.mediums {
		if m.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Errorf("%d destinations are marked default after the move, want exactly 1", defaults)
	}
}

// TestSetDefaultStorageMedium_IsNotBehindTheDestructiveGate is the
// route's own tier, asserted rather than left to the red-team walk's
// exemption list to imply. It moves no backup and rewrites no tier, so
// the gate has nothing to stand in front of, and putting it behind one
// would train an operator to click through the acknowledgment that
// matters.
func TestSetDefaultStorageMedium_IsNotBehindTheDestructiveGate(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.mediums = []service.StorageMediumSummary{
		{ID: "offsite_s3", Type: "s3", Bucket: "nas-backups", StorageClass: "STANDARD", UploadVerification: "readback"},
	}

	rec := rt.put(t, "/api/v1/storage-mediums/offsite_s3/default", "")
	if got := responseErrorCode(rec.Body.String()); got == "DESTRUCTIVE_OPERATIONS_DISABLED" {
		t.Fatalf("the route is behind the destructive gate: %s", rec.Body.String())
	}
	mustStatus(t, rec, http.StatusOK)
}

// TestSetDefaultStorageMedium_RefusesADestinationThisDeploymentDoesNotHave
// keeps the default from becoming a dangling reference, which is the one
// way this setting could point a new tier at nowhere.
func TestSetDefaultStorageMedium_RefusesADestinationThisDeploymentDoesNotHave(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	rec := rt.put(t, "/api/v1/storage-mediums/typo_s3/default", "")
	mustStatus(t, rec, http.StatusNotFound)
	if got := responseErrorCode(rec.Body.String()); got != "MEDIUM_NOT_FOUND" {
		t.Fatalf("error code = %q, want MEDIUM_NOT_FOUND", got)
	}
}

// TestRemoveStorageMedium_RefusesTheDefaultUnderItsOwnCode is the second
// removal refusal, and the point of the case is the CODE rather than the
// status.
//
// Both refusals are 409, and a caller that could not tell them apart
// would render "what is affected" under a refusal that is not about
// affected backups at all: MEDIUM_IN_USE means copies are there and the
// next step is to look at them, MEDIUM_IS_DEFAULT means nothing is
// necessarily there and the next step is to move one setting.
func TestRemoveStorageMedium_RefusesTheDefaultUnderItsOwnCode(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.errOnMediumWrite = fmt.Errorf(
		"%w: offsite_s3 is the destination a newly created retention tier starts on",
		service.ErrStorageMediumIsDefault)

	rec := rt.delete(t, "/api/v1/storage-mediums/offsite_s3")
	mustStatus(t, rec, http.StatusConflict)
	if got := responseErrorCode(rec.Body.String()); got != "MEDIUM_IS_DEFAULT" {
		t.Fatalf("error code = %q, want MEDIUM_IS_DEFAULT", got)
	}
	if !strings.Contains(rec.Body.String(), "default") {
		t.Errorf("the refusal does not say why it refused:\n%s", rec.Body.String())
	}
}

// TestCreateStorageMedium_AnUnprovenDestinationIsRefusedAsDeclared is
// #636 on this route: the service proves a create's destination itself
// now, and this handler has to answer a 409 the contract actually
// declares for the operation. A refusal a client is never told about is
// one it cannot handle, and this is the one a third-party client that
// never ran the candidate check will meet first.
func TestCreateStorageMedium_AnUnprovenDestinationIsRefusedAsDeclared(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.errOnMediumWrite = fmt.Errorf(
		"%w: the reach check failed. the endpoint answered and does not have bucket \"nas-backups\"",
		service.ErrStorageMediumNotProven)

	rec := rt.post(t, "/api/v1/storage-mediums", candidateBody)
	mustStatus(t, rec, http.StatusConflict)
	code := responseErrorCode(rec.Body.String())
	if code != "MEDIUM_CONNECTION_NOT_PROVEN" {
		t.Fatalf("error code = %q, want MEDIUM_CONNECTION_NOT_PROVEN", code)
	}
	// The step that failed reaches the caller. A refusal with nothing to
	// act on is a "no", and the whole point of an eight-step report is
	// that knowing WHICH step failed is most of the diagnosis.
	if !strings.Contains(rec.Body.String(), "reach") {
		t.Errorf("the refusal does not name the step that failed:\n%s", rec.Body.String())
	}
	declared := contractEndpoints()["createStorageMedium"].ErrorCodes[http.StatusConflict]
	found := false
	for _, c := range declared {
		if string(c) == code {
			found = true
		}
	}
	if !found {
		t.Errorf("the handler returned 409 %q, which api/v1/openapi.json does not declare for createStorageMedium at that status (it declares %v)", code, declared)
	}
}

// TestUpdateStorageMedium_AnUnprovenEditIsRefusedAsDeclared is the same
// thing on the edit route, which had no 409 at all before #636. An edit
// that cannot reach the destination it would create deserves the check
// more than a create does: a create that fails has produced nothing, and
// an edit that fails has broken a destination that was working.
func TestUpdateStorageMedium_AnUnprovenEditIsRefusedAsDeclared(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.mediums = []service.StorageMediumSummary{
		{ID: "offsite_s3", Type: "s3", Bucket: "nas-backups", StorageClass: "STANDARD", UploadVerification: "readback"},
	}
	rt.backend.errOnMediumWrite = fmt.Errorf(
		"%w: the reach check failed. the endpoint could not be reached",
		service.ErrStorageMediumNotProven)

	rec := rt.put(t, "/api/v1/storage-mediums/offsite_s3",
		`{"type":"s3","region":"eu-west-1","bucket":"nas-backups-2","storage_class":"STANDARD_IA"}`)
	mustStatus(t, rec, http.StatusConflict)
	code := responseErrorCode(rec.Body.String())
	if code != "MEDIUM_CONNECTION_NOT_PROVEN" {
		t.Fatalf("error code = %q, want MEDIUM_CONNECTION_NOT_PROVEN", code)
	}
	declared := contractEndpoints()["updateStorageMedium"].ErrorCodes[http.StatusConflict]
	found := false
	for _, c := range declared {
		if string(c) == code {
			found = true
		}
	}
	if !found {
		t.Errorf("the handler returned 409 %q, which api/v1/openapi.json does not declare for updateStorageMedium at that status (it declares %v)", code, declared)
	}
}

// TestStorageMediumWrites_CarryTheSkipAndNeverTheMark is the review
// finding PR #628 made on the source side, asked here before it can
// happen: `skip_connection_check` is an INSTRUCTION the service acts on,
// and there is no field on this body that sets the mark directly.
//
// The mark half is asserted by decoding into the request shape rather
// than by reading the struct, because the property is about what a CALLER
// can send: a body naming connection_unverified must not be able to write
// one. additionalProperties is false on this schema, so the check that
// matters here is that the spec the backend receives carries the mark
// from nowhere.
func TestStorageMediumWrites_CarryTheSkipAndNeverTheMark(t *testing.T) {
	rt := newReadSurfaceRouter(t)

	skipping := `{"id":"offsite_s3","type":"s3","region":"us-east-1","bucket":"nas-backups",` +
		`"credentials":{"credentials_id":"9b41c7e2"},"skip_connection_check":true}`
	mustStatus(t, rt.post(t, "/api/v1/storage-mediums", skipping), http.StatusCreated)
	if !rt.backend.lastMediumSpec.SkipConnectionCheck {
		t.Error("skip_connection_check did not reach the backend, so the API has no way to write a destination offline")
	}

	mustStatus(t, rt.post(t, "/api/v1/storage-mediums", candidateBody), http.StatusCreated)
	if rt.backend.lastMediumSpec.SkipConnectionCheck {
		t.Error("a body that said nothing about the check arrived asking to skip it; the default has to be the one that checks")
	}

	var decoded storageMediumRequest
	body := `{"id":"offsite_s3","type":"s3","bucket":"nas-backups","connection_unverified":true}`
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.spec().SkipConnectionCheck {
		t.Error("a caller's own claim about what it had done became an instruction to skip the check")
	}
}

// TestStorageMediumBody_ReportsTheMarkAndOmitsItWhenFalse is what makes
// an unverified destination visible on this API at all.
//
// Omitted when false on purpose, and that is not a byte count. An engine
// built before this field omits it always, so a client that read absence
// as a claim either way would be reading a version difference as a fact
// about somebody's bucket. Absence means "nothing here says this was
// skipped".
func TestStorageMediumBody_ReportsTheMarkAndOmitsItWhenFalse(t *testing.T) {
	rt := newReadSurfaceRouter(t)
	rt.backend.mediums = []service.StorageMediumSummary{
		{ID: "offsite_s3", Type: "s3", Bucket: "nas-backups", StorageClass: "STANDARD",
			UploadVerification: "readback", ConnectionUnverified: true},
		{ID: "cold_vault", Type: "s3", Bucket: "nas-archive", StorageClass: "STANDARD",
			UploadVerification: "readback"},
	}

	rec := rt.get(t, "/api/v1/storage-mediums")
	mustStatus(t, rec, http.StatusOK)

	var got listStorageMediumsResponse
	decodeInto(t, rec, &got)
	if len(got.Mediums) != 2 {
		t.Fatalf("the list carries %d destinations, want 2", len(got.Mediums))
	}
	if !got.Mediums[0].ConnectionUnverified {
		t.Error("a destination the engine reports as never proven reads back as one that was checked")
	}
	if got.Mediums[1].ConnectionUnverified {
		t.Error("a destination the engine says nothing about reads back as unverified")
	}
	if strings.Count(rec.Body.String(), "connection_unverified") != 1 {
		t.Errorf("the mark is rendered for a destination that does not carry it, so absence stops meaning absence:\n%s", rec.Body.String())
	}
}

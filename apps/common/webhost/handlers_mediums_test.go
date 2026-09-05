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
package webhost

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

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

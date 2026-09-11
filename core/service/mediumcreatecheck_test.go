// Issue #636 (H2.6): the destination half of what #628 closed on the
// source side.
//
// `POST /storage-mediums` accepted a destination nobody had verified,
// wrote it, and answered 201. The check lived in the CLI's `medium add`
// and in the S3 wizard, so a first-party client did the right thing and
// nothing else had to, and there was no mark either: a destination
// written unproven and one checked against a real bucket were the same
// destination in every list, on every screen and in every command's
// output, forever.
//
// These cases are that, asked of the service, because every surface
// writes through here. They are the exact counterparts of
// backupsetcreatecheck_test.go beside them, on the other noun.
//
// The endpoint in every failing case is a loopback port nothing listens
// on, for the reason that file's own header gives: a closed port is
// refused on the TCP connect in milliseconds where a hostname that does
// not resolve spends the resolver's timeout, and on a machine with a
// wildcard-answering resolver is not a failure at all. A red that depends
// on the developer's resolver is a red that goes green for the wrong
// reason.
//
// What cannot be shown here is a check that PASSES, because that needs a
// real S3 endpoint answering. It is proven where the endpoint is real, in
// core/tests/miniointegration/wizard_test.go, which is also where the
// mark is proven to come off.
package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/backupdproject/backupd/core/internal/config"
)

// unreachableEndpoint is an http:// URL on a loopback port nothing is
// listening on, so the probe's first network step is refused rather than
// answered or left hanging.
func unreachableEndpoint(t *testing.T) string {
	t.Helper()
	return "http://127.0.0.1:" + strconv.Itoa(aClosedLoopbackPort(t))
}

// unprovableSpec is a complete, valid destination description pointed at
// an endpoint that answers nothing, with a real credential reference this
// deployment minted.
//
// The credential is real on purpose. The check's first step is whether
// the credential can be OBTAINED, which is a file read on this host, and
// a spec with a bogus reference would be refused by resolveSpecCredentials
// before any check ran at all. So this fails at `reach`, which is the
// step that is actually about the destination.
func unprovableSpec(t *testing.T, svc *BackupService, id string) StorageMediumSpec {
	t.Helper()
	ref, err := svc.ImportStorageCredentials(context.Background(), testCanaryAccessKeyID, testCanarySecret, "")
	if err != nil {
		t.Fatalf("ImportStorageCredentials: %v", err)
	}
	return StorageMediumSpec{
		ID:          id,
		Type:        "s3",
		Region:      "us-east-1",
		Endpoint:    unreachableEndpoint(t),
		Bucket:      "nas-backups",
		Credentials: StorageMediumCredentials{ID: ref.ID},
	}
}

// TestCreateStorageMedium_ProvesTheDestinationBeforeItWrites is the
// finding itself: a create naming a destination this manager cannot reach
// is refused by the SERVICE, whoever the caller is, and leaves nothing
// behind.
//
// Three things have to be true of the refusal and the case asks all
// three. The error is ErrStorageMediumNotProven, so the API can answer a
// named 409 rather than a 500. The configuration file is byte for byte
// what it was, which is the "refuse, never partially apply" rule every
// other write on this path holds. And the destination is not readable
// back out of this process either, because a create that hot-reloaded a
// medium it did not write would leave one process disagreeing with its own
// configuration file.
func TestCreateStorageMedium_ProvesTheDestinationBeforeItWrites(t *testing.T) {
	svc, configPath := openTestService(t)
	spec := unprovableSpec(t, svc, "offsite_s3")
	before := mustRead(t, configPath)

	_, err := svc.CreateStorageMedium(context.Background(), spec)
	if !errors.Is(err, ErrStorageMediumNotProven) {
		t.Fatalf("CreateStorageMedium against an endpoint nothing answers on returned %v, want ErrStorageMediumNotProven: the service wrote whatever it was told, and only this repository's own CLI and wizard checked first", err)
	}

	if after := mustRead(t, configPath); after != before {
		t.Errorf("a refused create still wrote the configuration:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if _, err := svc.GetStorageMedium(context.Background(), "offsite_s3"); !errors.Is(err, ErrMediumNotFound) {
		t.Errorf("GetStorageMedium after a refused create = %v, want ErrMediumNotFound: the destination exists in this process even though the file was not written", err)
	}
}

// TestCreateStorageMedium_TheRefusalNamesTheStepAndNoCredential pins both
// halves of what the refusal may say.
//
// It has to name the step that failed, or an operator has a "no" with
// nothing to fix. And it may not carry credential material or an
// underlying error's text, which is FR-33's rule: mediumcheck composes
// every sentence in a report out of its own strings and the facts this
// product already publishes about a destination, and the classified cause
// goes to the log instead.
func TestCreateStorageMedium_TheRefusalNamesTheStepAndNoCredential(t *testing.T) {
	svc, _ := openTestService(t)
	spec := unprovableSpec(t, svc, "offsite_s3")

	_, err := svc.CreateStorageMedium(context.Background(), spec)
	if err == nil {
		t.Fatal("an unreachable destination was declared")
	}
	msg := err.Error()
	if !strings.Contains(msg, "reach") {
		t.Errorf("the refusal does not name the step that failed, so there is nothing in it to act on: %v", err)
	}
	for _, forbidden := range []string{testCanarySecret, testCanaryAccessKeyID} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("the refusal carries credential material: %v", err)
		}
	}
}

// TestCreateStorageMedium_SkippingTheCheckMarksTheDestination is the
// escape hatch, and the mark is the service's own record of using it.
//
// The endpoint is one the check would refuse, so the skip is what does
// the work: a build that ran the check regardless would refuse here.
//
// The control is the same spec written through mediumFromSpec with the
// skip off, because a create that PASSES its check needs a real S3
// endpoint, which this suite does not have in process. What can be shown
// is that the mark comes from the skip and from nothing else: the same
// description with the skip off carries no mark, and the encoder leaves
// the key out of the file entirely rather than writing false, so absence
// keeps meaning what it meant in every configuration written before this
// field existed.
func TestCreateStorageMedium_SkippingTheCheckMarksTheDestination(t *testing.T) {
	svc, configPath := openTestService(t)
	spec := unprovableSpec(t, svc, "offsite_s3")
	spec.SkipConnectionCheck = true

	got, err := svc.CreateStorageMedium(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateStorageMedium with the check skipped: %v", err)
	}
	if !got.ConnectionUnverified {
		t.Error("a destination written with the check skipped reads back as one that was checked")
	}
	if raw := mustRead(t, configPath); !strings.Contains(raw, "connection_unverified: true") {
		t.Errorf("the mark did not reach the configuration file:\n%s", raw)
	}

	proven := spec
	proven.SkipConnectionCheck = false
	medium, err := svc.mediumFromSpec(proven, false)
	if err != nil {
		t.Fatalf("mediumFromSpec: %v", err)
	}
	if medium.ConnectionUnverified {
		t.Error("a create that did not skip the check is marked as unverified; the mark has to come from the skip and from nothing else")
	}
	encoded, err := yaml.Marshal(medium)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if strings.Contains(string(encoded), "connection_unverified") {
		t.Errorf("an ordinary create writes the key; absence has to keep meaning what it meant in every file written before this field existed:\n%s", encoded)
	}
}

// TestUpdateStorageMedium_ProvesAnEditThatMovesTheDestination is the
// other half of the acceptance: an edit that changes what this
// destination IS gets the same check the create gets.
//
// It starts from a destination written with the skip, so the edit under
// test is one an operator would plausibly make (fix the bucket name and
// save), and it asserts the two directions separately: refused without
// the skip, written WITH it and still marked afterwards.
func TestUpdateStorageMedium_ProvesAnEditThatMovesTheDestination(t *testing.T) {
	svc, configPath := openTestService(t)
	spec := unprovableSpec(t, svc, "offsite_s3")
	spec.SkipConnectionCheck = true
	if _, err := svc.CreateStorageMedium(context.Background(), spec); err != nil {
		t.Fatalf("CreateStorageMedium with the check skipped: %v", err)
	}
	before := mustRead(t, configPath)

	edit := spec
	edit.SkipConnectionCheck = false
	edit.Bucket = "nas-backups-2"
	if _, err := svc.UpdateStorageMedium(context.Background(), edit); !errors.Is(err, ErrStorageMediumNotProven) {
		t.Fatalf("UpdateStorageMedium moving the bucket to an endpoint nothing answers on returned %v, want ErrStorageMediumNotProven", err)
	}
	if after := mustRead(t, configPath); after != before {
		t.Errorf("a refused edit still wrote the configuration:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	edit.SkipConnectionCheck = true
	got, err := svc.UpdateStorageMedium(context.Background(), edit)
	if err != nil {
		t.Fatalf("UpdateStorageMedium with the check skipped: %v", err)
	}
	if got.Bucket != "nas-backups-2" {
		t.Errorf("the edit did not land: bucket = %q", got.Bucket)
	}
	if !got.ConnectionUnverified {
		t.Error("an edit written with the check skipped reads back as one that was checked")
	}
}

// TestUpdateStorageMedium_AnEditThatMovesNothingRunsNoCheck is the other
// direction, and it is what stops the check being a wall in front of an
// edit form that submits everything it was showing.
//
// Every field of a storage medium describes the destination, so unlike a
// backup set there is no "edit that changes only how this deployment
// behaves". What there IS is the re-save: a form reads the destination,
// changes nothing, and puts the whole record back, which is exactly what
// happens when an operator opens the edit dialog and presses Save. That
// is not a new destination and must not be refused for one that was
// already unreachable when it was declared.
//
// The credential is deliberately not named, which is the other half of
// the same case: an edit that names none keeps the one already
// configured, so the record the comparison sees on both sides has to be
// the same record.
func TestUpdateStorageMedium_AnEditThatMovesNothingRunsNoCheck(t *testing.T) {
	svc, _ := openTestService(t)
	spec := unprovableSpec(t, svc, "offsite_s3")
	spec.SkipConnectionCheck = true
	if _, err := svc.CreateStorageMedium(context.Background(), spec); err != nil {
		t.Fatalf("CreateStorageMedium with the check skipped: %v", err)
	}

	resave := spec
	resave.SkipConnectionCheck = false
	resave.Credentials = StorageMediumCredentials{}
	got, err := svc.UpdateStorageMedium(context.Background(), resave)
	if err != nil {
		t.Fatalf("re-saving an unchanged destination was refused, so an edit form cannot press Save on a destination that is currently unreachable: %v", err)
	}
	if !got.ConnectionUnverified {
		t.Error("a re-save that ran no check cleared the mark, which would make the mark say 'somebody pressed Save' rather than 'this destination works'")
	}
}

// TestUpdateStorageMedium_AResolvedDefaultIsNotAMovedDestination is the
// re-save case again, in the shape an edit FORM actually produces, and it
// is the one that catches a comparison written against the raw record.
//
// config.Load leaves storage_class and upload_verification exactly as the
// operator wrote them, which is usually not at all. Every read surface
// reports the resolved value, so a form pre-fills "STANDARD" and
// "readback" against a file that says neither and submits both. A
// comparison that read those as a moved destination would run a network
// check on every Save from the destinations card, and would make a
// currently-unreachable destination impossible to edit at all, which is
// the exact thing the re-save branch exists to allow.
func TestUpdateStorageMedium_AResolvedDefaultIsNotAMovedDestination(t *testing.T) {
	svc, configPath := openTestService(t)
	spec := unprovableSpec(t, svc, "offsite_s3")
	spec.SkipConnectionCheck = true
	if _, err := svc.CreateStorageMedium(context.Background(), spec); err != nil {
		t.Fatalf("CreateStorageMedium with the check skipped: %v", err)
	}
	// The declaration really does leave both keys out, so this case is
	// about resolution and not about a fixture that wrote them.
	raw := mustRead(t, configPath)
	if strings.Contains(raw, "storage_class:") || strings.Contains(raw, "upload_verification:") {
		t.Fatalf("the fixture wrote a class into the file, so nothing here is resolved:\n%s", raw)
	}

	// What the destinations card reads back, and what its edit dialog
	// therefore submits.
	shown, err := svc.GetStorageMedium(context.Background(), "offsite_s3")
	if err != nil {
		t.Fatalf("GetStorageMedium: %v", err)
	}
	if shown.StorageClass == "" || shown.UploadVerification == "" {
		t.Fatalf("the read surface reports no resolved class, so this case proves nothing: %+v", shown)
	}

	resave := spec
	resave.SkipConnectionCheck = false
	resave.Credentials = StorageMediumCredentials{}
	resave.StorageClass = shown.StorageClass
	resave.UploadVerification = shown.UploadVerification
	if _, err := svc.UpdateStorageMedium(context.Background(), resave); err != nil {
		t.Fatalf("re-saving the destination exactly as the read surface reports it was refused, so an edit form cannot press Save on a destination that is currently unreachable: %v", err)
	}
}

// TestStorageMediumMark_IsNeverTakenFromTheLocalHardDrive keeps #622's
// synthesised entry out of #636 entirely.
//
// The local hard drive is not declared and cannot be created, so it has
// no create to check and no mark to carry, and a build that ran a bucket
// probe for it would be probing something that has no bucket, no endpoint
// and no credential. This is the boring assertion that stops that
// happening quietly.
func TestStorageMediumMark_IsNeverTakenFromTheLocalHardDrive(t *testing.T) {
	svc, _ := openTestService(t)
	local, err := svc.GetStorageMedium(context.Background(), StorageMediumLocalID)
	if err != nil {
		t.Fatalf("GetStorageMedium(local): %v", err)
	}
	if !local.IsLocal {
		t.Fatalf("the reserved id did not answer with the local entry: %+v", local)
	}
	if local.ConnectionUnverified {
		t.Error("the local hard drive reports itself unproven; it is not declared, cannot be created and has no connection to prove")
	}

	// And nothing tries to preflight it, which is what #636 asks for in
	// so many words. The three verbs that resolve a spec refuse the
	// reserved id as an invalid request, in one place, BEFORE the check
	// runs: without that, the check reaches internal/app, which refuses
	// the id with an error that is neither a validation failure nor
	// anything a caller can act on, so a request that has always been a
	// 400 would come back as "this manager broke".
	spec := unprovableSpec(t, svc, StorageMediumLocalID)
	for name, err := range map[string]error{
		"create": errOf(svc.CreateStorageMedium(context.Background(), spec)),
		"edit":   errOf(svc.UpdateStorageMedium(context.Background(), spec)),
		"probe":  preflightErrOf(svc.PreflightStorageMediumCandidate(context.Background(), spec)),
	} {
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s of the reserved local id returned %v, want ErrInvalidRequest", name, err)
		}
		if errors.Is(err, ErrStorageMediumNotProven) {
			t.Errorf("%s of the reserved local id was refused for a connection it does not have: %v", name, err)
		}
	}
}

// errOf and preflightErrOf drop the value half of a two-result call, so
// the table above reads as the question it asks.
func errOf(_ StorageMediumSummary, err error) error { return err }

func preflightErrOf(_ MediumPreflight, err error) error { return err }

// TestStorageMediumSpec_TheMarkIsNotAFieldACallerCanSet is the review
// finding #628 landed on the source side, asked here before it can
// happen: the mark is written from the skip and from nothing else.
//
// It compares the two config records the service builds for one
// description rather than asserting about a request field that does not
// exist, because the property worth pinning is the one an added field
// would break: whatever a caller sends, the ONLY thing that puts
// connection_unverified into the file is this service having been told to
// skip its own check.
func TestStorageMediumSpec_TheMarkIsNotAFieldACallerCanSet(t *testing.T) {
	svc, _ := openTestService(t)
	spec := unprovableSpec(t, svc, "offsite_s3")

	checked, err := svc.mediumFromSpec(spec, false)
	if err != nil {
		t.Fatalf("mediumFromSpec: %v", err)
	}
	spec.SkipConnectionCheck = true
	skipped, err := svc.mediumFromSpec(spec, false)
	if err != nil {
		t.Fatalf("mediumFromSpec: %v", err)
	}
	if checked.ConnectionUnverified || !skipped.ConnectionUnverified {
		t.Fatalf("the mark does not track the skip: checked = %v, skipped = %v", checked.ConnectionUnverified, skipped.ConnectionUnverified)
	}
	// And the two records are otherwise the same destination, so the skip
	// moves the mark and nothing else about what gets written.
	checked.ConnectionUnverified = skipped.ConnectionUnverified
	if !mediumsDescribeTheSameDestination(checked, skipped) {
		t.Errorf("the skip changed something other than the mark:\n%+v\n%+v", checked, skipped)
	}
}

// mediumsDescribeTheSameDestination is the case above's own comparison,
// spelled out here rather than reaching for reflect.DeepEqual on a
// Credentials block whose Command slice can be nil on one side and empty
// on the other.
func mediumsDescribeTheSameDestination(a, b config.StorageMedium) bool {
	return a.ID == b.ID && a.Type == b.Type && a.Region == b.Region && a.Endpoint == b.Endpoint &&
		a.Bucket == b.Bucket && a.Prefix == b.Prefix && a.StorageClass == b.StorageClass &&
		a.UploadVerification == b.UploadVerification && a.ConnectionUnverified == b.ConnectionUnverified &&
		a.Credentials.File == b.Credentials.File && a.Credentials.Env == b.Credentials.Env &&
		strings.Join(a.Credentials.Command, " ") == strings.Join(b.Credentials.Command, " ")
}

// TestPreflightStorageMedium_AFailingCheckLeavesTheMarkExactlyWhereItWas
// is the asymmetry that gives the mark its meaning.
//
// A check that passes clears it, which is proven against a real endpoint
// in core/tests/miniointegration. A check that FAILS must leave it, and
// that half can be proven here, because failing is what an unreachable
// endpoint does reliably. Clearing on any call at all would turn "this
// destination works" into "somebody pressed the button", which is a claim
// nobody needs and one an operator would reasonably misread.
func TestPreflightStorageMedium_AFailingCheckLeavesTheMarkExactlyWhereItWas(t *testing.T) {
	svc, configPath := openTestService(t)
	spec := unprovableSpec(t, svc, "offsite_s3")
	spec.SkipConnectionCheck = true
	if _, err := svc.CreateStorageMedium(context.Background(), spec); err != nil {
		t.Fatalf("CreateStorageMedium with the check skipped: %v", err)
	}

	report, err := svc.PreflightStorageMedium(context.Background(), "offsite_s3")
	if err != nil {
		t.Fatalf("PreflightStorageMedium: %v", err)
	}
	if report.OK {
		t.Fatal("an endpoint nothing answers on passed its check, so this case proves nothing about a failure")
	}

	got, err := svc.GetStorageMedium(context.Background(), "offsite_s3")
	if err != nil {
		t.Fatalf("GetStorageMedium: %v", err)
	}
	if !got.ConnectionUnverified {
		t.Error("a FAILING check cleared the unverified mark, so the mark now means somebody pressed the button rather than that the destination works")
	}
	if raw := mustRead(t, configPath); !strings.Contains(raw, "connection_unverified: true") {
		t.Errorf("a failing check took the mark out of the configuration file:\n%s", raw)
	}
}

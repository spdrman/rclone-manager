// Package config_test carries the pins that need both sides of an edge
// core/internal/backend may not cross (backend imports no config, and
// this repository's cycle - archive imports config, mediumcheck imports
// archive - means config cannot import backend back without creating
// one; see backend/doc.go). An external test package is not part of
// config, so it may import both and compare them, the same arrangement
// archivepin_test.go already uses to pin config's own archive-class set
// against internal/archive's table.
package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/backupd/core/internal/backend"
	"github.com/spdrman/backupd/core/internal/config"
)

// TestTheReservedInstanceIdMatchesConfigsOwn is issue #665's test 28:
// backend.ReservedInstanceID is a copy of config.MediumLocal, made
// because backend may not import config, and this is what keeps the
// copy from drifting silently.
func TestTheReservedInstanceIdMatchesConfigsOwn(t *testing.T) {
	if backend.ReservedInstanceID != config.MediumLocal {
		t.Fatalf("backend.ReservedInstanceID = %q, config.MediumLocal = %q: these name the same reserved id and must never drift apart",
			backend.ReservedInstanceID, config.MediumLocal)
	}
}

// yamlTagsOf returns every yaml tag name declared on typ's fields
// (options like ",omitempty" stripped), in struct declaration order.
// "-" is skipped, matching encoding/*'s own convention for "not part of
// the wire shape".
func yamlTagsOf(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var tags []string
	for i := range typ.NumField() {
		f := typ.Field(i)
		tag := f.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		tags = append(tags, name)
	}
	return tags
}

// storageMediumFieldsNotInTheManifest is every config.StorageMedium yaml
// tag that has no same-named field in the s3 manifest ON PURPOSE, each
// with the reason it is excluded rather than missing. This IS #667's
// checklist (issue #665 section 7.2): grow StorageMedium by a field with
// no entry here and no matching manifest field, and this test fails
// until one of the two is added.
var storageMediumFieldsNotInTheManifest = map[string]string{
	"id":                    "the instance's own identity (config.StorageMedium.ID), not one of the backend's collected fields",
	"type":                  "the instance's own identity (which backend this is an instance of), not one of the backend's collected fields",
	"connection_unverified": "written by core/service from StorageMediumSpec.SkipConnectionCheck, and never taken from an operator's own input (issue #636) - there is nothing for a manifest field to collect",
	"path":                  "issue #666's local_volume field, meaningless for an s3 medium; bundled/local_volume.json declares it instead, and the local_volume manifest's own expressibility is #666's concern, not this s3-specific pin's",
}

// TestTodaysS3MediumIsExpressibleInTheS3Manifest is issue #665's test
// 29, and #667's entire mechanical checklist: every yaml tag
// config.StorageMedium declares must either have a same-named field in
// bundled/s3.json, or be named in storageMediumFieldsNotInTheManifest
// with a reason; and every field bundled/s3.json declares must be one
// of StorageMedium's own yaml tags. Both directions fail if either side
// grows a field the other does not know about, which is exactly the
// drift #667 (moving S3's validation onto this registry) must not
// introduce: a deployment with configured S3 destinations and real
// backups has to keep working with no operator action.
func TestTodaysS3MediumIsExpressibleInTheS3Manifest(t *testing.T) {
	reg, err := backend.Bundled()
	if err != nil {
		t.Fatalf("backend.Bundled(): %v", err)
	}
	s3, err := reg.Backend("s3")
	if err != nil {
		t.Fatalf("backend.Backend(\"s3\"): %v", err)
	}
	manifestFields := map[string]bool{}
	for _, f := range s3.Fields {
		manifestFields[f.ID] = true
	}
	if len(manifestFields) == 0 {
		t.Fatal("the s3 manifest declares no fields, so this test checks nothing")
	}

	tags := yamlTagsOf(t, reflect.TypeOf(config.StorageMedium{}))
	if len(tags) == 0 {
		t.Fatal("config.StorageMedium declares no yaml tags, so this test checks nothing")
	}

	seenFromConfig := map[string]bool{}
	for _, tag := range tags {
		seenFromConfig[tag] = true
		if manifestFields[tag] {
			continue
		}
		if reason, excluded := storageMediumFieldsNotInTheManifest[tag]; excluded {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("storageMediumFieldsNotInTheManifest[%q] has no reason recorded", tag)
			}
			continue
		}
		t.Errorf("config.StorageMedium's %q field has no matching field in bundled/s3.json and is not in "+
			"storageMediumFieldsNotInTheManifest - #667 cannot express it, or this exclusion is missing", tag)
	}

	// The completeness half: dead exclusions, and manifest fields the
	// struct does not even declare - the shape a manifest could drift
	// AHEAD of the struct in, which the first loop cannot see.
	for excluded := range storageMediumFieldsNotInTheManifest {
		if !seenFromConfig[excluded] {
			t.Errorf("storageMediumFieldsNotInTheManifest names %q, and config.StorageMedium no longer declares that tag; delete the entry", excluded)
		}
	}
	for id := range manifestFields {
		if !seenFromConfig[id] {
			t.Errorf("bundled/s3.json declares field %q, and config.StorageMedium has no yaml tag by that name", id)
		}
	}
}

// TestTheS3ManifestsEnumsMatchConfigsClosedSets is issue #665's test 30:
// storage_class's declared values match config.StorageClasses(),
// upload_verification's match the UploadVerification* constants, and
// each field's unset_means matches what EffectiveStorageClass /
// EffectiveUploadVerification actually resolve an empty medium to -
// #665 does not let the manifest lose a class or drift from the
// accessor's own default in the move #667 makes.
func TestTheS3ManifestsEnumsMatchConfigsClosedSets(t *testing.T) {
	reg, err := backend.Bundled()
	if err != nil {
		t.Fatalf("backend.Bundled(): %v", err)
	}
	s3, err := reg.Backend("s3")
	if err != nil {
		t.Fatalf("backend.Backend(\"s3\"): %v", err)
	}

	storageClassField, ok := s3.Field("storage_class")
	if !ok {
		t.Fatal("bundled/s3.json declares no storage_class field")
	}
	var fromManifest []string
	for _, v := range storageClassField.Values {
		fromManifest = append(fromManifest, v.Value)
	}
	fromConfig := config.StorageClasses()
	sort.Strings(fromManifest)
	sortedConfig := append([]string(nil), fromConfig...)
	sort.Strings(sortedConfig)
	if !reflect.DeepEqual(fromManifest, sortedConfig) {
		t.Errorf("storage_class values disagree:\n  manifest: %v\n  config:   %v", fromManifest, fromConfig)
	}
	empty := config.StorageMedium{}
	if storageClassField.UnsetMeans != empty.EffectiveStorageClass() {
		t.Errorf("storage_class unset_means is %q, and EffectiveStorageClass() of an unset medium is %q",
			storageClassField.UnsetMeans, empty.EffectiveStorageClass())
	}

	verificationField, ok := s3.Field("upload_verification")
	if !ok {
		t.Fatal("bundled/s3.json declares no upload_verification field")
	}
	wantVerification := []string{config.UploadVerificationReadback, config.UploadVerificationAttested}
	var gotVerification []string
	for _, v := range verificationField.Values {
		gotVerification = append(gotVerification, v.Value)
	}
	sort.Strings(wantVerification)
	sortedGotVerification := append([]string(nil), gotVerification...)
	sort.Strings(sortedGotVerification)
	if !reflect.DeepEqual(sortedGotVerification, wantVerification) {
		t.Errorf("upload_verification values disagree:\n  manifest: %v\n  config:   %v", gotVerification, wantVerification)
	}
	if verificationField.UnsetMeans != empty.EffectiveUploadVerification() {
		t.Errorf("upload_verification unset_means is %q, and EffectiveUploadVerification() of an unset medium is %q",
			verificationField.UnsetMeans, empty.EffectiveUploadVerification())
	}
}

// TestAPreRegistryStorageMediumConfigStillLoadsAndRoundTrips is issue
// #665's test 32, the #667 blast-radius fixture (plan section 7.2).
// testdata/storage-mediums-pre-registry.yaml is a byte-frozen capture of
// origin/main's testdata/storage-mediums.yaml, taken at this PR's merge
// base, BEFORE any registry code existed. It is never edited to make a
// test pass: if #667 (moving S3's validation onto this registry) makes
// this fail, #667 is wrong, not the fixture.
//
// "Byte-identical" is checked between two successive
// Load-Validate-Marshal passes, not against the fixture's own raw
// bytes: Validate resolves OTHER defaults in place too (alerts,
// delete_safety_delay, and the rest of Config, none of which this issue
// touches), so the minimal fixture - which spells only what FR-27
// itself needs - never round-trips byte-for-byte against its own
// un-defaulted source text, and no existing test in this package claims
// otherwise (TestMarshal_ANoMediumConfigGainsNoMediumKeys compares two
// MARSHALS of the same loaded config for exactly this reason, never the
// source file against a marshal). What #665 - and #667 after it - can
// actually promise is TestValidate_IsIdempotentWithStorageMediums'
// property, applied to every field this fixture exercises: loading what
// was just marshaled and marshaling it again produces the identical
// bytes, so nothing about this configuration drifts on repeated loads.
func TestAPreRegistryStorageMediumConfigStillLoadsAndRoundTrips(t *testing.T) {
	const fixture = "testdata/storage-mediums-pre-registry.yaml"

	cfg, err := config.Load(fixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	first, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	reloadPath := filepath.Join(t.TempDir(), "reloaded.yaml")
	if err := os.WriteFile(reloadPath, first, 0o600); err != nil {
		t.Fatalf("writing the re-marshaled config: %v", err)
	}
	reloaded, err := config.Load(reloadPath)
	if err != nil {
		t.Fatalf("Load of the re-marshaled config: %v", err)
	}
	if err := reloaded.Validate(); err != nil {
		t.Fatalf("Validate of the re-marshaled config: %v", err)
	}
	second, err := yaml.Marshal(reloaded)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if string(first) != string(second) {
		t.Fatalf("Load -> Validate -> Marshal is not stable across a second pass:\n--- first ---\n%s\n--- second ---\n%s",
			first, second)
	}
}

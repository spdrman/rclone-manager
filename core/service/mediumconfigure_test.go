package service

import (
	"sort"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/backend"
	"github.com/spdrman/rclone-manager/core/internal/config"
)

// The field-id table's two directions have to agree (I2.2, issue #669).
//
// core/service holds the one place a manifest's vocabulary is folded
// onto config.StorageMedium's named fields, in both directions:
// mediumFieldValue reads and storageMediumFromFields writes. A field one
// of them handles and the other does not is the defect worth a test,
// because of what it looks like from a browser: the configure form shows
// a destination read through one half and saves it through the other, so
// a field that is readable but unwritable silently reverts on every save
// and a field that is writable but unreadable comes back blank in a form
// that then unsets it.
//
// Neither symptom raises an error anywhere. That is why this is asserted
// as a set equality rather than left to a reviewer noticing two switches
// have the same cases.

// tableFieldIDs is every field id the two halves claim, discovered by
// asking them rather than by listing them here - a list here would be a
// third copy of the table and the one nobody updates.
func tableFieldIDs(t *testing.T) (readable, writable []string) {
	t.Helper()

	// Every field id any bundled manifest declares, which is the
	// population the table has to cover. Read off the registry so a
	// manifest gaining a field puts it in scope automatically.
	registry, err := backend.Bundled()
	if err != nil {
		t.Fatalf("loading the bundled registry: %v", err)
	}
	seen := map[string]bool{}
	var ids []string
	for _, id := range registry.IDs() {
		manifest, err := registry.Backend(id)
		if err != nil {
			t.Fatalf("reading manifest %s: %v", id, err)
		}
		for _, field := range manifest.Fields {
			if field.Kind == backend.KindCredential || seen[field.ID] {
				continue
			}
			seen[field.ID] = true
			ids = append(ids, field.ID)
		}
	}

	for _, id := range ids {
		if mediumFieldValue(everyNamedFieldSet(), id) != "" {
			readable = append(readable, id)
		}
		if storageMediumFieldIsWritable(t, id, "probe-"+id) {
			writable = append(writable, id)
		}
	}
	sort.Strings(readable)
	sort.Strings(writable)
	return readable, writable
}

// everyNamedFieldSet is a medium with every field config.StorageMedium
// NAMES carrying a value, written as a literal.
//
// A literal, and deliberately not built through the write half: an
// earlier draft of this file constructed it with
// storageMediumFromFields, which made the two halves agree by
// construction - drop a write case and the read half saw an empty
// medium and dropped the same field, so the set equality below held and
// proved nothing. That was caught by mutating a case out and watching
// the test pass. The literal is what makes the halves independent, and
// it is the only place in this file that knows a struct field name.
func everyNamedFieldSet() config.StorageMedium {
	return config.StorageMedium{
		ID:                 "offsite",
		Type:               "s3",
		Region:             "a-region",
		Endpoint:           "https://an.endpoint",
		Bucket:             "a-bucket",
		Prefix:             "a/prefix",
		StorageClass:       "A_CLASS",
		UploadVerification: "readback",
	}
}

func storageMediumFieldIsWritable(t *testing.T, fieldID, value string) bool {
	t.Helper()
	_, err := storageMediumFromFields(
		config.StorageMedium{ID: "locker_one", Type: "s3"},
		backend.Manifest{
			ID:     "s3",
			Fields: []backend.Field{{ID: fieldID, Kind: backend.KindString}},
		},
		StorageMediumConfiguration{Fields: map[string]string{fieldID: value}},
	)
	return err == nil
}

func TestEveryFieldTheConfigurationReadsCanAlsoBeWritten(t *testing.T) {
	readable, writable := tableFieldIDs(t)

	if strings.Join(readable, ",") != strings.Join(writable, ",") {
		t.Errorf("the two halves of the field-id table disagree\n  readable: %v\n  writable: %v",
			readable, writable)
	}
	// The control for the assertion above: it is only meaningful while
	// the table covers something. An empty-versus-empty comparison
	// passes forever, including on the day both switches lose all their
	// cases.
	if len(readable) == 0 {
		t.Fatal("the field-id table claims no fields at all, so the equality above proves nothing")
	}
}

func TestAFieldWithNowhereToGoIsRefusedByName(t *testing.T) {
	// local_volume's `path` is this case today: config.StorageMedium has
	// no Path field on this branch (#666 adds it), so a configuration
	// naming it cannot be stored. The refusal has to NAME the field,
	// because the alternative is a save that reports success having
	// dropped the one value that decides where the backups go.
	_, err := storageMediumFromFields(
		config.StorageMedium{ID: "volume_one", Type: "local_volume"},
		backend.Manifest{
			ID:     "local_volume",
			Fields: []backend.Field{{ID: "path", Kind: backend.KindPath, Required: true}},
		},
		StorageMediumConfiguration{Fields: map[string]string{"path": "/mnt/backups"}},
	)
	if err == nil {
		t.Fatal("a field this manager cannot store was accepted; a save would have dropped it silently")
	}
	if !strings.Contains(err.Error(), "path") {
		t.Errorf("the refusal does not name the field it refused: %v", err)
	}
	// And it does not quote the operator's value back, which is
	// mediumcreds.go's rule and the reason every message in
	// backend/validate.go was rewritten: refusals by SHAPE, never by
	// content.
	if strings.Contains(err.Error(), "/mnt/backups") {
		t.Errorf("the refusal echoes the value it was given: %v", err)
	}
}

func TestAFieldLeftOutIsUnsetRatherThanLeftAlone(t *testing.T) {
	// A whole-record replace, which is what UpdateStorageMedium's own
	// doc argues for: a medium's fields are not independent, and a patch
	// would need a second spelling for "clear this". So a field the
	// caller did not send is unset, and the destination that had a
	// prefix and now has none is the destination that was described.
	spec, err := storageMediumFromFields(
		config.StorageMedium{ID: "offsite", Type: "s3", Prefix: "monthly", Region: "us-east-1"},
		backend.Manifest{
			ID: "s3",
			Fields: []backend.Field{
				{ID: "bucket", Kind: backend.KindString, Required: true},
				{ID: "prefix", Kind: backend.KindKeyPrefix},
				{ID: "region", Kind: backend.KindString},
			},
		},
		StorageMediumConfiguration{Fields: map[string]string{"bucket": "nas-backups"}},
	)
	if err != nil {
		t.Fatalf("folding a legal configuration: %v", err)
	}
	if spec.Bucket != "nas-backups" {
		t.Errorf("bucket: got %q, want %q", spec.Bucket, "nas-backups")
	}
	if spec.Prefix != "" {
		t.Errorf("prefix was left at %q; a field the caller did not send is unset, not inherited", spec.Prefix)
	}
	if spec.Region != "" {
		t.Errorf("region was left at %q; same rule", spec.Region)
	}
	// The two fields a configuration may never change: which instance
	// this is, and which backend it is an instance of.
	if spec.ID != "offsite" || spec.Type != "s3" {
		t.Errorf("identity moved: got %q/%q, want offsite/s3", spec.ID, spec.Type)
	}
}

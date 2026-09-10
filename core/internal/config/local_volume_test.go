package config

import (
	"strings"
	"testing"
)

// Tests for issue #666 (EPIC I / #664, I1.3): the local_volume medium
// type, so an operator with a second disk has somewhere to declare it,
// and more than one local destination is an ordinary configuration
// rather than a special case bolted beside the reserved local id.

// localVolumeConfig returns a Config Validate accepts, carrying one
// declared local_volume medium and a tier pointed at it. Individual
// tests copy it and break exactly one thing, mediumsConfig's own
// discipline.
func localVolumeConfig() Config {
	c := validConfig()
	c.Retention = Retention{
		Timezone:     "UTC",
		WeekStartsOn: "monday",
		Tiers: []RetentionTier{
			{Name: "daily", Granularity: GranularityDay, Keep: 7},
			{Name: "monthly", Granularity: GranularityMonth, Keep: 12, Medium: "second_disk"},
		},
	}
	c.StorageMediums = []StorageMedium{{
		ID:   "second_disk",
		Type: StorageMediumTypeLocalVolume,
		Path: "/mnt/second-disk/backups",
	}}
	return c
}

// TestValidate_ALocalVolumeConfigIsAccepted is the fixture's own control.
func TestValidate_ALocalVolumeConfigIsAccepted(t *testing.T) {
	c := localVolumeConfig()
	mustValidate(t, &c)
}

// TestValidate_LocalVolumeFieldRules is #666's own version of FR-27's
// validation table: every rule with a refusing case, so a rule that never
// fires cannot be mistaken for one that holds.
func TestValidate_LocalVolumeFieldRules(t *testing.T) {
	for _, tc := range []struct {
		name    string
		break_  func(*StorageMedium)
		wantErr []string
	}{
		{"empty path", func(m *StorageMedium) { m.Path = "" }, []string{"storage_mediums[0]", "path"}},
		{"relative path", func(m *StorageMedium) { m.Path = "relative/backups" }, []string{"storage_mediums[0]", "path", "absolute"}},
		{"path with a traversal segment", func(m *StorageMedium) { m.Path = "/mnt/../etc" }, []string{"storage_mediums[0]", "path"}},
		{"bucket set on a local_volume medium", func(m *StorageMedium) { m.Bucket = "nas-backups" }, []string{"storage_mediums[0]", "bucket", "local_volume"}},
		{"region set on a local_volume medium", func(m *StorageMedium) { m.Region = "us-east-1" }, []string{"storage_mediums[0]", "region", "local_volume"}},
		{"endpoint set on a local_volume medium", func(m *StorageMedium) { m.Endpoint = "https://example.com" }, []string{"storage_mediums[0]", "endpoint", "local_volume"}},
		{"storage class set on a local_volume medium", func(m *StorageMedium) { m.StorageClass = StorageClassStandard }, []string{"storage_mediums[0]", "storage_class", "local_volume"}},
		{"credentials.file set on a local_volume medium", func(m *StorageMedium) { m.Credentials = MediumCredentials{File: "/var/lib/backup-manager/creds"} }, []string{"storage_mediums[0]", "credentials", "local_volume"}},
		{"credentials.env set on a local_volume medium", func(m *StorageMedium) { m.Credentials = MediumCredentials{Env: "BACKUP_KEY"} }, []string{"storage_mediums[0]", "credentials", "local_volume"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := localVolumeConfig()
			tc.break_(&c.StorageMediums[0])
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a local_volume medium with %s", tc.name)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not carry %q", err, want)
				}
			}
		})
	}
}

// TestValidate_PathForbiddenOnAnS3Medium is LocalVolumeFieldRules' mirror
// on the s3 side: a field that only means something for a directory must
// not be silently accepted on a bucket, for validateMaxMovesPerCycle's
// own reason (an accepted, ignored key reads as one that took effect).
func TestValidate_PathForbiddenOnAnS3Medium(t *testing.T) {
	c := mediumsConfig()
	c.StorageMediums[0].Path = "/mnt/somewhere"
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted path set on an s3 medium")
	}
	if !strings.Contains(err.Error(), "path") || !strings.Contains(err.Error(), "s3") {
		t.Errorf("error %q does not name path and s3", err)
	}
}

// TestValidate_ReservedLocalIdIsLegalOnlyAsALocalVolumeInstance is #666's
// half of the doctrine Main ruled on for #670: config.MediumLocal stays
// reserved against every OTHER type (the existing "id claims the
// reserved local" case in TestValidate_StorageMediumFieldRules already
// pins that direction, unchanged), and becomes legal EXACTLY when it
// names an instance of the local_volume backend - "local" is legal only
// as instance zero of local_volume, never as any other type. This is
// what lets a first-run seed writer declare id: local, type: local_volume
// once and have every later restart go on validating the same file with
// no migration: the schema has to accept the shape the product itself
// writes, forever, not just at the moment it was written.
//
// The relaxation is schema-only. It says nothing about whether
// CreateStorageMedium or UpdateStorageMedium may ever mint or rename
// into this id - that operator-facing guard is mediums.go's own
// (mediumFromSpec), unchanged by this test or by anything in this
// package, and stays in force regardless of what this function accepts.
//
// This test does not point a tier explicitly AT "local" (`medium: local`
// in a tier is a separate, tier-level spelling rule -
// validateTierMedium's own refusal - and threading a genuinely-declared
// "local" through THAT surface is #670's invariant work, not #666's).
// The medium being declared at all, with no tier needing to name it
// explicitly (it is already every unset tier's destination by default),
// is the whole of what this schema-level relaxation has to prove.
func TestValidate_ReservedLocalIdIsLegalOnlyAsALocalVolumeInstance(t *testing.T) {
	c := localVolumeConfig()
	c.StorageMediums[0].ID = MediumLocal
	c.Retention.Tiers[1].Medium = ""
	mustValidate(t, &c)
}

// TestValidate_TierResolvesANamedLocalVolumeInstance proves a retention
// tier can select ANY declared local_volume instance by its own id, not
// only the reserved one: "local", "second_disk" and "archive_disk" are
// three names an operator chose for three destinations of one backend,
// and a tier has to be able to name any of them.
func TestValidate_TierResolvesANamedLocalVolumeInstance(t *testing.T) {
	c := validConfig()
	c.Retention = Retention{
		Timezone:     "UTC",
		WeekStartsOn: "monday",
		Tiers: []RetentionTier{
			{Name: "daily", Granularity: GranularityDay, Keep: 7},
			{Name: "monthly", Granularity: GranularityMonth, Keep: 12, Medium: "second_disk"},
			{Name: "yearly", Granularity: GranularityYear, Keep: 5, Medium: "archive_disk"},
		},
	}
	c.StorageMediums = []StorageMedium{
		{ID: "second_disk", Type: StorageMediumTypeLocalVolume, Path: "/mnt/second-disk"},
		{ID: "archive_disk", Type: StorageMediumTypeLocalVolume, Path: "/mnt/archive-disk"},
	}
	mustValidate(t, &c)
}

// TestValidate_DuplicateLocalVolumeAndS3IdsAreStillRefused proves the
// generic duplicate-id rule is type-agnostic: a local_volume instance and
// an s3 medium sharing an id are still one ambiguous reference, exactly
// like two mediums of the same type would be.
func TestValidate_DuplicateLocalVolumeAndS3IdsAreStillRefused(t *testing.T) {
	c := mediumsConfig()
	c.StorageMediums = append(c.StorageMediums, StorageMedium{
		ID:   c.StorageMediums[0].ID,
		Type: StorageMediumTypeLocalVolume,
		Path: "/mnt/second-disk",
	})
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted two mediums sharing one id across two types")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error %q does not say duplicate", err)
	}
}

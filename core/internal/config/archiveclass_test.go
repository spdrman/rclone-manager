package config

import (
	"strings"
	"testing"
)

// Tests for issue #442: a retention tier whose medium writes with an
// archive storage class validates clean today and can never work.
//
// The reasoning is #428's and it is short. A move has to reach VERIFIED
// before the source copy is deleted, VERIFIED means the destination
// achieved the class its medium requires, both configurable classes need
// to read the object, and an object written to GLACIER or DEEP_ARCHIVE is
// archived the instant it lands. So the move is refused, every cycle, for
// ever; and even if it were not, FR-30's standing invariant needs an
// ACTIVE placement at content class that an archived copy cannot hold.
//
// The refusal is scoped to the tier-to-medium PAIRING and not to the
// medium declaration. A declared archive-class medium holding objects an
// operator restores by hand is exactly what the restore operation exists
// for, so it has to keep validating; what cannot work is a retention tier
// delivering to one.

// archiveTierConfig is mediumsConfig with the one medium moved onto an
// archive class, which is the pairing this file is about. The tier
// already names that medium, so this is the smallest config that
// expresses the mistake.
func archiveTierConfig(class string) Config {
	c := mediumsConfig()
	c.StorageMediums[0].StorageClass = class
	return c
}

// TestValidate_ARetentionTierOnAnArchiveClassIsRefused is the acceptance
// line: Validate refuses the pairing, names the reason, and says what to
// do instead.
func TestValidate_ARetentionTierOnAnArchiveClassIsRefused(t *testing.T) {
	for _, class := range ArchiveStorageClasses() {
		t.Run(class, func(t *testing.T) {
			c := archiveTierConfig(class)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a retention tier delivering to a %s medium, which can never take delivery of an artifact", class)
			}
			for _, want := range []string{
				"retention.tiers[1]",            // the tier's own key, so an operator knows which line to edit
				"monthly",                       // the tier's name
				"offsite_s3",                    // the medium it names
				class,                           // the storage class the refusal turns on
				"archived the instant it lands", // the mechanism, in the engine's own words
				StorageClassStandard,            // at least one class that would work
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not carry %q, so it does not tell an operator what to change:\n%s", want, err)
				}
			}
			t.Logf("refused with: %v", err)
		})
	}
}

// TestValidate_ADeclaredArchiveMediumNoTierNamesStillValidates is the
// other half of the scope, and it is the half that is easy to break while
// implementing the first.
//
// FR-27 already calls a declared medium no tier references legal, and an
// archive-class one is the case the restore operation is written for: an
// operator who has objects on DEEP_ARCHIVE declares the medium so this
// product can see them and restore them, and points no tier at it because
// nothing is going to be delivered there. A refusal that reached the
// declaration would make that configuration unwritable.
func TestValidate_ADeclaredArchiveMediumNoTierNamesStillValidates(t *testing.T) {
	for _, class := range ArchiveStorageClasses() {
		t.Run(class, func(t *testing.T) {
			c := mediumsConfig()
			c.StorageMediums = append(c.StorageMediums, StorageMedium{
				ID:           "offsite_cold",
				Type:         StorageMediumTypeS3,
				Region:       "us-east-1",
				Bucket:       "nas-backups-cold",
				StorageClass: class,
				Credentials:  MediumCredentials{Env: "BACKUP_S3_COLD"},
			})
			mustValidate(t, &c)
		})
	}
}

// TestValidate_ANonArchiveClassTierIsUntouched is the accepting control
// the refusal above needs. Without it a check that refused every tier
// with a medium would pass the refusal table and nobody would notice
// until every deployment stopped loading.
//
// GLACIER_IR is the row that earns this test its keep. It carries the
// word Glacier and it reads on demand, so a check written against the
// name rather than against the class table would refuse it.
func TestValidate_ANonArchiveClassTierIsUntouched(t *testing.T) {
	archive := map[string]bool{}
	for _, class := range ArchiveStorageClasses() {
		archive[class] = true
	}
	warm := 0
	for _, class := range StorageClasses() {
		if archive[class] {
			continue
		}
		warm++
		t.Run(class, func(t *testing.T) {
			c := archiveTierConfig(class)
			mustValidate(t, &c)
		})
	}
	if warm < 2 {
		t.Fatalf("only %d non-archive class(es) were exercised, so this control proves almost nothing", warm)
	}
}

// TestValidate_ThePerSetChainIsCheckedToo is #333's second chain, which is
// the easy one to miss: a backup set may carry a whole retention policy of
// its own, and a tier in it can name a medium exactly as the deployment's
// policy can.
//
// A check that only walked the global chain would accept an archive
// pairing written one level down, and the artifacts of that one set would
// be the ones that never arrive.
func TestValidate_ThePerSetChainIsCheckedToo(t *testing.T) {
	c := mediumsConfig()
	c.StorageMediums = append(c.StorageMediums, StorageMedium{
		ID:           "offsite_cold",
		Type:         StorageMediumTypeS3,
		Region:       "us-east-1",
		Bucket:       "nas-backups-cold",
		StorageClass: StorageClassDeepArchive,
		Credentials:  MediumCredentials{Env: "BACKUP_S3_COLD"},
	})

	// The control first: the same set with its own chain on the WARM
	// medium validates, so the refusal below is about the class and not
	// about per-set chains being refused wholesale.
	warm := c
	warm.Sources[0].BackupSets[0].RetentionConfig = &Retention{
		Tiers: []RetentionTier{
			{Name: "daily", Granularity: GranularityDay, Keep: 7},
			{Name: "yearly", Granularity: GranularityYear, Keep: 3, Medium: "offsite_s3"},
		},
	}
	mustValidate(t, &warm)

	c.Sources[0].BackupSets[0].RetentionConfig = &Retention{
		Tiers: []RetentionTier{
			{Name: "daily", Granularity: GranularityDay, Keep: 7},
			{Name: "yearly", Granularity: GranularityYear, Keep: 3, Medium: "offsite_cold"},
		},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted a per-set retention chain delivering to a DEEP_ARCHIVE medium")
	}
	for _, want := range []string{"sources[0].backup_sets[0].retention.tiers[1]", "yearly", "offsite_cold", StorageClassDeepArchive} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q, so it does not point at the chain that wrote it:\n%s", want, err)
		}
	}
}

// TestValidate_AnUnknownStorageClassIsReportedOnceAtItsOwnKey is about the
// message an operator gets rather than about safety.
//
// An unrecognised class is already refused where it is written. Reporting
// it a second time as "this tier delivers to an archive class" would send
// somebody to the retention chain to fix a typo in the mediums block, so
// the pairing rule deliberately only runs for a class this build knows.
func TestValidate_AnUnknownStorageClassIsReportedOnceAtItsOwnKey(t *testing.T) {
	c := archiveTierConfig("GLACIER_DEEP_FLEXIBLE_INSTANT")
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted an unknown storage class")
	}
	if !strings.Contains(err.Error(), "storage_mediums[0]") {
		t.Errorf("the unknown class is not reported at the key that carries it:\n%s", err)
	}
	if strings.Contains(err.Error(), "archived the instant it lands") {
		t.Errorf("an unknown storage class was also reported as an archive pairing, which sends an operator to the wrong key:\n%s", err)
	}
}

// TestArchiveStorageClassesIsExactlyGlacierAndDeepArchive pins this
// package's own copy of the set, in this package, so a reader of the
// validator can see what it turns on without following an import.
//
// The pinning against internal/archive's table, which is the check that
// stops the two drifting, is in the external test package because only
// that one can import both. See archivepin_test.go.
func TestArchiveStorageClassesIsExactlyGlacierAndDeepArchive(t *testing.T) {
	got := ArchiveStorageClasses()
	want := []string{StorageClassGlacier, StorageClassDeepArchive}
	if len(got) != len(want) {
		t.Fatalf("ArchiveStorageClasses() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ArchiveStorageClasses() = %v, want %v", got, want)
		}
	}

	// Every one of them is a class the schema accepts. A set naming a
	// class validStorageClasses refuses would be a rule about something
	// no operator can write.
	for _, class := range got {
		if !validStorageClasses[class] {
			t.Errorf("%q is in the archive set and is not a class this schema accepts at all", class)
		}
	}

	// And the caller cannot reach into this package's own copy.
	got[0] = "MUTATED"
	if ArchiveStorageClasses()[0] == "MUTATED" {
		t.Error("ArchiveStorageClasses() returns the package's own slice, so a caller can edit the set the validator reads")
	}
}

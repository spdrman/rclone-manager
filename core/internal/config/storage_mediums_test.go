package config

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/artifactstore"
)

// Tests for issue #234 (EPIC E, E1.2): the storage-medium config schema,
// tier placement, and credential references. FR-27 owns the mediums and
// the tier key, FR-33 owns the credential custody half, FR-35 owns the
// round-trip rule.
//
// Nothing outside this package reads any of these fields yet. What is
// under test here is therefore exactly the schema: what a config file may
// say, what Validate refuses, and what a settings save must not inject
// into a file that never heard of any of it.

// mediumsConfig returns a Config that Validate accepts, carrying one
// declared medium and one tier pointed at it. Individual tests copy it and
// break exactly one thing, the same discipline validConfig() follows.
func mediumsConfig() Config {
	c := validConfig()
	c.Retention = Retention{
		Timezone:     "UTC",
		WeekStartsOn: "monday",
		Tiers: []RetentionTier{
			{Name: "daily", Granularity: GranularityDay, Keep: 7},
			{Name: "monthly", Granularity: GranularityMonth, Keep: 12, Medium: "offsite_s3"},
		},
	}
	c.StorageMediums = []StorageMedium{{
		ID:          "offsite_s3",
		Type:        StorageMediumTypeS3,
		Region:      "us-east-1",
		Bucket:      "nas-backups",
		Prefix:      "rclone-manager",
		Credentials: MediumCredentials{File: "/var/lib/backup-manager/s3/offsite_s3.creds"},
	}}
	return c
}

func mustValidate(t *testing.T, c *Config) {
	t.Helper()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestValidate_AMediumsConfigIsAccepted is the fixture's own control. Every
// refusal test below breaks one field of this config, so if the fixture did
// not validate on its own the refusals would all pass for the wrong reason.
func TestValidate_AMediumsConfigIsAccepted(t *testing.T) {
	c := mediumsConfig()
	mustValidate(t, &c)
}

// TestValidate_StorageMediumFieldRules is FR-27's validation table: every
// rule with a refusing case and an accepting one, so no rule can pass by
// never firing.
func TestValidate_StorageMediumFieldRules(t *testing.T) {
	for _, tc := range []struct {
		name    string
		break_  func(*StorageMedium)
		wantErr []string // every substring the message must carry
	}{
		{"empty id", func(m *StorageMedium) { m.ID = "" }, []string{"storage_mediums[0]", "id"}},
		{"id is not lower_snake_case", func(m *StorageMedium) { m.ID = "OffsiteS3" }, []string{"storage_mediums[0]", "OffsiteS3", "lower_snake_case"}},
		{"id starts with a digit", func(m *StorageMedium) { m.ID = "3s" }, []string{"storage_mediums[0]", "lower_snake_case"}},
		{"id claims the reserved local", func(m *StorageMedium) { m.ID = MediumLocal }, []string{"storage_mediums[0]", "local", "reserved"}},
		{"empty type", func(m *StorageMedium) { m.Type = "" }, []string{"storage_mediums[0]", "type", StorageMediumTypeS3}},
		{"unknown type", func(m *StorageMedium) { m.Type = "azure" }, []string{"storage_mediums[0]", "azure", StorageMediumTypeS3}},
		{"empty bucket", func(m *StorageMedium) { m.Bucket = "" }, []string{"storage_mediums[0]", "bucket"}},
		{"unknown storage class", func(m *StorageMedium) { m.StorageClass = "COLD" }, []string{"storage_mediums[0]", "storage_class", "COLD", StorageClassDeepArchive}},
		{"lower-case storage class", func(m *StorageMedium) { m.StorageClass = "standard" }, []string{"storage_mediums[0]", "storage_class", "standard"}},
		{"unknown upload verification", func(m *StorageMedium) { m.UploadVerification = "trust" }, []string{"storage_mediums[0]", "upload_verification", "trust", UploadVerificationReadback}},
		{"prefix with a leading slash", func(m *StorageMedium) { m.Prefix = "/rclone-manager" }, []string{"storage_mediums[0]", "prefix"}},
		{"prefix with a trailing slash", func(m *StorageMedium) { m.Prefix = "rclone-manager/" }, []string{"storage_mediums[0]", "prefix"}},
		{"prefix with an empty segment", func(m *StorageMedium) { m.Prefix = "rclone//manager" }, []string{"storage_mediums[0]", "prefix"}},
		{"prefix with a traversal segment", func(m *StorageMedium) { m.Prefix = "rclone/../manager" }, []string{"storage_mediums[0]", "prefix", ".."}},
		{"prefix with a dot segment", func(m *StorageMedium) { m.Prefix = "rclone/./manager" }, []string{"storage_mediums[0]", "prefix"}},
		{"bucket carrying a prefix", func(m *StorageMedium) { m.Bucket = "nas-backups/rclone-manager" }, []string{"storage_mediums[0]", "bucket", "prefix"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mediumsConfig()
			tc.break_(&c.StorageMediums[0])
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a medium with %s", tc.name)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not carry %q, so it does not tell the operator which key and which value collided", err, want)
				}
			}
		})
	}
}

// TestValidate_AcceptsEveryStorageClassAndVerificationMode is the accepting
// half of the two closed-set rules above. Without it a validator that
// refused every value would still pass the refusal table.
func TestValidate_AcceptsEveryStorageClassAndVerificationMode(t *testing.T) {
	// mediumsConfig's one medium is named by a tier, so an archive class
	// on it is the pairing #442 refuses. The class itself is still
	// accepted, on a medium no tier delivers to, which is the case the
	// restore operation exists for; that is what the second branch
	// checks, and archiveclass_test.go carries the refusal side.
	archived := map[string]bool{}
	for _, class := range ArchiveStorageClasses() {
		archived[class] = true
	}
	for _, class := range StorageClasses() {
		t.Run("storage_class "+class, func(t *testing.T) {
			c := mediumsConfig()
			if archived[class] {
				c.StorageMediums = append(c.StorageMediums, StorageMedium{
					ID:           "offsite_cold",
					Type:         StorageMediumTypeS3,
					Region:       "us-east-1",
					Bucket:       "nas-backups-cold",
					StorageClass: class,
					Credentials:  MediumCredentials{Env: "BACKUP_S3_COLD"},
				})
				mustValidate(t, &c)
				return
			}
			c.StorageMediums[0].StorageClass = class
			mustValidate(t, &c)
		})
	}
	// upload_verification is the one closed set whose members are not all
	// acceptable on their own, and the split is deliberate. `attested` is
	// IN the set, because the schema knows the word and has to refuse it
	// for the right reason; it is then refused by the achievability rule,
	// because no medium type this build has a backend for can produce the
	// full-object digest it needs (validateUploadVerificationIsAchievable,
	// and TestValidate_AttestedIsRefusedOnAMediumTypeThatCannotAttest).
	//
	// So the accepting half of the SET rule is carried by readback, and
	// the assertion on attested is that it is refused as unachievable
	// rather than as an unknown value. A validator that had started
	// refusing every mode would fail the first branch.
	for _, mode := range UploadVerificationModes() {
		t.Run("upload_verification "+mode, func(t *testing.T) {
			c := mediumsConfig()
			c.StorageMediums[0].UploadVerification = mode
			if mode != UploadVerificationAttested {
				mustValidate(t, &c)
				return
			}
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted upload_verification %q, which no medium type this build has a backend for can achieve", mode)
			}
			if !strings.Contains(err.Error(), "cannot be achieved") {
				t.Errorf("%q was refused, but not as unachievable: %v", mode, err)
			}
			if strings.Contains(err.Error(), "is not one of") {
				t.Errorf("%q was refused as a value outside the closed set; it is inside it, and the reason it cannot be written is a different one: %v", mode, err)
			}
		})
	}
	for _, typ := range StorageMediumTypes() {
		t.Run("type "+typ, func(t *testing.T) {
			c := mediumsConfig()
			c.StorageMediums[0].Type = typ
			mustValidate(t, &c)
		})
	}
	t.Run("both omitted", func(t *testing.T) {
		c := mediumsConfig()
		c.StorageMediums[0].StorageClass = ""
		c.StorageMediums[0].UploadVerification = ""
		mustValidate(t, &c)
	})
	// The accepting half of the prefix rules. Without these a validator
	// that refused every prefix, or every non-empty one, would still pass
	// the refusal table above.
	for _, prefix := range []string{"", "rclone-manager", "team/rclone-manager", "a/b/c", "rclone_manager.v2"} {
		t.Run("prefix "+prefix, func(t *testing.T) {
			c := mediumsConfig()
			c.StorageMediums[0].Prefix = prefix
			mustValidate(t, &c)
		})
	}
}

// TestValidate_RefusesDuplicateMediumIDs: two mediums with one id makes a
// tier's reference ambiguous, and a silent last-one-wins would decide where
// an operator's artifacts live by list order.
func TestValidate_RefusesDuplicateMediumIDs(t *testing.T) {
	c := mediumsConfig()
	c.StorageMediums = append(c.StorageMediums, c.StorageMediums[0])
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted two mediums declaring the same id")
	}
	for _, want := range []string{"storage_mediums[1]", "offsite_s3", "storage_mediums[0]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
}

// TestValidate_AnUnreferencedMediumIsLegal pins FR-27's explicit
// allowance: an operator staging a destination before pointing a tier at
// it has written a valid config, not a mistake.
func TestValidate_AnUnreferencedMediumIsLegal(t *testing.T) {
	c := mediumsConfig()
	c.StorageMediums = append(c.StorageMediums, StorageMedium{
		ID:          "staging_only",
		Type:        StorageMediumTypeS3,
		Bucket:      "nas-backups-staging",
		Credentials: MediumCredentials{Env: "BACKUP_S3_STAGING"},
	})
	mustValidate(t, &c)
}

// TestValidate_ADeclaredMediumIsLegalWithTheLegacyScalarSpelling is the
// other half of "an unreferenced medium is legal", in the branch that
// cannot reference one at all. The three daily_days/weekly_months/
// monthly_months scalars have no field for a medium, so a config using
// them plus a declared medium is an operator staging a destination before
// migrating to the chain spelling, which FR-27 calls legal.
func TestValidate_ADeclaredMediumIsLegalWithTheLegacyScalarSpelling(t *testing.T) {
	c := mediumsConfig()
	c.Retention = Retention{Timezone: "UTC", WeekStartsOn: "monday", DailyDays: 7}
	mustValidate(t, &c)

	if len(c.StorageMediums) != 1 {
		t.Fatalf("the fixture lost its medium: %+v", c.StorageMediums)
	}
	for i, tier := range c.Retention.EffectiveTiers() {
		if got := tier.EffectiveMedium(); got != MediumLocal {
			t.Errorf("the scalar chain's tier %d resolves to %q, want %q", i, got, MediumLocal)
		}
	}
}

// TestValidate_TierMediumReferences covers the tier half of FR-27: absence
// means local, a declared id resolves, a dangling id is refused, and the
// reserved local id is not a legal tier value because absence is how local
// is spelled.
func TestValidate_TierMediumReferences(t *testing.T) {
	t.Run("a declared medium resolves", func(t *testing.T) {
		c := mediumsConfig()
		mustValidate(t, &c)
		if got := c.Retention.Tiers[1].EffectiveMedium(); got != "offsite_s3" {
			t.Errorf("EffectiveMedium() = %q, want offsite_s3", got)
		}
	})

	t.Run("no medium key resolves to local", func(t *testing.T) {
		c := mediumsConfig()
		mustValidate(t, &c)
		if got := c.Retention.Tiers[0].EffectiveMedium(); got != MediumLocal {
			t.Errorf("EffectiveMedium() = %q, want %q", got, MediumLocal)
		}
		if c.Retention.Tiers[0].Medium != "" {
			t.Errorf("Medium = %q; validation must not write the resolved default back into the operator's file shape", c.Retention.Tiers[0].Medium)
		}
	})

	t.Run("a dangling reference is refused", func(t *testing.T) {
		c := mediumsConfig()
		c.Retention.Tiers[1].Medium = "not_declared"
		err := c.Validate()
		if err == nil {
			t.Fatal("Validate accepted a tier naming a medium no storage_mediums entry declares")
		}
		for _, want := range []string{"retention.tiers[1]", "not_declared", "storage_mediums"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	t.Run("a dangling reference is refused when no medium is declared at all", func(t *testing.T) {
		c := mediumsConfig()
		c.StorageMediums = nil
		err := c.Validate()
		if err == nil {
			t.Fatal("Validate accepted a tier medium with an empty storage_mediums list")
		}
		if !strings.Contains(err.Error(), "offsite_s3") {
			t.Errorf("error %q does not name the medium that does not exist", err)
		}
	})

	t.Run("the reserved local id is not a legal tier value", func(t *testing.T) {
		c := mediumsConfig()
		c.Retention.Tiers[1].Medium = MediumLocal
		err := c.Validate()
		if err == nil {
			t.Fatal("Validate accepted medium: local on a tier; absence is the only spelling of local, so a settings save cannot inject the key into a config that never opted in")
		}
		for _, want := range []string{"retention.tiers[1]", "local", "omit"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	t.Run("a malformed medium name is refused by shape alone", func(t *testing.T) {
		// ValidateRetention is the CLI's own override path and has no
		// storage_mediums list to resolve against, so the SHAPE rule has
		// to live with the tier rather than with the reference check, or
		// that path validates a chain nothing ever looked at.
		r := retentionWithTiers(RetentionTier{Name: "monthly", Granularity: GranularityMonth, Keep: 12, Medium: "Offsite S3"})
		err := ValidateRetention(&r)
		if err == nil {
			t.Fatal("ValidateRetention accepted a tier medium that is not lower_snake_case")
		}
		if !strings.Contains(err.Error(), "lower_snake_case") {
			t.Errorf("error %q does not say what a medium id must look like", err)
		}
	})
}

// TestValidate_PerSetRetentionMediumReferences is the second chain a
// config can carry, and the one a reference check that only walked the
// global policy would miss entirely.
//
// A per-set retention override (#333) is a whole chain in its own right,
// so a set writing "retention: {tiers: [...]}" names a medium exactly the
// way the deployment-wide policy does. I found this by rebasing onto the
// override landing rather than by design, which is why it gets its own
// test rather than a line in the table above.
func TestValidate_PerSetRetentionMediumReferences(t *testing.T) {
	// perSet returns a config whose one backup set overrides retention
	// with a chain, with that chain's second tier naming medium.
	perSet := func(medium string) Config {
		c := mediumsConfig()
		c.Sources[0].BackupSets[0].RetentionConfig = &Retention{
			Tiers: []RetentionTier{
				{Name: "daily", Granularity: GranularityDay, Keep: 7},
				{Name: "monthly", Granularity: GranularityMonth, Keep: 12, Medium: medium},
			},
		}
		return c
	}

	t.Run("a declared medium resolves", func(t *testing.T) {
		c := perSet("offsite_s3")
		mustValidate(t, &c)
		got := c.Sources[0].BackupSets[0].Retention.Tiers[1].EffectiveMedium()
		if got != "offsite_s3" {
			t.Errorf("the resolved per-set chain reports medium %q, want offsite_s3", got)
		}
	})

	t.Run("a dangling reference is refused", func(t *testing.T) {
		c := perSet("not_declared")
		err := c.Validate()
		if err == nil {
			t.Fatal("Validate accepted a per-set retention chain naming a medium no storage_mediums entry declares")
		}
		for _, want := range []string{"sources[0].backup_sets[0].retention", "tiers[1]", "not_declared"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q, so it does not locate the offending chain", err, want)
			}
		}
	})

	t.Run("the reserved local id is refused there too", func(t *testing.T) {
		c := perSet(MediumLocal)
		err := c.Validate()
		if err == nil {
			t.Fatal("Validate accepted medium: local in a per-set retention chain")
		}
	})

	t.Run("one dangling global medium is reported once, not once per set", func(t *testing.T) {
		// A set with NO override resolves to a clone of the global chain,
		// so walking every set's resolved policy would turn one mistake
		// into one message per backup set.
		c := mediumsConfig()
		c.Retention.Tiers[1].Medium = "not_declared"
		second := c.Sources[0].BackupSets[0]
		second.Name = "postgres-secondary"
		second.LocalPath = "/backups/production/postgres-secondary"
		c.Sources[0].BackupSets = append(c.Sources[0].BackupSets, second)

		err := c.Validate()
		if err == nil {
			t.Fatal("Validate accepted a dangling global medium")
		}
		if n := strings.Count(err.Error(), "not_declared"); n != 1 {
			t.Errorf("one dangling medium was reported %d times across %d backup sets; want once:\n%s",
				n, len(c.Sources[0].BackupSets), err)
		}
	})
}

// TestValidate_MediumCredentialSources is FR-33's schema half: three
// sources, exactly one set, and the same argv rules Key.Command already
// carries.
func TestValidate_MediumCredentialSources(t *testing.T) {
	t.Run("each source alone is accepted", func(t *testing.T) {
		for name, creds := range map[string]MediumCredentials{
			"file":    {File: "/var/lib/backup-manager/s3/offsite.creds"},
			"env":     {Env: "BACKUP_S3_OFFSITE"},
			"command": {Command: []string{"/usr/bin/op", "read", "op://infra/s3"}},
		} {
			t.Run(name, func(t *testing.T) {
				c := mediumsConfig()
				c.StorageMediums[0].Credentials = creds
				mustValidate(t, &c)
			})
		}
	})

	for _, tc := range []struct {
		name    string
		creds   MediumCredentials
		wantErr []string
	}{
		{"none set", MediumCredentials{}, []string{"storage_mediums[0].credentials", "file", "env", "command"}},
		{"file and env", MediumCredentials{File: "/a", Env: "B"}, []string{"storage_mediums[0].credentials", "exactly one"}},
		{"file and command", MediumCredentials{File: "/a", Command: []string{"/bin/true"}}, []string{"exactly one"}},
		{"env and command", MediumCredentials{Env: "B", Command: []string{"/bin/true"}}, []string{"exactly one"}},
		{"all three", MediumCredentials{File: "/a", Env: "B", Command: []string{"/bin/true"}}, []string{"exactly one"}},
		{"empty executable", MediumCredentials{Command: []string{""}}, []string{"storage_mediums[0].credentials.command", "executable"}},
		{"relative executable", MediumCredentials{Command: []string{"op", "read"}}, []string{"storage_mediums[0].credentials.command", "absolute"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mediumsConfig()
			c.StorageMediums[0].Credentials = tc.creds
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted credentials: %s", tc.name)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not carry %q", err, want)
				}
			}
		})
	}
}

// TestValidate_CredentialProblemsNeverEchoTheValue is the schema-side half
// of FR-33's "a resolver failure is reported by the shape of the problem,
// never by the content that failed". config.Validate never resolves a
// credential, but it does hold whatever an operator typed, and a message
// that quoted an env var's name is one careless edit away from quoting a
// value.
func TestValidate_CredentialProblemsNeverEchoTheValue(t *testing.T) {
	const canary = "canary-value-no-message-may-repeat"

	for _, tc := range []struct {
		name  string
		creds MediumCredentials
	}{
		{
			// Both sources a careless message could quote, set at once,
			// so this fires whichever one leaked.
			"two sources, both carrying the canary",
			MediumCredentials{File: "/var/lib/" + canary, Env: canary},
		},
		{
			// The argv case. Validate legitimately quotes Command[0], an
			// executable path, exactly as validateKey does; it must not
			// widen that to the whole argv, whose later elements are the
			// reference a secrets manager resolves.
			"a refused argv carrying the canary as an argument",
			MediumCredentials{Command: []string{"op", "read", "op://infra/" + canary}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mediumsConfig()
			c.StorageMediums[0].Credentials = tc.creds
			err := c.Validate()
			if err == nil {
				t.Fatal("precondition failed: these credentials must be refused, or this test asserts nothing")
			}
			if strings.Contains(err.Error(), canary) {
				t.Errorf("the refusal echoed the credential-bearing value back:\n%s", err)
			}
		})
	}
}

// TestLoad_RefusesAnInlineSecretUnderAMedium is FR-33's planted schema
// gate. There is no field for a literal key, so an inline one is an
// unknown field and dies in Load before validation runs at all.
func TestLoad_RefusesAnInlineSecretUnderAMedium(t *testing.T) {
	_, err := Load("testdata/medium_inline_secret.yaml")
	if err == nil {
		t.Fatal("Load accepted a config spelling secret_access_key inline under a storage medium; the schema must have no field a literal key fits into")
	}
	if !strings.Contains(err.Error(), "access_key_id") && !strings.Contains(err.Error(), "secret_access_key") {
		t.Errorf("error %q does not name the inline secret key it refused", err)
	}

	// Positive control: the same file with the two inline keys removed
	// loads, so the refusal above is about those keys and not about some
	// unrelated defect in the fixture.
	raw, readErr := os.ReadFile("testdata/medium_inline_secret.yaml")
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "access_key_id:") || strings.Contains(line, "secret_access_key:") {
			continue
		}
		kept = append(kept, line)
	}
	// The medium still needs a credential source, indented under it, or
	// the control would prove only that a different mistake also fails.
	repaired := t.TempDir() + "/repaired.yaml"
	body := strings.Join(kept, "\n") + "    credentials:\n      env: BACKUP_S3_OFFSITE\n"
	if err := os.WriteFile(repaired, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Load(repaired); err != nil {
		t.Fatalf("positive control: the same medium without the inline keys must load, got %v", err)
	}
}

// TestLoad_StorageMediumsRoundTripFromYAML proves the schema is spellable
// in a config file exactly as FR-27's example spells it. KnownFields(true)
// means a mis-tagged field would be a parse error rather than a silently
// ignored one, so this is the only test that can catch a wrong yaml tag.
func TestLoad_StorageMediumsRoundTripFromYAML(t *testing.T) {
	cfg, err := Load("testdata/storage-mediums.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	wantMediums := []StorageMedium{
		{
			ID:                 "offsite_s3",
			Type:               "s3",
			Region:             "us-east-1",
			Endpoint:           "",
			Bucket:             "nas-backups",
			Prefix:             "rclone-manager",
			StorageClass:       StorageClassStandard,
			UploadVerification: UploadVerificationReadback,
			Credentials:        MediumCredentials{File: "/var/lib/backup-manager/s3/offsite_s3.creds"},
		},
		{
			ID:           "offsite_annual",
			Type:         "s3",
			Region:       "us-east-1",
			Bucket:       "nas-backups-annual",
			StorageClass: StorageClassStandardIA,
			Credentials:  MediumCredentials{Command: []string{"/usr/bin/op", "read", "op://infra/backup-manager/s3-annual"}},
		},
		{
			ID:           "offsite_cold",
			Type:         "s3",
			Region:       "us-east-1",
			Bucket:       "nas-backups-cold",
			StorageClass: StorageClassDeepArchive,
			Credentials:  MediumCredentials{Command: []string{"/usr/bin/op", "read", "op://infra/backup-manager/s3-cold"}},
		},
		{
			ID:          "staging_only",
			Type:        "s3",
			Bucket:      "nas-backups-staging",
			Credentials: MediumCredentials{Env: "BACKUP_S3_STAGING"},
		},
	}
	if len(cfg.StorageMediums) != len(wantMediums) {
		t.Fatalf("parsed %d medium(s), want %d: %+v", len(cfg.StorageMediums), len(wantMediums), cfg.StorageMediums)
	}
	for i := range wantMediums {
		got, want := cfg.StorageMediums[i], wantMediums[i]
		if got.ID != want.ID || got.Type != want.Type || got.Region != want.Region ||
			got.Endpoint != want.Endpoint || got.Bucket != want.Bucket || got.Prefix != want.Prefix ||
			got.StorageClass != want.StorageClass || got.UploadVerification != want.UploadVerification ||
			got.Credentials.File != want.Credentials.File || got.Credentials.Env != want.Credentials.Env ||
			strings.Join(got.Credentials.Command, " ") != strings.Join(want.Credentials.Command, " ") {
			t.Errorf("storage_mediums[%d] = %+v, want %+v", i, got, want)
		}
	}

	// The annual tier names a colder-but-readable class rather than an
	// archive one. That changed with #442: an archive-class medium a tier
	// delivers to is refused at load, so the fixture that has to Validate
	// at the end of this test cannot spell that pairing. The DEEP_ARCHIVE
	// medium is still declared and still parsed, referenced by nothing,
	// which is the half of the shape that stays legal.
	wantTierMediums := []string{"", "offsite_s3", "offsite_annual"}
	if len(cfg.Retention.Tiers) != len(wantTierMediums) {
		t.Fatalf("parsed %d tier(s), want %d", len(cfg.Retention.Tiers), len(wantTierMediums))
	}
	for i, want := range wantTierMediums {
		if got := cfg.Retention.Tiers[i].Medium; got != want {
			t.Errorf("retention.tiers[%d].medium = %q, want %q", i, got, want)
		}
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate on the parsed config: %v", err)
	}
}

// TestEffectiveMediumDefaults covers the three accessors that resolve an
// omitted key, and the reason they are accessors rather than in-place
// defaults: a value Validate wrote back into the struct would be frozen
// into the operator's file by the next settings save (#294's lesson).
func TestEffectiveMediumDefaults(t *testing.T) {
	t.Run("an omitted tier medium is local", func(t *testing.T) {
		if got := (RetentionTier{}).EffectiveMedium(); got != MediumLocal {
			t.Errorf("EffectiveMedium() = %q, want %q", got, MediumLocal)
		}
	})
	t.Run("an explicit tier medium is itself", func(t *testing.T) {
		if got := (RetentionTier{Medium: "offsite_s3"}).EffectiveMedium(); got != "offsite_s3" {
			t.Errorf("EffectiveMedium() = %q, want offsite_s3", got)
		}
	})
	t.Run("an omitted storage class is STANDARD", func(t *testing.T) {
		if got := (StorageMedium{}).EffectiveStorageClass(); got != StorageClassStandard {
			t.Errorf("EffectiveStorageClass() = %q, want %q", got, StorageClassStandard)
		}
	})
	t.Run("an explicit storage class is itself", func(t *testing.T) {
		if got := (StorageMedium{StorageClass: StorageClassGlacier}).EffectiveStorageClass(); got != StorageClassGlacier {
			t.Errorf("EffectiveStorageClass() = %q, want %q", got, StorageClassGlacier)
		}
	})
	t.Run("an omitted upload verification is readback", func(t *testing.T) {
		if got := (StorageMedium{}).EffectiveUploadVerification(); got != UploadVerificationReadback {
			t.Errorf("EffectiveUploadVerification() = %q, want %q", got, UploadVerificationReadback)
		}
	})
	t.Run("an explicit upload verification is itself", func(t *testing.T) {
		if got := (StorageMedium{UploadVerification: UploadVerificationAttested}).EffectiveUploadVerification(); got != UploadVerificationAttested {
			t.Errorf("EffectiveUploadVerification() = %q, want %q", got, UploadVerificationAttested)
		}
	})
	t.Run("readback is the default, not attested", func(t *testing.T) {
		// FR-31's direction: the safe verification class is the one an
		// operator gets without asking. A default of "attested" would
		// mean a config that says nothing trusts the endpoint's own
		// checksum before deleting the local copy.
		if UploadVerificationReadback == UploadVerificationAttested {
			t.Fatal("the two verification modes must be distinct values")
		}
		if (StorageMedium{}).EffectiveUploadVerification() == UploadVerificationAttested {
			t.Error("an unconfigured medium must not default to trusting the endpoint's attestation")
		}
	})
}

// TestEffectiveTiersCarriesTheMedium: EffectiveTiers is the accessor every
// consumer of the chain reads (internal/retention decides through it, and
// core/service reports through it), so a medium that did not survive it
// would be a medium nothing downstream could ever see.
//
// The legacy scalar branch is checked in the same test because that is the
// branch every pre-EPIC-E config takes, and the answer there has to be
// "local everywhere" rather than an empty string that reads as unset.
func TestEffectiveTiersCarriesTheMedium(t *testing.T) {
	t.Run("an explicit chain", func(t *testing.T) {
		c := mediumsConfig()
		mustValidate(t, &c)
		tiers := c.Retention.EffectiveTiers()
		if len(tiers) != 2 {
			t.Fatalf("EffectiveTiers() has %d tier(s), want 2", len(tiers))
		}
		if tiers[1].Medium != "offsite_s3" {
			t.Errorf("EffectiveTiers()[1].Medium = %q, want offsite_s3", tiers[1].Medium)
		}
		if got := tiers[1].EffectiveMedium(); got != "offsite_s3" {
			t.Errorf("EffectiveMedium() = %q, want offsite_s3", got)
		}
	})

	t.Run("the legacy scalar chain is local throughout", func(t *testing.T) {
		r := Retention{}
		mustValidateRetention(t, &r)
		for i, tier := range r.EffectiveTiers() {
			if tier.Medium != "" {
				t.Errorf("the default chain's tier %d carries medium %q; the scalars cannot name one", i, tier.Medium)
			}
			if got := tier.EffectiveMedium(); got != MediumLocal {
				t.Errorf("the default chain's tier %d resolves to %q, want %q", i, got, MediumLocal)
			}
		}
	})
}

// mediumKeyLine matches a YAML mapping key this change introduces, at any
// indentation. FR-35 forbids either of them appearing in a file that never
// configured one.
var mediumKeyLine = regexp.MustCompile(`(?m)^\s*(storage_mediums|medium):`)

// TestMarshal_ANoMediumConfigGainsNoMediumKeys is FR-35's round-trip rule
// held at the one place that can hold it byte for byte: the marshaler
// core/service's writeConfigAtomically feeds the file from.
func TestMarshal_ANoMediumConfigGainsNoMediumKeys(t *testing.T) {
	// Both spellings of a config that never heard of any of this: the
	// explicit chain (which HAS tiers a medium key could land on) and the
	// legacy scalars (which have no tiers at all). The second is the
	// shape of every config written before FR-18, and the first is where
	// a missing omitempty would actually show.
	for _, fixture := range []string{"testdata/retention-tiers.yaml", "testdata/minimal.yaml"} {
		t.Run(fixture, func(t *testing.T) { assertNoMediumKeysSurviveAReMarshal(t, fixture) })
	}
}

func assertNoMediumKeysSurviveAReMarshal(t *testing.T, fixture string) {
	t.Helper()
	cfg, err := Load(fixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	base, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if found := mediumKeyLine.FindAllString(string(base), -1); len(found) != 0 {
		t.Errorf("a config that names no medium came back from a re-marshal carrying %v:\n%s", found, base)
	}

	// The byte comparison, in the one form that can actually fail. It is
	// deliberately NOT a before-Validate/after-Validate diff: Validate
	// resolves defaults IN PLACE and is documented to, so those two
	// differ for reasons that have nothing to do with mediums (a
	// defaulted delete_safety_delay, a defaulted alert threshold), and an
	// assertion that fails on the honest case teaches nothing.
	//
	// What FR-35 actually forbids is a settings form's EMPTY submission
	// becoming a key in the file. A form that renders a mediums list and
	// a per-tier medium picker, on a config that configured neither,
	// hands this struct an empty slice and an empty string, and without
	// omitempty those marshal to "storage_mediums: []" and "medium: """
	// on a file that never opted in. An older binary then refuses that
	// file outright under Load's KnownFields(true). So: set both to their
	// explicitly-empty form and require the bytes not to move.
	cfg.StorageMediums = []StorageMedium{}
	for i := range cfg.Retention.Tiers {
		cfg.Retention.Tiers[i].Medium = ""
	}
	afterEmptySubmission, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(base) != string(afterEmptySubmission) {
		t.Errorf("an explicitly-empty mediums submission changed the marshaled file.\nbefore:\n%s\nafter:\n%s", base, afterEmptySubmission)
	}

	// Positive control: the scan does find the keys when they are there,
	// so the two absences above are evidence rather than a dead regexp.
	withMediums := mediumsConfig()
	encoded, err := yaml.Marshal(&withMediums)
	if err != nil {
		t.Fatalf("Marshal a config that does declare a medium: %v", err)
	}
	found := mediumKeyLine.FindAllString(string(encoded), -1)
	if len(found) < 2 {
		t.Errorf("positive control: a config that declares a medium and points a tier at it must marshal both keys, got %v:\n%s", found, encoded)
	}
}

// TestValidate_IsIdempotentWithStorageMediums: Validate's own doc promises
// a second call is a no-op, and the CLI's override path depends on it.
func TestValidate_IsIdempotentWithStorageMediums(t *testing.T) {
	first := mediumsConfig()
	mustValidate(t, &first)
	encodedOnce, err := yaml.Marshal(&first)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	mustValidate(t, &first)
	encodedTwice, err := yaml.Marshal(&first)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(encodedOnce) != string(encodedTwice) {
		t.Errorf("a second Validate changed the config:\nonce:\n%s\ntwice:\n%s", encodedOnce, encodedTwice)
	}
}

// TestValidate_CollectsEveryMediumProblem: Validate collects rather than
// stopping at the first problem, so a config wrong in three places does not
// cost an operator three restarts. A per-medium early return is the easy
// way to break that.
func TestValidate_CollectsEveryMediumProblem(t *testing.T) {
	c := mediumsConfig()
	c.StorageMediums[0].Type = "azure"
	c.StorageMediums[0].StorageClass = "COLD"
	c.StorageMediums = append(c.StorageMediums, StorageMedium{
		ID:          "second_bad",
		Type:        StorageMediumTypeS3,
		Bucket:      "", // missing
		Credentials: MediumCredentials{Env: "X"},
	})
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted three separate medium problems")
	}
	for _, want := range []string{"azure", "COLD", "storage_mediums[1]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q; every problem must be reported in one pass", err, want)
		}
	}
}

// TestMediumLocalIsTheStoreKindOfTheSameName pins the one string two
// packages have to agree on. config names the implicit medium `local` and
// artifactstore names its only backend `local`; FR-29 records a placement
// under one of those and resolves a store from it, so a drift between the
// two spellings is an artifact whose location cannot be interpreted.
func TestMediumLocalIsTheStoreKindOfTheSameName(t *testing.T) {
	if MediumLocal != string(artifactstore.KindLocal) {
		t.Errorf("config.MediumLocal = %q but artifactstore.KindLocal = %q; a placement recorded under one cannot resolve a store under the other",
			MediumLocal, artifactstore.KindLocal)
	}
}

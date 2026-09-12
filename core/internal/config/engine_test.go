// The configuration seam EPIC K adds, tested from the direction that can
// break a running deployment.
//
// Nothing outside this package acts on any of these fields yet: the engine,
// the repository domain, the consistency mode and the verification level
// are schema and resolution only, and the lifecycle that reads them is
// #783. So what is under test here is exactly the schema -- what a config
// file may say, what Validate refuses, what it resolves silence into, and
// what a settings save must not inject into a file that never heard of any
// of it.
//
// The load-bearing case is the LAST of those. core/service rewrites the
// whole Config on every settings save, so a key that appeared in an
// operator's file because they changed an unrelated setting is a file an
// older binary refuses outright under Load's KnownFields(true) (FR-35), and
// a set that acquired an engine it never asked for would be a job silently
// reinterpreted.

package config

import (
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/backupdproject/backupd/core/internal/model"
)

// incrementalConfig returns a Config that Validate accepts, carrying one
// declared repository domain and one incremental set that names it, beside
// the artifact set validConfig() already has. Individual tests copy it and
// break exactly one thing, the same discipline validConfig() follows.
func incrementalConfig() Config {
	c := validConfig()
	c.RepositoryDomains = []RepositoryDomainConfig{{
		ID:          "production",
		Description: "Production snapshots for this deployment",
		Isolation:   string(model.RepositoryShared),
	}}

	incremental := c.Sources[0].BackupSets[0]
	incremental.Name = "uploads-tree"
	incremental.EngineConfig = string(model.EngineKopia)
	incremental.UUID = "6f1d2b7a-1c4e-4f8b-9a2d-3e5c7b9d1f00"
	incremental.RepositoryDomainConfig = "production"
	incremental.ConsistencyConfig = string(model.ModeExternalSnapshot)
	incremental.VerificationLevelConfig = string(model.LevelContentSample)
	incremental.RemotePath = "/snapshots/nightly/srv/uploads"
	incremental.SourceMountPrefix = "/snapshots/nightly"
	incremental.LocalPath = "/backups/production/uploads"
	incremental.Validation = Validation{}
	c.Sources[0].BackupSets = append(c.Sources[0].BackupSets, incremental)

	return c
}

// theSet is the resolved backup set at a position, so the assertions below
// read as statements about a set rather than about slice indices.
func theSet(t *testing.T, c *Config, i int) BackupSet {
	t.Helper()
	if len(c.Sources) == 0 || len(c.Sources[0].BackupSets) <= i {
		t.Fatalf("the fixture has no backup set at index %d", i)
	}
	return c.Sources[0].BackupSets[i]
}

// TestValidate_AnIncrementalConfigIsAccepted is the fixture's own control.
// Every refusal test below breaks one field of it, so a fixture that had
// stopped validating would make all of them pass for the wrong reason.
func TestValidate_AnIncrementalConfigIsAccepted(t *testing.T) {
	c := incrementalConfig()
	mustValidate(t, &c)
}

// TestValidate_AnOmittedEngineResolvesToArtifact is EPIC K's promise to
// every configuration that already exists, held at the layer an operator's
// file actually passes through.
//
// It runs over the checked-in fixtures rather than only over a struct built
// in Go, because those are the files an operator copies: full.yaml is the
// documented example and minimal.yaml is what proves the defaults are
// reachable without writing every key. Neither mentions an engine, and both
// must come out of Validate running the engine they have always run.
func TestValidate_AnOmittedEngineResolvesToArtifact(t *testing.T) {
	for _, fixture := range []string{
		"testdata/full.yaml",
		"testdata/minimal.yaml",
		"testdata/retention-tiers.yaml",
		"testdata/storage-mediums.yaml",
	} {
		t.Run(fixture, func(t *testing.T) {
			cfg, err := Load(fixture)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}

			for i, src := range cfg.Sources {
				for j, bs := range src.BackupSets {
					if bs.Engine != model.EngineArtifact {
						t.Errorf("sources[%d].backup_sets[%d] (%s) resolved to engine %q; a set that never mentioned an engine must keep running the artifact engine",
							i, j, bs.ID, bs.Engine)
					}
					// And it acquires none of the incremental
					// vocabulary: an artifact set has no repository to
					// share, no source identity to keep stable, and has
					// promised nothing about consistency.
					if !bs.Repository.IsZero() {
						t.Errorf("%s carries repository reference %s; an artifact set has no repository", bs.ID, bs.Repository)
					}
					if bs.SourceIdentity != "" {
						t.Errorf("%s carries source identity %s; only an incremental set has a snapshot lineage", bs.ID, bs.SourceIdentity)
					}
					if bs.Consistency != "" || bs.VerificationLevel != "" {
						t.Errorf("%s resolved consistency %q and verification level %q; resolving either for an artifact set invents a claim nobody made",
							bs.ID, bs.Consistency, bs.VerificationLevel)
					}
				}
			}
		})
	}
}

// engineKeyLine matches a YAML mapping key this change introduces, at any
// indentation. FR-35 forbids any of them appearing in a file that never
// configured one.
var engineKeyLine = regexp.MustCompile(`(?m)^\s*(engine|uuid|repository_domain|repository_domains|source_consistency|verification_level|source_mount_prefix):`)

// TestMarshal_ANoEngineConfigGainsNoEngineKeys is FR-35's round-trip rule
// held at the one place that can hold it byte for byte: the marshaler
// core/service's writeConfigAtomically feeds the file from.
//
// The second half is the one that can actually fail. A settings form that
// renders an engine picker and a domain picker, submitted unchanged on a
// config that configured neither, hands this struct empty strings and an
// empty slice; without omitempty those marshal to "engine: """ and
// "repository_domains: []" on a file that never opted in, and an older
// binary refuses that file outright.
func TestMarshal_ANoEngineConfigGainsNoEngineKeys(t *testing.T) {
	for _, fixture := range []string{"testdata/full.yaml", "testdata/minimal.yaml"} {
		t.Run(fixture, func(t *testing.T) {
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
			if found := engineKeyLine.FindAllString(string(base), -1); len(found) != 0 {
				t.Errorf("a config that names no engine came back from a re-marshal carrying %v:\n%s", found, base)
			}

			cfg.RepositoryDomains = []RepositoryDomainConfig{}
			for i := range cfg.Sources {
				for j := range cfg.Sources[i].BackupSets {
					bs := &cfg.Sources[i].BackupSets[j]
					bs.EngineConfig = ""
					bs.UUID = ""
					bs.RepositoryDomainConfig = ""
					bs.ConsistencyConfig = ""
					bs.VerificationLevelConfig = ""
					bs.SourceMountPrefix = ""
				}
			}
			afterEmptySubmission, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(base) != string(afterEmptySubmission) {
				t.Errorf("an explicitly-empty engine submission changed the marshaled file.\nbefore:\n%s\nafter:\n%s", base, afterEmptySubmission)
			}
		})
	}

	// Positive control: the scan does find the keys when they are there,
	// so the absences above are evidence rather than a dead regexp.
	withEngine := incrementalConfig()
	encoded, err := yaml.Marshal(&withEngine)
	if err != nil {
		t.Fatalf("Marshal a config that does configure an engine: %v", err)
	}
	if found := engineKeyLine.FindAllString(string(encoded), -1); len(found) < 6 {
		t.Errorf("positive control: a config with an incremental set must marshal every key it wrote, got %v:\n%s", found, encoded)
	}
}

// TestLoadParsesAnIncrementalSet is the golden parse: the checked-in
// example of a deployment running both engines, read through the same
// KnownFields(true) parser an operator's file goes through.
//
// It also holds the Load/Validate seam this package is built on. Load
// parses and resolves nothing: the engine, the repository reference and the
// source identity are all still zero afterwards, exactly as BackupSet.ID
// is, because a second place that resolves them is a second answer.
func TestLoadParsesAnIncrementalSet(t *testing.T) {
	cfg, err := Load("testdata/engine-kopia.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.RepositoryDomains) != 2 {
		t.Fatalf("parsed %d repository domains, want 2", len(cfg.RepositoryDomains))
	}
	if got, want := cfg.RepositoryDomains[0].ID, "production"; got != want {
		t.Errorf("RepositoryDomains[0].ID = %q, want %q", got, want)
	}
	if got, want := cfg.RepositoryDomains[1].Isolation, string(model.RepositoryIsolated); got != want {
		t.Errorf("RepositoryDomains[1].Isolation = %q, want %q", got, want)
	}

	incremental := theSet(t, cfg, 1)
	if got, want := incremental.EngineConfig, "kopia"; got != want {
		t.Errorf("the incremental set's engine = %q, want %q", got, want)
	}
	if incremental.Engine != "" || !incremental.Repository.IsZero() || incremental.SourceIdentity != "" {
		t.Errorf("Load resolved the engine seam (engine=%q repository=%s identity=%s); resolution is Validate's job, and a second place that does it is a second answer",
			incremental.Engine, incremental.Repository, incremental.SourceIdentity)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	artifact, incremental := theSet(t, cfg, 0), theSet(t, cfg, 1)
	if artifact.Engine != model.EngineArtifact {
		t.Errorf("the set with no engine key resolved to %q", artifact.Engine)
	}
	if incremental.Engine != model.EngineKopia {
		t.Fatalf("the incremental set resolved to engine %q", incremental.Engine)
	}
	if got, want := incremental.Repository.Domain, model.RepositoryDomainID("production"); got != want {
		t.Errorf("the incremental set's repository domain = %q, want %q", got, want)
	}
	if got, want := incremental.Repository.Set, incremental.ID; got != want {
		t.Errorf("the repository reference names set %q, want %q; a reference that does not carry the set cannot be checked for co-tenancy", got, want)
	}
	if got, want := incremental.Consistency, model.ModeExternalSnapshot; got != want {
		t.Errorf("consistency = %q, want %q", got, want)
	}
	if got, want := incremental.VerificationLevel, model.LevelContentSample; got != want {
		t.Errorf("verification level = %q, want %q", got, want)
	}
	if incremental.SourceIdentity == "" {
		t.Error("the incremental set has no source identity, so nothing can find its predecessor snapshot")
	}
}

// TestValidate_AnIncrementalSetResolvesTheWeakestClaimByDefault covers the
// two keys an incremental set may omit.
//
// Both defaults are the ones that promise least. A source nobody arranged
// anything around is live and best-effort, because inferring quiescence
// from silence would let a run report a point-in-time snapshot that never
// existed; and a restore point nobody asked to have read is verified
// structurally, because the stronger rungs cost real I/O that no operator
// asked for.
func TestValidate_AnIncrementalSetResolvesTheWeakestClaimByDefault(t *testing.T) {
	c := incrementalConfig()
	c.Sources[0].BackupSets[1].ConsistencyConfig = ""
	c.Sources[0].BackupSets[1].VerificationLevelConfig = ""
	mustValidate(t, &c)

	incremental := theSet(t, &c, 1)
	if got, want := incremental.Consistency, model.ModeLiveBestEffort; got != want {
		t.Errorf("an omitted source_consistency resolved to %q, want %q; a mode inferred from silence is a guarantee nobody made", got, want)
	}
	if got, want := incremental.VerificationLevel, model.LevelStructural; got != want {
		t.Errorf("an omitted verification_level resolved to %q, want %q", got, want)
	}
}

// TestValidate_RefusesAnUnknownEngine is "never silently reinterpret an
// existing job" at the config layer.
func TestValidate_RefusesAnUnknownEngine(t *testing.T) {
	c := incrementalConfig()
	c.Sources[0].BackupSets[1].EngineConfig = "kopia-v2"

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted engine: kopia-v2")
	}
	for _, want := range []string{"engine", "kopia-v2", "artifact", "kopia"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it does not say what was refused or what is legal: %v", want, err)
		}
	}
}

// TestValidate_AnIncrementalSetMustNameADeclaredDomain is the core of the
// Repository Domain seam: sharing is never inferred, so a set that stores
// snapshots has to say whose boundaries it shares, and the answer has to be
// a domain the config actually declares.
//
// A default would be the wrong answer in the only direction available. An
// incremental set that silently landed in some "default" repository would be
// deduplicating against, sharing an encryption key with and sharing a
// corruption fate with whatever else happened to be there -- which is
// exactly the decision EPIC K says must be intentional.
func TestValidate_AnIncrementalSetMustNameADeclaredDomain(t *testing.T) {
	t.Run("omitted", func(t *testing.T) {
		c := incrementalConfig()
		c.Sources[0].BackupSets[1].RepositoryDomainConfig = ""

		err := c.Validate()
		if err == nil {
			t.Fatal("Validate accepted an incremental set with no repository_domain; a set whose repository nobody named would share encryption, credentials and a corruption fate with whatever it landed beside")
		}
		for _, want := range []string{"repository_domain", "kopia"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not mention %q: %v", want, err)
			}
		}
	})

	t.Run("undeclared", func(t *testing.T) {
		c := incrementalConfig()
		c.Sources[0].BackupSets[1].RepositoryDomainConfig = "prod"

		err := c.Validate()
		if err == nil {
			t.Fatal("Validate accepted a repository_domain that no repository_domains entry declares")
		}
		for _, want := range []string{"prod", "repository_domains", "production"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not mention %q, so it does not say what is declared: %v", want, err)
			}
		}
	})
}

// TestValidate_RepositoryDomainsStateTheirBoundary is what makes the
// security boundary explicit in the file rather than in a default: a domain
// that does not say whether it is shared is refused, and so are the two
// ways a declaration can be ambiguous about which domain it is.
func TestValidate_RepositoryDomainsStateTheirBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*Config)
		want   []string
	}{
		{
			name:   "isolation omitted",
			break_: func(c *Config) { c.RepositoryDomains[0].Isolation = "" },
			want:   []string{"isolation", "shared", "isolated"},
		},
		{
			name:   "isolation misspelled",
			break_: func(c *Config) { c.RepositoryDomains[0].Isolation = "private" },
			want:   []string{"isolation", "private"},
		},
		{
			name:   "id empty",
			break_: func(c *Config) { c.RepositoryDomains[0].ID = "" },
			want:   []string{"repository_domains[0]", "id"},
		},
		{
			name:   "id is a path",
			break_: func(c *Config) { c.RepositoryDomains[0].ID = "prod/main" },
			want:   []string{"repository_domains[0]", "prod/main"},
		},
		{
			name: "duplicate ids",
			break_: func(c *Config) {
				c.RepositoryDomains = append(c.RepositoryDomains, RepositoryDomainConfig{
					ID:        "production",
					Isolation: string(model.RepositoryIsolated),
				})
			},
			// Two entries claiming one id is two boundaries with one
			// name: whichever one a set means, the other is silently
			// not in force.
			want: []string{"repository_domains[1]", "production", "repository_domains[0]"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := incrementalConfig()
			tc.break_(&c)

			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a repository domain with %s", tc.name)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// TestValidate_AnIsolatedDomainRefusesASecondSet is the boundary holding
// against the edit that would quietly dissolve it: a second set pointed at
// a domain an operator declared single-tenant.
//
// The shared control beside it is what makes this a test of the declared
// isolation rather than of co-tenancy in general, which is legal and
// common.
func TestValidate_AnIsolatedDomainRefusesASecondSet(t *testing.T) {
	c := incrementalConfig()
	c.RepositoryDomains[0].Isolation = string(model.RepositoryIsolated)

	second := c.Sources[0].BackupSets[1]
	second.Name = "uploads-archive"
	second.UUID = "0c9a44e2-77b1-4d3f-8a61-5f0ab2c3d4e5"
	second.RemotePath = "/snapshots/nightly/srv/archive"
	second.LocalPath = "/backups/production/archive"
	c.Sources[0].BackupSets = append(c.Sources[0].BackupSets, second)

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted two backup sets in a domain declared isolated; an isolated domain that silently becomes shared is a security boundary that only ever existed in the config file")
	}
	for _, want := range []string{"production", "production/uploads-tree", "production/uploads-archive", "shared"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it names neither the sets involved nor the way out: %v", want, err)
		}
	}

	// The control: the same two sets in a shared domain are accepted.
	c.RepositoryDomains[0].Isolation = string(model.RepositoryShared)
	mustValidate(t, &c)
}

// TestValidate_ADeclaredDomainNobodyUsesIsLegal keeps a staged
// configuration workable, the same relaxation storage mediums already have:
// declaring the boundary before pointing a set at it is how an operator
// builds one up, and refusing it would mean a domain can only be added in
// the same edit as the set that uses it.
func TestValidate_ADeclaredDomainNobodyUsesIsLegal(t *testing.T) {
	c := validConfig()
	c.RepositoryDomains = []RepositoryDomainConfig{{
		ID:        "future",
		Isolation: string(model.RepositoryShared),
	}}
	mustValidate(t, &c)
}

// TestValidate_RefusesIncrementalKeysOnAnArtifactSet is the dead-config
// rule this package already applies to sftp fields on a local remote: a key
// that cannot ever be acted on is refused rather than ignored, because an
// operator who wrote it believes something about how their backup runs.
func TestValidate_RefusesIncrementalKeysOnAnArtifactSet(t *testing.T) {
	for _, tc := range []struct {
		key    string
		break_ func(*BackupSet)
	}{
		{"repository_domain", func(bs *BackupSet) { bs.RepositoryDomainConfig = "production" }},
		{"source_consistency", func(bs *BackupSet) { bs.ConsistencyConfig = string(model.ModeExternalSnapshot) }},
		{"verification_level", func(bs *BackupSet) { bs.VerificationLevelConfig = string(model.LevelContentFull) }},
		{"source_mount_prefix", func(bs *BackupSet) { bs.SourceMountPrefix = "/mnt/user" }},
		{"uuid", func(bs *BackupSet) { bs.UUID = "6f1d2b7a-1c4e-4f8b-9a2d-3e5c7b9d1f00" }},
	} {
		t.Run(tc.key, func(t *testing.T) {
			c := incrementalConfig()
			tc.break_(&c.Sources[0].BackupSets[0]) // the artifact set

			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s on a set running the artifact engine, where nothing will ever read it", tc.key)
			}
			for _, want := range []string{tc.key, "kopia"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// TestValidate_AnIncrementalSetNeedsADurableIdentifier is what keeps a
// lineage attached to a set rather than to its name.
//
// A backup set's configured identity is source-plus-name (FR-7), and names
// get edited. If the snapshot lineage hung off the name, renaming a set in
// the UI would orphan every snapshot it has ever taken -- the next run
// would find no predecessor, re-read the whole source and store it again.
// So an incremental set carries an identifier nothing renames, and it is
// required rather than derived: deriving it from the name would be the bug
// this rule exists to prevent, with extra steps.
func TestValidate_AnIncrementalSetNeedsADurableIdentifier(t *testing.T) {
	t.Run("omitted", func(t *testing.T) {
		c := incrementalConfig()
		c.Sources[0].BackupSets[1].UUID = ""

		err := c.Validate()
		if err == nil {
			t.Fatal("Validate accepted an incremental set with no uuid; its snapshot lineage would hang off a name an operator can edit")
		}
		if !strings.Contains(err.Error(), "uuid") {
			t.Errorf("the refusal does not mention the key: %v", err)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		for _, bad := range []string{"not-a-uuid", "6f1d2b7a1c4e4f8b9a2d3e5c7b9d1f00", "6f1d2b7a-1c4e-4f8b-9a2d-3e5c7b9d1f0"} {
			c := incrementalConfig()
			c.Sources[0].BackupSets[1].UUID = bad

			if err := c.Validate(); err == nil {
				t.Errorf("Validate accepted uuid %q", bad)
			}
		}
	})

	t.Run("two sets may not share one", func(t *testing.T) {
		c := incrementalConfig()
		second := c.Sources[0].BackupSets[1]
		second.Name = "uploads-archive"
		second.RemotePath = "/snapshots/nightly/srv/archive"
		second.LocalPath = "/backups/production/archive"
		c.Sources[0].BackupSets = append(c.Sources[0].BackupSets, second)

		err := c.Validate()
		if err == nil {
			t.Fatal("Validate accepted two backup sets carrying one uuid; their snapshots would interleave into a single lineage and each set's retention would prune the other's restore points")
		}
		if !strings.Contains(err.Error(), second.UUID) {
			t.Errorf("the refusal does not name the duplicated identifier: %v", err)
		}
	})
}

// TestValidate_SourceIdentityIgnoresHowThisDeploymentReachesTheSource is the
// stability property at the config layer, over the three moves a real
// deployment makes: the staging directory changes, the mount the source
// arrives on changes, and the set gets renamed.
//
// None of those is a different source, and a lineage that forked on any of
// them would silently re-read and re-store everything.
func TestValidate_SourceIdentityIgnoresHowThisDeploymentReachesTheSource(t *testing.T) {
	base := incrementalConfig()
	mustValidate(t, &base)
	want := theSet(t, &base, 1).SourceIdentity
	if want == "" {
		t.Fatal("the fixture resolved no source identity")
	}

	for _, tc := range []struct {
		name string
		move func(*Config)
		hold bool // true: the identity must not change
	}{
		{
			name: "the local staging path moves",
			move: func(c *Config) { c.Sources[0].BackupSets[1].LocalPath = "/mnt/newdisk/backups/uploads" },
			hold: true,
		},
		{
			name: "the snapshot is mounted somewhere else",
			move: func(c *Config) {
				c.Sources[0].BackupSets[1].RemotePath = "/tmp/quiesce-47/srv/uploads"
				c.Sources[0].BackupSets[1].SourceMountPrefix = "/tmp/quiesce-47"
			},
			hold: true,
		},
		{
			name: "the set is renamed",
			move: func(c *Config) { c.Sources[0].BackupSets[1].Name = "uploads-tree-v2" },
			hold: true,
		},
		{
			name: "the port is written out instead of defaulted",
			move: func(c *Config) { c.Sources[0].BackupSets[1].Remote.Port = 22 },
			hold: true,
		},
		{
			name: "a different directory on the same source",
			move: func(c *Config) { c.Sources[0].BackupSets[1].RemotePath = "/snapshots/nightly/srv/other" },
			hold: false,
		},
		{
			name: "a different host",
			move: func(c *Config) { c.Sources[0].BackupSets[1].Remote.Host = "staging.example.internal" },
			hold: false,
		},
		{
			name: "a different durable identifier",
			move: func(c *Config) { c.Sources[0].BackupSets[1].UUID = "0c9a44e2-77b1-4d3f-8a61-5f0ab2c3d4e5" },
			hold: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := incrementalConfig()
			tc.move(&c)
			mustValidate(t, &c)

			got := theSet(t, &c, 1).SourceIdentity
			switch {
			case tc.hold && got != want:
				t.Errorf("the source identity forked when %s:\n  before %s\n  after  %s\n"+
					"the next run would find no predecessor snapshot, re-read the whole source and store a second full copy", tc.name, want, got)
			case !tc.hold && got == want:
				t.Errorf("the source identity survived %s, so two genuinely different sources share one snapshot lineage", tc.name)
			}
		})
	}
}

// TestValidate_RefusesAMountPrefixThatIsNotOne catches the configuration
// mistake the stability rule invites: a source_mount_prefix that is not
// actually a prefix of remote_path. Ignoring it would put the whole mount
// path back into the identity for the one deployment that tried hardest to
// get this right.
func TestValidate_RefusesAMountPrefixThatIsNotOne(t *testing.T) {
	for _, prefix := range []string{
		"/elsewhere",        // nothing to do with the path
		"/snapshots/night",  // matches as text, not as a path segment
		"snapshots/nightly", // relative
	} {
		c := incrementalConfig()
		c.Sources[0].BackupSets[1].SourceMountPrefix = prefix

		err := c.Validate()
		if err == nil {
			t.Errorf("Validate accepted source_mount_prefix %q against remote_path %q", prefix, c.Sources[0].BackupSets[1].RemotePath)
			continue
		}
		if !strings.Contains(err.Error(), "source_mount_prefix") {
			t.Errorf("the refusal does not name the key: %v", err)
		}
	}
}

// TestValidate_IsIdempotentWithAnIncrementalSet: Validate's own doc promises
// a second call is a no-op, and the CLI's retention override path depends on
// it. The engine seam resolves five fields, and a second pass that
// re-derived any of them differently would be a source identity that
// changed inside one process.
func TestValidate_IsIdempotentWithAnIncrementalSet(t *testing.T) {
	c := incrementalConfig()
	mustValidate(t, &c)
	first := theSet(t, &c, 1)

	mustValidate(t, &c)
	second := theSet(t, &c, 1)

	if first.Engine != second.Engine ||
		first.Repository != second.Repository ||
		first.Consistency != second.Consistency ||
		first.VerificationLevel != second.VerificationLevel ||
		first.SourceIdentity != second.SourceIdentity {
		t.Errorf("a second Validate resolved the engine seam differently:\n  first  %+v\n  second %+v",
			first.Repository, second.Repository)
	}
}

// Package conformance_test is EPIC E's composed end-to-end scenario:
// job one of issue #242.
//
// Everything else in this repository that touches a storage medium proves
// one piece. internal/placement proves the move engine against a double.
// tests/miniointegration proves the engine over a real S3 API, one move at
// a time, against a hand-built journal. tests/movecrash proves the engine
// survives a real SIGKILL at every phase boundary, against a local
// directory standing in for a bucket. tests/compat proves a medium-free
// deployment did not move.
//
// None of those runs the thing an operator actually configures: a chain of
// tiers, each naming a medium, deciding for itself which artifacts belong
// where, over real time, against a real endpoint. This package does that.
// It builds the three-tier chain FR-27 and the phase 2 exit gate name
// (daily on local, monthly on `s3`, annual on an `s3` cold class), seeds a
// backup set with artifacts spread over two years, and then advances the
// clock and lets the product's own arithmetic decide what has to move.
//
// # What is real here, and what is not
//
// Real: the config and its validation, the journal and its migrations, the
// GFS chain evaluation, FR-27's home-medium rule, the move engine, the
// rclone s3 backend, and a MinIO server in a container. The scenario never
// tells the engine what to do; it asks retention where each artifact
// belongs and hands the answer over unmodified.
//
// Not real, and said out loud rather than papered over:
//
//   - The daemon's own cycle loop does not drive this. The wiring from a
//     retention pass to placement.Engine.RunCycle is #239's, and it is not
//     in this tree. So this package composes the same pieces in the same
//     order the daemon will, one layer below the scheduler. Every function
//     it calls is the product's; the loop around them is this file's.
//     See threetier_test.go's own comment for exactly which call is
//     standing in for which.
//   - MinIO cannot emulate an archive class. It accepts the storage-class
//     header and stores an ordinary object, and it implements no Glacier
//     restore. archiveboundary_test.go establishes both of those as facts
//     about the fixture, checked on every run rather than asserted in
//     prose, so the day either stops being true this suite says so instead
//     of quietly starting to certify something it never covered.
//
// # The invariant watcher is continuous
//
// FR-30's standing invariant, "a managed-complete artifact has at least
// one ACTIVE placement at a sufficient verification class, at every
// instant", is the property this whole EPIC is built to keep. Checking it
// before a move and again after one proves almost nothing: the interesting
// window is the middle, where a wrong ordering leaves both copies
// disposable for a few milliseconds and then tidies up after itself.
//
// So watcher_test.go does not sample. It decorates every operation that
// can change the invariant's truth value and evaluates it at each one:
// after every journal write the engine makes, and before every call that
// removes a copy. That set is complete, and the argument for why is the
// same one tests/movecrash makes: the invariant is a function of the
// durable journal and of which bytes exist, the journal only changes when
// something writes it, and bytes only stop existing when something deletes
// them. Time passing changes neither. Guarding every event that can
// falsify a property is a stronger claim than any polling interval, not a
// weaker one.
//
// That argument is only worth anything if the watcher has been watched to
// catch something a sampler would miss, so sampler_test.go plants a
// transient breach that opens and closes inside one move and runs both
// against it. The sampler passes. The watcher fails. That comparison, in
// the gate, is what "continuous" means here.
package conformance_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/tests/miniofixture"
)

// The scenario's fixed points. They are constants rather than literals
// scattered through the cells because the whole suite is one story and a
// cell that spelled a tier name differently would be testing a different
// chain.
const (
	scenarioSource = "production"
	scenarioSet    = "postgres-primary"

	// mediumOffsite is the monthly tier's home: an ordinary S3 bucket.
	mediumOffsite = "offsite_s3"

	// mediumDeepFreeze is the annual tier's home, configured for a cold
	// class. What that class does and does not mean against this fixture
	// is archiveboundary_test.go's subject.
	mediumDeepFreeze = "deep_freeze_s3"

	tierDaily   = "daily"
	tierMonthly = "monthly"
	tierAnnual  = "annual"
)

// scenarioNow is the clock the story starts on. Everything is dated
// relative to it, and it is a fixed date rather than time.Now so a run in
// January decides what a run in July decides.
var scenarioNow = time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)

// setID is the one backup set this scenario has.
var setID = model.BackupSetID{Source: scenarioSource, Set: scenarioSet}

// world is one composed scenario: a MinIO server with two buckets, a real
// journal, a real config, and a backup set's worth of artifacts on local
// disk.
type world struct {
	t   *testing.T
	ctx context.Context

	fixture *miniofixture.Fixture

	// dir is the scenario's own temp directory; root is the backup set's
	// local_path underneath it.
	dir  string
	root string

	journal *state.Journal
	cfg     *config.Config

	// offsite and deepFreeze are the two configured mediums, at the
	// transport boundary, exactly as internal/app would build them.
	offsite    transport.Medium
	deepFreeze transport.Medium

	// artifacts is every artifact this scenario seeded, oldest first,
	// with the bytes each one holds.
	artifacts []seeded
}

// seeded is one artifact the scenario planted, and what it holds.
type seeded struct {
	id model.ArtifactID
	// discoveredAt is the producer-side date retention buckets it by.
	discoveredAt time.Time
	content      []byte
	hash         string
}

// newWorld stands the scenario up: MinIO, two buckets, a validated config
// with the three-tier chain, a journal, and every artifact seeded to
// COMPLETE with its only copy on local disk.
//
// Every artifact starts local because that is where an artifact is when it
// has just been ingested, which is the state FR-27's home rule is supposed
// to move it out of. Seeding one already on a medium would be seeding the
// answer.
func newWorld(t *testing.T) *world {
	t.Helper()

	fixture := miniofixture.Start(t)
	offsiteBucket := fixture.NewBucket(t).Bucket
	deepBucket := fixture.NewBucket(t).Bucket

	dir := t.TempDir()
	root := filepath.Join(dir, "backups", scenarioSet)
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("creating the backup set's local_path: %v", err)
	}

	w := &world{
		t:       t,
		ctx:     context.Background(),
		fixture: fixture,
		dir:     dir,
		root:    root,
	}
	w.cfg = w.loadConfig(offsiteBucket, deepBucket)
	w.offsite = w.mediumFromConfig(mediumOffsite)
	w.deepFreeze = w.mediumFromConfig(mediumDeepFreeze)
	w.journal = w.openJournal()
	w.seedArtifacts()
	return w
}

// loadConfig writes the three-tier chain the phase 2 exit gate names as a
// real config.yaml and loads it through the product's own
// config.LoadAndValidate.
//
// A file rather than a struct literal, and LoadAndValidate rather than a
// hand-built Config, because this scenario's premise is that an operator
// can WRITE daily-local, monthly-s3, annual-cold and have it start. A
// chain this product refuses at load is not a scenario, it is a fiction,
// and the refusal has to show up here rather than in the middle of a move.
// Building the struct in Go would skip exactly the check that says the
// premise is real.
func (w *world) loadConfig(offsiteBucket, deepBucket string) *config.Config {
	w.t.Helper()

	yaml := fmt.Sprintf(`poll_interval: 15m

state:
  database: %[1]s/state.db

sources:
  - id: %[2]s
    backup_sets:
      - id: %[3]s
        remote:
          type: local
        remote_path: %[1]s/exports
        local_path: %[4]s
        completion:
          strategy: stable
          stable_for: 10m
        stale_after: 30h

storage_mediums:
  - id: %[5]s
    type: s3
    region: %[6]s
    endpoint: %[7]s
    bucket: %[8]s
    credentials:
      file: %[9]s
  - id: %[10]s
    type: s3
    region: %[6]s
    endpoint: %[7]s
    bucket: %[11]s
    storage_class: %[12]s
    credentials:
      file: %[9]s

retention:
  timezone: UTC
  week_starts_on: monday
  tiers:
    - name: %[13]s
      granularity: day
      keep: 7
    - name: %[14]s
      granularity: month
      keep: 12
      medium: %[5]s
    - name: %[15]s
      granularity: year
      keep: 5
      medium: %[10]s
`,
		w.dir, scenarioSource, scenarioSet, w.root,
		mediumOffsite, w.fixture.Region, w.fixture.Endpoint, offsiteBucket, w.fixture.CredentialsFile,
		mediumDeepFreeze, deepBucket, config.StorageClassGlacier,
		tierDaily, tierMonthly, tierAnnual)

	path := filepath.Join(w.dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		w.t.Fatalf("writing the scenario's config.yaml: %v", err)
	}
	cfg, err := config.LoadAndValidate(path)
	if err != nil {
		w.t.Fatalf("the three-tier chain the phase 2 exit gate names does not load: %v", err)
	}
	return cfg
}

// mediumFromConfig is internal/app's own translation from a configured
// medium to the transport boundary. Using it rather than hand-building a
// transport.Medium is what makes the storage class, the endpoint and the
// credential reference this scenario runs against the ones the daemon
// would build from the same file.
func (w *world) mediumFromConfig(id string) transport.Medium {
	w.t.Helper()
	medium, _, err := app.MediumFor(w.cfg, id)
	if err != nil {
		w.t.Fatalf("resolving medium %q out of the scenario's config: %v", id, err)
	}
	return medium
}

// chain is the retention chain, in chain order, which is the order
// FR-27's home rule reads.
func (w *world) chain() []config.RetentionTier { return w.cfg.Retention.Tiers }

// backupSet is the set config the move engine resolves a local path
// through.
func (w *world) backupSet() config.BackupSet {
	return config.BackupSet{Name: scenarioSet, ID: setID, LocalPath: w.root}
}

func (w *world) openJournal() *state.Journal {
	w.t.Helper()
	j, err := state.Open(w.ctx, filepath.Join(w.dir, "journal.db"))
	if err != nil {
		w.t.Fatalf("opening the journal: %v", err)
	}
	w.t.Cleanup(func() { _ = j.Close() })
	return j
}

// scenarioArtifacts is the cast, and the dates are chosen so that each
// tier of the chain owns a different one on day zero and so that advancing
// the clock moves an artifact from one tier to the next.
//
// Relative to scenarioNow (2026-09-04), the chain resolves to:
//
//	daily   day granularity,   keep 7  -> 2026-08-29 .. 2026-09-04
//	monthly month granularity, keep 12 -> 2025-10 .. 2026-09
//	annual  year granularity,  keep 5  -> 2022 .. 2026
//
// so:
//
//	fresh    2026-09-03  daily selects it            -> home local
//	summer   2026-07-15  monthly's July bucket       -> home offsite
//	stale    2026-07-01  older sibling in July       -> nothing selects it
//	ancient  2024-06-15  annual's 2024 bucket        -> home deep freeze
//
// stale is deliberately in the cast. An artifact no tier selects has no
// home, and FR-27 says such an artifact stays exactly where it is rather
// than being moved somewhere on its way to being deleted. A scenario in
// which every artifact has a home would never check that.
func scenarioArtifacts() []struct {
	name string
	at   time.Time
} {
	return []struct {
		name string
		at   time.Time
	}{
		{"2024-06-15T02-00-00Z.dump", time.Date(2024, 6, 15, 2, 0, 0, 0, time.UTC)},
		{"2026-07-01T02-00-00Z.dump", time.Date(2026, 7, 1, 2, 0, 0, 0, time.UTC)},
		{"2026-07-15T02-00-00Z.dump", time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)},
		{"2026-09-03T02-00-00Z.dump", time.Date(2026, 9, 3, 2, 0, 0, 0, time.UTC)},
	}
}

func (w *world) seedArtifacts() {
	w.t.Helper()
	for _, a := range scenarioArtifacts() {
		id := model.ArtifactID{Set: setID, Name: a.name}
		content := []byte(fmt.Sprintf("artifact %s: %s", a.name,
			"the durable bytes of one backup, long enough that a truncated copy is a different size"))
		w.artifacts = append(w.artifacts, seeded{
			id: id, discoveredAt: a.at, content: content, hash: sha256Hex(content),
		})
		w.seedOneOnLocal(id, content, a.at)
	}
}

// seedOneOnLocal walks a real artifact to COMPLETE through the real
// journal, leaving its only ACTIVE placement on local disk exactly where
// lifecycle.Commit leaves one.
func (w *world) seedOneOnLocal(id model.ArtifactID, content []byte, at time.Time) {
	w.t.Helper()

	localPath := filepath.Join(w.root, id.Name)
	if err := os.WriteFile(localPath, content, 0o600); err != nil {
		w.t.Fatalf("writing %s to the backup set's local_path: %v", id.Name, err)
	}

	size := int64(len(content))
	hash := sha256Hex(content)
	partial := localPath + ".partial"

	if _, err := w.journal.Discover(w.ctx, id, id.String()+":discover", "backups/"+id.Name,
		state.RemoteIdentity{Size: &size, Hash: hash, HashAlg: "sha256"}, at); err != nil {
		w.t.Fatalf("Discover %s: %v", id.Name, err)
	}
	verified := at
	for _, tr := range []state.Transition{
		{Artifact: id, Key: id.String() + ":transferring", From: "DISCOVERED", To: "TRANSFERRING", OccurredAt: at, LocalPath: &partial},
		{Artifact: id, Key: id.String() + ":transferred", From: "TRANSFERRING", To: "TRANSFERRED", OccurredAt: at,
			Transfer: &state.TransferResult{BytesTransferred: size, Checksummed: true}},
		{Artifact: id, Key: id.String() + ":verifying", From: "TRANSFERRED", To: "VERIFYING", OccurredAt: at},
		{Artifact: id, Key: id.String() + ":verified", From: "VERIFYING", To: "VERIFIED", OccurredAt: at,
			Hashes:     &state.HashUpdate{Hash: hash, Alg: "sha256"},
			Validation: &state.ValidationUpdate{Passed: true, Detail: "seeded"}},
		{Artifact: id, Key: id.String() + ":committing", From: "VERIFIED", To: "COMMITTING", OccurredAt: at},
		{Artifact: id, Key: id.String() + ":committed", From: "COMMITTING", To: "COMMITTED", OccurredAt: at, LocalPath: &localPath,
			Placement: &state.PlacementUpdate{Medium: state.MediumLocal, Location: localPath, Size: &size,
				Hash: hash, HashAlg: "sha256", VerificationClass: state.VerificationContent,
				VerifiedAt: &verified, Status: state.PlacementActive}},
		{Artifact: id, Key: id.String() + ":pending", From: "COMMITTED", To: "REMOTE_DELETE_PENDING", OccurredAt: at},
		{Artifact: id, Key: id.String() + ":complete", From: "REMOTE_DELETE_PENDING", To: "COMPLETE", OccurredAt: at},
	} {
		if _, err := w.journal.RecordTransition(w.ctx, tr); err != nil {
			w.t.Fatalf("%s -> %s: %v", id.Name, tr.To, err)
		}
	}
}

// records reads every artifact in the backup set out of the journal, which
// is what a retention pass is handed.
func (w *world) records() []state.Record {
	w.t.Helper()
	out := make([]state.Record, 0, len(w.artifacts))
	for _, a := range w.artifacts {
		rec, err := w.journal.Get(w.ctx, a.id)
		if err != nil {
			w.t.Fatalf("reading %s out of the journal: %v", a.id.Name, err)
		}
		out = append(out, rec)
	}
	return out
}

// ids is every seeded artifact's id, which is what the watcher watches.
func (w *world) ids() []model.ArtifactID {
	out := make([]model.ArtifactID, 0, len(w.artifacts))
	for _, a := range w.artifacts {
		out = append(out, a.id)
	}
	return out
}

// mediumByID resolves an id the way internal/app.MediumFor does, which is
// the translation the engine's resolver needs.
func (w *world) mediumByID(id string) (transport.Medium, bool) {
	switch id {
	case mediumOffsite:
		return w.offsite, true
	case mediumDeepFreeze:
		return w.deepFreeze, true
	}
	return transport.Medium{}, false
}

func (w *world) localPath(id model.ArtifactID) string { return filepath.Join(w.root, id.Name) }

func (w *world) localExists(id model.ArtifactID) bool {
	_, err := os.Lstat(w.localPath(id))
	return err == nil
}

// keyOn is the deterministic object key an artifact takes on a medium,
// computed the same way the engine computes it.
func (w *world) keyOn(medium transport.Medium, id model.ArtifactID) string {
	w.t.Helper()
	key, err := transport.MediumKey(medium.Prefix, id)
	if err != nil {
		w.t.Fatalf("computing the key for %s on %s: %v", id.Name, medium.ID, err)
	}
	return key
}

// --- small shared helpers ---------------------------------------------

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// placementOn returns an artifact's placement on one medium, whatever its
// status, and whether there is one at all.
func placementOn(rec state.Record, medium string) (state.Placement, bool) {
	for _, p := range rec.Placements {
		if p.Medium == medium {
			return p, true
		}
	}
	return state.Placement{}, false
}

// activeMediumOf is where the journal says an artifact's one durable copy
// is, for an assertion. It is deliberately NOT the planner's lookup: the
// planner uses internal/app.ActiveMediumFromRecords, which is the product's
// own, and a test that asserted with the same function it drove with would
// be checking that a function agrees with itself.
func activeMediumOf(rec state.Record) []string {
	var out []string
	for _, p := range rec.Placements {
		if p.Status == state.PlacementActive {
			out = append(out, p.Medium)
		}
	}
	return out
}

// describe renders an artifact's placements for a failure message.
func describe(rec state.Record) string {
	if len(rec.Placements) == 0 {
		return "no placements at all"
	}
	out := ""
	for i, p := range rec.Placements {
		if i > 0 {
			out += ", "
		}
		class := p.VerificationClass
		if class == "" {
			class = "unverified"
		}
		out += fmt.Sprintf("%s=%s/%s", p.Medium, p.Status, class)
	}
	return out
}

// sufficientClass is the standard this scenario holds the invariant to.
// Content, which is what FR-30 means by "read-back or better", and which
// is what both mediums here are configured for: neither declares
// upload_verification, so neither has opted into the weaker rung.
var sufficientClass = []placement.Class{placement.Content}

// writeFile is os.WriteFile at the mode this repository uses for anything
// derived from a backup artifact.
func writeFile(path string, body []byte) error { return os.WriteFile(path, body, 0o600) }

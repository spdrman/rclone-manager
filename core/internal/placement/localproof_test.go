package placement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file tests proveLocalSourceSafe directly, in-package, for the same
// reason internal/retention tests pruneVerifySafeToDelete directly: two of
// FR-20's checks defend against a state.Record the journal itself will not
// produce. scanRecord validates every artifact id it reads through
// model.NewArtifactID, so a name like "../secret.txt" cannot come back out
// of the database at all. That is a good thing and it is also why the
// containment proof cannot be reached through the engine: the attack it
// stops is a hand-edited row, schema drift, or a future scanRecord bug,
// and the honest way to test a defence against a bug that does not exist
// yet is to construct the record it would produce.
//
// TestTheSixChecksOfFR20AllFire below is the table that keeps this
// function honest as a whole: each of FR-20's six named checks gets a
// world in which exactly that check is the one that refuses.

var proofNow = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

// rawArtifact builds an ArtifactID without model.NewArtifactID's basename
// validation, which is what a corrupted or hand-edited journal row would
// deserialise into if the loader ever stopped checking.
func rawArtifact(set model.BackupSetID, name string) model.ArtifactID {
	return model.ArtifactID{Set: set, Name: name}
}

func proofEngine(t *testing.T, root string, set model.BackupSetID) *Engine {
	t.Helper()
	return &Engine{
		Sets: staticSets{set: config.BackupSet{Name: set.Set, ID: set, LocalPath: root}},
		Now:  func() time.Time { return proofNow },
	}
}

type staticSets struct{ set config.BackupSet }

func (s staticSets) Set(id model.BackupSetID) (config.BackupSet, error) {
	return s.set, nil
}

func proofRecord(artifact model.ArtifactID, path string, size int64) state.Record {
	return state.Record{
		Artifact:  artifact,
		State:     "COMPLETE",
		LocalPath: path,
		Placements: []state.Placement{{
			Medium: state.MediumLocal, Location: path, Size: &size,
			Hash: strings.Repeat("b", 64), HashAlg: "sha256",
			VerificationClass: state.VerificationContent, Status: state.PlacementDeletePending,
		}},
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// TestTheLocalProofRejectsPathTraversalViaACraftedArtifactName is the
// attack a naive filepath.Join produces: an artifact name carrying "..",
// whose computed containing directory is not the configured root at all.
func TestTheLocalProofRejectsPathTraversalViaACraftedArtifactName(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "setA")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	secret := filepath.Join(base, "secret.txt")
	writeFile(t, secret, "a sibling of the backup root, never a member of it")

	set := model.BackupSetID{Source: "s", Set: "setA"}
	artifact := rawArtifact(set, "../secret.txt")
	computed := filepath.Join(root, "../secret.txt")
	if computed != secret {
		t.Fatalf("this test's premise is wrong: %q is not %q", computed, secret)
	}

	e := proofEngine(t, root, set)
	path, err := e.proveLocalSourceSafe(proofRecord(artifact, computed, 49), state.Placement{
		Medium: state.MediumLocal, Location: computed, Status: state.PlacementDeletePending,
	})
	if err == nil {
		t.Fatalf("a traversal escape was approved for deletion at %q", path)
	}
	if !strings.Contains(err.Error(), "is not the canonical backup-set root") {
		t.Errorf("refused with %q; the containment check should be the one that fired", err)
	}
	if _, statErr := os.Stat(secret); statErr != nil {
		t.Fatalf("the file outside the root was touched: %v", statErr)
	}
}

// TestTheLocalProofRejectsASiblingDirectoryWhoseNameExtendsTheRoot is the
// classic prefix-confusion bug: ".../setA-evil" shares a prefix with
// ".../setA", so a strings.HasPrefix containment check accepts it. Exact
// equality of the two canonical directories has no notion of a prefix.
func TestTheLocalProofRejectsASiblingDirectoryWhoseNameExtendsTheRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "setA")
	evil := filepath.Join(base, "setA-evil")
	for _, d := range []string{root, evil} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	victim := filepath.Join(evil, "a.dump")
	writeFile(t, victim, "not this backup set's")

	set := model.BackupSetID{Source: "s", Set: "setA"}
	artifact := rawArtifact(set, "../setA-evil/a.dump")
	computed := filepath.Join(root, "../setA-evil/a.dump")

	e := proofEngine(t, root, set)
	if _, err := e.proveLocalSourceSafe(proofRecord(artifact, computed, 21), state.Placement{
		Medium: state.MediumLocal, Location: computed, Status: state.PlacementDeletePending,
	}); err == nil {
		t.Fatal("a sibling directory whose name extends the root was accepted as inside it")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("the sibling directory's file was touched: %v", err)
	}
}

// TestTheLocalProofAllowsABackupRootThatIsItselfASymlink keeps the two
// tests above from passing for the wrong reason. A NAS root that is a
// symlink to real storage is an ordinary deployment, and refusing it would
// make this whole gate a permanent no.
func TestTheLocalProofAllowsABackupRootThatIsItselfASymlink(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "storage")
	if err := os.Mkdir(real, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "backups")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	content := "the artifact"
	writeFile(t, filepath.Join(real, "a.dump"), content)

	set := model.BackupSetID{Source: "s", Set: "setA"}
	artifact := rawArtifact(set, "a.dump")
	computed := filepath.Join(link, "a.dump")

	e := proofEngine(t, link, set)
	path, err := e.proveLocalSourceSafe(proofRecord(artifact, computed, int64(len(content))), state.Placement{
		Medium: state.MediumLocal, Location: computed, Size: sizePtr(int64(len(content))),
		Status: state.PlacementDeletePending,
	})
	if err != nil {
		t.Fatalf("a symlinked backup root was refused: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(real, "a.dump"))
	if err != nil {
		t.Fatalf("resolving the expected path: %v", err)
	}
	if path != resolved {
		t.Errorf("the proof returned %q, want the resolved %q", path, resolved)
	}
}

func sizePtr(v int64) *int64 { return &v }

// TestTheSixChecksOfFR20AllFire is the completeness table. FR-20 asks for
// six things to hold before a local delete, and each row here breaks
// exactly one of them and names the refusal that must follow. A check that
// stopped being reachable, or that started being satisfied by something
// else, shows up here as a row that no longer fires.
func TestTheSixChecksOfFR20AllFire(t *testing.T) {
	set := model.BackupSetID{Source: "s", Set: "setA"}
	content := "the artifact's bytes"

	type world struct {
		name    string
		build   func(t *testing.T, root string) (model.ArtifactID, state.Placement, state.Record)
		wantErr string
	}

	worlds := []world{
		{
			name: "a final managed artifact, never a .partial",
			build: func(t *testing.T, root string) (model.ArtifactID, state.Placement, state.Record) {
				a := rawArtifact(set, "a.dump.partial")
				p := filepath.Join(root, a.Name)
				writeFile(t, p, content)
				rec := proofRecord(a, p, int64(len(content)))
				return a, state.Placement{Medium: state.MediumLocal, Location: p, Status: state.PlacementDeletePending}, rec
			},
			wantErr: "carries the .partial marker",
		},
		{
			name: "positively identified: the journal's path is the computed one",
			build: func(t *testing.T, root string) (model.ArtifactID, state.Placement, state.Record) {
				a := rawArtifact(set, "a.dump")
				p := filepath.Join(root, a.Name)
				writeFile(t, p, content)
				rec := proofRecord(a, p, int64(len(content)))
				return a, state.Placement{Medium: state.MediumLocal, Location: filepath.Join(root, "b.dump"), Status: state.PlacementDeletePending}, rec
			},
			wantErr: "refusing to guess which is correct",
		},
		{
			name: "never a symlink at the final path",
			build: func(t *testing.T, root string) (model.ArtifactID, state.Placement, state.Record) {
				a := rawArtifact(set, "a.dump")
				p := filepath.Join(root, a.Name)
				target := filepath.Join(root, "target.dump")
				writeFile(t, target, content)
				if err := os.Symlink(target, p); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				rec := proofRecord(a, p, int64(len(content)))
				return a, state.Placement{Medium: state.MediumLocal, Location: p, Status: state.PlacementDeletePending}, rec
			},
			wantErr: "is a symlink",
		},
		{
			name: "canonicalized and proven beneath the configured root",
			build: func(t *testing.T, root string) (model.ArtifactID, state.Placement, state.Record) {
				a := rawArtifact(set, "../outside.dump")
				p := filepath.Join(root, a.Name)
				writeFile(t, p, content)
				rec := proofRecord(a, p, int64(len(content)))
				return a, state.Placement{Medium: state.MediumLocal, Location: p, Status: state.PlacementDeletePending}, rec
			},
			wantErr: "is not the canonical backup-set root",
		},
		{
			name: "a real regular file, not a directory or a device",
			build: func(t *testing.T, root string) (model.ArtifactID, state.Placement, state.Record) {
				a := rawArtifact(set, "a.dump")
				p := filepath.Join(root, a.Name)
				if err := os.Mkdir(p, 0o750); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				rec := proofRecord(a, p, int64(len(content)))
				return a, state.Placement{Medium: state.MediumLocal, Location: p, Status: state.PlacementDeletePending}, rec
			},
			wantErr: "is not a regular file",
		},
		{
			name: "the bytes on disk are still the ones the journal measured",
			build: func(t *testing.T, root string) (model.ArtifactID, state.Placement, state.Record) {
				a := rawArtifact(set, "a.dump")
				p := filepath.Join(root, a.Name)
				writeFile(t, p, content+" and more")
				rec := proofRecord(a, p, int64(len(content)))
				return a, state.Placement{
					Medium: state.MediumLocal, Location: p, Size: sizePtr(int64(len(content))),
					Status: state.PlacementDeletePending,
				}, rec
			},
			wantErr: "and the placement records",
		},
		{
			name: "a configured, absolute backup-set root",
			build: func(t *testing.T, root string) (model.ArtifactID, state.Placement, state.Record) {
				a := rawArtifact(set, "a.dump")
				p := filepath.Join(root, a.Name)
				writeFile(t, p, content)
				rec := proofRecord(a, p, int64(len(content)))
				return a, state.Placement{Medium: state.MediumLocal, Location: p, Status: state.PlacementDeletePending}, rec
			},
			wantErr: "is not an absolute path",
		},
	}

	for _, w := range worlds {
		t.Run(w.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "setA")
			if err := os.Mkdir(root, 0o750); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			artifact, src, rec := w.build(t, root)
			_ = artifact

			configured := root
			if strings.Contains(w.wantErr, "absolute") {
				configured = "relative/root"
			}
			e := proofEngine(t, configured, set)

			path, err := e.proveLocalSourceSafe(rec, src)
			if err == nil {
				t.Fatalf("the proof approved %q for deletion; FR-20's %q check should have refused", path, w.name)
			}
			if !strings.Contains(err.Error(), w.wantErr) {
				t.Errorf("refused with %q, want a refusal containing %q", err, w.wantErr)
			}
		})
	}
}

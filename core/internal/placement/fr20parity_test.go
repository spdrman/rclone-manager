package placement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/retention"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// TestTheLocalSourceProofMatchesFR20 is the test proveLocalSourceSafe's own
// doc comment has been naming since it was written, and which did not
// exist until now.
//
// That is worth saying plainly, because of what the comment was doing with
// the name. FR-20's local-delete discipline is implemented twice in this
// repository, here and in internal/retention's pruneVerifySafeToDelete,
// and the comment justified the duplication by pointing at a test that
// "walks both and pins them together". There was no such test anywhere.
// Two implementations of a delete-safety proof were documented as being
// held in agreement by something nobody had written, which is worse than
// an undocumented duplication: it reads like the risk was handled.
//
// # Why the duplication stays
//
// Collapsing the two is the other available answer and it is the wrong
// one. The whole value of either function is that it re-derives every fact
// from ITS OWN caller's freshly re-read world immediately before the
// dangerous act, and the two callers do not have the same world: prune
// holds a GFS verdict and a backup set, the move engine holds a move
// journal and a placement row it has just re-read. A shared exported
// helper would have to take the union, and the argument for the redundancy
// (internal/lifecycle/remotedelete.go's package doc makes it for FR-15,
// and both of these files quote it) is that a safety check worth having is
// worth re-running at the point of the dangerous action rather than
// upstream of it.
//
// What the duplication is not allowed to do is DRIFT. So this is one table
// of worlds, each breaking exactly one of FR-20's checks, run through both
// implementations, with both required to refuse and to refuse about the
// same thing. A check quietly lost on either side turns one half of a row
// green while the other stays red, and the row fails.
//
// # Reaching the other implementation
//
// pruneVerifySafeToDelete is unexported and this test cannot be in two
// packages, so retention is driven through PruneDecide, which is the
// mandatory dry-run and calls it. That is the honest way round anyway: the
// rule has to hold on the surface an operator actually reads, not only on
// a function they cannot call.
//
// # The two divergences, asserted rather than assumed
//
// The two are not identical and must not be. A missing file is
// convergence to the move engine (the delete already happened; see
// errSourceAlreadyGone) and a refusal to prune (the journal and the disk
// disagree about a backup nobody is moving), and the move engine checks a
// recorded size that FR-20 never asked for. Both are argued in prose in
// both files, and both are pinned below in the direction that matters, so
// a change that accidentally aligns them shows up here rather than in a
// silent behaviour change on a delete path.

var fr20Now = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

// fr20Chain is a live one-tier chain whose whole window is fr20Now's own
// calendar day, so an artifact dated before that is a guaranteed delete
// candidate and DecideKeep's Keep flag depends on nothing this test did
// not put there. It is internal/retention's own pruneTodayOnlyChain,
// written out because that helper is unexported.
func fr20Chain() config.Retention {
	off := false
	return config.Retention{
		Timezone:     "UTC",
		WeekStartsOn: "monday",
		Tiers: []config.RetentionTier{
			{Name: "daily", Granularity: config.GranularityDay, Keep: 1},
		},
		ProtectLastKnownGood: &off,
	}
}

// fr20World is one world both proofs are asked about.
type fr20World struct {
	set      config.BackupSet
	rec      state.Record
	src      state.Placement
	artifact model.ArtifactID
}

// fr20Good is the well-formed world: an absolute root, a real regular file
// at the path the root and the artifact's name compute, and a journal that
// records exactly that path.
func fr20Good(t *testing.T) *fr20World {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "setA")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	set := model.BackupSetID{Source: "pg", Set: "setA"}
	artifact := rawArtifact(set, "a.dump")
	path := filepath.Join(root, artifact.Name)
	body := "the artifact's bytes"
	writeFile(t, path, body)
	size := int64(len(body))

	return &fr20World{
		set:      config.BackupSet{Name: set.Set, ID: set, LocalPath: root},
		artifact: artifact,
		rec: state.Record{
			Artifact: artifact, State: "COMPLETE", LocalPath: path,
			DiscoveredAt: fr20Now.Add(-48 * time.Hour),
			UpdatedAt:    fr20Now.Add(-48 * time.Hour),
			Placements: []state.Placement{{
				Medium: state.MediumLocal, Location: path, Size: &size,
				Hash: strings.Repeat("b", 64), HashAlg: "sha256",
				VerificationClass: state.VerificationContent,
				Status:            state.PlacementDeletePending,
			}},
		},
		src: state.Placement{
			Medium: state.MediumLocal, Location: path, Size: &size,
			Status: state.PlacementDeletePending,
		},
	}
}

// root is the directory the good world put the artifact in, which several
// rows need in order to build a decoy beside it.
func (w *fr20World) root() string { return w.set.LocalPath }

// retarget points every path the two proofs read at p, so a row breaks one
// FR-20 rule rather than accidentally breaking the identity check as well.
func (w *fr20World) retarget(p string) {
	w.rec.LocalPath = p
	w.rec.Placements[0].Location = p
	w.src.Location = p
}

// rename changes the artifact's name everywhere, which is how the crafted
// name rows work: both proofs COMPUTE the path from the name, so the name
// is the input under test.
func (w *fr20World) rename(t *testing.T, name string) {
	t.Helper()
	w.artifact = rawArtifact(w.artifact.Set, name)
	w.rec.Artifact = w.artifact
	w.retarget(filepath.Join(w.root(), name))
}

// askPlacement runs the move engine's proof.
func (w *fr20World) askPlacement() (string, error) {
	e := &Engine{
		Sets: staticSets{set: w.set},
		Now:  func() time.Time { return fr20Now },
	}
	return e.proveLocalSourceSafe(w.rec, w.src)
}

// askRetention runs FR-20's prune through the surface an operator reads.
func (w *fr20World) askRetention(t *testing.T) retention.PruneVerdict {
	t.Helper()
	verdicts, err := retention.PruneDecide(fr20Now, fr20Chain(), w.set, []state.Record{w.rec}, retention.AllLocal)
	if err != nil {
		t.Fatalf("PruneDecide could not run at all: %v", err)
	}
	if len(verdicts) != 1 {
		t.Fatalf("PruneDecide returned %d verdicts for one record: %+v", len(verdicts), verdicts)
	}
	return verdicts[0]
}

// fr20Rule is one of FR-20's checks, with the phrase each implementation
// refuses in. The two phrasings differ, and that is fine: what has to
// match is that both refuse, in the same world, about the same rule.
type fr20Rule struct {
	rule          string
	build         func(t *testing.T, w *fr20World)
	wantPlacement string
	wantRetention string
}

var fr20Rules = []fr20Rule{
	{
		rule:          "the backup set has a configured local_path",
		build:         func(_ *testing.T, w *fr20World) { w.set.LocalPath = "" },
		wantPlacement: "has no configured local_path",
		// Prune refuses one step earlier, in pruneEvaluate, because
		// artifactstore.NewLocal will not resolve an empty root under the
		// process working directory. Different sentence, same rule, and
		// both name local_path.
		wantRetention: "local_path as its root",
	},
	{
		rule:          "the configured local_path is absolute",
		build:         func(_ *testing.T, w *fr20World) { w.set.LocalPath = "relative/root" },
		wantPlacement: "is not an absolute path",
		wantRetention: "must be an absolute path",
	},
	{
		rule: "a final managed artifact, never a .partial",
		build: func(t *testing.T, w *fr20World) {
			w.rename(t, "a.dump.partial")
			writeFile(t, filepath.Join(w.root(), "a.dump.partial"), "in flight")
		},
		wantPlacement: "carries the .partial marker",
		wantRetention: "carries the .partial marker",
	},
	{
		rule: "positively identified: the journal's path is the computed one",
		build: func(t *testing.T, w *fr20World) {
			w.retarget(filepath.Join(w.root(), "b.dump"))
			writeFile(t, filepath.Join(w.root(), "b.dump"), "somebody else's")
		},
		wantPlacement: "refusing to guess which is correct",
		wantRetention: "refusing to guess which is correct",
	},
	{
		rule: "never a symlink at the final path",
		build: func(t *testing.T, w *fr20World) {
			path := w.rec.LocalPath
			target := filepath.Join(w.root(), "target.dump")
			writeFile(t, target, "the decoy")
			if err := os.Remove(path); err != nil {
				t.Fatalf("removing the real file: %v", err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatalf("planting the symlink: %v", err)
			}
		},
		wantPlacement: "is a symlink",
		wantRetention: "is a symlink",
	},
	{
		rule: "a real regular file, not a directory or a device",
		build: func(t *testing.T, w *fr20World) {
			path := w.rec.LocalPath
			if err := os.Remove(path); err != nil {
				t.Fatalf("removing the real file: %v", err)
			}
			if err := os.Mkdir(path, 0o750); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		},
		wantPlacement: "is not a regular file",
		wantRetention: "is not a regular file",
	},
	{
		rule: "canonicalized and proven beneath the configured root",
		build: func(t *testing.T, w *fr20World) {
			// A crafted name whose computed directory is the root's
			// PARENT, which is the escape a naive filepath.Join produces.
			outside := filepath.Join(filepath.Dir(w.root()), "secret.txt")
			writeFile(t, outside, "a sibling of the backup root, never a member of it")
			w.rename(t, "../secret.txt")
		},
		wantPlacement: "is not the canonical backup-set root",
		wantRetention: "is not the canonical backup-set root",
	},
	{
		rule: "a sibling directory whose name merely extends the root's is outside it",
		build: func(t *testing.T, w *fr20World) {
			evil := w.root() + "-evil"
			if err := os.Mkdir(evil, 0o750); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			writeFile(t, filepath.Join(evil, "a.dump"), "not this backup set's")
			w.rename(t, "../"+filepath.Base(evil)+"/a.dump")
		},
		wantPlacement: "is not the canonical backup-set root",
		wantRetention: "is not the canonical backup-set root",
	},
}

func TestTheLocalSourceProofMatchesFR20(t *testing.T) {
	// The negative control. Every row below is an argument that two
	// refusals agree, and two refusals in a world both implementations
	// refuse for some unrelated reason would agree just as loudly.
	t.Run("0 the well-formed world, which both implementations approve", func(t *testing.T) {
		w := fr20Good(t)

		path, err := w.askPlacement()
		if err != nil {
			t.Fatalf("the move engine's proof refused a well-formed world: %v", err)
		}
		if path == "" {
			t.Error("the move engine's proof approved the delete and named no path")
		}

		v := w.askRetention(t)
		if v.Action != retention.PruneDelete {
			t.Fatalf("FR-20's prune said %s about a well-formed world: %s", v.Action, v.Reason)
		}
		if _, statErr := os.Lstat(w.rec.LocalPath); statErr != nil {
			t.Errorf("PruneDecide is a dry run and the file is gone: %v", statErr)
		}
	})

	for _, r := range fr20Rules {
		t.Run(r.rule, func(t *testing.T) {
			// Two worlds rather than one, because both proofs read the
			// filesystem and one of them is allowed to be run first only
			// if that cannot matter. Building each its own copy removes
			// the question.
			pw := fr20Good(t)
			r.build(t, pw)
			path, err := pw.askPlacement()
			if err == nil {
				t.Errorf("the move engine's proof APPROVED deleting %q in a world where %q is broken", path, r.rule)
			} else if !strings.Contains(err.Error(), r.wantPlacement) {
				t.Errorf("the move engine refused with %q, want a refusal containing %q", err, r.wantPlacement)
			}

			rw := fr20Good(t)
			r.build(t, rw)
			v := rw.askRetention(t)
			if v.Action != retention.PruneRefuse {
				t.Errorf("FR-20's prune said %s in a world where %q is broken; the two implementations of this rule have drifted, and the one that stopped checking deletes a backup", v.Action, r.rule)
			} else if !strings.Contains(v.Reason, r.wantRetention) {
				t.Errorf("FR-20's prune refused with %q, want a refusal containing %q", v.Reason, r.wantRetention)
			}
		})
	}
}

// TestTheTwoFR20ProofsDivergeOnlyWhereTheyArgueTheyDo pins the two places
// the implementations deliberately disagree.
//
// They are asserted rather than left to the prose because a divergence
// nobody tests is indistinguishable from a bug, in either direction: the
// move engine converging on a missing file is what stops #372's crash
// leaving a move stuck at SOURCE_DELETE_PENDING for ever, and prune
// refusing on one is what stops a journal that has lost touch with the
// disk quietly counting as work done.
func TestTheTwoFR20ProofsDivergeOnlyWhereTheyArgueTheyDo(t *testing.T) {
	t.Run("a source that is already gone: convergence for a move, a refusal for a prune", func(t *testing.T) {
		pw := fr20Good(t)
		if err := os.Remove(pw.rec.LocalPath); err != nil {
			t.Fatalf("removing the file: %v", err)
		}
		if _, err := pw.askPlacement(); err != errSourceAlreadyGone {
			t.Errorf("the move engine answered %v for a source that is already gone, want errSourceAlreadyGone; a refusal here leaves the move at SOURCE_DELETE_PENDING for ever with a row that says DELETE_PENDING about nothing", err)
		}

		rw := fr20Good(t)
		if err := os.Remove(rw.rec.LocalPath); err != nil {
			t.Fatalf("removing the file: %v", err)
		}
		v := rw.askRetention(t)
		if v.Action != retention.PruneRefuse {
			t.Errorf("FR-20's prune said %s about a file that is not there; nothing was deleted, so nothing may be reported as deleted", v.Action)
		}
	})

	t.Run("the recorded size: the move engine's check, and not FR-20's", func(t *testing.T) {
		pw := fr20Good(t)
		writeFile(t, pw.rec.LocalPath, "the artifact's bytes, and then some more of them")
		if _, err := pw.askPlacement(); err == nil {
			t.Error("the move engine approved a source whose size no longer matches the placement; that is FR-16's identity idea applied to the local end, and the destination was verified against the OLD bytes")
		} else if !strings.Contains(err.Error(), "and the placement records") {
			t.Errorf("the move engine refused with %q, want the size refusal", err)
		}

		rw := fr20Good(t)
		writeFile(t, rw.rec.LocalPath, "the artifact's bytes, and then some more of them")
		v := rw.askRetention(t)
		if v.Action != retention.PruneDelete {
			t.Errorf("FR-20's prune said %s about a file whose size changed: %s. FR-20 lists six checks and a recorded size is not one of them, and prune has no placement row to compare against anyway (FR-32); if this rule is wanted there it has to be argued and added, not arrive by drift",
				v.Action, v.Reason)
		}
	})
}

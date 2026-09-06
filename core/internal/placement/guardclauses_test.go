package placement

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/archive"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is guardSourceDelete's completeness table, and it exists
// because an adversarial review deleted clauses out of the guard and
// watched the suite stay green.
//
// sourcedelete_test.go drives the real engine, which is the right way to
// prove the guard is REACHED and that a refusal really leaves the source
// on disk. It is the wrong way to prove every clause is LOAD-BEARING,
// for the same reason localproof_test.go gives about FR-20's containment
// proof: several of these clauses defend against a journal the engine
// itself will not produce. The phase clause is the clearest case. Nothing
// can reach a source delete from a phase other than SOURCE_DELETE_PENDING,
// because phases.go's table forbids it and state.AdvanceMove's WHERE
// clause forbids it again, so a test that drives the engine can never
// make that clause fire, and deleting it left every test in this package
// passing.
//
// So this calls the guard directly, with a hand-built move and record, and
// gives every refusal in it a world where exactly that refusal is the one
// that answers. TestTheGuardTableCoversEveryRefusal then counts the
// refusals in the source and fails if the table has fewer rows, so the
// next clause added without a test fails the build instead of shipping
// unproven.
//
// Measured before it was written: deleting the phase clause, the
// destination's verified-at and hash clauses, or the source's
// no-placement and no-location clauses left `go test ./internal/...`
// entirely green.

// guardWorld is one call to guardSourceDelete: a filesystem, a move, a
// record and an engine, all correct, with one thing broken.
type guardWorld struct {
	root     string
	artifact model.ArtifactID
	move     state.Move
	rec      state.Record
	engine   *Engine
	store    *guardStore
}

const (
	guardSrcMedium = "s3-src"
	guardDstMedium = "s3-dst"
	guardKey       = "production/pg/a.dump"
	guardHash      = "5f0e2c1b7a3d4e6f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7"
)

var guardNow = time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)

// newGuardWorld builds the world in which guardSourceDelete APPROVES the
// delete: a local source in DELETE_PENDING under the configured root, a
// destination on a medium that is ACTIVE, content-verified, at the key the
// move recorded, with the same hash and a verification time, no tier
// wanting the source, and a destination class that reads on demand.
//
// Every row below starts here and breaks one thing. Without that, a row
// could be passing because the world was broken in some second way the
// test never named.
func newGuardWorld(t *testing.T) *guardWorld {
	t.Helper()

	root := t.TempDir()
	set := model.BackupSetID{Source: "pg", Set: "setA"}
	artifact := rawArtifact(set, "a.dump")
	body := "the artifact's bytes"
	local := filepath.Join(root, artifact.Name)
	writeFile(t, local, body)
	size := int64(len(body))
	verified := guardNow.Add(-time.Minute)

	w := &guardWorld{
		root:     root,
		artifact: artifact,
		move: state.Move{
			ID: 1, Artifact: artifact, Phase: state.MoveSourceDeletePending,
			SourceMedium: state.MediumLocal, DestinationMedium: guardDstMedium,
			DestinationKey: guardKey,
		},
		rec: state.Record{
			Artifact: artifact, State: "COMPLETE", LocalPath: local,
			Placements: []state.Placement{
				{
					Medium: state.MediumLocal, Location: local, Size: &size,
					Hash: guardHash, HashAlg: "sha256",
					VerificationClass: state.VerificationContent,
					Status:            state.PlacementDeletePending,
				},
				{
					Medium: guardDstMedium, Location: guardKey, Size: &size,
					Hash: guardHash, HashAlg: "sha256",
					VerificationClass: state.VerificationContent, VerifiedAt: &verified,
					Status: state.PlacementActive,
				},
			},
		},
		store: &guardStore{t: t, size: size},
	}
	w.engine = &Engine{
		Store:   w.store,
		Mediums: guardMediums{class: map[string]string{guardDstMedium: config.StorageClassStandard, guardSrcMedium: config.StorageClassStandard}},
		Sets:    staticSets{set: config.BackupSet{Name: set.Set, ID: set, LocalPath: root}},
		Tiers:   &guardTiers{},
		Now:     func() time.Time { return guardNow },
	}
	return w
}

// localPlacement and mediumPlacement address the two rows a row-mutating
// world edits, by medium rather than by index, so reordering the fixture
// cannot silently point a row at the wrong copy.
func (w *guardWorld) placement(t *testing.T, medium string) *state.Placement {
	t.Helper()
	for i := range w.rec.Placements {
		if w.rec.Placements[i].Medium == medium {
			return &w.rec.Placements[i]
		}
	}
	t.Fatalf("the world has no placement on %q", medium)
	return nil
}

func (w *guardWorld) dropPlacement(t *testing.T, medium string) {
	t.Helper()
	var kept []state.Placement
	for _, p := range w.rec.Placements {
		if p.Medium != medium {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(w.rec.Placements) {
		t.Fatalf("the world has no placement on %q to drop", medium)
	}
	w.rec.Placements = kept
}

// onMediumSource moves the source end onto a medium, so the guard takes
// proveMediumSourceSafe's branch instead of proveLocalSourceSafe's.
func (w *guardWorld) onMediumSource(t *testing.T) {
	t.Helper()
	src := w.placement(t, state.MediumLocal)
	src.Medium = guardSrcMedium
	src.Location = "archive/pg/a.dump"
	w.move.SourceMedium = guardSrcMedium
}

func (w *guardWorld) run() (deleteTarget, error) {
	return w.engine.guardSourceDelete(context.Background(), w.move, w.rec, Content)
}

type guardTiers struct {
	selected bool
	why      string
	err      error
	asked    int
}

func (g *guardTiers) SourceStillSelected(_ context.Context, _ state.Record, _ string) (bool, string, error) {
	g.asked++
	return g.selected, g.why, g.err
}

type guardMediums struct {
	class map[string]string
	err   error
}

func (m guardMediums) Resolve(id string) (transport.Medium, Class, error) {
	if m.err != nil {
		return transport.Medium{}, "", m.err
	}
	class, ok := m.class[id]
	if !ok {
		return transport.Medium{}, "", fmt.Errorf("no medium %q is configured", id)
	}
	return transport.Medium{ID: id, StorageClass: class}, Content, nil
}

// guardStore is the medium boundary. DeleteObject panics: nothing this
// file does may ever reach a delete, and a panic says so louder than a
// counter nobody asserts on.
type guardStore struct {
	t          *testing.T
	size       int64
	statErr    error
	restore    *transport.RestoreState
	restoreErr error
}

func (s *guardStore) StatObject(_ context.Context, _ transport.Medium, _ string) (transport.ObjectInfo, error) {
	if s.statErr != nil {
		return transport.ObjectInfo{}, s.statErr
	}
	return transport.ObjectInfo{Size: s.size}, nil
}

func (s *guardStore) RestoreStatus(_ context.Context, _ transport.Medium, _ string) (*transport.RestoreState, error) {
	if s.restoreErr != nil {
		return nil, s.restoreErr
	}
	return s.restore, nil
}

func (s *guardStore) DeleteObject(_ context.Context, _ transport.Medium, key string) error {
	s.t.Fatalf("the guard reached a delete of %q; nothing in this file approves one", key)
	return nil
}

func (s *guardStore) UploadFromLocal(context.Context, transport.Medium, string, string, transport.UploadOptions) (transport.UploadResult, error) {
	return transport.UploadResult{}, errors.New("not used by the guard")
}

func (s *guardStore) OpenObject(context.Context, transport.Medium, string) (io.ReadCloser, error) {
	return nil, errors.New("not used by the guard")
}

func (s *guardStore) ObjectChecksum(context.Context, transport.Medium, string, transport.HashAlgorithm) (transport.ChecksumAttestation, error) {
	return transport.ChecksumAttestation{}, errors.New("not used by the guard")
}

// guardClause is one row: what it breaks, and the phrase the refusal must
// carry.
type guardClause struct {
	name    string
	break_  func(t *testing.T, w *guardWorld)
	wantErr string
}

// guardClauses is the table. Every refusal guardSourceDelete can return
// has a row, and TestTheGuardTableCoversEveryRefusal counts the source to
// prove it.
var guardClauses = []guardClause{
	{
		name:    "1 the phase, which only SOURCE_DELETE_PENDING satisfies",
		break_:  func(_ *testing.T, w *guardWorld) { w.move.Phase = state.MoveVerified },
		wantErr: "only SOURCE_DELETE_PENDING authorises a source delete",
	},
	{
		name: "1 the two ends of the move are two places",
		break_: func(t *testing.T, w *guardWorld) {
			w.onMediumSource(t)
			w.move.DestinationMedium = guardSrcMedium
		},
		wantErr: "at both ends",
	},
	{
		name:    "2 the artifact is still COMPLETE",
		break_:  func(_ *testing.T, w *guardWorld) { w.rec.State = "QUARANTINED_LOST" },
		wantErr: "only COMPLETE artifacts may move",
	},
	{
		name:    "3 the journal records a destination placement at all",
		break_:  func(t *testing.T, w *guardWorld) { w.dropPlacement(t, guardDstMedium) },
		wantErr: "records no placement on the destination",
	},
	{
		name: "3 the destination placement is ACTIVE",
		break_: func(t *testing.T, w *guardWorld) {
			w.placement(t, guardDstMedium).Status = state.PlacementDeletePending
		},
		wantErr: "is DELETE_PENDING, not ACTIVE",
	},
	{
		name: "3 the destination placement records the key this move copied to",
		break_: func(t *testing.T, w *guardWorld) {
			w.placement(t, guardDstMedium).Location = guardKey + ".other"
		},
		wantErr: "refusing to guess which is the copy",
	},
	{
		name: "3 the destination was verified at the class this medium requires",
		break_: func(t *testing.T, w *guardWorld) {
			w.placement(t, guardDstMedium).VerificationClass = state.VerificationExistence
		},
		wantErr: `records verification class "existence"`,
	},
	{
		name: "3 the destination records WHEN it was verified",
		break_: func(t *testing.T, w *guardWorld) {
			w.placement(t, guardDstMedium).VerifiedAt = nil
		},
		wantErr: "records no verification time",
	},
	{
		name: "3 the destination records a hash",
		break_: func(t *testing.T, w *guardWorld) {
			dst := w.placement(t, guardDstMedium)
			dst.Hash = ""
			// The source's hash goes too, so the hash-agreement clause
			// below cannot be the one that answers: two empty strings are
			// equal, and this row is about the emptiness, not the
			// disagreement.
			w.placement(t, state.MediumLocal).Hash = ""
		},
		wantErr: "records no hash",
	},
	{
		name:    "4 the journal still records a placement on the source medium",
		break_:  func(t *testing.T, w *guardWorld) { w.dropPlacement(t, state.MediumLocal) },
		wantErr: "records no placement there any more",
	},
	{
		name: "4 the source placement carries the recorded intent to delete",
		break_: func(t *testing.T, w *guardWorld) {
			w.placement(t, state.MediumLocal).Status = state.PlacementActive
		},
		wantErr: "a delete is only ever issued against DELETE_PENDING",
	},
	{
		name: "4 the source placement records a location",
		break_: func(t *testing.T, w *guardWorld) {
			w.placement(t, state.MediumLocal).Location = ""
		},
		wantErr: "records no location",
	},
	{
		name: "5 the two copies are the same bytes",
		break_: func(t *testing.T, w *guardWorld) {
			w.placement(t, guardDstMedium).Hash = strings.Repeat("a", 64)
		},
		wantErr: "so they are not the same bytes",
	},
	{
		name:    "6 a retention-tier guard is configured at all",
		break_:  func(_ *testing.T, w *guardWorld) { w.engine.Tiers = nil },
		wantErr: "no retention-tier guard is configured",
	},
	{
		name: "6 the tier guard could answer",
		break_: func(_ *testing.T, w *guardWorld) {
			w.engine.Tiers = &guardTiers{err: errors.New("the retention chain could not be evaluated")}
		},
		wantErr: "asking whether a tier still selects it",
	},
	{
		name: "6 no tier still selects the source medium",
		break_: func(_ *testing.T, w *guardWorld) {
			w.engine.Tiers = &guardTiers{selected: true, why: "the daily tier is still local"}
		},
		wantErr: "a retention tier still selects it",
	},
	{
		name: "7 the medium source has a store to be asked through",
		break_: func(t *testing.T, w *guardWorld) {
			w.onMediumSource(t)
			w.engine.Store = nil
		},
		wantErr: "no medium store is configured",
	},
	{
		name: "7 the source medium is one the configuration still declares",
		break_: func(t *testing.T, w *guardWorld) {
			w.onMediumSource(t)
			w.engine.Mediums = guardMediums{class: map[string]string{guardDstMedium: config.StorageClassStandard}}
		},
		wantErr: "no medium \"s3-src\" is configured",
	},
	{
		name: "7 the medium answered, rather than could not be reached",
		break_: func(t *testing.T, w *guardWorld) {
			w.onMediumSource(t)
			w.store.statErr = transport.NewError(transport.Transient, "stat", errors.New("the endpoint reset the connection"))
		},
		wantErr: "the medium could not be asked about",
	},
	{
		name: "7 the object on the medium is the size the journal recorded",
		break_: func(t *testing.T, w *guardWorld) {
			w.onMediumSource(t)
			w.store.size = 9999
		},
		wantErr: "and the placement records",
	},
	{
		name: "8 every copy is on a class this build can reason about",
		break_: func(_ *testing.T, w *guardWorld) {
			w.engine.Mediums = guardMediums{class: map[string]string{guardDstMedium: "GLACIER_DEEP_MAYBE"}}
		},
		wantErr: "which this build does not recognise",
	},
	{
		name: "8 some surviving copy can actually be read right now",
		break_: func(_ *testing.T, w *guardWorld) {
			w.engine.Mediums = guardMediums{class: map[string]string{guardDstMedium: config.StorageClassDeepArchive}}
		},
		wantErr: "verified and retrievable right now",
	},
}

// TestEveryClauseOfTheSourceDeleteGuardRefuses runs the table.
func TestEveryClauseOfTheSourceDeleteGuardRefuses(t *testing.T) {
	// The negative control first. Without it, every row below could be
	// refusing for a reason the world had all along.
	t.Run("0 the world every row starts from is one the guard approves", func(t *testing.T) {
		w := newGuardWorld(t)
		target, err := w.run()
		if err != nil {
			t.Fatalf("the unbroken world was refused with %q; every row below would then be proving nothing", err)
		}
		if target.localPath == "" {
			t.Errorf("the guard approved the delete and named %+v, which is not the local source copy", target)
		}
	})

	for _, c := range guardClauses {
		t.Run(c.name, func(t *testing.T) {
			w := newGuardWorld(t)
			c.break_(t, w)

			target, err := w.run()
			if err == nil {
				t.Fatalf("the guard APPROVED deleting %+v in a world where %q is broken", target, c.name)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("refused with %q, want a refusal containing %q; some other clause answered first, so this one is still unproven", err, c.wantErr)
			}
			if _, statErr := os.Lstat(filepath.Join(w.root, w.artifact.Name)); statErr != nil {
				t.Errorf("the source copy is gone after a refusal: %v", statErr)
			}
		})
	}
}

// TestTheGuardTableCoversEveryRefusal is what keeps the table honest
// against the next clause.
//
// It counts the refusals guardSourceDelete can return, in the source, and
// requires the table above to have at least that many rows. A clause added
// without a row fails here rather than shipping as a line nobody has ever
// watched fire, which is exactly how the five untested ones got in.
//
// It counts refusal SITES rather than matching messages, because a rule
// that matched on message text would be a rule about wording and would go
// green again the moment somebody rephrased one.
func TestTheGuardTableCoversEveryRefusal(t *testing.T) {
	files := packageFiles(t)
	fn := engineMethod(files, "guardSourceDelete")
	if fn == nil {
		t.Fatal("no (*Engine).guardSourceDelete was found, so this test proved nothing")
	}

	sites := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "refuse" {
			sites++
		}
		return true
	})
	if sites == 0 {
		t.Fatal("no refusal was found in guardSourceDelete at all, which is a much worse bug than the one this test looks for")
	}

	// The medium-specific proof refuses in its own function, and its
	// refusals reach the caller unwrapped, so the table's clause-7 rows
	// are about those. They are counted separately for the same reason.
	medium := engineMethod(files, "proveMediumSourceSafe")
	if medium == nil {
		t.Fatal("no (*Engine).proveMediumSourceSafe was found")
	}
	ast.Inspect(medium.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "refuse" {
			sites++
		}
		return true
	})

	if len(guardClauses) < sites {
		t.Errorf("guardSourceDelete and proveMediumSourceSafe hold %d refusals and this table has %d rows; a clause with no row is a clause nobody has watched fire, and this guard is the one immediately before deleting the last local copy of somebody's backup",
			sites, len(guardClauses))
	}
}

// unusedGuardTypes keeps the archive import honest: the table's last row
// is about archive's own refusal, and naming the sentinel here makes a
// rename of it a compile error rather than a row that quietly stops
// matching.
var _ = archive.ErrNoRetrievableCopy

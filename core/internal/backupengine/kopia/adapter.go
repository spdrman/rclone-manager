// Package kopia is the only package in this repository that imports Kopia.
//
// It implements backupengine.Engine and backupengine.Repository over the
// embedded Kopia Go API, in process. There is no `kopia` binary, no PATH
// lookup and no subprocess: lifecycle_test.go's TestNoSubprocess reads these
// sources and fails if anything here so much as names os/exec, because "we
// embed it" is a claim that decays into "we shell out to it" one convenient
// shortcut at a time.
//
// Two storage backends are registered, filesystem and native s3, and that
// is a decision rather than an import line, the same way
// internal/transport/rclone treats its backend set. Every additional
// backend is a dependency, an authentication surface and a failure mode.
// A repository on a mounted NAS share is a filesystem repository, which is
// the case this product started with; a repository in a bucket is the
// case #781 added, natively, without the rclone-backed provider that
// would have made the whole list available for one import line. The
// argument for that refusal is in repository.go, which owns everything
// about a repository's storage and lifecycle; this file owns what happens
// inside an open one.
//
// The Kopia version is pinned in core/go.mod and bumping it is a project
// event, not a background update. See
// docs/adr/0006-embed-kopia-behind-backupengine-adapter.md and
// docs/adr/0011-kopia-repository-adapter.md.
package kopia

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/restore"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/kopia/kopia/snapshot/snapshotmaintenance"
	"github.com/kopia/kopia/snapshot/upload"

	"github.com/backupdproject/backupd/core/internal/backupengine"
)

// verifyErrorBudget caps how many per-object findings one Verify collects
// before it stops walking.
//
// Kopia's tree walker treats this as "abort after N errors", and its default
// is 1, which turns a verification report into "the first thing that is
// wrong". For a tool whose operator question is "how bad is it", the first
// error is the least useful possible answer, so this is deliberately high
// enough to describe real damage and still bounded, because a repository that
// is wrong in ten thousand places does not need the ten thousand and first
// finding to justify a restore from elsewhere.
const verifyErrorBudget = 1000

// Adapter implements backupengine.Engine over embedded Kopia packages.
//
// It holds no repository state, and like the rclone adapter that is
// load-bearing: every repository handle it hands out owns its own Kopia
// repository object and its own config file path, so two backupd
// operations against two repositories share no cached connection, no
// cached format blob and no cached credential.
//
// The one field is a clock, which exists because the clock-skew health
// check cannot be tested against a clock that agrees. See WithClock.
type Adapter struct {
	now func() time.Time
}

// New returns an adapter. It takes no Kopia types, by design.
func New(opts ...Option) *Adapter {
	a := &Adapter{now: time.Now}

	for _, opt := range opts {
		opt(a)
	}

	return a
}

// The compile-time assertions that this package really satisfies the
// boundary. Production wiring passes kopia.New() to a constructor that
// accepts the interface, so a signature that drifted would only fail at that
// one call site, which is a worse place to find out than here.
var (
	_ backupengine.Engine     = (*Adapter)(nil)
	_ backupengine.Repository = (*repository)(nil)
)

// repository is one open Kopia repository behind backupengine.Repository.
type repository struct {
	rep    repo.Repository
	direct repo.DirectRepository

	// loc is how this handle was opened, kept so that Health can reach
	// the storage again without being handed the location a second time.
	//
	// It holds no secret material. Both credentials on it are references
	// (secretref.Ref), which is what makes keeping it for the life of the
	// handle a safe thing to do rather than a custody decision.
	loc backupengine.RepositoryLocation

	// adapter is the engine that opened this handle, for its clock and its
	// storage dispatch.
	adapter *Adapter

	closeOnce sync.Once
	closeErr  error
}

// Snapshot implements backupengine.Repository.
//
// Incrementality happens here and nothing asked for it: FindPreviousManifests
// looks up what this repository already holds for this exact source, and the
// uploader reuses any entry whose name, size, mtime, mode and owner still
// match. Note what that list does not include: content. A file rewritten in
// place with its size and mtime preserved is reused unread, so this is only
// as trustworthy as the source's metadata, which is issue #793's whole
// subject and not something this adapter can fix from below.
func (r *repository) Snapshot(ctx context.Context, req backupengine.SnapshotRequest) (backupengine.SnapshotInfo, error) {
	si, err := sourceInfo(req.Source)
	if err != nil {
		return backupengine.SnapshotInfo{}, err
	}

	sourceEntry, err := localfs.NewEntry(si.Path)
	if err != nil {
		return backupengine.SnapshotInfo{}, fmt.Errorf("opening source %s: %w", si.Path, err)
	}

	policyTree, err := policy.TreeForSource(ctx, r.rep, si)
	if err != nil {
		return backupengine.SnapshotInfo{}, fmt.Errorf("resolving policy for %s: %w", si.Path, err)
	}

	previous, err := snapshot.FindPreviousManifests(ctx, r.rep, si, nil)
	if err != nil {
		return backupengine.SnapshotInfo{}, fmt.Errorf("finding previous snapshots of %s: %w", si.Path, err)
	}

	var man *snapshot.Manifest

	err = repo.WriteSession(ctx, r.rep, repo.WriteSessionOptions{Purpose: "backupd:snapshot"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			u := upload.NewUploader(w)

			m, err := u.Upload(ctx, sourceEntry, policyTree, si, previous...)
			if err != nil {
				return fmt.Errorf("uploading %s: %w", si.Path, err)
			}

			m.Description = req.Description
			m.Tags = req.Tags

			id, err := snapshot.SaveSnapshot(ctx, w, m)
			if err != nil {
				return fmt.Errorf("saving snapshot of %s: %w", si.Path, err)
			}

			m.ID = id
			man = m

			return nil
		})
	if err != nil {
		return backupengine.SnapshotInfo{}, err
	}

	return snapshotInfo(man), nil
}

// ListSnapshots implements backupengine.Repository.
func (r *repository) ListSnapshots(ctx context.Context, src backupengine.Source) ([]backupengine.SnapshotInfo, error) {
	si, err := sourceInfo(src)
	if err != nil {
		return nil, err
	}

	mans, err := snapshot.ListSnapshots(ctx, r.rep, si)
	if err != nil {
		return nil, fmt.Errorf("listing snapshots of %s: %w", si.Path, err)
	}

	// Kopia does not promise an order here, and the boundary does: oldest
	// first, because retention reads this list and an order that holds by
	// accident is an order that stops holding during an upgrade.
	sort.SliceStable(mans, func(i, j int) bool {
		return mans[i].StartTime.Before(mans[j].StartTime)
	})

	out := make([]backupengine.SnapshotInfo, 0, len(mans))
	for _, m := range mans {
		out = append(out, snapshotInfo(m))
	}

	return out, nil
}

// Verify implements backupengine.Repository.
func (r *repository) Verify(ctx context.Context, id backupengine.SnapshotID) (backupengine.VerifyReport, error) {
	man, err := r.load(ctx, id)
	if err != nil {
		return backupengine.VerifyReport{}, err
	}

	root, err := snapshotfs.SnapshotRoot(r.rep, man)
	if err != nil {
		return backupengine.VerifyReport{}, fmt.Errorf("resolving snapshot root: %w", err)
	}

	v := snapshotfs.NewVerifier(ctx, r.rep, snapshotfs.VerifierOptions{
		// 100 means every file's bytes are read back and decrypted, not
		// just its index entry checked. Anything less is sampling, and a
		// backup that is "probably" restorable is the failure mode this
		// product exists to avoid; callers who want sampling can get it by
		// verifying fewer snapshots, not by half-verifying one.
		VerifyFilesPercent: 100,
		MaxErrors:          verifyErrorBudget,
	})

	result, verifyErr := v.InParallel(ctx, func(tw *snapshotfs.TreeWalker) error {
		return tw.Process(ctx, root, "")
	})

	report := backupengine.VerifyReport{
		ObjectsVerified: result.Stats.ProcessedObjectCount,
		FilesVerified:   result.Stats.ReadFileCount,
		BytesVerified:   result.Stats.ReadBytes,
		Errors:          result.ErrorStrings,
	}

	// A walk that found damage and a walk that was torn down both come back
	// with partial stats, a populated error list and a non-nil error, and
	// they are not the same outcome: the first is an answer, the second is
	// an absence of one. Deciding between them by counting findings is what
	// this code used to do, and a cancelled verification then looked like a
	// completed one that found two problems -- the two problems being the
	// cancellation, reported once per object the walker abandoned.
	//
	// So the error is passed through whenever the walk did not complete,
	// alongside the report of what it did manage to read. ctx.Err() takes
	// precedence when it is set, because a torn-down read reports whatever
	// the layer below felt like reporting and the caller who cancelled
	// needs errors.Is(err, context.Canceled) to hold.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return report, fmt.Errorf("verifying snapshot %s: %w", id, ctxErr)
	}

	if verifyErr != nil {
		return report, fmt.Errorf("verifying snapshot %s: %w", id, verifyErr)
	}

	return report, nil
}

// Restore implements backupengine.Repository.
func (r *repository) Restore(ctx context.Context, id backupengine.SnapshotID, req backupengine.RestoreRequest) (backupengine.RestoreReport, error) {
	man, err := r.load(ctx, id)
	if err != nil {
		return backupengine.RestoreReport{}, err
	}

	root, err := snapshotfs.SnapshotRoot(r.rep, man)
	if err != nil {
		return backupengine.RestoreReport{}, fmt.Errorf("resolving snapshot root: %w", err)
	}

	target, err := filepath.Abs(req.TargetPath)
	if err != nil {
		return backupengine.RestoreReport{}, fmt.Errorf("resolving restore target %s: %w", req.TargetPath, err)
	}

	if err := os.MkdirAll(target, 0o750); err != nil {
		return backupengine.RestoreReport{}, fmt.Errorf("creating restore target %s: %w", target, err)
	}

	out := &restore.FilesystemOutput{
		TargetPath:           target,
		OverwriteDirectories: req.Overwrite,
		OverwriteFiles:       req.Overwrite,
		OverwriteSymlinks:    req.Overwrite,
		SkipOwners:           req.SkipOwners,

		// A restore that crashed halfway must not leave a file that looks
		// complete and is not, because the next thing to read it is a
		// verification that will say it is fine.
		WriteFilesAtomically: true,
	}

	if err := out.Init(ctx); err != nil {
		return backupengine.RestoreReport{}, fmt.Errorf("preparing restore output: %w", err)
	}

	// RestoreDirEntryAtDepth is the depth below which Kopia stops writing
	// real files and starts writing `.kopia-entry` placeholder stubs for a
	// later shallow expansion. Its zero value is depth zero, meaning the
	// whole restore is placeholders, which is a restore that passes every
	// statistic and contains none of the data. MaxInt32 is how Kopia's own
	// CLI spells "actually restore everything" (cli.unlimitedDepth), and it
	// has to be said explicitly because the useful default is not the zero
	// value here.
	stats, err := restore.Entry(ctx, r.rep, out, root, restore.Options{
		RestoreDirEntryAtDepth: math.MaxInt32,
	})
	if err != nil {
		return backupengine.RestoreReport{}, fmt.Errorf("restoring snapshot: %w", err)
	}

	return backupengine.RestoreReport{
		Files:       int64(stats.RestoredFileCount),
		Directories: int64(stats.RestoredDirCount),
		Symlinks:    int64(stats.RestoredSymlinkCount),
		Bytes:       stats.RestoredTotalFileSize,
	}, nil
}

// DeleteSnapshot implements backupengine.Repository.
func (r *repository) DeleteSnapshot(ctx context.Context, id backupengine.SnapshotID) error {
	// Loaded first so that deleting something that is already gone is
	// reported rather than swallowed: Kopia's DeleteManifest is idempotent
	// and returns nil for an unknown id, which would let a retention bug
	// that deletes the same snapshot twice look like two successful
	// deletions instead of one plus a mistake.
	if _, err := r.load(ctx, id); err != nil {
		return err
	}

	return repo.WriteSession(ctx, r.rep, repo.WriteSessionOptions{Purpose: "backupd:delete-snapshot"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			if err := w.DeleteManifest(ctx, manifest.ID(id)); err != nil {
				return fmt.Errorf("deleting snapshot %s: %w", id, err)
			}

			return nil
		})
}

// Maintain implements backupengine.Repository.
func (r *repository) Maintain(ctx context.Context, mode backupengine.MaintenanceMode) (backupengine.MaintenanceReport, error) {
	var kmode maintenance.Mode

	switch mode {
	case backupengine.MaintenanceQuick:
		kmode = maintenance.ModeQuick
	case backupengine.MaintenanceFull:
		kmode = maintenance.ModeFull
	default:
		return backupengine.MaintenanceReport{}, fmt.Errorf("kopia: unsupported maintenance mode %q", mode)
	}

	before, err := runCount(ctx, r.direct)
	if err != nil {
		return backupengine.MaintenanceReport{}, err
	}

	err = repo.DirectWriteSession(ctx, r.direct, repo.WriteSessionOptions{Purpose: "backupd:maintenance"},
		func(ctx context.Context, dw repo.DirectRepositoryWriter) error {
			// force=true bypasses Kopia's "is this the owning host"
			// check. backupd is the only writer of the repositories it
			// manages and it is the thing being asked to maintain them
			// right now; deferring to an owner string written by whichever
			// machine created the repository would mean a NAS-side
			// maintenance window that silently never runs.
			//
			// SafetyFull, not SafetyNone: full safety keeps recently
			// written content out of garbage collection, which is what
			// makes maintenance safe to run while a snapshot is in
			// progress. SafetyNone exists and is faster and is not used
			// here, because the thing it trades away is the only property
			// that matters.
			if err := snapshotmaintenance.Run(ctx, dw, kmode, true, maintenance.SafetyFull); err != nil {
				return fmt.Errorf("running %s maintenance: %w", mode, err)
			}

			return nil
		})
	if err != nil {
		return backupengine.MaintenanceReport{}, err
	}

	after, err := runCount(ctx, r.direct)
	if err != nil {
		return backupengine.MaintenanceReport{}, err
	}

	// Ran is read back out of the repository's own maintenance history
	// rather than inferred from a nil error, because Kopia reports "another
	// process holds the maintenance lock" and "nothing was due" by returning
	// nil, and both of those are outcomes an operator needs to be able to
	// tell apart from work having happened.
	return backupengine.MaintenanceReport{Mode: mode, Ran: after > before}, nil
}

// Close implements backupengine.Repository.
func (r *repository) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		if err := r.rep.Close(ctx); err != nil {
			r.closeErr = fmt.Errorf("closing repository: %w", err)
		}
	})

	return r.closeErr
}

// load resolves one of our opaque SnapshotIDs to a Kopia manifest, turning
// "no such manifest" into the boundary's own sentinel.
func (r *repository) load(ctx context.Context, id backupengine.SnapshotID) (*snapshot.Manifest, error) {
	man, err := snapshot.LoadSnapshot(ctx, r.rep, manifest.ID(id))
	if err != nil {
		// Two distinct sentinels reach here for the same operator
		// situation: snapshot.ErrSnapshotNotFound when the manifest id is
		// well-formed and absent, manifest.ErrNotFound from the layer
		// below it. Matching only the first would leave the second
		// leaking out as an opaque wrapped error on some future
		// refactor upstream, so both are named.
		if errors.Is(err, snapshot.ErrSnapshotNotFound) || errors.Is(err, manifest.ErrNotFound) {
			return nil, backupengine.ErrSnapshotNotFound
		}

		return nil, fmt.Errorf("loading snapshot %s: %w", id, err)
	}

	return man, nil
}

// runCount totals the maintenance runs the repository has recorded.
func runCount(ctx context.Context, dr repo.DirectRepository) (int, error) {
	sched, err := maintenance.GetSchedule(ctx, dr)
	if err != nil {
		return 0, fmt.Errorf("reading maintenance schedule: %w", err)
	}

	var n int
	for _, runs := range sched.Runs {
		n += len(runs)
	}

	return n, nil
}

// sourceInfo converts our Source identity into Kopia's, and is the only place
// that knows the two are shaped the same.
func sourceInfo(src backupengine.Source) (snapshot.SourceInfo, error) {
	if src.Host == "" || src.User == "" {
		return snapshot.SourceInfo{}, errors.New("kopia: source must carry both host and user; they are part of its identity in the repository")
	}

	path, err := filepath.Abs(src.Path)
	if err != nil {
		return snapshot.SourceInfo{}, fmt.Errorf("resolving source path %s: %w", src.Path, err)
	}

	return snapshot.SourceInfo{Host: src.Host, UserName: src.User, Path: path}, nil
}

// snapshotInfo converts a Kopia manifest into the boundary's own report.
func snapshotInfo(man *snapshot.Manifest) backupengine.SnapshotInfo {
	info := backupengine.SnapshotInfo{
		ID:          backupengine.SnapshotID(man.ID),
		Source:      backupengine.Source{Host: man.Source.Host, User: man.Source.UserName, Path: man.Source.Path},
		Start:       man.StartTime.ToTime(),
		End:         man.EndTime.ToTime(),
		Description: man.Description,
		Files:       int64(man.Stats.TotalFileCount),
		Bytes:       man.Stats.TotalFileSize,
		ReusedFiles: int64(man.Stats.CachedFiles),
		NewFiles:    int64(man.Stats.NonCachedFiles),
		Incomplete:  man.IncompleteReason,
	}

	info.Directories = int64(man.Stats.TotalDirectoryCount)

	// A manifest loaded from the repository carries its directory summary on
	// the root entry, where the live uploader's Stats does not always have a
	// directory count; prefer the summary when it is there so that a
	// snapshot reported at creation and the same snapshot reported by
	// ListSnapshots do not disagree about their own shape.
	if man.RootEntry != nil && man.RootEntry.DirSummary != nil {
		info.Directories = man.RootEntry.DirSummary.TotalDirCount
		info.Files = man.RootEntry.DirSummary.TotalFileCount
		info.Bytes = man.RootEntry.DirSummary.TotalFileSize
	}

	return info
}

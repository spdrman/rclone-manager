package backupengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	kopiafs "github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/virtualfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/kopia/kopia/snapshot/upload"
)

const (
	// spikeHost and spikeUser are the two halves of a Kopia SourceInfo this
	// spike does not model yet. A snapshot's identity in Kopia is
	// (host, user, path); this engine only needs the path to separate one
	// streamed file from another, so the other two are constants and are
	// named as placeholders rather than looking like a policy decision.
	spikeHost = "backupd-spike"
	spikeUser = "backupd"

	// rootDirName is the virtual directory the streamed file hangs off.
	// Kopia's uploader takes a directory or a file at the root of a
	// snapshot, and a bare fs.StreamingFile is neither, so one has to
	// exist. It holds exactly one entry and never touches a disk.
	rootDirName = "."

	// defaultMaxAttempts bounds how many times a broken stream is re-read
	// from byte zero. Three is the same shape internal/transport/retry
	// uses: enough to ride out one transport hiccup, few enough that a
	// remote that is genuinely gone is reported rather than hammered.
	defaultMaxAttempts = 3

	configFileName = "repository.config"
)

// Config is everything this engine needs to exist.
type Config struct {
	// RepoDir is the filesystem Kopia repository. It is created and
	// initialised if it is not one already.
	RepoDir string

	// StateDir holds the Kopia connection config. It is separate from
	// RepoDir because it is local state about a remote thing, and because
	// a test that measures repository growth must not be measuring it.
	StateDir string

	// Password protects the repository.
	Password string

	// MaxAttempts bounds whole-stream retries. Zero means
	// defaultMaxAttempts.
	MaxAttempts int
}

// Snapshot is what one completed run stored, described without a single
// Kopia type.
type Snapshot struct {
	// ID is the snapshot manifest id, and is what OpenSnapshot takes.
	ID string

	// Name is the file name that was snapshotted.
	Name string

	// Bytes is how many bytes were read from the source and stored. For a
	// stream this is not known in advance, so it is reported afterwards.
	Bytes int64

	// UploadedBytes is how many bytes this run actually pushed into the
	// repository's blob storage. On an unchanged re-snapshot it is
	// near zero while Bytes is the whole file, and that difference is the
	// measurement that says content was reused rather than re-stored.
	UploadedBytes int64

	// RootObjectID is the content address of the snapshot root.
	RootObjectID string

	// Attempts is how many times the source had to be opened, so a caller
	// can tell a clean run from one that survived an interruption.
	Attempts int

	StartTime time.Time
	EndTime   time.Time
}

// Engine snapshots streams into a Kopia repository.
type Engine struct {
	rep         repo.Repository
	maxAttempts int
}

// Open connects to the repository under cfg.RepoDir, initialising it if it
// does not exist yet.
//
// Content caching is left switched off (an empty CacheDirectory, which is
// how Kopia spells "no cache"). That is not tuning: a local cache is a
// second place the source's bytes could land, and the claim this spike
// makes is that there is no such place.
func Open(ctx context.Context, cfg Config) (*Engine, error) {
	switch {
	case cfg.RepoDir == "":
		return nil, errors.New("backupengine: RepoDir is required")
	case cfg.StateDir == "":
		return nil, errors.New("backupengine: StateDir is required")
	case cfg.Password == "":
		return nil, errors.New("backupengine: Password is required")
	}

	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}

	if err := os.MkdirAll(cfg.RepoDir, 0o700); err != nil {
		return nil, fmt.Errorf("backupengine: creating repository directory: %w", err)
	}

	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("backupengine: creating state directory: %w", err)
	}

	configFile := filepath.Join(cfg.StateDir, configFileName)

	st, err := filesystem.New(ctx, &filesystem.Options{Path: cfg.RepoDir}, true)
	if err != nil {
		return nil, fmt.Errorf("backupengine: opening filesystem storage: %w", err)
	}

	if err := repo.Initialize(ctx, st, &repo.NewRepositoryOptions{}, cfg.Password); err != nil &&
		!errors.Is(err, repo.ErrAlreadyInitialized) {
		_ = st.Close(ctx)

		return nil, fmt.Errorf("backupengine: initializing repository: %w", err)
	}

	if _, statErr := os.Stat(configFile); statErr != nil {
		if err := repo.Connect(ctx, configFile, st, cfg.Password, &repo.ConnectOptions{}); err != nil {
			_ = st.Close(ctx)

			return nil, fmt.Errorf("backupengine: connecting to repository: %w", err)
		}
	}

	// Connect persisted everything Open needs; Open builds its own
	// storage handle from that config, so this one is done.
	_ = st.Close(ctx)

	rep, err := repo.Open(ctx, configFile, cfg.Password, &repo.Options{})
	if err != nil {
		return nil, fmt.Errorf("backupengine: opening repository: %w", err)
	}

	return &Engine{rep: rep, maxAttempts: maxAttempts}, nil
}

// Close releases the repository.
func (e *Engine) Close(ctx context.Context) error {
	if err := e.rep.Close(ctx); err != nil {
		return fmt.Errorf("backupengine: closing repository: %w", err)
	}

	return nil
}

// SnapshotStream reads src once, straight into the repository, and saves a
// snapshot manifest only if the read completed.
//
// A stream that breaks is retried by re-opening the source from byte zero,
// at most Config.MaxAttempts times. A cancelled context is not retried: it
// is an instruction, not a failure.
func (e *Engine) SnapshotStream(ctx context.Context, src StreamSource) (Snapshot, error) {
	var lastErr error

	for attempt := 1; attempt <= e.maxAttempts; attempt++ {
		snap, err := e.snapshotOnce(ctx, src, attempt)
		if err == nil {
			return snap, nil
		}

		lastErr = err

		if ctx.Err() != nil {
			return Snapshot{}, err
		}
	}

	return Snapshot{}, lastErr
}

// snapshotOnce is one attempt: one open of the remote, one pass over its
// bytes, and a manifest saved only at the end of a clean pass.
//
// The whole attempt runs inside one Kopia write session. That is what makes
// "no torn snapshot marked good" structural rather than careful: if the
// callback returns an error the session is closed without flushing, so
// nothing a partial read produced is committed and no manifest exists to
// find it by.
func (e *Engine) snapshotOnce(ctx context.Context, src StreamSource, attempt int) (Snapshot, error) {
	streams := &openStreams{}
	prog := &captureProgress{}
	si := sourceInfo(src.Name())

	root := virtualfs.NewStaticDirectory(rootDirName, []kopiafs.Entry{
		newStreamingEntry(src, streams),
	})

	// Unblock a read that is already in flight when the caller cancels.
	// Kopia's copy loop only checks its cancellation flag between reads,
	// so a reader parked on a remote socket is not reachable from inside
	// the uploader. Closing it from out here is.
	attemptDone := make(chan struct{})
	defer close(attemptDone)

	go func() {
		select {
		case <-ctx.Done():
			streams.closeAll()
		case <-attemptDone:
		}
	}()

	// Belt and braces for the paths Kopia's own defer does not cover (a
	// source opened but never handed to the copy loop). guardedReader.Close
	// is idempotent, so this costs nothing when Kopia got there first.
	defer streams.closeAll()

	var uploaded atomic.Int64

	var (
		man *snapshot.Manifest
		id  manifest.ID
	)

	err := repo.WriteSession(ctx, e.rep, repo.WriteSessionOptions{
		Purpose:  "backupengine.SnapshotStream",
		OnUpload: func(n int64) { uploaded.Add(n) },
	}, func(ctx context.Context, w repo.RepositoryWriter) error {
		u := upload.NewUploader(w)
		u.Progress = prog
		u.FailFast = true
		u.DisableIgnoreRules = true
		u.ParallelUploads = 1

		// No previous manifests are passed, and that is deliberate.
		// Kopia will happily reuse a previous entry for a StreamingFile
		// when mode and modtime match (findCachedEntry takes the
		// commonMetadataEquals branch for streams), which skips the read
		// entirely. That is a statement of trust in the remote's
		// metadata, not a measurement of anything, and this spike is
		// here to measure. Every run reads every byte; reuse has to earn
		// itself at the content level.
		m, uerr := u.Upload(ctx, root, policy.BuildTree(nil, policy.DefaultPolicy), si)
		if uerr != nil {
			return uerr
		}

		if bad := prog.uploadFailure(m); bad != nil {
			return bad
		}

		sid, serr := snapshot.SaveSnapshot(ctx, w, m)
		if serr != nil {
			return fmt.Errorf("saving snapshot manifest: %w", serr)
		}

		man, id = m, sid

		return nil
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("backupengine: snapshotting %q (attempt %d): %w", src.Name(), attempt, err)
	}

	return Snapshot{
		ID:            string(id),
		Name:          src.Name(),
		Bytes:         man.Stats.TotalFileSize,
		UploadedBytes: uploaded.Load(),
		RootObjectID:  man.RootObjectID().String(),
		Attempts:      attempt,
		StartTime:     man.StartTime.ToTime(),
		EndTime:       man.EndTime.ToTime(),
	}, nil
}

// OpenSnapshot reads back what a snapshot stored, as a stream.
//
// The reader is Kopia's, over the repository, and it is the only place in
// this package where random access exists at all -- restore reads from
// content-addressed storage, not from a remote.
func (e *Engine) OpenSnapshot(ctx context.Context, snapshotID string) (io.ReadCloser, error) {
	man, err := snapshot.LoadSnapshot(ctx, e.rep, manifest.ID(snapshotID))
	if err != nil {
		return nil, fmt.Errorf("backupengine: loading snapshot %q: %w", snapshotID, err)
	}

	rootEntry, err := snapshotfs.SnapshotRoot(e.rep, man)
	if err != nil {
		return nil, fmt.Errorf("backupengine: resolving snapshot root: %w", err)
	}

	dir, ok := rootEntry.(kopiafs.Directory)
	if !ok {
		return nil, fmt.Errorf("backupengine: snapshot root is %T, not a directory", rootEntry)
	}

	name := nameFromSourceInfo(man.Source)

	child, err := dir.Child(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("backupengine: finding %q in snapshot: %w", name, err)
	}

	file, ok := child.(kopiafs.File)
	if !ok {
		return nil, fmt.Errorf("backupengine: %q restored as %T, not a file", name, child)
	}

	r, err := file.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("backupengine: opening %q for restore: %w", name, err)
	}

	return r, nil
}

// Snapshots lists what has been stored for one file name, oldest first.
func (e *Engine) Snapshots(ctx context.Context, name string) ([]Snapshot, error) {
	mans, err := snapshot.ListSnapshots(ctx, e.rep, sourceInfo(name))
	if err != nil {
		return nil, fmt.Errorf("backupengine: listing snapshots of %q: %w", name, err)
	}

	out := make([]Snapshot, 0, len(mans))

	for _, m := range mans {
		out = append(out, Snapshot{
			ID:           string(m.ID),
			Name:         name,
			Bytes:        m.Stats.TotalFileSize,
			RootObjectID: m.RootObjectID().String(),
			StartTime:    m.StartTime.ToTime(),
			EndTime:      m.EndTime.ToTime(),
		})
	}

	return out, nil
}

func sourceInfo(name string) snapshot.SourceInfo {
	return snapshot.SourceInfo{Host: spikeHost, UserName: spikeUser, Path: "/" + name}
}

func nameFromSourceInfo(si snapshot.SourceInfo) string {
	return filepath.Base(si.Path)
}

// captureProgress is a Kopia upload progress sink that exists to catch the
// one thing Upload does not return.
//
// Kopia treats a failure to read ONE entry as a property of that entry
// rather than of the run: processEntryUploadResult records it against the
// directory and returns nil, so Upload hands back a perfectly well-formed
// manifest with ErrorCount set. Saving that manifest is exactly the torn
// snapshot marked good that must never happen, and the underlying error is
// only ever offered here, through Progress.Error.
type captureProgress struct {
	upload.NullUploadProgress

	mu    sync.Mutex
	first error
}

func (p *captureProgress) Error(_ string, err error, isIgnored bool) {
	if isIgnored {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.first == nil {
		p.first = err
	}
}

// uploadFailure reports why a manifest must not be saved, or nil if it may.
func (p *captureProgress) uploadFailure(m *snapshot.Manifest) error {
	p.mu.Lock()
	first := p.first
	p.mu.Unlock()

	if first != nil {
		return first
	}

	if m.Stats.ErrorCount > 0 {
		return fmt.Errorf("upload reported %d error(s) but named none", m.Stats.ErrorCount)
	}

	if m.IncompleteReason != "" {
		return fmt.Errorf("upload is incomplete: %s", m.IncompleteReason)
	}

	if m.RootEntry == nil {
		return errors.New("upload produced no root entry")
	}

	return nil
}

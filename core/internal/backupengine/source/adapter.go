package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/backupdproject/backupd/core/internal/backend"
	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/sourceconsistency"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// SymlinkPolicy is what this adapter does with a symbolic link on the
// source.
//
// There are two members and there is deliberately no third. "Follow" is
// absent because following is not a policy this product has decided it
// can defend: a followed link can leave the backup root, produce a cycle,
// cross onto another filesystem, or pull in a tree nobody asked for, and
// each of those needs its own answer before the word appears in a config
// file. Nothing here follows a link, under either policy.
type SymlinkPolicy string

const (
	// SymlinkIgnore records the link's existence in the run report and
	// stores nothing for it. It is the default because it is what every
	// backend this build ships declares (symlink_semantics: "skip").
	SymlinkIgnore SymlinkPolicy = "ignore"

	// SymlinkPreserve stores the link itself - its target as bytes,
	// under the link's own path - so a restore can recreate it.
	//
	// It is available only on a backend whose matrix declares
	// symlink_semantics "store" AND a transport that can read a link's
	// target without following it. Both are refusals at configuration
	// time rather than surprises at run time, because a policy that
	// silently degrades to "ignore" is a policy that lies to whoever set
	// it.
	SymlinkPreserve SymlinkPolicy = "preserve"
)

// DefaultConcurrency is how many objects are read at once when a caller
// has not said.
//
// Four, and the shape of the number matters more than the value. One is
// too few: a remote read is mostly latency, so a single-threaded walk
// spends a backup window waiting. Unbounded is the failure this
// constant exists to prevent - one buffer and one connection per object
// in flight, over a source with a million of them, is the same
// out-of-memory kill bounded enumeration was built to avoid, arriving
// from the other side. Four is enough to keep a link busy and small
// enough that the peak is a number rather than a function of the source.
const DefaultConcurrency = 4

// maxReportedReasons caps how many per-object sentences a Report carries.
//
// A report is read by a person. The thousandth reason tells them nothing
// the first fifty did not, and a run over a source that is entirely
// unreadable would otherwise build a slice with one string per file,
// which is the unbounded allocation this package refuses everywhere else.
// The counters stay exact; only the prose is sampled, and the report says
// how many were dropped.
const maxReportedReasons = 64

// ErrNoStreamer and friends are the construction refusals. They are
// separate from the capability refusals in profile.go because they are
// wiring mistakes rather than facts about a backend: a caller that built
// an Adapter without a way to open a stream has a bug, and a backend that
// cannot stream has a manifest.
var (
	ErrNoStreamer = errors.New("source: the adapter has no way to open a source stream")
	ErrNoSink     = errors.New("source: the adapter has nowhere to put what it reads")
	ErrNoStater   = errors.New("source: the adapter has no way to re-stat an object after reading it, so no read window could be checked")
	ErrNoLinks    = errors.New("source: the symlink policy is \"preserve\" and this transport cannot read a link target without following it")
)

// Deps are the capabilities the adapter is given. Every one of them is
// read-only against the source: there is no delete, no write and no move
// anywhere in this set, which is how "a backup never deletes from the
// source" is enforced rather than promised.
type Deps struct {
	// Streamer opens an object for one forward read. Required.
	Streamer Streamer

	// Stater answers what the source says about an object, and is what
	// the post-read half of the mutation check asks. Required unless
	// every run this adapter will perform declares a consistency mode
	// that guarantees a point in time.
	Stater Stater

	// Enumerator walks the source. Required for Backup; BackupPaths does
	// not use it, which is what lets a backend that cannot be listed in
	// bounded memory still be backed up from a path list.
	Enumerator transport.Enumerator

	// Links reads a symbolic link's target without following it.
	// Optional, and required only under SymlinkPreserve.
	Links LinkReader

	// Profiles resolves an rclone backend name to the capability answers
	// this adapter reads. Nil means ProfileFor, which is the bundled
	// matrix and is what production wires.
	//
	// It is a seam rather than a constant for one reason worth stating:
	// the refusals in this package are only worth having if they are
	// exercised, and half of them are for backends this build does not
	// ship - one that cannot stream, one that stores symlinks, one that
	// reports nothing a read window could be checked against. Without
	// this field those branches would be unreachable and therefore
	// untested, which is the state a fail-closed gate must never be in.
	// It cannot loosen a shipped backend: the bundled manifests are the
	// only thing ProfileFor will read.
	Profiles func(rcloneBackend string) (Profile, error)
}

// Options are the run-wide decisions.
type Options struct {
	// Symlinks is what happens to a symbolic link. Empty means
	// SymlinkIgnore.
	Symlinks SymlinkPolicy

	// Mode is the consistency mode the operator declared for this
	// source. It decides whether a read window has to be checked at all:
	// a mode that guarantees a point in time is a promise that the
	// source cannot move while it is read.
	Mode model.ConsistencyMode

	// Preset is the operator's metadata-trust preset, which with the
	// backend's trust class decides the verification policy the run
	// reports.
	Preset model.MetadataTrustPreset

	// Concurrency is how many objects are read at once. Zero means
	// DefaultConcurrency; negative is refused rather than quietly
	// meaning one, because a caller that computed a negative worker
	// count has a bug.
	Concurrency int

	// MaxAttempts bounds how many times one object is re-read when it
	// moved under the reader or the stream broke. Zero means
	// sourceconsistency.DefaultMaxAttempts.
	//
	// This is the run's ONLY retry bound. Everything below the adapter
	// is asked for exactly one attempt (see RepositorySink.Store), so
	// this number is the number of reads a failing object costs, not a
	// factor in a product of three of them.
	MaxAttempts int

	// ChunkEntries bounds one directory read during enumeration. Zero
	// means transport.DefaultChunkEntries.
	ChunkEntries int

	// OnResult, when set, is called once per source entry with what
	// happened to it, in worker order.
	//
	// It exists because a Report cannot hold a record per file - that is
	// the allocation this whole path avoids - and a caller that wants
	// per-file detail (a catalog writer, a progress feed) needs it
	// streamed for the same reason the directory listing is. It is
	// called from several goroutines at once and must be safe for that;
	// the adapter does not serialise it, because serialising every
	// callback would put a run-wide mutex on the hot path for the
	// benefit of callers that mostly do not need one.
	OnResult func(Result)
}

// Result is what happened to one source entry.
type Result struct {
	Path     string
	Kind     sourceconsistency.Kind
	Outcome  sourceconsistency.Outcome
	Attempts int

	// Bytes is what the read actually produced, which on a verified
	// result is also what the source held.
	Bytes int64

	// StoredID is the sink's handle for what was stored, empty when
	// nothing was.
	StoredID string

	// Reason is one sentence, populated for every outcome including the
	// successful ones: a retried object is a fact about the source worth
	// reading even when it settled.
	Reason string
}

// Verified reports whether these bytes were proven coherent across their
// own read window.
func (r Result) Verified() bool {
	return r.Outcome == sourceconsistency.OutcomeStable || r.Outcome == sourceconsistency.OutcomeRetried
}

// Report is one run, in counters and a bounded sample of sentences.
//
// It is counters rather than records for the reason the package doc
// gives: a source with a million files must cost a report the size of a
// report, not the size of the source. A caller that needs per-file detail
// receives it through Options.OnResult as the run produces it.
type Report struct {
	// Backend is the manifest id the capability decisions were read
	// from, so a report says which matrix it was judged against.
	Backend string

	// Trust and Policy are what this backend's declarations and the
	// operator's preset came to. They are on the report because "why did
	// this run re-read everything" is a question an operator asks.
	Trust  model.TrustClassification
	Policy model.VerificationPolicy

	// Entries is how many source entries the run considered, of every
	// kind, including the ones it refused.
	Entries int64

	// Stored is how many objects were stored AND proven coherent.
	Stored int64

	// Bytes is what those reads produced; UploadedBytes is how much of
	// it was new to the repository. The gap between them is the
	// deduplication report.
	Bytes         int64
	UploadedBytes int64

	// Attempts is how many reads the run performed, so a clean run and
	// one that survived a busy source are distinguishable.
	Attempts int64

	// Retried counts objects that settled only after moving under the
	// reader.
	Retried int64

	// Incomplete counts objects whose content this run does not hold a
	// proven copy of: torn beyond the retry bound, unreadable, or
	// refused. Vanished counts the ordinary case of a file deleted
	// between the listing and the read, which is not an error but is
	// still not a capture.
	Incomplete int64
	Unreadable int64
	Vanished   int64

	// Refused counts entries whose PATH this adapter would not use.
	Refused int64

	// SkippedSymlink, SkippedSpecial and SkippedExcluded are the entries
	// deliberately not stored, by reason. They are separate counters
	// because they answer different questions: the first two are policy,
	// the third is configuration.
	SkippedSymlink  int64
	SkippedSpecial  int64
	SkippedExcluded int64

	// Reasons is up to maxReportedReasons sentences about entries that
	// were not stored, and DroppedReasons is how many more there were.
	Reasons        []string
	DroppedReasons int64
}

// Complete reports whether every entry the run touched was either stored
// and proven, or deliberately skipped by policy.
//
// A refusal counts against it. An entry whose name this adapter would not
// use is an entry the operator selected and the backup does not contain,
// and a run report that called itself complete anyway would be the exact
// silent hole this package exists to prevent.
func (r Report) Complete() bool {
	return r.Incomplete == 0 && r.Refused == 0 && r.Unreadable == 0
}

// Adapter reads a backup source and hands its objects to a sink.
type Adapter struct {
	deps Deps
	opts Options
}

// New builds an adapter, refusing a wiring mistake here rather than
// discovering it on the thousandth file of a backup window.
func New(deps Deps, opts Options) (*Adapter, error) {
	if deps.Streamer == nil {
		return nil, ErrNoStreamer
	}

	if opts.Concurrency < 0 {
		return nil, fmt.Errorf("source: a concurrency of %d is not a number of workers", opts.Concurrency)
	}

	if opts.MaxAttempts < 0 {
		return nil, fmt.Errorf("source: an attempt bound of %d is not a number of attempts", opts.MaxAttempts)
	}

	switch opts.Symlinks {
	case "":
		opts.Symlinks = SymlinkIgnore
	case SymlinkIgnore, SymlinkPreserve:
	default:
		return nil, fmt.Errorf("source: %q is not a symlink policy; it is %q or %q", opts.Symlinks, SymlinkIgnore, SymlinkPreserve)
	}

	if opts.Symlinks == SymlinkPreserve && deps.Links == nil {
		return nil, ErrNoLinks
	}

	if opts.Concurrency == 0 {
		opts.Concurrency = DefaultConcurrency
	}

	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = sourceconsistency.DefaultMaxAttempts
	}

	return &Adapter{deps: deps, opts: opts}, nil
}

// Request is one backup of one source.
type Request struct {
	// Source is the transport-level source: where it is, how to reach
	// it, what it excludes.
	Source transport.Source

	// Sink is where the bytes go.
	Sink Sink
}

// Backup walks the source and stores everything under it.
//
// It requires a backend whose capability matrix declares bounded_listing
// and refuses one that does not, BEFORE anything is dialed. That refusal
// is the whole point of the gate: a walk of a backend with no resumable
// directory cursor materialises the directory in the layer underneath
// before any code here could count it, so a refusal that arrived later
// would arrive after the memory it was supposed to save. A source on such
// a backend is backed up with BackupPaths.
func (a *Adapter) Backup(ctx context.Context, req Request) (Report, error) {
	r, err := a.newRun(req)
	if err != nil {
		return Report{}, err
	}

	if err := r.profile.Enumerable(); err != nil {
		return Report{}, err
	}

	if a.deps.Enumerator == nil {
		return Report{}, errors.New("source: the adapter has no enumerator, so it cannot walk a source")
	}

	return r.execute(ctx, func(ctx context.Context, feed func(transport.RemoteArtifact) error) error {
		opts := transport.EnumerateOptions{
			ChunkEntries: a.opts.ChunkEntries,
			// The adapter has a symlink policy and a special-file
			// policy, and a policy cannot be applied to entries it is
			// never told about. Silently dropping them would make "this
			// backup contains your source" true only for the parts that
			// happened to be regular files, and say so nowhere.
			ReportNonRegular: true,
		}

		//nolint:wrapcheck // the enumerator classifies its own errors; re-wrapping loses the category.
		return a.deps.Enumerator.Enumerate(ctx, req.Source, opts, feed)
	})
}

// BackupPaths stores exactly the objects named, and walks nothing.
//
// It is how a source on a backend that cannot be listed within a memory
// bound is still backed up: SFTP declares bounded_listing false - pkg/sftp
// reads a directory as one slice with no resumable cursor - and declares
// streaming_open true, so its objects can be read even though its
// directories cannot be walked safely. The paths come from the caller,
// and they go through exactly the same safety gate as the ones a listing
// produces, because a path in a configuration file is no more trustworthy
// than a path in a directory.
func (a *Adapter) BackupPaths(ctx context.Context, req Request, paths []string) (Report, error) {
	r, err := a.newRun(req)
	if err != nil {
		return Report{}, err
	}

	if a.deps.Stater == nil {
		return Report{}, ErrNoStater
	}

	return r.execute(ctx, func(ctx context.Context, feed func(transport.RemoteArtifact) error) error {
		for _, p := range paths {
			if err := ctx.Err(); err != nil {
				return err //nolint:wrapcheck // a cancelled run reports the cancellation.
			}

			safe, err := SafeRelPath(p)
			if err != nil {
				// Refused before it is stat'ed: an unsafe path is not
				// dialed, so a name designed to escape never reaches the
				// transport at all.
				r.refuse(p, err)

				continue
			}

			art, err := a.deps.Stater.StatSource(ctx, req.Source, safe)
			if err != nil {
				r.recordFailure(safe, sourceconsistency.KindRegular, statOutcome(err), err)

				continue
			}
			art.Path = safe

			if art.Kind == transport.EntryKindUnknown {
				// The caller named this path, which is an assertion
				// that it is an object to back up, and no transport
				// this build has can classify a remote path anyway:
				// rclone's Fs.NewObject returns an object for a fifo
				// and follows a symlink to its target, so "it
				// resolved" answers a different question.
				//
				// This is the one place a kind is taken on trust, and
				// it is taken from the operator rather than guessed
				// from a resolution. It is NOT how an enumerated entry
				// is treated: a walk classifies what it finds, and an
				// unclassified entry off a walk is skipped rather than
				// opened (see handle).
				//
				// A caller who names a fifo gets a read that waits for
				// a writer. That is bounded by the run's context and by
				// nothing else, which is the honest statement: this
				// adapter cannot make a remote path safe to open when
				// no layer beneath it will say what the path is.
				art.Kind = transport.EntryKindRegular
			}

			if err := feed(art); err != nil {
				return err
			}
		}

		return nil
	})
}

// newRun resolves everything that is decided once per run and refuses
// every combination that cannot be honoured, before a single byte moves.
func (a *Adapter) newRun(req Request) (*run, error) {
	if req.Sink == nil {
		return nil, ErrNoSink
	}

	lookup := a.deps.Profiles
	if lookup == nil {
		lookup = ProfileFor
	}

	profile, err := lookup(req.Source.Type)
	if err != nil {
		return nil, err
	}

	if err := profile.Streamable(); err != nil {
		return nil, err
	}

	if err := profile.MutationDetectable(a.opts.Mode); err != nil {
		return nil, err
	}

	if !a.opts.Mode.GuaranteesPointInTime() && a.deps.Stater == nil {
		return nil, fmt.Errorf("%w: consistency mode %q does not promise the source holds still", ErrNoStater, a.opts.Mode)
	}

	if a.opts.Symlinks == SymlinkPreserve && profile.SymlinkSemantics() != backend.SymlinksStored {
		return nil, fmt.Errorf(
			"source: the symlink policy is %q and backend %q declares symlink_semantics %q, so storing a link would mean inventing a capability it does not have",
			SymlinkPreserve, profile.BackendID, profile.SymlinkSemantics())
	}

	trust := model.ClassifyMetadataTrust(profile.Signals())

	return &run{
		adapter:  a,
		profile:  profile,
		source:   req.Source,
		sink:     req.Sink,
		excluded: profile.ExcludeMatcher(req.Source.ExcludePaths),
		report: Report{
			Backend: profile.BackendID,
			Trust:   trust,
			Policy:  model.VerificationPolicyFor(trust.Class, a.opts.Preset),
		},
	}, nil
}

// run is one backup in progress.
type run struct {
	adapter  *Adapter
	profile  Profile
	source   transport.Source
	sink     Sink
	excluded func(string) bool

	mu     sync.Mutex
	report Report
}

// execute runs a producer against a bounded pool of readers.
//
// The producer is synchronous and pushes into a channel the workers read
// from, so a full channel blocks the walk. That backpressure is what
// keeps the run bounded: without it a fast local listing would build a
// queue of every path in the source while four workers read four of them.
//
// Every path out of this function - clean, refused by the producer,
// cancelled - closes the work channel exactly once and waits for every
// worker to return. A run that returned while a worker was still reading
// would leak a goroutine and a connection per abandoned object, and the
// leak would be invisible until a daemon that had run a few thousand
// backups stopped being able to open files.
func (r *run) execute(ctx context.Context, produce func(context.Context, func(transport.RemoteArtifact) error) error) (Report, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	work := make(chan transport.RemoteArtifact)

	var wg sync.WaitGroup

	for range r.adapter.opts.Concurrency {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for art := range work {
				r.handle(ctx, art)
			}
		}()
	}

	err := produce(ctx, func(art transport.RemoteArtifact) error {
		select {
		case work <- art:
			return nil
		case <-ctx.Done():
			return ctx.Err() //nolint:wrapcheck // a cancelled run reports the cancellation.
		}
	})

	close(work)
	wg.Wait()

	if err != nil {
		return r.snapshot(), err
	}

	return r.snapshot(), nil
}

// handle applies every policy that can be decided without reading, and
// hands what survives to the reader.
func (r *run) handle(ctx context.Context, art transport.RemoteArtifact) {
	if err := ctx.Err(); err != nil {
		r.recordFailure(art.Path, KindOf(art.Kind), sourceconsistency.OutcomeUnreadable, err)

		return
	}

	safe, err := SafeRelPath(art.Path)
	if err != nil {
		r.refuse(art.Path, err)

		return
	}
	art.Path = safe

	if r.excluded(safe) {
		r.count(func(rep *Report) { rep.Entries++; rep.SkippedExcluded++ })

		return
	}

	kind := KindOf(art.Kind)
	switch kind {
	case sourceconsistency.KindSymlink:
		r.handleSymlink(ctx, art)
	case sourceconsistency.KindOther:
		// A socket, a fifo or a device node. It is never opened, and the
		// "never" is the point rather than the tidiness: a fifo with no
		// writer blocks in Read until something writes to it, so an
		// adapter that opened one because it had no policy for it would
		// hang a backup window on a file nobody meant to back up.
		r.count(func(rep *Report) { rep.Entries++; rep.SkippedSpecial++ })
		r.emit(Result{
			Path:    safe,
			Kind:    kind,
			Outcome: sourceconsistency.OutcomeNotAFile,
			Reason:  "a socket, fifo or device node, which holds no content this product copies and is never opened",
		})
	case sourceconsistency.KindRegular:
		r.capture(ctx, art)
	case sourceconsistency.KindDir:
		// A directory never reaches here: the enumerator descends into
		// one rather than yielding it, and KindOf has no transport kind
		// that maps to this. It is named so the switch is exhaustive
		// over the vocabulary rather than falling through silently if a
		// future enumerator does yield one.
		r.count(func(rep *Report) { rep.Entries++; rep.SkippedSpecial++ })
	}
}

// handleSymlink applies the symlink policy. Nothing here follows a link
// under either policy; the difference is whether the link itself is
// stored.
func (r *run) handleSymlink(ctx context.Context, art transport.RemoteArtifact) {
	if r.adapter.opts.Symlinks == SymlinkIgnore {
		r.count(func(rep *Report) { rep.Entries++; rep.SkippedSymlink++ })
		r.emit(Result{
			Path:    art.Path,
			Kind:    sourceconsistency.KindSymlink,
			Outcome: sourceconsistency.OutcomeNotAFile,
			Reason:  "a symbolic link, which this run's symlink policy skips and which is never followed",
		})

		return
	}

	target, err := r.adapter.deps.Links.ReadSourceLink(ctx, r.source, art.Path)
	if err != nil {
		r.recordFailure(art.Path, sourceconsistency.KindSymlink, statOutcome(err), err)

		return
	}

	if err := linkTargetWithinRoot(art.Path, target); err != nil {
		r.refuse(art.Path, err)

		return
	}

	// The link is stored as its target, which is what a symbolic link
	// IS: a small file whose content is a path. Nothing resolves it, so
	// a link to a file that does not exist is stored exactly as
	// faithfully as one that does.
	r.store(ctx, art, sourceconsistency.KindSymlink, func() (backupengine.StreamSource, func() int64, func()) {
		s := newLiteralStream([]byte(target), r.profile.ModTime(art.ModTime))

		return s, s.BytesRead, func() {}
	})
}

// capture reads one object, checks the window it was read across, and
// records what was proven.
func (r *run) capture(ctx context.Context, art transport.RemoteArtifact) {
	r.store(ctx, art, sourceconsistency.KindRegular, func() (backupengine.StreamSource, func() int64, func()) {
		s := newObjectStream(r.adapter.deps.Streamer, r.source, art.Path, r.profile.ModTime(art.ModTime))

		return s, s.BytesRead, s.closeAll
	})
}

// store is the retry boundary and the mutation check, in one place for
// both of the things that can be stored.
//
// The loop is the ONLY retry in the run. Every attempt is a fresh open
// from byte zero, because a remote stream cannot be resumed without a
// seek this boundary refuses to pretend it has, and the sink is asked for
// exactly one attempt of its own so that three bounds cannot multiply
// into twenty-seven reads of a file that is never going to settle.
func (r *run) store(
	ctx context.Context,
	art transport.RemoteArtifact,
	kind sourceconsistency.Kind,
	open func() (backupengine.StreamSource, func() int64, func()),
) {
	before := r.profile.StatOf(art)
	before.Kind = kind

	var (
		attempts int
		lastWhy  string
	)

	for attempt := 1; attempt <= r.adapter.opts.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			r.recordFailure(art.Path, kind, sourceconsistency.OutcomeUnreadable, err)

			return
		}

		attempts = attempt

		stream, bytesRead, closeAll := open()

		// The watcher is the cancellation contract, and it has to be a
		// goroutine because Store does not return while the read is
		// blocked. A context is not something a Read already waiting on
		// a remote socket consults; closing the reader from the outside
		// is the only thing that unblocks one, and the only place that
		// can be done from is beside the call rather than after it. It
		// costs one goroutine per in-flight object, which is bounded by
		// the worker count, and it exits on whichever of the two
		// channels fires first.
		finished := make(chan struct{})

		var watcher sync.WaitGroup

		watcher.Add(1)

		go func() {
			defer watcher.Done()

			select {
			case <-ctx.Done():
				closeAll()
			case <-finished:
			}
		}()

		stored, err := r.sink.Store(ctx, Object{
			Path:    art.Path,
			Kind:    kind,
			Size:    before.Size,
			ModTime: r.profile.ModTime(art.ModTime),
			Stream:  stream,
			Attempt: attempt,
		})

		close(finished)
		watcher.Wait()
		closeAll()

		if cerr := ctx.Err(); cerr != nil {
			// A cancelled run is an instruction, not a failure, and is
			// never retried. Whatever the sink reported is a symptom of
			// the teardown rather than a fact about the source.
			r.recordFailure(art.Path, kind, sourceconsistency.OutcomeUnreadable, cerr)

			return
		}

		if err != nil {
			lastWhy = err.Error()
			if attempt < r.adapter.opts.MaxAttempts {
				continue
			}

			r.recordAttempts(attempts)
			r.recordFailure(art.Path, kind, statOutcome(err), err)

			return
		}

		settled, why := r.settled(ctx, art, &before, stored, bytesRead())
		if settled {
			r.recordStored(art.Path, kind, attempt, stored, why)

			return
		}

		lastWhy = why

		// The bytes just stored came from a read that cannot be proven
		// coherent, so the restore point they made is taken away again.
		// This is what "never store a torn file as verified" has to mean
		// on a path that reads a stream once: the tear is only visible
		// after the bytes are already in the repository.
		if derr := r.sink.Discard(ctx, stored.ID); derr != nil {
			r.recordAttempts(attempts)
			r.recordFailure(art.Path, kind, sourceconsistency.OutcomeIncomplete, fmt.Errorf(
				"%s, and the torn copy stored as %q could not be removed: %w", why, stored.ID, derr))

			return
		}
	}

	r.recordAttempts(attempts)
	r.count(func(rep *Report) { rep.Entries++; rep.Incomplete++ })
	r.note(fmt.Sprintf("%s: still moving after %d reads: %s", art.Path, attempts, lastWhy))
	r.emit(Result{
		Path:     art.Path,
		Kind:     kind,
		Outcome:  sourceconsistency.OutcomeIncomplete,
		Attempts: attempts,
		Reason:   lastWhy,
	})
}

// settled reports whether the object held still across the read, and the
// sentence explaining the answer either way.
//
// The comparison is between the metadata BEFORE the read and the source's
// answer AFTER it, plus the byte count the read actually produced. The
// third of those is the one a metadata-only check misses: a source that
// truncates a file and restores its size and timestamp leaves the two
// stats agreeing about a file whose bytes are not the ones that were
// read.
//
// before is updated in place so that a retry compares against the state
// the source has now rather than the state the listing described, which
// is what lets a file that was rewritten once settle on the next attempt
// instead of failing against a baseline that is never coming back.
func (r *run) settled(ctx context.Context, art transport.RemoteArtifact, before *sourceconsistency.Stat, stored Stored, read int64) (bool, string) {
	if r.adapter.opts.Mode.GuaranteesPointInTime() {
		// The operator declared a frozen image. There is nothing for a
		// post-read stat to find, and asking for one would be a round
		// trip per object to confirm a promise the mode already made.
		return true, "read under a consistency mode that guarantees a point in time"
	}

	after, err := r.adapter.deps.Stater.StatSource(ctx, r.source, art.Path)
	if err != nil {
		return false, fmt.Sprintf("the source would not say what is at this path after the read: %v", err)
	}

	afterStat := r.profile.StatOf(after)
	if after.Kind == transport.EntryKindUnknown {
		// The stat did not classify the entry. That is an absence of
		// evidence, not evidence of a change: carrying the earlier kind
		// across keeps a transport that answers no kind at all from
		// reporting every object as having changed shape mid-read.
		afterStat.Kind = before.Kind
	}

	if moved := sourceconsistency.DescribeMovement(*before, afterStat); moved != "" {
		*before = afterStat

		return false, moved
	}

	// The read-length check applies to an object whose bytes came from
	// the object. A preserved symbolic link's stored bytes are its
	// TARGET, and what a source reports as a link's size is the target's
	// length on one backend and zero on another, so comparing the two
	// would report every link on half the backends as torn. The
	// metadata comparison above still covers a link: a target that
	// changed under the read changes the link's own stat.
	if before.Kind == sourceconsistency.KindRegular && r.profile.StableSize() && after.Size != read {
		*before = afterStat

		return false, fmt.Sprintf(
			"the read produced %d bytes and the source says the object is %d, so the read did not see the whole of it",
			read, after.Size)
	}

	if stored.Bytes != read {
		// The sink reported a length that is not the length the stream
		// produced. Nothing about the source is wrong here; the sink is,
		// and a mutation check that trusted its number would be checking
		// the source against a claim rather than against a measurement.
		return false, fmt.Sprintf(
			"the sink reported %d bytes for a stream that produced %d, so nothing here can be proven",
			stored.Bytes, read)
	}

	return true, "nothing about the object changed across the read"
}

// --- reporting -----------------------------------------------------------

func (r *run) count(f func(*Report)) {
	r.mu.Lock()
	defer r.mu.Unlock()

	f(&r.report)
}

func (r *run) note(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.report.Reasons) >= maxReportedReasons {
		r.report.DroppedReasons++

		return
	}

	r.report.Reasons = append(r.report.Reasons, reason)
}

func (r *run) emit(res Result) {
	if r.adapter.opts.OnResult != nil {
		r.adapter.opts.OnResult(res)
	}
}

func (r *run) recordAttempts(n int) {
	r.count(func(rep *Report) { rep.Attempts += int64(n) })
}

func (r *run) refuse(rawPath string, err error) {
	r.count(func(rep *Report) { rep.Entries++; rep.Refused++ })
	r.note(err.Error())
	r.emit(Result{
		Path:    rawPath,
		Outcome: sourceconsistency.OutcomeUnreadable,
		Reason:  err.Error(),
	})
}

func (r *run) recordFailure(p string, kind sourceconsistency.Kind, outcome sourceconsistency.Outcome, err error) {
	r.count(func(rep *Report) {
		rep.Entries++
		switch outcome {
		case sourceconsistency.OutcomeVanished:
			rep.Vanished++
		case sourceconsistency.OutcomeIncomplete:
			rep.Incomplete++
		default:
			rep.Unreadable++
		}
	})
	r.note(p + ": " + err.Error())
	r.emit(Result{Path: p, Kind: kind, Outcome: outcome, Reason: err.Error()})
}

func (r *run) recordStored(p string, kind sourceconsistency.Kind, attempts int, stored Stored, why string) {
	outcome := sourceconsistency.OutcomeStable
	if attempts > 1 {
		outcome = sourceconsistency.OutcomeRetried
	}

	r.count(func(rep *Report) {
		rep.Entries++
		rep.Stored++
		rep.Bytes += stored.Bytes
		rep.UploadedBytes += stored.UploadedBytes
		rep.Attempts += int64(attempts)
		if attempts > 1 {
			rep.Retried++
		}
	})

	if attempts > 1 {
		r.note(fmt.Sprintf("%s: settled after %d reads: %s", p, attempts, why))
	}

	r.emit(Result{
		Path:     p,
		Kind:     kind,
		Outcome:  outcome,
		Attempts: attempts,
		Bytes:    stored.Bytes,
		StoredID: stored.ID,
		Reason:   why,
	})
}

func (r *run) snapshot() Report {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := r.report
	out.Reasons = append([]string(nil), r.report.Reasons...)

	return out
}

// statOutcome reads a transport failure as one of the capture outcomes.
//
// A vanished file is separated from an unreadable one because they are
// different facts with different remedies: a backup source is a live
// directory somebody else is writing to, and a file deleted between the
// listing and the read is ordinary, while a permission error is a
// configuration problem an operator has to fix.
func statOutcome(err error) sourceconsistency.Outcome {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return sourceconsistency.OutcomeUnreadable
	}
	var terr *transport.Error
	if errors.As(err, &terr) && terr.Category == transport.NotFound {
		return sourceconsistency.OutcomeVanished
	}

	if strings.Contains(err.Error(), "object not found") {
		return sourceconsistency.OutcomeVanished
	}

	return sourceconsistency.OutcomeUnreadable
}

// linkTargetWithinRoot refuses a symbolic link whose target leaves the
// backup root.
//
// The check is lexical and is performed on the target STRING, without
// resolving anything, which is the only way to check it honestly: a
// resolution would follow the link, and following is what this package
// does not do. An absolute target is outside the root by definition -
// the root is a relative namespace here - and a relative one is joined
// onto the link's own directory and required to stay inside.
func linkTargetWithinRoot(linkPath, target string) error {
	if target == "" {
		return fmt.Errorf("%w: the link at %q has an empty target", ErrUnsafePath, linkPath)
	}

	if strings.ContainsRune(target, 0) {
		return fmt.Errorf("%w: the link at %q has a target containing a NUL byte", ErrUnsafePath, linkPath)
	}

	if strings.HasPrefix(target, "/") || isWindowsDriveRelative(target) || strings.Contains(target, `\`) {
		return fmt.Errorf("%w: the link at %q points at %q, which is outside the backup root", ErrUnsafePath, linkPath, target)
	}

	resolved := path.Join(path.Dir(linkPath), target)
	if resolved == ".." || strings.HasPrefix(resolved, "../") || resolved == "." {
		return fmt.Errorf("%w: the link at %q points at %q, which resolves to %q, outside the backup root", ErrUnsafePath, linkPath, target, resolved)
	}

	return nil
}

// literalStream is a StreamSource over bytes this process already holds:
// a symbolic link's target, which is the only content a source hands over
// that is not read from an object.
type literalStream struct {
	data    []byte
	modTime time.Time

	mu   sync.Mutex
	read int64
}

func newLiteralStream(data []byte, modTime time.Time) *literalStream {
	return &literalStream{data: data, modTime: modTime}
}

func (s *literalStream) ModTime() time.Time { return s.modTime }

func (s *literalStream) Open(context.Context) (io.ReadCloser, error) {
	s.mu.Lock()
	s.read = 0
	s.mu.Unlock()

	return &literalReader{stream: s, remaining: s.data}, nil
}

func (s *literalStream) BytesRead() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.read
}

type literalReader struct {
	stream    *literalStream
	remaining []byte
}

func (l *literalReader) Read(p []byte) (int, error) {
	if len(l.remaining) == 0 {
		return 0, io.EOF
	}

	n := copy(p, l.remaining)
	l.remaining = l.remaining[n:]

	l.stream.mu.Lock()
	l.stream.read += int64(n)
	l.stream.mu.Unlock()

	return n, nil
}

func (l *literalReader) Close() error { return nil }

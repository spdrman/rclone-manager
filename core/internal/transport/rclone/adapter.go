// Package rclone is the only package in this repository that imports rclone.
//
// Exactly three backends are registered. Importing all of them for
// convenience would cost binary size, dependency surface, initialization
// complexity and accidental configuration exposure, so each one is an
// architecture decision rather than an import line (FR-4). local and sftp
// are FR-4's own two; s3 is EPIC E's FR-28, and it is the entire S3
// implementation this product has, in Go and in TypeScript alike.
package rclone

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	// local and sftp are the two backends FR-4 requires, and s3 is the
	// third, added by EPIC E's FR-28 as the storage-medium implementation.
	// Importing them, together with fs/operations below, also registers
	// crypt transitively. See backends.go for the traced cause, why it's
	// accepted rather than removed, the measured cost of s3, and the test
	// that keeps this exact set enforced.
	_ "github.com/rclone/rclone/backend/local"
	_ "github.com/rclone/rclone/backend/s3"
	_ "github.com/rclone/rclone/backend/sftp"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/walk"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Adapter implements transport.Transport over embedded rclone packages.
type Adapter struct{}

// New returns an adapter. It takes no rclone types, by design.
func New() *Adapter { return &Adapter{} }

var _ transport.Transport = (*Adapter)(nil)

// ErrUnsupportedHash is returned by RemoteHash when the requested algorithm
// is one this adapter does not know how to translate to an rclone hash.Type
// at all, or one the backend behind src cannot compute for this object.
//
// It exists so errors.go's classify can recognize this case by identity
// (errors.Is) instead of matching this same package's own error strings a
// second time. Matching a dependency's wording is unavoidable in a couple of
// documented spots in errors.go because no typed value exists to reach for
// instead; matching a string adapter.go itself produces never had that
// excuse, since a sentinel defined right here is exactly as cheap and does
// not silently drift if one file's wording changes without the other's.
var ErrUnsupportedHash = errors.New("rclone: unsupported hash")

// fsFor builds an rclone Fs for a source without touching any on-disk rclone
// config file. Everything comes from the manager's own configuration, so there
// is no ambient rclone state to leak in.
//
// sftp options are built by sftpConfig in ssh.go, which owns the SSH
// authentication and host-key verification posture required by FR-6.
func (a *Adapter) fsFor(ctx context.Context, src transport.Source) (fs.Fs, error) {
	return a.newFs(ctx, src, false)
}

// fsForHashing builds the same Fs fsFor does, plus the two sftp options
// rclone needs before it will compute a SHA-256 digest (see withSHA256 in
// ssh.go, and in particular why those options must not reach the Fs that
// copies).
func (a *Adapter) fsForHashing(ctx context.Context, src transport.Source) (fs.Fs, error) {
	return a.newFs(ctx, src, true)
}

func (a *Adapter) newFs(ctx context.Context, src transport.Source, forHashing bool) (fs.Fs, error) {
	info, err := fs.Find(src.Type)
	if err != nil {
		return nil, fmt.Errorf("backend %q is not registered in this binary: %w", src.Type, err)
	}

	cfg := configmap.Simple{}
	if src.Type == "sftp" {
		sftpCfg, err := sftpConfig(src)
		if err != nil {
			return nil, err
		}
		cfg = sftpCfg
		if forHashing {
			cfg = withSHA256(cfg)
		}
	}

	f, err := info.NewFs(ctx, src.ID, src.Root, cfg)
	if err != nil {
		// A backend may hand back a LIVE Fs alongside an error, and
		// rclone's sftp backend does exactly that. NewFsWithConnection
		// ends `return f, err`, and the err that reaches here can be
		// fs.ErrorIsFile: the root named an existing file, so rclone
		// re-points the Fs at the parent directory and reports it, with
		// a connection already opened and sitting in that Fs's pool.
		//
		// Dropping it here is the one leak "release on the way out of
		// each operation" does not catch, because there is no way out to
		// release on: no caller ever sees this Fs, so no caller ever
		// defers a shutdown for it. It is also the worst kind, since
		// nothing recovers it short of a restart if rclone's own drainer
		// is not running.
		//
		// Ordinary misconfiguration reaches it. config.Validate asks only
		// that remote_path be a non-empty absolute path, so pointing it
		// at a file is accepted, and the daemon then repeats it every
		// poll interval for as long as it runs.
		if f != nil {
			shutdownFs(ctx, f)
		}
		return nil, fmt.Errorf("source %q: %w", src.ID, err)
	}
	return f, nil
}

// oneConnectionAtATime returns a context whose rclone configuration keeps
// a single operation to a single SFTP connection.
//
// An Fs is not a connection, it is a pool, and two of rclone's own
// settings decide how wide one operation opens that pool. Both default
// above one, and both were measured against the real fixture in
// connections_gate_test.go:
//
//   - Checkers (8 by default) is how many goroutines walk a tree that the
//     backend cannot list recursively, and sftp has no ListR, so a plain
//     recursive List over 24 subdirectories opened 8 connections. A
//     directory per producer run (gitea-runs/<RUN_ID>/*.dump) is a normal
//     FR-8 layout, so this is the ordinary case, not a deep-tree edge.
//   - MultiThreadStreams (4 by default) is how many concurrent readers
//     rclone splits a download across once the file is bigger than
//     --multi-thread-cutoff (256Mi). Multi-GB database dumps are exactly
//     what this manager fetches, so the default path for its own subject
//     matter is four connections for one copy.
//
// Against a host that queues the surplus that is invisible. Against one
// that rejects it, which is what #264 is about, it is a failed backup that
// names nothing an operator can act on. Bounding both here means the
// number of connections a source sees is a property of this adapter, not
// something an operator has to discover and then defend against with a
// per-remote ceiling.
//
// The throughput this gives up is small and worth naming rather than
// hand-waving: sftpConfig pins concurrency at 64, which is 64 requests in
// flight inside the one connection, so a single stream is not a single
// request. What is given up is the parallelism ACROSS connections, and for
// a backup manager pulling one artifact at a time from a hardened host
// that parallelism was never the point.
//
// fs.AddConfig copies the caller's ConfigInfo rather than replacing it, so
// everything else the caller configured (a bandwidth limit, in particular)
// survives untouched. That matters more than it looks: rclone captures the
// ambient ConfigInfo into an Fs at construction, so one caller's settings
// reaching another operation would be permanent for that Fs's whole life.
// TestOneConnectionAtATimeLeavesEverythingElseAlone is what holds it.
//
// It also caps ConnectTimeout, for a reason that has nothing to do with
// connection counts but everything to do with being the one place every
// operation passes through. See ConnectTimeout.
func oneConnectionAtATime(ctx context.Context) context.Context {
	ctx, ci := fs.AddConfig(ctx)
	ci.Checkers = 1
	ci.MultiThreadStreams = 1
	if ci.ConnectTimeout <= 0 || ci.ConnectTimeout > fs.Duration(ConnectTimeout) {
		ci.ConnectTimeout = fs.Duration(ConnectTimeout)
	}
	return ctx
}

// ConnectTimeout is the ceiling this adapter puts on rclone's own
// --contimeout, which is the deadline both of rclone's dials are built
// with: fs/fshttp's NewDialer sets net.Dialer.Timeout from it, and
// backend/sftp sets ssh.ClientConfig.Timeout from the same value.
//
// # Why a ceiling exists at all
//
// rclone's default is 60 seconds, and app.DefaultRetryPolicy allows six
// attempts. Since issue #388 correctly reclassified a connect timeout
// rclone imposed on itself as Transient rather than as a cancellation
// nobody asked for, those six attempts are all really spent, so a source
// that blackholes costs six dials plus backoff before a cycle reports
// FAILED. At rclone's 60s that is about six and a half minutes, against
// the "a little over two minutes" DefaultRetryPolicy's own doc claims and
// used to hold. Issue #415 is that gap: the bound still holds in the sense
// that matters for safety, because the budget is per set and cycles are
// sequential, but a doc describing a bound the code no longer keeps is the
// same defect class as a comment describing a pragma that had stopped
// being true.
//
// # Where fifteen seconds comes from
//
// From both ends, and neither of them is a preference.
//
// The ceiling is the budget the doc already claims. Six attempts and this
// policy's backoff caps (1s, 2s, 4s, 8s, 16s) put at most 31 seconds of
// waiting between them, so the dialling has to fit inside roughly 89
// seconds for the whole thing to land near two minutes: (120s - 31s) / 6
// is 14.8s per attempt. Fifteen gives a worst case of 2m1s, which is what
// "a little over two minutes" means, and app.DefaultRetryBudget is the
// test-pinned arithmetic rather than this sentence.
//
// The floor is a measured handshake.
// TestConnectTimeoutLeavesARealHandshakeRoom dials the real Docker sshd
// fixture through this exact code path and reports what a legitimate
// connect actually costs on the host running it, so the margin between a
// real connect and this ceiling is a number in the run's own output rather
// than an assurance here. That fixture is loopback Docker, so it
// establishes a floor and not a WAN worst case; what makes fifteen seconds
// safe for a slow link is the size of the margin over it, which that test
// prints and asserts.
//
// # Why it is a ceiling rather than an assignment
//
// A caller that has already asked for LESS keeps what it asked for. That
// is what makes this a bound rather than a policy: errors_test.go's
// connect-timeout evidence runs at 500ms so that six real dials into a
// blackhole cost three seconds instead of ninety, and a bound that
// overwrote it would make its own proof unaffordable.
const ConnectTimeout = 15 * time.Second

// shutdownFs releases the backend resources an Fs holds, which for sftp
// means its pool of open SSH connections (#264).
//
// Every operation in this file builds its own Fs, and before this existed
// nothing ever released one, so the connections a cycle held open grew with
// the number of operations it performed and were bounded by nothing at all.
// Against a host that queues connections that is merely wasteful. Against
// one that rejects them it is a failed backup: both production sources this
// manager pulls from reject a third simultaneous connection from one
// address with a TCP reset, so listing succeeded on the first pool and the
// transfer's pool was refused before it sent a byte.
//
// Releasing after each operation rather than caching the Fs is deliberate.
// rclone captures the ambient ConfigInfo into an Fs at construction and
// never re-reads it, so a cached Fs would silently apply the first caller's
// settings, a bandwidth limit above all, to every later one for as long as
// it lived. Nothing tests that directly, because there is no cache to test:
// the discipline is what makes the question unaskable, and a backup tool
// that quietly transfers at somebody else's limit is a worse outcome than a
// connection setup per operation, which is what main was already paying
// anyway (see this function's caller doc and #355: main opened a fresh pool
// per operation too, it just never closed one).
//
// The error is swallowed for a future backend rather than for this one.
// sftp's Shutdown is `return f.drainPool(ctx)` and drainPool has no path
// that returns non-nil, so today there is no error here to swallow; the
// `_ =` is what keeps that true when a backend that does report one is
// added, since a failure to hang up cleanly, after the operation has
// already produced its answer, must not turn a good backup into a reported
// failure.
//
// What drainPool CAN do is decline: it returns nil early, closing nothing,
// while f.getSessions() != 0. That would leak a pool while reporting
// success, and the reason it does not is sftpConfig's idle_timeout, which
// leaves rclone's own drain timer running to finish the job (see the
// comment there, and #355's finding that with idle_timeout unset that
// timer is never created at all).
func shutdownFs(ctx context.Context, f fs.Fs) {
	if do, ok := f.(fs.Shutdowner); ok {
		_ = do.Shutdown(ctx)
	}
}

func toArtifact(o fs.Object) transport.RemoteArtifact {
	return transport.RemoteArtifact{
		Path:    o.Remote(),
		Size:    o.Size(),
		ModTime: o.ModTime(context.Background()).Unix(),
	}
}

// List recurses the whole tree beneath src's root, not just the top
// directory.
//
// It used to call f.List(ctx, ""), which lists exactly one directory level.
// A producer that writes one directory per run, for example
// gitea-runs/<RUN_ID>/*.dump, is a normal FR-8 layout, not an edge case, and
// under the old call every artifact placed in a subdirectory went missing
// from List with no error at all: Stat and CopyToLocal could still reach it
// by path, so the gap was invisible until something went looking for a
// backup that discovery had silently never seen. That is exactly the
// protection-dies-quietly failure this project exists to prevent, so it is
// not something a caller can be trusted to notice and route around: List
// itself has to stop lying about what exists.
//
// The fix is unconditional, full recursion rather than a configurable depth
// knob, because there is nothing here for a depth setting to mean: FR-5's
// per-backup-set include patterns (config.BackupSet.Include) are validated
// as filename patterns, never path patterns (see
// internal/config/validate.go's rejection of any "/" in an include
// pattern), so they already only ever match a candidate's basename
// regardless of how deep it sits. A "recurse N levels" option would filter
// something the configuration has no way to describe in the first place.
// Unconditional recursion is therefore the reading that is actually
// consistent with the configuration surface that exists today, not an
// implicit behaviour bolted on beside it. internal/discovery is where a
// basename that turns up at two different depths (and therefore collides as
// one model.ArtifactID) gets handled explicitly, loudly, and by name.
//
// walk.GetAll with maxLevel -1 is rclone's own idiom for this (see its
// fstest package): it uses the backend's native recursive listing when one
// is available and falls back to walking directory by directory otherwise,
// so this works identically against local and sftp. includeAll=true is
// passed explicitly because this adapter never configures an rclone filter
// for a caller to accidentally rely on; making that explicit here is
// cheaper than leaving the answer to whatever ambient default happens to be
// in effect.
//
// Full recursion is also where a plain listing turns into a fan-out of
// connections, because sftp has no native recursive listing and rclone
// walks what it cannot list recursively with one goroutine per --checkers.
// oneConnectionAtATime below is what bounds that; see its doc for the
// measurement.
func (a *Adapter) List(ctx context.Context, src transport.Source) ([]transport.RemoteArtifact, error) {
	ctx = oneConnectionAtATime(ctx)
	f, err := a.fsFor(ctx, src)
	if err != nil {
		return nil, WrapCtx(ctx, "list", err)
	}
	defer shutdownFs(ctx, f)
	objs, _, err := walk.GetAll(ctx, f, "", true, -1)
	if err != nil {
		return nil, WrapCtx(ctx, "list", err)
	}
	out := make([]transport.RemoteArtifact, 0, len(objs))
	for _, o := range objs {
		out = append(out, toArtifact(o))
	}

	// Sort by remote path so listing is deterministic.
	//
	// walk.GetAll returns whatever order the backend produced, which for the
	// local backend is directory-read order and is not stable between runs.
	// That does not matter while every artifact is independent, but it stops
	// being harmless the moment two remote paths share a basename, because
	// model.ArtifactID identifies an artifact by basename alone. Two run
	// directories that both contain backup.dump collide as one identity, and
	// with an unordered listing the winner is whichever the backend happened
	// to yield first. That makes ingestion a coin flip: one cycle takes
	// run-1's copy and reports run-2 as a conflict, the next cycle does the
	// reverse, and neither is reliably backed up.
	//
	// Sorting does not fix the collision, it makes the outcome repeatable,
	// which is the difference between a conflict an operator can reason about
	// and a nondeterministic one they cannot. The real fix is for an artifact
	// identity to carry more than a basename, which is a change to
	// model.ArtifactID and to everything keyed on it.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Stat resolves one object and, unlike List, asks the backend for the
// strongest identity attributes it can supply.
//
// WHY STAT HASHES AND LIST DOES NOT. Stat is the pre-delete recheck path.
// FR-16 compares the identity captured at discovery against the identity
// observed now, and model.CompareIdentity can only reach ConfidenceStrong on
// a hash or a backend-supplied stable id. Without one of those it returns
// Unconfirmed at Weak confidence, Preserve() is true, and FR-15 refuses the
// delete. Stat returning only path, size and mtime therefore did not merely
// weaken the check, it made a successful delete unreachable on every backend,
// including ones that hash perfectly well.
//
// List deliberately does not do this. It runs over every object under a
// source root, and on sftp rclone computes a hash by running a command on the
// server, so hashing during a listing would be one round trip per artifact
// for an attribute discovery does not need yet.
//
// A backend that cannot hash still returns an empty hash here, which is the
// correct outcome rather than a failure: a hardened shell-less sftp account
// genuinely cannot answer, so the comparison stays Weak and the delete stays
// refused, which is what FR-16 asks for.
func (a *Adapter) Stat(ctx context.Context, src transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	ctx = oneConnectionAtATime(ctx)
	f, err := a.fsFor(ctx, src)
	if err != nil {
		return transport.RemoteArtifact{}, WrapCtx(ctx, "stat", err)
	}
	defer shutdownFs(ctx, f)
	o, err := f.NewObject(ctx, remotePath)
	if err != nil {
		return transport.RemoteArtifact{}, WrapCtx(ctx, "stat", err)
	}

	art := toArtifact(o)

	if f.Hashes().Contains(hash.SHA256) {
		// A hash failure is not fatal here. The artifact is still usable with
		// weaker identity, and reporting "I could not hash" as "stat failed"
		// would turn a degraded check into an outage.
		if h, herr := o.Hash(ctx, hash.SHA256); herr == nil && h != "" {
			art.Hash = h
			art.HashAlg = transport.SHA256
		}
	}
	if ider, ok := o.(fs.IDer); ok {
		art.ID = ider.ID()
	}
	return art, nil
}

func (a *Adapter) CopyToLocal(ctx context.Context, src transport.Source, remotePath, localPartialPath string) (transport.TransferResult, error) {
	ctx = oneConnectionAtATime(ctx)
	srcFs, err := a.fsFor(ctx, src)
	if err != nil {
		return transport.TransferResult{}, WrapCtx(ctx, "copy_to_local", err)
	}
	defer shutdownFs(ctx, srcFs)
	o, err := srcFs.NewObject(ctx, remotePath)
	if err != nil {
		return transport.TransferResult{}, WrapCtx(ctx, "copy_to_local", err)
	}
	dstDir, dstName := splitPath(localPartialPath)
	dstFs, err := fs.NewFs(ctx, dstDir)
	if err != nil {
		return transport.TransferResult{}, WrapCtx(ctx, "copy_to_local", err)
	}
	// Copy, never Move. The remote source is deleted later, by the lifecycle
	// manager, and only after a durable commit (FR-11, FR-15).
	//
	// copyWithProgress is a wrapper, not a second code path: with no
	// transport.ProgressReporter on ctx (the ordinary case, and every
	// caller before issue #221) it calls the closure and nothing else
	// happens. o.Size() is the only total that can honestly be reported
	// for this copy, and it is read here, from the object the copy is
	// actually about, rather than guessed downstream.
	var dst fs.Object
	if err := copyWithProgress(ctx, o.Size(), func(ctx context.Context) error {
		var copyErr error
		dst, copyErr = operations.Copy(ctx, dstFs, nil, dstName, o)
		return copyErr
	}); err != nil {
		return transport.TransferResult{}, WrapCtx(ctx, "copy_to_local", err)
	}
	return transport.TransferResult{BytesTransferred: dst.Size()}, nil
}

func (a *Adapter) RemoteHash(ctx context.Context, src transport.Source, remotePath string, alg transport.HashAlgorithm) (string, error) {
	ctx = oneConnectionAtATime(ctx)
	f, err := a.fsForHashing(ctx, src)
	if err != nil {
		return "", WrapCtx(ctx, "remote_hash", err)
	}
	defer shutdownFs(ctx, f)
	o, err := f.NewObject(ctx, remotePath)
	if err != nil {
		return "", WrapCtx(ctx, "remote_hash", err)
	}
	var ht hash.Type
	switch alg {
	case transport.SHA256:
		ht = hash.SHA256
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedHash, alg)
	}
	// An unsupported remote hash must surface as an explicit capability result,
	// never as a silent downgrade of configured verification (FR-13).
	if !f.Hashes().Contains(ht) {
		return "", fmt.Errorf("%w: backend %q cannot compute %s", ErrUnsupportedHash, src.Type, alg)
	}

	sum, err := o.Hash(ctx, ht)
	if err != nil {
		// The Hashes() guard above stopped being the whole answer for sftp
		// once fsForHashing pinned sha256sum_command: pinning it is what
		// makes rclone skip its own (broken, see withSHA256) probe, and
		// skipping the probe means Hashes() reports SHA-256 without ever
		// having established that the account can run the command. So for
		// sftp the capability question is now settled here, by running it,
		// and a failure to run it is the same fact the guard above reports:
		// this account cannot compute this hash.
		//
		// Joining the sentinel keeps that fact classifiable. classify is
		// sentinel-based on purpose (see its doc), and without this a
		// shell-less account's refusal would land in Permanent, which is
		// the label for "we do not know what this was". The message keeps
		// rclone's own text, which names the command that failed and is
		// more use than "cannot compute sha256" was.
		//
		// The cost is fidelity in one direction: an ssh session that dies
		// mid-hash is also called a capability absence. Nothing downstream
		// can tell the difference today (verify.go fails the artifact on
		// any RemoteHash error, and neither category is retried), and the
		// alternative is a string match on rclone's error text, which this
		// package deliberately does not do.
		if src.Type == "sftp" {
			return "", fmt.Errorf("%w: %w", ErrUnsupportedHash, err)
		}
		return "", err
	}
	return sum, nil
}

func (a *Adapter) DeleteRemote(ctx context.Context, src transport.Source, remotePath string) error {
	ctx = oneConnectionAtATime(ctx)
	f, err := a.fsFor(ctx, src)
	if err != nil {
		return WrapCtx(ctx, "delete_remote", err)
	}
	defer shutdownFs(ctx, f)
	o, err := f.NewObject(ctx, remotePath)
	if err != nil {
		return WrapCtx(ctx, "delete_remote", err)
	}
	return WrapCtx(ctx, "delete_remote", o.Remove(ctx))
}

func splitPath(p string) (dir, name string) {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i], p[i+1:]
		}
	}
	return ".", p
}

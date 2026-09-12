package sourceconsistency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/backupdproject/backupd/core/internal/model"
)

// DigestAlgorithm is the content hash a capture records. It is named so the
// catalogue can store the algorithm beside the digest, the same way
// model.RemoteIdentity stores HashAlg beside Hash: a digest whose algorithm
// is unknown cannot be compared with anything later.
const DigestAlgorithm = "sha256"

// DefaultMaxAttempts is how many times a capture will re-read a file that
// moved under it before giving up and reporting the run incomplete.
//
// Three, because the bound is the point. One attempt cannot distinguish "a
// writer happened to touch this file once" from "this file is being written
// continuously", and an unbounded retry on a source that is always moving -
// an active log, a busy mailbox - is a run that never ends. Two retries is
// enough to settle the overwhelmingly common case (a single publish landing
// during the read) and small enough that a genuinely busy file is reported
// promptly rather than after minutes of doomed re-reading.
const DefaultMaxAttempts = 3

// defaultChunkSize is the read size. Large enough that the syscall overhead
// is irrelevant beside the hashing, small enough not to matter to a
// concurrent run's memory.
const defaultChunkSize = 128 << 10

// minChunkSize is the floor bufferFor will not go below, so a tree of tiny
// files does not pay an allocation per file that is smaller than the
// allocator's own granularity anyway.
const minChunkSize = 4 << 10

// Outcome is how a capture ended. The set is closed and every member is
// either verified or not; there is deliberately no "probably fine".
type Outcome string

const (
	// OutcomeStable means nothing moved: the file was read once and its
	// metadata was identical before and after.
	OutcomeStable Outcome = "stable"

	// OutcomeRetried means something moved and a later attempt, within the
	// bound, saw a file that held still. The recorded digest is the settled
	// content, never the torn read that triggered the retry.
	OutcomeRetried Outcome = "retried"

	// OutcomeIncomplete means the file could not be captured coherently
	// within the retry bound, or could not be read at all. It is not
	// verified, and a run holding one is not complete.
	OutcomeIncomplete Outcome = "incomplete"

	// OutcomeVanished means the path no longer names anything. In a live
	// tree this is ordinary; it is still not a capture, so the run says so
	// rather than quietly containing one fewer file than the operator
	// selected.
	OutcomeVanished Outcome = "vanished"
)

// Capture is one file's result.
type Capture struct {
	Path string

	// Size and Digest describe the bytes actually read, not the bytes the
	// listing promised. On a verified capture the two agree by construction;
	// keeping the read's own count is what makes the disagreement detectable
	// in the first place.
	Size      int64
	Digest    string
	DigestAlg string

	// ModTimeNanos is the modification time that was true across the whole
	// read window, which is the only modification time worth storing: the
	// pre-read value is a value the file may no longer have.
	ModTimeNanos int64

	Outcome  Outcome
	Attempts int

	// Reason says what happened, in a sentence an operator can act on. It is
	// populated for every outcome including the successful ones, because a
	// retried capture is a fact about the source worth reading even when it
	// settled.
	Reason string
}

// Verified reports whether these bytes were proven coherent. Only the two
// settled outcomes qualify. Callers must not store an unverified capture as
// content; that is the single rule this package exists to enforce.
func (c Capture) Verified() bool {
	return c.Outcome == OutcomeStable || c.Outcome == OutcomeRetried
}

// movedDuringRead reports whether this capture saw the source move. It is
// true for a settled retry as well as for the failures, because a run under
// a mode that promised the source could not move needs to hear about a
// mutation that was successfully worked around just as much as one that was
// not.
func (c Capture) movedDuringRead() bool {
	return c.Attempts > 1 || c.Outcome == OutcomeIncomplete || c.Outcome == OutcomeVanished
}

// Reader captures files from a Source, bounding its own read window.
type Reader struct {
	Source Source

	// MaxAttempts bounds the retry. Zero means DefaultMaxAttempts; there is
	// no value meaning "unlimited".
	MaxAttempts int

	// ChunkSize is the read size, zero meaning defaultChunkSize.
	ChunkSize int

	// ConfirmDigest reads each file a second time and compares digests.
	//
	// This is the only defence against a mutation whose metadata was put
	// back - the same length, the modification time restored - which is
	// invisible to every stat-based check by construction. It doubles the
	// read bandwidth of a run, so it is not on by default; ConfirmReadRequired
	// is where the decision is made from the mode and the policy, and
	// NewReader applies it.
	ConfirmDigest bool
}

// ConfirmReadRequired reports whether a second read buys anything for this
// combination of source arrangement and content policy.
//
// It does not under a mode that guarantees a point in time: a frozen image
// cannot change while it is read, so the second read would be a full extra
// pass over the source for no information. It does not under a policy that
// permits metadata to skip content either, and that is the honest shape of
// that trade rather than an oversight: an operator who chose the strong
// preset on a weak source has already said they would rather not re-read
// content, and re-reading it TWICE for the files that are read would be the
// opposite of what they asked for. Under an always-verify policy the first
// read is already happening, so the second is the cheapest correctness this
// package can offer.
func ConfirmReadRequired(mode model.ConsistencyMode, pol model.VerificationPolicy) bool {
	return !mode.GuaranteesPointInTime() && !pol.MetadataMaySkipContent()
}

// NewReader builds the reader a run should use, so no call site has to
// remember the relationship between the mode, the policy and the confirm
// read.
func NewReader(src Source, mode model.ConsistencyMode, pol model.VerificationPolicy) Reader {
	return Reader{
		Source:        src,
		MaxAttempts:   DefaultMaxAttempts,
		ChunkSize:     defaultChunkSize,
		ConfirmDigest: ConfirmReadRequired(mode, pol),
	}
}

func (r Reader) maxAttempts() int {
	if r.MaxAttempts > 0 {
		return r.MaxAttempts
	}

	return DefaultMaxAttempts
}

func (r Reader) chunkSize() int {
	if r.ChunkSize > 0 {
		return r.ChunkSize
	}

	return defaultChunkSize
}

// bufferFor sizes the read buffer for a file the stat says is this long.
//
// It exists because measuring it mattered: a fixed defaultChunkSize buffer
// per file costs 128 KiB of allocation whether the file is a gigabyte or
// ten bytes, and a source tree is mostly small files. Over 20,000 ten-byte
// files that was 2.5 GiB of garbage and it dominated the whole capture,
// nearly tripling the per-file cost (the measurements are in
// docs/adr/0009).
//
// The hint is only a hint: it comes from a stat that may already be stale,
// which is the entire premise of this package. A buffer smaller than the
// file simply means more iterations of a loop that reads to EOF, so a file
// that grew between the stat and the read is still read in full.
func (r Reader) bufferFor(hint int64) []byte {
	size := r.chunkSize()

	if hint >= 0 && hint < int64(size) {
		// One byte over, so a file whose length the stat got exactly right
		// still takes one Read to reach EOF rather than two.
		if wanted := int(hint) + 1; wanted > minChunkSize {
			size = wanted
		} else {
			size = minChunkSize
		}
	}

	return make([]byte, size)
}

// Capture reads one file and reports what was proven about it.
//
// It never returns an error, for the reason sourcecheck.Run gives about its
// own report: every way this can go wrong is a fact about the source, which
// is an ORDINARY OUTCOME and belongs on the result. A Go error here would be
// a claim that this manager broke, and a file being deleted mid-scan is not
// that.
func (r Reader) Capture(ctx context.Context, path string) Capture {
	attempts := r.maxAttempts()

	var lastMovement string

	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Capture{
				Path:      path,
				DigestAlg: DigestAlgorithm,
				Outcome:   OutcomeIncomplete,
				Attempts:  attempt - 1,
				Reason:    "the run stopped before this file was captured: " + err.Error(),
			}
		}

		got, movement, err := r.attempt(ctx, path)

		switch {
		case errors.Is(err, fs.ErrNotExist):
			return Capture{
				Path:      path,
				DigestAlg: DigestAlgorithm,
				Outcome:   OutcomeVanished,
				Attempts:  attempt,
				Reason:    "the path no longer names a file, so nothing was captured for it",
			}

		case err != nil:
			return Capture{
				Path:      path,
				DigestAlg: DigestAlgorithm,
				Outcome:   OutcomeIncomplete,
				Attempts:  attempt,
				Reason:    "the file could not be read: " + err.Error(),
			}

		case movement != "":
			lastMovement = movement
			continue
		}

		got.Attempts = attempt
		if attempt == 1 {
			got.Outcome = OutcomeStable
			got.Reason = "the file held still across its own read"
		} else {
			got.Outcome = OutcomeRetried
			got.Reason = fmt.Sprintf("captured on attempt %d; the previous attempt saw that %s", attempt, lastMovement)
		}

		return got
	}

	return Capture{
		Path:      path,
		DigestAlg: DigestAlgorithm,
		Outcome:   OutcomeIncomplete,
		Attempts:  attempts,
		Reason: fmt.Sprintf("the file changed on every one of %d attempts, most recently because %s; no coherent copy of it is in this run",
			attempts, lastMovement),
	}
}

// attempt performs one read and reports either a capture, or a sentence
// saying what moved, or an error. Exactly one of the three is meaningful.
//
// The checks are in the order they can fail, and each one is checking
// something the others cannot see:
//
//  1. a stat before the read, so there is something to compare against;
//  2. an fstat on the OPEN DESCRIPTOR after it, which answers about the
//     object the bytes came from even if the path has since been replaced;
//  3. the byte count against that fstat's size, which catches a read that
//     ended early or ran long while the metadata happened to agree;
//  4. a stat of the PATH, which is the only check that sees a rename into
//     place or a delete, both of which leave the descriptor perfectly
//     readable;
//  5. optionally a second full read, which is the only check that sees a
//     mutation whose metadata was restored.
func (r Reader) attempt(ctx context.Context, path string) (Capture, string, error) {
	before, err := r.Source.Stat(path)
	if err != nil {
		return Capture{}, "", err
	}

	digest, read, after, err := r.readOnce(ctx, path, before.Size)
	if err != nil {
		return Capture{}, "", err
	}

	if moved := describeMovement(before, after); moved != "" {
		return Capture{}, moved, nil
	}

	if read != after.Size {
		return Capture{}, fmt.Sprintf("the read produced %d bytes where the file reports %d, so the file changed length while it was being read", read, after.Size), nil
	}

	current, err := r.Source.Stat(path)
	if err != nil {
		return Capture{}, "", err
	}

	if moved := describeMovement(after, current); moved != "" {
		return Capture{}, moved, nil
	}

	if r.ConfirmDigest {
		confirmed, _, _, err := r.readOnce(ctx, path, after.Size)
		if err != nil {
			return Capture{}, "", err
		}

		if confirmed != digest {
			return Capture{}, "the file read twice in this run produced two different digests while its size and modification time never moved, which is an in-place rewrite with a restored timestamp", nil
		}
	}

	return Capture{
		Path:         path,
		Size:         read,
		Digest:       digest,
		DigestAlg:    DigestAlgorithm,
		ModTimeNanos: after.ModTimeNanos,
	}, "", nil
}

// readOnce reads a file to the end, hashing as it goes, and returns the
// digest, the byte count, and the stat of the descriptor the bytes came
// from. The fstat is taken here, while the handle is still open, because
// after the Close there is nothing left to ask.
func (r Reader) readOnce(ctx context.Context, path string, sizeHint int64) (digest string, read int64, after Stat, err error) {
	f, err := r.Source.Open(path)
	if err != nil {
		return "", 0, Stat{}, err
	}
	defer f.Close() //nolint:errcheck // a read-only handle's Close reports nothing this decision depends on

	h := sha256.New()
	buf := r.bufferFor(sizeHint)

	for {
		if err := ctx.Err(); err != nil {
			return "", 0, Stat{}, err
		}

		n, readErr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			read += int64(n)
		}

		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", 0, Stat{}, readErr
		}
	}

	after, err = f.Stat()
	if err != nil {
		return "", 0, Stat{}, err
	}

	return hex.EncodeToString(h.Sum(nil)), read, after, nil
}

// Run accumulates one pass over a source and answers the two questions a
// run report has to answer separately: is this run complete, and did the
// source behave the way the operator said it would.
//
// The separation matters. A quiesce hook that stopped the wrong container
// produces a run that is COMPLETE - every file was captured coherently -
// and an operator belief that is false. Folding that into "incomplete"
// would either cry wolf or hide it, depending on which way the fold went.
type Run struct {
	Mode   model.ConsistencyMode
	Trust  model.TrustClass
	Policy model.VerificationPolicy

	captures   []Capture
	incomplete []string
	violations []string
}

// Record files one capture. It is the only way into a Run, so the
// completeness and violation bookkeeping cannot be bypassed by appending to
// a slice.
func (r *Run) Record(c Capture) {
	r.captures = append(r.captures, c)

	if !c.Verified() {
		r.incomplete = append(r.incomplete, fmt.Sprintf("%s: %s", c.Path, c.Reason))
	}

	if r.Mode.MutationIsContractViolation() && c.movedDuringRead() {
		r.violations = append(r.violations, fmt.Sprintf("%s changed during the run, but this source is declared %s: %s", c.Path, r.Mode, c.Reason))
	}
}

// Captures returns every capture recorded, verified or not.
func (r *Run) Captures() []Capture { return r.captures }

// Complete reports whether every file this run touched was captured
// coherently. A single unverified capture makes it false: a run that is
// missing a file, or holding a file it could not prove, is not a restore
// point anybody should be told is whole.
func (r *Run) Complete() bool { return len(r.incomplete) == 0 }

// IncompleteReasons is one sentence per file that was not captured, which
// is what a run report lists and what an operator reads to decide whether
// to care.
func (r *Run) IncompleteReasons() []string { return r.incomplete }

// ContractViolations is one sentence per file that moved under a mode which
// said it could not. Empty under ModeLiveBestEffort by construction.
func (r *Run) ContractViolations() []string { return r.violations }

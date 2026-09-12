package sourceconsistency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"

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

// minChunkSize is the floor bufferSize will not go below, so a tree of tiny
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

	// OutcomeIncomplete means the source kept moving: the file could not be
	// captured coherently within the retry bound. It is not verified, a run
	// holding one is not complete, and under a mode that promised the source
	// could not move it is also a contract violation.
	OutcomeIncomplete Outcome = "incomplete"

	// OutcomeUnreadable means the source did not answer: an open or a read
	// or a stat failed, the source would not say what is at the path, or
	// this run was cancelled before the file was captured.
	//
	// It is separate from OutcomeIncomplete because the two are different
	// facts with different remedies, and folding them together made a
	// permission error look like a busy file. An unreadable path says
	// NOTHING about whether the source moved, so it is not reported as a
	// contract violation under a quiesced or snapshot mode: an EACCES is not
	// evidence that somebody wrote to the source.
	OutcomeUnreadable Outcome = "unreadable"

	// OutcomeVanished means the path no longer names anything. In a live
	// tree this is ordinary; it is still not a capture, so the run says so
	// rather than quietly containing one fewer file than the operator
	// selected.
	OutcomeVanished Outcome = "vanished"

	// OutcomeNotAFile means the path names something this reader does not
	// read for content: a symlink, a directory, a socket, a device. Kind
	// says which.
	//
	// It is not a failure and it does not make a run incomplete. Whether a
	// symlink is stored, followed or skipped is the backend capability
	// matrix's symlink_semantics decision (see doc.go), so the honest answer
	// from a content reader is to name what is there and leave the decision
	// where it belongs. Reporting a symlink as incomplete - which is what
	// reading through it and then comparing an lstat with an fstat used to
	// do - cried wolf on every link in the tree.
	OutcomeNotAFile Outcome = "not_a_file"
)

// Capture is one file's result.
type Capture struct {
	Path string

	// Kind is what the source said was at the path. It is on every capture,
	// not only on the OutcomeNotAFile ones, so a caller routing a tree does
	// not have to infer it from the outcome.
	Kind Kind

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

// contentIsMissing reports whether this capture leaves a hole in the run:
// the path was selected, it has content, and this run does not hold a proven
// copy of it.
//
// OutcomeNotAFile is the one unverified outcome that is not a hole. There
// was no content to capture, so nothing is missing.
func (c Capture) contentIsMissing() bool {
	switch c.Outcome {
	case OutcomeStable, OutcomeRetried, OutcomeNotAFile:
		return false
	default:
		return true
	}
}

// movedDuringRead reports whether this capture is EVIDENCE that the source
// moved. It is true for a settled retry as well as for the failures that
// were caused by movement, because a run under a mode that promised the
// source could not move needs to hear about a mutation that was
// successfully worked around just as much as one that was not.
//
// It is false for OutcomeUnreadable and OutcomeNotAFile, which are the
// outcomes that carry no information about movement at all.
func (c Capture) movedDuringRead() bool {
	switch c.Outcome {
	case OutcomeRetried, OutcomeIncomplete, OutcomeVanished:
		return true
	default:
		return false
	}
}

// Reader captures files from a Source, bounding its own read window.
//
// Its fields are unexported and set once by NewReader, because one of them
// is not a knob. Whether the bytes a run banks as verified were confirmed by
// a second read is a property of the source's consistency mode, and a
// caller that could set it per call site could bank a torn read as verified
// by forgetting to (see ConfirmReadRequired).
type Reader struct {
	source        Source
	maxAttempts   int
	chunkSize     int
	confirmDigest bool
}

// ReaderOptions are the bounds a caller may choose. Both are performance
// dials with safe defaults and neither changes what a capture PROVES, which
// is why they are the only two things here.
type ReaderOptions struct {
	// MaxAttempts bounds the retry; zero means DefaultMaxAttempts. There is
	// no value meaning "unlimited".
	MaxAttempts int

	// ChunkSize is the read size; zero means defaultChunkSize.
	ChunkSize int
}

// ConfirmReadRequired reports whether a path that is read must be read
// TWICE and its digests compared.
//
// The answer is yes everywhere except under a mode that guarantees a point
// in time: a frozen image cannot change while it is read, so a second pass
// would double the I/O of a run for no information.
//
// It deliberately does not depend on the verification policy. The policy
// decides WHICH paths are read - all of them, a deterministic sample, the
// ones whose backend evidence disagrees - and a path that policy decided to
// skip costs nothing here. But once a path IS read, the confirmation is not
// optional: the second read is the only check that sees an in-place rewrite
// whose size and modification time were restored, and banking such a read
// as verified is precisely the failure this package exists to prevent. An
// earlier version of this function also returned false under any policy
// that let metadata skip content, on the theory that an operator who asked
// to read less should not have the sampled 5% read twice. That reasoning
// confused the cost of a read with the trustworthiness of one: it made the
// sample - the only thing standing between a weak source and a silently
// stale restore point - unable to detect the mutation it exists to catch.
// The sample's price is now two reads per sampled path, which is the price
// of the sample meaning anything.
func ConfirmReadRequired(mode model.ConsistencyMode) bool {
	return !mode.GuaranteesPointInTime()
}

// NewReader builds the reader a run should use. It is the only way to get
// one: the confirm read is derived from the mode here so that no call site
// has to remember the relationship, and so that none of them can get it
// wrong.
func NewReader(src Source, mode model.ConsistencyMode, opts ReaderOptions) Reader {
	r := Reader{
		source:        src,
		maxAttempts:   DefaultMaxAttempts,
		chunkSize:     defaultChunkSize,
		confirmDigest: ConfirmReadRequired(mode),
	}

	if opts.MaxAttempts > 0 {
		r.maxAttempts = opts.MaxAttempts
	}
	if opts.ChunkSize > 0 {
		r.chunkSize = opts.ChunkSize
	}

	return r
}

// ConfirmsDigest reports whether this reader reads every path twice and
// compares digests. A run report says so, because it is the difference
// between "these bytes were coherent" and "these bytes were coherent and
// were not rewritten underneath us with the timestamp put back".
func (r Reader) ConfirmsDigest() bool { return r.confirmDigest }

// MaxAttempts is the retry bound this reader will not exceed.
func (r Reader) MaxAttempts() int { return r.maxAttempts }

// bufferSize sizes the read buffer for a file the stat says is this long.
//
// It exists because measuring it mattered: a fixed defaultChunkSize buffer
// per file costs 128 KiB of allocation whether the file is a gigabyte or
// ten bytes, and a source tree is mostly small files. Over 20,000 ten-byte
// files that was 2.5 GiB of garbage and it dominated the whole capture,
// nearly tripling the per-file cost (the measurements, and the command that
// reproduces them, are in docs/adr/0009).
//
// The hint is only a hint: it comes from a stat that may already be stale,
// which is the entire premise of this package. A buffer smaller than the
// file simply means more iterations of a loop that reads to EOF, so a file
// that grew between the stat and the read is still read in full.
func (r Reader) bufferSize(hint int64) int {
	size := r.chunkSize
	if size <= 0 {
		size = defaultChunkSize
	}

	if hint >= 0 && hint < int64(size) {
		// One byte over, so a file whose length the stat got exactly right
		// still takes one Read to reach EOF rather than two.
		if wanted := int(hint) + 1; wanted > minChunkSize {
			size = wanted
		} else {
			size = minChunkSize
		}
	}

	return size
}

// scratch hands back a buffer of the size this file wants, reusing the one
// the capture already has whenever it is big enough.
//
// One capture performs up to six reads - three attempts, doubled by the
// confirm read - all of the same file, and allocating per read allocated the
// same buffer six times. The buffer belongs to a single Capture call rather
// than to the Reader, because a Reader is a value that may be shared by
// concurrent captures and a buffer on it would be a data race.
func (r Reader) scratch(buf *[]byte, hint int64) []byte {
	want := r.bufferSize(hint)
	if cap(*buf) < want {
		*buf = make([]byte, want)
	}

	return (*buf)[:want]
}

// attemptOutcome says which field of an attemptResult is meaningful. It is
// an enumeration rather than a convention about nil-ness because the three
// cases are three different facts - the file settled, the source moved, the
// source would not answer - and a caller that confused the last two turned
// a permission error into a report that somebody was writing to the source.
type attemptOutcome int

const (
	// attemptSettled: capture holds a coherent read.
	attemptSettled attemptOutcome = iota

	// attemptMoved: movement holds a sentence about what changed under the
	// read. Retryable.
	attemptMoved

	// attemptFailed: err holds an I/O failure. Not retryable, and not
	// evidence of movement.
	attemptFailed

	// attemptNoContent: kind holds what the source says is at the path,
	// which is not a regular file.
	attemptNoContent
)

type attemptResult struct {
	outcome  attemptOutcome
	capture  Capture
	movement string
	kind     Kind
	err      error
}

// Capture reads one file and reports what was proven about it.
//
// It never returns an error, for the reason sourcecheck.Run gives about its
// own report: every way this can go wrong is a fact about the source, which
// is an ORDINARY OUTCOME and belongs on the result. A Go error here would be
// a claim that this manager broke, and a file being deleted mid-scan is not
// that.
func (r Reader) Capture(ctx context.Context, path string) Capture {
	attempts := r.maxAttempts
	if attempts <= 0 {
		attempts = DefaultMaxAttempts
	}

	var (
		buf          []byte
		lastMovement string
	)

	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Capture{
				Path:      path,
				DigestAlg: DigestAlgorithm,
				Outcome:   OutcomeUnreadable,
				Attempts:  attempt - 1,
				Reason:    "the run stopped before this file was captured: " + err.Error(),
			}
		}

		got := r.attempt(ctx, path, &buf)

		switch got.outcome {
		case attemptNoContent:
			return Capture{
				Path:      path,
				Kind:      got.kind,
				DigestAlg: DigestAlgorithm,
				Outcome:   OutcomeNotAFile,
				Attempts:  attempt,
				Reason:    fmt.Sprintf("the path names a %s, which has no content for this reader to capture", got.kind),
			}

		case attemptFailed:
			if errors.Is(got.err, fs.ErrNotExist) {
				return Capture{
					Path:      path,
					DigestAlg: DigestAlgorithm,
					Outcome:   OutcomeVanished,
					Attempts:  attempt,
					Reason:    "the path no longer names a file, so nothing was captured for it",
				}
			}

			return Capture{
				Path:      path,
				DigestAlg: DigestAlgorithm,
				Outcome:   OutcomeUnreadable,
				Attempts:  attempt,
				Reason:    "the source did not answer for this file: " + got.err.Error(),
			}

		case attemptMoved:
			lastMovement = got.movement

			continue
		}

		// Everything that reaches here is attemptSettled: a coherent read,
		// with the metadata that was true across the whole window.

		settled := got.capture
		settled.Attempts = attempt
		if attempt == 1 {
			settled.Outcome = OutcomeStable
			settled.Reason = "the file held still across its own read"
		} else {
			settled.Outcome = OutcomeRetried
			settled.Reason = fmt.Sprintf("captured on attempt %d; the previous attempt saw that %s", attempt, lastMovement)
		}

		return settled
	}

	return Capture{
		Path:      path,
		Kind:      KindRegular,
		DigestAlg: DigestAlgorithm,
		Outcome:   OutcomeIncomplete,
		Attempts:  attempts,
		Reason: fmt.Sprintf("the file changed on every one of %d attempts, most recently because %s; no coherent copy of it is in this run",
			attempts, lastMovement),
	}
}

// attempt performs one read and reports which of the four things happened.
//
// The checks are in the order they can fail, and each one is checking
// something the others cannot see:
//
//  1. a stat before the read, which says whether there is content here at
//     all and gives the comparison something to compare against;
//  2. an fstat on the OPEN DESCRIPTOR after it, which answers about the
//     object the bytes came from even if the path has since been replaced;
//  3. the byte count against that fstat's size, which catches a read that
//     ended early or ran long while the metadata happened to agree;
//  4. a stat of the PATH, which is the only check that sees a rename into
//     place or a delete, both of which leave the descriptor perfectly
//     readable;
//  5. where the mode calls for it, a second full read, which is the only
//     check that sees a mutation whose metadata was restored.
func (r Reader) attempt(ctx context.Context, path string, buf *[]byte) attemptResult {
	before, err := r.source.Stat(ctx, path)
	if err != nil {
		return attemptResult{outcome: attemptFailed, err: err}
	}

	switch before.Kind {
	case KindRegular:
	case KindDir, KindSymlink, KindOther:
		return attemptResult{outcome: attemptNoContent, kind: before.Kind}
	default:
		return attemptResult{outcome: attemptFailed, err: fmt.Errorf("the source did not say what kind of object is at %q, so this reader will not read it", path)}
	}

	digest, read, after, err := r.readOnce(ctx, path, buf, before.Size)
	if err != nil {
		return attemptResult{outcome: attemptFailed, err: err}
	}

	if moved := DescribeMovement(before, after); moved != "" {
		return attemptResult{outcome: attemptMoved, movement: moved}
	}

	if read != after.Size {
		return attemptResult{
			outcome:  attemptMoved,
			movement: fmt.Sprintf("the read produced %d bytes where the file reports %d, so the file changed length while it was being read", read, after.Size),
		}
	}

	current, err := r.source.Stat(ctx, path)
	if err != nil {
		return attemptResult{outcome: attemptFailed, err: err}
	}

	if moved := DescribeMovement(after, current); moved != "" {
		return attemptResult{outcome: attemptMoved, movement: moved}
	}

	if r.confirmDigest {
		confirmed, _, _, err := r.readOnce(ctx, path, buf, after.Size)
		if err != nil {
			return attemptResult{outcome: attemptFailed, err: err}
		}

		if confirmed != digest {
			return attemptResult{
				outcome:  attemptMoved,
				movement: "the file read twice in this run produced two different digests while its size and modification time never moved, which is an in-place rewrite with a restored timestamp",
			}
		}
	}

	return attemptResult{
		outcome: attemptSettled,
		capture: Capture{
			Path:         path,
			Kind:         after.Kind,
			Size:         read,
			Digest:       digest,
			DigestAlg:    DigestAlgorithm,
			ModTimeNanos: after.ModTimeNanos,
		},
	}
}

// readOnce reads a file to the end, hashing as it goes, and returns the
// digest, the byte count, and the stat of the descriptor the bytes came
// from. The fstat is taken here, while the handle is still open, because
// after the Close there is nothing left to ask.
func (r Reader) readOnce(ctx context.Context, path string, buf *[]byte, sizeHint int64) (digest string, read int64, after Stat, err error) {
	f, err := r.source.Open(ctx, path)
	if err != nil {
		return "", 0, Stat{}, err
	}
	defer f.Close() //nolint:errcheck // a read-only handle's Close reports nothing this decision depends on

	h := sha256.New()
	chunk := r.scratch(buf, sizeHint)

	for {
		// The context is handed to the read AND checked here. The first is
		// what abandons a read already in flight against a source that has
		// stopped answering; the second is this reader refusing to keep
		// asking, which does not depend on the source having implemented
		// its half of the contract correctly.
		if err := ctx.Err(); err != nil {
			return "", 0, Stat{}, err
		}

		n, readErr := f.Read(ctx, chunk)
		if n > 0 {
			h.Write(chunk[:n])
			read += int64(n)
		}

		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", 0, Stat{}, readErr
		}
	}

	after, err = f.Stat(ctx)
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
//
// Everything on it is unexported and every accessor hands back a copy. A
// run report is the record of what a run proved; a caller that could append
// to the slice it was handed, or move the mode after the captures were
// classified against it, could make that record say something the run never
// observed.
type Run struct {
	mode   model.ConsistencyMode
	trust  model.TrustClass
	policy model.VerificationPolicy

	captures   []Capture
	incomplete []string
	violations []string
}

// NewRun starts a run under a declared mode, a derived trust class and the
// policy that follows from them.
func NewRun(mode model.ConsistencyMode, trust model.TrustClass, policy model.VerificationPolicy) *Run {
	return &Run{mode: mode, trust: trust, policy: policy}
}

// Mode is the consistency mode the operator declared for this source.
func (r *Run) Mode() model.ConsistencyMode { return r.mode }

// Trust is the metadata-trust class derived for this source's backend.
func (r *Run) Trust() model.TrustClass { return r.trust }

// Policy is the content-verification policy this run applied.
func (r *Run) Policy() model.VerificationPolicy { return r.policy }

// Record files one capture. It is the only way into a Run, so the
// completeness and violation bookkeeping cannot be bypassed by appending to
// a slice.
func (r *Run) Record(c Capture) {
	r.captures = append(r.captures, c)

	if c.contentIsMissing() {
		r.incomplete = append(r.incomplete, fmt.Sprintf("%s: %s", c.Path, c.Reason))
	}

	if r.mode.MutationIsContractViolation() && c.movedDuringRead() {
		r.violations = append(r.violations, fmt.Sprintf("%s changed during the run, but this source is declared %s: %s", c.Path, r.mode, c.Reason))
	}
}

// Captures returns a copy of every capture recorded, verified or not.
func (r *Run) Captures() []Capture { return slices.Clone(r.captures) }

// Complete reports whether every file this run touched was captured
// coherently. A single missing file makes it false: a run that is missing a
// file, or holding a file it could not prove, is not a restore point
// anybody should be told is whole.
func (r *Run) Complete() bool { return len(r.incomplete) == 0 }

// IncompleteReasons is one sentence per file whose content this run does not
// hold a proven copy of, which is what a run report lists and what an
// operator reads to decide whether to care.
func (r *Run) IncompleteReasons() []string { return slices.Clone(r.incomplete) }

// ContractViolations is one sentence per file that moved under a mode which
// said it could not. Empty under ModeLiveBestEffort by construction.
func (r *Run) ContractViolations() []string { return slices.Clone(r.violations) }

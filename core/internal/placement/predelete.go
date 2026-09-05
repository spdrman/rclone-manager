package placement

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file is issue #439, and the thing it is careful about is not the
// saving.
//
// A move at content class read the whole artifact back twice. Once in
// verifyDestination, which is what produces VERIFIED and the destination's
// placements row, and once again in deleteSource, from scratch,
// immediately before the source delete. Both are full reads, both are
// billed as egress, and on an archive class both would need a restore. A
// 100 GB artifact cost 200 GB of egress to move, and the operator-facing
// cost table said "a full download", singular.
//
// # Why the second read is not simply removable
//
// engine.go's file comment makes the argument and it is a good one. The
// re-verification runs immediately before an irreversible act so that the
// act is authorised by a fact established NOW rather than by one
// established earlier and possibly stale, and it runs unconditionally so
// that a move interrupted by a crash and a move verified a microsecond ago
// take the same branch. #372 is open because a state machine grew a phase
// and the driver never got a case for it; "only re-read when we did not
// just read it ourselves" is a second path, and the crash suite would then
// be proving things about the cheap one.
//
// So the question this file answers is not "can the second read go". It is
// "what is the weakest thing that still authorises destroying the only
// other copy, and can the first read's result supply it without the
// authorisation becoming a lie".
//
// # What the second read actually defends, and for how long
//
// It defends a window: everything that could have happened to the
// destination between the read that produced VERIFIED and the delete. That
// window has two very different sizes.
//
// On the RESUME path it is however long the process was down, which is
// unbounded. A bucket lifecycle rule transitioned the object, a restore
// window expired, an operator overwrote the key, the endpoint lost it. The
// journal cannot know any of that, VERIFIED is a fact about a world nobody
// has looked at since, and the read is worth every byte it costs. Nothing
// in this file touches that path: a proof lives in one stack frame of one
// walk of one move (see advance), so a move picked up by a later cycle or
// a later process never has one and always reads.
//
// On the NOMINAL path it is two journal writes. The read that produced
// VERIFIED finished, AdvanceMove recorded it, intendSourceDelete recorded
// the intent, and deleteSource ran. Milliseconds. Paying one artifact's
// worth of egress to cover milliseconds is not a guarantee, because
// whatever the read is defending against is equally free to strike a
// millisecond LATER, when this engine is no longer looking at all and will
// not look again until the next revalidation pass. A defence with a
// millisecond of coverage out of an artifact's multi-year retention is a
// coincidence, not a property.
//
// # So the read is replaced by a proof with an age and an identity
//
// deleteSource still asks, unconditionally, at the same point, for a
// content-class verdict about the destination, and still feeds the answer
// into the same switch. What changed is where the verdict may come from.
// The read this process performed a moment ago stands in for a fresh one
// only when ALL of these hold:
//
//  1. PROVENANCE. It was produced by this engine, in this cycle, in this
//     walk of this move, by this engine's own content read. That is
//     structural rather than checked: readBackProof is a local in advance,
//     it is handed to verifyDestination and to deleteSource and to nothing
//     else, and deleteSource consumes it, so it cannot authorise two
//     deletes and cannot survive a return from advance. A resumed move
//     never has one. TestNothingHoldsAPreDeleteProofBeyondOneWalkOfOneMove
//     is what stops it becoming an Engine field, which is the one edit
//     that would quietly reintroduce the stale-fact problem.
//
//  2. SUBJECT. It names the same move, the same destination medium and
//     key, the same required class, and the same hash, hash algorithm and
//     size the delete is about, re-read from the journal at the delete
//     rather than remembered.
//
//  3. AGE. Its age on this engine's clock is between zero and
//     preDeleteProofValidity. A negative age is a clock that moved
//     backwards, which is not a young proof, it is an unreadable one.
//
//  4. CONTINUITY. The medium's own account of the object, taken
//     immediately BEFORE the read and again immediately before the delete,
//     is identical: same size, same mod time, same storage class. A medium
//     that will not answer, or that reports no mod time at all, gives the
//     check nothing to work with and there is no proof.
//
// Any one of those failing does exactly what the code did before: a full
// read, at the same point, feeding the same switch.
//
// # What that leaves an attacker or a corruption
//
// The identity is taken BEFORE the bytes are read, and that ordering is
// the whole of clause 4. A HEAD taken AFTER the read would describe an
// object that had already been overwritten while the GET was streaming the
// previous version, and the delete-time HEAD would agree with it, so the
// delete would be authorised by a hash of bytes that are no longer there.
// Taken before, an overwrite anywhere in [read, delete] moves the mod time
// off the value the proof carries and the proof is void; an overwrite
// BEFORE the read is served by the read itself and fails the hash.
//
// What survives is an overwrite that lands in the same second the endpoint
// stamped on this product's own upload, by a principal that can write to
// the destination, on an artifact small enough that the whole verify fits
// inside that second. S3 reports mod times in whole seconds, so that
// second is the resolution limit of the only continuity signal there is.
// There is no finer one available on purpose: FR-32 gives
// transport.ObjectInfo nowhere to carry a digest a medium volunteered,
// because a caller that can read one is a caller that will eventually
// compare it against a recorded hash. See transport/medium.go, which
// names the specific field it refuses to have.
//
// It is worth being plain about how much that residual is worth to an
// attacker: a principal who can write to the destination bucket can
// destroy or corrupt that copy at any moment for the rest of the
// artifact's life, and neither the old second read nor this proof stops
// them. Silent corruption is the same shape. What the pre-delete check
// genuinely buys is protection against a destination that went bad while
// nobody was driving this move, and that is the resume path, which still
// reads in full every time.
//
// # Only the class that costs egress is ever proved
//
// Attested is one metadata call and existence is one HEAD. Re-running
// either immediately before the delete costs nothing worth saving, and a
// check run on the spot is strictly better than a proof with an age, so
// those classes keep the unconditional fresh check and nothing here
// applies to them.

// preDeleteProofValidity is the longest a content read may stand as the
// authorisation for a source delete.
//
// The nominal path spends two journal writes between the read and the
// delete, so anything above a few seconds is already generous; two minutes
// is chosen to be slack enough that a loaded SQLite journal, a slow
// retention-tier guard or a scheduler hiccup does not silently start
// costing an artifact's egress per move, and small enough that "recently"
// means something a person would agree with. It is not a config key, and
// it should not become one: it is not a safety/cost dial an operator can
// usefully turn, because the only thing on the other side of it is a
// second full download whose value is argued above.
const preDeleteProofValidity = 2 * time.Minute

// objectIdentity is everything a medium will say about an object without
// serving its bytes, which is the whole of what the continuity check has
// to work with.
//
// There is no digest field and there will not be one. FR-32's rule is
// that a checksum a medium volunteers is not a content hash, and
// transport.ObjectInfo is shaped so that no caller can read one and
// compare it against a recorded hash. Asking for an attestation is a
// deliberate act with its own method and its own rung of the ladder, and
// this is not that.
type objectIdentity struct {
	size         int64
	modTime      int64
	storageClass string

	// usable is whether the medium said enough for this to mean anything.
	// A stat that failed, or one whose mod time the backend does not
	// report, leaves it false, and an identity that is not usable never
	// equals another one: comparison goes through matches, not ==.
	usable bool
}

// matches reports whether two identities describe the same unchanged
// object. An unusable identity on either side is not a match, which is the
// fail-safe direction: "I could not tell" and "it is the same" must never
// collapse into one another, for internal/artifactstore's ErrNotPresent
// reason.
func (id objectIdentity) matches(other objectIdentity) bool {
	if !id.usable || !other.usable {
		return false
	}
	return id.size == other.size && id.modTime == other.modTime && id.storageClass == other.storageClass
}

// readBackProof is one process's own record of the content read that
// produced VERIFIED: what it read, when, and what the medium said the
// object was at the moment it started reading.
//
// It is a value carried on the stack through one walk of one move and
// handed to exactly two functions. It is deliberately not on Engine, not
// in the journal and not in any map: a proof that can be found by a later
// cycle is a stale fact wearing a fresh label, which is precisely the
// thing deleteSource's read exists to refuse.
type readBackProof struct {
	held bool

	moveID   int64
	medium   string
	key      string
	class    Class
	hash     string
	hashAlg  string
	size     int64
	hasSize  bool
	identity objectIdentity
	result   Result
}

// record keeps the verdict of a read that just passed, if it is the kind
// of verdict that can stand for anything.
//
// Everything it refuses to keep is a case where the pre-delete check is
// either cheap already or would be resting on something it cannot state.
func (p *readBackProof) record(mv state.Move, src state.Placement, want Class, result Result, id objectIdentity) {
	if p == nil {
		return
	}
	*p = readBackProof{}
	switch {
	case want != Content:
		// The cheap classes keep their unconditional fresh check.
		return
	case !result.Passed || result.Class != Content:
		// Nothing to stand for. A failed read is not a proof of anything
		// and the caller is about to recopy or abandon anyway.
		return
	case !id.usable:
		// The medium would not say what the object was when the read
		// started, so nothing can say whether it is still that now.
		return
	case result.At.IsZero():
		// A verdict with no time on it has no age, and the whole of
		// clause 3 is an age.
		return
	case src.Hash == "":
		return
	}
	p.held = true
	p.moveID = mv.ID
	p.medium = mv.DestinationMedium
	p.key = mv.DestinationKey
	p.class = Content
	p.hash = src.Hash
	p.hashAlg = src.HashAlg
	if src.Size != nil {
		p.size, p.hasSize = *src.Size, true
	}
	p.identity = id
	p.result = result
}

// isAbout reports whether this proof is about the same object, the same
// class and the same bytes the delete is about.
//
// Every field compared here is re-read from the journal by deleteSource
// immediately before the call, so a source placement that changed under
// the move, a destination key that is not the one this move copied to, or
// a medium whose required class was resolved differently, all void the
// proof rather than being papered over by it.
func (p *readBackProof) isAbout(mv state.Move, src state.Placement, want Class) bool {
	switch {
	case p == nil || !p.held:
		return false
	case want != Content || p.class != Content:
		return false
	case p.moveID != mv.ID || p.medium != mv.DestinationMedium || p.key != mv.DestinationKey:
		return false
	case !strings.EqualFold(p.hash, src.Hash) || !strings.EqualFold(p.hashAlg, src.HashAlg):
		return false
	case p.hasSize != (src.Size != nil):
		return false
	case p.hasSize && p.size != *src.Size:
		return false
	}
	return true
}

// take spends the proof and returns the verdict, with the detail rewritten
// so that anything reading it is told the read is not from this instant.
//
// It spends it because a proof that could authorise two deletes is a proof
// about the wrong number of things. Result.At keeps the time of the actual
// read: a timestamp moved forward to the moment it was used would be the
// exact lie this whole file is arranged to avoid.
func (p *readBackProof) take(now time.Time) Result {
	res := p.result
	res.Detail = fmt.Sprintf(
		"%s; that read finished %s before this delete, and %q reports the same size, mod time and storage class for it now",
		res.Detail, now.Sub(res.At).Round(time.Millisecond), p.medium)
	*p = readBackProof{}
	return res
}

// reverifyForDelete produces the one fact deleteSource acts on: a
// content-class verdict about the destination, valid now.
//
// It is the single producer, it is called unconditionally and its answer
// goes into the same switch it always did, which is what keeps the nominal
// path and the resume path one path. What it decides is where the verdict
// comes from, and every way of failing that decision ends at exactly the
// call deleteSource used to make.
func (e *Engine) reverifyForDelete(ctx context.Context, mv state.Move, src state.Placement, proof *readBackProof) (Result, Class, error) {
	want, err := e.destinationClass(mv)
	if err != nil || !proof.isAbout(mv, src, want) {
		return e.verifyCopy(ctx, mv, src, false)
	}
	// Clause 3, the age. A proof from the future is not a young proof; it
	// is a clock nothing here can reason about.
	now := e.now()
	if age := now.Sub(proof.result.At); age < 0 || age > preDeleteProofValidity {
		return e.verifyCopy(ctx, mv, src, false)
	}
	// Clause 4, the continuity. This is the request that replaces the
	// download: one metadata call, no egress, and it is asked about the
	// same key the proof names.
	if !e.identifyDestination(ctx, mv).matches(proof.identity) {
		return e.verifyCopy(ctx, mv, src, false)
	}
	return proof.take(now), want, nil
}

// identifyBeforeReadBack takes the destination's identity immediately
// before its bytes are read, which is what a proof from that read is about.
//
// The ordering is load-bearing; see this file's comment. It spends nothing
// on a move that could not produce a proof anyway: a class other than
// content has nothing to save, and a destination the archive gate is about
// to refuse gets no request at all, because #437 removed exactly that
// request and putting a HEAD back in front of the refusal would undo half
// of it.
//
// Its error is discarded on purpose. A medium that will not answer leaves
// an identity that matches nothing, which costs the move the second read
// it would have paid for anyway, and there is nothing here worth failing a
// verification over.
func (e *Engine) identifyBeforeReadBack(ctx context.Context, mv state.Move) objectIdentity {
	want, err := e.destinationClass(mv)
	if err != nil || want != Content {
		return objectIdentity{}
	}
	if err := e.destinationCanBeVerified(mv.DestinationMedium); err != nil {
		return objectIdentity{}
	}
	return e.identifyDestination(ctx, mv)
}

// identifyDestination asks whatever holds the destination copy what it
// says about that object without serving its bytes.
//
// It never returns an error. Every way of failing produces the zero
// identity, which matches nothing, and the only consequence of that is the
// full read this file exists to make optional rather than to remove.
func (e *Engine) identifyDestination(ctx context.Context, mv state.Move) objectIdentity {
	if mv.DestinationMedium == config.MediumLocal {
		if e.Local == nil {
			return objectIdentity{}
		}
		st, err := e.Local.Stat(ctx, mv.DestinationKey)
		if err != nil || st.Size == nil || st.ModTime == nil {
			return objectIdentity{}
		}
		// Nanoseconds, because a local filesystem has them and throwing
		// resolution away would make the local end of a move weaker than
		// the remote end for no reason.
		return objectIdentity{size: *st.Size, modTime: st.ModTime.UnixNano(), usable: true}
	}
	medium, _, err := e.resolve(mv.DestinationMedium)
	if err != nil || e.Store == nil {
		return objectIdentity{}
	}
	info, err := e.Store.StatObject(ctx, medium, mv.DestinationKey)
	if err != nil || info.ModTime == 0 {
		// A backend that reports no mod time (ObjectInfo says so with a
		// zero) leaves size and storage class as the only signals, and
		// neither of them changes when an object is overwritten with
		// something else of the same length. That is not enough to
		// authorise a delete on, so it is not enough to be an identity.
		return objectIdentity{}
	}
	return objectIdentity{
		size:         info.Size,
		modTime:      info.ModTime,
		storageClass: info.StorageClass,
		usable:       true,
	}
}

// destinationClass is the class a move to this destination has to achieve,
// which is the medium's configured upload_verification for a medium and
// content for a local file, exactly as verifyCopy reads it.
func (e *Engine) destinationClass(mv state.Move) (Class, error) {
	if mv.DestinationMedium == config.MediumLocal {
		// verifyLocalCopy produces Content and can produce nothing else:
		// there is no cheaper way to look at a local file that proves
		// anything about its bytes.
		return Content, nil
	}
	_, want, err := e.resolve(mv.DestinationMedium)
	return want, err
}

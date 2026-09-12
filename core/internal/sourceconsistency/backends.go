package sourceconsistency

import (
	"fmt"

	"github.com/backupdproject/backupd/core/internal/backend"
	"github.com/backupdproject/backupd/core/internal/model"
)

// This file is the projection Phase 0 promised and Phase 1 owes: the
// five facts a trust classification needs, DERIVED from the backend
// capability matrix instead of restated beside it.
//
// What was here before was a map literal - BundledSourceSignals - holding
// one row per bundled backend, with mtime precision, stable size and
// metadata support copied by hand out of bundled/*.json. It was correct
// when it was written and it was documented as Phase 0 only, because a
// second home for a fact is a second place to change it and the drift
// arrives silently: the matrix says 1ns, the copy says 1s, nothing
// compares them, and a classification an operator's restore point depends
// on is decided by whichever of the two a reader happened to open. It had
// already drifted once (ADR 0009 records the cost).
//
// So four of the five are read off backend.Capabilities, and the fifth is
// not, and that split is the whole design of this file.
//
// # Why four project and one cannot
//
// mtime_precision, stable_size, metadata_support and generation_identity
// are matrix keys that answer exactly the question model.SourceSignals
// asks, so they project (with one NAMED narrowing, below, for the
// resolution this manager actually keeps).
//
// hash_support does not, and the matrix says so itself: its key
// documentation states that it answers "could a hash be obtained AT ALL",
// including by reading the whole object, while
// model.SourceSignals.RemoteHashAlgorithms is restricted to hashes that
// arrive WITHOUT this manager transferring the bytes. rclone advertises
// md5 and sha1 for a local disk and for sftp and computes both by reading
// the file, so projecting hash_support straight across would classify a
// local disk as TrustStrong on the strength of work that has not
// happened - the exact failure the restriction exists to prevent. The two
// keys look alike and mean different things, which is why the residual is
// a named table below rather than a copy, and why its default is empty.

// heldHashes is the one column the capability matrix does not answer:
// which checksums a backend HOLDS, and will therefore hand back without
// this manager reading the object's bytes.
//
// It is keyed by manifest id, its default is none, and none is the
// conservative answer: a backend absent from this map classifies weaker,
// which costs reads and never costs correctness. A row here is a claim
// that a backend answers a hash query out of its own metadata, and it is
// reviewed as a diff for the same reason every other capability claim in
// this repository is.
var heldHashes = map[string][]string{
	// The object store's md5 is a validator it already holds, so it costs
	// a HEAD and not a GET. The caveat is per object rather than per
	// backend - a multipart upload's ETag is a hash of hashes and not a
	// content md5 - and Decide is what closes it: a policy whose premise
	// is a remote hash, applied to an object that has none, reads the
	// object.
	"s3": {"md5"},

	// local_volume and sftp are deliberately absent. rclone can produce
	// md5 and sha1 for both and does it by reading every byte - over the
	// SSH session, through sha1sum, in the sftp case, which the
	// shell-less forced-subsystem account this project recommends cannot
	// run at all. A hash that costs a full read is not a signal that lets
	// a read be skipped; it IS the read.
}

// transportMTimeFloor is the finest modification time that survives into
// this manager, whatever the backend keeps.
//
// transport.RemoteArtifact.ModTime is unix SECONDS on every path into it,
// so a backend declaring 1ns or 1ms has its sub-second half truncated
// before any comparison in this package can see it. Recording the
// declared precision here would be claiming a resolution this engine
// discards, which is the precise shape of unproven metadata assumption
// this package exists to refuse.
//
// It does not change any class - a settable timestamp is weak evidence at
// any resolution - it changes how wide the blind window is, from a
// nanosecond to a second, which is the number a reason string quotes to
// an operator. A transport that carried nanoseconds would move this
// constant, deliberately, with the tests that pin it moving too.
const transportMTimeFloor = model.MTimeSecond

// SignalsFromCapabilities projects one backend's declared capability
// matrix onto the five facts a trust classification reads.
//
// backendID selects the residual hash column; it is the manifest id, not
// the rclone backend name.
func SignalsFromCapabilities(backendID string, caps backend.Capabilities) model.SourceSignals {
	return model.SourceSignals{
		MTimePrecision:       projectMTimePrecision(caps.MTimePrecision),
		StableSize:           caps.StableSize,
		RemoteHashAlgorithms: heldHashes[backendID],
		ObjectGeneration:     caps.GenerationIdentity == backend.GenerationVersioned,
		MetadataSupport:      projectMetadataSupport(caps.MetadataSupport),
	}
}

// projectMTimePrecision coarsens a declared precision to what survives the
// transport, and passes an unmeasured one straight through as unmeasured.
//
// The comparison is on the DURATION rather than on the string, so a
// precision added to the matrix later is coarsened by the same rule
// instead of falling through a switch nobody updated. A precision this
// build cannot read as a duration (the matrix's "unknown", or a value
// from a newer manifest format) stays unknown, which classifies as
// TrustUnknown: the cautious end, reached by not answering rather than by
// guessing.
func projectMTimePrecision(declared backend.MTimePrecision) model.MTimePrecision {
	mapped := model.MTimePrecision(declared)

	res, ok := mapped.Resolution()
	if !ok {
		return model.MTimePrecisionUnknown
	}

	floor, _ := transportMTimeFloor.Resolution()
	if res < floor {
		return transportMTimeFloor
	}

	return mapped
}

// projectMetadataSupport carries the matrix's answer across, and turns
// anything this build does not recognise into "none".
//
// The two vocabularies are spelled identically ("full", "partial",
// "none") and are still two types, because backend describes a backend
// and model describes what a classification may assume. An unrecognised
// value becomes MetadataNone, which reaches TrustUnknown: a manifest
// written against a newer vocabulary must not be read as the most
// generous member of the older one.
func projectMetadataSupport(declared backend.MetadataSupport) model.MetadataSupport {
	switch model.MetadataSupport(declared) {
	case model.MetadataFull:
		return model.MetadataFull
	case model.MetadataPartial:
		return model.MetadataPartial
	default:
		return model.MetadataNone
	}
}

// SourceSignalsFor returns what the bundled matrix establishes about a
// backend, and false for a backend the matrix does not describe or has
// not qualified.
//
// The false is not a formality and must not be turned into a zero-value
// default by a caller: the zero SourceSignals classifies as TrustUnknown,
// which is the right policy for an unqualified backend and the wrong
// REPORT, because it would read as "we measured this and it tells us
// nothing" instead of "nobody has filled in this row". A caller that
// cannot proceed without signals should refuse by name, the way every
// other layer in this codebase refuses a capability it does not have.
func SourceSignalsFor(backendID string) (model.SourceSignals, bool) {
	reg, err := backend.Bundled()
	if err != nil {
		return model.SourceSignals{}, false
	}

	m, err := reg.Backend(backendID)
	if err != nil {
		return model.SourceSignals{}, false
	}

	caps, ok := m.DeclaredCapabilities()
	if !ok {
		return model.SourceSignals{}, false
	}

	return SignalsFromCapabilities(backendID, caps), true
}

// ErrNoSignals is the refusal for a backend the bundled matrix does not
// qualify, for the callers that need a sentence rather than a bool.
var ErrNoSignals = fmt.Errorf("%w: nothing establishes what this backend's metadata can be trusted to say", backend.ErrUnqualifiedBackend)

// PolicyForBackend is the whole chain in one call: a backend id and the
// operator's preset in, the content policy a run should apply out. It
// exists so that no call site has to know that a policy is derived from a
// trust class which is derived from capability signals, and so that all
// three steps move together when one of them changes.
func PolicyForBackend(backendID string, preset model.MetadataTrustPreset) (model.VerificationPolicy, bool) {
	sig, ok := SourceSignalsFor(backendID)
	if !ok {
		return model.VerificationPolicy{}, false
	}

	return model.VerificationPolicyFor(model.ClassifyMetadataTrust(sig).Class, preset), true
}

// TrustForBackend is the middle step on its own, for a surface that shows
// an operator what this product thinks of their source and why. The
// reason is the load-bearing half: "weak" on a panel is an accusation,
// and "no hash arrives without reading the file" is an explanation.
func TrustForBackend(backendID string) (model.TrustClassification, bool) {
	sig, ok := SourceSignalsFor(backendID)
	if !ok {
		return model.TrustClassification{}, false
	}

	return model.ClassifyMetadataTrust(sig), true
}

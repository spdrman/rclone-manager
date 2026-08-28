package model

import "fmt"

// RemoteIdentity is what the manager persists about a remote object at
// discovery, and recaptures immediately before deleting it, so the two can be
// compared (FR-16, TOCTOU protection). A remote path can be reused: the
// object that answered a Stat at discovery time is not guaranteed to be the
// same object present at the same path minutes or hours later, when the
// lifecycle manager is about to delete it. This type is the shared shape
// every producer and consumer of that comparison agrees on.
//
// This type deliberately does not import the transport package, because
// model depends on nothing (see the package doc in ids.go): a caller sitting
// on top of transport.RemoteArtifact builds a RemoteIdentity by copying
// Path, Size and ModTime across, translating Hash/HashAlg from whatever
// transport.HashAlgorithm was used (its string form is enough here), and
// carrying transport.RemoteArtifact.ID into StableID.
type RemoteIdentity struct {
	// Path is the remote path this identity was captured at. Two identities
	// being compared are normally captured at the same path (discover, then
	// recheck before delete), so a mismatch here usually means a caller
	// compared the wrong pair of captures rather than that the backend
	// renamed anything. It is still checked first and treated as decisive,
	// because there is no scenario where a path mismatch should be waved
	// through.
	Path string

	// Size is the object's size in bytes at capture time.
	Size int64

	// ModTime is the object's modification time in unix seconds, or 0 when
	// the backend did not report one. Do not treat a ModTime match as proof
	// of anything on its own: most filesystems (and several rclone backends)
	// only report one-second granularity, so a same-second replacement is
	// invisible to it. See Confidence and CompareIdentity below for how this
	// gets weighed.
	ModTime int64

	// Hash is a cryptographic hash of the object's content, computed by the
	// backend, and HashAlg names which algorithm it is. Both are empty when
	// no hash was available at capture time.
	//
	// A hash is not guaranteed to exist. In particular, rclone's sftp
	// backend computes remote hashes by invoking a hash command (sha1sum,
	// md5sum, ...) over the SSH session, which requires shell access.
	// Against a properly hardened, shell-less SFTP account (a forced
	// internal-sftp subsystem with no login shell), that command can never
	// run and the backend reports no hash support at all. That is exactly
	// the account posture this project recommends operators use, so a
	// design that assumed a hash is always available would be wrong for its
	// own recommended deployment. CompareIdentity degrades honestly instead:
	// it falls back to weaker evidence and says so through Confidence,
	// rather than pretending the weaker evidence is proof.
	Hash    string
	HashAlg string

	// StableID is a backend-specific identifier for this object, stable
	// across whatever operations the backend guarantees it survives (for
	// example an object store's object or version id), or empty when the
	// backend offers nothing like it. FR-16 calls this out explicitly
	// alongside hash as one of the "strongest practical available
	// attributes", so CompareIdentity treats a populated StableID as strong
	// evidence, on the same footing as a hash. That is only sound if the
	// backend actually changes the identifier whenever the underlying
	// content is replaced; a caller wiring up a backend where the
	// identifier instead survives a content overwrite (e.g. it names a slot
	// rather than a version) should leave StableID empty rather than
	// populate it with a signal it cannot back up.
	StableID string
}

func (r RemoteIdentity) hasHash() bool     { return r.Hash != "" && r.HashAlg != "" }
func (r RemoteIdentity) hasModTime() bool  { return r.ModTime != 0 }
func (r RemoteIdentity) hasStableID() bool { return r.StableID != "" }

// Confidence classifies how strong the evidence behind a Verdict is.
// FR-16 does not treat confidence as a bare bool: a matching cryptographic
// hash is proof, but a matching size and modification time is only
// corroborating evidence, because mtime granularity can hide a same-second
// replacement. Collapsing that distinction into true/false would make it
// impossible to tell, downstream, whether "not confident enough" ever
// happened for a reason worth an operator's attention (loss of hashing
// capability, e.g. a hardened SFTP account) versus routine agreement.
type Confidence int

const (
	// ConfidenceNone means no attribute pair usable beyond an already-agreed
	// size was available to reason about at all (typically: no hash on
	// either side, no stable id on either side, and modification time
	// missing on at least one side). There is no signal here, weak or
	// strong, in either direction.
	ConfidenceNone Confidence = iota

	// ConfidenceWeak means corroborating evidence agreed (size and
	// modification time both matched) but nothing ruled out a same-second,
	// same-size content replacement. This is deliberately not enough to
	// confirm a match: FR-16's rule is that identity established with only
	// this much confidence must still be treated as "cannot confirm",
	// because the failure mode being defended against is precisely a fresh
	// write landing on a reused path within the same mtime tick.
	ConfidenceWeak

	// ConfidenceStrong means the verdict rests on an attribute that cannot
	// silently lie about content: a cryptographic hash comparison, a
	// backend stable-identifier comparison, or an outright mismatch on
	// path, size, or modification time (a mismatch on any of those is
	// always decisive, never merely corroborating).
	ConfidenceStrong
)

func (c Confidence) String() string {
	switch c {
	case ConfidenceNone:
		return "none"
	case ConfidenceWeak:
		return "weak"
	case ConfidenceStrong:
		return "strong"
	default:
		return fmt.Sprintf("Confidence(%d)", int(c))
	}
}

// Verdict is the conclusion CompareIdentity reaches. It is a three-way
// outcome rather than a bool precisely because FR-16 requires "identity
// cannot be established with sufficient confidence" to be distinguishable
// from "confirmed changed": both refuse a pending deletion, but they call for
// different operator responses. A confirmed change is a sign something
// wrote where this manager's own artifact used to live and probably wants
// investigating as a possible incident. An unconfirmed comparison is a sign
// this backend/account cannot currently prove identity strongly enough
// (commonly: no hash available) and probably wants a configuration fix, not
// an investigation.
type Verdict int

const (
	// VerdictUnconfirmed means identity could not be established with
	// sufficient confidence. Per FR-16, a caller must treat this the same
	// as VerdictChanged for the purpose of a pending deletion: preserve the
	// remote object. See Confidence for why this happened.
	VerdictUnconfirmed Verdict = iota

	// VerdictUnchanged means the current object is confirmed, with strong
	// confidence, to be the same object captured as discovered. This is the
	// only verdict under which FR-16 permits a pending deletion to proceed.
	VerdictUnchanged

	// VerdictChanged means the current object is confirmed, with strong
	// confidence, to no longer correspond to the object captured as
	// discovered (different path, size, modification time, hash, or stable
	// identifier). A pending deletion must be refused.
	VerdictChanged
)

func (v Verdict) String() string {
	switch v {
	case VerdictUnconfirmed:
		return "unconfirmed"
	case VerdictUnchanged:
		return "unchanged"
	case VerdictChanged:
		return "changed"
	default:
		return fmt.Sprintf("Verdict(%d)", int(v))
	}
}

// IdentityComparison is the full result of comparing two RemoteIdentity
// captures: the outcome, how much confidence backs it, and a short,
// human-readable reason suitable for an audit log line explaining why a
// deletion was allowed to proceed or was refused.
type IdentityComparison struct {
	Verdict    Verdict
	Confidence Confidence
	Reason     string
}

// Preserve reports FR-16's actual policy decision: whether the remote object
// must be preserved rather than deleted. Only a VerdictUnchanged backed by
// ConfidenceStrong clears the bar; VerdictChanged and VerdictUnconfirmed
// both preserve, which is the point of modeling them as distinct outcomes
// while still agreeing on the one bit of policy that matters here.
func (c IdentityComparison) Preserve() bool {
	return c.Verdict != VerdictUnchanged || c.Confidence != ConfidenceStrong
}

// CompareIdentity compares discovered (captured at discovery time) against
// current (recaptured immediately before a pending deletion) and returns the
// FR-16 verdict, reaching for the strongest practical available attribute at
// each step:
//
//  1. path - a mismatch is always decisive.
//  2. hash - when both sides carry one, computed with the same algorithm, a
//     match or mismatch is decisive proof either way.
//  3. backend stable identifier - the same, when both sides carry one and
//     neither side already settled the question via hash.
//  4. size - a mismatch is always decisive.
//  5. modification time - a mismatch, when both sides report one, is
//     decisive; but an agreement here, with nothing stronger available,
//     only ever reaches ConfidenceWeak, never confirms a match.
//
// If none of the above can settle it, the verdict is VerdictUnconfirmed with
// ConfidenceNone: there was no usable signal at all beyond an agreeing size.
func CompareIdentity(discovered, current RemoteIdentity) IdentityComparison {
	if discovered.Path != current.Path {
		return IdentityComparison{
			Verdict:    VerdictChanged,
			Confidence: ConfidenceStrong,
			Reason:     fmt.Sprintf("path changed from %q to %q", discovered.Path, current.Path),
		}
	}

	algNote := ""
	if discovered.hasHash() && current.hasHash() {
		if discovered.HashAlg == current.HashAlg {
			if discovered.Hash != current.Hash {
				return IdentityComparison{
					Verdict:    VerdictChanged,
					Confidence: ConfidenceStrong,
					Reason:     fmt.Sprintf("%s hash mismatch", discovered.HashAlg),
				}
			}
			return IdentityComparison{
				Verdict:    VerdictUnchanged,
				Confidence: ConfidenceStrong,
				Reason:     fmt.Sprintf("%s hash matches", discovered.HashAlg),
			}
		}
		// Both sides have a hash, but of different algorithms: they cannot
		// be compared directly. This is not the same as neither side having
		// one, so it is worth saying in whatever reason eventually gets
		// returned, rather than silently falling through as if no hash had
		// ever been present.
		algNote = fmt.Sprintf(" (a %s hash and a %s hash were both present but not comparable)", discovered.HashAlg, current.HashAlg)
	}

	if discovered.hasStableID() && current.hasStableID() {
		if discovered.StableID != current.StableID {
			return IdentityComparison{
				Verdict:    VerdictChanged,
				Confidence: ConfidenceStrong,
				Reason:     "backend stable identifier mismatch",
			}
		}
		return IdentityComparison{
			Verdict:    VerdictUnchanged,
			Confidence: ConfidenceStrong,
			Reason:     "backend stable identifier matches",
		}
	}

	if discovered.Size != current.Size {
		return IdentityComparison{
			Verdict:    VerdictChanged,
			Confidence: ConfidenceStrong,
			Reason:     fmt.Sprintf("size changed from %d to %d", discovered.Size, current.Size),
		}
	}

	if discovered.hasModTime() && current.hasModTime() {
		if discovered.ModTime != current.ModTime {
			return IdentityComparison{
				Verdict:    VerdictChanged,
				Confidence: ConfidenceStrong,
				Reason:     fmt.Sprintf("modification time changed from %d to %d", discovered.ModTime, current.ModTime),
			}
		}
		return IdentityComparison{
			Verdict:    VerdictUnconfirmed,
			Confidence: ConfidenceWeak,
			Reason:     "size and modification time agree, but no hash or backend stable identifier is available to confirm content; a same-second replacement would look identical" + algNote,
		}
	}

	return IdentityComparison{
		Verdict:    VerdictUnconfirmed,
		Confidence: ConfidenceNone,
		Reason:     "size agrees, but no hash, backend stable identifier, or modification time is available to compare beyond it" + algNote,
	}
}

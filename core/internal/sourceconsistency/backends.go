package sourceconsistency

import "github.com/backupdproject/backupd/core/internal/model"

// BundledSourceSignals is what this build has established about each
// backend it can read a source from, as the five facts a trust
// classification needs.
//
// It is a table in Go, reviewed as a diff, for the reason
// core/internal/backend/doc.go gives about its own registry: a capability a
// backend is assumed to have is a claim this product makes on an operator's
// behalf, and the place to make one is somewhere a reviewer sees it. Each
// row below is annotated with what it rests on, because the rows an operator
// would most like to be optimistic about are exactly the ones where
// optimism costs a silently stale restore point.
//
// The values correspond key for key to the backend capability matrix's
// mtime_precision, stable_size, hash_support and metadata_support, with one
// deliberate narrowing: RemoteHashAlgorithms records only hashes the backend
// returns WITHOUT this manager reading the object. See its field
// documentation in model; the narrowing is why local_volume and sftp carry
// no hashes here even though rclone advertises md5 and sha1 for both.
var BundledSourceSignals = map[string]model.SourceSignals{
	// A local filesystem: the richest metadata of the three and no content
	// evidence at all. Sizes that mean what they say, full ownership and
	// mode, and what it cannot do is answer "has this content changed"
	// without being read - which is why the richest metadata in the set
	// still classifies weak.
	//
	// The timestamp is recorded as one SECOND and that is not what the
	// filesystem carries. Every filesystem this product ships on (APFS,
	// ext4, XFS, btrfs, ZFS) keeps nanoseconds, and the backend capability
	// matrix says 1ns because the matrix describes the BACKEND. This table
	// describes what survives into this manager, and
	// transport.RemoteArtifact.ModTime is unix SECONDS on every path into it
	// (transport.go:152), so the sub-second half is truncated before any
	// comparison here can see it. Recording 1ns would be claiming a
	// resolution this engine discards - the precise shape of unproven
	// metadata assumption this table exists to refuse.
	//
	// It does not change the class: a settable timestamp is weak evidence at
	// any resolution. It changes how wide the blind window is, from a
	// nanosecond to a second, which is the number a reason string quotes to
	// an operator. A transport that carried nanoseconds would move this row,
	// deliberately, with the test below moving too.
	"local_volume": {
		MTimePrecision:  model.MTimeSecond,
		StableSize:      true,
		MetadataSupport: model.MetadataFull,
	},

	// An object store. The one strong source in the set, and strong for the
	// version identifier rather than for the timestamp.
	//
	// The millisecond resolution is the capability matrix's measured answer
	// for this backend; this manager's own seconds truncation applies to it
	// as well, and it is recorded as declared here because the class does
	// not rest on the timestamp at all - the generation identifier settles
	// every comparison before a timestamp is consulted.
	//
	// The md5 entry is the ETag, and it is honest about a caveat rather than
	// omitting it: an object uploaded in multiple parts has an ETag that is
	// a hash of hashes, not a content hash, and such an object reports no
	// usable md5. That is a PER-OBJECT gap in a per-backend capability, and
	// Decide is what closes it: a policy whose premise is a remote hash,
	// applied to an object that has none, reads the object.
	"s3": {
		MTimePrecision:       model.MTimeMillisecond,
		StableSize:           true,
		RemoteHashAlgorithms: []string{"md5"},
		ObjectGeneration:     true,
		MetadataSupport:      model.MetadataPartial,
	},

	// SFTP. Second-resolution timestamps, because the protocol's attribute
	// structure carries mtime as a count of seconds (RFC draft-ietf-secsh-
	// filexfer, version 3, which is what rclone's sftp backend negotiates),
	// so there is no sub-second information to have in the first place.
	//
	// Metadata support is partial rather than full, matching the capability
	// matrix: ownership and mode come back as the far host's numeric ids
	// with no way to resolve them, and extended attributes do not come back
	// at all.
	//
	// And no hash, for the reason model/identity.go already wrote down about
	// the delete path: rclone computes an sftp remote hash by running
	// sha1sum or md5sum over the SSH session, which requires a shell, and
	// this project's own recommended posture is a shell-less, forced-
	// subsystem account. The capability matrix reports the same empty list
	// for the same reason - hash availability there is a property of
	// somebody else's PATH - so this is one row where the narrowing and the
	// matrix happen to agree.
	"sftp": {
		MTimePrecision:  model.MTimeSecond,
		StableSize:      true,
		MetadataSupport: model.MetadataPartial,
	},
}

// SourceSignalsFor returns what has been established about a backend, and
// false for a backend nothing has been established about.
//
// The false is not a formality and must not be turned into a zero-value
// default by a caller: the zero SourceSignals classifies as TrustUnknown,
// which is the right policy for an unmeasured backend but the wrong REPORT,
// because it would read as "we measured this and it tells us nothing"
// instead of "nobody has filled in this row". A caller that cannot proceed
// without signals should refuse by name, the way every other layer in this
// codebase refuses a capability it does not have.
func SourceSignalsFor(backendID string) (model.SourceSignals, bool) {
	sig, ok := BundledSourceSignals[backendID]

	return sig, ok
}

// PolicyForBackend is the whole chain in one call: a backend id and the
// operator's preset in, the content policy a run should apply out. It exists
// so that no call site has to know that a policy is derived from a trust
// class which is derived from capability signals, and so that all three
// steps move together when one of them changes.
func PolicyForBackend(backendID string, preset model.MetadataTrustPreset) (model.VerificationPolicy, bool) {
	sig, ok := SourceSignalsFor(backendID)
	if !ok {
		return model.VerificationPolicy{}, false
	}

	return model.VerificationPolicyFor(model.ClassifyMetadataTrust(sig).Class, preset), true
}

// TrustForBackend is the middle step on its own, for a surface that shows an
// operator what this product thinks of their source and why. The reason is
// the load-bearing half: "weak" on a panel is an accusation, and "no hash
// arrives without reading the file" is an explanation.
func TrustForBackend(backendID string) (model.TrustClassification, bool) {
	sig, ok := SourceSignalsFor(backendID)
	if !ok {
		return model.TrustClassification{}, false
	}

	return model.ClassifyMetadataTrust(sig), true
}

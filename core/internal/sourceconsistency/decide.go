package sourceconsistency

import (
	"fmt"
	"hash/fnv"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
)

// Entry is what the catalogue remembers about one path, and what a fresh
// listing produces for the same path. The same type serves both so that a
// decision is a comparison of like with like; the fields a listing cannot
// fill (Digest, VerifiedAt) are simply empty on the current side.
type Entry struct {
	Path string

	Size         int64
	ModTimeNanos int64

	// Mode and Owner are carried because the embedded engine compares them
	// (kopiaMetadataReuse, in decide_test.go, is that rule in Go) and
	// because a changed mode is a change worth storing. Neither bears on
	// CONTENT, so neither forces a re-read on its own: a chmod does not
	// rewrite a file.
	Mode  uint32
	Owner string

	// Digest is the content hash this manager computed when it last read the
	// file, with DigestAlg naming the algorithm.
	Digest    string
	DigestAlg string

	// RemoteHash is a hash the BACKEND computed, with RemoteHashAlg naming
	// it. Empty where the backend offers none for this object, which is a
	// per-object state and not only a per-backend one: an s3 object uploaded
	// in multiple parts has a backend validator that is not a content hash,
	// while its neighbour uploaded in one part has one.
	RemoteHash    string
	RemoteHashAlg string

	// GenerationID is the backend's generation or version identifier, empty
	// where there is none. The contract is model.RemoteIdentity.StableID's:
	// an identifier that survives a content overwrite names a slot rather
	// than a version and must be left empty.
	GenerationID string

	// VerifiedAt is when this manager last read the content and confirmed
	// the digest. The zero time means never, which is why a policy's
	// interval check has to treat it as infinitely stale rather than as
	// "now".
	VerifiedAt time.Time
}

// IsZero reports whether this is the absence of a previous entry. A path is
// the one field every real entry has, and the zero Entry compares equal to
// itself on size and timestamp, so a decision written without this check
// would reuse content for a file it has never seen.
func (e Entry) IsZero() bool { return e.Path == "" }

func (e Entry) hasRemoteHash() bool { return e.RemoteHash != "" && e.RemoteHashAlg != "" }

// Action is what a run does about one path's content.
type Action string

const (
	// ActionReuse means the content stored for this path may be carried
	// forward without reading the file.
	ActionReuse Action = "reuse"

	// ActionReadAndVerify means the file is read and hashed, and the result
	// compared with what was stored.
	ActionReadAndVerify Action = "read_and_verify"
)

// Decision is an action and the sentence that justifies it. The sentence is
// what a run report shows an operator asking why an incremental backup
// re-read four hundred gigabytes, and answering that with a mode name would
// not survive the follow-up question.
type Decision struct {
	Action Action
	Reason string
}

func reuse(format string, args ...any) Decision {
	return Decision{Action: ActionReuse, Reason: fmt.Sprintf(format, args...)}
}

func read(format string, args ...any) Decision {
	return Decision{Action: ActionReadAndVerify, Reason: fmt.Sprintf(format, args...)}
}

// Decide answers whether this path's stored content may be carried forward,
// given what the catalogue holds (prev), what a fresh listing reports (cur),
// and the content policy derived from the source's metadata-trust class.
//
// The order is the order the evidence is decisive:
//
//  1. no previous entry, or a different path, is a read. There is nothing to
//     carry forward.
//  2. a policy that does not permit metadata to skip content is a read,
//     before any metadata is looked at. This is the clause that overrides
//     the engine's own heuristic, and it has to come first or the metadata
//     comparison below would agree with the engine and stop.
//  3. metadata that moved is a read, under every policy. Agreement is never
//     proof; disagreement is always decisive. (This is identity.go's rule,
//     from the other direction.)
//  4. then the policy's own strategy decides: the backend's hash or
//     generation identifier, a deterministic sample, or an age.
//
// Every branch that reuses is bounded by the policy's ReverifyInterval, so
// no path in any source goes unread forever.
func Decide(prev, cur Entry, pol model.VerificationPolicy, now time.Time) Decision {
	if prev.IsZero() {
		return read("this path has no previous capture to carry forward")
	}

	if prev.Path != cur.Path {
		return read("the stored entry is for %q and the listing is for %q", prev.Path, cur.Path)
	}

	if !pol.MetadataMaySkipContent() {
		return read("the content policy for this source does not let metadata stand in for content: %s", pol.Reason)
	}

	if prev.Size != cur.Size {
		return read("the size changed from %d to %d", prev.Size, cur.Size)
	}

	if prev.ModTimeNanos != cur.ModTimeNanos {
		return read("the modification time changed")
	}

	interval := pol.ReverifyInterval
	stale := prev.VerifiedAt.IsZero() || now.Sub(prev.VerifiedAt) >= interval

	switch pol.Mode {
	case model.VerifyRemoteHash:
		if prev.GenerationID != "" && cur.GenerationID != "" {
			if prev.GenerationID != cur.GenerationID {
				return read("the backend's generation identifier changed from %q to %q", prev.GenerationID, cur.GenerationID)
			}
			if stale {
				return read("the generation identifier still matches, but this path has not been read in full for longer than %s", interval)
			}

			return reuse("the backend's generation identifier is unchanged, which is the same content")
		}

		if prev.hasRemoteHash() && cur.hasRemoteHash() {
			if prev.RemoteHashAlg != cur.RemoteHashAlg {
				return read("the backend reported a %s hash before and a %s hash now, which cannot be compared", prev.RemoteHashAlg, cur.RemoteHashAlg)
			}
			if prev.RemoteHash != cur.RemoteHash {
				return read("the backend's %s hash changed", prev.RemoteHashAlg)
			}
			if stale {
				return read("the backend's %s hash still matches, but this path has not been read in full for longer than %s", prev.RemoteHashAlg, interval)
			}

			return reuse("the backend's %s hash is unchanged, which is the same content", prev.RemoteHashAlg)
		}

		return read("this source's policy verifies content through the backend's own hash or generation identifier, and this object has neither")

	case model.VerifyPeriodic:
		if stale {
			return read("nothing in the metadata moved, but this path has not been read in full for longer than %s", interval)
		}

		return reuse("the metadata is unchanged and this path was read in full less than %s ago", interval)

	case model.VerifySampled:
		if sampled(cur.Path, pol.SampleFraction) {
			return read("this path is in this run's verification sample")
		}
		if stale {
			return read("this path has been missed by the sample for longer than %s", interval)
		}

		return reuse("the metadata is unchanged, this path is not in this run's sample, and it was verified less than %s ago", interval)

	default:
		return read("the content policy for this source is %q, which this package does not know how to satisfy without reading", pol.Mode)
	}
}

// samplingBuckets is the resolution the sample fraction is applied at. A
// million buckets makes a fraction as small as 0.0001 meaningful and keeps
// the arithmetic in integers up to the comparison.
const samplingBuckets = 1_000_000

// sampled decides whether a path is in this run's verification sample.
//
// It is a hash of the path, not a random draw, and that is a requirement
// rather than a convenience: a random sample makes a run's cost and a run's
// coverage both unreproducible, so an operator who saw an unexplained
// re-read cannot ask whether it would happen again, and a test cannot pin
// the behaviour at all. Hashing the path means the same source samples the
// same files until the fraction changes.
//
// FNV-1a because the property needed is a well-mixed distribution over
// short strings, not resistance to anybody choosing paths on purpose. A
// source whose owner is adversarial about which of their own files get
// verified is not a threat this sampling is defending against.
func sampled(path string, fraction float64) bool {
	if fraction <= 0 {
		return false
	}
	if fraction >= 1 {
		return true
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(path))

	return float64(h.Sum64()%samplingBuckets) < fraction*samplingBuckets
}

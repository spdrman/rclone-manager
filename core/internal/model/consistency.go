// The source side of what identity.go already established for the
// destination side: this product cannot always prove content identity, so
// the strength of the evidence is part of the answer rather than a detail
// the answer hides.
//
// identity.go asks "is the object at this remote path still the object I
// decided to delete". This file asks the two questions an incremental
// backup engine asks before it decides not to read a file at all:
//
//  1. can this source change WHILE a run reads it, and if it can, what does
//     a change mean? That is the consistency mode, and it is a property of
//     how the operator arranged the source, not of the backend.
//  2. is what this backend reports about a file strong enough to stand in
//     for the file's content across two runs? That is the metadata trust
//     class, and it is a property of the backend's protocol.
//
// The two are separate axes on purpose. A frozen LVM snapshot cannot be
// torn by a concurrent writer and still tells you nothing about whether a
// file changed since last week; a versioned object store proves content
// identity across runs and is still being written to while you read it.
// Collapsing them into one "is this source safe" flag loses whichever half
// the flag was not about.
//
// # Why a nanosecond timestamp is still only weak evidence
//
// The tempting rule is that a same-path, same-size, same-modification-time
// file is the same file, and with nanosecond resolution the window where
// that is wrong looks vanishingly small. It is not a window. A modification
// time is SETTABLE: rsync --times, tar -p, cp -p, an editor that preserves
// timestamps, and restoring the source itself from a backup all write new
// content and then put the old timestamp back. None of those is exotic and
// every one of them defeats the rule completely, at any resolution.
//
// This matters here specifically because it is exactly the rule the embedded
// engine uses. kopia v0.23.1 decides a file's content can be reused from
// name, mode, owner, modification time and size, comparing the current
// directory entry against the previous snapshot's
// (snapshot/upload/upload.go:694-720), re-hashing nothing unless
// ForceHashPercentage is raised from its zero default (upload.go:756-767).
// That is a good default for a desktop backup tool and it is not a
// sufficient answer for a product whose job is to be right about whether a
// restore point contains the file an operator thinks it contains. So the
// classification below never reaches TrustStrong on timestamps, and
// VerificationPolicyFor is what sits above the engine and decides when the
// engine's own answer has to be overridden with a read.

package model

import (
	"fmt"
	"time"
)

// ConsistencyMode is what the operator has arranged around the source while
// a run reads it. It is declared rather than detected, because none of the
// three can be proven from this side: quiescence is a claim about a process
// this manager does not control, and a snapshot is a claim about a volume it
// did not create. What this type buys is that the claim is RECORDED, so a
// mutation observed during a run can be reported against it (see
// MutationIsContractViolation).
type ConsistencyMode string

const (
	// ModeLiveBestEffort is a source being read while whatever writes to it
	// keeps writing. It is the default because it is the only mode that
	// needs no cooperation, and it promises the least: files are individually
	// coherent as far as the reader can prove (see internal/sourceconsistency),
	// and the set of files is not a point in time.
	ModeLiveBestEffort ConsistencyMode = "live_best_effort"

	// ModeExternallyQuiesced is a source whose writers the operator has
	// stopped, or flushed and locked, for the duration of the run - a
	// stopped container, a database in backup mode, a paused service. The
	// run is a point in time only to the extent that the claim is true,
	// which is why a mutation seen under this mode is reported.
	ModeExternallyQuiesced ConsistencyMode = "externally_quiesced"

	// ModeExternalSnapshot is a source that CANNOT change while the run
	// reads it, because what is being read is a frozen image: an LVM or ZFS
	// snapshot, a VSS shadow copy, a read-only clone. This is the only mode
	// under which a run is a point in time, and the only one under which the
	// second read that catches a torn file is pointless work.
	ModeExternalSnapshot ConsistencyMode = "external_snapshot"
)

// ConsistencyModes is every mode. A fourth entry is an operator-visible
// decision (it reaches the API, the wizard and the run report) and an ADR
// change, which is what the count assertion in the tests is defending.
var ConsistencyModes = []ConsistencyMode{
	ModeLiveBestEffort,
	ModeExternallyQuiesced,
	ModeExternalSnapshot,
}

func (m ConsistencyMode) String() string { return string(m) }

// ParseConsistencyMode reads a mode back from configuration or from the
// catalogue, and refuses anything else rather than defaulting.
//
// Defaulting would be wrong in the one direction that matters: the parser
// cannot know whether a typo was meant to be the weakest mode or the
// strongest, and answering ModeLiveBestEffort (the zero-ish, most
// conservative promise) would silently discard an operator's snapshot
// arrangement, while answering ModeExternalSnapshot would invent a
// point-in-time guarantee nobody made.
func ParseConsistencyMode(s string) (ConsistencyMode, error) {
	for _, m := range ConsistencyModes {
		if string(m) == s {
			return m, nil
		}
	}

	return "", fmt.Errorf("unknown source consistency mode %q", s)
}

// GuaranteesPointInTime reports whether a run over this source is a single
// moment rather than a walk through a moving tree. Only a frozen image is.
//
// This is the bit a surface must consult before printing anything like
// "consistent": under the other two modes a directory read early in the run
// and a file read late in it can disagree, and no amount of per-file care
// changes that.
func (m ConsistencyMode) GuaranteesPointInTime() bool {
	return m == ModeExternalSnapshot
}

// MutationIsContractViolation reports whether a mutation observed during a
// run contradicts what this mode claimed.
//
// Under ModeLiveBestEffort it does not: a live source changing is the mode,
// and reporting it as a fault would fill a run report with the one thing
// that mode exists to accommodate. Under the other two it does, and it is
// the most useful thing this manager can tell an operator, because both
// failures are silent otherwise: a quiesce hook that stopped the wrong
// container, and a snapshot path that was actually the live mount.
func (m ConsistencyMode) MutationIsContractViolation() bool {
	return m == ModeExternallyQuiesced || m == ModeExternalSnapshot
}

// Describe is the operator-facing sentence for this mode. The ADR's matrix
// and the wizard's mode picker are both renderings of these, so a mode
// without one would render as a blank row somewhere.
func (m ConsistencyMode) Describe() string {
	switch m {
	case ModeLiveBestEffort:
		return "read while the source keeps changing; each file is captured coherently or reported, and the run as a whole is not a single point in time"
	case ModeExternallyQuiesced:
		return "the operator has stopped or flushed the writers for the duration of the run; a change seen during the run is reported, because it means the arrangement did not hold"
	case ModeExternalSnapshot:
		return "the run reads a frozen image (LVM, ZFS, VSS, a read-only clone), so it is a single point in time and a change seen during it means the path was not the snapshot"
	default:
		return ""
	}
}

// TrustClass says how far what a backend REPORTS about a file may be
// trusted to stand in for the file's content across two runs.
//
// It is deliberately the same three-way shape as identity.go's Confidence,
// for the same reason: "we could not establish this" has to be
// distinguishable from "we established it and it is not enough", because
// the first is usually a configuration or capability problem an operator can
// fix and the second is a property of the protocol they are stuck with.
type TrustClass string

const (
	// TrustStrong means the backend hands back something that cannot
	// silently lie about content: a hash it computed without this manager
	// reading the object, or an identifier that changes when the object is
	// overwritten.
	TrustStrong TrustClass = "strong"

	// TrustWeak means the backend's metadata is understood and is not
	// enough. Size and modification time agreeing is corroboration, not
	// proof, at any resolution - see the package-level note on settable
	// timestamps.
	TrustWeak TrustClass = "weak"

	// TrustUnknown means what this backend reports has not been
	// established: no stated timestamp resolution, no metadata, or sizes
	// that do not hold still. Unknown is treated more harshly than weak,
	// because a weak signal can at least be sampled around and an
	// unestablished one cannot.
	TrustUnknown TrustClass = "unknown"
)

func (c TrustClass) String() string { return string(c) }

// MTimePrecision is the resolution a backend's modification times actually
// carry, as the capability matrix states it (the "mtime_precision" key).
// The strings are the matrix's, so a value can travel from a backend
// manifest to here without translation.
type MTimePrecision string

const (
	MTimeNanosecond  MTimePrecision = "1ns"
	MTimeMillisecond MTimePrecision = "1ms"
	MTimeSecond      MTimePrecision = "1s"
	MTimeTwoSecond   MTimePrecision = "2s"

	// MTimePrecisionUnknown is an honest admission and a legitimate value.
	// It classifies as TrustUnknown, which is the point: a backend nobody
	// has measured gets the policy that assumes nothing, rather than the
	// policy that assumes the best.
	MTimePrecisionUnknown MTimePrecision = "unknown"
)

// Resolution reads a precision as a duration, and reports false for
// "unknown" and for anything this package does not recognise. The two are
// not distinguished here on purpose: a value nobody measured and a value
// somebody mistyped are equally unusable, and both must reach the same
// cautious classification rather than one of them reaching a default.
func (p MTimePrecision) Resolution() (time.Duration, bool) {
	switch p {
	case MTimeNanosecond:
		return time.Nanosecond, true
	case MTimeMillisecond:
		return time.Millisecond, true
	case MTimeSecond:
		return time.Second, true
	case MTimeTwoSecond:
		return 2 * time.Second, true
	default:
		return 0, false
	}
}

// MetadataSupport is how much of a file's metadata a backend can report at
// all, as the capability matrix states it (the "metadata_support" key).
type MetadataSupport string

const (
	MetadataFull    MetadataSupport = "full"
	MetadataPartial MetadataSupport = "partial"
	MetadataNone    MetadataSupport = "none"
)

// SourceSignals is the narrow set of backend facts a trust classification
// needs. It is this package's own struct rather than the backend capability
// matrix itself, because model depends on nothing (see the package doc in
// ids.go) and because only five of the matrix's keys bear on this question.
// A caller holding a capability matrix copies these five across; the field
// documentation below names the matrix key each one corresponds to.
type SourceSignals struct {
	// MTimePrecision is the matrix's "mtime_precision".
	MTimePrecision MTimePrecision

	// StableSize is the matrix's "stable_size": whether a size reported by a
	// listing is the size a read will produce. A backend that reports
	// sizes it cannot honour (a streamed object of unknown length, a
	// compressed-on-the-fly file) leaves this false, and a false here
	// reaches TrustUnknown, because size is the load-bearing half of every
	// metadata comparison.
	StableSize bool

	// RemoteHashAlgorithms is the matrix's "hash_support", RESTRICTED to
	// hashes the backend returns WITHOUT this manager transferring the
	// object's bytes.
	//
	// The restriction is the whole value of the field and it is easy to get
	// wrong when copying from the matrix. rclone advertises md5 and sha1 for
	// a local filesystem and for sftp, and computes both by reading the
	// entire file (over the SSH session, via sha1sum, in the sftp case -
	// which a shell-less account, the posture this project recommends,
	// cannot run at all). A hash that costs a full read is not a metadata
	// signal that lets a read be skipped; it IS the read. Recording it here
	// would classify a local disk as strong on the strength of work that has
	// not happened.
	RemoteHashAlgorithms []string

	// ObjectGeneration reports whether the backend exposes a generation or
	// version identifier that CHANGES when the object is overwritten. The
	// same warning identity.go gives about RemoteIdentity.StableID applies:
	// an identifier that names a slot rather than a version survives a
	// content overwrite and must leave this false.
	ObjectGeneration bool

	// MetadataSupport is the matrix's "metadata_support".
	MetadataSupport MetadataSupport
}

// TrustClassification is a class and the sentence that justifies it. The
// sentence is not decoration: "weak" on its own is not actionable, and the
// reason is what a run report shows an operator asking why their source is
// being re-read.
type TrustClassification struct {
	Class  TrustClass
	Reason string
}

// ClassifyMetadataTrust derives the trust class from what a backend
// reports, in the order the evidence is decisive:
//
//  1. a hash the backend computes without this manager reading the object,
//     or a generation identifier that changes on overwrite, is strong. These
//     are the only two signals that cannot silently lie about content.
//  2. failing that, signals that were never established - no readable
//     timestamp resolution, no metadata, unstable sizes - are unknown.
//  3. everything else is weak: understood, and not enough.
//
// Strong evidence is tested FIRST, before the completeness checks, because
// it does not depend on them. An object store that reports no usable
// timestamp and versions every object proves content identity by the
// version, and demoting it for the timestamp it does not need would force a
// full re-read of a source that can answer the question exactly.
func ClassifyMetadataTrust(s SourceSignals) TrustClassification {
	if len(s.RemoteHashAlgorithms) > 0 {
		return TrustClassification{
			Class:  TrustStrong,
			Reason: fmt.Sprintf("the backend reports a content hash (%s) without this manager reading the object, so content identity can be compared directly", s.RemoteHashAlgorithms[0]),
		}
	}

	if s.ObjectGeneration {
		return TrustClassification{
			Class:  TrustStrong,
			Reason: "the backend exposes a generation identifier that changes when an object is overwritten, so a matching identifier is the same content",
		}
	}

	if s.MetadataSupport != MetadataFull && s.MetadataSupport != MetadataPartial {
		return TrustClassification{
			Class:  TrustUnknown,
			Reason: fmt.Sprintf("the backend's metadata support is %q, so there is nothing established to reason about", string(s.MetadataSupport)),
		}
	}

	if !s.StableSize {
		return TrustClassification{
			Class:  TrustUnknown,
			Reason: "the backend does not guarantee that a size reported by a listing is the size a read produces, and size is the load-bearing half of every metadata comparison",
		}
	}

	resolution, ok := s.MTimePrecision.Resolution()
	if !ok {
		return TrustClassification{
			Class:  TrustUnknown,
			Reason: fmt.Sprintf("the backend's modification-time resolution is %q, which is not a resolution this manager can reason about", string(s.MTimePrecision)),
		}
	}

	return TrustClassification{
		Class: TrustWeak,
		Reason: fmt.Sprintf("size and a %s modification time are the strongest signals available, and a modification time is settable: an in-place rewrite that restores it is invisible at any resolution",
			resolution),
	}
}

// MetadataTrustPreset is the operator's answer to "how much should this
// product trust what your source tells it". Two values, because a third
// would be a number nobody can choose between: the question is really
// "would you rather pay I/O or carry risk", and that has two ends.
//
// Neither value is named "strong", and that is deliberate rather than
// awkward. "strong" is already a TrustClass - a DERIVED statement about
// what a backend can prove - and a preset with the same spelling made
// "strong" mean two unrelated things in the same sentence: a source could
// be strong under the strong preset, or weak under it, and a run report
// that said "strong" told an operator nothing about which half it meant.
// The preset names what the operator is asking for; the class names what
// the source can back up.
type MetadataTrustPreset string

const (
	// PresetTrustMetadata takes the backend's metadata at close to face
	// value and pays the least I/O. On a source whose trust class is strong
	// it is the right default. On a weak one it is a deliberate, stated
	// trade: sampled verification and a re-read floor, not proof.
	PresetTrustMetadata MetadataTrustPreset = "trust_metadata"

	// PresetConservative spends read bandwidth instead of trusting
	// metadata. On a weak or unknown source it re-reads and re-hashes every
	// file every run, which is the only thing that catches an in-place
	// rewrite with a restored timestamp.
	PresetConservative MetadataTrustPreset = "conservative"
)

func (p MetadataTrustPreset) String() string { return string(p) }

// VerificationMode is what a policy does about content.
type VerificationMode string

const (
	// VerifyNever exists to be named and never chosen. It is what "trust
	// the metadata and never look again" would be called, it is what kopia's
	// own default amounts to for an unchanged-looking file, and
	// VerificationPolicyFor never returns it - a test asserts that over
	// every input, including classes and presets this package does not
	// recognise.
	VerifyNever VerificationMode = "never"

	// VerifyRemoteHash compares the backend's own content hash or generation
	// identifier every run, and reads only when they disagree or are absent
	// for a particular object. No content is transferred to verify.
	VerifyRemoteHash VerificationMode = "remote_hash"

	// VerifySampled re-reads and re-hashes a deterministic fraction of paths
	// every run, so a source that is lying about its metadata is found by
	// the sample rather than by a restore.
	VerifySampled VerificationMode = "sampled"

	// VerifyPeriodic re-reads a path once its last verification is older
	// than the interval, whatever the metadata says.
	VerifyPeriodic VerificationMode = "periodic"

	// VerifyAlways re-reads and re-hashes every file every run. Metadata is
	// used for nothing except deciding what exists.
	VerifyAlways VerificationMode = "always"
)

// The cadences the policy matrix hands out. They are named rather than
// inline because each one is a published promise, and a promise with a
// number in it should be greppable.
const (
	// remoteHashReadFloor is how often a strong source gets read in full
	// even though its hashes have agreed every run. The backend's hash is
	// the backend's claim about its own storage; nothing but a read catches a
	// source that has rotted underneath a hash it still reports.
	remoteHashReadFloor = 90 * 24 * time.Hour

	// strongConservativeInterval is the conservative preset's answer on a
	// strong source: the hash comparison still runs, and a full read happens
	// monthly regardless.
	strongConservativeInterval = 30 * 24 * time.Hour

	// weakSampleInterval is the floor under sampling. Without it, the 95% of
	// paths the sample misses would be "skip forever" with extra steps.
	weakSampleInterval = 7 * 24 * time.Hour

	// weakSampleFraction is the share of paths a weak source re-reads every
	// run under the trust-metadata preset.
	weakSampleFraction = 0.05
)

// VerificationPolicy is what a run does about content for one source: the
// strategy, the cadence under which a skipped path stops being skipped, and
// the sentence that says why.
type VerificationPolicy struct {
	Mode VerificationMode

	// ReverifyInterval is how old a path's last verification may get before
	// it is read regardless of metadata. It is zero only for VerifyAlways,
	// where there is nothing to age.
	ReverifyInterval time.Duration

	// SampleFraction is the share of paths VerifySampled re-reads each run,
	// in (0,1]. Zero for every other mode.
	SampleFraction float64

	Reason string
}

// MetadataMaySkipContent reports whether this policy permits a path's
// content to go unread on the strength of metadata. It is the one bit
// callers branch on, and it is false for VerifyAlways - and for any mode
// this method has not been taught, which is the safe direction.
func (p VerificationPolicy) MetadataMaySkipContent() bool {
	switch p.Mode {
	case VerifyRemoteHash, VerifySampled, VerifyPeriodic:
		return true
	default:
		return false
	}
}

// VerificationPolicyFor is the matrix: a trust class and an operator preset
// in, a content policy out.
//
// Two properties hold over every input, including inputs this function does
// not recognise, and both are asserted by tests rather than left to
// reading:
//
//   - VerifyNever is never returned. There is no class and no preset that
//     buys "trust the metadata forever".
//   - a policy that may skip content always names the interval after which
//     it stops skipping, so every path in every source is read on some
//     bounded cadence.
//
// An unrecognised preset is treated as PresetConservative and an
// unrecognised class as TrustUnknown, because a typo in a configuration key
// must never buy a weaker policy than the one that was asked for.
func VerificationPolicyFor(class TrustClass, preset MetadataTrustPreset) VerificationPolicy {
	conservative := preset != PresetTrustMetadata

	switch class {
	case TrustStrong:
		if conservative {
			return VerificationPolicy{
				Mode:             VerifyPeriodic,
				ReverifyInterval: strongConservativeInterval,
				Reason:           "the backend proves content identity, and the conservative preset reads every path in full monthly anyway, because a backend's hash is a claim about its own storage",
			}
		}

		return VerificationPolicy{
			Mode:             VerifyRemoteHash,
			ReverifyInterval: remoteHashReadFloor,
			Reason:           "the backend's own content hash or generation identifier is compared every run, and any path it cannot answer for is read; a full read floor catches a source that rotted under a hash it still reports",
		}

	case TrustWeak:
		if conservative {
			return VerificationPolicy{
				Mode:   VerifyAlways,
				Reason: "the source's metadata cannot prove content identity, and the conservative preset declines to guess: every file is read and hashed every run",
			}
		}

		return VerificationPolicy{
			Mode:             VerifySampled,
			ReverifyInterval: weakSampleInterval,
			SampleFraction:   weakSampleFraction,
			Reason:           fmt.Sprintf("the source's metadata cannot prove content identity, so %.0f%% of paths are re-read each run and every path is re-read at least weekly", weakSampleFraction*100),
		}

	default:
		return VerificationPolicy{
			Mode:   VerifyAlways,
			Reason: "what this source reports about its files has not been established, so metadata is used for nothing but deciding what exists",
		}
	}
}

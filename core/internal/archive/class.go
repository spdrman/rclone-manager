// Package archive owns what a storage class means for getting an
// artifact's bytes back, and the explicit operation that makes an archived
// copy readable again (EPIC E, FR-31's archive rules and FR-34).
//
// # Why a package of its own
//
// Everything else in this product assumes that a durable copy is a copy it
// can read. An archive class breaks that assumption without breaking the
// copy: the object is there, it is durable, it is exactly the bytes that
// were uploaded, and a GET of it fails anyway until somebody pays for a
// restore and waits hours for it. So "the object exists" and "I can have
// the object" stop being the same sentence, and every part of this system
// that quietly treated them as one needs a place to ask which of the two
// it actually has.
//
// This package is that place. It holds one table of what each class means,
// one closed vocabulary for what can be done with a copy right now, the
// explicit restore operation, and the refusal that follows from them in
// this package's own vocabulary: an archived copy that nobody can read is
// not a reason to delete one that somebody can (sourcedelete.go).
//
// The other refusal those facts imply, that an archived copy cannot earn a
// verification class that requires reading it, produces a verification
// class rather than an access state, so it lives beside the ladder in
// internal/placement (gate.go). That is also which way the package edge
// runs: placement imports this package to ask what a class means and
// whether a surviving copy is readable, and this package imports nothing
// of placement's. The move engine is the code that deletes a copy, and it
// has to be able to ask here before it does.
//
// # What it deliberately does not hold
//
// No prices, and no estimate of when a particular restore will finish.
// FR-34 is explicit that this product serves what it holds, and it holds
// neither a price list nor anything S3 will tell it about progress. The
// class table below carries the provider's own published figure for how
// long a restore from a class takes, which is a documented property of the
// class rather than a prediction about one object, and it says so in the
// words an operator reads.
package archive

import (
	"fmt"
	"sort"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// Behaviour is everything this product knows about reading an object back
// out of one storage class.
//
// It is the single table the REFACTOR step of #241 asks for. The class
// NAMES live in internal/config, because that is where a configuration
// value gets validated against a closed set, and what each name MEANS
// lives here, because meaning is not a validation concern. class_test.go
// pins the two together in both directions, so a class config accepts with
// no row here, or a row here for a class config would refuse, fails the
// build rather than being discovered by an operator whose restore did
// nothing.
type Behaviour struct {
	// Class is the class this row is about, spelled as S3 spells it.
	Class string

	// Archive is the whole point of this package: the object exists and
	// is durable, and its bytes cannot be read until an explicit restore
	// has been asked for and has finished.
	//
	// It is not a synonym for "cold" or "cheap". GLACIER_IR is a cold,
	// cheap class that reads on demand, so it is not archive; a caller
	// that wants "will a GET work" must read this field and not guess
	// from the name.
	Archive bool

	// RetrievalBilled is whether the provider charges to read an object
	// of this class back, over and above the egress every read costs.
	//
	// This product never states an amount (FR-34: it has no price list
	// and no negotiated rates, so any figure it printed would be made
	// up). It states the FACT that a bill exists, which it does hold, and
	// which is the part an operator needs before starting something.
	RetrievalBilled bool

	// RestoreWait is the provider's own published figure for how long a
	// standard restore from this class takes, in plain words, or empty
	// for a class that needs no restore.
	//
	// Read the wording carefully before changing it. This is a documented
	// property of the class, which this product does hold, and it is NOT
	// an estimate for a particular restore, which it does not: S3 reports
	// a restore as in progress or done and never as a percentage or an
	// ETA, so nothing here may be rendered as one. The string is written
	// to survive being pasted straight into a terminal or a UI without
	// somebody later reading it as a countdown.
	RestoreWait string
}

// behaviours is THE table. Every storage class this product accepts
// appears exactly once.
//
// The two archive rows are the reason this package exists. The five
// others are here rather than assumed, because "everything I did not list
// reads immediately" is exactly the default that turns a class nobody
// thought about into a silent claim that its bytes are available.
var behaviours = map[string]Behaviour{
	config.StorageClassStandard: {
		Class: config.StorageClassStandard,
	},
	config.StorageClassStandardIA: {
		Class:           config.StorageClassStandardIA,
		RetrievalBilled: true,
	},
	config.StorageClassOneZoneIA: {
		Class:           config.StorageClassOneZoneIA,
		RetrievalBilled: true,
	},
	config.StorageClassIntelligentTiering: {
		// INTELLIGENT_TIERING is the awkward one and it is worth saying
		// why it is not marked Archive. An object in this class reads on
		// demand while it sits in the frequent, infrequent or archive
		// instant tiers, and needs a restore only once the operator has
		// opted that bucket into one of the asynchronous archive access
		// tiers. The class name alone does not say which, and S3 reports
		// the difference in a header this product's transport boundary
		// does not carry.
		//
		// So this row states what the class itself guarantees, which is
		// that a read is not guaranteed to fail. The case where it does
		// fail is not silently wrong: a content verification against such
		// an object gets the provider's InvalidObjectState back, which
		// internal/placement turns into a capability refusal rather than
		// into a verification failure, so an unreadable object is
		// reported as unread rather than as corrupt. What must never
		// happen is the reverse, and that is what the Archive flag on the
		// two rows below prevents.
		Class: config.StorageClassIntelligentTiering,
	},
	config.StorageClassGlacierIR: {
		// Glacier Instant Retrieval reads on demand. The word Glacier in
		// the name is the trap this row exists to disarm.
		Class:           config.StorageClassGlacierIR,
		RetrievalBilled: true,
	},
	config.StorageClassGlacier: {
		Class:           config.StorageClassGlacier,
		Archive:         true,
		RetrievalBilled: true,
		RestoreWait:     "AWS publishes a standard restore from GLACIER as taking hours, typically three to five; S3 reports a restore as in progress or finished and never reports a percentage or a finishing time, so neither this product nor anything reading it can tell you when this particular restore will be done",
	},
	config.StorageClassDeepArchive: {
		Class:           config.StorageClassDeepArchive,
		Archive:         true,
		RetrievalBilled: true,
		RestoreWait:     "AWS publishes a standard restore from DEEP_ARCHIVE as taking up to twelve hours, and a bulk one up to forty eight; S3 reports a restore as in progress or finished and never reports a percentage or a finishing time, so neither this product nor anything reading it can tell you when this particular restore will be done",
	},
}

// ErrUnknownClass is returned for a storage class that has no row in the
// table above.
//
// It is deliberately an error rather than a default. The tempting default
// is "treat what I do not recognise as ordinary", and that default is a
// claim that an object is readable, made about exactly the class nobody
// thought about. Config validation already refuses an unknown class at
// load, so a Medium held in memory always carries a known one or none at
// all, which means this error firing is a drift between the two lists and
// not something an operator can cause by editing config.yaml.
var ErrUnknownClass = fmt.Errorf("archive: unknown storage class")

// Of returns what is known about class.
//
// The empty string is the medium's own default, which is STANDARD, the
// same resolution config.StorageMedium.EffectiveStorageClass makes; a
// caller holding a transport.Medium built from a medium that named no
// class gets the STANDARD row rather than an error.
func Of(class string) (Behaviour, error) {
	if class == "" {
		class = config.StorageClassStandard
	}
	b, ok := behaviours[class]
	if !ok {
		return Behaviour{}, fmt.Errorf("%w: %q", ErrUnknownClass, class)
	}
	return b, nil
}

// IsArchive reports whether class holds objects that cannot be read
// without an explicit restore first.
//
// An unrecognised class answers true, which is the opposite of what Of
// does and is deliberate: Of's caller is asking a question and can be told
// it has no answer, while this one is a predicate in the middle of a
// decision and has to resolve to something. The direction that is safe
// when nothing is known is "you cannot read this", because the cost of
// being wrong that way is a refusal an operator can override, and the cost
// of being wrong the other way is a copy deleted against bytes nobody
// could have fetched.
func IsArchive(class string) bool {
	b, err := Of(class)
	if err != nil {
		return true
	}
	return b.Archive
}

// Classes returns every storage class this table describes, sorted, so a
// test can compare it against the closed set config accepts.
func Classes() []string {
	out := make([]string, 0, len(behaviours))
	for c := range behaviours {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

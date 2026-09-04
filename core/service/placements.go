// This file is the read half of EPIC E's FR-34 at the service boundary
// (#240): where an artifact's copies actually are, whether each one can be
// read, and what has actually been proven about it.
//
// # The rule the whole file is written to
//
// Absence is never rendered as presence. There are three different answers
// hiding behind a naive "stored on offsite_s3", and this boundary keeps
// them apart:
//
//   - THERE IS NO COPY HERE. There is no placement row. An artifact still
//     transferring has none at all, and its .partial on disk is not one.
//   - THERE IS A COPY AND NOBODY CAN REACH IT. A row with access
//     "unreachable": this deployment no longer has a way to get to the
//     medium, so it can neither confirm nor deny the copy.
//   - THERE IS A COPY AND NOBODY HAS CHECKED IT. A row with no
//     verification class at all, which is not a weak pass.
//
// Issue #361 was a run cycle that backed nothing up and reported success.
// Every one of the three above, collapsed into a tick, is that same defect
// wearing a different medium.
package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// MediumTypeLocal is what a local placement reports as its kind.
//
// It is spelled here rather than borrowed from config.MediumLocal (which
// is an ID, not a type) because a caller outside core/ has to be able to
// branch on "is this copy on this machine" without string-matching a
// reserved id it was never told about.
const MediumTypeLocal = "local"

// Placement is one DURABLE copy of one artifact, as a caller outside core/
// sees it.
//
// A value of this type exists because the journal recorded a finished
// copy. It is never a placeholder for a copy that might be there: see this
// file's own doc.
type Placement struct {
	// Medium is "local" or the id of a configured storage medium.
	Medium string

	// MediumType is MediumTypeLocal or the configured medium's type.
	//
	// It is reported rather than derived by the caller so one product
	// decides what local means. A client comparing Medium against the
	// literal "local" is a client that has quietly acquired a second copy
	// of a reserved identifier.
	MediumType string

	// Location is an absolute path for a local copy and an object key for
	// a medium copy. Never a credential and never a signed URL: nothing on
	// this boundary can carry either, and FR-33's canary test asserts it.
	Location string

	// SizeBytes is what this copy measures, or nil when nobody recorded
	// it. A pointer because an artifact can genuinely be zero bytes, so a
	// zero must not double as "not reported".
	SizeBytes *int64

	// StorageClass is the medium's effective storage class, empty for a
	// local copy.
	StorageClass string

	// VerificationClass is the strongest class this copy has ACHIEVED
	// (FR-31): "content", "attested", "existence", or EMPTY when nothing
	// has verified it.
	//
	// Empty is a first-class answer and the reason this is a string rather
	// than a bool. A surface that renders empty as anything but "nobody
	// has checked this" has converted "we do not know" into "we do".
	VerificationClass string

	// VerifiedAt is when VerificationClass was last achieved, or the zero
	// time when it never has been.
	VerifiedAt time.Time

	// Access is one of placement.Accesses: whether this copy can be read
	// right now. See core/internal/placement.Access.
	Access string

	// Status is "ACTIVE" or "DELETE_PENDING". A placement the journal
	// knows is GONE never reaches here at all, because a row for it would
	// read as a copy.
	Status string
}

// StorageMediumSummary is one configured storage medium, described with
// everything a surface needs and nothing that could identify a caller to
// the provider.
//
// FR-33 lists what may never appear in an API response, and key material
// is at the top of that list. The absence here is structural rather than
// filtered: there is no field for a secret, in either direction, and
// config.MediumCredentials is not reachable from this type at all.
type StorageMediumSummary struct {
	ID           string
	Type         string
	Bucket       string
	Region       string
	StorageClass string

	// ReadsRequireRestore is true when this medium's storage class cannot
	// be read on demand. It is computed here, from the same predicate the
	// engine's own access states use, so a surface never has to hold its
	// own list of which classes are archive.
	ReadsRequireRestore bool
}

// VerificationClassInfo is one rung of FR-31's ladder, with the engine's
// own words for what it proves and what it takes.
//
// The strings come from core/internal/placement, not from this package and
// not from a frontend. That is the point: a surface with its own copy of
// what "existence" proves eventually says something the engine does not,
// and the sentence an operator reads when deciding whether a backup is
// safe is a bad place for a stale paraphrase.
type VerificationClassInfo struct {
	Class  string
	Proves string

	// Requires is placement.Class.Cost, under the name it carries on the
	// wire. The engine's word is fine where it lives; on the contract, a
	// field called "cost" is a field one release away from holding a
	// figure this product cannot compute honestly, and the contract's
	// no-cost gate (core/tests/compat) refuses the name outright. The
	// words are the same either way: what achieving this class takes.
	Requires string

	// DownloadsObject is placement.Class.CostsEgress, renamed for the
	// same reason: it is the same predicate the engine refuses automatic
	// medium revalidation on, served rather than restated. It is a
	// mechanism, and this is where the surface reads it from.
	DownloadsObject bool
}

// StorageSchemaInfo is the closed sets and the consent text a
// storage-medium mapping is written against, served for the reason
// RetentionSchemaInfo is served: so a form validates against the rules the
// engine applies rather than a transcription of them.
type StorageSchemaInfo struct {
	VerificationClasses []VerificationClassInfo

	// MediumDisclosure is the consent text FR-27 requires an operator be
	// shown before the first save that sends a tier's artifacts off local
	// disk. UpdateSettings refuses such a write without an
	// acknowledgment, and this is the text behind that refusal, so a
	// client renders the product's words rather than its own.
	MediumDisclosure string

	// RetrievalDisclosure is the plain statement about reading a copy back
	// off a medium. It carries no figure, and it never will: this product
	// has no price list and no knowledge of what an operator negotiated,
	// so a number here would be invented (the #211 rule).
	RetrievalDisclosure string
}

// The two disclosures, in one place, spelled once.
//
// They are constants rather than something a caller assembles because the
// UI and the CLI both have to say the same thing, and because the second
// sentence of MediumDisclosure is the whole consent: an operator who reads
// past it has still been told that the copy on their NAS gets deleted.
const (
	mediumDisclosureText = "Backups that only this tier keeps will live only on that storage medium. " +
		"After a backup uploads and I verify it, I delete the copy on this machine. " +
		"That deletion is what the setting is for, and once this is saved it happens " +
		"automatically whenever retention runs, with no further prompt. " +
		"A medium on an archive storage class cannot be read on demand at all: getting a " +
		"backup back means asking for a restore and waiting hours, and the provider " +
		"reports no progress while it waits."

	retrievalDisclosureText = "Reading a copy back off a storage medium is billed by your provider. " +
		"I hold no price list and no knowledge of your rates, so I report the bytes and the " +
		"storage class and stop there rather than showing you a number I made up."
)

// StorageSchema reports the vocabulary and the consent text a storage
// medium is configured against. A package-level function for
// RetentionSchema's reason: the answer is a property of the product, not
// of any one running service.
func StorageSchema() StorageSchemaInfo {
	classes := make([]VerificationClassInfo, 0, len(placement.Classes))
	for _, c := range placement.Classes {
		classes = append(classes, VerificationClassInfo{
			Class:           string(c),
			Proves:          c.Proves(),
			Requires:        c.Cost(),
			DownloadsObject: c.CostsEgress(),
		})
	}
	return StorageSchemaInfo{
		VerificationClasses: classes,
		MediumDisclosure:    mediumDisclosureText,
		RetrievalDisclosure: retrievalDisclosureText,
	}
}

// mediumIndex is what the running configuration says about the places
// placements name, keyed by medium id.
//
// It exists because a placement row alone cannot answer "can this be read"
// (see placement.MediumFacts): whether this deployment still has a way to
// reach a medium is a fact about config.yaml, and only this layer holds
// both halves.
type mediumIndex map[string]config.StorageMedium

func indexMediums(cfg *config.Config) mediumIndex {
	out := make(mediumIndex, len(cfg.StorageMediums))
	for _, m := range cfg.StorageMediums {
		out[m.ID] = m
	}
	return out
}

// factsFor is what placement.AccessOf needs to know about one placement's
// medium.
func (idx mediumIndex) factsFor(p state.Placement) placement.MediumFacts {
	if p.IsLocal() {
		return placement.MediumFacts{Declared: true}
	}
	m, declared := idx[p.Medium]
	if !declared {
		return placement.MediumFacts{Declared: false}
	}
	return placement.MediumFacts{Declared: true, StorageClass: m.EffectiveStorageClass()}
}

// toServicePlacements projects an artifact's journal placements onto the
// boundary.
//
// GONE rows are dropped, and that is the load-bearing line in this
// function. state.PlacementGone means "the copy is no longer there and the
// journal knows it", so serving it would put a row on a surface that reads
// as a copy in every layout anyone would write for one. Absence of a copy
// is absence of a row, which is the same thing an artifact that never had
// one reports, and it is what makes an empty list mean exactly one thing.
func toServicePlacements(recs []state.Placement, idx mediumIndex) []Placement {
	out := make([]Placement, 0, len(recs))
	for _, p := range recs {
		if p.Status == state.PlacementGone {
			continue
		}
		out = append(out, toServicePlacement(p, idx))
	}
	return out
}

func toServicePlacement(p state.Placement, idx mediumIndex) Placement {
	out := Placement{
		Medium:            p.Medium,
		MediumType:        MediumTypeLocal,
		Location:          p.Location,
		SizeBytes:         p.Size,
		VerificationClass: p.VerificationClass,
		Status:            p.Status,
		Access:            string(placement.AccessOf(p, idx.factsFor(p))),
	}
	if !p.IsLocal() {
		// A placement naming a medium the configuration no longer declares
		// keeps its own id and reports no type and no class, because this
		// deployment genuinely does not know either any more. Inventing
		// "s3" here would be guessing, and the access state already says
		// the honest thing about it.
		if m, declared := idx[p.Medium]; declared {
			out.MediumType = m.Type
			out.StorageClass = m.EffectiveStorageClass()
		} else {
			out.MediumType = ""
		}
	}
	if p.VerifiedAt != nil {
		out.VerifiedAt = *p.VerifiedAt
	}
	return out
}

// toStorageMediumSummaries projects the configured mediums onto the
// settings boundary, in declaration order.
func toStorageMediumSummaries(cfg *config.Config) []StorageMediumSummary {
	out := make([]StorageMediumSummary, 0, len(cfg.StorageMediums))
	for _, m := range cfg.StorageMediums {
		class := m.EffectiveStorageClass()
		out = append(out, StorageMediumSummary{
			ID:                  m.ID,
			Type:                m.Type,
			Bucket:              m.Bucket,
			Region:              m.Region,
			StorageClass:        class,
			ReadsRequireRestore: placement.StorageClassNeedsRestore(class),
		})
	}
	return out
}

// ErrMediumDisclosureRequired is the refusal FR-27's consent gate
// produces: a settings write that would send a retention tier's artifacts
// to a non-local medium, arriving without the operator's acknowledgment.
//
// It is its own sentinel rather than an ErrInvalidRequest, because a
// caller has to be able to tell "you sent me something malformed" from
// "you sent me something I understood perfectly and will not do until
// somebody has read this". Those call for opposite UI: one is a field to
// fix, the other is a paragraph to read.
var ErrMediumDisclosureRequired = errors.New("service: this settings write needs the storage-medium disclosure acknowledged")

// tierMedium is one tier-to-medium mapping, named by both halves because
// the refusal has to say which tier's artifacts are about to leave.
type tierMedium struct {
	Tier   string
	Medium string
}

// newTierMediumMappings reports the tier-to-medium mappings submitted
// would ADD to the chain currently in force: the ones the operator is
// consenting to, and nothing else.
//
// It is shared by the two writes that can introduce one, the deployment's
// policy (UpdateSettings) and one backup set's own (SetBackupSetRetention),
// because an override is a whole chain in its own right and can name a
// medium exactly as the global policy can. A gate that only stood in front
// of the settings write would be a gate one PUT walks around.
//
// # Why per tier rather than per medium
//
// FR-27 requires the disclosure "before the first save that maps any tier
// of a backup-affecting chain to a non-local medium". Read distributively
// over tiers, which is how this reads it: each tier-to-medium pair is its
// own deletion consequence, for its own set of artifacts, on a medium with
// its own storage class. A configuration that already sends monthly to
// offsite_s3 has consented to monthly leaving; it has not consented to
// daily leaving, and it certainly has not consented to annual going to an
// archive class nothing can read without a restore.
//
// A pure rename of a tier that keeps its medium therefore asks again. That
// is the safe direction and it costs one checkbox, which is a better trade
// than a rule subtle enough that a save could slip an artifact set off
// local disk without anybody being told.
//
// Nothing here is a validation rule. Whether a named medium exists at all
// is config.Validate's question, asked over the whole config a few lines
// later; a dangling medium refused there is refused for the same reason
// the same file hand-edited would be.
func newTierMediumMappings(inForce []config.RetentionTier, submitted []RetentionTier) []tierMedium {
	// Only a submitted chain can introduce a mapping. A write that leaves
	// the chain alone leaves every mapping exactly as the file has it,
	// including a mapping an operator put there by hand, which this gate
	// has no business re-litigating.
	if len(submitted) == 0 {
		return nil
	}

	before := make(map[string]string, len(inForce))
	for _, t := range inForce {
		before[t.Name] = t.EffectiveMedium()
	}

	var introduced []tierMedium
	for _, t := range submitted {
		medium := t.Medium
		if medium == "" || medium == config.MediumLocal {
			continue
		}
		if was, existed := before[t.Name]; existed && was == medium {
			continue
		}
		introduced = append(introduced, tierMedium{Tier: t.Name, Medium: medium})
	}
	return introduced
}

// mediumDisclosureRefusal builds the refusal, and the refusal IS the
// disclosure.
//
// A caller that only got "acknowledgment required" back would have to hold
// its own copy of what is being acknowledged, and a second copy of a
// consent text is a consent text that drifts from what the product
// actually does. So the words travel with the refusal, and a client that
// renders the message has, by construction, shown the operator the right
// thing.
func mediumDisclosureRefusal(introduced []tierMedium) error {
	pairs := make([]string, 0, len(introduced))
	for _, m := range introduced {
		pairs = append(pairs, fmt.Sprintf("%s -> %s", m.Tier, m.Medium))
	}
	return fmt.Errorf("%w. This write sends %s. %s %s",
		ErrMediumDisclosureRequired,
		strings.Join(pairs, ", "),
		mediumDisclosureText,
		retrievalDisclosureText)
}

// AccessStates, VerificationClasses and PlacementStatuses are the closed
// vocabularies this boundary serves, re-exported from the engine's own
// constants.
//
// They exist because core/internal is not importable from apps/ or from
// anything outside core/, so without them the only way for the API layer
// to hold its contract to the engine's vocabulary would be a second,
// hand-written copy of each list, in the layer whose whole job is to not
// have one. Deriving them here means the /api/v1 enum, the generated
// TypeScript union the UI narrows against, and the strings the engine
// actually writes to the journal all come from one place, which is what
// makes a drift test possible rather than decorative.
// # Where the access vocabulary will come from
//
// #241 (E2.4) is landing core/internal/archive, which defines the same
// four values with the same strings and derives them from more than this
// package can see: it knows whether a restore is running and when a
// finished one expires. When it lands, this function should return its
// vocabulary and toServicePlacement should ask it for the state, and
// nothing else in the API or the UI moves: the contract enum, the drift
// test that pins it, the generated TypeScript union and every surface
// narrowing against it all read THIS function. That is the whole reason
// it exists rather than each layer holding its own list.
func AccessStates() []string {
	out := make([]string, 0, len(placement.Accesses))
	for _, a := range placement.Accesses {
		out = append(out, string(a))
	}
	return out
}

// VerificationClasses is FR-31's ladder, strongest first. The order is
// meaningful: it is what "stronger than" means.
//
// The empty class is deliberately not in it. "Nothing has verified this
// copy" is spelled by the absence of a class, not by a rung named
// nothing, and a surface offered an empty rung will eventually render it
// as one.
func VerificationClasses() []string {
	out := make([]string, 0, len(placement.Classes))
	for _, c := range placement.Classes {
		out = append(out, string(c))
	}
	return out
}

// PlacementStatuses is the two statuses a copy on this boundary can carry.
//
// state.PlacementGone is NOT among them, and its absence is the contract:
// a copy the journal knows is gone is not served at all, so a client is
// never handed a row it would have to know not to render.
func PlacementStatuses() []string {
	return []string{state.PlacementActive, state.PlacementDeletePending}
}

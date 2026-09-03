package placement

import (
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// Access says whether one copy of an artifact can be READ right now
// (EPIC E, FR-34).
//
// # Why this is not a boolean, and not a sentence
//
// "Is my backup there" and "can I have my backup" stopped being the same
// question the moment a copy could live on an object store. An object on
// DEEP_ARCHIVE is there, in the sense that the journal's row for it is
// true and a HEAD request answers; it is also hours away from being
// readable, and getting it costs money. A surface that renders that as
// "stored on offsite_cold" beside a green tick has told an operator
// something that will be false in the only moment they care about, which
// is the moment they need the file back.
//
// So a placement carries an access state from a closed set, and this
// package is where that state is decided. The values are deliberately not
// prose: the API serves them as an enum, the CLI prints from the same
// enum, and a surface that wants a sentence writes one against a value it
// cannot mistype.
type Access string

const (
	// AccessImmediate means a read starts now: a local copy, or a medium
	// copy on a class that serves objects on demand.
	AccessImmediate Access = "immediate"

	// AccessRequiresRestore means the bytes are there and cannot be read
	// until an explicit restore has been asked for and has finished. S3
	// does not say how long that takes, so nothing here estimates it.
	AccessRequiresRestore Access = "requires_restore"

	// AccessRestoring means a restore has been asked for and has not
	// finished. S3 reports no percentage, so no percentage is shown.
	//
	// Nothing produces this value yet: the restore operation is #241
	// (E2.4), and AccessOf below has no path that returns it. It is in the
	// vocabulary now, and served in the contract now, because the closed
	// set is what every surface narrows against, and a set that grows
	// later is a set every surface has to be revisited for. AccessesTest
	// pins the gap so it reads as coordination rather than as a bug.
	AccessRestoring Access = "restoring"

	// AccessUnreachable means this deployment cannot currently get to the
	// place the copy is in, so nothing can confirm it either way.
	//
	// It is emphatically NOT "the copy is gone". Those two call for
	// opposite responses (one is an outage or a configuration mistake, the
	// other is a lost backup), and a surface that collapses them either
	// panics an operator over a removed config block or reassures one
	// whose bucket has actually been emptied.
	AccessUnreachable Access = "unreachable"
)

// Accesses is the closed vocabulary, in the order a surface reads best:
// from "you can have it now" to "nobody can say".
var Accesses = []Access{AccessImmediate, AccessRequiresRestore, AccessRestoring, AccessUnreachable}

// Valid reports whether a is one of the four.
func (a Access) Valid() bool {
	for _, known := range Accesses {
		if a == known {
			return true
		}
	}
	return false
}

// MediumFacts is everything the RUNNING CONFIGURATION says about the place
// a placement names. It is the caller's job to supply it, because this
// package cannot load a config and the journal does not hold one: a
// placement row records where bytes were put, and whether this deployment
// still has a way to get back there is a fact about config.yaml, not about
// the artifact.
type MediumFacts struct {
	// Declared is whether the running configuration still declares this
	// medium. False is the case that matters: an operator removed the
	// storage_mediums entry (or restored a config from before it existed)
	// while artifacts still live there. There is then no bucket, no
	// endpoint and no credential to reach it with, which is exactly what
	// "unreachable" means, and it is a state the journal alone cannot
	// see.
	Declared bool

	// StorageClass is the medium's effective storage class. Empty for the
	// local medium, which has no such thing.
	StorageClass string
}

// ArchiveStorageClasses are the classes whose objects cannot be read until
// an explicit restore has run (FR-31, FR-34).
//
// GLACIER_IR is deliberately NOT here. Instant Retrieval serves objects on
// demand; it is a price tier, not an access tier, and treating it as
// archive would tell an operator to wait hours for a file they could have
// had at once.
//
// #241 (E2.4) is the issue that adds the restore operation itself. If it
// grows its own idea of which classes are archive, that idea and this one
// have to become one function, not two.
var ArchiveStorageClasses = []string{config.StorageClassGlacier, config.StorageClassDeepArchive}

// StorageClassNeedsRestore reports whether class has to be restored before
// anything can read it.
func StorageClassNeedsRestore(class string) bool {
	for _, archive := range ArchiveStorageClasses {
		if class == archive {
			return true
		}
	}
	return false
}

// AccessOf reports whether p can be read right now, given what the running
// configuration says about the place it names.
//
// It reads no network and takes no lock. That is the point: this answer is
// rendered on every artifact list, and an access state that cost a HEAD
// request per placement would either be slow or be cached, and a cached
// answer about whether a backup is reachable is a stale answer presented
// as a current one.
//
// There is no path here that returns AccessRestoring; see that constant.
func AccessOf(p state.Placement, m MediumFacts) Access {
	if p.IsLocal() {
		// The local medium is always declared (it is the backup set's own
		// root) and has no storage class. Whether the FILE is readable is a
		// different question, asked by verification, and answered by the
		// verification class rather than by inventing a reachability check
		// this function cannot honestly perform.
		return AccessImmediate
	}
	if !m.Declared {
		return AccessUnreachable
	}
	if StorageClassNeedsRestore(m.StorageClass) {
		return AccessRequiresRestore
	}
	return AccessImmediate
}

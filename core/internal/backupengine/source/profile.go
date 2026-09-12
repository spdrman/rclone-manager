package source

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/internal/backend"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/sourceconsistency"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// ErrUnstreamable is the refusal for a backend whose objects cannot be
// read as a stream. There is no configuration that fixes it and no
// fallback offered, because the fallback would be a staging copy, which
// is the design this package exists to make unavailable.
var ErrUnstreamable = errors.New("source: this backend cannot open an object as a stream, and this adapter will not stage a copy of it instead")

// ErrUndetectableMutation is the refusal for a backend that declares
// nothing a read window can be checked against: no stable size, no
// readable modification time, no generation identity.
//
// It is a refusal rather than a warning because the alternative is
// recording bytes as verified with nothing behind the claim. An operator
// whose source really cannot answer has a configuration answer available
// - a consistency mode that guarantees a point in time, where the source
// provably cannot move while it is read - and that mode is honoured here.
var ErrUndetectableMutation = errors.New("source: this backend reports nothing that could show an object changed while it was being read")

// Profile is one backend's capability matrix, projected onto the
// questions this adapter asks. It is the only place a capability is read,
// so a capability that has to be re-derived somewhere else is a bug in
// this type rather than a second opinion.
type Profile struct {
	// BackendID is the manifest id ("local_volume"), not the rclone
	// backend name ("local").
	BackendID string

	caps backend.Capabilities
}

// ProfileFor resolves the bundled manifest for an rclone backend name and
// projects it.
//
// A transport no manifest describes is a refusal (the registry's
// ErrUnqualifiedBackend), not a permissive default: this build knows what
// it knows, and a source backed up on the assumption that it "probably
// behaves like the others" is a restore point resting on nothing.
func ProfileFor(rcloneBackend string) (Profile, error) {
	reg, err := backend.Bundled()
	if err != nil {
		return Profile{}, fmt.Errorf("reading the bundled backend registry: %w", err)
	}

	m, err := reg.ByRcloneBackend(rcloneBackend)
	if err != nil {
		return Profile{}, err
	}

	caps, declared := m.DeclaredCapabilities()
	if !declared {
		return Profile{}, fmt.Errorf(
			"%w: backend %q declares no capability matrix, so nothing says what may be read from it or believed about it",
			backend.ErrUnqualifiedBackend, m.ID)
	}

	return Profile{BackendID: m.ID, caps: caps}, nil
}

// NewProfile builds a profile from a capability matrix directly, for the
// callers that already hold one and for the tests that need a backend
// this build does not ship - a source that stores symlinks, a source with
// no usable metadata at all - to prove the refusals fire.
func NewProfile(backendID string, caps backend.Capabilities) Profile {
	return Profile{BackendID: backendID, caps: caps}
}

// Capabilities returns the matrix this profile projects, unmodified.
func (p Profile) Capabilities() backend.Capabilities { return p.caps }

// StableSize reports whether a size this backend reports describes the
// bytes a read of the object will return.
//
// It is asked separately from StatOf because the two use it for opposite
// purposes: StatOf leaves the size out when it is not stable, so a
// comparison does not rest on it, and the read check needs to know that
// the size is absent for that reason rather than because the object is
// empty.
func (p Profile) StableSize() bool { return p.caps.StableSize }

// Streamable reports whether an object on this backend may be opened as a
// stream, and refuses by name when it may not.
func (p Profile) Streamable() error {
	if !p.caps.StreamingOpen {
		return fmt.Errorf("%w: %q", ErrUnstreamable, p.BackendID)
	}

	return nil
}

// Enumerable reports whether this backend's directories may be walked
// within a memory bound.
//
// The decision belongs to backend.Manifest.PlanEnumeration and is asked
// of the matrix here rather than re-derived, which is why this method is
// three lines: a second implementation of "may I list this" is a second
// chance to answer yes.
func (p Profile) Enumerable() error {
	if !p.caps.BoundedListing {
		return fmt.Errorf("%w: %q. Back this source up from an explicit path list instead", backend.ErrUnboundedListing, p.BackendID)
	}

	return nil
}

// ModTime turns the unix-seconds timestamp a transport reports into the
// time this adapter will record, or the zero time when the backend keeps
// no timestamp this engine can read.
//
// The zero time is the point of the method. A backend whose
// mtime_precision is "unknown" has not said its timestamps mean anything,
// and recording 1970-01-01 - which is what a zero unix second becomes if
// it is passed through unexamined - is fabricating a fact about the
// operator's data. Nothing downstream reuses content on a modification
// time (see backupengine.StreamSnapshotRequest), so an absent one costs
// nothing but honesty.
func (p Profile) ModTime(unixSeconds int64) time.Time {
	if unixSeconds == 0 {
		return time.Time{}
	}

	if _, ok := model.MTimePrecision(p.caps.MTimePrecision).Resolution(); !ok {
		return time.Time{}
	}

	return time.Unix(unixSeconds, 0).UTC()
}

// ContentIdentity returns the artifact's identifier if - and only if -
// this backend names an object's CONTENT with it.
//
// generation_identity is the one key that answers this, which is what its
// documentation in the matrix says and why nothing here looks at anything
// else. A path is a slot: it survives an overwrite, so two different sets
// of bytes have the same one, and using it as a content identity is
// precisely how a rewritten file passes as unchanged. "unknown" is
// treated as "none": an identifier nobody has classified is not evidence.
func (p Profile) ContentIdentity(a transport.RemoteArtifact) string {
	if p.caps.GenerationIdentity != backend.GenerationVersioned {
		return ""
	}

	return a.ID
}

// MutationDetectable reports whether a read window on this backend can be
// checked at all, and refuses by name when it cannot.
//
// mode is consulted because a mode that guarantees a point in time
// answers the question a different way: a frozen image cannot change
// while it is read, so there is nothing for a post-read comparison to
// find and its absence costs nothing.
func (p Profile) MutationDetectable(mode model.ConsistencyMode) error {
	if mode.GuaranteesPointInTime() {
		return nil
	}

	if p.caps.StableSize {
		return nil
	}

	if _, ok := model.MTimePrecision(p.caps.MTimePrecision).Resolution(); ok {
		return nil
	}

	if p.caps.GenerationIdentity == backend.GenerationVersioned {
		return nil
	}

	return fmt.Errorf("%w: %q under consistency mode %q", ErrUndetectableMutation, p.BackendID, mode)
}

// StatOf reads one artifact as the facts a read window is compared
// across.
//
// Two narrowings are applied and both are the same rule: a fact the
// backend does not report is left empty rather than defaulted, because an
// empty field is read as "no opinion" by
// sourceconsistency.DescribeMovement while a defaulted one is read as
// evidence. Size is carried only where the matrix says a reported size
// describes the bytes a read returns; identity only where the backend
// names content.
func (p Profile) StatOf(a transport.RemoteArtifact) sourceconsistency.Stat {
	stat := sourceconsistency.Stat{
		Kind:     KindOf(a.Kind),
		Identity: p.ContentIdentity(a),
	}

	if p.caps.StableSize {
		stat.Size = a.Size
	}

	if t := p.ModTime(a.ModTime); !t.IsZero() {
		stat.ModTimeNanos = t.UnixNano()
	}

	return stat
}

// Signals is the trust projection this backend feeds to the verification
// policy, taken from the one authority that owns it.
func (p Profile) Signals() model.SourceSignals {
	return sourceconsistency.SignalsFromCapabilities(p.BackendID, p.caps)
}

// SymlinkSemantics is what a symbolic link IS on this backend.
func (p Profile) SymlinkSemantics() backend.SymlinkSemantics {
	return p.caps.SymlinkSemantics
}

// FoldsCase reports whether exclusion matching against this backend has
// to ignore case.
//
// It is true unless the backend declares itself case-sensitive, which
// makes "unknown" - the honest answer for a local volume and for sftp,
// where the truth belongs to whichever filesystem is mounted at the
// operator's path - behave like case-insensitive. That asymmetry is
// deliberate and it is chosen by what each mistake costs: an exclusion
// that matches too much leaves a file out of a backup, which the run
// report says out loud, and an exclusion that matches too little backs up
// the directory of secrets an operator explicitly named. Only one of
// those is discovered by reading the report.
func (p Profile) FoldsCase() bool {
	return p.caps.CaseSensitivity != backend.CaseSensitive
}

// ExcludeMatcher builds the predicate that says whether one source path
// is excluded, honouring this backend's case answer.
//
// A prefix match, not a glob: transport.Source.ExcludePaths is a list of
// paths relative to the root, and every entry at or beneath one is
// excluded, which is what excluding a directory has to mean.
func (p Profile) ExcludeMatcher(excludes []string) func(string) bool {
	if len(excludes) == 0 {
		return func(string) bool { return false }
	}

	fold := p.FoldsCase()
	prepared := make([]string, 0, len(excludes))

	for _, e := range excludes {
		e = strings.Trim(e, "/")
		if e == "" {
			continue
		}
		if fold {
			e = strings.ToLower(e)
		}
		prepared = append(prepared, e)
	}

	return func(candidate string) bool {
		if fold {
			candidate = strings.ToLower(candidate)
		}
		for _, e := range prepared {
			if Contains(e, candidate) {
				return true
			}
		}

		return false
	}
}

// KindOf maps the transport's answer about a directory entry onto the
// vocabulary a capture speaks.
//
// It is the one place the two meet, and it is a function rather than a
// shared type because the two packages answer to different owners:
// transport describes what a filesystem said, sourceconsistency describes
// what a capture may claim. An unknown kind maps to KindOther, which is
// the answer that skips the entry rather than the answer that opens it.
func KindOf(k transport.EntryKind) sourceconsistency.Kind {
	switch k {
	case transport.EntryKindRegular:
		return sourceconsistency.KindRegular
	case transport.EntryKindSymlink:
		return sourceconsistency.KindSymlink
	case transport.EntryKindOther, transport.EntryKindUnknown:
		return sourceconsistency.KindOther
	default:
		return sourceconsistency.KindOther
	}
}

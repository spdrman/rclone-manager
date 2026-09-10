package backend

import "errors"

// ErrUnknownBackend is what Registry.Backend returns for an id no
// manifest declares.
var ErrUnknownBackend = errors.New("backend: no manifest declares this backend")

// ErrMalformedManifest is what Load returns, wrapped, when any embedded
// or supplied manifest fails to parse or fails one of the rules validate.go
// enforces. See doc.go, "A malformed bundled manifest fails the whole
// registry": Load never returns the good manifests alongside a warning
// about the bad one.
var ErrMalformedManifest = errors.New("backend: manifest is malformed")

// manifestError is one problem this package found, wrapping either
// sentinel above with %w so errors.Is still sees it once several
// problems are joined into one error by Load.
//
// A named string type rather than errors.New for the individual
// messages, mirroring transport.mediumKeyError and rclone.credentialsError
// and for their stated reason: two unrelated refusals from this package
// must never compare equal by an == or a switch that meant to compare
// something else.
type manifestError string

func (e manifestError) Error() string { return string(e) }

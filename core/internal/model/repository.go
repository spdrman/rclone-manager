// The boundary EPIC K calls a Repository Domain, and the refusals that
// make it a boundary rather than a label.
//
// A content-addressed repository is one encryption key, one credential, one
// deduplication span, one maintenance owner, one corruption blast radius
// and one set of administrators. Those six are not separable: they are
// properties of the repository, so two backup sets stored in one repository
// share all of them, whatever the config file's intent was.
//
// # Why this needs a type instead of a comment
//
// The pressure runs one way. Deduplication is better the more data shares a
// repository, maintenance is cheaper when there is one repository to
// maintain, and credentials are simpler when there is one to hold. Every
// operational instinct therefore pushes towards a single repository for
// everything, and the cost only becomes visible during the incident that
// makes it matter: a repository whose format blob is damaged takes every
// set in it, a passphrase that has to be rotated has to be rotated for
// everyone, and a customer whose data must be isolated is not isolated by
// being in a different directory of the same repository.
//
// So the domain is declared, its co-tenancy is stated rather than inferred,
// and an attempt to share across one is refused by this package with a
// message that names what would have been shared. A refusal that just says
// "not allowed" is an assertion somebody overrules; a refusal that lists
// the six is an argument.
//
// # What is deliberately not here
//
// Where a repository's bytes live, how it is unlocked, and what maintenance
// it needs are all absent: those belong to core/internal/backupengine and
// its adapter (#781), and a domain that carried a storage location would
// make this package the place a second repository-storage vocabulary grows.
// A domain is the boundary. The repository behind it is somebody else's
// type.

package model

import (
	"errors"
	"fmt"
	"strings"
)

// ErrForeignDomain is returned when a reference names a domain other than
// the one being asked, including when two references naming different
// domains are asked to share a repository. It is distinct from
// ErrIsolationViolated because the operator response differs: this one is
// "these two sets belong in different repositories", which is usually
// correct and final.
var ErrForeignDomain = errors.New("model: backup set belongs to another repository domain")

// ErrIsolationViolated is returned when a domain declared isolated is asked
// to hold a second backup set. The way out is a real decision an operator
// can make -- declare the domain shared, or point the second set at another
// domain -- which is why it is distinguishable from ErrForeignDomain.
var ErrIsolationViolated = errors.New("model: repository domain is isolated and already holds another backup set")

// RepositoryDomainID names one repository domain, e.g. production,
// security-sensitive or customer-a.
//
// It is a validated type rather than a bare string for the reason
// BackupSetID is: this value decides which repository a set's snapshots go
// into, so two ids that an operator typed differently must never quietly
// become one, and an id that can name a parent directory must never be
// buildable by concatenation.
type RepositoryDomainID string

// NewRepositoryDomainID validates a domain id and returns it.
//
// The rules are BackupSetID's (see validPart: not empty, no path
// separator, no surrounding whitespace, no control characters) plus the two
// this value needs on its own account. A domain id is destined to name a
// directory and a credential in #781, so "." and ".." are refused rather
// than cleaned, and a backslash is refused for the same reason the forward
// slash is: a domain whose name traverses is a repository somewhere nobody
// declared.
func NewRepositoryDomainID(s string) (RepositoryDomainID, error) {
	if err := validPart("repository domain id", s); err != nil {
		return "", err
	}

	switch s {
	case ".", "..":
		return "", fmt.Errorf("repository domain id %q is a relative path, not a name", s)
	}

	if strings.ContainsAny(s, `\`) {
		return "", fmt.Errorf("repository domain id %q must not contain a path separator", s)
	}

	return RepositoryDomainID(s), nil
}

func (d RepositoryDomainID) String() string { return string(d) }

// IsZero reports whether this is the unset value. A zero domain id must be
// treated as a programming error rather than as a wildcard: two references
// carrying one would compare equal and look like co-tenants of the same
// nameless repository.
func (d RepositoryDomainID) IsZero() bool { return d == "" }

// RepositoryIsolation is whether a domain may hold more than one backup
// set. It is stated, never inferred, and it has no zero value that means
// anything: see RepositoryDomain.Validate.
type RepositoryIsolation string

const (
	// RepositoryShared is a domain whose sets are intentionally
	// co-tenants: they deduplicate against each other, share one key and
	// one credential, and fail together. This is the ordinary answer for
	// one deployment's own data, and it is the answer that buys the
	// deduplication an incremental engine exists for.
	RepositoryShared RepositoryIsolation = "shared"

	// RepositoryIsolated is a domain that holds exactly one backup set:
	// the customer-a/customer-b case, and the security-sensitive case
	// where an operator has decided that cross-set deduplication is not
	// worth a shared key and a shared corruption fate.
	//
	// It is enforced (see MayShare) rather than documented, because the
	// way an isolation boundary dissolves is not a decision, it is an
	// edit: somebody points a second set at the domain that was already
	// configured and working.
	RepositoryIsolated RepositoryIsolation = "isolated"
)

// RepositoryIsolations is both answers. There is no third, and the absence
// is deliberate: "shared with these two other sets" is a repository per
// group, which is what declaring another domain already expresses.
var RepositoryIsolations = []RepositoryIsolation{RepositoryShared, RepositoryIsolated}

func (i RepositoryIsolation) String() string { return string(i) }

// ParseRepositoryIsolation reads an isolation back from configuration, and
// refuses everything else including silence.
//
// Silence is refused rather than defaulted, and that is the whole point of
// the key. A default of "shared" would mean a security boundary that exists
// only in the head of whoever wrote the config, and a default of "isolated"
// would silently forgo the deduplication that is the reason to run this
// engine at all. An operator declaring a repository domain is declaring a
// boundary; they have to say which one.
func ParseRepositoryIsolation(s string) (RepositoryIsolation, error) {
	for _, isolation := range RepositoryIsolations {
		if string(isolation) == s {
			return isolation, nil
		}
	}

	if s == "" {
		return "", fmt.Errorf("a repository domain must state its isolation (%q or %q); co-tenancy is never inferred",
			RepositoryShared, RepositoryIsolated)
	}

	return "", fmt.Errorf("unknown repository isolation %q (expected %q or %q)", s, RepositoryShared, RepositoryIsolated)
}

// RepositoryBoundary names one of the things every co-tenant of a
// repository shares. The strings are what a refusal prints, so they are the
// vocabulary an operator reads when they are told two sets may not share a
// repository.
type RepositoryBoundary string

const (
	// BoundaryEncryption is the repository's encryption key. One
	// repository, one key: a set in it can be read by anything that can
	// read the repository.
	BoundaryEncryption RepositoryBoundary = "encryption"

	// BoundaryCredential is what unlocks the repository. Rotating it
	// rotates it for every set in the domain, and anything holding it
	// holds all of them.
	BoundaryCredential RepositoryBoundary = "credential"

	// BoundaryCorruption is the blast radius. A damaged format blob, index
	// or content blob is damage to the repository, not to one set in it.
	BoundaryCorruption RepositoryBoundary = "failure_corruption"

	// BoundaryMaintenance is the repository-wide maintenance that reclaims
	// space. It has one owner and repository-wide safety semantics, so
	// every set in the domain is quiesced, fenced and blocked by the same
	// maintenance.
	BoundaryMaintenance RepositoryBoundary = "maintenance"

	// BoundaryDeduplication is the content span. It is the benefit
	// co-tenancy buys and it is also an information channel: identical
	// content across two sets is stored once, which is observable.
	BoundaryDeduplication RepositoryBoundary = "deduplication"

	// BoundaryAdministrativeTrust is who may act on the repository.
	// Administering one set in a domain means administering the
	// repository that holds the others.
	BoundaryAdministrativeTrust RepositoryBoundary = "administrative_trust"
)

// RepositoryBoundaries is every boundary co-tenancy crosses, which is
// exactly what sharing a repository means.
//
// There is deliberately no way to share some and not others. A field
// offering that would be a promise the storage layer cannot keep: the
// deduplication span and the encryption key are the same repository.
var RepositoryBoundaries = []RepositoryBoundary{
	BoundaryEncryption,
	BoundaryCredential,
	BoundaryCorruption,
	BoundaryMaintenance,
	BoundaryDeduplication,
	BoundaryAdministrativeTrust,
}

// RepositoryDomain is one declared boundary: an identity, the operator's
// own description of what it is for, and whether it admits co-tenants.
type RepositoryDomain struct {
	ID RepositoryDomainID

	// Description is the operator's sentence about what this domain holds.
	// It is optional and never interpreted; it exists because a domain
	// list with five ids and no prose is a list nobody can review, and
	// reviewing it is how an isolation mistake gets caught before an
	// audit.
	Description string

	// Isolation is stated, never derived. See Validate.
	Isolation RepositoryIsolation
}

// Validate reports whether this domain is fully declared.
//
// The isolation check is the load-bearing one: a zero Isolation is not a
// permissive default here, it is an incomplete declaration, and accepting
// it would turn "this repository is isolated" into a belief rather than a
// boundary.
func (d RepositoryDomain) Validate() error {
	if d.ID.IsZero() {
		return errors.New("repository domain has no id; an unnamed boundary cannot be referenced or enforced")
	}

	if _, err := ParseRepositoryIsolation(string(d.Isolation)); err != nil {
		return fmt.Errorf("repository domain %q: %w", d.ID, err)
	}

	return nil
}

// Shares is every boundary a set in this domain shares with every other set
// in it. It returns a copy: a caller that appended to the package's own
// slice would rewrite what a repository domain means for the whole process.
func (d RepositoryDomain) Shares() []RepositoryBoundary {
	out := make([]RepositoryBoundary, len(RepositoryBoundaries))
	copy(out, RepositoryBoundaries)

	return out
}

// Admits reports whether this domain accepts one backup set's reference,
// i.e. whether that set's snapshots may be stored in this domain's
// repository.
//
// A reference naming another domain is refused here rather than downstream
// because this is the only place that holds both halves: the set's claim
// and the domain's declaration. A repository that accepted a set from
// another domain would be the boundary failure this whole file exists to
// prevent, and it would be invisible afterwards -- the snapshots would be
// there, deduplicated, under the wrong key.
func (d RepositoryDomain) Admits(ref RepositoryRef) error {
	if err := d.Validate(); err != nil {
		return err
	}

	if err := ref.Validate(); err != nil {
		return err
	}

	if ref.Domain != d.ID {
		return fmt.Errorf("%w: backup set %s names repository domain %q, not %q; %s",
			ErrForeignDomain, ref.Set, ref.Domain, d.ID, sharingCosts())
	}

	return nil
}

// MayShare reports whether two backup sets may occupy this domain's one
// repository.
//
// Three cases, and each one is a real configuration:
//
//   - the same set twice is not co-tenancy at all, and must not be reported
//     as a violation, or an isolated domain could not hold the single set
//     it exists for;
//   - two sets that both name this domain share it, which is what a shared
//     domain IS;
//   - two sets in a domain declared isolated are refused, because what the
//     operator declared was single tenancy and the second set is the edit
//     that would have dissolved it.
func (d RepositoryDomain) MayShare(a, b RepositoryRef) error {
	if err := d.Admits(a); err != nil {
		return err
	}

	if err := d.Admits(b); err != nil {
		return err
	}

	if a.Set == b.Set {
		return nil
	}

	if d.Isolation == RepositoryIsolated {
		return fmt.Errorf("%w: repository domain %q is declared %q and cannot hold both %s and %s; "+
			"declare it %q or point one of them at another domain (%s)",
			ErrIsolationViolated, d.ID, RepositoryIsolated, a.Set, b.Set, RepositoryShared, sharingCosts())
	}

	return nil
}

// RepositoryRef is one backup set's reference to the domain whose
// repository holds its snapshots.
//
// It carries the SET as well as the domain, which is what makes a reference
// checkable rather than decorative: co-tenancy is a statement about two
// sets, and a bare domain id could not tell an isolated domain holding one
// set apart from an isolated domain holding two.
type RepositoryRef struct {
	Domain RepositoryDomainID
	Set    BackupSetID
}

// IsZero reports whether this reference is unset, which is what a backup
// set running the artifact engine carries: that engine has no repository,
// so there is nothing for it to point at.
func (r RepositoryRef) IsZero() bool { return r.Domain.IsZero() && r.Set.IsZero() }

// Validate refuses the half-built references a validator could otherwise
// hand to Admits. Both halves matter: without the domain two references
// compare equal and look like co-tenants of one nameless repository, and
// without the set no refusal can name which set to fix.
func (r RepositoryRef) Validate() error {
	if r.Domain.IsZero() {
		return errors.New("repository reference names no domain")
	}

	if r.Set.IsZero() {
		return errors.New("repository reference names no backup set")
	}

	return nil
}

// String renders "domain:source/set", the form that appears in a refusal.
func (r RepositoryRef) String() string { return r.Domain.String() + ":" + r.Set.String() }

// sharingCosts is the sentence every sharing refusal ends with. It is one
// function because the list must not drift between the two refusals: an
// operator comparing them would reasonably conclude the two cases share
// different things.
func sharingCosts() string {
	var b strings.Builder

	b.WriteString("sharing one repository shares ")

	for i, boundary := range RepositoryBoundaries {
		switch {
		case i == len(RepositoryBoundaries)-1:
			b.WriteString(" and ")
		case i > 0:
			b.WriteString(", ")
		}

		b.WriteString(string(boundary))
	}

	return b.String()
}

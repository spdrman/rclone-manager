// Repository Domains, tested as a boundary rather than as a label.
//
// The claim this file holds is EPIC K's: two backup sets share a repository
// ONLY when the six things a repository makes common -- its encryption key,
// its credential, its corruption fate, its maintenance window, its
// deduplication span and its administrative access -- are all intentionally
// shared. That claim is worth nothing as prose, because the pressure runs
// the other way: co-tenancy is what maximises deduplication, so every
// operational instinct pushes towards one repository for everything and
// discovers the boundary afterwards, during an incident.
//
// So the tests below are about refusals: a set in another domain, and a
// second set in a domain declared to hold one.

package model

import (
	"errors"
	"strings"
	"testing"
)

// domains is the topology EPIC K says must be expressible, built once here
// because every test needs more than one domain to say anything: a single
// domain cannot demonstrate a boundary.
func domains(t *testing.T) map[string]RepositoryDomain {
	t.Helper()

	built := map[string]RepositoryDomain{}
	for _, d := range []struct {
		id        string
		isolation RepositoryIsolation
	}{
		{"production", RepositoryShared},
		{"security-sensitive", RepositoryIsolated},
		{"customer-a", RepositoryIsolated},
		{"customer-b", RepositoryIsolated},
		{"archive", RepositoryShared},
	} {
		id, err := NewRepositoryDomainID(d.id)
		if err != nil {
			t.Fatalf("NewRepositoryDomainID(%q): %v", d.id, err)
		}
		domain := RepositoryDomain{ID: id, Isolation: d.isolation, Description: d.id + " domain"}
		if err := domain.Validate(); err != nil {
			t.Fatalf("RepositoryDomain(%q).Validate(): %v", d.id, err)
		}
		built[d.id] = domain
	}

	return built
}

// ref is a backup set's reference to a domain, built through the model so
// the tests exercise the same construction a validator does.
func ref(t *testing.T, domain RepositoryDomain, source, set string) RepositoryRef {
	t.Helper()

	id, err := NewBackupSetID(source, set)
	if err != nil {
		t.Fatalf("NewBackupSetID(%q, %q): %v", source, set, err)
	}

	r := RepositoryRef{Domain: domain.ID, Set: id}
	if err := r.Validate(); err != nil {
		t.Fatalf("RepositoryRef.Validate(): %v", err)
	}

	return r
}

// TestRepositoryDomains_MultipleDomainsAreExpressible is the topology
// requirement stated as a test: production, security-sensitive, two
// customers and an archive, all distinct, all valid.
//
// It is not a tautology about map keys. The thing being checked is that
// nothing in this model funnels a deployment towards one domain -- no
// singleton, no "default" that others are variations of -- because a model
// that can only really express one domain is how customer-A's data ends up
// deduplicated against customer-B's.
func TestRepositoryDomains_MultipleDomainsAreExpressible(t *testing.T) {
	t.Parallel()

	built := domains(t)
	if len(built) != 5 {
		t.Fatalf("built %d domains, want the 5 EPIC K names", len(built))
	}

	seen := map[RepositoryDomainID]bool{}
	for name, d := range built {
		if seen[d.ID] {
			t.Errorf("domain %q shares an identity with another domain, which would merge two boundaries into one", name)
		}
		seen[d.ID] = true
	}

	// And the two isolations are both reachable: a deployment that wants
	// one repository for everything and one that wants a repository per
	// customer are both expressible, and neither is implied.
	if built["production"].Isolation != RepositoryShared || built["customer-a"].Isolation != RepositoryIsolated {
		t.Errorf("the two isolations did not survive construction: production=%q customer-a=%q",
			built["production"].Isolation, built["customer-a"].Isolation)
	}
}

// TestRepositoryDomain_AdmitsOnlyItsOwnMembers is the first half of the
// boundary: a reference that names another domain is refused, by the domain
// that was asked.
func TestRepositoryDomain_AdmitsOnlyItsOwnMembers(t *testing.T) {
	t.Parallel()

	built := domains(t)
	production, customerA := built["production"], built["customer-a"]

	own := ref(t, production, "production", "postgres-primary")
	if err := production.Admits(own); err != nil {
		t.Errorf("a domain refused a set that names it: %v", err)
	}

	foreign := ref(t, customerA, "customer-a", "uploads")
	err := production.Admits(foreign)
	if err == nil {
		t.Fatal("the production domain admitted a set that names customer-a; a repository that accepts a set from another domain IS the boundary failure this model exists to prevent")
	}
	if !errors.Is(err, ErrForeignDomain) {
		t.Errorf("Admits refused a foreign set with %v, which callers cannot distinguish from any other refusal", err)
	}
	for _, want := range []string{"production", "customer-a", "customer-a/uploads"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q, so it does not say which set and which domains are involved", err, want)
		}
	}
}

// TestRepositoryDomain_MayShare_OnlyInsideOneDomain is the sharing rule: two
// sets may occupy one repository when, and only when, they both name the
// same domain.
//
// The refusal has to name what would be shared, which is why the boundary
// list is part of the model rather than a comment. "customer-a may not
// share with customer-b" is an assertion; "sharing one repository shares
// the encryption key, the credential, the corruption fate, the maintenance
// window, the deduplication span and administrative access" is an argument,
// and it is the one that survives the next person who wants better
// deduplication ratios.
func TestRepositoryDomain_MayShare_OnlyInsideOneDomain(t *testing.T) {
	t.Parallel()

	built := domains(t)
	production := built["production"]

	pg := ref(t, production, "production", "postgres-primary")
	uploads := ref(t, production, "production", "uploads")

	if err := production.MayShare(pg, uploads); err != nil {
		t.Errorf("two sets in one shared domain were refused: %v; intentional co-tenancy is what a shared domain IS", err)
	}

	// A set compared with itself is not co-tenancy and must not be
	// reported as a violation: an isolated domain holding one set is the
	// whole point of an isolated domain.
	isolated := built["customer-a"]
	a := ref(t, isolated, "customer-a", "uploads")
	if err := isolated.MayShare(a, a); err != nil {
		t.Errorf("an isolated domain refused its own single set: %v", err)
	}

	// Cross-domain, asked of either domain, is refused and says what
	// would have been shared.
	b := ref(t, built["customer-b"], "customer-b", "uploads")
	err := isolated.MayShare(a, b)
	if err == nil {
		t.Fatal("a set in customer-a was allowed to share a repository with a set in customer-b")
	}
	if !errors.Is(err, ErrForeignDomain) {
		t.Errorf("cross-domain sharing was refused with %v rather than ErrForeignDomain", err)
	}
	for _, boundary := range RepositoryBoundaries() {
		if !strings.Contains(err.Error(), string(boundary)) {
			t.Errorf("the refusal %q does not name the %q boundary; a refusal that does not say what would be shared is an assertion an operator can only overrule",
				err, boundary)
		}
	}
}

// TestRepositoryDomain_IsolatedRefusesASecondSet is the case the shared
// arm cannot cover: both sets name the domain they are allowed to name, and
// the domain still refuses, because what it was declared to be is
// single-tenant.
//
// This is the customer-A/customer-B and the security-sensitive case. Both
// are configurations where an operator has decided that deduplication
// across sets is not worth a shared key, a shared credential and a shared
// corruption fate, and this refusal is what makes that decision hold
// against a later edit that points a second set at the same domain.
func TestRepositoryDomain_IsolatedRefusesASecondSet(t *testing.T) {
	t.Parallel()

	built := domains(t)
	sensitive := built["security-sensitive"]

	keys := ref(t, sensitive, "production", "hsm-exports")
	audit := ref(t, sensitive, "production", "audit-log")

	err := sensitive.MayShare(keys, audit)
	if err == nil {
		t.Fatal("a domain declared isolated accepted two different backup sets; an isolated domain that silently becomes shared is a security boundary that only existed in the config file")
	}
	if !errors.Is(err, ErrIsolationViolated) {
		t.Errorf("the refusal was %v, which callers cannot tell apart from a cross-domain refusal; the operator response differs (point one set elsewhere vs. declare the domain shared)", err)
	}
	for _, want := range []string{"security-sensitive", "production/hsm-exports", "production/audit-log", string(RepositoryShared)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q, so it names neither the sets involved nor the way out", err, want)
		}
	}

	// The control: the same two sets in a shared domain are fine, so the
	// refusal above is about the declared isolation and not about the
	// sets.
	shared := built["archive"]
	if err := shared.MayShare(
		RepositoryRef{Domain: shared.ID, Set: keys.Set},
		RepositoryRef{Domain: shared.ID, Set: audit.Set},
	); err != nil {
		t.Errorf("the same two sets were refused in a shared domain: %v", err)
	}
}

// TestRepositoryBoundaries_AreSharedAllOrNothing pins the model's one
// simplification, and it is a deliberate one: there is no way to share a
// repository's deduplication span without sharing its encryption key,
// because they are the same repository. A partial-sharing field would be a
// promise the storage layer cannot keep.
func TestRepositoryBoundaries_AreSharedAllOrNothing(t *testing.T) {
	t.Parallel()

	if len(RepositoryBoundaries()) != 6 {
		t.Fatalf("RepositoryBoundaries() has %d entries (%v); EPIC K names six, and dropping one would understate what co-tenancy costs",
			len(RepositoryBoundaries()), RepositoryBoundaries())
	}

	built := domains(t)
	for _, d := range built {
		shares := d.Shares()
		if len(shares) != len(RepositoryBoundaries()) {
			t.Fatalf("domain %q shares %d of %d boundaries; a repository cannot share some of them and not others",
				d.ID, len(shares), len(RepositoryBoundaries()))
		}
	}

	// Shares must not hand out the package's own slice, or one caller
	// appending to its result rewrites the boundary list for everybody.
	shares := built["production"].Shares()
	shares[0] = "tampered"
	if RepositoryBoundaries()[0] == "tampered" {
		t.Error("Shares returned the package-level slice; a caller can now rewrite what a repository domain means")
	}
}

// TestNewRepositoryDomainID_RefusesWhatCannotBeANameSafely applies the same
// rules BackupSetID's halves obey, for the same reasons, plus the two a
// domain id needs on its own account: it is destined to name a directory
// and a credential, so "." and ".." are refused rather than cleaned.
func TestNewRepositoryDomainID_RefusesWhatCannotBeANameSafely(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{"production", "customer-a", "archive_2026", "security.sensitive"} {
		if _, err := NewRepositoryDomainID(ok); err != nil {
			t.Errorf("NewRepositoryDomainID(%q): %v", ok, err)
		}
	}

	for _, bad := range []string{
		"",             // an unnamed boundary is not a boundary
		".",            // both of these would name a directory that is not the domain's
		"..",           //
		"a/b",          // the separator BackupSetID reserves
		`a\b`,          // and the one a Windows-ish path would use
		" production",  // refused rather than trimmed: two ids an operator typed differently must not become one
		"production ",  //
		"prod\nuction", // a name that turns one audit line into two
	} {
		if got, err := NewRepositoryDomainID(bad); err == nil {
			t.Errorf("NewRepositoryDomainID(%q) = %q with no error", bad, got)
		}
	}
}

// TestRepositoryDomain_Validate_RefusesAnUnstatedIsolation is the config
// seam's reason for existing: a domain that does not say whether it is
// shared is not an explicit security boundary, and the zero value must not
// silently become the permissive one.
func TestRepositoryDomain_Validate_RefusesAnUnstatedIsolation(t *testing.T) {
	t.Parallel()

	id, err := NewRepositoryDomainID("production")
	if err != nil {
		t.Fatalf("NewRepositoryDomainID: %v", err)
	}

	err = RepositoryDomain{ID: id}.Validate()
	if err == nil {
		t.Fatal("a repository domain with no isolation stated validated; an unstated boundary that defaults to shared is how a security boundary becomes a comment")
	}
	for _, want := range []string{string(RepositoryShared), string(RepositoryIsolated)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q, so it does not say what to write", err, want)
		}
	}

	if _, err := ParseRepositoryIsolation(""); err == nil {
		t.Error("ParseRepositoryIsolation(\"\") succeeded; silence is not one of the two answers")
	}
	if _, err := ParseRepositoryIsolation("private"); err == nil {
		t.Error("ParseRepositoryIsolation accepted a value that is neither shared nor isolated")
	}
	for _, isolation := range RepositoryIsolations() {
		got, err := ParseRepositoryIsolation(string(isolation))
		if err != nil || got != isolation {
			t.Errorf("ParseRepositoryIsolation(%q) = %q, %v", isolation, got, err)
		}
	}
}

// TestRepositoryRef_Validate refuses the half-built references a validator
// could otherwise hand to Admits, where a zero domain id would compare
// equal to another zero one and two unrelated sets would look like
// co-tenants of the same nameless repository.
func TestRepositoryRef_Validate(t *testing.T) {
	t.Parallel()

	built := domains(t)
	good := ref(t, built["production"], "production", "postgres-primary")

	if good.IsZero() {
		t.Error("a fully-built reference reports itself as the unset value")
	}
	if !(RepositoryRef{}).IsZero() {
		t.Error("the zero reference does not report itself as unset, which is what an artifact set carries")
	}

	for name, bad := range map[string]RepositoryRef{
		"no domain": {Set: good.Set},
		"no set":    {Domain: good.Domain},
		"neither":   {},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("a reference with %s validated; two such references would compare equal and look like co-tenants of one repository", name)
		}
	}

	if !strings.Contains(good.String(), "production") {
		t.Errorf("RepositoryRef.String() = %q, which does not name the domain", good.String())
	}
}

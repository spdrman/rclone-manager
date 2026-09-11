package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/spdrman/backupd/core/internal/transport"
)

// The listing's filters, and the refusal that keeps an empty answer meaning
// one thing.
//
// The filter cases are ordinary. The refusal cases are issue #187 and they
// assert the error's Kind and Name rather than that some error came back,
// because a filter that refused for an unrelated reason (an unreadable
// journal, say) satisfies "err != nil" while telling the operator nothing
// about the name they typed.
//
// The rows with no expected Kind are positive controls and they are not
// decoration. One of them, a Set with no Source, is the case that breaks if
// resolution is ever rewritten as a count of how many sets matched: a source
// configured with no backup sets matches nothing either, and calling that "no
// such source" names the wrong thing. The pair of "uploads exists, staging
// exists, staging/uploads does not" is FR-7's source-plus-set identity stated
// as a test.
//
// Two backup sets in one fixture need two fake transports, because
// fakeTransport is keyed on remote path alone and ignores the Source
// entirely, so a shared one lets each set discover the other's objects.

func TestListArtifacts_FiltersBySourceAndSet(t *testing.T) {
	prodBS := testBackupSet(t, t.TempDir())
	prodBS.Name = "postgres-primary"
	prodBS.ID = mustSetID(t, "production", "postgres-primary")

	stagingBS := testBackupSet(t, t.TempDir())
	stagingBS.Name = "postgres-primary"
	stagingBS.ID = mustSetID(t, "staging", "postgres-primary")

	// Two separate fake transports, one per backup set: fakeTransport
	// ignores transport.Source entirely (it is keyed purely by remote
	// path), so discovering two backup sets against one shared instance
	// would let each one see the other's objects too.
	prodTr := newFakeTransport()
	prodTr.put("prod.dump", "prod payload", epoch.Unix())

	stagingTr := newFakeTransport()
	stagingTr.put("staging.dump", "staging payload", epoch.Unix())

	journal := openJournal(t)
	ctx := context.Background()
	discoverOneRecord(t, ctx, journal, prodTr, transport.Source{ID: "prod"}, prodBS)
	discoverOneRecord(t, ctx, journal, stagingTr, transport.Source{ID: "staging"}, stagingBS)

	svc := New(testConfig(t, testSource("production", prodBS), testSource("staging", stagingBS)), journal, prodTr, nil)

	all, err := svc.ListArtifacts(ctx, ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len(all) = %d, want 2", len(all))
	}

	prodOnly, err := svc.ListArtifacts(ctx, ArtifactFilter{Source: "production"})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(prodOnly) != 1 || prodOnly[0].RemotePath != "prod.dump" {
		t.Errorf("prodOnly = %+v, want exactly the production artifact", prodOnly)
	}

	stagingOnly, err := svc.ListArtifacts(ctx, ArtifactFilter{Source: "staging", Set: "postgres-primary"})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(stagingOnly) != 1 || stagingOnly[0].RemotePath != "staging.dump" {
		t.Errorf("stagingOnly = %+v, want exactly the staging artifact", stagingOnly)
	}

	// Issue #569: the same narrowing, spelled the way every surface that
	// PRINTS a backup set spells it. This fixture is the shape that makes
	// the assertion worth making, since both sources configure a set
	// called postgres-primary and only the source half of the id tells
	// them apart.
	byID, err := svc.ListArtifacts(ctx, ArtifactFilter{Set: "staging/postgres-primary"})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(byID) != 1 || byID[0].RemotePath != "staging.dump" {
		t.Errorf("byID = %+v, want exactly the staging artifact", byID)
	}
}

// TestListArtifacts_RefusesABareSetNameTwoSourcesShare is the other half
// of issue #569.
//
// FR-7 makes a backup set's identity source-plus-set, so a name two
// sources both configure identifies neither of them. Answering it with
// one set's rows, or with both merged, hands an operator who filtered for
// one host another host's backups with nothing in the output to doubt, and
// the same convention applied across a fleet produces that collision on
// every deployment that has one.
//
// The candidates are asserted individually rather than as a rendered
// sentence: what the refusal owes the operator is the two ids they can
// retype, and how they are punctuated is this package's business.
func TestListArtifacts_RefusesABareSetNameTwoSourcesShare(t *testing.T) {
	prodPostgres := testBackupSet(t, t.TempDir())
	prodPostgres.Name = "postgres-primary"
	prodPostgres.ID = mustSetID(t, "production", "postgres-primary")

	stagingPostgres := testBackupSet(t, t.TempDir())
	stagingPostgres.Name = "postgres-primary"
	stagingPostgres.ID = mustSetID(t, "staging", "postgres-primary")

	svc := New(
		testConfig(t,
			testSource("production", prodPostgres),
			testSource("staging", stagingPostgres),
		),
		openJournal(t), newFakeTransport(), nil)

	records, err := svc.ListArtifacts(context.Background(), ArtifactFilter{Set: "postgres-primary"})

	var ambiguous *AmbiguousSetError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("ListArtifacts(Set: postgres-primary) error = %v (%T), want an *AmbiguousSetError", err, err)
	}
	if ambiguous.Name != "postgres-primary" {
		t.Errorf("the refusal is about %q, want the name that was typed", ambiguous.Name)
	}
	want := []string{"production/postgres-primary", "staging/postgres-primary"}
	if len(ambiguous.Candidates) != len(want) {
		t.Fatalf("candidates = %v, want both configured ids %v", ambiguous.Candidates, want)
	}
	if !slices.Equal(ambiguous.Candidates, want) {
		t.Errorf("candidates = %v, want %v in configuration order", ambiguous.Candidates, want)
	}
	if !strings.Contains(ambiguous.Error(), want[0]) || !strings.Contains(ambiguous.Error(), want[1]) {
		t.Errorf("the refusal reads %q and does not name both candidates; an operator cannot retype what they are not shown", ambiguous.Error())
	}
	if records != nil {
		t.Errorf("ListArtifacts returned %d record(s) alongside its refusal, want none", len(records))
	}
}

// TestListArtifacts_RefusesAFilterThatNamesTwoSources covers the one
// mistake the composite id makes possible, and it lives here rather than
// beside the command because the command refuses that pair on the
// command line before it ever opens anything: this is the answer for a
// caller that builds the filter by hand, which is every caller this
// presentation-agnostic package is meant to have.
//
// What it asserts about the refusal is that it is NOT the not-found one.
// Both halves of this pair name real, configured things, and reporting it
// as a missing backup set would print "no configured backup set named
// staging/postgres-primary" about a set that is configured, which is the
// untruth #569 was reported for in the first place.
func TestListArtifacts_RefusesAFilterThatNamesTwoSources(t *testing.T) {
	prodPostgres := testBackupSet(t, t.TempDir())
	prodPostgres.Name = "postgres-primary"
	prodPostgres.ID = mustSetID(t, "production", "postgres-primary")

	stagingPostgres := testBackupSet(t, t.TempDir())
	stagingPostgres.Name = "postgres-primary"
	stagingPostgres.ID = mustSetID(t, "staging", "postgres-primary")

	svc := New(
		testConfig(t,
			testSource("production", prodPostgres),
			testSource("staging", stagingPostgres),
		),
		openJournal(t), newFakeTransport(), nil)

	filter := ArtifactFilter{Source: "staging", Set: "production/postgres-primary"}
	records, err := svc.ListArtifacts(context.Background(), filter)

	if err == nil {
		t.Fatalf("ListArtifacts(%+v) returned %d record(s) and no error; a filter naming two different sources names no backup set", filter, len(records))
	}
	var notFound *NotFoundError
	if errors.As(err, &notFound) {
		t.Errorf("ListArtifacts(%+v) refused with %v, which says something configured is not configured", filter, err)
	}
	for _, want := range []string{"production", "staging"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ListArtifacts(%+v) refused with %q, which does not name %q; the whole complaint is that two sources were named", filter, err, want)
		}
	}
	if records != nil {
		t.Errorf("ListArtifacts returned %d record(s) alongside its refusal, want none", len(records))
	}
}

// TestListArtifacts_RefusesAnUnconfiguredFilter is issue #187's proof.
//
// FR-7 makes a backup set's identity source-plus-set, so a --source or a
// --backup-set naming something the config does not have names no
// identity at all. An empty listing is the answer to a question about a
// backup set that exists and has not run yet, and it cannot also be the
// answer to a question about one that does not exist: those two states
// call for opposite responses from an operator, and "0 artifact(s)"
// cannot tell them apart. Fetch already refuses the identical name
// through the same *NotFoundError, so this asserts the same refusal from
// the listing side.
//
// Every refusal case here asserts the error's Kind and Name, not merely
// that some error came back: a filter that refused for a different reason
// (an unreachable journal, say) would satisfy "err != nil" while telling
// the operator nothing about the name they typed. The rows with an empty
// wantKind are the positive controls, and one of them (Set with no
// Source) is the case that would break if the resolution were written as
// a naive "no backup set matched" count.
func TestListArtifacts_RefusesAnUnconfiguredFilter(t *testing.T) {
	prodPostgres := testBackupSet(t, t.TempDir())
	prodPostgres.Name = "postgres-primary"
	prodPostgres.ID = mustSetID(t, "production", "postgres-primary")

	prodUploads := testBackupSet(t, t.TempDir())
	prodUploads.Name = "uploads"
	prodUploads.ID = mustSetID(t, "production", "uploads")

	stagingPostgres := testBackupSet(t, t.TempDir())
	stagingPostgres.Name = "postgres-primary"
	stagingPostgres.ID = mustSetID(t, "staging", "postgres-primary")

	svc := New(
		testConfig(t,
			testSource("production", prodPostgres, prodUploads),
			testSource("staging", stagingPostgres),
		),
		openJournal(t), newFakeTransport(), nil)

	tests := []struct {
		name     string
		filter   ArtifactFilter
		wantKind string // empty means the filter names something real
		wantName string
	}{
		{name: "no filter at all", filter: ArtifactFilter{}},
		{name: "a configured source", filter: ArtifactFilter{Source: "production"}},
		{name: "a configured source and set", filter: ArtifactFilter{Source: "production", Set: "uploads"}},
		{name: "a set name only one source configures", filter: ArtifactFilter{Set: "uploads"}},
		{name: "the composite id every surface that prints a set prints", filter: ArtifactFilter{Set: "staging/postgres-primary"}},
		{
			name:     "an unconfigured source",
			filter:   ArtifactFilter{Source: "no-such-source"},
			wantKind: "source",
			wantName: "no-such-source",
		},
		{
			name:     "an unconfigured set inside a configured source",
			filter:   ArtifactFilter{Source: "production", Set: "no-such-set"},
			wantKind: "backup set",
			wantName: "production/no-such-set",
		},
		{
			name:     "an unconfigured set with no source to narrow it",
			filter:   ArtifactFilter{Set: "no-such-set"},
			wantKind: "backup set",
			wantName: "no-such-set",
		},
		{
			// FR-7 again: "uploads" is real, "staging" is real, and
			// staging/uploads is not.
			name:     "a real set name under the wrong source",
			filter:   ArtifactFilter{Source: "staging", Set: "uploads"},
			wantKind: "backup set",
			wantName: "staging/uploads",
		},
		{
			// The same three refusals again, spelled as the composite id
			// #569 taught this filter to take. The name each one reports
			// is the half that is actually missing, so an operator who
			// mistyped the source is not sent looking for the set.
			name:     "a composite id under a source nobody configured",
			filter:   ArtifactFilter{Set: "no-such-source/postgres-primary"},
			wantKind: "source",
			wantName: "no-such-source",
		},
		{
			name:     "a composite id naming no set under a real source",
			filter:   ArtifactFilter{Set: "production/no-such-set"},
			wantKind: "backup set",
			wantName: "production/no-such-set",
		},
		{
			name:     "a composite id pairing a real source with the wrong set",
			filter:   ArtifactFilter{Set: "staging/uploads"},
			wantKind: "backup set",
			wantName: "staging/uploads",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			records, err := svc.ListArtifacts(context.Background(), tc.filter)

			if tc.wantKind == "" {
				if err != nil {
					t.Fatalf("ListArtifacts(%+v) = %v, want no error", tc.filter, err)
				}
				return
			}

			var notFound *NotFoundError
			if !errors.As(err, &notFound) {
				t.Fatalf("ListArtifacts(%+v) error = %v (%T), want a *NotFoundError", tc.filter, err, err)
			}
			if notFound.Kind != tc.wantKind || notFound.Name != tc.wantName {
				t.Errorf("ListArtifacts(%+v) refused %s %q, want %s %q",
					tc.filter, notFound.Kind, notFound.Name, tc.wantKind, tc.wantName)
			}
			if records != nil {
				t.Errorf("ListArtifacts(%+v) returned %d record(s) alongside its refusal, want none",
					tc.filter, len(records))
			}
		})
	}
}

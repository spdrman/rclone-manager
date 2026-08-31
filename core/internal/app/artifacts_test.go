package app

import (
	"context"
	"errors"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

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
		{name: "a set name configured under some source", filter: ArtifactFilter{Set: "postgres-primary"}},
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

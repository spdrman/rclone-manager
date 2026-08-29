package app

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/internal/transport"
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

package backupengine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/model"
)

func domainID(t *testing.T, id string) model.RepositoryDomainID {
	t.Helper()

	domain, err := model.NewRepositoryDomainID(id)
	if err != nil {
		t.Fatalf("NewRepositoryDomainID(%q): %v", id, err)
	}

	return domain
}

// ownershipStore is a store over a fresh directory, returned with that
// directory so a test can build a second store over the same one.
func ownershipStore(t *testing.T) (backupengine.FileMaintenanceOwnershipStore, string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "state")

	store, err := backupengine.NewFileMaintenanceOwnershipStore(dir)
	if err != nil {
		t.Fatalf("NewFileMaintenanceOwnershipStore: %v", err)
	}

	return store, dir
}

// TestMaintenanceOwnershipSurvivesARestart is the whole reason this record
// is durable rather than in memory: the question "who owns maintenance for
// this repository" has to have the same answer after a crash as before it,
// or two instances both answer "me".
func TestMaintenanceOwnershipSurvivesARestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, dir := ownershipStore(t)
	domain := domainID(t, "production")

	quick := time.Now().UTC().Truncate(time.Second)
	full := quick.Add(-48 * time.Hour)

	want := backupengine.MaintenanceOwnership{
		Domain:       domain,
		Owner:        "nas-01",
		LastQuick:    quick,
		LastFull:     full,
		NextEligible: quick.Add(time.Hour),
		LastResult: backupengine.MaintenanceOutcome{
			At:   quick,
			Mode: backupengine.MaintenanceQuick,
			Ran:  true,
		},
	}

	if err := store.Save(ctx, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A second store over the same directory is the restart: nothing is
	// carried over in memory.
	reopened, err := backupengine.NewFileMaintenanceOwnershipStore(dir)
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}

	got, err := reopened.Load(ctx, domain)
	if err != nil {
		t.Fatalf("Load after a restart: %v", err)
	}

	if got.Owner != want.Owner || !got.LastQuick.Equal(want.LastQuick) || !got.LastFull.Equal(want.LastFull) {
		t.Errorf("Load returned %+v, want %+v", got, want)
	}

	if !got.NextEligible.Equal(want.NextEligible) {
		t.Errorf("NextEligible came back as %s, want %s", got.NextEligible, want.NextEligible)
	}

	if got.LastResult.Mode != want.LastResult.Mode || got.LastResult.Ran != want.LastResult.Ran {
		t.Errorf("LastResult came back as %+v, want %+v", got.LastResult, want.LastResult)
	}
}

// TestLoadDistinguishesNeverOwnedFromUnreadable is the distinction that
// keeps this record safe to act on.
//
// "No record" means nobody has owned this repository yet, and the response
// to it is to claim it. "Unreadable record" must never produce that
// answer, because the repository it describes may well be being maintained
// by the instance whose record was truncated by a crash.
func TestLoadDistinguishesNeverOwnedFromUnreadable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, dir := ownershipStore(t)
	domain := domainID(t, "production")

	if _, err := store.Load(ctx, domain); !errors.Is(err, backupengine.ErrNoMaintenanceOwnership) {
		t.Errorf("Load with no record = %v, want ErrNoMaintenanceOwnership", err)
	}

	if err := store.Save(ctx, backupengine.MaintenanceOwnership{Domain: domain, Owner: "nas-01"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Truncate the record the way a crash mid-write would, if the write
	// were not atomic.
	record := filepath.Join(dir, domain.String()+".maintenance.json")
	if err := os.WriteFile(record, []byte(`{"domain":"produ`), 0o600); err != nil {
		t.Fatalf("truncating the record: %v", err)
	}

	_, err := store.Load(ctx, domain)
	if err == nil {
		t.Fatalf("Load accepted a truncated record")
	}

	if errors.Is(err, backupengine.ErrNoMaintenanceOwnership) {
		t.Errorf("Load read a truncated record as an unowned repository: %v", err)
	}
}

// TestLoadRefusesARecordForAnotherRepository covers the one way a
// correctly-formed record can still be the wrong answer: a state directory
// copied between deployments, or a file renamed by hand.
func TestLoadRefusesARecordForAnotherRepository(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, dir := ownershipStore(t)
	wanted := domainID(t, "production")
	other := domainID(t, "customer-a")

	if err := store.Save(ctx, backupengine.MaintenanceOwnership{Domain: other, Owner: "nas-01"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := os.Rename(
		filepath.Join(dir, other.String()+".maintenance.json"),
		filepath.Join(dir, wanted.String()+".maintenance.json"),
	); err != nil {
		t.Fatalf("renaming the record: %v", err)
	}

	if _, err := store.Load(ctx, wanted); err == nil {
		t.Errorf("Load accepted a record naming another repository")
	}
}

// TestSaveRefusesAnIncompleteClaim covers both halves of what makes the
// record a claim: without a domain it cannot be matched to a repository,
// and without an owner it claims nothing.
func TestSaveRefusesAnIncompleteClaim(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _ := ownershipStore(t)

	for _, tc := range []struct {
		name   string
		record backupengine.MaintenanceOwnership
	}{
		{"no domain", backupengine.MaintenanceOwnership{Owner: "nas-01"}},
		{"no owner", backupengine.MaintenanceOwnership{Domain: domainID(t, "production")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := store.Save(ctx, tc.record); err == nil {
				t.Errorf("Save accepted a record with %s", tc.name)
			}
		})
	}
}

// TestStoreRefusesARelativeDirectory is about a failure with no symptom:
// a relative state directory resolves against whatever the working
// directory happens to be, so a daemon that changed directory would stop
// finding the records it wrote and quietly decide every repository was
// unowned.
func TestStoreRefusesARelativeDirectory(t *testing.T) {
	t.Parallel()

	if _, err := backupengine.NewFileMaintenanceOwnershipStore("var/lib/backupd"); err == nil {
		t.Errorf("NewFileMaintenanceOwnershipStore accepted a relative directory")
	}
}

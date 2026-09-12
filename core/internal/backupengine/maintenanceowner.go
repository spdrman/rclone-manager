package backupengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
)

// This file is the record maintenance scheduling will be decided from
// (#786), and nothing that decides anything.
//
// # Why it lands now, empty of policy
//
// A repository has exactly one maintenance owner. That is not a design
// choice this project gets to make: quick and full maintenance rewrite
// index blobs and reclaim content, two processes doing it concurrently
// against one repository is how a repository loses content it still
// references, and the embedded engine's own answer -- an owner string in
// the repository, checked by default -- is one the adapter deliberately
// overrides (see the adapter's Maintain), because deferring to whichever
// machine happened to create the repository means a maintenance window
// that silently never runs.
//
// Having overridden it, this product owes the same guarantee from its own
// side, and that guarantee needs a durable record: who owns maintenance
// for this repository, when quick and full last ran, when the next one is
// allowed, and how the last one ended. Writing that record down is
// separable from deciding anything with it, and separating them is what
// lets the repository adapter land complete: #786 adds the scheduler, the
// capacity checks and the ownership handover, and it adds them against a
// record that already exists, is already persisted beside the repository's
// other local state, and is already proven to survive a restart.
//
// What is deliberately absent: any method that decides whether
// maintenance may run now. NextEligible is a recorded fact, not an
// enforced one, and nothing here reads the clock to compare against it. A
// half-made scheduling decision in this file would be a second scheduler
// for #786 to contend with rather than a foundation to build on.

// ErrNoMaintenanceOwnership is returned by a store that holds no record
// for a repository, which is the ordinary state of one that has never been
// maintained.
//
// It is distinct from a read failure because the two lead somewhere
// different: no record means "nobody has owned this yet, claim it", and an
// unreadable record means "something is wrong with this manager's state
// directory", which must never be silently treated as the first.
var ErrNoMaintenanceOwnership = errors.New("backupengine: no maintenance ownership record for this repository")

// MaintenanceOwner identifies the backupd instance that owns maintenance
// for one repository.
//
// It is a free-form string rather than a validated type because what
// usefully identifies an instance differs per deployment -- a hostname, a
// container id, an operator-assigned name -- and this record's job is to
// let two instances notice they disagree, which any stable distinct string
// achieves. The one rule is that it must be stable across restarts of the
// same instance, or every restart looks like a new owner.
type MaintenanceOwner string

func (o MaintenanceOwner) String() string { return string(o) }

// IsZero reports whether no owner is recorded.
func (o MaintenanceOwner) IsZero() bool { return o == "" }

// MaintenanceOutcome is how one maintenance run ended.
//
// Ran and Err are separate because "declined to do anything" is a normal,
// successful outcome (nothing was due, or another process held the lock)
// and recording it as a failure would have an operator investigating a
// healthy repository. Err is a string rather than an error because this
// record is persisted: an error's identity does not survive being written
// to a file, and pretending otherwise invites a reader to route on
// something that is only ever text by the time they see it.
type MaintenanceOutcome struct {
	// At is when the run finished.
	At time.Time `json:"at"`

	// Mode is which maintenance was attempted.
	Mode MaintenanceMode `json:"mode"`

	// Ran is whether it actually did work.
	Ran bool `json:"ran"`

	// Err is the failure, rendered, or empty when it succeeded.
	Err string `json:"error,omitempty"`
}

// MaintenanceOwnership is the durable record of one repository's
// maintenance state.
type MaintenanceOwnership struct {
	// Domain is which repository this is about. It is the repository's
	// stable id rather than a storage path for RepositoryLocation's
	// reason: a record keyed by a path stops matching the first time a
	// mount point moves.
	Domain model.RepositoryDomainID `json:"domain"`

	// Owner is the instance that owns maintenance for it.
	Owner MaintenanceOwner `json:"owner"`

	// LastQuick and LastFull are when each mode last ran. Zero means
	// never, which is a fact and not a missing value: a repository that
	// has never had a full maintenance is exactly what an operator needs
	// to be told about.
	LastQuick time.Time `json:"last_quick,omitempty"`
	LastFull  time.Time `json:"last_full,omitempty"`

	// NextEligible is the earliest time the owner intends to run
	// maintenance again. It is recorded, never enforced here; see this
	// file's header.
	NextEligible time.Time `json:"next_eligible,omitempty"`

	// LastResult is how the most recent attempt ended, whichever mode it
	// was.
	LastResult MaintenanceOutcome `json:"last_result,omitzero"`
}

// Validate refuses a record that could not be matched back to a
// repository or an owner, which are the two halves that make it a claim
// rather than a note.
func (m MaintenanceOwnership) Validate() error {
	if m.Domain.IsZero() {
		return errors.New("backupengine: a maintenance ownership record must name its repository domain")
	}

	if m.Owner.IsZero() {
		return errors.New("backupengine: a maintenance ownership record must name its owner; an unowned claim is not a claim")
	}

	return nil
}

// MaintenanceOwnershipStore persists maintenance ownership records.
//
// It is an interface with one implementation because #786 will need a
// second one in its tests, and because the storage choice here (a file
// beside the repository's other local state) is the kind of decision a
// later requirement -- ownership visible to another instance, which a
// local file cannot provide -- may have to change without every caller
// noticing.
type MaintenanceOwnershipStore interface {
	// Load returns the record for one repository, or
	// ErrNoMaintenanceOwnership.
	Load(ctx context.Context, domain model.RepositoryDomainID) (MaintenanceOwnership, error)

	// Save writes one record, replacing any previous one for the same
	// repository.
	Save(ctx context.Context, record MaintenanceOwnership) error
}

// FileMaintenanceOwnershipStore keeps one JSON file per repository under a
// directory this manager owns.
//
// A file rather than the state journal, and that is a decision worth
// stating: the journal is the artifact catalog, its schema is migrated,
// and a repository's maintenance state is not artifact state. A file under
// the same state directory that already holds the repository's connection
// config and index cache keeps everything about one repository's local
// state in one place, and makes "delete this repository's local state"
// something an operator can do with a directory.
type FileMaintenanceOwnershipStore struct {
	// dir holds one file per repository.
	dir string
}

var _ MaintenanceOwnershipStore = FileMaintenanceOwnershipStore{}

// NewFileMaintenanceOwnershipStore returns a store writing under dir,
// which must be absolute: a relative state directory is a different
// directory per working directory, and the failure shows up as a
// repository that has forgotten it was ever maintained.
func NewFileMaintenanceOwnershipStore(dir string) (FileMaintenanceOwnershipStore, error) {
	if !filepath.IsAbs(dir) {
		return FileMaintenanceOwnershipStore{}, fmt.Errorf("backupengine: maintenance ownership directory %q is relative", dir)
	}

	return FileMaintenanceOwnershipStore{dir: filepath.Clean(dir)}, nil
}

// path is where one repository's record lives. The domain id is safe as a
// filename for ReservedLocalDir's reason: model.NewRepositoryDomainID
// already refuses everything that would make it unsafe.
func (s FileMaintenanceOwnershipStore) path(domain model.RepositoryDomainID) string {
	return filepath.Join(s.dir, domain.String()+".maintenance.json")
}

// Load implements MaintenanceOwnershipStore.
func (s FileMaintenanceOwnershipStore) Load(_ context.Context, domain model.RepositoryDomainID) (MaintenanceOwnership, error) {
	if domain.IsZero() {
		return MaintenanceOwnership{}, errors.New("backupengine: cannot load a maintenance ownership record without a repository domain")
	}

	raw, err := os.ReadFile(s.path(domain))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return MaintenanceOwnership{}, ErrNoMaintenanceOwnership
		}

		return MaintenanceOwnership{}, fmt.Errorf("backupengine: reading maintenance ownership for %s: %w", domain, err)
	}

	var record MaintenanceOwnership
	if err := json.Unmarshal(raw, &record); err != nil {
		// Not ErrNoMaintenanceOwnership: a corrupt record must never read
		// as an unowned repository, because the answer to unowned is
		// "claim it" and claiming a repository somebody else is
		// maintaining is the thing this record exists to prevent.
		return MaintenanceOwnership{}, fmt.Errorf("backupengine: maintenance ownership record for %s is unreadable: %w", domain, err)
	}

	if record.Domain != domain {
		return MaintenanceOwnership{}, fmt.Errorf(
			"backupengine: maintenance ownership record at %s is for repository %q, not %q",
			s.path(domain), record.Domain, domain)
	}

	return record, nil
}

// Save implements MaintenanceOwnershipStore.
//
// The write is atomic -- a temporary file in the same directory, then a
// rename -- because the alternative is a truncated record after a crash,
// which Load correctly refuses to read and which therefore locks the
// repository out of maintenance until somebody deletes a file by hand.
func (s FileMaintenanceOwnershipStore) Save(_ context.Context, record MaintenanceOwnership) error {
	if err := record.Validate(); err != nil {
		return err
	}

	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("backupengine: encoding maintenance ownership for %s: %w", record.Domain, err)
	}

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("backupengine: creating maintenance ownership directory: %w", err)
	}

	final := s.path(record.Domain)

	tmp, err := os.CreateTemp(s.dir, "."+record.Domain.String()+".maintenance-*")
	if err != nil {
		return fmt.Errorf("backupengine: creating a temporary maintenance ownership file: %w", err)
	}

	name := tmp.Name()

	defer os.Remove(name) //nolint:errcheck // best effort; the rename below is what matters

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close() //nolint:errcheck // already failing

		return fmt.Errorf("backupengine: writing maintenance ownership for %s: %w", record.Domain, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("backupengine: closing maintenance ownership for %s: %w", record.Domain, err)
	}

	if err := os.Rename(name, final); err != nil {
		return fmt.Errorf("backupengine: installing maintenance ownership for %s: %w", record.Domain, err)
	}

	return nil
}

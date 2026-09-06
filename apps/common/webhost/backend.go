package webhost

import (
	"context"

	"github.com/spdrman/rclone-manager/core/service"
)

// The seam between this package and core/.
//
// It is an interface rather than a direct dependency on
// core/service.BackupService for one reason that shows up in every test in
// this package: standing up a real BackupService means a SQLite journal, a
// state directory, migrations and an rclone transport, and a handler test
// that has to build all that in order to check a JSON field is a handler
// test nobody writes. The doubles in fixtures_test.go implement this
// instead.
//
// What is worth reading rather than skimming is which methods are
// read-only. That property is not visible from a signature, and it is what
// decides whether the route in front of a method carries requireCSRF, the
// destructive gate, both or neither. So each method's own doc says which
// bucket of docs/EPIC-B-multi-nas.md §50 it falls into, and router.go's
// route table is where that answer is spent.
//
// The interface is deliberately not the whole of BackupService. Only what
// the handlers here call is listed, so an addition to core's public
// surface does not silently become part of the HTTP API's reach.

// BackupServiceClient is the seam this package talks to core/ through:
// exactly the subset of core/service.BackupService's method set the HTTP
// handlers below need, expressed as an interface so a test can substitute
// a double instead of standing up a real SQLite journal and rclone
// transport for every handler test (see fixtures_test.go's
// syncFakeBackend and asyncFakeBackend).
//
// *service.BackupService satisfies this directly (see the compile-time
// assertion below); production wiring passes one built with
// service.Open.
type BackupServiceClient interface {
	// ConfigRevision reports the configuration revision the backend is
	// currently running, so a client's stale picture of it can be
	// detected (docs/EPIC-B-multi-nas.md §14).
	ConfigRevision() string

	// Ready reports whether this backend completed
	// docs/EPIC-B-multi-nas.md §46.1's startup sequence. It is the fact
	// behind /health/ready and GET /system/version's "ready" field, and
	// §36 makes it the precondition a client checks before a destructive
	// operation — so it is asked of the backend, which knows, rather than
	// inferred in this package from some other value that happens to be
	// non-empty. See core/service.BackupService.Ready.
	Ready() bool

	// SubmitRunCycle persists and starts (or, replaying an idempotency
	// key, returns the already-submitted) run_cycle operation. See
	// core/service.BackupService.SubmitRunCycle's doc for the durability
	// and decoupling contract this package relies on without
	// re-implementing.
	SubmitRunCycle(ctx context.Context, req service.RunCycleRequest) (service.Operation, error)

	// SubmitRestorePlacement persists and starts a restore of one archived
	// copy (EPIC E, FR-34). Unlike SubmitRunCycle, nothing about the work
	// it starts happens in this process: the provider carries on
	// restoring across a restart, which is why its row is exempt from the
	// startup sweep and why its status is re-derived on every read. See
	// core/service.BackupService.SubmitRestorePlacement.
	SubmitRestorePlacement(ctx context.Context, req service.RestorePlacementRequest) (service.RestoreSubmission, error)

	// GetOperation returns the current state of a previously submitted
	// operation, for authenticated polling (docs/EPIC-B-multi-nas.md
	// §15.7).
	GetOperation(ctx context.Context, id string) (service.Operation, error)

	// PreviewRetention computes source/set's current FR-18/FR-19/FR-20
	// retention plan and issues it a fresh, immutable plan_id
	// (docs/EPIC-B-multi-nas.md §15.6, issue #96/B3.1). It is read-only:
	// see GET /api/v1/backup-sets/{source}/{set}/retention/preview
	// (handlers_retention.go), which this package never wraps in the
	// destructive-operations gate.
	PreviewRetention(ctx context.Context, source, set string) (service.RetentionPlan, error)

	// ApplyRetentionPlan applies exactly the plan named by
	// req.PlanID, or applies nothing at all and returns
	// service.ErrRetentionPlanStale (docs/EPIC-B-multi-nas.md §15.6's
	// RETENTION_PLAN_STALE contract). See POST /api/v1/backup-sets/
	// {source}/{set}/retention/apply (handlers_retention.go), which — unlike
	// PreviewRetention — this package DOES wrap in the destructive-
	// operations gate: this is the one operation in the whole EPIC that
	// deletes local restore points.
	ApplyRetentionPlan(ctx context.Context, req service.ApplyRetentionRequest) (service.RetentionPlan, error)

	// ListBackupSets and GetBackupSet back GET /api/v1/backup-sets and
	// GET /api/v1/backup-sets/{id} (issue #146/B2.7): read-only, no
	// destructive gate, matching docs/EPIC-B-multi-nas.md §50's
	// read-only bucket ("list sources", "view configuration").
	ListBackupSets(ctx context.Context) ([]service.BackupSet, error)
	GetBackupSet(ctx context.Context, id string) (service.BackupSet, error)

	// CreateBackupSet backs POST /api/v1/backup-sets: the add-backup-set
	// wizard's (#98) three Save buttons, wired for the first time by
	// issue #146. See service.BackupService.CreateBackupSet's own doc
	// for the persist-then-hot-reload sequence this method performs.
	CreateBackupSet(ctx context.Context, req service.CreateBackupSetRequest) (service.CreateBackupSetResult, error)

	// UpdateBackupSet backs PATCH /api/v1/backup-sets/{source}/{set}
	// (issue #350): the edit half of backup-set CRUD, which #146 never
	// built, so until now a configured set could only be changed by
	// hand-editing config.yaml on the machine it runs on.
	//
	// The request is sparse (every field a pointer, nil means leave
	// alone), which is what makes the Web UI's per-box Save write only
	// that box at the layer that persists rather than as a promise the UI
	// makes. State-changing but not destructive, so this package wraps
	// the route in requireCSRF and NOT requireDestructiveGate, following
	// POST /api/v1/backup-sets' own precedent; see
	// core/service.UpdateBackupSet's own doc for the persist-then-reload
	// sequence it shares with creation.
	UpdateBackupSet(ctx context.Context, id string, req service.UpdateBackupSetRequest) (service.BackupSet, error)

	// ImportSSHKey backs POST /api/v1/ssh-keys: the wizard's "Import
	// key" step, persisting client-validated key material server-side
	// for the first time (issue #146). passphrase is "" for an
	// unencrypted key; see service.BackupService.ImportSSHKey's own doc
	// for what a non-empty one does (#269).
	ImportSSHKey(ctx context.Context, raw []byte, passphrase string) (service.SSHKeyRef, error)

	// ProbeHostKey backs POST /api/v1/ssh/host-key-probe: the wizard's
	// "Verify server" step, fetching a real fingerprint instead of a
	// mock (issue #146). Read-only, per §50.
	ProbeHostKey(ctx context.Context, host string, port int) (service.HostKeyProbe, error)

	// TestConnection backs POST /api/v1/backup-sets/test-connection: a
	// pre-save, non-destructive reachability/auth check against a
	// candidate source (issue #146). Read-only, per §50 ("test SSH").
	TestConnection(ctx context.Context, req service.ConnectionTestRequest) (service.ConnectionTestResult, error)

	// ListStorageStatus backs GET /api/v1/system/storage: the FR-21
	// capacity assessment for every configured backup set's local
	// destination (docs/EPIC-B-multi-nas.md §56's Storage UX), issue #104
	// (B3.4). Read-only, per §50 ("view configuration"/health-adjacent
	// surface) — see core/service.BackupService.ListStorageStatus and
	// this package's own handlers_storage.go for why nothing reachable
	// from here can turn a "critical" result into a deletion or a call
	// into internal/retention's apply path.
	ListStorageStatus(ctx context.Context) ([]service.StorageStatus, error)

	// ManagerStorage backs the other half of GET /api/v1/system/storage
	// (issue #286): the ONE manager-wide reading, taken from the
	// filesystem the backup root is on, carrying this manager's own
	// consumption and the operator's cap alongside the volume's figures.
	//
	// It is a separate call rather than something derivable from
	// ListStorageStatus because summing that list cannot answer the same
	// question: an unconfigured instance sums to zero, two sets on one
	// volume sum to twice the disk, and a manager-wide cap has no per-set
	// entry to live on. See core/service.ManagerStorage's own doc, and in
	// particular what a reading with Known false must and must not be
	// rendered as.
	ManagerStorage(ctx context.Context) (service.ManagerStorage, error)

	// Settings and UpdateSettings back GET and PATCH /api/v1/settings
	// (issue #140/B3.7): the one generic, authenticated, CSRF-protected
	// settings surface a shared Web UI administers server-side
	// configuration through, rather than a route per setting.
	//
	// Settings is read-only (docs/EPIC-B-multi-nas.md §50's "view
	// configuration") and reports the policy actually in effect, with
	// FR-18's retention chain already resolved — see
	// core/service.RetentionSettings' own doc for why a caller never sees
	// the legacy daily_days/weekly_months/monthly_months spelling here.
	//
	// UpdateSettings is state-changing but NOT destructive (§50's
	// "create/edit backup set" bucket): it edits configuration and
	// touches no backup data, so this package wraps it in requireCSRF but
	// not requireDestructiveGate — see router.go's own comment on the
	// route for the full reasoning, and settings_boundary_test.go for the
	// proof that a write here and a CLI read agree. Every write it
	// performs goes through the same config.Validate a hand-edited YAML
	// file goes through at boot, and refuses rather than partially
	// applying; this package neither duplicates nor bypasses any part of
	// that.
	Settings(ctx context.Context) (service.Settings, error)
	UpdateSettings(ctx context.Context, req service.UpdateSettingsRequest) (service.Settings, error)

	// SetBackupSetEnabled backs POST /api/v1/backup-sets/{id}/enabled
	// (issue #211): the one write that turns a configured backup set on
	// or off. State-changing but not destructive, per
	// docs/EPIC-B-multi-nas.md §50 -- see core/service's own doc for what
	// a disabled set stops doing and, more importantly, for what it does
	// not touch.
	SetBackupSetEnabled(ctx context.Context, id string, enabled bool) (service.BackupSet, error)

	// SetBackupSetReadOnly backs POST /api/v1/backup-sets/{id}/read-only
	// (issue #316): the CRUD-parity write that turns issue #282's
	// read-only declaration on or off for an already-persisted backup
	// set. State-changing but not destructive — see
	// core/service.SetBackupSetReadOnly's own doc for what each direction
	// does and, in particular, does not undo.
	SetBackupSetReadOnly(ctx context.Context, id string, readOnly bool) (service.BackupSet, error)

	// RemoveBackupSet backs DELETE
	// /api/v1/backup-sets/{source}/{set} (issue #391): take one backup
	// set out of the configuration, so nothing is collected for it from
	// here on.
	//
	// Configuration only, and that is the contract rather than an
	// implementation detail. Every artifact the set produced stays on
	// storage and stays listed under GET /api/v1/backups, which is what
	// the confirmation an operator accepts in the Web UI has always
	// promised; see core/service/backupsetremove.go's own package doc for
	// what it keeps, for what a re-created set with the same id re-adopts,
	// and for the residue an interrupted transfer can leave.
	//
	// State-changing but NOT destructive under docs/EPIC-B-multi-nas.md
	// §50, in the same tier as UpdateBackupSet and CreateBackupSet:
	// nothing reachable from here touches, moves or deletes a byte of
	// backup data. Deleting retained backups is a different operation
	// with a different consent model (FR-20 and the destructive gate),
	// and it is not this one.
	RemoveBackupSet(ctx context.Context, id string) error

	// BackupSetRetention, SetBackupSetRetention and
	// ClearBackupSetRetention back GET/PUT/DELETE
	// /api/v1/backup-sets/{source}/{set}/retention (issue #333): read
	// which retention policy one backup set is retained under, give that
	// set a whole policy of its own, or remove it so the set inherits the
	// deployment's again.
	//
	// Three methods rather than one sparse update, because "give this set
	// no policy of its own" cannot be a value on a request where a nil
	// field already means "leave this alone" — see
	// core/service/backupsetretention.go's own package doc. State-changing
	// but not destructive: this writes configuration, and the retention
	// apply it can change the outcome of stays behind the destructive gate
	// and re-reads the policy at plan time.
	BackupSetRetention(ctx context.Context, id string) (service.BackupSetRetention, error)
	SetBackupSetRetention(ctx context.Context, id string, o service.RetentionOverride) (service.BackupSetRetention, error)
	ClearBackupSetRetention(ctx context.Context, id string) (service.BackupSetRetention, error)
	// BackupSetEditState, BeginBackupSetEdit, RenewBackupSetEdit and
	// EndBackupSetEdit back the three /edit-hold routes (issue #350).
	//
	// A backup set being edited while a cycle runs against it is two
	// writers on one definition, so entering edit mode holds that one
	// set: the pass currently running against it stops, and the scheduler
	// starts no new one until the hold is released or its lease lapses.
	// The read is separate from the write precisely so an operator can be
	// shown what they are about to stop and then decline; see
	// core/service/edithold.go for the lease, and for why it is a lease
	// rather than a flag.
	BackupSetEditState(ctx context.Context, id string) (service.BackupSetEditState, error)
	BeginBackupSetEdit(ctx context.Context, id string) (service.EditHold, error)
	EndBackupSetEdit(ctx context.Context, id string) error

	// TestBackupSetConnection backs the persisted-set mode of POST
	// /api/v1/backup-sets/test-connection (issue #211): the same
	// non-destructive reachability check TestConnection performs, against
	// a set that already exists, so a client re-checking one never has to
	// echo back the key reference and trusted host line it is configured
	// with.
	TestBackupSetConnection(ctx context.Context, id string) (service.ConnectionTestResult, error)

	// ListArtifacts and GetArtifact back GET /api/v1/backups, GET
	// /api/v1/backups/{id} and GET /api/v1/quarantine (issue #211):
	// read-only reads of the FR-9 journal `backup-manager artifacts`
	// already prints.
	ListArtifacts(ctx context.Context, filter service.ArtifactFilter) ([]service.Artifact, error)
	GetArtifact(ctx context.Context, id string) (service.Artifact, error)

	// RevalidateArtifact, RetryArtifactIngestion and ReinstateArtifact
	// back the three quarantine actions. Revalidate reports a verdict and
	// writes nothing; retry takes the re-ingest edge out of quarantine and
	// re-fetches from the remote; reinstate keeps the durable local copy
	// and returns the artifact to the state it already held, which is the
	// answer when the remote is gone or the quarantine was the mistake
	// (issue #220). Reinstating permanently forfeits the artifact's remote
	// delete, which is what makes it safe to offer at all; see
	// core/internal/lifecycle.
	RevalidateArtifact(ctx context.Context, id string) (service.ArtifactCheck, error)
	RetryArtifactIngestion(ctx context.Context, id string) error

	// RetryFailedArtifact backs POST /api/v1/backups/{id}/retry (issue
	// #419): the same re-entry into the pipeline RetryArtifactIngestion
	// performs, one state along, for a backup that is FAILED rather than
	// quarantined. FAILED declares that exit and nothing in this product
	// had ever taken it, so a backup that reached it stopped being worked
	// on permanently.
	RetryFailedArtifact(ctx context.Context, id, note string) error
	ReinstateArtifact(ctx context.Context, id, note string) (service.ArtifactReinstatement, error)

	// PreflightStorageMedium backs POST
	// /api/v1/storage-mediums/{id}/preflight (issue #443): the medium
	// equivalent of TestBackupSetConnection above, and deliberately a
	// stronger check than that one. It writes a probe object, reads it
	// back, compares the class it landed in against the class the
	// configuration claims, asks whether the verification class the
	// medium declares can actually be achieved there, and deletes the
	// probe. A medium that does not work comes back as a report saying
	// so, never as an error; the error is for an id this configuration
	// does not declare.
	PreflightStorageMedium(ctx context.Context, id string) (service.MediumPreflight, error)

	// ListActivity backs GET /api/v1/activity: a read of the durable,
	// append-only lifecycle record, not a second event stream.
	ListActivity(ctx context.Context, limit int) ([]service.ActivityEvent, error)

	// ListOperations backs GET /api/v1/operations: the list counterpart
	// of GetOperation, for a client that holds no operation id to poll
	// with.
	ListOperations(ctx context.Context, limit int) ([]service.Operation, error)

	// Health backs GET /api/v1/system/health: FR-24's backup-freshness
	// verdict for every configured backup set, the same computation
	// `backup-manager status` prints. Deliberately not the same question
	// as /health/ready, which is about this process rather than about
	// whether backups are landing.
	Health(ctx context.Context) (service.HealthReport, error)

	// ScanCatalog and RebuildCatalog back POST /api/v1/catalog/scan and
	// POST /api/v1/catalog/rebuild: FR-9 journal recovery from the
	// recovery manifests already on disk. The scan is the rebuild with
	// nothing written, sharing one implementation so a preview predicts
	// the real pass exactly.
	ScanCatalog(ctx context.Context) (service.CatalogReport, error)
	RebuildCatalog(ctx context.Context) (service.CatalogReport, error)
}

var _ BackupServiceClient = (*service.BackupService)(nil)

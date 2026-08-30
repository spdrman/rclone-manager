// Package service is core's public application-service boundary
// (docs/EPIC-B-multi-nas.md §3.3, §7.2, issue #94/B1.5).
//
// Every package under core/internal is off limits to anything outside
// core/ by construction: Go's own "internal" import rule means
// apps/common/webhost (a different module) cannot import
// core/internal/app, core/internal/config, core/internal/state or
// core/internal/transport, no matter what core/'s go.mod or the repo-root
// go.work say. This package is the seam §7.2 calls for: it sits inside
// core/'s own module tree (so it CAN import those internal packages) and
// exposes only plain, provider-agnostic types and functions — never a
// config.Config, a state.Record, a state.Operation, or anything else an
// internal package owns — to whatever sits on top of it: apps/common/
// webhost today, a future CLI or another provider tomorrow.
//
// BackupService wraps exactly one internal/app.Service, plus the
// idempotency-keyed, configuration-revision-checked durable-operation
// plumbing issue #94 adds on top of it (operations.go). Nothing here
// re-implements lifecycle, discovery, reconciliation or retention policy;
// see internal/app's own package doc for why none of that belongs here
// either.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// closeDrainTimeout bounds how long Close (below) waits for an in-flight
// executeRunCycle to notice ctx was canceled and finish before closing the
// journal out from under it anyway. It is a var, not a const, so a test can
// shrink it rather than waiting out the real value.
//
// Five seconds is generous next to the one journal write (Complete/
// FailOperation) left to make once internal/app.Service.RunCycle itself
// returns, while short enough that an operator restarting a stuck process
// is not left waiting indefinitely on Close; it bounds the grace period
// Close gives RunCycle to wind down, not RunCycle's own execution time,
// which keeps going on its goroutine regardless (Go has no way to force a
// goroutine to stop) until it next checks ctx.Err() between backup sets
// (see internal/app/cycle.go's own shutdown-safety doc).
var closeDrainTimeout = 5 * time.Second

// BackupService is the one exported type provider code depends on. It is
// deliberately opaque: every field that decides how it behaves is
// unexported, so a caller outside core/ can only ever drive it through the
// methods below, never by reaching past them into an internal.Service,
// a *state.Journal or a *config.Config it could not even name.
type BackupService struct {
	// state bundles inner (the wrapped internal/app.Service) and revision
	// (ConfigRevision's value) behind one atomic.Pointer, so every reader
	// — ConfigRevision, SubmitRunCycle/executeRunCycle (operations.go),
	// the scheduler's own tick (scheduler.go), TestConnection
	// (backupsets.go) and ListBackupSets/GetBackupSet (backupsets.go) —
	// always observes one consistent, non-torn {inner, revision} pair
	// with a lock-free Load(), no matter how many goroutines read
	// concurrently with CreateBackupSet's own hot-reload Store() after a
	// config write. Before this, inner and revision were two plain
	// fields written under configMu (below) but read by every one of
	// those call sites with no lock at all — a real, reachable data race
	// under the Go memory model (net/http runs each request on its own
	// goroutine, and the scheduler ticks independently), not merely a
	// theoretical one; see CreateBackupSet's own doc for the write side
	// of this contract.
	state atomic.Pointer[configState]

	journal *state.Journal
	logger  *obs.Logger

	// pollInterval is cfg.PollInterval.Duration(), copied out at
	// construction time so PollInterval() (scheduler.go) can report it
	// without exposing *config.Config itself, which a caller outside
	// core/ cannot even name.
	pollInterval time.Duration

	// ctx/cancel give executeRunCycle a lifetime independent of both
	// context.Background() and any single request's context: it is
	// canceled by Close, so a process shutdown can actually ask an
	// in-flight RunCycle to stop starting new backup sets (see
	// internal/app/cycle.go's own shutdown-safety doc), something
	// context.Background() alone could never do. See executeRunCycle's own
	// doc for why the journal writes that record an operation's outcome
	// deliberately do NOT use this context.
	ctx    context.Context
	cancel context.CancelFunc

	// wg tracks every executeRunCycle goroutine currently running, so
	// Close can wait for it to actually finish (bounded by
	// closeDrainTimeout) instead of returning, and closing the journal,
	// while one is still writing to it.
	wg sync.WaitGroup

	// runOnce enforces this package's single-flight invariant: at most one
	// executeRunCycle call may be inside internal/app.Service.RunCycle at a
	// time, restoring on the API side the "no concurrent pass over the
	// same backup set" guarantee cycle.go's own doc says RunCycle provides
	// "by construction, not by a lock this package has to remember to
	// take" — a guarantee that held only as long as the CLI, calling
	// RunCycle once per process invocation, was RunCycle's sole caller.
	// SubmitRunCycle's goroutine-per-operation is the first caller in this
	// codebase that can call it concurrently with itself; see that
	// method's own doc for why a second submission while this is held is
	// rejected rather than queued.
	runOnce sync.Mutex

	// retentionMu guards retentionPlans (retention.go): every previewed
	// retention plan this BackupService currently holds, keyed by its own
	// plan_id, until ApplyRetentionPlan consumes it (applied, found stale,
	// or expired) or it is simply never applied at all. This is
	// deliberately an in-memory, non-durable store, unlike the operations
	// table: a preview carries its own expires_at precisely so nothing
	// needs to survive a restart — see retention.go's own doc for what IS
	// durable (the apply itself, once confirmed).
	retentionMu    sync.Mutex
	retentionPlans map[string]retentionPlanRecord

	// configPath is the YAML file this BackupService was opened from
	// (Open), or "" for a BackupService built directly with New (every
	// core/ test, which constructs its own *config.Config in memory and
	// has no file backing it). CreateBackupSet (backupsets.go) refuses to
	// run at all when this is "": persisting a backup-set change to a
	// config that has no file of its own to write back to would either
	// silently no-op or panic deeper in, neither of which is the honest
	// failure a caller needs.
	configPath string

	// alertSink is the proactive-alert delivery mechanism a provider app
	// installed through EnableAlerts (alerts.go), or nil when none was
	// ever installed. It is kept here, not only on the wrapped
	// internal/app.Service, because CreateBackupSet's hot reload re-reads
	// alerts.enabled from disk and has to be able to turn alerting ON, in
	// a process that started with it off, without a restart: that needs
	// the mechanism itself, which the wrapped Service does not hold when
	// alerting is off. It is written under configMu, which is also the
	// lock CreateBackupSet reads it under.
	alertSink AlertSink

	// releaseJournal drops the SHARED journal lock runStartupSequence took
	// on this BackupService's behalf (startup.go, lock_unix.go), and is
	// called by Close once the journal handle itself is closed. It is nil
	// for a BackupService built with New, which never took one: New's
	// caller opened the journal itself and owns whatever locking that
	// implied.
	releaseJournal func() error

	// ready records that this BackupService came from Open, and therefore
	// that §46.1's startup sequence completed. See Ready's own doc for why
	// this is stored rather than re-derived: the previous definition of
	// readiness (a non-empty config revision) was true for every
	// BackupService that could exist, on the one flag §36 puts in front of
	// a destructive operation.
	ready bool

	// validatorDir is where this BackupService's registered-validator
	// scripts materialize: a "validators" directory beside
	// cfg.State.Database (validatorScriptDir). It is derived once, at
	// construction, from the config this BackupService was built from,
	// and never changes -- a hot reload cannot move the state database,
	// since config.Validate would have to accept a whole new one first,
	// and CreateBackupSet only ever appends a backup set. Close removes
	// the directory. It is "" for a BackupService built from a config
	// with no state.database (a core/ test's in-memory *config.Config),
	// which is what makes resolving a validator on one fail loudly
	// instead of writing an executable somewhere guessed.
	validatorDir string

	// configMu serializes every call that reads-modifies-writes this
	// BackupService's configuration (today: CreateBackupSet) against
	// ITSELF — two concurrent CreateBackupSet calls must not interleave
	// their read-modify-write of the config file. It is a separate lock
	// from runOnce on purpose: runOnce guards "at most one RunCycle
	// executing", a completely different invariant, and a backup-set
	// creation blocking on, or being blocked by, an in-progress run_cycle
	// would be a surprising and unnecessary coupling between the two.
	// configMu does NOT protect state (above) — state's own
	// atomic.Pointer is what makes every READ of it safe with no lock at
	// all; configMu only ever needs to keep two WRITERS (two overlapping
	// CreateBackupSet calls) from racing each other's file write (see
	// backupsets.go's CreateBackupSet for the full sequence).
	configMu sync.Mutex
}

// configState bundles a BackupService's wrapped internal/app.Service
// (inner) with the configuration revision (revision) computed from
// exactly the *config.Config inner was itself built from, so the two can
// never be swapped independently and observed as a mismatched pair: a
// reader that Load()s one configState always gets the revision that
// actually describes that inner, never inner from one hot-reload and
// revision from another. See BackupService.state's own doc for why this
// is a Pointer, not two separate fields.
type configState struct {
	inner    *app.Service
	revision string
}

// New builds a BackupService from already-constructed dependencies. This
// is the constructor core/'s own tests use (they can build a
// *config.Config and a *state.Journal directly, exactly as
// internal/app's own tests do); apps/common/webhost, which cannot
// construct any of those, uses Open instead.
//
// tr and logger may be nil, with the same meaning internal/app.New already
// documents: a nil Transport is only safe for a Config with nothing that
// ever needs to reach a remote, and a nil logger is a silent no-op — every
// obs.Logger method used in this package (operations.go) is a safe no-op
// on a nil *obs.Logger, exactly as internal/obs's own package doc
// promises.
//
// New does NOT resolve a backup set's Validation.ValidatorID into a
// runnable Validation.Command: that is load-time work, and
// OpenConfigAndJournal (below) is where it happens, so it covers both
// production entry points (Open here, and cmd/backup-manager's own
// openService) in one place rather than each caller of this constructor
// remembering it. A cfg handed to New with an unresolved ValidatorID is
// not silently un-validated either: internal/lifecycle/verify.go refuses
// an artifact whose backup set names a validator that was never resolved,
// rather than reading it as "no validator configured".
//
// New also sweeps journal for any operation left at "queued" or "running"
// by a previous process using it (see
// internal/state.Journal.FailInterruptedOperations's own doc): a fresh
// BackupService has made no SubmitRunCycle call of its own yet, so any row
// in either status cannot belong to this instance, and nothing would ever
// move it out of that state otherwise.
func New(cfg *config.Config, journal *state.Journal, tr transport.Transport, logger *obs.Logger) *BackupService {
	ctx, cancel := context.WithCancel(context.Background())
	b := &BackupService{
		journal:        journal,
		logger:         logger,
		pollInterval:   cfg.PollInterval.Duration(),
		ctx:            ctx,
		cancel:         cancel,
		retentionPlans: make(map[string]retentionPlanRecord),
		validatorDir:   validatorScriptDir(cfg),
	}
	b.state.Store(&configState{inner: app.New(cfg, journal, tr, logger), revision: computeConfigRevision(cfg)})

	if _, err := journal.FailInterruptedOperations(context.Background(), now(), "interrupted by restart"); err != nil {
		logger.Error(context.Background(), "sweep-interrupted-operations", err)
	}

	return b
}

// OpenConfigAndJournal loads and validates configPath and opens (migrating)
// its configured SQLite journal at cfg.State.Database. This is the
// bootstrap sequence Open (below) and cmd/backup-manager's own openService
// helper both need — the exact same "read this config file, open/migrate
// this journal" side effects, previously implemented twice — factored out
// so it exists in exactly one place; see this package's introducing PR
// description for why that duplication existed in the first place and
// this issue's own review for why it stopped being acceptable.
//
// Everything between loading the config and having a usable journal is
// docs/EPIC-B-multi-nas.md §46.1's startup sequence, and it lives in
// runStartupSequence (startup.go), not here: read that function's own doc
// for the ordered steps, why the state directory is validated before the
// lock is taken, and exactly what each failure between them does to the
// data already on disk. What matters at THIS level is the contract it
// gives every caller: a failure returns a non-nil error and a nil
// *state.Journal, which Open (below) and cmd/backup-manager's openService
// both already treat as fatal, so a failed migration means no
// BackupService is ever constructed and no daemon, API, scheduler tick or
// transfer ever starts.
func OpenConfigAndJournal(ctx context.Context, configPath string) (*config.Config, *state.Journal, func() error, error) {
	cfg, err := config.LoadAndValidate(configPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("service: load config: %w", err)
	}

	journal, releaseJournal, err := runStartupSequence(ctx, cfg.State.Database)
	if err != nil {
		return nil, nil, nil, err
	}

	// Load-time resolution of every backup set's registered validator
	// (validator.go's applyValidatorCatalog): the config file carries an
	// id, the running process carries the command it resolves to. This
	// happens AFTER runStartupSequence, which is what validated the state
	// directory the scripts materialize into, and after LoadAndValidate,
	// which refuses a Validation naming both an id and a command.
	//
	// An unregistered id fails startup here, deliberately. The alternative
	// -- carrying on with no validator for that backup set -- would mean a
	// typo in validator_id silently disabling the one check standing
	// between a bad artifact and remote deletion, with the operator
	// believing it was on.
	if err := applyValidatorCatalog(cfg); err != nil {
		// runStartupSequence already took the shared journal lock and
		// opened the journal, and this function's contract is that a
		// failure returns a nil *state.Journal. Both have to be given back
		// here, in that order (see Close's own comment on why the lock is
		// released only after the handle is closed), or a startup that
		// fails on a bad validator_id leaves the lock held for the life of
		// the process that is about to exit anyway.
		_ = journal.Close()
		if releaseJournal != nil {
			_ = releaseJournal()
		}
		return nil, nil, nil, err
	}

	return cfg, journal, releaseJournal, nil
}

// Open is the production constructor: it loads and validates configPath,
// opens (and migrates) its configured SQLite journal (both via
// OpenConfigAndJournal), and wires a real rclone transport, since a web
// host has no narrower "read-only" use case the way some CLI subcommands
// do — the API can always be asked to run a cycle.
//
// The returned cleanup func closes the journal; callers should always
// `defer cleanup()` (or handle its error) once they are done with the
// returned BackupService.
func Open(ctx context.Context, configPath string) (*BackupService, func() error, error) {
	logger := obs.New(os.Stdout, obs.LevelInfo)

	cfg, journal, releaseJournal, err := OpenConfigAndJournal(ctx, configPath)
	if err != nil {
		// §46.1 asks for a failed startup to be actionable, not merely
		// fatal. The process is about to exit without ever constructing a
		// BackupService (which is what makes readiness fail closed), so
		// this line is the only record of WHY it did: without it an
		// operator diagnosing a container that will not come up has a
		// non-zero exit code and nothing else.
		logger.Error(ctx, "startup", err)
		return nil, nil, err
	}

	svc := New(cfg, journal, rclone.New(), logger)
	svc.configPath = configPath
	svc.releaseJournal = releaseJournal
	// ready is set here, and only here: Open is the one constructor that
	// runs §46.1's startup sequence, so it is the one constructor that can
	// truthfully report the sequence completed. See the field's own doc.
	svc.ready = true
	return svc, svc.Close, nil
}

// Close cancels this BackupService's own execution context (see the ctx
// field's doc) and waits, up to closeDrainTimeout, for any in-flight
// executeRunCycle goroutine to finish before releasing the underlying
// journal handle. Waiting first, rather than closing the journal
// immediately, is what keeps a RunCycle that is already past its last
// ctx.Err() check from hitting a closed database mid-write; timing out
// instead of waiting forever is what keeps a process shutdown from hanging
// on an operation that ignores cancellation for longer than that (a stuck
// remote, for example) — Close proceeds anyway once the timeout elapses,
// though the goroutine itself, unkillable in Go, keeps running until it
// eventually returns on its own.
func (b *BackupService) Close() error {
	b.cancel()

	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(closeDrainTimeout):
		b.logger.Error(context.Background(), "close",
			fmt.Errorf("timed out after %s waiting for an in-flight operation to finish", closeDrainTimeout))
	}

	err := b.journal.Close()
	// The shared journal lock is released only after the journal handle
	// itself is closed, never before: the whole point of holding it is that
	// no other process migrates this journal while THIS process still has
	// it open, and "still has it open" ends at the line above, not at the
	// start of Close.
	if b.releaseJournal != nil {
		if releaseErr := b.releaseJournal(); releaseErr != nil && err == nil {
			err = releaseErr
		}
	}

	// The materialized validator scripts go last, after the drain above
	// has given any in-flight cycle its chance to finish running one.
	// They are this process's own scaffolding, rewritten from the
	// embedded copies on the next start, so removing them loses nothing
	// -- and not removing them is how the previous implementation leaked
	// a directory per process start. A failure here is logged, never
	// returned: the caller has nothing to do about it, and reporting a
	// clean shutdown as failed because a directory would not delete would
	// be worse than the leak.
	if rmErr := removeValidatorScripts(b.validatorDir); rmErr != nil {
		b.logger.Error(context.Background(), "close", rmErr)
	}
	return err
}

// Ready reports whether this BackupService was constructed by Open, having
// completed docs/EPIC-B-multi-nas.md §46.1's startup sequence in full
// (state-directory validation, the startup lock, the pending-migration
// check, any migration, and the shared journal lock it still holds).
//
// It is a fact this value owns, not something re-derived from its
// configuration: a BackupService built with New has not run that sequence
// (its caller opened the journal some other way, or handed in one built
// for a test) and says so. §36 makes readiness the precondition an API
// client checks before a destructive operation, and a precondition that
// cannot be false is worse than no precondition at all.
func (b *BackupService) Ready() bool {
	return b.ready
}

// ConfigRevision identifies the exact configuration content this
// BackupService was built from. It changes if, and only if, the
// configuration's content changes: two BackupService instances built from
// byte-for-byte identical config report the same revision, and any
// difference in the loaded configuration (a source added, a field edited)
// changes it. SubmitRunCycle compares a caller-supplied revision against
// this to refuse acting against a configuration the caller no longer has
// an accurate picture of (docs/EPIC-B-multi-nas.md §14, §15.6's
// RETENTION_PLAN_STALE precedent applied to configuration generally).
//
// It is computed at construction time from whatever *config.Config the
// caller handed New (or Open loaded from disk), and again by
// CreateBackupSet (backupsets.go) every time it hot-reloads b.state after
// persisting a change: two BackupService values (or the same one, before
// and after a CreateBackupSet call) report different revisions exactly
// when their underlying configuration content actually differs, whether
// that difference came from a restart against a manually edited YAML
// file or from an in-process backup-set creation.
func (b *BackupService) ConfigRevision() string {
	return b.state.Load().revision
}

// computeConfigRevision hashes a canonical YAML encoding of cfg. YAML
// (rather than, say, fmt.Sprintf("%+v", cfg)) is used because
// internal/config's own struct tags are already YAML tags with a fixed,
// declaration-order field layout and no maps, so encoding it is
// deterministic without this package needing to canonicalize anything
// itself, and because it is already an internal/config-side dependency
// this package pulls in for free (LoadAndValidate uses the same library),
// not a new one.
func computeConfigRevision(cfg *config.Config) string {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		// cfg is a plain data structure (strings, ints, durations, slices
		// of the same): yaml.Marshal failing here would mean
		// internal/config grew a field this package's assumption no
		// longer holds for, which is a programmer error to notice loudly,
		// not a runtime condition to paper over with a fallback revision
		// that would silently never change.
		panic(fmt.Sprintf("service: computing config revision: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// Version is the plain, provider-agnostic shape of "what is this binary"
// (docs/EPIC-B-multi-nas.md §15.1's GET /api/v1/system/version). Field
// names are chosen so nothing here spells "rclone" or "sqlite": that is
// enforced by a contract test at the HTTP layer
// (apps/common/webhost), but choosing neutral names here is what makes
// that test pass by construction rather than by the HTTP layer having to
// rename fields defensively.
type Version struct {
	CoreVersion   string
	Commit        string
	GoVersion     string
	EngineVersion string
}

// BuildVersion wraps internal/app.BuildVersionInfo, translating its shape
// into Version. It is a package-level function, not a BackupService
// method, because it needs no instance state — exactly like
// BuildVersionInfo itself.
func BuildVersion(binaryVersion, commit string) Version {
	info := app.BuildVersionInfo(binaryVersion, commit)
	return Version{
		CoreVersion:   info.BinaryVersion,
		Commit:        info.Commit,
		GoVersion:     info.GoVersion,
		EngineVersion: info.RcloneVersion,
	}
}

// now is a seam over time.Now so a future test can freeze the clock this
// package reads for operation timestamps; nothing currently overrides it.
var now = func() time.Time { return time.Now().UTC() }

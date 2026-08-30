// Package app is the presentation-agnostic application-service layer the
// EPIC's "UI / Presentation Architecture" section calls for:
//
//	          BackupService
//	               |
//	 +-------------+-------------+
//	 v             v             v
//	CLI          daemon      future HTTP API
//
// Every business rule this project has (which cycle order is safe, when a
// transfer is allowed to begin, when a remote object may be destroyed, what
// "healthy" means) already lives in internal/config, internal/state,
// internal/lifecycle, internal/discovery, internal/reconcile,
// internal/retention, internal/capacity and internal/health. Nothing in
// this repository had, until this package, ever called any of it: every
// one of those packages was fully built and fully tested in isolation, but
// nothing wired them together into something an operator could run.
//
// This package is that wiring, and only that wiring. It orchestrates
// lifecycle, discovery, reconciliation, retention, capacity and health
// exactly as the EPIC's cycle order requires (reconcile, then discover,
// then per-artifact transfer/verify/commit/delete, then a retention
// preview), and it does so in one place so that cmd/backup-manager's `run`
// and `daemon` subcommands, and every other CLI command that needs a
// use case (`status`, `fetch`, `retention`, `reconcile`, `validate`, ...),
// call the exact same Service methods. A future HTTP API is meant to be
// able to do the same without this package changing shape: nothing here
// prints anything, reads os.Args, or reads os.Stdin, on purpose.
//
// # Business rules stay where they were built
//
// This package deliberately does not re-implement anything the packages
// above already own. It never decides what a legal state transition is
// (that is lifecycle.Advance's job, called through this package), it never
// decides whether a remote object is safe to delete (lifecycle.DeleteRemote
// owns FR-15/FR-16 revalidation completely), and it never deletes a local
// file for retention (FR-20 is issue #21, still open at the time this
// package was written; see retention.go for exactly what this package does
// and does not do about it). What this package owns is sequencing: calling
// the right function, on the right artifact, in the right order, and
// stopping at the right boundary when a shutdown is requested.
package app

import (
	"context"
	"sync"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/alert"
	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/retry"
)

// Journal is the slice of internal/state.Journal every use case in this
// package needs. It is an interface, not *state.Journal, for the same
// reason internal/lifecycle.Journal, internal/reconcile.Journal and
// internal/revalidate.Journal already are: a test can substitute a fake
// without standing up SQLite, and this package cannot reach past this
// surface into migrations or schema concerns it does not own.
//
// Its method set is a superset of every one of those narrower interfaces
// (Get and RecordTransition for lifecycle.Journal; those two plus
// ListByBackupSet for reconcile.Journal and revalidate.Journal), so a
// Journal value here can be passed directly wherever those packages'
// Deps.Journal fields expect their own narrower interface: Go accepts an
// interface-to-interface assignment whenever the source's method set is a
// superset of the destination's.
type Journal interface {
	Get(ctx context.Context, id model.ArtifactID) (state.Record, error)
	RecordTransition(ctx context.Context, t state.Transition) (state.Outcome, error)
	ListByBackupSet(ctx context.Context, set model.BackupSetID) ([]state.Record, error)
	ListByState(ctx context.Context, st string) ([]state.Record, error)
	LastEnteredAt(ctx context.Context, id model.ArtifactID, st string) (time.Time, bool, error)
}

var _ Journal = (*state.Journal)(nil)

// DefaultRetryPolicy bounds how long this package's own network-facing use
// cases (a discovery pass, a reconciliation pass, and the copy step inside
// lifecycle.Transfer) keep retrying a transport.Transient failure before
// giving up for this cycle.
//
// It is deliberately bounded, unlike retry.DefaultPolicy's unbounded
// (MaxAttempts: 0, "retry until ctx is done") schedule: FR-1 requires this
// package to "continue processing unrelated sources after one source
// fails", and every use case in this package runs one backup set at a time,
// sequentially (see cycle.go and daemon.go). An unbounded retry against one
// unreachable source would starve every other configured backup set for as
// long as the outage lasts, which is exactly the failure this bound exists
// to prevent. Six attempts with this schedule spans a little over two
// minutes worst case (1s, 2s, 4s, 8s, 16s, capped at 30s), long enough to
// ride out a genuine blip without holding a whole cycle hostage to a
// genuinely down source; a source still unreachable after that is picked up
// again next cycle, which for `daemon` is a bounded wait of its own
// (poll_interval), not a lost recovery opportunity.
var DefaultRetryPolicy = retry.Policy{
	BaseDelay:   time.Second,
	MaxDelay:    30 * time.Second,
	Multiplier:  2,
	MaxAttempts: 6,
}

// Service is the application-service layer's one exported type. Every
// field is a dependency this package needs handed to it; Service computes
// nothing at construction time; New only fills in the documented defaults
// for whichever fields are left at their zero value.
type Service struct {
	// Config is the manager's already-validated runtime configuration
	// (config.LoadAndValidate). Service never mutates it.
	Config *config.Config

	// Journal is the FR-9 lifecycle journal every use case reads from and
	// writes to.
	Journal Journal

	// Transport is the FR-3 transport boundary. It is nil for use cases
	// that never need to reach a remote (Sources, ListArtifacts,
	// RetentionPreview, ValidateArtifact, BuildHealthReport): calling a
	// method that does need one with a nil Transport returns a clear error
	// rather than a nil-pointer panic.
	Transport transport.Transport

	// Logger is the FR-23 structured-observability sink. A nil Logger is a
	// safe no-op everywhere (see internal/obs's package doc), so leaving
	// this unset is always safe, just silent.
	Logger *obs.Logger

	// Now is injectable so a test can control every clock reading this
	// package makes (cycle timestamps, retention's "as of" instant, health's
	// "as of" instant). Nil means time.Now.
	Now func() time.Time

	// Capacity is FR-21's thresholds, consulted before every transfer
	// begins (see pipeline.go's admitCapacity). Its zero value
	// (WarningFreeBytes, CriticalFreeBytes and SafetyMarginBytes all 0) is
	// valid (internal/capacity.Thresholds.Validate requires only that
	// Warning >= Critical) and still enforces FR-21's hard rule, "do not
	// begin a transfer known not to fit at all", because
	// internal/capacity.Assess reports Critical whenever the artifact
	// simply does not fit, regardless of what the thresholds are. What the
	// zero value cannot do is warn before the disk is actually full, since
	// there is no configured margin to warn against. See this package's
	// introducing PR description for why: internal/config.BackupSet has no
	// warning_free_bytes / critical_free_bytes / safety_margin_bytes
	// fields yet, and extending it is out of this package's file scope.
	Capacity capacity.Thresholds

	// RetryPolicy bounds this package's own network-facing retries (see
	// DefaultRetryPolicy). The zero value uses DefaultRetryPolicy.
	RetryPolicy retry.Policy

	// Alerts is Work Package 3.5's proactive-alert dispatcher
	// (docs/EPIC-B-multi-nas.md §71). Nil, the zero value, means
	// alerting is off, which is the default and what every caller that
	// never calls EnableAlerts gets. Set it through EnableAlerts
	// (alerts.go) rather than directly: that is where the configured
	// opt-in is honoured.
	Alerts *alert.Dispatcher

	mu            sync.Mutex
	lastPoll      map[model.BackupSetID]time.Time
	lastRetention map[model.BackupSetID]time.Time
}

// New builds a Service from its required dependencies. Transport and
// Logger may be left nil by the caller afterward for read-only use cases
// that do not need them; New itself never rejects a nil value here, since
// which fields a given CLI command actually needs is that command's own
// business (see cmd/backup-manager).
func New(cfg *config.Config, journal Journal, tr transport.Transport, logger *obs.Logger) *Service {
	return &Service{
		Config:    cfg,
		Journal:   journal,
		Transport: tr,
		Logger:    logger,
	}
}

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func (s *Service) logger() *obs.Logger { return s.Logger }

func (s *Service) retryPolicy() retry.Policy {
	if s.RetryPolicy != (retry.Policy{}) {
		return s.RetryPolicy
	}
	return DefaultRetryPolicy
}

// lifecycleDeps adapts Service into the lifecycle.Deps shape every
// lifecycle.Advance/Transfer/Verify/Commit/DeleteRemote call needs.
func (s *Service) lifecycleDeps() lifecycle.Deps {
	return lifecycle.Deps{Journal: s.Journal, Transport: s.Transport, Now: s.Now}
}

// sourceFor builds the transport.Source one backup set's configured Remote
// describes. This is the one place config.Remote's fields are translated
// into transport.Source's, so every use case in this package (the cycle,
// fetch, reconcile) agrees on exactly how that translation works.
func sourceFor(src config.Source, bs config.BackupSet) transport.Source {
	r := bs.Remote
	return transport.Source{
		ID:   bs.ID.String(),
		Type: r.Type,
		Host: r.Host,
		Port: r.Port,
		User: r.User,
		// All three key sources have to travel, not just the file one.
		// config.Validate has already refused anything but exactly one of
		// them, and normalized the deprecated key_file into Key.File, so
		// forwarding all three here is what makes key.env and key.command
		// work in a real run rather than only in the adapter's own tests.
		KeyFile:    r.Key.File,
		KeyEnv:     r.Key.Env,
		KeyCommand: r.Key.Command,
		KnownHosts: r.KnownHosts,
		Root:       bs.RemotePath,
	}
}

// lookupBackupSet finds the configured (config.Source, config.BackupSet)
// pair named by sourceName/setName. It is what `fetch --source ... --backup-set
// ...` resolves its two flags through.
func (s *Service) lookupBackupSet(sourceName, setName string) (config.Source, config.BackupSet, error) {
	for _, src := range s.Config.Sources {
		if src.Name != sourceName {
			continue
		}
		for _, bs := range src.BackupSets {
			if bs.Name == setName {
				return src, bs, nil
			}
		}
		return config.Source{}, config.BackupSet{}, &NotFoundError{Kind: "backup set", Name: sourceName + "/" + setName}
	}
	return config.Source{}, config.BackupSet{}, &NotFoundError{Kind: "source", Name: sourceName}
}

// backupSetConfigFor finds the configured (config.Source, config.BackupSet)
// pair for an already-resolved model.BackupSetID, e.g. one read back off a
// journal record. ok is false when no configured backup set has this id
// (the journal remembers artifacts from a backup set an operator has since
// removed from config).
func (s *Service) backupSetConfigFor(id model.BackupSetID) (config.Source, config.BackupSet, bool) {
	for _, src := range s.Config.Sources {
		for _, bs := range src.BackupSets {
			if bs.ID == id {
				return src, bs, true
			}
		}
	}
	return config.Source{}, config.BackupSet{}, false
}

func (s *Service) recordSuccessfulPoll(set model.BackupSetID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastPoll == nil {
		s.lastPoll = make(map[model.BackupSetID]time.Time)
	}
	s.lastPoll[set] = s.now()
}

func (s *Service) lastPollAt(set model.BackupSetID) *time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.lastPoll[set]
	if !ok {
		return nil
	}
	return &t
}

func (s *Service) recordRetentionRun(set model.BackupSetID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastRetention == nil {
		s.lastRetention = make(map[model.BackupSetID]time.Time)
	}
	s.lastRetention[set] = s.now()
}

func (s *Service) lastRetentionAt(set model.BackupSetID) *time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.lastRetention[set]
	if !ok {
		return nil
	}
	return &t
}

// NotFoundError reports that a CLI-supplied name (a --source, a
// --backup-set, an artifact id) does not match anything this package knows
// about.
type NotFoundError struct {
	Kind string
	Name string
}

func (e *NotFoundError) Error() string {
	return "app: no configured " + e.Kind + " named " + e.Name
}

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
	"time"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// BackupService is the one exported type provider code depends on. It is
// deliberately opaque: every field that decides how it behaves is
// unexported, so a caller outside core/ can only ever drive it through the
// methods below, never by reaching past them into an internal.Service,
// a *state.Journal or a *config.Config it could not even name.
type BackupService struct {
	inner    *app.Service
	journal  *state.Journal
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
// ever needs to reach a remote, and a nil logger is a silent no-op.
func New(cfg *config.Config, journal *state.Journal, tr transport.Transport, logger *obs.Logger) *BackupService {
	return &BackupService{
		inner:    app.New(cfg, journal, tr, logger),
		journal:  journal,
		revision: computeConfigRevision(cfg),
	}
}

// Open is the production constructor: it loads and validates configPath
// exactly as cmd/backup-manager's own openService helper does, opens (and
// migrates) its configured SQLite journal, and wires a real rclone
// transport, since a web host has no narrower "read-only" use case the way
// some CLI subcommands do — the API can always be asked to run a cycle.
//
// The returned cleanup func closes the journal; callers should always
// `defer cleanup()` (or handle its error) once they are done with the
// returned BackupService.
func Open(ctx context.Context, configPath string) (*BackupService, func() error, error) {
	cfg, err := config.LoadAndValidate(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("service: load config: %w", err)
	}

	journal, err := state.Open(ctx, cfg.State.Database)
	if err != nil {
		return nil, nil, fmt.Errorf("service: open state: %w", err)
	}

	svc := New(cfg, journal, rclone.New(), obs.New(os.Stdout, obs.LevelInfo))
	return svc, journal.Close, nil
}

// Close releases the underlying journal handle.
func (b *BackupService) Close() error {
	return b.journal.Close()
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
// This has no persistence or reload story of its own: it is computed once,
// at construction time, from whatever *config.Config the caller handed
// New (or Open loaded from disk). Backup-set CRUD and any other API
// surface that would actually let a configuration change while a process
// keeps running are out of this issue's scope (see this package's
// introducing PR description); today, two BackupService values report
// different revisions because they were each built from a different
// config, for example across a restart with an edited YAML file, or, in a
// test, deliberately, to prove the conflict check itself.
func (b *BackupService) ConfigRevision() string {
	return b.revision
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

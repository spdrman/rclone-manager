// Package config is the FR-5 configuration layer: it reads the manager's
// whole runtime configuration from an operator-supplied YAML file, and
// validates it before anything downstream is allowed to touch it.
//
// Two things this package is built around:
//
//   - Configuration must never require recompilation. Nothing about a
//     deployment's shape (which sources, which backup sets, retention
//     policy, timeouts) is compiled in. Load reads it from a file path at
//     runtime, so changing any of it is an edit and a restart, never a
//     rebuild.
//
//   - Configuration must be validated before any destructive processing
//     begins. Load and Validate are deliberately two different steps: Load
//     only parses, Validate is the one place that decides whether a parsed
//     config is safe to act on. A retention pass, a remote delete or a
//     local prune should never be the first place a bad config gets
//     noticed (see validate.go for the reasoning behind each check).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// Config is the manager's whole runtime configuration (FR-5).
//
// A Config fresh out of Load has not been checked for semantic sense: call
// Validate (or use LoadAndValidate) before anything reads it for real.
type Config struct {
	PollInterval Duration  `yaml:"poll_interval"`
	State        State     `yaml:"state"`
	Sources      []Source  `yaml:"sources"`
	Retention    Retention `yaml:"retention"`
}

// State configures the SQLite lifecycle journal (FR-9). SQLite is
// mandatory, so there is exactly one field here, not a choice of backend.
type State struct {
	Database string `yaml:"database"`
}

// Source is one origin of backup sets, e.g. "production" or "staging". It
// exists as a grouping level because a backup set's identity (FR-7) is
// source-plus-set, not the set name alone: two different sources are
// allowed to each have a "postgres-primary" set without colliding.
type Source struct {
	Name       string      `yaml:"id"`
	BackupSets []BackupSet `yaml:"backup_sets"`
}

// BackupSet is one stream of logically interchangeable restore points
// (FR-7): one remote, one local destination, one retention/verification
// policy.
type BackupSet struct {
	Name string `yaml:"id"`

	// ID is never read from YAML directly: the file has no single "id" key
	// for it, only this backup set's own Name plus its parent Source's
	// Name. Validate builds it through model.NewBackupSetID and populates
	// this field, so nothing in this package assembles a BackupSetID by
	// concatenating strings. It stays the zero value until Validate
	// succeeds.
	ID model.BackupSetID `yaml:"-"`

	Remote     Remote     `yaml:"remote"`
	RemotePath string     `yaml:"remote_path"`
	LocalPath  string     `yaml:"local_path"`
	Include    []string   `yaml:"include"`
	Completion Completion `yaml:"completion"`
	StaleAfter Duration   `yaml:"stale_after"`

	// Disabled excludes this backup set from RunCycle (internal/app/
	// cycle.go) without removing its configuration: FR-7's "disable
	// source" state-changing-but-non-destructive action (§50), and
	// issue #146 (B2.7)'s "Save disabled" wizard tier. The zero value
	// (false) is enabled, so every backup set an operator already has
	// running today, written before this field existed, keeps running
	// unchanged after an upgrade.
	Disabled bool `yaml:"disabled"`

	Validation   Validation   `yaml:"validation"`
	Revalidation Revalidation `yaml:"revalidation"`
}

// Remote describes where a backup set's artifacts come from. Type selects
// which of the two backends FR-4 registers is used; the fields that matter
// depend on which one.
type Remote struct {
	Type string `yaml:"type"` // "local" or "sftp"; see FR-4
	Host string `yaml:"host"`
	Port int    `yaml:"port"` // 0 means the backend's default port
	User string `yaml:"user"`

	// KeyFile is deprecated in favour of Key.File, and still works exactly
	// as before (#74): docs/ssh-setup.md and docs/deployment.md both
	// document it, and an operator's existing config must not break. It is
	// treated as one more spelling of "the file resolver", not a fourth,
	// independent key source: Validate refuses a config that sets both this
	// and Key.File, exactly as it refuses two of Key's own fields set
	// together.
	KeyFile string `yaml:"key_file"`

	// Key names exactly one way to obtain this remote's SSH private key.
	// See the Key type's own doc for why File, Env and Command are the only
	// three, and why there is deliberately no field here for raw key bytes.
	Key Key `yaml:"key"`

	KnownHosts string `yaml:"known_hosts"`
}

// Key names exactly one way for an sftp Remote to obtain its SSH private
// key (#74). Exactly one of File, Env or Command may be set; Validate
// enforces that (and, for the deprecated Remote.KeyFile alias, treats it as
// interchangeable with Key.File rather than a fourth option).
//
// There is deliberately no field here for the key's raw bytes. Key only
// ever names WHERE the key lives, never carries it: a file rclone opens
// itself, an environment variable this process reads, or a command it
// runs. That omission is the whole point of the type, not an oversight: the
// one place resolved key material is allowed to reach rclone (its key_pem
// option, see internal/transport/rclone/ssh.go) must only ever be reachable
// through a resolver's output, never through anything an operator can spell
// directly in YAML. Search this package for "key_pem" if that sentence
// looks like it needs proving; you will not find it, on purpose.
type Key struct {
	// File points at the private key on disk. This is the default and the
	// documented preference (docs/ssh-setup.md): of the three, it is the
	// only one that never puts key material into this process's own
	// memory, because rclone opens the file itself rather than this
	// program reading it first.
	File string `yaml:"file"`

	// Env names an environment variable this process reads the key from at
	// connection time.
	Env string `yaml:"env"`

	// Command is an argv array: Command[0] is the executable, invoked
	// directly (never through a shell, so shell metacharacters in any
	// element are inert literal bytes), and the rest are its arguments. Its
	// stdout is treated as the key. This is the path a future secrets
	// manager (OpenBao, Vault, SOPS, 1Password, AWS Secrets Manager, ...)
	// adopts without this project taking a dependency on any of their SDKs
	// or picking a winner among them.
	Command []string `yaml:"command"`
}

// isZero reports whether none of Key's three sources are set, so callers
// can tell "no key: block in the YAML at all" apart from a source with an
// empty value in one of its fields.
func (k Key) isZero() bool {
	return k.File == "" && k.Env == "" && len(k.Command) == 0
}

// Completion selects how a backup set decides a remote artifact is finished
// being written, rather than still in flight (FR-8).
type Completion struct {
	Strategy  string   `yaml:"strategy"` // "rename", "marker" or "stable"
	StableFor Duration `yaml:"stable_for"`

	// DeleteSafetyDelay is WP3.2's additional deletion-safety delay
	// (docs/EPIC-B-multi-nas.md §26 Step 3, §71 Work Package 3.2): only
	// used, and required, when Strategy == "stable". "stable" only ever
	// confirms a size/mtime heuristic, never a producer completion
	// signal the way "rename"/"marker" do, so FR-15's remote-delete gate
	// (internal/lifecycle/remotedelete.go) additionally requires this
	// much time to have passed since the artifact last reached a
	// confirmed-good journal state before it treats a "stable" artifact
	// as equivalent to one completed by "rename" or "marker". This is
	// deliberately a separate field from StableFor, not a second use of
	// it: StableFor answers "has this looked done long enough to start
	// processing it at all" (internal/discovery/complete.go); this
	// answers a different question asked at a different, later, more
	// dangerous point in the pipeline, "has it looked done long enough
	// to destroy the only other copy".
	DeleteSafetyDelay Duration `yaml:"delete_safety_delay"`
}

// Validation configures how a transferred artifact gets checked before it's
// allowed to be treated as a good restore point (FR-13).
type Validation struct {
	Hash    string   `yaml:"hash"` // "" or "sha256"
	Command *Command `yaml:"command"`
}

// Command is an optional external validator, e.g. something that opens a
// database dump and confirms it actually restores. It is a pointer field on
// Validation so that "no validator configured" (command: null, or the key
// left out entirely) is distinguishable from a validator that happens to
// have an empty executable.
type Command struct {
	Executable string   `yaml:"executable"`
	Timeout    Duration `yaml:"timeout"`
}

// Revalidation configures Phase 4's scheduled re-verification of artifacts
// that already reached a durable, once-good state (COMMITTED,
// REMOTE_DELETE_PENDING or COMPLETE). Bit rot does not announce itself, and
// a backup that verified six months ago is not guaranteed to still verify
// today; this is what re-checks it without waiting for a restore attempt
// to find out the hard way.
//
// It is entirely optional. The zero value (Hash false, Command nil) means
// disabled: nothing is re-checked, ever, for this backup set, which is
// exactly today's behavior and stays the default so an existing config
// keeps working unchanged. Re-reading, and potentially re-hashing, a NAS's
// worth of already-verified data has a real I/O cost, so an operator has
// to opt in explicitly and choose both a cadence (Interval) and a scope
// (MaxPerCycle) rather than this package guessing safe values for either;
// see validateRevalidation.
type Revalidation struct {
	// Interval is how long since an artifact's last check still counts as
	// fresh; once exceeded, the artifact becomes due for another one.
	Interval Duration `yaml:"interval"`

	// MaxPerCycle bounds how many due artifacts a single revalidation pass
	// actually checks, so a backlog of simultaneously-due artifacts (for
	// example right after a large initial backfill all finished within
	// the same window) cannot turn into one unbounded read-and-hash sweep
	// across the whole backup set.
	MaxPerCycle int `yaml:"max_per_cycle"`

	// Hash, when true, recomputes the local final file's SHA-256 and
	// compares it against the hash recorded at VERIFIED (FR-13). An
	// artifact that was originally verified without hash: sha256 has
	// nothing recorded to compare a fresh read against; that is a no-op
	// for that one artifact, not a failure.
	Hash bool `yaml:"hash"`

	// Command is an optional restore-test hook: the stronger form of
	// revalidation, proving the artifact still actually restores rather
	// than only that its bytes are unchanged. It reuses exactly the same
	// untrusted-subprocess contract Validation.Command already
	// established for FR-13 (fixed environment, its own process group,
	// bounded captured output, fail-closed on its timeout).
	Command *Command `yaml:"command"`
}

// Retention configures GFS retention (FR-18) and last-known-good protection
// (FR-19). See validate.go for the default each field falls back to when
// left at its YAML zero value, and why those particular defaults were
// chosen rather than the field's literal zero value.
//
// # Global, not per-backup-set (issue #111 decision)
//
// This block is deliberately one policy for the whole Config, applied to
// every backup set through GFSDecide/PruneDecide's shared cfg argument,
// not a field on BackupSet. That is a decision, not an oversight: the
// shared web UI's own BackupSet type (ui/shared/src/types/backup.ts)
// already models a `retention` field per backup set, and its mock
// fixtures (ui/shared/src/api/mock.ts) already give two backup sets
// different override values, which could easily be mistaken for evidence
// that per-set retention is already a real, working capability. It is
// not: nothing in this package or internal/retention has ever supported
// a per-backup-set override, and issue #111 keeps it that way rather than
// letting the UI's already-drawn shape settle the question by accident.
// Introducing real per-set overrides is a legitimate future capability,
// but it is a separate, larger change (new schema, new validation, a new
// resolution order between set-level and global values) that deserves its
// own issue rather than riding in on a config/CLI-first change.
type Retention struct {
	Timezone             string `yaml:"timezone"`
	WeekStartsOn         string `yaml:"week_starts_on"`
	DailyDays            int    `yaml:"daily_days"`
	WeeklyMonths         int    `yaml:"weekly_months"`
	MonthlyMonths        int    `yaml:"monthly_months"`
	ProtectLastKnownGood *bool  `yaml:"protect_last_known_good"`
}

// Load reads and parses the YAML file at path. It does not validate the
// result: call Validate, or use LoadAndValidate, before acting on it.
//
// Unknown keys are a parse error, not a silently ignored field: a typo like
// "pol_interval" should be reported as exactly that, not surface later as a
// mysteriously-zero poll_interval.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parsing config %q: file is empty", path)
		}
		return nil, fmt.Errorf("parsing config %q: %w", path, err)
	}
	return &cfg, nil
}

// LoadAndValidate reads, parses and validates the config in one call. Most
// callers want this. Load and Validate stay separate as their own exported
// steps for callers that need to inspect or adjust a config in between,
// tests chief among them.
func LoadAndValidate(path string) (*Config, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

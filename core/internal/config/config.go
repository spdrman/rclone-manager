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
	"path/filepath"
	"strings"
	"time"

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
	Alerts       Alerts    `yaml:"alerts"`
	Capacity     Capacity  `yaml:"capacity,omitempty"`
}

// Capacity is FR-21's configuration: how much space this manager is allowed
// to occupy, and the two levels at which it should say something about it
// (issue #286).
//
// # Why this block exists at all, and why it exists all at once
//
// internal/capacity has enforced FR-21 since it was written, but with
// caller-supplied numbers nothing outside a test ever supplied: apps/common
// /webhost's storage route said, in so many words, that its warning and
// critical figures were "structurally zero until internal/config grows
// capacity fields". This is that block. The cap an operator asked for and
// the thresholds the guard has always wanted are the same missing
// configuration, so they land together rather than the second set arriving
// as a near-identical block a release later.
//
// # Bytes, always
//
// Every number here is a byte count. A units picker belongs to the form an
// operator types into, not to the file: a config that could carry either
// "100" meaning megabytes or "100" meaning gigabytes is a config whose
// meaning depends on a second field, and getting that pair out of step is
// a two-order-of-magnitude mistake nobody would see. The Settings UI shows
// MB/GB and converts at the edge; what lands here is bytes.
type Capacity struct {
	// CapBytes is the ceiling on how much space this manager may occupy on
	// the filesystem underneath BackupRoot.
	//
	// ZERO MEANS NO CAP: use the whole volume, and report the reading
	// against the volume. That is this product's default, and it is a
	// sentinel rather than a literal, so nothing anywhere may resolve it
	// to a number or read it as "a ceiling of zero bytes, refuse
	// everything". Completion.DeleteSafetyDelay resolves ITS zero to a
	// documented default for the opposite reason: reading that zero
	// literally would turn a safety gate off, whereas reading this one
	// literally would turn the product off. Validate refuses a negative
	// value and says, in the refusal, that 0 is how an operator asks for
	// no cap.
	//
	// The cap is enforced, not merely displayed. internal/capacity weighs
	// it against how much this manager is already using and refuses a
	// transfer that would exceed it, exactly as it already refuses one the
	// disk cannot hold; see that package's "Two different questions"
	// section for why the refusal fires on whichever of the two is
	// smaller.
	CapBytes int64 `yaml:"cap_bytes,omitempty"`

	// WarningFreeBytes and CriticalFreeBytes are FR-21's two levels,
	// measured against whatever headroom actually binds: the disk's free
	// space with no cap set, and the cap's remaining allowance with one.
	//
	// Zero means "no line here", which is the default and is what every
	// deployment has effectively been running with. A warning line below
	// the critical floor cannot be honoured (internal/capacity.Thresholds
	// .Validate refuses it, and the level would jump straight from OK to
	// CRITICAL), so Validate refuses that pair at load rather than letting
	// every storage reading come back "misconfigured" with nothing naming
	// the two numbers responsible. That rule also means a critical floor
	// with no warning line is refused: state both, or neither.
	WarningFreeBytes  int64 `yaml:"warning_free_bytes,omitempty"`
	CriticalFreeBytes int64 `yaml:"critical_free_bytes,omitempty"`

	// SafetyMarginBytes is held back on top of every incoming artifact's
	// own size before a transfer is admitted. See internal/capacity's
	// headroom-arithmetic section for what it is meant to cover (listing
	// drift, block rounding, other writers on the same volume) and for why
	// it is deliberately a plain byte count this product does not try to
	// compute on an operator's behalf.
	SafetyMarginBytes int64 `yaml:"safety_margin_bytes,omitempty"`

	// BackupRoot names the one directory whose filesystem the manager-wide
	// storage reading is taken from.
	//
	// Leave it unset and it is derived: see EffectiveBackupRoot, which
	// takes the directory every configured backup set's local_path has in
	// common. That covers the deployment this product actually ships (one
	// bind-mounted backup volume, every set beneath it) with no operator
	// input at all. Set it when the derivation cannot answer honestly,
	// which is when backup sets sit on genuinely different volumes.
	//
	// It exists as a field rather than as pure derivation because of the
	// container-versus-host trap: the engine runs in a container, and the
	// filesystem to measure is the one the backup root is on AS THE
	// CONTAINER SEES IT, never the container's own root filesystem and
	// never the host's "/". A derivation that fell back to "/" would
	// report a confident number about the wrong disk, which is worse than
	// reporting nothing because nobody would notice; so the derivation
	// gives up instead, and this field is how an operator answers when it
	// does.
	BackupRoot string `yaml:"backup_root,omitempty"`
}

// EffectiveBackupRoot is the directory whose filesystem a manager-wide
// storage reading should be taken from, or "" when this configuration does
// not know.
//
// An empty answer is a real answer and must be rendered as "capacity is not
// known yet", never as zero bytes and never as a percentage. There are two
// ways to get one: a configuration with no backup sets at all, and one
// whose sets share no ancestor but the filesystem root (see
// Capacity.BackupRoot for why "/" is refused rather than returned).
//
// Purely derived, never written back into the Config. Resolving it in place
// during Validate would freeze today's derivation into an operator's file
// the next time anything re-marshals it, so a later release that learned to
// answer better would be ignored on exactly the deployments that never
// chose a root.
func (c *Config) EffectiveBackupRoot() string {
	if c.Capacity.BackupRoot != "" {
		return c.Capacity.BackupRoot
	}

	root := ""
	for _, src := range c.Sources {
		for _, bs := range src.BackupSets {
			if bs.LocalPath == "" {
				continue
			}
			// A disabled backup set still occupies its destination, and
			// still shares the volume this reading is about, so it counts
			// towards the root exactly like an enabled one.
			if root == "" {
				root = filepath.Clean(bs.LocalPath)
				continue
			}
			root = commonAncestorDir(root, filepath.Clean(bs.LocalPath))
		}
	}
	if root == "/" {
		return ""
	}
	return root
}

// commonAncestorDir is the deepest directory a and b are both inside,
// comparing whole path segments rather than characters: "/data/backups" and
// "/data/backups-old" share "/data", never "/data/backups", and a prefix
// comparison that missed that would measure a sibling volume.
func commonAncestorDir(a, b string) string {
	as := strings.Split(a, "/")
	bs := strings.Split(b, "/")
	n := len(as)
	if len(bs) < n {
		n = len(bs)
	}
	shared := 0
	for shared < n && as[shared] == bs[shared] {
		shared++
	}
	if shared <= 1 {
		// Only the empty leading segment matched, so the two share
		// nothing but "/".
		return "/"
	}
	return strings.Join(as[:shared], "/")
}

// DefaultRepeatedFailureThreshold is how many artifacts have to be
// sitting in FAILED for one backup set before Alerts calls that "repeated
// failure" (docs/EPIC-B-multi-nas.md §71). Three is a deliberate middle:
// one failed artifact is routine (the retry policy exists precisely
// because transfers fail), while waiting for a large number would mean
// the notification arrives long after an operator could still have done
// something about it.
const DefaultRepeatedFailureThreshold = 3

// Alerts configures Work Package 3.5's proactive notification path
// (docs/EPIC-B-multi-nas.md §71): stale backups, repeated failures, a
// changed SSH host key and critical storage pressure, delivered through
// the platform's own local notification capability.
//
// # Why this is opt-in, and why that is one bool
//
// §71 asks for "one explicit opt-in ... mechanism", and this block is
// where that opt-in is spelled. The zero value (Enabled false) is off, so
// a configuration written before this block existed, or one that simply
// leaves it out, does not start notifying anybody after an upgrade. There
// is deliberately no per-condition on/off switch and no channel
// selection: §71 also says "do not add a broad notification framework in
// v1", and a matrix of toggles here is exactly how that starts. An
// operator either wants to be told about the four conditions this
// product considers alert-worthy, or does not.
//
// Where the alert actually goes is not configured here at all. The
// delivery mechanism is the platform's own notifier, supplied by the
// provider app at the apps/ layer (core/ cannot import apps/, §7.1), so
// there is nothing platform-shaped in this file for an operator to get
// wrong, and no URL, command or credential for this package to have to
// validate or redact.
type Alerts struct {
	// Enabled is the explicit opt-in. False, including by omission, means
	// no proactive alert is ever delivered.
	Enabled bool `yaml:"enabled"`

	// RepeatedFailureThreshold is how many artifacts must currently be in
	// FAILED for one backup set before that counts as §71's "repeated
	// failure". Validate fills in DefaultRepeatedFailureThreshold when
	// this is left at zero; see there for why a literal zero is never
	// read as "alert on the first failure".
	//
	// This threshold governs the accumulated-failures arm only. A backup
	// set internal/health places in its FAILING state (an irrecoverable
	// QUARANTINED_LOST artifact, or a FAILED artifact with no retry
	// scheduled) alerts regardless of this number, because that state
	// means a human is needed now: see internal/alert's
	// BackupSetConditions.
	RepeatedFailureThreshold int `yaml:"repeated_failure_threshold"`
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

	// Sensitive marks this remote's endpoint identity, its Host, Port and
	// User, as something that must never reach a log line or a journal
	// detail (issue #295). It is opt-in and defaults to false: whether a
	// port is a credential is a deployment's decision, not a universal
	// fact (issue #264 states the rule for the one deployment that needs
	// it), and a log line that omits "connection refused to WHAT" is a
	// worse default for every deployment that hasn't made that call. It
	// is set per remote, not globally, because two backup sets in the
	// same config file are free to differ (a public mirror beside an
	// internal host only reachable on the operator's own network).
	//
	// internal/app.New reads this across every configured Source's
	// BackupSets at startup (and again on every hot config reload) and
	// builds the obs.Redactor both the process logger and the state
	// journal filter every rendered error message and journal detail
	// through before either one is written down. Nothing here changes
	// what this manager does, only what its own observability output is
	// allowed to contain.
	Sensitive bool `yaml:"sensitive_endpoint,omitempty"`
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
	// used when Strategy == "stable". "stable" only ever confirms a
	// size/mtime heuristic, never a producer completion signal the way
	// "rename"/"marker" do, so FR-15's remote-delete gate
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
	//
	// A zero value is not read literally. Validate resolves it to
	// DefaultDeleteSafetyDelay, the same way validateRetention resolves a
	// zero tier to its documented default: this key did not exist before
	// WP3.2, so every config file written against an earlier release
	// omits it, and reading the omission as "no delay is required" would
	// silently turn the gate off on exactly the deployments that never
	// got the chance to opt in. Only a negative value is refused.
	DeleteSafetyDelay Duration `yaml:"delete_safety_delay"`

	// ManifestMarker is the directory-level completion marker filename the
	// "marker" strategy looks for (issue #291), only used when
	// Strategy == "marker". Before this field existed, that filename was
	// the fixed literal "_SUCCESS" (borrowed from the well-known
	// Hadoop/Spark convention), a choice internal/discovery/complete.go's
	// package doc used to call out as deliberate rather than an oversight.
	// It stopped being defensible the moment a real read-only producer
	// showed up with its own completion signal under a different name
	// (SHA256SUMS, written last, after every artifact) that this manager
	// has no ability to rename to match: the producer cannot be
	// reconfigured, so the manager has to be able to recognise the name
	// the producer already uses.
	//
	// A zero value is not read literally, the same way DeleteSafetyDelay's
	// isn't: Validate resolves it to DefaultManifestMarker, so every config
	// written before this field existed keeps recognising exactly the
	// marker it recognised before. The resolved name is validated as a
	// bare filename, on the same terms Include patterns are: no path
	// separator, no "."/".." traversal. It is a single literal name, never
	// a pattern; the sibling per-artifact marker convention (markerSuffix,
	// "<artifact>.complete") is unrelated to this field and stays fixed.
	ManifestMarker string `yaml:"manifest_marker"`
}

// DefaultDeleteSafetyDelay is the Completion.DeleteSafetyDelay that
// Validate fills in when a "stable" backup set does not set one.
//
// One hour is picked to be longer than any plausible gap between a
// producer finishing a write and the size/mtime heuristic noticing, while
// still short enough that a daily archive reclaims its remote space on the
// same day it was captured. It is deliberately much larger than the
// stable_for values this project documents (minutes), because the two
// answer different questions: stable_for gates starting work on an
// artifact, this gates destroying the only other copy of it.
const DefaultDeleteSafetyDelay = time.Hour

// DefaultManifestMarker is the Completion.ManifestMarker that Validate
// fills in when a "marker" backup set does not set one. It is the name
// internal/discovery/complete.go recognised unconditionally before this
// field existed, so an unset manifest_marker leaves every existing
// configuration behaving exactly as it did before (issue #291).
const DefaultManifestMarker = "_SUCCESS"

// Validation configures how a transferred artifact gets checked before it's
// allowed to be treated as a good restore point (FR-13).
type Validation struct {
	Hash string `yaml:"hash"` // "" or "sha256"

	// ValidatorID names one entry in core/service's registered
	// application-validator catalog (docs/EPIC-B-multi-nas.md §26 Step 5,
	// issues #99/#162). It is the ONLY way the API/UI layer can select an
	// application validator: that layer never names an executable, and
	// core/service is the only thing that turns an id back into a
	// Command.
	//
	// This package deliberately does not know the catalog -- core/service
	// imports this package, not the other way around -- so nothing here
	// decides whether a given id is registered. An unregistered id is
	// refused at load time by core/service, which fails startup rather
	// than quietly running a backup set with no validator the operator
	// believes is configured. What validate.go does check is the shape:
	// one bare token, and never alongside a Command.
	//
	// Storing the id, rather than the executable path it resolves to, is
	// the whole point. That path is materialized per deployment and can
	// move between releases; a config.yaml holding a stale one would fail
	// every artifact in the backup set after a restart, with an error
	// naming a directory instead of the cause.
	ValidatorID string `yaml:"validator_id"`

	Command *Command `yaml:"command"`
}

// ErrValidatorNotResolved reports a Validation that names a ValidatorID
// nothing ever turned into a runnable Command. See ResolvedCommand.
var ErrValidatorNotResolved = errors.New("config: the configured application validator was never resolved to a runnable command")

// ResolvedCommand is how every consumer of a Validation must ask for its
// application validator, rather than reading the Command field directly.
// It returns nil, nil for "no validator configured", the command for one
// that was resolved, and ErrValidatorNotResolved for the one combination
// that must never be read as either: a ValidatorID with no Command.
//
// core/service resolves every id at load time and refuses to start on one
// it does not recognize, so a nil Command beside a non-empty id means some
// path skipped resolution entirely -- and the one thing that must never
// do is read as "no validator was configured, carry on". The operator
// asked for one; transferring and then deleting the remote source without
// it is exactly the outcome FR-13 exists to prevent.
//
// The rule lives here, where a Validation is interpreted, instead of at
// each place one is consumed, because it was previously enforced in
// internal/lifecycle's verify path and not in internal/app's
// operator-triggered revalidation path, which reported an artifact as
// passing without ever running the validator its backup set names. An
// invariant that has to be re-implemented per consumer gets missed by
// exactly one consumer.
func (v Validation) ResolvedCommand() (*Command, error) {
	if v.Command == nil {
		if v.ValidatorID != "" {
			return nil, fmt.Errorf("%w: %q", ErrValidatorNotResolved, v.ValidatorID)
		}
		return nil, nil
	}
	return v.Command, nil
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
	Timezone     string `yaml:"timezone"`
	WeekStartsOn string `yaml:"week_starts_on"`

	// DailyDays, WeeklyMonths and MonthlyMonths are the original
	// three-scalar spelling of FR-18's default chain, kept as sugar for
	// exactly the three-entry Tiers list DefaultTierChain builds. They and
	// Tiers are mutually exclusive: see Tiers' own doc, and validate.go's
	// validateRetention, for why that is an error rather than a silent
	// precedence rule.
	//
	// All three carry omitempty because this config file is not only read
	// by the product, it is rewritten by it: core/service's
	// writeConfigAtomically re-marshals the whole Config on every wizard
	// save. Without omitempty a tiers-based policy comes back from that
	// round trip with "daily_days: 0" sitting above the operator's chain,
	// which reads as "daily retention is off" and invites the one edit
	// (setting it to 7) that Validate refuses outright, leaving a daemon
	// that will not start. Zero already means "not configured" for all
	// three, and validateRetention resolves them to 7/3/12 before any
	// write, so nothing is lost by omitting them.
	DailyDays     int `yaml:"daily_days,omitempty"`
	WeeklyMonths  int `yaml:"weekly_months,omitempty"`
	MonthlyMonths int `yaml:"monthly_months,omitempty"`

	// Tiers is FR-18's generalized retention chain: an ordered list of any
	// number of named tiers, each bucketing artifacts at its own
	// granularity over its own look-back window. KEEP is the union of
	// every tier's selections (plus FR-19's protected term); anything the
	// union does not claim, whether it fell in a gap between two tiers'
	// windows or outside every window, is a delete candidate.
	//
	// An empty Tiers means "use the three scalars above", which is what
	// makes an existing config file that never heard of this field keep
	// producing exactly the decisions it produced before the field
	// existed. Setting both is refused by Validate: an operator who wrote
	// both is asking two different questions, and picking one silently is
	// how a retention policy ends up deleting on terms nobody wrote.
	//
	// An explicitly empty list (tiers: []) is not distinguishable from an
	// absent key here and reads the same way: the three scalars above,
	// which resolve to 7/3/12. So emptying the chain does not spell "keep
	// nothing", it reinstates the default daily/weekly/monthly policy.
	// That is deliberately the fail-safe direction (more retention, not
	// less), and it is why the keep validation message points at dropping
	// one tier rather than at emptying the list. There is no "keep
	// nothing" spelling in this schema at all: retention is turned off by
	// not running a retention pass.
	//
	// Order is the order the operator wrote, and is preserved end to end
	// (it fixes the order tier names appear in a KEEP verdict). Because
	// KEEP is a union, order never changes which artifacts are kept.
	//
	// omitempty for the same round-trip reason as the three scalars
	// above: a legacy config file must not come back from a wizard save
	// with an empty "tiers: []" injected into it, which an older binary
	// then rejects outright under Load's KnownFields(true).
	Tiers []RetentionTier `yaml:"tiers,omitempty"`

	ProtectLastKnownGood *bool `yaml:"protect_last_known_good"`
}

// Retention granularity names. These are the values RetentionTier's
// Granularity and WindowUnit fields accept, spelled exactly as they are
// written in YAML.
//
// GranularityDays is the "any other period" escape hatch FR-18 calls for:
// paired with PeriodDays it expresses a fortnightly, ten-daily or any
// other fixed-length chain step the named list above does not cover. Its
// buckets are anchored to a fixed epoch rather than to today, so a custom
// period's bucket boundaries never move depending on the day the
// calculation happens to run (see internal/retention's own doc).
const (
	GranularityDay      = "day"
	GranularityWeek     = "week"
	GranularityMonth    = "month"
	GranularityQuarter  = "quarter"
	GranularityHalfYear = "half_year"
	GranularityYear     = "year"
	GranularityDays     = "days"
)

// TierLastKnownGoodName is reserved: FR-19's protected term already
// occupies the LAST_KNOWN_GOOD name on the wire (internal/retention's
// TierLastKnownGood), so a configured tier may not claim it and make a
// GFS selection indistinguishable from last-known-good protection in a
// verdict's tier list.
const TierLastKnownGoodName = "last_known_good"

// RetentionTier is one link in FR-18's retention chain.
//
// The shape is deliberately flat and enumerated rather than free-form: a
// settings form (B3.7) renders it as two selects and two numbers, and can
// validate every field client-side against the same closed value sets
// config.Validate checks server-side.
type RetentionTier struct {
	// Name identifies the tier and is what surfaces in a KEEP verdict.
	// It must be lower_snake_case (^[a-z][a-z0-9_]*$) and unique within
	// the chain; internal/retention upper-cases it for the wire, so
	// "daily" is reported as "DAILY" exactly as it always has been.
	Name string `yaml:"name"`

	// Granularity is the calendar bucket this tier groups artifacts into:
	// one of the Granularity* constants above. Each bucket contributes at
	// most one artifact to KEEP, the newest valid one in it.
	Granularity string `yaml:"granularity"`

	// PeriodDays is the length of a custom period, in days. It is
	// required when Granularity is GranularityDays and must be zero
	// otherwise, hence omitempty: the schema doc says period_days is
	// absent from every other tier, and a config file this product wrote
	// should read the way the doc says one looks.
	PeriodDays int `yaml:"period_days,omitempty"`

	// Keep is how many of this tier's look-back units to reach back over,
	// counting the current one. Keep: 7 on a day-granularity tier is
	// today plus the six days before it.
	Keep int `yaml:"keep"`

	// WindowUnit optionally measures the look-back in a unit other than
	// Granularity. It accepts every Granularity* constant except
	// GranularityDays, and defaults to Granularity when empty.
	//
	// This exists because a tier's window is not always counted in its own
	// buckets, and specifically because FR-18's default weekly tier is not:
	// weekly_months buckets by week but looks back over calendar months.
	// Without this field that default could not be expressed in the new
	// schema, and "the old keys keep producing identical decisions" would
	// be false for one tier out of three.
	//
	// omitempty for the same reason as PeriodDays: empty means "defaults
	// to Granularity", which is the ordinary case, so a written-back
	// config should not carry a window_unit: "" on every tier.
	WindowUnit string `yaml:"window_unit,omitempty"`
}

// DefaultTierChain returns the three-tier chain the DailyDays,
// WeeklyMonths and MonthlyMonths scalars are sugar for.
//
// This is the single definition of that expansion. internal/retention
// resolves an empty Retention.Tiers through it, and validate.go checks
// the scalars that feed it, so there is no second, potentially divergent,
// spelling of "what do the three old keys mean" anywhere in the tree.
//
// The values are passed in rather than read from a Retention so this can
// be called on an unvalidated policy without implying it has been
// defaulted: a non-positive window yields a tier internal/retention treats
// as disabled, which is exactly how the scalars have always behaved for a
// caller that bypassed Validate.
func DefaultTierChain(dailyDays, weeklyMonths, monthlyMonths int) []RetentionTier {
	return []RetentionTier{
		{Name: "daily", Granularity: GranularityDay, Keep: dailyDays},
		{Name: "weekly", Granularity: GranularityWeek, Keep: weeklyMonths, WindowUnit: GranularityMonth},
		{Name: "monthly", Granularity: GranularityMonth, Keep: monthlyMonths},
	}
}

// DefaultDailyDays, DefaultWeeklyMonths and DefaultMonthlyMonths are
// FR-18's documented retention windows, the values validateRetention fills
// in when a config sets neither an explicit chain nor the legacy scalar
// (validate.go reads them from here rather than spelling the numbers a
// second time). They are exported because "what IS the default chain" is a
// question asked outside this package too: core/service serves it in the
// settings schema so the Web UI's "Restore default chain" button fills its
// form from the product's actual default rather than from a copy of these
// numbers transcribed into a frontend, where nothing would notice it going
// stale. A narrowed window arrived at that way is a data-loss-shaped
// drift.
const (
	DefaultDailyDays     = 7
	DefaultWeeklyMonths  = 3
	DefaultMonthlyMonths = 12
)

// DefaultRetentionTiers is the chain a Retention that configures neither
// spelling resolves to: DefaultTierChain over the three defaults above.
// One function, so there is exactly one answer to "what does this product
// keep by default" for validate.go, for the settings schema, and for
// anything that comes later.
func DefaultRetentionTiers() []RetentionTier {
	return DefaultTierChain(DefaultDailyDays, DefaultWeeklyMonths, DefaultMonthlyMonths)
}

// EffectiveTiers returns the tier chain this policy actually decides
// with: the explicit Tiers list when one is configured, and otherwise the
// DefaultTierChain expansion of the three scalars.
//
// Validate refuses a Retention that sets both, so on any validated policy
// exactly one of the two branches is meaningful. The branch order still
// favours an explicit list, so a caller that reached here with a
// hand-built, unvalidated Retention gets the more specific of the two
// rather than a silent fall-back to scalars it never set.
func (r Retention) EffectiveTiers() []RetentionTier {
	if len(r.Tiers) > 0 {
		return r.Tiers
	}
	return DefaultTierChain(r.DailyDays, r.WeeklyMonths, r.MonthlyMonths)
}

// DefaultFileName is the configuration file's name inside the
// configuration DIRECTORY. Packaging mounts the directory, not the file
// (distribution/packaging/canonical.json's config role, issue #196),
// because the engine creates and atomically replaces this file and keeps
// two on-demand stores beside it, and because a directory is the only
// shape that can honestly be empty on a fresh install.
const DefaultFileName = "config.yaml"

// ResolvePath turns a configuration path an operator supplied into the
// file to read or write. A path naming an existing DIRECTORY resolves to
// DefaultFileName inside it; anything else is returned unchanged.
//
// It exists because #196 made the packaged mount a directory, so
// `--config /etc/backup-manager/config` is now the natural thing for an
// operator to type. Without this, that spelling fails with "is a
// directory" from deep inside the YAML reader, which says nothing about
// what to do instead.
//
// A path that does not exist is returned unchanged rather than guessed
// at: the caller is about to create it, and turning a not-yet-existing
// file path into a directory path would create the file in the wrong
// place.
func ResolvePath(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return path
	}
	return filepath.Join(path, DefaultFileName)
}

// ExplainConfigMountShape names the one failure an operator cannot read out
// of the error the filesystem gives them, and returns "" for every other.
//
// #196 turned the configuration mount from a read-only FILE into a writable
// DIRECTORY. Two platforms carry an operator's old answer forward across an
// upgrade rather than re-asking for it: TrueNAS stores an installed
// application's answers, and Unraid keeps the mappings already in the user's
// own template copy. On both, an upgrade can end up bind-mounting the old
// config.yaml FILE at the new directory path. The engine then resolves
// --config to <that file>/config.yaml, which is ENOTDIR, and crash-loops on
// "reading config ...: not a directory" — a message that names neither the
// mount nor the migration, on a deployment that was working ten minutes ago.
//
// So the check is on the shape rather than on the errno: the parent of the
// configuration file exists and is not a directory. That is only ever a mount
// mistake, it is decidable without guessing at platform-specific error text,
// and it is the one case where a reader needs to be told about an issue
// number rather than about a path.
func ExplainConfigMountShape(configFile string) string {
	dir := filepath.Dir(configFile)
	info, err := os.Stat(dir)
	if err != nil || info.IsDir() {
		return ""
	}
	return fmt.Sprintf("%s is a file, not a directory: issue #196 made the configuration mount a writable DIRECTORY holding %s, so a deployment upgraded from a mount of the config FILE has to point that mount at the directory instead. See docs/runtime-contract.md", dir, DefaultFileName)
}

// Load reads and parses the YAML file at path. It does not validate the
// result: call Validate, or use LoadAndValidate, before acting on it.
//
// Unknown keys are a parse error, not a silently ignored field: a typo like
// "pol_interval" should be reported as exactly that, not surface later as a
// mysteriously-zero poll_interval.
func Load(path string) (*Config, error) {
	path = ResolvePath(path)
	data, err := os.ReadFile(path)
	if err != nil {
		if hint := ExplainConfigMountShape(path); hint != "" {
			return nil, fmt.Errorf("reading config %q: %w (%s)", path, err, hint)
		}
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

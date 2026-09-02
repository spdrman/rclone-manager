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

	// StorageMediums declares the non-local destinations an artifact's
	// durable copy may live on (EPIC E, FR-27). It sits at the top level
	// rather than inside Retention because a medium is a place, not a
	// policy: a tier NAMES one, and a medium no tier names yet is a
	// legal, staged configuration.
	//
	// omitempty for the round-trip reason Retention.Tiers' own doc spells
	// out at length: core/service's writeConfigAtomically re-marshals the
	// whole Config on every settings save, and a config file that never
	// heard of mediums must not come back from that with a
	// "storage_mediums: []" injected into it, which an older binary then
	// refuses outright under Load's KnownFields(true). FR-35 makes that a
	// gate rather than an intention.
	StorageMediums []StorageMedium `yaml:"storage_mediums,omitempty"`

	Alerts        Alerts        `yaml:"alerts"`
	Capacity      Capacity      `yaml:"capacity,omitempty"`
	KeyEncryption KeyEncryption `yaml:"key_encryption,omitempty"`
}

// KeyEncryption names an optional, config-wide way to obtain the key
// (#298) that protects secret material this manager persists to disk
// under configPath's sibling directories -- today, an imported SSH
// private key under keysDirIn's ssh_keys/ (core/service/backupsets.go);
// a future medium credential (EPIC E's S3 keys, #235) is expected to
// reuse this same field rather than inventing its own, since the
// question ("how is a secret this program itself must write to disk
// protected at rest") does not change with the kind of secret.
//
// It mirrors Key and Passphrase's own File/Env/Command shape exactly
// (three ways to name where a secret comes from, never a field to paste
// one into directly), for the same reason those two do: a bare
// "key_encryption: <value>" field an operator could type directly into
// YAML would be one more credential sitting in the clear next to
// everything else in this file, the exact problem #298 exists to close
// for the key it protects.
//
// It is entirely optional. The zero value (File, Env and Command all
// empty) means "no encryption key is configured", which is every
// config.yaml written before this field existed and every one written
// since that simply does not opt in: an imported key is then persisted
// exactly as before #298, a plaintext file protected only by filesystem
// permissions (#293). That is a deliberate, documented trade, not a
// silent downgrade: see docs/ssh-setup.md's "Encrypting the key store at
// rest" section for the threat this does and does not cover, and in
// particular why the resolved key here MUST live outside whatever
// directory tree an SMB/AFP share exports, or protecting the key file
// buys nothing at all against the exact exposure #298 was filed over.
type KeyEncryption struct {
	// File points at a file holding the encryption key, read directly by
	// this process at the point a secret file is written or first
	// decrypted -- there is no rclone-shaped backend here for this to
	// hand a path to instead, unlike Key.File, so this file's content
	// necessarily enters this process's memory. See the type doc above
	// for why that is an accepted, documented cost rather than an
	// oversight.
	File string `yaml:"file"`

	// Env names an environment variable this process reads the
	// encryption key from.
	Env string `yaml:"env"`

	// Command is an argv array: Command[0] is the executable, invoked
	// directly (never through a shell), and the rest are its arguments.
	// Its stdout is treated as the encryption key. Mirrors Key.Command
	// and Passphrase.Command exactly, for the identical reason: this is
	// the path a secrets manager (OpenBao, Vault, SOPS, 1Password, AWS
	// Secrets Manager, ...) adopts without this project taking a
	// dependency on any of their SDKs.
	Command []string `yaml:"command"`
}

// isZero reports whether none of KeyEncryption's three sources are set:
// "no encryption key is configured", the case for every config that
// predates #298 and for every one since that has not opted in.
func (e KeyEncryption) isZero() bool {
	return e.File == "" && e.Env == "" && len(e.Command) == 0
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

	// ReadOnly is issue #282's source-level default for every backup set
	// declared under this source: "pull from here, never delete here".
	// It exists at this level, and not only per set, because read-only-ness
	// is normally a property of the whole host (a production machine an
	// engagement is only allowed to read from), not of one particular
	// backup set on it; grouping sources the way these deployments actually
	// are means declaring it once here covers every set under it without
	// repeating the flag per set.
	//
	// A set's own BackupSet.ReadOnlyConfig, when set, overrides this
	// default; see that field's doc for the full precedence rule and
	// Validate for where the two are resolved into BackupSet.ReadOnly.
	ReadOnly bool `yaml:"read_only,omitempty"`
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

	// ReadOnlyConfig is issue #282's per-set override of the parent
	// Source's ReadOnly default: "pull from here, never delete here",
	// declared for this one backup set specifically. It is a pointer, not
	// a plain bool, because the three-way choice it has to express -- "not
	// set here, inherit the source's default", "explicitly read-only
	// regardless of the source default", "explicitly NOT read-only
	// regardless of the source default" -- has no honest two-value
	// encoding: a plain bool's zero value (false) could never be told
	// apart from an operator who typed `read_only: false` on purpose to
	// carve one set out of an otherwise read-only source.
	//
	// This field is never read directly outside Validate. Every other
	// consumer in this codebase reads the resolved ReadOnly field below,
	// exactly as every consumer of a backup set's identity reads ID rather
	// than reassembling it from Name and a parent Source, so "did this
	// come from Validate yet" is never a question call sites have to ask
	// twice.
	ReadOnlyConfig *bool `yaml:"read_only,omitempty"`

	// RetentionConfig is issue #333's per-set override of the top-level
	// Retention policy: "retain THIS set on this chain, whatever the rest
	// of the deployment does". It is a pointer for the same reason
	// ReadOnlyConfig is, and the reason is sharper here: Retention's zero
	// value is not a policy at all, so a plain struct could never tell an
	// operator who wrote no retention block apart from one who wrote an
	// empty one, and validateRetention would resolve the second into the
	// documented 7/3/12 default chain rather than into inheritance. Nil
	// means inherit; non-nil means this set decides for itself.
	//
	// The override is whole-policy, never a field-by-field merge with the
	// global one. Merging would produce a chain nobody wrote and nobody
	// could predict from reading either half, which is the same reasoning
	// validateRetention already applies when it refuses a config that sets
	// both the tiers list and the legacy scalars.
	//
	// An override has to name a WHOLE chain: a tiers list, or all three
	// of daily_days, weekly_months and monthly_months. Naming two of the
	// three would resolve the third to the product default rather than to
	// the deployment's policy, which is how a set silently ends up
	// retaining less than the operator who wrote the deployment's policy
	// believes. resolveBackupSetRetention's doc has the whole rule,
	// including what an omitted timezone inherits and why.
	//
	// Like ReadOnlyConfig, this field is never read directly outside
	// Validate. Every other consumer reads the resolved Retention field
	// below.
	//
	// Writing one is a one-way door for a deployment that might roll back:
	// Load's KnownFields(true) makes any key it does not know a parse
	// error, so a config file carrying a set-level retention block cannot
	// be read at all by a build from before this field existed. The same
	// is true of every key this schema has ever gained, which is why
	// nothing here is emitted unless it was written (omitempty), but this
	// one is worth saying out loud because retention is the surface an
	// operator is most likely to reach for during an incident, which is
	// also when a rollback is most likely.
	RetentionConfig *Retention `yaml:"retention,omitempty"`

	// Retention is the fully-resolved policy this backup set is actually
	// retained under, filled in by Validate from RetentionConfig when it
	// is set and from the Config's own top-level Retention otherwise, on
	// the same before/after-Validate discipline ID and ReadOnly follow. An
	// unvalidated BackupSet reads the zero Retention here regardless of
	// what YAML said.
	//
	// This is a resolved copy rather than a pointer to the global policy
	// on purpose: a later edit to the global Retention must not
	// retroactively change what an already-resolved set was retained
	// under without a Validate pass to make it so.
	//
	// The rule that falls out of that, and the one thing a caller holding
	// a *Config has to remember: any mutation of the top-level Retention
	// has to be followed by Validate (or ResolveBackupSetRetention), or
	// every set goes on deciding under the policy that was in force when
	// it was last resolved. cmd/backup-manager's retention override flags
	// are the live instance of this, and were a silent no-op until they
	// re-resolved.
	Retention Retention `yaml:"-"`

	// ReadOnly is the fully-resolved answer to "may this backup set's
	// remote source ever be deleted", filled in by Validate from
	// ReadOnlyConfig (when set) or the parent Source's ReadOnly default
	// (otherwise) -- the same before/after-Validate discipline ID follows.
	// An unvalidated BackupSet reads false here regardless of what YAML
	// said, same as an unresolved ID reads "".
	//
	// With this true, FR-15's whole delete step never runs for this set:
	// core/internal/app's pipeline calls lifecycle.RetainRemote instead of
	// lifecycle.DeleteRemote, so transport.Transport.DeleteRemote is never
	// invoked, full stop, rather than merely being asked and refused (see
	// internal/lifecycle/remotedelete.go's package doc for why "usually
	// refuses" is not the same promise, and issue #282 for the deployment
	// that made the difference matter). An artifact that reaches
	// COMMITTED under a read-only set is routed to the REMOTE_RETAINED
	// terminal state instead of REMOTE_DELETE_PENDING -> COMPLETE.
	//
	// The default (false, including by omission of both this field and the
	// source's) is unchanged from every configuration written before this
	// field existed: FR-15's delete step runs exactly as it always has.
	// This cannot be inferred from a set's own history -- a source that
	// has never had a successful delete has told this package nothing
	// either way -- so it is only ever set by an explicit yaml key, never
	// derived.
	ReadOnly bool `yaml:"-"`

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

	// MaxConnections caps how many simultaneous SFTP connections ONE
	// OPERATION against this remote may open. Zero, including by omission,
	// means unlimited, which is rclone's own default and is what every
	// config written before this field existed means, so adding it changes
	// no existing deployment.
	//
	// Read "one operation" literally, because the name promises more than
	// the setting delivers and the difference is the kind that bites at
	// three in the morning. rclone hands out connection tokens per Fs, and
	// this manager builds a fresh Fs for every list, stat, copy, hash and
	// delete (see internal/transport/rclone/adapter.go and #355). So this
	// bounds an operation, not a remote: a scheduled cycle and an operator
	// clicking "test connection" in the web UI are two operations, and a
	// host that sees them at the same time sees up to twice this number.
	//
	// It is per remote rather than global because whether a host caps
	// concurrent connections is a fact about that host, not about this
	// manager: a hardened VPS and a NAS on the same LAN are free to differ,
	// the same reasoning sensitive_endpoint above is built on.
	//
	// Setting it is a belt, not the braces. A host that caps connections
	// usually REJECTS the surplus rather than queueing it, so exceeding
	// the cap is not slow, it is a failed backup reported as a bare
	// connection error (issue #264: both production sources reject a third
	// simultaneous connection from one address with a TCP reset). What
	// keeps this manager under such a cap is the adapter opening one
	// connection per operation by construction, which it does whether or
	// not this is set. This is what an operator can point rclone at
	// directly, and it is what still holds if a future rclone or a future
	// backend decides to open more.
	MaxConnections int `yaml:"max_connections,omitempty"`
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

	// Passphrase names exactly one way to obtain the passphrase that
	// decrypts File, Env or Command when the key material they resolve to
	// is itself passphrase-protected (#269). It is optional: the zero
	// value means "this key is not passphrase-protected", which is every
	// config.yaml written before this field existed, so an unencrypted
	// key.file continues to work with nothing here to configure.
	//
	// It offers the same three resolvers (File, Env, Command) that File,
	// Env and Command above already do, for the same reason those exist: a
	// bare passphrase field an operator could type directly in YAML would
	// be one more credential sitting in the clear next to everything else
	// in this file. See the Passphrase type's own doc for why it is a
	// separate type rather than a field of type Key.
	Passphrase Passphrase `yaml:"passphrase"`
}

// isZero reports whether none of Key's three sources are set, so callers
// can tell "no key: block in the YAML at all" apart from a source with an
// empty value in one of its fields.
func (k Key) isZero() bool {
	return k.File == "" && k.Env == "" && len(k.Command) == 0
}

// Passphrase names exactly one way to obtain the passphrase that decrypts a
// passphrase-protected Key (#269): File, Env or Command, mirroring Key's
// own three fields both in name and in meaning. It is a separate type from
// Key, rather than Key growing a field of Key's own type, because Go
// refuses a struct that directly contains itself; the two are otherwise
// identical in shape on purpose, "the same three resolvers" the issue
// itself asked for as the smallest change that keeps Key's existing shape.
//
// Exactly one of File, Env or Command may be set when any is; Validate
// enforces that, mirroring Key's own rule. Unlike Key.File, Passphrase.File
// IS read by this program: rclone's sftp backend has no option of its own
// for "read the passphrase from this file", only key_file_pass, which
// takes the passphrase text itself (internal/transport/rclone/ssh.go). So
// there is no way to keep a passphrase out of this process's memory the
// way Key.File keeps the key material out, and this type does not pretend
// otherwise.
type Passphrase struct {
	// File points at a file holding the passphrase, read directly by this
	// process (see the type doc above for why that differs from Key.File).
	File string `yaml:"file"`

	// Env names an environment variable this process reads the passphrase
	// from at connection time.
	Env string `yaml:"env"`

	// Command is an argv array, run directly and never through a shell,
	// exactly like Key.Command; its stdout is treated as the passphrase.
	Command []string `yaml:"command"`
}

// isZero reports whether none of Passphrase's three sources are set: "this
// key is not passphrase-protected", the case for every config that
// predates #269.
func (p Passphrase) isZero() bool {
	return p.File == "" && p.Env == "" && len(p.Command) == 0
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
	// Order DOES decide where a kept artifact lives, and that is new
	// with EPIC E's mediums (FR-27). The home-medium rule is that the
	// first tier in chain order which currently selects an artifact names
	// the medium that artifact belongs on. Two tiers selecting the same
	// artifact is the common case rather than a corner (daily and monthly
	// both claim the first backup of a month), so with a per-tier Medium
	// the chain's order stops being purely presentational: reordering a
	// chain can change where artifacts are stored even though it still
	// cannot change which ones are kept. Operators write chains
	// fine-to-coarse, so the first selecting tier is the warmest, which
	// is the behaviour the daily-local, monthly-offsite story needs.
	//
	// Nothing acts on that rule yet. This package validates the mapping;
	// the planner that reads it is EPIC E Phase 2 (#239), and the mover
	// it feeds is #238. Saying it here rather than there is deliberate:
	// the EPIC's own review rejected an earlier draft precisely for
	// giving chain order a second, load-bearing meaning without saying so
	// in the field's documentation.
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

	// Medium names the storage medium this tier's artifacts live on: the
	// id of one of Config.StorageMediums (EPIC E, FR-27).
	//
	// EMPTY MEANS LOCAL, the backup set's own local_path with exactly
	// today's semantics, and empty is the ONLY spelling of local. A tier
	// writing "medium: local" is refused rather than accepted as a
	// synonym, which is what makes the round-trip rule structural instead
	// of a promise: with local unspellable, the only way a medium: key
	// reaches a config file at all is an operator opting into a real
	// destination, so no settings save can inject one into a file that
	// never asked for it. See Retention.Tiers' own omitempty note for
	// what that injection would cost, and note that an older binary
	// meeting an unknown medium: key fails Load outright.
	//
	// A medium that no storage_mediums entry declares is a validation
	// error, not a fall-back to local. Silently storing artifacts
	// somewhere other than where the operator wrote is the wrong
	// direction on the one decision this field exists to make.
	//
	// The medium is only expressible in this, the tiers spelling. The
	// three legacy daily_days/weekly_months/monthly_months scalars cannot
	// name one and do not need to: adopting mediums means adopting the
	// chain. The CLI's own -tier override cannot name one either (its
	// syntax is name:granularity:keep[:window_unit]), so an override
	// replaces the file's chain with an all-local one. That is inert
	// while nothing reads this field, and it is #239's to answer when
	// retention starts planning on it.
	//
	// omitempty, for the round-trip reason above.
	Medium string `yaml:"medium,omitempty"`
}

// EffectiveMedium is the medium this tier's artifacts belong on: the id it
// names, or MediumLocal when it names none.
//
// This is an accessor rather than a default Validate writes back into the
// struct, and the distinction is the same one Retention.EffectiveTiers
// makes. A resolved value written into the struct would be re-marshaled
// into the operator's own config file by the next settings save, freezing
// today's default into a file that never chose it (issue #294), and here
// it would additionally break FR-35 outright by putting a medium: key on
// every tier of a config that configured no mediums at all.
func (t RetentionTier) EffectiveMedium() string {
	if t.Medium == "" {
		return MediumLocal
	}
	return t.Medium
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

// RetentionIsOverride reports whether this backup set declares its own
// retention policy rather than inheriting the deployment's (issue #333).
//
// It reads the raw RetentionConfig rather than comparing the resolved
// Retention against the global one, because those are different
// questions: a set may legitimately declare a chain identical to the
// global policy, and "the operator wrote this here" is what a preview
// needs to report, not "these two happen to match today".
func (b BackupSet) RetentionIsOverride() bool {
	return b.RetentionConfig != nil
}

// clone returns a Retention that shares no mutable state with the
// receiver, so resolving inheritance by assignment cannot leave a set's
// resolved chain aliased to the global policy's backing array. Assigning
// the struct alone would copy the slice header and leave both pointing at
// the same tiers, where an edit through either would be visible through
// the other.
func (r Retention) clone() Retention {
	out := r
	if r.Tiers != nil {
		out.Tiers = append([]RetentionTier(nil), r.Tiers...)
	}
	if r.ProtectLastKnownGood != nil {
		v := *r.ProtectLastKnownGood
		out.ProtectLastKnownGood = &v
	}
	return out
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

// MediumLocal is the implicit storage medium every deployment already
// has: the backup set's own local_path, with exactly today's semantics.
//
// It is reserved in both directions. A declared medium may not claim this
// id (there would then be two answers to "where is local"), and a tier may
// not name it either (absence is how local is spelled, see
// RetentionTier.Medium). It exists as a named constant because a placement
// record has to say where an artifact IS, and "local" is the answer for
// every artifact in every deployment written before EPIC E.
//
// This is deliberately the same string as artifactstore.KindLocal, which
// names the store that resolves a local placement. config does not import
// artifactstore to say so (this package sits under everything and imports
// nothing of the sort), so the agreement is pinned by a test instead.
const MediumLocal = "local"

// StorageMediumTypeS3 is the one medium type this schema accepts.
//
// The set is closed and grows only by a future FR, because a new backend
// is an architecture decision rather than an import line: EPIC E's FR-28
// is that decision for s3, and it says the implementation is the embedded
// rclone's own s3 backend behind the FR-3 transport boundary, with no AWS
// SDK entering the tree in Go or in TypeScript. A type this package
// accepted without that decision having been made would be a config an
// operator could write and nothing could serve.
const StorageMediumTypeS3 = "s3"

// The closed set of S3 storage classes a medium may ask for (FR-27).
//
// These are spelled exactly as S3 spells them, upper case included, so
// what an operator writes is what reaches the backend rather than
// something this package case-folded on the way past. The archive classes
// are in the set because EPIC E means to support them honestly (FR-34's
// requires_restore access state), not because they behave like the rest:
// GLACIER and DEEP_ARCHIVE objects cannot be read without an explicit
// restore that takes hours, which is #241's subject.
const (
	StorageClassStandard           = "STANDARD"
	StorageClassStandardIA         = "STANDARD_IA"
	StorageClassOneZoneIA          = "ONEZONE_IA"
	StorageClassIntelligentTiering = "INTELLIGENT_TIERING"
	StorageClassGlacierIR          = "GLACIER_IR"
	StorageClassGlacier            = "GLACIER"
	StorageClassDeepArchive        = "DEEP_ARCHIVE"
)

// UploadVerificationReadback and UploadVerificationAttested are the two
// ways a medium may be asked to prove an upload arrived intact before the
// local copy is deleted (FR-31).
//
// Readback downloads the object again and re-hashes it against the hash
// this product recorded when it ingested the artifact. It costs egress and
// it is the DEFAULT, because the alternative asks the destination to grade
// its own work: a hostile or broken endpoint can echo back the checksum it
// was handed at upload without having stored a byte, and the reward for
// believing it is a deleted local copy. EPIC E's security review rejected
// an earlier draft over exactly that.
//
// Attested accepts the provider's own checksum. It is a per-medium opt-in
// that names its trust assumption out loud, for an operator who has
// decided their endpoint is trustworthy and their egress bill is not
// negotiable.
//
// Neither is implemented here. This package decides only what a config may
// say; #235 resolves it and the move engine (#238) acts on it.
const (
	UploadVerificationReadback = "readback"
	UploadVerificationAttested = "attested"
)

// StorageMedium is one declared, non-local destination an artifact's
// durable copy may live on (EPIC E, FR-27).
//
// A medium is a place, and this struct is the whole description of it:
// which backend, which bucket, which key namespace, which storage class,
// how an upload is proven, and where the credentials come FROM. It is not
// a policy. Which artifacts end up here is decided by which retention
// tiers name this medium's id, and by nothing in this struct.
//
// # Nothing reads this yet
//
// This is the schema slice of EPIC E and it lands ahead of everything that
// acts on it, deliberately: #235 turns a medium into a MediumStore over
// the rclone s3 backend, #236 records placements, and Phase 2 moves bytes.
// Until then a configured medium is a declaration of intent that validates
// and does nothing, which is the EPIC's own phasing (Phase 1 builds every
// load-bearing wall and still cannot move or delete anything it could not
// before). It is worth naming because this repository has removed
// decorative config before (#299), and the difference is that these fields
// have a scheduled reader rather than none.
//
// # There is no field for a secret, and there never will be
//
// See MediumCredentials. An access key spelled inline in config.yaml is an
// UNKNOWN field, refused by Load's KnownFields(true) before validation
// runs at all, which is the same custody model Key already applies to the
// SSH private key.
type StorageMedium struct {
	// ID is how a retention tier names this medium. It must be
	// lower_snake_case (StorageMediumIDPattern), unique across the list,
	// and not MediumLocal, which is reserved.
	//
	// The lower_snake_case rule is the one RetentionTier.Name already
	// follows, for the same reason: this id is not decoration, it is
	// reported back to an operator as the place their artifacts live, and
	// one canonical spelling per medium is what lets a settings form
	// validate it client-side against the same rule Validate applies.
	ID string `yaml:"id"`

	// Type names the backend. The only value is StorageMediumTypeS3; see
	// that constant for why the set is closed.
	Type string `yaml:"type"`

	// Region is the provider region, passed through to the backend
	// unexamined.
	//
	// This package does not validate it, and that is a deliberate limit
	// rather than an omission: the set of legal regions belongs to the
	// provider and changes without this product being rebuilt, so a list
	// here would be a second, staler copy that refuses a region that
	// works. The same reasoning validAbsolutePath's doc gives for leaving
	// key_file's expansion to transport applies: this package may not
	// import rclone, so it cannot check what rclone accepts.
	Region string `yaml:"region,omitempty"`

	// Endpoint overrides the provider endpoint, for an S3-compatible
	// service (MinIO, Ceph, Backblaze, a private gateway). Empty means the
	// AWS endpoint for Region.
	//
	// Unvalidated here for Region's reason: what an endpoint may look like
	// is rclone's s3 backend's question, and answering it twice, in two
	// places, with two slightly different ideas of a URL, is how a config
	// gets refused for a spelling that would have worked.
	Endpoint string `yaml:"endpoint,omitempty"`

	// Bucket is the bucket artifacts are written into. Required: a medium
	// with no bucket names no destination at all, and there is no
	// defensible default to invent for one.
	//
	// A bucket name carrying a "/" is refused, because that is one
	// specific mistake worth catching in words an operator can act on:
	// "nas-backups/rclone-manager" is a bucket and a prefix written into
	// one field, and the refusal says so rather than letting the backend
	// report a bucket name it cannot resolve.
	Bucket string `yaml:"bucket"`

	// Prefix is the key namespace inside Bucket, so one bucket can hold
	// more than this product's artifacts. Optional; empty puts the key
	// layout at the root of the bucket.
	//
	// FR-28 fixes that layout as <prefix>/<source>/<set>/<artifact-name>,
	// joined with "/", which is why the shape rules exist: a leading or
	// trailing slash, or a doubled one, produces an empty key segment and
	// two spellings of the same object. A ".." segment is refused as well,
	// even though S3 has no traversal to exploit, because a key is not
	// only ever a key: restoring an artifact writes it to a local path
	// derived from that key, and a namespace that cannot contain ".." is
	// one fewer place for that to go wrong.
	Prefix string `yaml:"prefix,omitempty"`

	// StorageClass is the S3 storage class objects are written with, one
	// of the StorageClass* constants. Empty means StorageClassStandard;
	// resolve it through EffectiveStorageClass rather than reading this
	// field directly.
	StorageClass string `yaml:"storage_class,omitempty"`

	// UploadVerification is how an upload is proven before the local copy
	// is deleted: UploadVerificationReadback (the default) or
	// UploadVerificationAttested. Empty means readback; resolve it through
	// EffectiveUploadVerification.
	//
	// The default is the expensive one on purpose. See the constants.
	UploadVerification string `yaml:"upload_verification,omitempty"`

	// Credentials names where this medium's credentials come from. Exactly
	// one of its three sources must be set.
	Credentials MediumCredentials `yaml:"credentials"`
}

// EffectiveStorageClass is the storage class this medium writes with:
// whatever it names, or StorageClassStandard when it names none.
//
// An accessor rather than a value Validate fills in, for the reason
// RetentionTier.EffectiveMedium's doc gives: a default written back into
// the struct is a default frozen into the operator's file by the next
// settings save.
func (m StorageMedium) EffectiveStorageClass() string {
	if m.StorageClass == "" {
		return StorageClassStandard
	}
	return m.StorageClass
}

// EffectiveUploadVerification is how this medium proves an upload:
// whatever it names, or UploadVerificationReadback when it names none.
//
// The unconfigured answer is the one that reads the object back and
// re-hashes it. A medium that said nothing must never be the medium that
// gets believed on its own word, because what follows a believed upload is
// deleting the local copy.
func (m StorageMedium) EffectiveUploadVerification() string {
	if m.UploadVerification == "" {
		return UploadVerificationReadback
	}
	return m.UploadVerification
}

// MediumCredentials names exactly one way to obtain a storage medium's
// credentials: File, Env or Command (EPIC E, FR-33).
//
// It mirrors Key, Passphrase and KeyEncryption field for field, and that
// is the whole point rather than a coincidence. This project already
// decided how a secret is named in configuration, when the secret was an
// SSH private key: three ways to say where it comes FROM, and no way at
// all to paste one IN. S3 credentials are a bigger prize than an SSH key
// scoped to one hardened account, since they unlock every retained
// artifact on the medium at once, so they get the model that already
// exists rather than a new one invented alongside it.
//
// # There is no field for a literal key, and that is the enforcement
//
// Search this type for access_key_id or secret_access_key; you will not
// find them, on purpose, exactly as Key's own doc says about key_pem. That
// absence is not a style preference, it is the mechanism: Load decodes
// with KnownFields(true), so a config spelling a key inline is a PARSE
// error before Validate is ever reached, and there is no path by which a
// literal secret becomes a value this program holds from the config file.
// A test plants that violation and proves it is refused.
//
// The runtime half of FR-33 is not here: resolving these sources, wrapping
// what comes back in obs.Secret, and proving the resolved value never
// reaches a log, an error, an API response or a manifest is #235's, with a
// canary test. This type is the schema half, and its whole job is that
// there is nothing here to leak.
type MediumCredentials struct {
	// File points at a credentials file on disk, in the AWS
	// shared-credentials format rclone reads itself. This is the preferred
	// source for Key.File's reason, which is stronger here than anywhere
	// else in this schema: rclone opens the file, so the secret never
	// enters this process's memory at all, and a secret this process never
	// holds is a secret it cannot log.
	//
	// The file belongs under this manager's private state directory
	// (/var/lib/backup-manager), never under the backup root: the backup
	// root is what a NAS deployment exports over SMB or AFP, and #298 was
	// filed over precisely that exposure for the SSH key. This package
	// does not enforce that placement, the same way it does not enforce it
	// for Key.File; it is stated here so the person writing the path reads
	// it in the same place they type it.
	File string `yaml:"file,omitempty"`

	// Env names an environment variable the credentials are read from.
	Env string `yaml:"env,omitempty"`

	// Command is an argv array: Command[0] is the executable, invoked
	// directly and never through a shell (so shell metacharacters in any
	// element are inert literal bytes), and the rest are its arguments.
	// Its stdout is treated as the credentials.
	//
	// Identical to Key.Command in shape and in intent: this is how a
	// secrets manager (OpenBao, Vault, SOPS, 1Password, AWS Secrets
	// Manager) is adopted without this project taking a dependency on any
	// of their SDKs or picking a winner among them.
	Command []string `yaml:"command,omitempty"`
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

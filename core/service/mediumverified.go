// Issue #636 (H2.6): the mark that says a storage destination was never
// proven, and the one thing that takes it away.
//
// This file is backupsetverified.go with the other noun in it, and that
// is the point rather than an accident. #623 states one invariant over
// two connections ("neither should be relied on until it has been
// proven"), #624 and #628 made it true on the source side, and #636 is
// the destination side of the same sentence. A second shape here would
// mean an operator learning the same rule twice.
//
// # Why there is a mark at all
//
// `medium add` has verified by default since #443 and `--no-verify` has
// been an explicit opt-out with output that says nothing was contacted.
// What was missing is everything that outlives that line. The check lived
// in the CLI and in the S3 wizard, so POST /storage-mediums accepted a
// destination nobody had verified and answered 201, and a destination
// written unproven was indistinguishable from one checked against a real
// bucket on every surface, forever. #636 calls that a hole rather than an
// escape hatch, in the same words #624 used for the source side.
//
// So the skip is now an instruction to the SERVICE
// (StorageMediumSpec.SkipConnectionCheck), the service runs the check
// itself, and using the skip is recorded in the configuration
// (config.StorageMedium.ConnectionUnverified) rather than only announced.
// Nothing a caller sends writes the mark: an earlier shape of the source
// side's request carried the mark as a claim about what the caller had
// done, and that made it a claim the server could not check (PR #628
// review). This side is built the way that one ended up.
//
// # Why a passing check clears it, and a failing one does not
//
// A mark that could never be removed would not be a state, it would be a
// scar: a destination declared offline on a Tuesday and proven on the
// Wednesday would go on reporting itself unproven for the rest of its
// life, and warnings that cannot be resolved are warnings people learn to
// scroll past. So a `test connection` that PASSES against it removes it.
//
// A check that fails leaves it exactly where it was, and that asymmetry
// is the whole meaning of the mark. Clearing on any call at all would
// turn "this destination works" into "somebody pressed the button".
//
// # The one place this diverges from the source side, said out loud
//
// A source connection check opens a connection and lists a folder. A
// destination check WRITES a probe object into somebody's bucket and
// deletes it again. CreateStorageMedium's own doc used to give that as
// the reason a create must not verify: "a create that silently ran a
// probe would also write a probe object into somebody's bucket from a
// call whose name says nothing about buckets".
//
// That objection is answered rather than overruled, and on its own terms.
// The probe is the check this product already runs from `medium add` and
// from the wizard's step 3 before every save a first-party client makes,
// so the object was already being written on the ordinary path; what
// changes is only WHICH process decides it happens. It lives at a random
// key under a reserved segment no configured artifact can produce, and it
// is deleted by the same run that wrote it (internal/mediumcheck). And
// the alternative is the state #636 exists to end. What the objection
// does still buy is the escape hatch: an operator who does not want this
// deployment touching a bucket at declaration time says so, once, with
// --no-verify, and gets a mark rather than a silent success.
//
// # Which process clears it
//
// The one whose configuration file the mark is in. A passing check is a
// configuration WRITE now, so `medium preflight` / `medium test-connection`
// goes through the same door `medium add` goes through: beside a serving
// engine the check happens in the process that owns config.yaml and the
// live feed. That is #628's conclusion for `backup-set test-connection`,
// and it costs this verb something that one did not, because this verb
// already existed as a read (see cmdMedium's own doc).

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// ErrStorageMediumNotProven is a write that declares where this
// deployment's backups will be sent, refused because that destination
// could not be proven (issue #636).
//
// Its own sentinel, beside ErrStorageMediumInUse and
// ErrStorageMediumIsDefault, because it is the same SHAPE of answer as
// they are: not "your request is malformed" and not "this manager broke",
// but "what you asked for is a decision, and here is what it costs". The
// way past it is SkipConnectionCheck on the spec, and the message names
// the step that failed so the operator has something to fix rather than
// only something to override.
//
// It is a SEPARATE sentinel from ErrConnectionNotProven rather than a
// second use of it, even though the two say the same thing about the two
// halves of one invariant. The refusals reach a caller through different
// doors, carrying different codes (BACKUP_SET_CONNECTION_NOT_PROVEN and
// MEDIUM_CONNECTION_NOT_PROVEN), and they are composed out of different
// reports: a source check's own sentence, and one of internal/mediumcheck's
// eight steps. A surface that could not tell them apart would render a
// bucket's failure under a form about an SSH key.
var ErrStorageMediumNotProven = errors.New("service: the destination this storage medium describes could not be proven")

// storageMediumCheckTimeout bounds one destination check.
//
// Longer than connectionTestTimeout's ten seconds, and deliberately so
// rather than by oversight: a source check makes one connection and lists
// a folder, and this one obtains a credential, reaches an endpoint,
// writes an object, reads it back, asks for its class and deletes it,
// each of which is its own round trip under rclone's own 15-second
// connect ceiling (transport/rclone.ConnectTimeout). Ten seconds here
// would report a working bucket on a slow link as an unreachable one.
//
// A bound exists at all for the reason PR #628's review found on the
// source side: this check runs with configMu held on the edit path, so a
// check that could hang indefinitely is an edit that parks every other
// configuration writer in the process behind it until a restart.
const storageMediumCheckTimeout = 60 * time.Second

// proveStorageMedium runs the destination check in front of a write and
// turns a failing report into the refusal.
//
// It is the SAME check `medium preflight --candidate`, the wizard's step
// 3 and `medium add` all run, reached through the same resolution
// (mediumFromSpec), because a check that proved a slightly different
// destination from the one about to be written would prove nothing. That
// is the property StorageMediumSpec's own doc states for the shared
// request shape, applied to the shared resolution.
//
// An error from the probe itself, as opposed to a report saying no, is
// returned as it is. Those are different things: a bucket that is not
// there is what an operator did, and an instance with no way to reach a
// medium at all is what broke, and PreflightStorageMedium's own doc draws
// the same line one function up.
func (b *BackupService) proveStorageMedium(ctx context.Context, medium config.StorageMedium) error {
	checkCtx, cancel := context.WithTimeout(ctx, storageMediumCheckTimeout)
	defer cancel()

	report, err := b.state.Load().inner.PreflightMediumCandidate(checkCtx, medium)
	if err != nil {
		return fmt.Errorf("service: proving candidate storage medium %s: %w", medium.ID, err)
	}
	projected := toMediumPreflight(report)
	if projected.OK {
		return nil
	}
	return storageMediumNotProvenRefusal(projected)
}

// storageMediumNotProvenRefusal builds the refusal out of the step that
// actually failed.
//
// The FIRST failure and not all of them, because there is only ever one:
// internal/mediumcheck records a failure and then skips everything that
// depended on it, so a report has one failed step and a tail of skipped
// ones. Rendering the skips here would be reading an operator a list of
// things nobody tried.
//
// Both halves of the sentence are safe to echo, and structurally rather
// than carefully. mediumcheck composes every Detail out of its own
// strings and the facts this product already publishes about a
// destination (its id, its bucket, its storage class); no underlying
// error text ever reaches a Check, and there is no field on one it could
// travel in. The classified cause, which names a path on this host or the
// name of an environment variable, goes to the manager's log instead.
// That is FR-33, and it is the same guarantee core/service already relies
// on when it echoes a config.ValidationError.
func storageMediumNotProvenRefusal(report MediumPreflight) error {
	for _, c := range report.Checks {
		if c.Outcome != mediumCheckOutcomeFailed {
			continue
		}
		return fmt.Errorf("%w: the %s check failed. %s", ErrStorageMediumNotProven, c.Step, c.Detail)
	}
	// Unreachable while a report that is not OK has a failed step in it,
	// and here rather than as a panic so a future step vocabulary that
	// grew a third failing outcome produces a refusal somebody can read
	// instead of a nil error that reads as a success.
	return fmt.Errorf("%w: the check did not pass, and reported no step that failed", ErrStorageMediumNotProven)
}

// mediumCheckOutcomeFailed is internal/mediumcheck's own word for a step
// that failed, as it arrives on this boundary.
//
// A constant rather than the string at the one call site above, because
// MediumPreflightCheck.Outcome's doc pins the closed set of three and a
// literal here would be the fourth place that set is written down.
const mediumCheckOutcomeFailed = "failed"

// changesTheDestination reports whether an edit moves anything the
// destination check proves (issue #636).
//
// It compares the whole record rather than a list of fields, and unlike
// changesTheConnection beside it that needs no derivation from the check:
// EVERY field of a config.StorageMedium describes the destination itself.
// The id names it, the type and endpoint and region decide who is being
// dialled, the bucket and prefix decide where the objects go, the
// credential decides whether that account is allowed to put them there,
// and the storage class and verification class are two of
// internal/mediumcheck's own eight steps. There is no equivalent here of
// a backup set's local_path or completion strategy, which are facts about
// THIS deployment that a network check can have no opinion about, so
// there is nothing to leave out.
//
// That means the issue's own list (endpoint, region, bucket, prefix,
// credentials) is a floor rather than the rule. A storage class edited
// from STANDARD to an archive class changes what the deliverable and
// storage_class steps answer, and refusing to re-check it would be
// refusing to re-check the one edit that can make a destination unable to
// take delivery at all.
//
// The mark itself is excluded, obviously but not silently: it is this
// service's own bookkeeping and not part of the destination, and a
// comparison that saw it would run a network check because the previous
// check's result had been written down.
func changesTheDestination(before, after config.StorageMedium) bool {
	return !reflect.DeepEqual(comparableMedium(before), comparableMedium(after))
}

// comparableMedium is m with the mark cleared and every empty slice made
// nil, so two records that differ only in how an absent credential
// command is spelled compare equal.
//
// The empty-slice half is not a corner case, and comparableSource carries
// the same fix for the same reason one file over. This product's own write
// path encodes a medium through yaml.Marshal and reads it back, and a
// credential rebuilt from a credentials_id has a nil Command where one
// round-tripped through the file can have an empty one. reflect.DeepEqual
// tells those apart, so without this every re-save of an unchanged
// destination would run a network check for a command that is there on
// neither side.
func comparableMedium(m config.StorageMedium) config.StorageMedium {
	m.ConnectionUnverified = false
	if len(m.Credentials.Command) == 0 {
		m.Credentials.Command = nil
	}
	return m
}

// clearStorageMediumUnverified removes issue #636's mark from one
// destination after a check has actually passed against it.
//
// It is clearConnectionUnverified with the other noun in it, quiet by
// construction for the identical reasons: a destination that is not
// marked, a service with no configuration file to write to, and an id
// this configuration does not declare all return having done nothing.
// This is called from a check whose own result has already been decided,
// and the check succeeded; turning a bookkeeping failure here into a
// failed check would tell an operator their bucket is unreachable because
// this process could not rewrite a file.
//
// What a failure here does instead is say so on the feed, at warn, naming
// the destination. An operator who sees a destination still reporting
// itself unproven after a check they watched pass has a line explaining
// why, which is the difference between a bug and a mystery.
func (b *BackupService) clearStorageMediumUnverified(ctx context.Context, id string) {
	if b.configPath == "" {
		// A service built over an in-memory configuration (New rather
		// than Open) has no file the mark could have come from or could
		// be cleared in. Nothing to do and nothing to report.
		return
	}
	if id == "" || id == StorageMediumLocalID {
		// The local hard drive is synthesised on every read and declared
		// nowhere (#622), so it carries no mark and there is nothing here
		// to take off it.
		return
	}

	// Read under the same lock every other configuration write in this
	// package takes, and read from DISK rather than from b.state, for the
	// reason writeStorageMedium gives: the write below has to be based on
	// the file's actual current content, never on a possibly stale
	// in-memory copy of it.
	b.configMu.Lock()
	defer b.configMu.Unlock()

	cfg, err := config.Load(b.configPath)
	if err != nil {
		b.reportUnclearedMediumMark(ctx, id, "this deployment's configuration could not be re-read", err)
		return
	}

	found := false
	for i := range cfg.StorageMediums {
		if cfg.StorageMediums[i].ID != id {
			continue
		}
		if !cfg.StorageMediums[i].ConnectionUnverified {
			// The ordinary case, and the one that keeps this cheap: a
			// destination that was proven when it was declared carries no
			// mark, so pressing the button writes nothing at all.
			return
		}
		cfg.StorageMediums[i].ConnectionUnverified = false
		found = true
	}
	if !found {
		return
	}

	// Encoded BEFORE Validate, which resolves retention, alerts and
	// defaults in place. persistConfig's own comment carries the full
	// reasoning, and this does the same sequence by hand rather than
	// calling it because a failure here must be reported on the feed and
	// swallowed rather than returned: the check the operator ran has
	// already passed.
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		b.reportUnclearedMediumMark(ctx, id, "this deployment's configuration could not be encoded", err)
		return
	}
	if err := cfg.Validate(); err != nil {
		b.reportUnclearedMediumMark(ctx, id, "the configuration this would have written is not one this deployment would load", err)
		return
	}
	applyValidators, err := planValidatorCatalog(cfg)
	if err != nil {
		b.reportUnclearedMediumMark(ctx, id, "this deployment's validator catalog could not be resolved", err)
		return
	}
	if err := writeConfigBytesAtomically(b.configPath, encoded); err != nil {
		b.reportUnclearedMediumMark(ctx, id, "this deployment's configuration could not be written", err)
		return
	}
	applyValidators()
	b.adoptConfig(cfg)

	// Said on the feed, at info, because it is a change to the
	// configuration and every other change to the configuration says so.
	// Under connection_test rather than an event name of its own: #622
	// settled that this is "test connection" on every surface an operator
	// reads, and a second event name would be the same question filed
	// under two words again.
	b.logger.Event(ctx, obs.LevelInfo, connectionTestEventName,
		"connection test: this storage destination is no longer marked as unverified, because it has now been proven",
		slog.String("storage_medium", id),
	)
}

// reportUnclearedMediumMark says that a check passed and the mark stayed
// on, with the reason, rather than letting the two disagree in silence.
//
// The cause goes on the line because everything here is a fact about THIS
// deployment's own configuration file, which is the operator's own
// property and the thing they would go and look at. That is the opposite
// call from a check step's cause (internal/mediumcheck's FR-33 rule), and
// it is the same distinction: a transport error names somebody else's
// endpoint, and a configuration error names the file in front of you.
func (b *BackupService) reportUnclearedMediumMark(ctx context.Context, id, what string, err error) {
	b.logger.Event(ctx, obs.LevelWarn, connectionTestEventName,
		"connection test: this storage destination was proven, but it is still marked as unverified because "+what,
		slog.String("storage_medium", id),
		slog.String("cause", err.Error()),
	)
}

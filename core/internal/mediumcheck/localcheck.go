package mediumcheck

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spdrman/rclone-manager/core/internal/capacity"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Test connection for the LOCAL hard drive (H2.2, issue #622).
//
// # Why the local destination has a check at all
//
// Until #622 the local backup root was not a destination on any surface:
// it was the thing a tier meant when it named nothing, so there was
// nowhere to put a button and, the argument ran, nothing to test because
// no network is involved.
//
// That argument is wrong in the way that matters. A local destination can
// be broken in exactly the shapes a remote one can, minus the credential:
// the path can be gone (a NAS volume that did not mount is the ordinary
// case, and it looks like a working deployment right up to the first
// write), it can be there and unwritable by the uid this service runs as
// (a container bind-mount whose owner is the host user is the ordinary
// case there), and the filesystem can be full, which is the one failure
// that arrives while everything is configured perfectly. Every one of
// those is discovered today by a transfer, mid-cycle, after a backup has
// already been chosen to move.
//
// So this is Run's argument applied one destination over: prove what will
// bite, before the thing that needs it to work finds out.
//
// # It reports the same Report the S3 check reports
//
// One shape, so a surface renders one list. That is not tidiness: the
// destinations card, the tier picker and `medium test-connection` all
// draw a report without knowing which kind of destination produced it,
// and a second shape would be a second renderer, which is how one of them
// comes to say something the other does not.
//
// The step vocabulary is shared for the same reason, and where a step
// cannot mean anything here it is SKIPPED rather than quietly passed.
// `credentials` is skipped because a local directory reads none, and
// `storage_class` is skipped because a filesystem has no classes. A
// surface that rendered either as passed would be telling an operator
// something was established that nobody looked at, which is the exact
// failure Skipped exists as a first-class outcome to prevent.
//
// # The three answers #622 asks for, and how they are told apart
//
// A missing path, a permission problem and a full filesystem have to read
// as three different problems, because they have three different fixes.
// They are distinguished by the STEP that failed, which is the
// machine-readable half a surface branches on, and by the category where a
// transport-shaped one exists:
//
//	missing path      reach fails, category not_found
//	permission        reach or write fails, category permission_denied
//	full filesystem   space fails, with no category
//
// The full-filesystem case has no category on purpose. Check.Category's
// own doc says it is empty when a step failed for a reason no transport
// produced, and no transport produced this one: it is a statfs reading
// weighed against the operator's own FR-21 numbers. Inventing a category
// for it would mean adding one to transport.Category, which is a
// vocabulary lifecycle branches on and whose every member has to answer
// Retryable and halt classification. A step name is the honest place for
// this distinction and is the field a surface reads first anyway.
//
// # Why the permission check is a real write and not a mode comparison
//
// The question an operator has is "can the service write here", and the
// only answer that is not a guess is a write. Comparing st_mode and the
// process uid gets ACLs wrong, gets group membership wrong, gets
// read-only mounts wrong and gets container user namespaces wrong, and
// every one of those is a live arrangement for this product. This process
// runs as the service uid, so the probe write below is asked with exactly
// the credentials a real transfer would be asked with.
//
// # FR-33 holds here too
//
// Nothing an os error says reaches a Check. A path is a fact about this
// machine, and a report of this is rendered in a browser and exported
// from a terminal, so the underlying error goes to the log through
// Observe and the Check carries one of this package's own sentences. That
// is fail()'s rule, unchanged, and the reason RunLocal composes its
// failures out of transport.Error values: the category comes from the
// classification rather than from a string somebody wrote at the call
// site.

// StepSpace is whether the filesystem a local destination writes into has
// room for a backup to land (issue #622).
//
// It is not in Steps, which is the ORDER a storage-medium preflight runs
// its own steps in and has to stay exactly that: a bucket has no free
// space this manager can read, so a step asking about one would be
// permanently skipped on every S3 report for no reason a reader could
// act on. LocalSteps below is where it lives.
const StepSpace Step = "space"

// LocalSteps is every step a local test connection performs, in order.
//
// A list of its own rather than Steps with an insertion, because the two
// destinations genuinely prove different things and pretending otherwise
// would put a permanently skipped step on one of them. What they SHARE is
// the Step vocabulary and the Report shape, which is what a surface needs.
var LocalSteps = []Step{
	StepCredentials, StepReach, StepDeliverable, StepSpace,
	StepWrite, StepReadBack, StepStorageClass, StepVerification, StepDelete,
}

// LocalTarget describes the local destination being checked: the
// directory backups land in, and the operator's own FR-21 lines that
// decide when it is too full to take one.
//
// The thresholds arrive as plain byte counts rather than as a
// config.Capacity, which is internal/capacity's own rule for the same
// numbers: this package has no need to know where they came from, only
// that they are bytes, and a config type here would change shape every
// time that one does.
type LocalTarget struct {
	// Root is the directory this deployment's backups land in, already
	// resolved (config.EffectiveBackupRoot). Empty is a real state and is
	// its own failure: a deployment with no backup set configured has no
	// local destination to check yet, and saying so is a better answer
	// than checking the process's working directory.
	Root string

	// SafetyMarginBytes is FR-21's margin: the space this manager holds
	// back before every transfer. Space below it is space a transfer will
	// already refuse to start into, so a destination there is not ready
	// whatever a probe write of a hundred bytes would say.
	//
	// Unsigned, matching capacity.Thresholds and capacity.Stat rather
	// than internal/config's signed fields, because every comparison in
	// this file is against a statfs reading and a mixed-signedness
	// comparison is the one arithmetic mistake worth designing out.
	SafetyMarginBytes uint64

	// CriticalFreeBytes is the operator's own critical line, zero meaning
	// they drew none. It is weighed as well as the margin because they
	// are different statements: the margin is what one transfer needs,
	// and the critical line is where this deployment has already decided
	// it is in trouble.
	CriticalFreeBytes uint64
}

// localProbeDir is the directory segment every probe this file writes
// lives in, inside the backup root.
//
// A directory of its own rather than a dotfile beside the backups, for
// probePrefix's reason one storage kind over: an artifact may
// legitimately be called anything, and a shared namespace is how a probe
// and a backup come to have one spelling. It is removed on the way out,
// and an empty one left behind by a process that was killed mid-check is
// harmless and says what it is.
const localProbeDir = ".rclone-manager-preflight"

// RunLocal performs one test connection against the local hard drive and
// reports what it found, in the identical Report shape Run produces for a
// storage medium.
//
// observe, when set, is called once per failed step with the underlying
// cause, so the operator's log keeps the diagnostic FR-33 keeps out of the
// report. Nil discards it, exactly as Deps.Observe does.
//
// The returned error is non-nil only for something that stopped the check
// running at all, which here means a cancelled context. A destination that
// does not work is a successful call with a Report saying so, which is
// Run's own contract and the reason a surface can render both the same
// way.
func RunLocal(ctx context.Context, observe func(Step, error), target LocalTarget) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("mediumcheck: local test connection: %w", err)
	}

	r := &localRun{observe: observe, target: target}
	r.skip(StepCredentials, "a local destination reads no credential: the backups are written by this service to a directory on this machine.")
	r.reachable()
	r.deliverable()
	r.roomy()
	r.written()
	r.skip(StepStorageClass, "a filesystem has no storage classes, so there is nothing here that could have landed in a different one than the configuration asked for.")
	return r.report(), nil
}

// localRun holds one local check's state while the steps walk forward,
// mirroring run's shape so the two files read the same way.
type localRun struct {
	observe func(Step, error)
	target  LocalTarget

	checks map[Step]Check

	// reached is true once the root is there and is a directory.
	reached bool

	// probe is the file the write step created, so the delete step knows
	// whether it is rolling something back or explaining why it is not.
	probe string
}

func (r *localRun) record(step Step, outcome Outcome, category, detail string) {
	if r.checks == nil {
		r.checks = make(map[Step]Check, len(LocalSteps))
	}
	r.checks[step] = Check{Step: step, Outcome: outcome, Category: category, Detail: detail}
}

func (r *localRun) pass(step Step, detail string) { r.record(step, Passed, "", detail) }
func (r *localRun) skip(step Step, detail string) { r.record(step, Skipped, "", detail) }

// fail is FR-33 enforced rather than remembered, exactly as run.fail is:
// the error, which names a path on this machine, goes to the log, and the
// Check gets a category and a sentence this package composed.
func (r *localRun) fail(step Step, err error, detail string) {
	if r.observe != nil && err != nil {
		r.observe(step, err)
	}
	r.record(step, Failed, categoryName(err), detail)
}

// reachable is the local equivalent of a HEAD against the bucket: is the
// place named, is it there, and is it a directory.
//
// The three are separate sentences because they are separate fixes. No
// root at all is a deployment with nothing configured yet; a root that is
// not there is nearly always a volume that did not mount; a root that is a
// file is a typo in the path.
func (r *localRun) reachable() {
	if r.target.Root == "" {
		r.fail(StepReach, errors.New("no backup root is configured or derivable"),
			"This deployment cannot say which directory its backups land in yet. That is what a deployment with no backup set configured looks like, and it settles as soon as the first one is added.")
		return
	}

	info, err := os.Stat(r.target.Root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.fail(StepReach, transport.NewError(transport.NotFound, "stat", err),
			"The directory this deployment's backups land in is not there. On a NAS that is almost always a volume that did not mount: the path is right and nothing is behind it, so a backup would be written into an empty mount point on the system disk.")
		return
	case errors.Is(err, fs.ErrPermission):
		r.fail(StepReach, transport.NewError(transport.PermissionDenied, "stat", err),
			"The directory this deployment's backups land in cannot be looked at by the user this service runs as. Something above it in the path denies traversal to that user.")
		return
	case err != nil:
		r.fail(StepReach, err, "The directory this deployment's backups land in could not be examined. The manager's log carries the reason.")
		return
	case !info.IsDir():
		r.fail(StepReach, transport.NewError(transport.Configuration, "stat", errors.New("backup root is not a directory")),
			"The path this deployment's backups land in is not a directory. Backups are written as files inside it, so nothing can be delivered here until the path names a directory.")
		return
	}
	r.reached = true
	r.pass(StepReach, "The directory this deployment's backups land in is there and is a directory.")
}

// deliverable is the local answer to "can an artifact be delivered here at
// all", and unlike the S3 one it never depends on a class: a filesystem
// takes delivery of whatever it is given and hands it straight back.
func (r *localRun) deliverable() {
	if !r.reached {
		r.skip(StepDeliverable, "This was never tried: the directory could not be reached.")
		return
	}
	r.pass(StepDeliverable, "A local directory takes delivery of a backup and can read it back immediately, with no restore and no waiting.")
}

// roomy is the full-filesystem answer, and the one step with no
// counterpart in the S3 check.
//
// It weighs the reading against the operator's own two numbers rather
// than against a figure this package invented, because "full" is a
// decision this deployment has already made: a transfer refuses to start
// into less than the safety margin, so a destination with less than that
// is not ready however cheerfully a hundred-byte probe would write.
func (r *localRun) roomy() {
	if !r.reached {
		r.skip(StepSpace, "This was never tried: the directory could not be reached.")
		return
	}

	stat, err := capacity.StatPath(r.target.Root)
	if err != nil {
		r.fail(StepSpace, err, "The free space on the filesystem this deployment's backups land on could not be read. The manager's log carries the reason.")
		return
	}

	available := stat.AvailableBytes
	switch {
	case available == 0:
		r.fail(StepSpace, errors.New("no space available on the backup filesystem"),
			"The filesystem this deployment's backups land on is full. Nothing can be written here at all until something frees space, and retention is not what frees it: this manager never deletes a backup to make room.")
	case r.target.SafetyMarginBytes > 0 && available < r.target.SafetyMarginBytes:
		r.fail(StepSpace, errors.New("available space is below the configured safety margin"),
			fmt.Sprintf("The filesystem this deployment's backups land on has %s available, which is below the %s safety margin this deployment holds back before every transfer. A transfer would be refused before it started.",
				humanBytes(available), humanBytes(r.target.SafetyMarginBytes)))
	case r.target.CriticalFreeBytes > 0 && available < r.target.CriticalFreeBytes:
		r.fail(StepSpace, errors.New("available space is below the configured critical line"),
			fmt.Sprintf("The filesystem this deployment's backups land on has %s available, which is below the %s critical line this deployment set for itself.",
				humanBytes(available), humanBytes(r.target.CriticalFreeBytes)))
	default:
		r.pass(StepSpace, fmt.Sprintf("The filesystem this deployment's backups land on has %s available.", humanBytes(available)))
	}
}

// written is the write, the read back and the delete, taken together
// because the delete is the rollback of the write and skipping it would
// leave a probe file in somebody's backup directory.
func (r *localRun) written() {
	if !r.reached {
		r.skip(StepWrite, "This was never tried: the directory could not be reached.")
		r.skip(StepReadBack, "This was never tried: nothing was written.")
		r.skip(StepVerification, "This was never tried: nothing was written.")
		r.skip(StepDelete, "This was never tried: nothing was written, so there is nothing to roll back.")
		return
	}

	dir := filepath.Join(r.target.Root, localProbeDir)
	name, err := localProbeName()
	if err != nil {
		r.fail(StepWrite, err, "A probe file name could not be generated, so nothing was written. The manager's log carries the reason.")
		r.skip(StepReadBack, "This was never tried: nothing was written.")
		r.skip(StepVerification, "This was never tried: nothing was written.")
		r.skip(StepDelete, "This was never tried: nothing was written, so there is nothing to roll back.")
		return
	}
	path := filepath.Join(dir, name)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.failWrite(err)
		return
	}
	if err := os.WriteFile(path, probeBody, 0o600); err != nil {
		r.failWrite(err)
		// The directory may have been created by the MkdirAll above, so
		// take it back out rather than leaving a directory behind for a
		// check that established nothing.
		_ = os.Remove(dir)
		return
	}
	r.probe = path
	r.pass(StepWrite, "A probe file was written into the directory this deployment's backups land in, as the user this service runs as.")

	// Read back before the delete, so a filesystem that accepts a write
	// and returns something else is caught here rather than by a restore.
	// It is the identical claim the S3 check's read_back makes and it
	// matters for the same reason: a write nobody read is a write nobody
	// proved.
	got, err := os.ReadFile(path)
	switch {
	case err != nil:
		r.fail(StepReadBack, err, "The probe file could not be read back after it was written. The manager's log carries the reason.")
		r.skip(StepVerification, "This was never tried: the probe could not be read back.")
	case !bytes.Equal(got, probeBody):
		r.fail(StepReadBack, errors.New("probe file content differs from what was written"),
			"The probe file came back with content this manager did not write. A restore from here would not return the backup that was stored.")
		r.skip(StepVerification, "This was never tried: the probe did not come back byte for byte.")
	default:
		r.pass(StepReadBack, "The probe file came back byte for byte.")
		r.pass(StepVerification, "A backup here is proven by reading it back, which this check just did. There is nothing that has to be asked of a provider and nothing that costs egress.")
	}

	if err := os.Remove(path); err != nil {
		r.fail(StepDelete, err, "The probe file could not be deleted, so this check left a file behind. A destination this manager can write to and not delete from is one no retention pass can ever clean up.")
		return
	}
	// The directory is best-effort: it is empty and reserved, so leaving
	// it is not a failure of anything an operator asked about, and a
	// second concurrent check holding a probe in it is a legitimate
	// reason for the remove to fail.
	_ = os.Remove(dir)
	r.pass(StepDelete, "The probe file was deleted, so this check left nothing behind.")
}

// failWrite is the write step's failure, told apart by what os said, so
// the commonest local problem (a directory the service uid cannot write
// to) reads as a permission problem rather than as a generic one.
func (r *localRun) failWrite(err error) {
	detail := "A probe file could not be written into the directory this deployment's backups land in. The manager's log carries the reason."
	classified := err
	switch {
	case errors.Is(err, fs.ErrPermission):
		classified = transport.NewError(transport.PermissionDenied, "write", err)
		detail = "The directory this deployment's backups land in cannot be written to by the user this service runs as. In a container this is usually the bind-mount's owner: the directory belongs to a user on the host and this service is a different one."
	case errors.Is(err, fs.ErrNotExist):
		classified = transport.NewError(transport.NotFound, "write", err)
		detail = "The directory this deployment's backups land in went away between being looked at and being written to."
	}
	r.fail(StepWrite, classified, detail)
	r.skip(StepReadBack, "This was never tried: nothing was written.")
	r.skip(StepVerification, "This was never tried: nothing was written.")
	r.skip(StepDelete, "This was never tried: nothing was written, so there is nothing to roll back.")
}

// report assembles the Checks in LocalSteps order, with the same
// "a step that recorded nothing is a hole rather than silence" rule
// run.report holds.
func (r *localRun) report() Report {
	out := Report{Medium: localReportMedium, OK: true, Checks: make([]Check, 0, len(LocalSteps))}
	for _, step := range LocalSteps {
		c, ok := r.checks[step]
		if !ok {
			c = Check{Step: step, Outcome: Skipped, Detail: "this check did not run"}
		}
		if c.Outcome == Failed {
			out.OK = false
		}
		out.Checks = append(out.Checks, c)
	}
	return out
}

// localReportMedium is the id this report names, and it is deliberately
// config.MediumLocal's string rather than an import of that package.
//
// mediumcheck sits under everything and imports internal/config nowhere,
// the same way internal/config does not import artifactstore to say that
// MediumLocal and KindLocal are the same word. The agreement is pinned by
// a test instead, which is the convention this tree already uses for
// exactly this pair.
const localReportMedium = "local"

// localProbeName mints the probe file's name. Random rather than fixed,
// so two checks running at once do not read each other's file and report
// a content mismatch neither filesystem is responsible for.
func localProbeName() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mediumcheck: generating a probe file name: %w", err)
	}
	return "probe-" + hex.EncodeToString(raw[:]), nil
}

// humanBytes renders a byte count the way an operator reads one, because
// the sentence it lands in is about whether there is room and "83886080"
// is not an answer to that. Powers of 1024 with one decimal, which is the
// convention the capacity surfaces in this product already use.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

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
	"sort"
	"syscall"

	"github.com/backupdproject/backupd/core/internal/capacity"
	"github.com/backupdproject/backupd/core/internal/transport"
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

// StepDistinctVolume is whether a local destination lives on a
// genuinely different filesystem from every OTHER local destination this
// deployment already writes backups into (issue #666).
//
// It is StepSpace's twin in every way that matters: not in Steps for the
// identical reason (an S3 bucket has no filesystem to be the SAME
// filesystem as, so the step would be a permanent, meaningless skip on
// every remote report), and it exists because a directory can fail in a
// fourth way none of reach/write/space names: two configured
// destinations can be the SAME disk under two different paths, which is
// a backup strategy that only LOOKS like one. A drive failure that takes
// out one path takes out both, so a retention tier that thinks it holds
// a second copy on this "second" destination is wrong in exactly the way
// #666 exists to catch before an operator finds out from a failed
// restore.
const StepDistinctVolume Step = "distinct_volume"

// LocalSteps is every step a local test connection performs, in order.
//
// A list of its own rather than Steps with an insertion, because the two
// destinations genuinely prove different things and pretending otherwise
// would put a permanently skipped step on one of them. What they SHARE is
// the Step vocabulary and the Report shape, which is what a surface needs.
var LocalSteps = []Step{
	StepCredentials, StepReach, StepDistinctVolume, StepDeliverable, StepSpace,
	StepWrite, StepReadBack, StepStorageClass, StepVerification, StepDelete,
}

// LocalTarget describes the local destination being checked: the
// directory backups land in, the same FR-21 lines that decide when it is
// too full to take one, and every OTHER local directory this deployment
// already writes backups into (issue #666).
//
// The thresholds arrive as plain byte counts rather than as a
// config.Capacity, which is internal/capacity's own rule for the same
// numbers: this package has no need to know where they came from, only
// that they are bytes, and a config type here would change shape every
// time that one does.
//
// This one struct describes BOTH the backup root's own check and a
// declared local_volume instance's check (#666): the two used to be one
// destination and are now a family of them, and a caller distinguishes
// them only by which id and which other roots it passes in, never by a
// second type. RunLocal does not know or care whether ID is the reserved
// local id or an operator-chosen one; it is a string this package
// composes into a Report and a message, nothing more.
type LocalTarget struct {
	// Root is the directory this destination writes into, already
	// resolved. Empty is a real state and is its own failure: a
	// deployment with no backup set configured has no local destination
	// to check yet, and saying so is a better answer than checking the
	// process's working directory.
	Root string

	// ID is this destination's own id, carried into the Report and into
	// StepDistinctVolume's messages. Empty means the reserved local id
	// (localReportMedium, "local"), so every caller that predates #666
	// and never set this field goes on reporting exactly what it always
	// reported.
	ID string

	// OtherRoots is every OTHER local destination this deployment
	// already writes backups into, keyed by its own id: the implicit
	// backup root under the reserved local id, and every other declared
	// local_volume instance. Empty means there is nothing yet to compare
	// against, which is the ordinary state before a second local
	// destination exists, and StepDistinctVolume passes rather than
	// failing on an empty comparison set: "nothing to collide with" is
	// not the same claim as "proven distinct".
	OtherRoots map[string]string

	// SafetyMarginBytes is held back on top of every incoming artifact's
	// own size before a transfer is admitted. See internal/capacity's
	// headroom-arithmetic section for what it is meant to cover (listing
	// drift, block rounding, other writers on the same volume) and for why
	// it is a plain byte count this package does not try to compute on an
	// operator's behalf.
	SafetyMarginBytes uint64

	// CriticalFreeBytes is the operator's own critical line, zero meaning
	// they drew none. It is weighed as well as the margin because they
	// are different statements: the margin is what one transfer needs,
	// and the critical line is where this deployment has already decided
	// it is in trouble.
	CriticalFreeBytes uint64
}

// id is the id RunLocal reports and names in messages: ID, or the
// reserved local id when the caller left it unset (see ID's own doc).
func (t LocalTarget) id() string {
	if t.ID == "" {
		return localReportMedium
	}
	return t.ID
}

// localProbeDir is the directory segment every probe this file writes
// lives in, inside the backup root.
//
// A directory of its own rather than a dotfile beside the backups, for
// probePrefix's reason one storage kind over: an artifact may
// legitimately be called anything, and a shared namespace is how a probe
// and a backup come to have one spelling.
//
// # It is created and never removed, and that is the fix rather than laziness
//
// This used to be taken back out on the way through, on the reasoning
// that a check should leave nothing behind. What it actually left behind
// was a race. Two checks running at once both MkdirAll it and both remove
// it, so one can remove it between the other's MkdirAll and its
// WriteFile, and the loser reports a failed write classified not_found.
// That is the arm whose sentence says the directory "went away between
// being looked at and being written to", which is how this product says a
// NAS volume did not mount, produced by somebody double-clicking a
// button.
//
// Two at once is the ordinary case rather than an exotic one: the
// destinations card and every retention tier's picker all offer this
// check on one page, and the local destination is the one they point at
// by default.
//
// So the directory stays. It is empty, it is reserved, it is named for
// what it is, and transport.MediumKey cannot compose an artifact key
// under it, so what it costs is an inode. The probe FILE is still
// removed, which is the claim StepDelete actually makes: a destination
// this manager can write to and not delete from is one no retention pass
// could ever clean up.
const localProbeDir = ".backupd-preflight"

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
	r.distinctFilesystem()
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

// distinctFilesystem is #666's fourth local answer: is this directory
// genuinely on its own disk, or is it the SAME disk as another
// configured local destination reached by a different path.
//
// It runs right after reachable, before anything is written, because it
// is a property of the PATH itself and answers the same kind of question
// reachable does: not "does this directory work" but "is this the
// directory it claims to be". An operator who points a second
// destination at a subdirectory of the first one, or at a bind-mount of
// the same underlying disk under a different name, has declared two
// destinations that are one disk failure away from being zero, and
// nothing about that mistake shows up in a write, a read-back or a free
// space reading: all three succeed identically whether or not the disk
// underneath is shared.
//
// An empty OtherRoots is not evidence of anything: it is the ordinary
// state before a second local destination exists, or the state of the
// implicit backup root's own check before #666's callers started passing
// their sibling destinations in. This step is deliberately never SKIPPED
// once the directory is reached, even with nothing to compare against:
// it PASSES, because "there is nothing to collide with yet" is a
// distinct, positive answer to "is this destination distinct", not an
// untried step.
func (r *localRun) distinctFilesystem() {
	if !r.reached {
		r.skip(StepDistinctVolume, "This was never tried: the directory could not be reached.")
		return
	}

	dev, err := deviceIDOf(r.target.Root)
	if err != nil {
		r.fail(StepDistinctVolume, err, "The filesystem this directory lives on could not be identified. The manager's log carries the reason.")
		return
	}

	others := make([]string, 0, len(r.target.OtherRoots))
	for id := range r.target.OtherRoots {
		others = append(others, id)
	}
	sort.Strings(others)

	for _, id := range others {
		otherDev, err := deviceIDOf(r.target.OtherRoots[id])
		if err != nil {
			// That other destination's own problem (gone, unmounted,
			// unreadable) is not this one's to report; its own check,
			// run separately, is where that surfaces.
			continue
		}
		if otherDev == dev {
			r.fail(StepDistinctVolume, fmt.Errorf("shares a filesystem with local destination %q", id),
				fmt.Sprintf("This directory is on the same filesystem as %q. Two destinations on one disk protect against nothing a single copy did not already protect against: a drive failure takes both down together, so a retention tier that treats this as a second copy of %q is not getting one.",
					id, id))
			return
		}
	}
	r.pass(StepDistinctVolume, "This directory is on its own filesystem, distinct from every other local destination this deployment writes to.")
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
	out := Report{Medium: r.target.id(), OK: true, Checks: make([]Check, 0, len(LocalSteps))}
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

// deviceIDOf identifies the filesystem that contains path, by the device
// number POSIX stat(2) reports (st_dev): two paths on the same device are
// on the same filesystem regardless of how differently they are spelled,
// which is exactly what a bind-mount, a symlink or a subdirectory of an
// already-configured destination would otherwise hide from a plain string
// comparison of paths.
//
// A var rather than a plain function so a test can substitute a fake
// mapping and prove distinctFilesystem's two branches (same device,
// different device) without needing two real filesystems mounted on the
// machine running the test - the identical reason TestRunLocal_
// NoRoomFailsItsOwnStep drives the safety margin rather than trying to
// fill a real disk. Nothing overrides this in production.
var deviceIDOf = defaultDeviceIDOf

// defaultDeviceIDOf is deviceIDOf's real implementation. syscall.Stat_t's
// Dev field is available on every platform capacity.StatPath already
// targets (linux/amd64, linux/arm64, darwin; see that file's own doc), so
// no build-tag split is needed here.
func defaultDeviceIDOf(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("mediumcheck: stat %q: %w", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("mediumcheck: %q: no device information available on %T", path, info.Sys())
	}
	return uint64(st.Dev), nil
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

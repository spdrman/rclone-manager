package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file answers one question, and the shape of the answer is the part
// worth explaining: three verdicts rather than a bool.
//
// "Not valid" has two meanings here that must never be run together. An
// artifact that should have a readable local copy and does not is a
// corruption finding, and it quarantines. An artifact whose durable copy is
// on a storage medium has no local copy to check, which is not a fault and
// is not a verdict about the artifact at all.
//
// A bool cannot hold both, and the price of collapsing them has already
// been paid once. The comment saying a completed move must not read as a
// missing local file sat directly above four lines that read it exactly
// that way, and the first move to complete in production was quarantined as
// lost on the following cycle. The third verdict exists so that mistake
// stops being expressible.

// localVerdict is what checkLocalFinal concluded about an artifact's local
// copy. It has three values rather than a bool because "not valid" has
// two meanings that reconcile must never confuse, and a bool cannot hold
// both.
type localVerdict string

const (
	// localValid: a readable local copy exists and passed every check.
	localValid localVerdict = "valid"

	// localInvalid: the artifact should have a readable local copy and
	// does not, or has one that fails a check. This is the verdict that
	// quarantines.
	localInvalid localVerdict = "invalid"

	// localOnMedium: there is no local copy to check, and that is not a
	// fault. The artifact's durable copy is on a storage medium (an
	// ACTIVE medium placement with no ACTIVE local one), which is what a
	// completed move leaves behind. Reconcile has no way to read a medium
	// and no mandate to: FR-31 makes anything beyond an existence check
	// operator-initiated, and internal/revalidate is what runs that
	// existence check. So this is not a verdict about the artifact at
	// all, and a caller that treats it as localInvalid records a healthy
	// backup as an irrecoverable loss.
	localOnMedium localVerdict = "on-medium"
)

// localValidity is the outcome of checking one artifact's recorded local
// final file against what the journal says it should be. Reason is
// populated whenever Verdict is not localValid: for localInvalid it says
// what failed, for localOnMedium it says where the copy is.
//
// A localValid verdict carries a Reason only when the check found
// something an operator still has to know about, which today means one
// thing: the row's own two size records contradict each other and the file
// settled it (issue #662). A fault nobody is told about is how #662's
// empty record survived four states of a lifecycle, so the sentence
// travels with the verdict rather than being dropped on the floor because
// the answer happened to be "valid".
type localValidity struct {
	Verdict localVerdict
	Reason  string
}

// invalid builds the quarantining verdict.
func invalid(reason string) localValidity {
	return localValidity{Verdict: localInvalid, Reason: reason}
}

// note renders a localValid verdict's Reason as a clause a handler can
// append to its own sentence, or nothing when there is nothing to add.
//
// It is a method rather than four string concatenations in reconcile.go so
// that the four handlers cannot drift on whether a reported record fault
// reaches the operator at all.
func (v localValidity) note() string {
	if v.Reason == "" {
		return ""
	}
	return "; " + v.Reason
}

// checkLocalFinal is FR-17's "is the final local copy still good" check,
// the reconciliation-time twin of the question
// internal/lifecycle/remotedelete.go's verifyLocalFinal asks immediately
// before a delete. I could not reuse that function directly, it is
// unexported in a package I am not allowed to modify, so I reimplemented
// the same three checks against the same journal fields here: the file
// exists where the artifact's own placement says it is, its size matches
// whichever of the journal's two independent size records was captured,
// and, when a local hash was recorded at VERIFIED, its content still
// hashes to that value.
//
// A missing file counts as invalid, not as a separate "absent" case: by
// the time an artifact reaches COMMITTED, REMOTE_DELETE_PENDING, COMPLETE
// or REMOTE_RETAINED, FR-17's table only distinguishes "final" from
// "invalid final" for these states, with no third option, and a final copy
// that is not even there any more cannot honestly be called anything but
// invalid.
func checkLocalFinal(rec state.Record) localValidity {
	// Asked of the artifact's ACTIVE local placement (EPIC E, FR-29)
	// rather than of rec.LocalPath directly. The two are the same value
	// for every artifact that has one in Phase 1; the difference is what
	// happens once an artifact's only copy can be on a storage medium,
	// which this check must not read as a missing local file.
	//
	// That sentence was written above the four lines that did precisely
	// that: a false answer became "no local final path is recorded", which
	// is localInvalid, and the first completed move in production was
	// quarantined as lost on the next cycle. The bool cannot say WHY there
	// is no readable local path, so the second question is asked here,
	// before anything is called invalid.
	localPath, ok := rec.ReadableLocalPath()
	if !ok {
		if mediums := rec.ActiveMediumPlacements(); len(mediums) > 0 {
			return localValidity{Verdict: localOnMedium, Reason: fmt.Sprintf(
				"no local copy to check: the durable copy is on storage medium %s, which reconciliation does not read (revalidation existence-checks it, FR-31)",
				describeMediumPlacements(mediums))}
		}
		if len(rec.Placements) > 0 {
			return invalid("no ACTIVE copy of this artifact is recorded anywhere: every placement in the journal is GONE or DELETE_PENDING")
		}
		return invalid("no local final path is recorded in the journal")
	}

	info, err := os.Stat(localPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return invalid(fmt.Sprintf("local final file %s is missing", localPath))
		}
		return invalid(fmt.Sprintf("stat %s: %v", localPath, err))
	}
	if info.IsDir() {
		return invalid(fmt.Sprintf("local final path %s is a directory, not a file", localPath))
	}

	expected, source, contradiction := expectedLocalSize(rec)
	if contradiction != nil {
		return settleRecordContradiction(rec, localPath, info.Size(), contradiction)
	}
	if source != "" && info.Size() != expected {
		return invalid(fmt.Sprintf(
			"local final file %s is %d bytes, expected %d (from %s)", localPath, info.Size(), expected, source))
	}

	if rec.LocalHashAlg != "" {
		if !strings.EqualFold(rec.LocalHashAlg, string(transport.SHA256)) {
			return invalid(fmt.Sprintf("cannot verify local identity: unsupported recorded hash algorithm %q", rec.LocalHashAlg))
		}
		sum, err := sha256File(localPath)
		if err != nil {
			return invalid(fmt.Sprintf("hashing %s: %v", localPath, err))
		}
		if !strings.EqualFold(sum, rec.LocalHash) {
			return invalid(fmt.Sprintf(
				"local final file %s hash %s does not match the %s hash recorded at verification, %s",
				localPath, sum, rec.LocalHashAlg, rec.LocalHash))
		}
	}

	return localValidity{Verdict: localValid}
}

// settleRecordContradiction answers a row whose own two independent size
// records contradict each other, by reading the file instead of condemning
// it (issue #662, defect 2).
//
// # Why this is not a verdict about the artifact
//
// "recorded remote size 294 disagrees with recorded transfer size 0" was
// the sentence a live deployment quarantined an intact 294-byte backup on.
// Both of its operands are journal fields. A disagreement between two
// records is a fault in the RECORDS; which of them is wrong is a question
// only the bytes can answer, and the file has already been stat'ed by the
// time this runs.
//
// # Which record the file is compared against, and why that one
//
// The remote identity, captured at discovery from the remote object's own
// stat. It is the operand that cannot have been produced by the local
// read-back this fault comes out of: #662's row recorded 0 bytes
// transferred AND the sha256 of the empty string, one answer from one
// empty read of one file. The transfer size and the recorded local hash
// are that same answer twice, so neither can be used to check the other's
// story, and the discovery-time remote size is the only independent
// measurement in the row.
//
// That is also why the recorded hash is not consulted on the agreeing
// branch below: half of a self-contradictory record is not evidence. The
// row is reported as needing repair, with what was measured, rather than
// being silently believed.
//
// A file that matches neither figure IS refused, and the refusal names the
// digest this function measured. A reason built only out of record fields
// cannot tell an operator whether the file was inspected or only the
// bookkeeping was, and in the field it was only the bookkeeping.
func settleRecordContradiction(rec state.Record, localPath string, onDisk int64, contradiction error) localValidity {
	if rec.Remote.Size == nil {
		// Unreachable through expectedLocalSize, which only contradicts
		// itself when both sizes are recorded. Stated rather than assumed,
		// because a nil dereference here would be a panic inside a
		// reconciliation pass.
		return invalid(contradiction.Error())
	}
	remote := *rec.Remote.Size

	if onDisk != remote {
		sum, err := sha256File(localPath)
		if err != nil {
			return invalid(fmt.Sprintf(
				"this artifact's own records contradict each other (%v) and hashing %s to settle it failed: %v",
				contradiction, localPath, err))
		}
		return invalid(fmt.Sprintf(
			"local final file %s is %d bytes and hashes to %s, which is not the %d bytes the remote object was recorded as at discovery; "+
				"this artifact's own records also contradict each other (%v), so the copy was checked against the remote identity, "+
				"the one figure the local read-back could not have written",
			localPath, onDisk, sum, remote, contradiction))
	}

	return localValidity{Verdict: localValid, Reason: fmt.Sprintf(
		"this artifact's journal row needs repair (%v), and the durable local copy was checked against the remote identity instead: "+
			"%s is %d bytes, exactly the size the remote object was recorded as at discovery. "+
			"The recorded local hash was not used, because it was recorded from the same read of the same copy as the contradicted byte count",
		contradiction, localPath, onDisk)}
}

// describeMediumPlacements names each medium copy and the verification
// class it has achieved, for the one reason an operator reads when
// reconciliation leaves a moved artifact alone. The class is there
// because "on cold_offsite, unverified" and "on cold_offsite, content
// verified" are different facts about how safe that artifact is, and the
// reason is the only place this package says either.
func describeMediumPlacements(ps []state.Placement) string {
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		class := p.VerificationClass
		if class == "" {
			class = "unverified"
		}
		parts = append(parts, fmt.Sprintf("%q (%s)", p.Medium, class))
	}
	return strings.Join(parts, ", ")
}

// expectedLocalSize picks the size the local final file must have from
// whichever of the journal's two independent size records is present, and
// reports the two disagreeing with each other rather than silently
// preferring one. source is "" only when neither is recorded at all, in
// which case there is nothing to compare a file size against and
// checkLocalFinal skips that part of the check; the hash check, when a
// hash was recorded, still applies regardless.
//
// The returned error is a fault in the ROW, not a verdict about the file,
// and issue #662 is what the difference cost. checkLocalFinal used to turn
// it straight into invalid(), which quarantines, so an intact 294-byte
// backup was condemned on two numbers disagreeing with each other while
// the file itself, already stat'ed, was never asked.
// settleRecordContradiction is what answers it now.
func expectedLocalSize(rec state.Record) (size int64, source string, err error) {
	haveRemote := rec.Remote.Size != nil
	haveTransfer := rec.Transfer != nil

	switch {
	case haveRemote && haveTransfer:
		if *rec.Remote.Size != rec.Transfer.BytesTransferred {
			return 0, "", fmt.Errorf(
				"recorded remote size %d disagrees with recorded transfer size %d", *rec.Remote.Size, rec.Transfer.BytesTransferred)
		}
		return *rec.Remote.Size, "recorded remote size", nil
	case haveRemote:
		return *rec.Remote.Size, "recorded remote size", nil
	case haveTransfer:
		return rec.Transfer.BytesTransferred, "recorded transfer size", nil
	default:
		return 0, "", nil
	}
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

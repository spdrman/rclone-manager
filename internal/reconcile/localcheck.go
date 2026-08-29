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

	"github.com/spdrman/rclone-manager/internal/state"
	"github.com/spdrman/rclone-manager/internal/transport"
)

// localValidity is the outcome of checking one artifact's recorded local
// final file against what the journal says it should be. Reason is
// populated, and only meaningful, when Valid is false.
type localValidity struct {
	Valid  bool
	Reason string
}

// checkLocalFinal is FR-17's "is the final local copy still good" check,
// the reconciliation-time twin of the question
// internal/lifecycle/remotedelete.go's verifyLocalFinal asks immediately
// before a delete. I could not reuse that function directly, it is
// unexported in a package I am not allowed to modify, so I reimplemented
// the same three checks against the same journal fields here: the file
// exists at rec.LocalPath, its size matches whichever of the journal's two
// independent size records was captured, and, when a local hash was
// recorded at VERIFIED, its content still hashes to that value.
//
// A missing file counts as invalid, not as a separate "absent" case: by
// the time an artifact reaches COMMITTED, REMOTE_DELETE_PENDING or
// COMPLETE, FR-17's table only distinguishes "final" from "invalid final"
// for these states, with no third option, and a final copy that is not
// even there any more cannot honestly be called anything but invalid.
func checkLocalFinal(rec state.Record) localValidity {
	if rec.LocalPath == "" {
		return localValidity{Reason: "no local final path is recorded in the journal"}
	}

	info, err := os.Stat(rec.LocalPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return localValidity{Reason: fmt.Sprintf("local final file %s is missing", rec.LocalPath)}
		}
		return localValidity{Reason: fmt.Sprintf("stat %s: %v", rec.LocalPath, err)}
	}
	if info.IsDir() {
		return localValidity{Reason: fmt.Sprintf("local final path %s is a directory, not a file", rec.LocalPath)}
	}

	expected, source, err := expectedLocalSize(rec)
	if err != nil {
		return localValidity{Reason: err.Error()}
	}
	if source != "" && info.Size() != expected {
		return localValidity{Reason: fmt.Sprintf(
			"local final file %s is %d bytes, expected %d (from %s)", rec.LocalPath, info.Size(), expected, source)}
	}

	if rec.LocalHashAlg != "" {
		if !strings.EqualFold(rec.LocalHashAlg, string(transport.SHA256)) {
			return localValidity{Reason: fmt.Sprintf("cannot verify local identity: unsupported recorded hash algorithm %q", rec.LocalHashAlg)}
		}
		sum, err := sha256File(rec.LocalPath)
		if err != nil {
			return localValidity{Reason: fmt.Sprintf("hashing %s: %v", rec.LocalPath, err)}
		}
		if !strings.EqualFold(sum, rec.LocalHash) {
			return localValidity{Reason: fmt.Sprintf(
				"local final file %s hash %s does not match the %s hash recorded at verification, %s",
				rec.LocalPath, sum, rec.LocalHashAlg, rec.LocalHash)}
		}
	}

	return localValidity{Valid: true}
}

// expectedLocalSize mirrors internal/lifecycle/remotedelete.go's helper of
// the same purpose: it picks the size the local final file must have from
// whichever of the journal's two independent size records is present, and
// refuses outright (a non-nil error) if the two disagree with each other,
// rather than silently preferring one. source is "" only when neither is
// recorded at all, in which case there is nothing to compare a file size
// against and checkLocalFinal skips that part of the check; the hash check,
// when a hash was recorded, still applies regardless.
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

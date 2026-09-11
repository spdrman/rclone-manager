// Issue #662, defect 1, at the last seam that could have caught it.
//
// On a live NAS a copy that had genuinely written its bytes was read back
// as empty, and the empty answer became the record. Two shapes came out of
// the one fault. rclone's own copy-time comparison caught two of them
// ("md5 hashes differ ... dst d41d8cd98f00b204e9800998ecf8427e", the md5 of
// the empty string) and the product recorded FAILED, which is the correct
// thing to do with a transport that reports a corrupt transfer. The third
// slipped past: the journal and the sidecar recovery manifest both recorded
//
//	size_bytes: 0
//	checksum:   e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
//	            (the sha256 of the empty string)
//	"verification_class": "content"
//
// over a file that was 294 bytes and perfectly good.
//
// Commit is where that record becomes durable, and it is the one step that
// holds both halves at once: it has the record in hand and it has just
// linked the file into place. It never compares them. localPlacementFor
// copies rec.Transfer.BytesTransferred and rec.LocalHash straight through,
// and writeRecoveryManifest derives every field from the same record, so
// whatever the read-back said is what gets written down, with
// verification_class: "content" stamped on top of it because a hash string
// is present.
//
// So the assertion here is against the bytes on disk, never against another
// record. A test that compared the placement to the manifest would agree
// with itself: both are copies of the same row, and both were wrong
// together in the field.
//
// The positive control in each table is the same fixture with the fault
// absent, and it is not decoration. Without it a check reading "the
// recorded size equals the file's size" could be satisfied by a fixture
// that wrote no file at all (0 == 0), which is exactly the vacuous pass
// this defect is made of.
//
// The FR-12 control at the bottom is the fence. The collision refusal is
// correct and issue #662 says so explicitly; a fix that resolved the
// operator's dead end by letting a transfer overwrite an occupied final
// name would pass every other case in this repository and must fail that
// one.
package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/recovery"
	"github.com/backupdproject/backupd/core/internal/state"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// diversionsPayload is 294 bytes, the size of the artifact issue #662 was
// filed about (dpkg.diversions.5.gz). The exact number matters only because
// every sentence in that issue is written in terms of "294 bytes over a
// record that says 0", so a reader comparing this file to the issue should
// find the same number rather than have to translate.
var diversionsPayload = func() []byte {
	b := make([]byte, 294)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}()

// emptySHA256 is the sha256 of no bytes at all: the checksum #662 found in
// the sidecar manifest of a 294-byte file. It is spelled out rather than
// computed so a reader can match it against the issue by eye.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sha256Of is the on-disk answer every assertion in this file is made
// against. Computing it from the file rather than from a fixture constant
// is the point: the defect is a record that disagrees with the bytes, so
// the bytes have to be the operand.
func sha256Of(t *testing.T, path string) (int64, string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s to establish what is actually on disk: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return int64(len(data)), hex.EncodeToString(sum[:])
}

// walkToVerifiedRecording drives an artifact from DISCOVERED to VERIFIED
// through every edge the state machine declares, carrying the transfer
// result and the local hash the caller wants recorded.
//
// It is a sibling of commit_test.go's walkToVerified rather than a change
// to it: that helper deliberately records no transfer result and no hash,
// which is the right fixture for the convergence tests it serves and is
// precisely the two fields this file is about.
func walkToVerifiedRecording(
	t *testing.T, ctx context.Context, d Deps, artifact model.ArtifactID,
	partial string, recordedSize int64, recordedHash string,
) {
	t.Helper()
	remoteSize := int64(len(diversionsPayload))
	steps := []struct {
		from, to  State
		localPath *string
		remote    *state.RemoteIdentity
		transfer  *state.TransferResult
		hashes    *state.HashUpdate
	}{
		{from: "", to: Discovered, remote: &state.RemoteIdentity{Size: &remoteSize}},
		{from: Discovered, to: Transferring, localPath: &partial},
		{from: Transferring, to: Transferred, transfer: &state.TransferResult{BytesTransferred: recordedSize}},
		{from: Transferred, to: Verifying},
		{from: Verifying, to: Verified, hashes: &state.HashUpdate{Hash: recordedHash, Alg: string(transport.SHA256)}},
	}
	for i, s := range steps {
		if _, err := Advance(ctx, d, state.Transition{
			Artifact:   artifact,
			Key:        fmt.Sprintf("662-setup-%d-%s", i, s.to),
			From:       string(s.from),
			To:         string(s.to),
			LocalPath:  s.localPath,
			RemotePath: "/var/backups/" + artifact.Name,
			Remote:     s.remote,
			Transfer:   s.transfer,
			Hashes:     s.hashes,
		}); err != nil {
			t.Fatalf("walkToVerifiedRecording: Advance %s -> %s: %v", s.from, s.to, err)
		}
	}
}

// TestIssue662_CommitRecordsTheBytesItMadeDurable is issue #662's
// defect 1: the commit step writes a size and a checksum it never checked
// against the file it is committing, so a read-back that came back empty
// becomes a permanent, content-verified record of an empty backup over a
// file that is not empty.
//
// Both halves of the record are asserted, journal placement and sidecar
// recovery manifest, because #662 found the zero in both and a fix that
// corrected only one would leave the recovery path lying. Both are compared
// against a fresh read of the committed file.
func TestIssue662_CommitRecordsTheBytesItMadeDurable(t *testing.T) {
	onDiskSize := int64(len(diversionsPayload))
	sum := sha256.Sum256(diversionsPayload)
	onDiskHash := hex.EncodeToString(sum[:])

	cases := []struct {
		name string

		// recordedSize and recordedHash are what the read-back told the
		// earlier steps, which is the only thing Commit has to go on
		// today.
		recordedSize int64
		recordedHash string
	}{
		{
			// The measured row from #662: a 294-byte file, a record that
			// says zero bytes and hashes to the empty string.
			name:         "an empty read-back recorded over a 294-byte file",
			recordedSize: 0,
			recordedHash: emptySHA256,
		},
		{
			// The positive control. Same fixture, same assertions, the
			// fault absent: the read-back agreed with the file. This is
			// what proves the assertions above can be satisfied at all,
			// and that the case above fails for the defect rather than
			// because the instrument is broken.
			name:         "control: a read-back that agreed with the file",
			recordedSize: onDiskSize,
			recordedHash: onDiskHash,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			j := openTestJournal(t)
			d := Deps{Journal: j}
			artifact := mustID(t)

			dir := t.TempDir()
			partial := mustPartialPath(t, dir, artifact)
			final := mustFinalPath(t, dir, artifact)
			if err := os.WriteFile(partial, diversionsPayload, 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			// The fixture has to be non-empty or every assertion below
			// compares zero against zero and passes having proved nothing.
			if got, _ := sha256Of(t, partial); got == 0 {
				t.Fatalf("fixture wrote an empty .partial; this test cannot say anything about a zero record over a non-empty file")
			}

			walkToVerifiedRecording(t, ctx, d, artifact, partial, tc.recordedSize, tc.recordedHash)

			out, err := Commit(ctx, d, CommitInput{
				Artifact:      artifact,
				LocalDir:      dir,
				CommittingKey: "662-committing",
				CommittedKey:  "662-committed",
			})
			if err != nil {
				t.Fatalf("Commit: %v", err)
			}

			// Everything from here is measured against the committed file
			// itself, never against another field of the same record.
			diskSize, diskHash := sha256Of(t, final)
			if diskSize == 0 {
				t.Fatalf("the committed file at %s is empty; the fixture did not survive the commit", final)
			}

			var placement *state.Placement
			for i := range out.Record.Placements {
				if out.Record.Placements[i].Medium == state.MediumLocal {
					placement = &out.Record.Placements[i]
					break
				}
			}
			if placement == nil {
				t.Fatalf("commit recorded no local placement at all for %s; placements = %+v", artifact, out.Record.Placements)
			}

			if placement.Size == nil {
				t.Errorf("#662 defect 1: the local placement records no size at all for %s, which is %d bytes on disk", final, diskSize)
			} else if *placement.Size != diskSize {
				t.Errorf(
					"#662 defect 1: the journal's local placement records %d bytes for a durable copy that is %d bytes on disk (%s); "+
						"a size that was never checked against the file is what let an empty read-back become the permanent record",
					*placement.Size, diskSize, final)
			}
			if placement.Hash != diskHash {
				t.Errorf(
					"#662 defect 1: the journal's local placement records checksum %s for a durable copy that hashes to %s on disk (%s)",
					placement.Hash, diskHash, final)
			}
			// The class is the claim, and it is the half that makes the
			// record actively misleading rather than merely wrong: a
			// placement that says content verification passed, having
			// compared nothing to the file.
			if placement.VerificationClass == state.VerificationContent && placement.Hash != diskHash {
				t.Errorf(
					"#662 defect 1: the local placement claims verification_class=%q while its recorded checksum %s does not describe the file it points at (%s hashes to %s)",
					placement.VerificationClass, placement.Hash, final, diskHash)
			}

			manifest, err := recovery.ReadManifest(recovery.ManifestPath(dir, artifact.Name))
			if err != nil {
				t.Fatalf("reading the sidecar recovery manifest: %v", err)
			}
			if manifest.SizeBytes != diskSize {
				t.Errorf(
					"#662 defect 1: the sidecar recovery manifest records size_bytes %d for a file that is %d bytes on disk (%s); "+
						"the manifest is the record a rebuild trusts when the journal is gone",
					manifest.SizeBytes, diskSize, final)
			}
			if manifest.Checksum != diskHash {
				t.Errorf(
					"#662 defect 1: the sidecar recovery manifest records checksum %s for a file that hashes to %s on disk (%s)",
					manifest.Checksum, diskHash, final)
			}
			for _, p := range manifest.Placements {
				if p.Medium != state.MediumLocal {
					continue
				}
				if p.SizeBytes == nil || *p.SizeBytes != diskSize {
					got := "nil"
					if p.SizeBytes != nil {
						got = fmt.Sprintf("%d", *p.SizeBytes)
					}
					t.Errorf(
						"#662 defect 1: the manifest's local placement records size_bytes %s for a file that is %d bytes on disk (%s)",
						got, diskSize, final)
				}
			}
		})
	}
}

// TestIssue662_TransferRefusesToRecordACopyShorterThanItsObject is the
// other end of issue #662's defect 1, at the step that first writes the
// number down.
//
// Transfer takes transport.TransferResult.BytesTransferred verbatim and
// records it as the transfer result, with nothing standing between the
// backend's answer and the journal. In the field the backend's answer was
// zero for an object the same journal row already recorded as 294 bytes,
// captured at discovery from the remote's own stat, and nothing anywhere
// compared the two. Everything downstream then agreed with the zero,
// including the verification step, whose expectedSize prefers the transfer
// result over the remote identity.
//
// The assertion is deliberately about the OUTCOME rather than about a
// particular remedy: a transfer that copied nothing for a non-empty object
// must not reach TRANSFERRED carrying a zero-byte result. Refusing the
// copy, retrying it, or re-measuring the .partial all satisfy it.
func TestIssue662_TransferRefusesToRecordACopyShorterThanItsObject(t *testing.T) {
	cases := []struct {
		name string

		// reported is what CopyToLocal claims it transferred; written is
		// what it actually leaves at the .partial path. The field fault is
		// the pair (0, 0) against a remote object of 294 bytes: an empty
		// read-back of a copy that did happen.
		reported int64
		written  []byte
	}{
		{
			name:     "a zero-byte copy of a 294-byte remote object",
			reported: 0,
			written:  nil,
		},
		{
			// Positive control: the same transport, the same journal, the
			// same assertions, with the copy reporting what it really
			// moved. It has to pass today, or the case above proves
			// nothing about the defect.
			name:     "control: a copy that reported the whole object",
			reported: int64(len(diversionsPayload)),
			written:  diversionsPayload,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			j := openTestJournal(t)
			artifact := mustID(t)
			dir := t.TempDir()

			remoteSize := int64(len(diversionsPayload))
			if remoteSize == 0 {
				t.Fatal("fixture payload is empty; a short-copy test needs a non-empty remote object")
			}
			if _, err := j.Discover(ctx, artifact, "662-discover", "/var/backups/"+artifact.Name,
				state.RemoteIdentity{Size: &remoteSize}, epoch662); err != nil {
				t.Fatalf("Discover: %v", err)
			}

			tr := &shortCopyTransport{reported: tc.reported, written: tc.written}
			out, err := Transfer(ctx, Deps{Journal: j, Transport: tr}, TransferParams{
				Artifact:   artifact,
				LocalDir:   dir,
				AttemptKey: "662-transfer",
			})
			if tr.calls != 1 {
				t.Fatalf("precondition: CopyToLocal was called %d times, want exactly 1; this test says nothing if the copy never ran", tr.calls)
			}

			if State(out.Record.State) != Transferred {
				// A refusal is one of the correct answers, so this is not
				// a failure: report what happened and stop.
				t.Logf("transfer did not reach TRANSFERRED (state=%q, err=%v)", out.Record.State, err)
				return
			}
			if out.Record.Transfer == nil {
				t.Fatalf("TRANSFERRED recorded with no transfer result at all for %s", artifact)
			}
			if out.Record.Transfer.BytesTransferred != remoteSize {
				t.Errorf(
					"#662 defect 1: TRANSFERRED recorded BytesTransferred=%d for a remote object the same journal row records as %d bytes; "+
						"the backend's own answer is written down unchecked, and every later step (verification's expectedSize, the commit "+
						"placement, the sidecar manifest) prefers it over the remote size that contradicts it",
					out.Record.Transfer.BytesTransferred, remoteSize)
			}
		})
	}
}

// TestIssue662_FR12StillRefusesAStrayFileAtTheFinalName is the FR-12 fence, and
// it passes today. It is here so that a fix for issue #662's dead end
// cannot be built by weakening the collision refusal.
//
// #662 says this outright: the refusal is correct, and
// FinalNameCollisionError's own doc is right that only an operator can tell
// a stray file apart from the real thing. The bug is that the product never
// lets that operator act, not that it refuses. A transfer that overwrote
// the file below would satisfy every other case in this file and must fail
// this one.
func TestIssue662_FR12StillRefusesAStrayFileAtTheFinalName(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	artifact := mustID(t)
	dir := t.TempDir()

	remoteSize := int64(len(diversionsPayload))
	if _, err := j.Discover(ctx, artifact, "662-fence-discover", "/var/backups/"+artifact.Name,
		state.RemoteIdentity{Size: &remoteSize}, epoch662); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// A file the journal has never heard of, at the name the artifact is
	// about to claim. Nothing in this package can tell it from a real
	// backup, which is the whole reason FR-12 refuses.
	final := mustFinalPath(t, dir, artifact)
	stray := []byte("a file nobody in this product can identify")
	if err := os.WriteFile(final, stray, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tr := &shortCopyTransport{reported: remoteSize, written: diversionsPayload}
	_, err := Transfer(ctx, Deps{Journal: j, Transport: tr}, TransferParams{
		Artifact:   artifact,
		LocalDir:   dir,
		AttemptKey: "662-fence",
	})

	var collision *FinalNameCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("FR-12: err = %v, want a *FinalNameCollisionError; the collision refusal must survive any fix for #662", err)
	}
	if tr.calls != 0 {
		t.Errorf("FR-12: CopyToLocal ran %d times; the refusal must happen before any byte is written", tr.calls)
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("the stray file is gone after a refused transfer: %v", err)
	}
	if string(got) != string(stray) {
		t.Errorf("FR-12: the file at the final name was modified by a refused transfer:\n got %q\nwant %q", got, stray)
	}
	if _, err := os.Stat(mustPartialPath(t, dir, artifact)); !os.IsNotExist(err) {
		t.Errorf("FR-12: a refused transfer left a .partial behind at %s (err=%v)", mustPartialPath(t, dir, artifact), err)
	}
}

// epoch662 is the instant every fixture in this file is discovered at. A
// fixed one, for the reason internal/app's own epoch is fixed: nothing here
// is about the clock, and a fixture reading the real one decides
// differently either side of midnight.
var epoch662 = time.Date(2026, 9, 7, 1, 18, 48, 0, time.UTC)

// shortCopyTransport is a transport.Transport that can write one thing and
// report another, which is the whole of issue #662's defect 1 expressed as
// a double.
//
// Every other method returns an explicit error rather than a zero value,
// following the convention internal/reconcile's and
// internal/lifecycle/remotedelete_test.go's own fakes already use: a test
// that reaches further than it meant to fails on a sentence naming the
// method rather than on a silently plausible empty answer.
type shortCopyTransport struct {
	reported int64
	written  []byte
	calls    int
}

var _ transport.Transport = (*shortCopyTransport)(nil)

func (f *shortCopyTransport) List(context.Context, transport.Source) ([]transport.RemoteArtifact, error) {
	return nil, errors.New("shortCopyTransport: List not used")
}

func (f *shortCopyTransport) Stat(_ context.Context, _ transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	return transport.RemoteArtifact{Path: remotePath, Size: int64(len(diversionsPayload))}, nil
}

func (f *shortCopyTransport) CopyToLocal(_ context.Context, _ transport.Source, _, localPartialPath string) (transport.TransferResult, error) {
	f.calls++
	if err := os.MkdirAll(filepath.Dir(localPartialPath), 0o755); err != nil {
		return transport.TransferResult{}, err
	}
	if err := os.WriteFile(localPartialPath, f.written, 0o644); err != nil {
		return transport.TransferResult{}, err
	}
	return transport.TransferResult{BytesTransferred: f.reported}, nil
}

func (f *shortCopyTransport) RemoteHash(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
	return "", errors.New("shortCopyTransport: RemoteHash not used")
}

func (f *shortCopyTransport) DeleteRemote(context.Context, transport.Source, string) error {
	return errors.New("shortCopyTransport: DeleteRemote not used")
}

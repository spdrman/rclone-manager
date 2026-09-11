// Issue #662, the two seams the fix for it opened in commit.go itself.
//
// #662's remedy was to stop writing the journal's local placement and the
// sidecar recovery manifest from the journal row and write them from a
// fresh measurement of the committed file instead. That is right, and it
// is what emptyrecord_662_test.go pins. It also moved two things that the
// old, record-derived code did not have to think about, and this file is
// about those two.
//
//  1. Commit's already-COMMITTED convergence branch now re-derives the
//     manifest from a measurement taken at convergence time, which is a
//     different instant from the commit. When that measurement disagrees
//     with the record, the branch computed the disagreement and threw it
//     away: it makes no journal write, so commitMeasurement.Fault -- the
//     entire channel by which a disagreement reaches an operator -- had
//     nowhere to go, and the manifest was overwritten to certify whatever
//     the file said now. The manifest is "the record a rebuild trusts when
//     the journal is gone" (writeRecoveryManifest's own doc), so that is a
//     rebuild being handed a certificate of the damage.
//
//  2. measureCommitted's corrective re-hash reads the file to record what
//     is actually there, and that fallback runs precisely when the
//     environment has ALREADY demonstrated that it lies about read-backs:
//     it is reached only because the recorded byte count contradicted
//     os.Stat. io.Copy returns a nil error on a short read, so the digest
//     could describe fewer bytes than the size beside it, and
//     localPlacementFor then stamps verification_class: content on the
//     pair. That is the exact invariant #662 is about, one layer in.
//
// Both assertions here are made against the artefact an operator or a
// rebuild would actually read -- the manifest file's bytes, and the
// returned error -- never against a second copy of the same record.
package lifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/backupdproject/backupd/core/internal/recovery"
	"github.com/backupdproject/backupd/core/internal/state"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// TestIssue662_ConvergedCommitRefusesToRestampTheManifestFromADamagedFile
// commits a real artifact, damages the committed file underneath it, and
// calls Commit again with the same keys so the already-COMMITTED
// convergence branch runs.
//
// The assertion that matters is the second one: the manifest on disk must
// be BYTE-IDENTICAL to what it was before the converged call. Asserting
// the error alone would be satisfied by a fix that returned an error after
// having already written the file, and the file is what a rebuild reads.
func TestIssue662_ConvergedCommitRefusesToRestampTheManifestFromADamagedFile(t *testing.T) {
	ctx := t.Context()
	j := openTestJournal(t)
	d := Deps{Journal: j}
	artifact := mustID(t)

	dir := t.TempDir()
	partial := mustPartialPath(t, dir, artifact)
	final := mustFinalPath(t, dir, artifact)
	if err := os.WriteFile(partial, diversionsPayload, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sum := sha256.Sum256(diversionsPayload)
	walkToVerifiedRecording(t, ctx, d, artifact, partial, int64(len(diversionsPayload)), hex.EncodeToString(sum[:]))

	in := CommitInput{
		Artifact:      artifact,
		LocalDir:      dir,
		CommittingKey: "662-restamp-committing",
		CommittedKey:  "662-restamp-committed",
	}
	if _, err := Commit(ctx, d, in); err != nil {
		t.Fatalf("first Commit: %v", err)
	}

	manifestPath := recovery.ManifestPath(dir, artifact.Name)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("reading the sidecar recovery manifest the first commit wrote: %v", err)
	}
	// Precondition, not decoration: if the manifest did not record the
	// true size to begin with, the comparison after the damage would be
	// comparing one wrong answer against another.
	honest, err := recovery.ReadManifest(manifestPath)
	if err != nil {
		t.Fatalf("parsing the manifest the first commit wrote: %v", err)
	}
	if honest.SizeBytes != int64(len(diversionsPayload)) {
		t.Fatalf("precondition: the first commit recorded size_bytes %d for a %d-byte artifact; this test cannot say anything about a re-stamp",
			honest.SizeBytes, len(diversionsPayload))
	}

	// The damage. A truncation is the shape #662 was filed about -- fewer
	// bytes at the final name than the record says -- and it leaves the
	// journal row entirely untouched, which is the situation the converged
	// branch actually meets.
	const damagedSize = 10
	if err := os.Truncate(final, damagedSize); err != nil {
		t.Fatalf("truncating the committed file to simulate the damage: %v", err)
	}

	out, err := Commit(ctx, d, in)

	after, readErr := os.ReadFile(manifestPath)
	if readErr != nil {
		t.Fatalf("the sidecar recovery manifest is unreadable after the converged call: %v", readErr)
	}
	if !bytes.Equal(before, after) {
		t.Errorf(
			"#662: the already-COMMITTED convergence branch re-stamped the sidecar recovery manifest from a fresh measurement of a damaged file, "+
				"certifying the damage in the one record a rebuild trusts when the journal is gone.\n"+
				"before the converged call: %s\n after the converged call: %s\n"+
				"(the file at %s was truncated to %d bytes; the journal still records %d)",
			before, after, final, damagedSize, len(diversionsPayload))
	}
	if err == nil {
		t.Errorf(
			"#662: converging on an already-COMMITTED artifact whose file no longer matches the record returned err = <nil> (state=%q); "+
				"this branch makes no journal write, so the disagreement it measured reaches nobody at all -- which is #662's own failure mode",
			out.Record.State)
		return
	}
	// The error is a secondary assertion, but it has to be actionable:
	// an operator meeting it needs the path and both counts.
	for _, want := range []string{final, "10", "294"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("#662: the converged refusal does not name %q, so an operator cannot act on it: %v", want, err)
		}
	}
}

// TestIssue662_ConvergedCommitStillRewritesAnIdenticalManifestWhenNothingChanged
// is the positive control for the refusal above, and it is the reason the
// refusal can be narrow rather than a blanket "never write on convergence".
//
// writeRecoveryManifest's converged-branch call exists so a process killed
// after the COMMITTED journal write but before the manifest was ever
// written still gets one. That path must keep working, and the bytes it
// writes must be the same bytes -- which is precisely the property the
// refusal above is what enforces.
func TestIssue662_ConvergedCommitStillRewritesAnIdenticalManifestWhenNothingChanged(t *testing.T) {
	ctx := t.Context()
	j := openTestJournal(t)
	d := Deps{Journal: j}
	artifact := mustID(t)

	dir := t.TempDir()
	partial := mustPartialPath(t, dir, artifact)
	if err := os.WriteFile(partial, diversionsPayload, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sum := sha256.Sum256(diversionsPayload)
	walkToVerifiedRecording(t, ctx, d, artifact, partial, int64(len(diversionsPayload)), hex.EncodeToString(sum[:]))

	in := CommitInput{
		Artifact:      artifact,
		LocalDir:      dir,
		CommittingKey: "662-converge-committing",
		CommittedKey:  "662-converge-committed",
	}
	if _, err := Commit(ctx, d, in); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	manifestPath := recovery.ManifestPath(dir, artifact.Name)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("reading the manifest the first commit wrote: %v", err)
	}

	// Stand in for the crash writeRecoveryManifest's doc describes: the
	// COMMITTED journal write landed, the manifest did not survive.
	if err := os.Remove(manifestPath); err != nil {
		t.Fatalf("simulating a crash between the COMMITTED write and the manifest write: %v", err)
	}

	out, err := Commit(ctx, d, in)
	if err != nil {
		t.Fatalf("converged Commit over an UNCHANGED file: %v; the refusal must be narrow enough to leave the crash-recovery path working", err)
	}
	if State(out.Record.State) != Committed {
		t.Fatalf("converged Commit: state = %q, want %s", out.Record.State, Committed)
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("the converged call did not rewrite the lost manifest: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the converged call rewrote a DIFFERENT manifest for an unchanged file:\nbefore: %s\n after: %s", before, after)
	}
}

// TestIssue662_MeasureCommittedRefusesAHashItCouldNotReadWhole drives
// measureCommitted's corrective re-hash over a path whose os.Stat size and
// whose readable byte count genuinely disagree, and requires it to refuse
// rather than pair one with the other.
//
// The fixture is a FIFO, because that is the one construction that
// produces the disagreement deterministically from inside a test on a
// filesystem that behaves: a FIFO stats as zero bytes and then reads out
// however many bytes a writer put in it, with io.Copy returning the nil
// error it also returns for a short read of a regular file. The fault #662
// hit in the field -- a read-back that reports one thing and the file that
// is another -- is not reproducible on demand on a healthy local
// filesystem, and this fallback is reached ONLY when the environment has
// already proved it can do exactly that. So the fixture stands in for the
// lying read-back, and what is asserted is the function's own arithmetic:
// it must not return a size from one source and a digest from another.
func TestIssue662_MeasureCommittedRefusesAHashItCouldNotReadWhole(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "lying-readback.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}

	// Precondition: the stat size and the readable byte count have to
	// actually differ, or this test proves nothing.
	info, err := os.Stat(fifo)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() == int64(len(diversionsPayload)) {
		t.Fatalf("precondition: the fixture stats as %d bytes, the same as the %d it will read, so there is no disagreement to refuse",
			info.Size(), len(diversionsPayload))
	}

	written := make(chan error, 1)
	go func() {
		// Opening the write end blocks until measureCommitted opens the
		// read end, which is what pairs the two without a sleep.
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			written <- err
			return
		}
		_, writeErr := w.Write(diversionsPayload)
		written <- errors.Join(writeErr, w.Close())
	}()

	// A record whose byte count contradicts the stat, which is the only
	// way the corrective re-hash is reached at all.
	rec := state.Record{
		Artifact:     mustID(t),
		Transfer:     &state.TransferResult{BytesTransferred: int64(len(diversionsPayload))},
		LocalHash:    emptySHA256,
		LocalHashAlg: string(transport.SHA256),
	}

	m, err := measureCommitted(fifo, rec)
	if writeErr := <-written; writeErr != nil {
		t.Fatalf("the fixture's writer failed, so nothing was read: %v", writeErr)
	}

	if err == nil {
		t.Fatalf(
			"#662: measureCommitted stat'ed %s as %d bytes, then read %d bytes out of it, and returned no error: "+
				"Size=%d paired with a %s digest of different bytes (%s), which localPlacementFor stamps verification_class=%q on. "+
				"io.Copy reports nil on a short read, and this fallback runs only because the environment already lied about a read-back once",
			fifo, info.Size(), len(diversionsPayload), m.Size, m.Alg, m.Hash,
			localPlacementFor(m, fifo).VerificationClass)
	}
	for _, want := range []string{fifo, "294", "partial read"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("#662: measureCommitted's refusal does not name %q: %v", want, err)
		}
	}
	if m.Hash != "" || m.Size != 0 {
		t.Errorf("#662: measureCommitted refused but still returned a measurement (%+v); a refused measurement must not be recordable", m)
	}
}

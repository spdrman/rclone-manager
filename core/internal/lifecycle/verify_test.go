package lifecycle

// Fakes in this file are named verifyJournal/verifyTransport, deliberately
// distinct from engine_test.go's fakeJournal (whose Get is unused there,
// since Advance never calls it) and from transfer_test.go's
// fakeTransferJournal/fakeTransport (owned by Transfer's own tests, and not
// configurable the way Verify's RemoteHash-heavy tests need). testArtifact
// is reused as-is from transfer_test.go rather than redeclared.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/retry"
)

// --- fakes ---

// verifyJournal is a minimal in-memory Journal for Verify's own tests. It
// needs a working Get (Verify reads the current record itself, unlike
// Advance's own tests), plus the same idempotency-key-replay and
// from-state-mismatch behaviour the real journal has, so tests can trust
// what they observe.
type verifyJournal struct {
	rec       state.Record
	getErr    error
	recordErr error
	seen      map[string]state.Outcome
	recorded  []state.Transition

	// enteredAt mirrors what state.Journal.LastEnteredAt reads out of the
	// real transition log: the time of the newest transition INTO a state
	// from a different one. A same-state pass does not touch it, which is
	// the whole property remotedelete.go's WP3.2 gate depends on, so a
	// fake that simply stamped every write here would quietly hide the bug
	// that check exists to prevent.
	enteredAt map[string]time.Time
}

func newVerifyJournal(rec state.Record) *verifyJournal {
	return &verifyJournal{rec: rec, seen: make(map[string]state.Outcome), enteredAt: make(map[string]time.Time)}
}

func (j *verifyJournal) LastEnteredAt(_ context.Context, _ model.ArtifactID, st string) (time.Time, bool, error) {
	at, ok := j.enteredAt[st]
	return at, ok, nil
}

// LastTransition is unused by the step under test, and the safety property
// it exists for (issue #220's reinstatement forfeiture) is proved against a
// real journal in remotedelete_reinstate_test.go, not here. Reporting "no
// such edge" is the honest answer for a fake that records no log at all.
func (j *verifyJournal) LastTransition(context.Context, model.ArtifactID, string, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func (j *verifyJournal) Get(context.Context, model.ArtifactID) (state.Record, error) {
	if j.getErr != nil {
		return state.Record{}, j.getErr
	}
	return j.rec, nil
}

func (j *verifyJournal) RecordTransition(_ context.Context, t state.Transition) (state.Outcome, error) {
	j.recorded = append(j.recorded, t)

	if out, ok := j.seen[t.Key]; ok {
		return state.Outcome{Applied: false, Record: out.Record}, nil
	}
	if j.recordErr != nil {
		return state.Outcome{}, j.recordErr
	}
	if j.rec.State != t.From {
		return state.Outcome{}, fmt.Errorf("verifyJournal: state mismatch: have %q, want from %q", j.rec.State, t.From)
	}

	if t.To != t.From {
		j.enteredAt[t.To] = t.OccurredAt
	}
	j.rec.State = t.To
	if t.Hashes != nil {
		j.rec.LocalHash = t.Hashes.Hash
		j.rec.LocalHashAlg = t.Hashes.Alg
	}
	if t.Validation != nil {
		passed := t.Validation.Passed
		j.rec.ValidationPassed = &passed
		j.rec.ValidationDetail = t.Validation.Detail
	}
	if t.Retry != nil {
		j.rec.RetryCount = t.Retry.Count
		j.rec.LastError = t.Retry.LastError
		j.rec.NextRetryAt = t.Retry.NextAttempt
	}

	out := state.Outcome{Applied: true, Record: j.rec}
	j.seen[t.Key] = out
	return out, nil
}

func (j *verifyJournal) currentState() string { return j.rec.State }

func (j *verifyJournal) transitionsTo(to State) []state.Transition {
	var out []state.Transition
	for _, t := range j.recorded {
		if t.To == string(to) {
			out = append(out, t)
		}
	}
	return out
}

// verifyTransport is a minimal transport.Transport. Only RemoteHash is ever
// exercised by Verify; the rest exist solely to satisfy the interface, and
// fail loudly if a test accidentally reaches them.
type verifyTransport struct {
	remoteHashFunc  func(ctx context.Context, source transport.Source, remotePath string, alg transport.HashAlgorithm) (string, error)
	remoteHashCalls int
}

func (t *verifyTransport) List(context.Context, transport.Source) ([]transport.RemoteArtifact, error) {
	return nil, errors.New("verifyTransport: List not used")
}

func (t *verifyTransport) Stat(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
	return transport.RemoteArtifact{}, errors.New("verifyTransport: Stat not used")
}

func (t *verifyTransport) CopyToLocal(context.Context, transport.Source, string, string) (transport.TransferResult, error) {
	return transport.TransferResult{}, errors.New("verifyTransport: CopyToLocal not used")
}

func (t *verifyTransport) RemoteHash(ctx context.Context, source transport.Source, remotePath string, alg transport.HashAlgorithm) (string, error) {
	t.remoteHashCalls++
	if t.remoteHashFunc == nil {
		return "", errors.New("verifyTransport: RemoteHash not configured")
	}
	return t.remoteHashFunc(ctx, source, remotePath, alg)
}

func (t *verifyTransport) DeleteRemote(context.Context, transport.Source, string) error {
	return errors.New("verifyTransport: DeleteRemote not used")
}

// --- helpers ---

func verifyWriteLocalFile(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artifact.dump")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("writing local file: %v", err)
	}
	return path
}

// verifyingRecord builds the journal row Verify expects to find: VERIFYING,
// with a recorded transfer result matching localPath's real size.
func verifyingRecord(t *testing.T, localPath string, size int64) state.Record {
	t.Helper()
	return state.Record{
		Artifact:   testArtifact(t),
		RemotePath: "backups/backup-2026-08-27.dump.zst",
		LocalPath:  localPath,
		State:      string(Verifying),
		Transfer:   &state.TransferResult{BytesTransferred: size},
	}
}

// mustScript writes an executable POSIX shell script and returns its
// absolute path, satisfying config.Validate's requirement that a
// validator's executable be absolute.
func mustScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "validator.sh")
	full := "#!/bin/sh\n" + body
	if err := os.WriteFile(path, []byte(full), 0o755); err != nil {
		t.Fatalf("writing validator script: %v", err)
	}
	return path
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// --- layer 1: transfer verification, always ---

func TestVerify_TransferOnly_NoHashNoValidator_Verifies(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := &verifyTransport{}

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact:   rec.Artifact,
		AttemptKey: "attempt-1",
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Verified) {
		t.Fatalf("state = %q, want %q", out.Record.State, Verified)
	}
	if tr.remoteHashCalls != 0 {
		t.Fatalf("RemoteHash called %d times, want 0 when Hash policy is empty", tr.remoteHashCalls)
	}
	if out.Record.LocalHash == "" || out.Record.LocalHashAlg != "sha256" {
		t.Fatalf("expected a recorded local hash even with no hash policy, got %+v", out.Record)
	}
}

func TestVerify_TransferVerification_MissingLocalFile_Fails(t *testing.T) {
	rec := verifyingRecord(t, filepath.Join(t.TempDir(), "does-not-exist"), 10)
	j := newVerifyJournal(rec)
	tr := &verifyTransport{}

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Failed) {
		t.Fatalf("state = %q, want %q", out.Record.State, Failed)
	}
}

func TestVerify_TransferVerification_SizeMismatch_Fails(t *testing.T) {
	path := verifyWriteLocalFile(t, []byte("short"))
	rec := verifyingRecord(t, path, 999) // does not match what's actually on disk
	j := newVerifyJournal(rec)
	tr := &verifyTransport{}

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Failed) {
		t.Fatalf("state = %q, want %q", out.Record.State, Failed)
	}
	failed := j.transitionsTo(Failed)
	if len(failed) != 1 || !strings.Contains(failed[0].Detail, "expected 999 bytes") {
		t.Fatalf("FAILED detail = %+v, want it to name the expected size", failed)
	}
}

func TestVerify_TransferVerification_NoExpectedSize_Fails(t *testing.T) {
	path := verifyWriteLocalFile(t, []byte("x"))
	rec := verifyingRecord(t, path, 0)
	rec.Transfer = nil // no transfer result and no remote size recorded

	j := newVerifyJournal(rec)
	tr := &verifyTransport{}

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Failed) {
		t.Fatalf("state = %q, want %q: with no expected size on record, this must refuse rather than guess", out.Record.State, Failed)
	}
}

// --- layer 2: hash verification ---

func TestVerify_Hash_TrustsProducerSuppliedChecksum_SkipsRemoteHash(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	rec.Transfer.Checksummed = true
	j := newVerifyJournal(rec)
	tr := &verifyTransport{remoteHashFunc: func(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
		t.Fatal("RemoteHash must not be called once the transfer step already recorded a trustworthy checksum")
		return "", nil
	}}

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Hash: "sha256"},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Verified) {
		t.Fatalf("state = %q, want %q", out.Record.State, Verified)
	}
	if tr.remoteHashCalls != 0 {
		t.Fatalf("RemoteHash called %d times, want 0", tr.remoteHashCalls)
	}
}

func TestVerify_Hash_MatchesRemoteHash_Verifies(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	want := sha256Hex(content)

	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := &verifyTransport{remoteHashFunc: func(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
		return want, nil
	}}

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Hash: "sha256"},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Verified) {
		t.Fatalf("state = %q, want %q", out.Record.State, Verified)
	}
	if tr.remoteHashCalls != 1 {
		t.Fatalf("RemoteHash called %d times, want 1", tr.remoteHashCalls)
	}
	if out.Record.LocalHash != want {
		t.Fatalf("recorded LocalHash = %q, want %q", out.Record.LocalHash, want)
	}
}

func TestVerify_Hash_MismatchQuarantines(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := &verifyTransport{remoteHashFunc: func(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
		return sha256Hex([]byte("something else entirely")), nil
	}}

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Hash: "sha256"},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Quarantined) {
		t.Fatalf("state = %q, want %q: a proven hash mismatch is a content-invalid finding, not a mere failure", out.Record.State, Quarantined)
	}
}

// TestVerify_Hash_RequiredButCapabilityAbsent_FailsExplicitly_NeverSilentlyVerifies
// is the proof for this issue's central constraint: against a hardened,
// shell-less SFTP account (docs/ssh-setup.md's recommended setup),
// rclone's sftp backend cannot run a remote hash command at all, and
// Transport.RemoteHash returns exactly the classified error simulated
// here (transport.UnsupportedCapability; see
// internal/transport/rclone/errors_test.go's
// unsupported_capability_remote_hash_on_a_shell_less_account and gate_test.go's
// RemoteHashCapability for that fact proved against a real fixture). A
// configured "Hash: sha256" policy must never be silently satisfied by the
// transfer-verification checks alone once this happens.
func TestVerify_Hash_RequiredButCapabilityAbsent_FailsExplicitly_NeverSilentlyVerifies(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := &verifyTransport{remoteHashFunc: func(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
		return "", transport.NewError(transport.UnsupportedCapability, "remote_hash", errors.New("backend cannot compute sha256"))
	}}

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Hash: "sha256"},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State == string(Verified) {
		t.Fatal("an unsupported remote hash must never be silently accepted as Verified")
	}
	if out.Record.State != string(Failed) {
		t.Fatalf("state = %q, want %q", out.Record.State, Failed)
	}
	failed := j.transitionsTo(Failed)
	if len(failed) != 1 {
		t.Fatalf("recorded %d FAILED transitions, want 1", len(failed))
	}
	if !strings.Contains(failed[0].Detail, "unsupported_capability") {
		t.Fatalf("FAILED detail = %q, want it to explicitly name the capability category", failed[0].Detail)
	}
}

func TestVerify_Hash_EmptyPolicy_NeverCallsRemoteHashEvenWithoutAProducerChecksum(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	rec.Transfer.Checksummed = false
	j := newVerifyJournal(rec)
	tr := &verifyTransport{remoteHashFunc: func(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
		t.Fatal("RemoteHash must not be called when the operator did not configure a hash policy")
		return "", nil
	}}

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Hash: ""},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Verified) {
		t.Fatalf("state = %q, want %q: transfer verification alone is a legitimate, explicitly-chosen posture", out.Record.State, Verified)
	}
}

func TestVerify_Hash_RetriesTransientFailureThenSucceeds(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	want := sha256Hex(content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)

	attempts := 0
	tr := &verifyTransport{remoteHashFunc: func(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
		attempts++
		if attempts < 2 {
			return "", transport.NewError(transport.Transient, "remote_hash", errors.New("connection reset"))
		}
		return want, nil
	}}

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Hash: "sha256"},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Verified) {
		t.Fatalf("state = %q, want %q", out.Record.State, Verified)
	}
	if attempts < 2 {
		t.Fatalf("attempts = %d, want at least 2 (a transient failure must be retried)", attempts)
	}
}

func TestVerify_Hash_DoesNotRetryUnsupportedCapability(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)

	attempts := 0
	tr := &verifyTransport{remoteHashFunc: func(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
		attempts++
		return "", transport.NewError(transport.UnsupportedCapability, "remote_hash", errors.New("no shell"))
	}}

	if _, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Hash: "sha256"},
	}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("RemoteHash called %d times, want exactly 1: retrying an unsupported capability cannot change the answer", attempts)
	}
}

// --- hash lookup cancellation: a stop request, not a verdict ---

func TestVerify_AlreadyCancelledContext_RefusesWithoutTouchingTheJournal(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := &verifyTransport{remoteHashFunc: func(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
		t.Fatal("RemoteHash should not even be attempted against an already-cancelled context")
		return "", nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Verify(ctx, Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Hash: "sha256"},
	})
	if err == nil {
		t.Fatal("Verify succeeded despite an already-cancelled context")
	}
	if len(j.recorded) != 0 {
		t.Fatalf("the journal was written to despite cancellation: %+v", j.recorded)
	}
}

func TestVerify_HashLookupCancelledDuringRetryBackoff_LeavesJournalAtVerifying(t *testing.T) {
	orig := remoteHashRetryPolicy
	remoteHashRetryPolicy = retry.Policy{BaseDelay: 200 * time.Millisecond, MaxDelay: 200 * time.Millisecond, Multiplier: 2}
	t.Cleanup(func() { remoteHashRetryPolicy = orig })

	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)

	started := make(chan struct{}, 1)
	tr := &verifyTransport{remoteHashFunc: func(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		return "", transport.NewError(transport.Transient, "remote_hash", errors.New("connection reset"))
	}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started // wait for the first attempt, then cancel during the backoff wait
		cancel()
	}()

	_, err := Verify(ctx, Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Hash: "sha256"},
	})
	if err == nil {
		t.Fatal("Verify succeeded despite cancellation during retry backoff")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) = false, want true: %v", err)
	}
	if j.currentState() != string(Verifying) {
		t.Fatalf("journal state = %q, want %q: cancellation must not be recorded as a verdict", j.currentState(), Verifying)
	}
	if len(j.recorded) != 0 {
		t.Fatalf("the journal was written to despite cancellation: %+v", j.recorded)
	}
}

// --- layer 3: application validation ---

func TestVerify_Validator_Passes_Verifies(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := &verifyTransport{}

	script := mustScript(t, "exit 0\n")

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Command: &config.Command{Executable: script, Timeout: config.Duration(5 * time.Second)}},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Verified) {
		t.Fatalf("state = %q, want %q", out.Record.State, Verified)
	}
	if out.Record.ValidationPassed == nil || !*out.Record.ValidationPassed {
		t.Fatalf("ValidationPassed = %v, want true", out.Record.ValidationPassed)
	}
}

func TestVerify_Validator_Fails_Quarantines(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := &verifyTransport{}

	script := mustScript(t, "echo \"corrupt: page checksum mismatch\" >&2\nexit 1\n")

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Command: &config.Command{Executable: script, Timeout: config.Duration(5 * time.Second)}},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Quarantined) {
		t.Fatalf("state = %q, want %q", out.Record.State, Quarantined)
	}
	if out.Record.ValidationPassed == nil || *out.Record.ValidationPassed {
		t.Fatalf("ValidationPassed = %v, want false", out.Record.ValidationPassed)
	}
	if !strings.Contains(out.Record.ValidationDetail, "corrupt: page checksum mismatch") {
		t.Fatalf("ValidationDetail = %q, want it to contain the validator's own output", out.Record.ValidationDetail)
	}
}

func TestVerify_Validator_CannotStart_Fails(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := &verifyTransport{}

	missing := filepath.Join(t.TempDir(), "does-not-exist")

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Command: &config.Command{Executable: missing, Timeout: config.Duration(5 * time.Second)}},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Failed) {
		t.Fatalf("state = %q, want %q: a validator that never ran gives no verdict on content", out.Record.State, Failed)
	}
}

func TestVerify_Validator_DoesNotInheritAmbientEnvironment(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := &verifyTransport{}

	t.Setenv("RCLONE_MANAGER_TEST_SECRET", "super-secret-value")

	script := mustScript(t, "echo \"SECRET=$RCLONE_MANAGER_TEST_SECRET\"\necho \"ARTIFACT=$RCLONE_MANAGER_ARTIFACT_PATH\"\necho \"ARG1=$1\"\nexit 1\n")

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Command: &config.Command{Executable: script, Timeout: config.Duration(5 * time.Second)}},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if strings.Contains(out.Record.ValidationDetail, "super-secret-value") {
		t.Fatalf("the validator saw an ambient secret it must never inherit: %q", out.Record.ValidationDetail)
	}
	if !strings.Contains(out.Record.ValidationDetail, "ARTIFACT="+path) {
		t.Fatalf("the validator did not see its artifact path via env: %q", out.Record.ValidationDetail)
	}
	if !strings.Contains(out.Record.ValidationDetail, "ARG1="+path) {
		t.Fatalf("the validator did not see its artifact path via argv[1]: %q", out.Record.ValidationDetail)
	}
}

func TestVerify_Validator_OutputIsBounded(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := &verifyTransport{}

	script := mustScript(t, "yes A | head -c 1000000\nexit 1\n")

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Command: &config.Command{Executable: script, Timeout: config.Duration(5 * time.Second)}},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Quarantined) {
		t.Fatalf("state = %q, want %q", out.Record.State, Quarantined)
	}
	if len(out.Record.ValidationDetail) > maxValidatorOutput+64 {
		t.Fatalf("ValidationDetail length = %d, an untrusted validator's output must be bounded near %d", len(out.Record.ValidationDetail), maxValidatorOutput)
	}
}

// TestVerify_Validator_Timeout_KillsProcess_Quarantines is the flagship
// proof of two FR-13 requirements at once: a required validator that never
// answers must be treated as a failure (fail closed, QUARANTINED), and the
// validator process itself must actually be killed, not merely abandoned.
func TestVerify_Validator_Timeout_KillsProcess_Quarantines(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := &verifyTransport{}

	pidFile := filepath.Join(t.TempDir(), "pid")
	markerFile := filepath.Join(t.TempDir(), "marker")
	script := mustScript(t, fmt.Sprintf("echo $$ > %s\nsleep %d\necho done > %s\n", shQuote(pidFile), int(hookNeverAnswers.Seconds()), shQuote(markerFile)))

	start := time.Now()
	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Command: &config.Command{Executable: script, Timeout: config.Duration(hookTimeoutBudget)}},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if elapsed > hookReturnBudget {
		t.Fatalf("Verify took %s to return; the validator should have been killed well before its %s sleep finished", elapsed, hookNeverAnswers)
	}
	if out.Record.State != string(Quarantined) {
		t.Fatalf("state = %q, want %q: a validator that never answers must fail closed", out.Record.State, Quarantined)
	}
	if !strings.Contains(out.Record.ValidationDetail, "timeout") {
		t.Fatalf("ValidationDetail = %q, want it to mention the timeout", out.Record.ValidationDetail)
	}

	pid := timedOutHookPID(t, pidFile)

	deadline := time.Now().Add(2 * time.Second)
	for {
		killErr := syscall.Kill(pid, 0)
		if errors.Is(killErr, syscall.ESRCH) {
			break // confirmed: the process is actually gone
		}
		if time.Now().After(deadline) {
			t.Fatalf("validator process %d is still alive well after its timeout; it was abandoned, not killed", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := os.Stat(markerFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("marker file exists: the validator ran to completion despite its timeout, so it was not actually killed")
	}
}

// hookTimeoutBudget is the timeout the two "a hook that never answers is
// killed" tests give their script, and it is deliberately not tight.
//
// Both tests prove the same contract: the process really is killed, not
// abandoned. Proving "really killed" needs the script's own pid, which
// means the script has to be forked, scheduled and get one line written
// before the timeout fires. At 200ms it did not, on a machine running
// several full gate runs at once: the fork lost the race, the pid file
// never appeared, and the test failed on a missing file rather than on
// anything about the behaviour it exists to prove (issue #377).
//
// Nothing about the contract needs the budget to be small. It has to be
// short enough that the test is quick and long enough that a fork cannot
// lose, and every assertion downstream is what actually carries the
// meaning: the call has to return far sooner than the script's own sleep
// would allow, the script's process has to be gone afterwards, and its
// completion marker must not exist. Those hold at any budget with a
// margin under hookNeverAnswers.
const hookTimeoutBudget = 2 * time.Second

// hookNeverAnswers is how long the test script sleeps for. It is an order
// of magnitude above hookTimeoutBudget so that "returned before the sleep
// finished" cannot be true by accident on a slow machine.
const hookNeverAnswers = 30 * time.Second

// hookReapBackstop mirrors the c.WaitDelay both implementations set
// (restorecheck.go and verify.go, "c.WaitDelay = 5 * time.Second"). It is
// Go's own backstop, not this project's: os/exec kills the child that long
// after the context is done regardless of what c.Cancel did. Nothing reads
// it from the production code, so if that value ever changes this constant
// has to change with it, which is what TestHookTimeoutBudgets_CanStillFail
// below is for.
const hookReapBackstop = 5 * time.Second

// hookReturnBudget bounds how long the call under test may take, and it is
// the only assertion in either of these two tests that can separate a hook
// that was killed from one that was merely abandoned. That is not obvious,
// and it is worth writing down because a reasonable-looking number here
// makes both tests vacuous.
//
// The pid read below looks like the proof that the process was killed. It
// is not. Wait does not return until hookReapBackstop has done its own
// killing, so by the time any check runs the process really is gone and
// the marker really was never written, whether or not c.Cancel killed
// anything. Those assertions pass either way.
//
// The elapsed time is what tells them apart, and the two regimes are far
// apart and both insensitive to load, because each is set by a deadline
// rather than by the scheduler. A killed hook returns at about
// hookTimeoutBudget. An abandoned one returns at hookTimeoutBudget plus
// hookReapBackstop. So the bound goes strictly between them, and it is
// derived from the budget rather than written out, so the two cannot drift
// apart: three seconds above a correct run, two below the backstop.
//
// I checked both directions rather than reasoning about them. With the
// SIGKILL taken out of c.Cancel in both implementations and nothing else
// changed, both tests return in 7.00s: at the old 10s budget they passed,
// and at this one they fail on this line. With the real implementation
// they return in 2.00s.
//
// Found by the author of #382, who reached these two tests from the other
// direction (issue #371, a cold exec on macOS costing more than the old
// 200ms budget) and checked it against this branch.
const hookReturnBudget = hookTimeoutBudget + 3*time.Second

// pidFileWait is how long timedOutHookPID waits for the killed script's
// pid to become readable. It starts after the call under test has already
// returned, so the script has had its whole hookTimeoutBudget to run
// before this wait even begins; this is only for the write becoming
// visible, not for the script starting.
const pidFileWait = 2 * time.Second

// timedOutHookPID reads the pid the killed script wrote. It polls rather
// than reading once: the write happens on the child's own schedule, and a
// filesystem that has not made it visible yet is not the same thing as a
// script that never started. The failure message names the difference,
// because that is what took the longest to work out the first time.
func timedOutHookPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(pidFileWait)
	for {
		pidBytes, err := os.ReadFile(pidFile)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
			if convErr == nil {
				return pid
			}
			if time.Now().After(deadline) {
				t.Fatalf("pid file holds %q, which is not a pid: %v", string(pidBytes), convErr)
			}
		} else if time.Now().After(deadline) {
			t.Fatalf("no pid file %s after the call returned, and the script had %s to write one before that: it never got far enough, which means it was killed before it started rather than while it was hanging. On a loaded machine that is a fork losing a race with the timeout, not a defect in the code under test (issue #377): %v",
				pidFileWait, hookTimeoutBudget, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// shQuote is a minimal single-quote wrap for the paths this test file
// interpolates into a generated shell script. t.TempDir() paths on the
// platforms this project targets never contain a single quote, so this
// only needs to produce syntactically valid sh, not defend against
// adversarial input.
func shQuote(s string) string { return "'" + s + "'" }

// --- the flagship proof: a required validator's failure blocks source deletion ---

// TestFailingValidatorBlocksSourceDeletion drives a real *state.Journal
// (the same package production code uses, not a fake) through the nominal
// path from DISCOVERED to VERIFYING, runs Verify with a validator that
// rejects the artifact, and then proves the resulting QUARANTINED state
// structurally forecloses ever reaching a state Transport.DeleteRemote
// could be called from (Committed, RemoteDeletePending) without the whole
// pipeline running again from DISCOVERED.
func TestFailingValidatorBlocksSourceDeletion(t *testing.T) {
	ctx := context.Background()
	j, err := state.Open(ctx, filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = j.Close() }()

	artifact := testArtifact(t)
	content := []byte("dump-bytes")
	localPath := verifyWriteLocalFile(t, content)

	if _, err := j.Discover(ctx, artifact, "attempt-1:discover", "backups/backup.dump", state.RemoteIdentity{}, time.Now()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, err := Advance(ctx, Deps{Journal: j}, state.Transition{
		Artifact: artifact, Key: "attempt-1:transferring", From: string(Discovered), To: string(Transferring),
		LocalPath: &localPath,
	}); err != nil {
		t.Fatalf("Advance to Transferring: %v", err)
	}
	if _, err := Advance(ctx, Deps{Journal: j}, state.Transition{
		Artifact: artifact, Key: "attempt-1:transferred", From: string(Transferring), To: string(Transferred),
		Transfer: &state.TransferResult{BytesTransferred: int64(len(content))},
	}); err != nil {
		t.Fatalf("Advance to Transferred: %v", err)
	}
	if _, err := Advance(ctx, Deps{Journal: j}, state.Transition{
		Artifact: artifact, Key: "attempt-1:verifying", From: string(Transferred), To: string(Verifying),
	}); err != nil {
		t.Fatalf("Advance to Verifying: %v", err)
	}

	script := mustScript(t, "echo \"pg_restore --list: archive header checksum mismatch\" >&2\nexit 1\n")

	out, err := Verify(ctx, Deps{Journal: j, Transport: &verifyTransport{}}, VerifyParams{
		Artifact:   artifact,
		AttemptKey: "attempt-1",
		Validation: config.Validation{Command: &config.Command{Executable: script, Timeout: config.Duration(5 * time.Second)}},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Quarantined) {
		t.Fatalf("state = %q, want %q", out.Record.State, Quarantined)
	}

	// Structural proof #1: Quarantined reaches exactly three states, and
	// none is REMOTE_DELETE_PENDING or COMPLETE. DISCOVERED re-ingests from
	// scratch; COMMITTED and REMOTE_RETAINED (issue #315) are the two
	// operator-only reinstatement targets issue #220 and #315 add, which
	// proofs #2 and #3 below show cannot help this artifact reach a delete
	// either.
	assertStateSet(t, "Successors(Quarantined)", Successors(Quarantined), Discovered, Committed, RemoteRetained)

	// Structural proof #2: Advance itself, backed by the real journal,
	// refuses every attempt to move straight from Quarantined to a
	// delete-eligible or later state, and leaves the journal untouched
	// when it does.
	for _, illegal := range []State{Verified, Committing, RemoteDeletePending, Complete} {
		if _, err := Advance(ctx, Deps{Journal: j}, state.Transition{
			Artifact: artifact,
			Key:      "attempt-1:illegal:" + string(illegal),
			From:     string(Quarantined),
			To:       string(illegal),
		}); err == nil {
			t.Fatalf("Advance allowed QUARANTINED -> %s; a failing validator must not reach a delete-eligible state directly", illegal)
		}
	}

	// Structural proof #3: the one edge that does exist refuses this
	// artifact by name. It was quarantined out of VERIFYING, so it never
	// durably committed at all, and the transition log is what says so.
	if _, err := ReinstateFromQuarantine(ctx, Deps{Journal: j}, QuarantineReinstateParams{
		Artifact:   artifact,
		AttemptKey: "attempt-1:reinstate",
		Evidence:   ReinstatementEvidence{ValidatorPassed: true, Summary: "a second validator run passed"},
	}); err == nil {
		t.Fatal("ReinstateFromQuarantine promoted an artifact the validator rejected before it ever committed")
	} else if _, ok := AsNeverHeldTargetState(err); !ok {
		t.Fatalf("err = %v, want a *NeverHeldTargetStateError", err)
	}

	rec, err := j.Get(ctx, artifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.State != string(Quarantined) {
		t.Fatalf("journal state = %q after the refused Advance attempts, want it to still be %q", rec.State, Quarantined)
	}

	// Structural proof #4, and the one that makes FR-13's guarantee
	// survive the new edge no matter what any caller does: force the
	// artifact onto COMMITTED with a bare Advance, bypassing
	// ReinstateFromQuarantine and every rule it enforces, and the remote
	// delete is STILL refused. The refusal comes from the transition log,
	// which now records a QUARANTINED -> COMMITTED edge, so it does not
	// depend on the writer having gone through the sanctioned entry point.
	if _, err := Advance(ctx, Deps{Journal: j}, state.Transition{
		Artifact: artifact,
		Key:      "attempt-1:forced-committed",
		From:     string(Quarantined),
		To:       string(Committed),
	}); err != nil {
		t.Fatalf("forcing QUARANTINED -> COMMITTED: %v", err)
	}
	tp := &deleteTransport{}
	_, err = DeleteRemote(ctx, Deps{Journal: j, Transport: tp}, DeleteRemoteRequest{
		Artifact:           artifact,
		AttemptKey:         "attempt-1:delete",
		CompletionStrategy: "rename",
	})
	_ = requireRefusal(t, err, reinstatementCheck)
	if tp.deleteCalls != 0 {
		t.Fatalf("transport.DeleteRemote called %d times, want 0: a required validator failure must prevent source deletion (FR-13)", tp.deleteCalls)
	}
}

func TestVerify_SameAttemptKey_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	j, err := state.Open(ctx, filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = j.Close() }()

	artifact := testArtifact(t)
	content := []byte("dump-bytes")
	localPath := verifyWriteLocalFile(t, content)

	if _, err := j.Discover(ctx, artifact, "a1:discover", "backups/x", state.RemoteIdentity{}, time.Now()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, err := Advance(ctx, Deps{Journal: j}, state.Transition{
		Artifact: artifact, Key: "a1:transferring", From: string(Discovered), To: string(Transferring), LocalPath: &localPath,
	}); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if _, err := Advance(ctx, Deps{Journal: j}, state.Transition{
		Artifact: artifact, Key: "a1:transferred", From: string(Transferring), To: string(Transferred),
		Transfer: &state.TransferResult{BytesTransferred: int64(len(content))},
	}); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if _, err := Advance(ctx, Deps{Journal: j}, state.Transition{
		Artifact: artifact, Key: "a1:verifying", From: string(Transferred), To: string(Verifying),
	}); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	deps := Deps{Journal: j, Transport: &verifyTransport{}}
	params := VerifyParams{Artifact: artifact, AttemptKey: "a1"}

	out1, err := Verify(ctx, deps, params)
	if err != nil {
		t.Fatalf("Verify (first): %v", err)
	}
	if !out1.Applied {
		t.Fatal("the first Verify call should have applied the transition")
	}

	out2, err := Verify(ctx, deps, params)
	if err != nil {
		t.Fatalf("Verify (second): %v", err)
	}
	if out2.Applied {
		t.Fatal("a second Verify call with the same AttemptKey should be recognised as a replay, not applied again")
	}
	if out2.Record.State != out1.Record.State {
		t.Fatalf("replay state = %q, want %q", out2.Record.State, out1.Record.State)
	}
}

// --- input validation ---

func TestVerify_RejectsMissingRequiredParams(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := &verifyTransport{}

	cases := []struct {
		name string
		deps Deps
		p    VerifyParams
	}{
		{"no journal", Deps{Transport: tr}, VerifyParams{Artifact: rec.Artifact, AttemptKey: "a"}},
		{"no transport", Deps{Journal: j}, VerifyParams{Artifact: rec.Artifact, AttemptKey: "a"}},
		{"no attempt key", Deps{Journal: j, Transport: tr}, VerifyParams{Artifact: rec.Artifact}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Verify(context.Background(), c.deps, c.p); err == nil {
				t.Fatalf("Verify accepted %s", c.name)
			}
		})
	}
}

// TestVerify_ValidatorIDWithoutAResolvedCommand_FailsClosed is issue
// #162's fail-closed guard on the seam it introduces. A backup set names
// its validator by id in config.yaml, and core/service resolves that id
// into Validation.Command at load time. If some path ever reaches Verify
// with the id still set and the Command still nil -- a constructor that
// skipped resolution, a hot reload that forgot it -- the honest answer is
// a refusal, not "no validator was configured, carry on": the operator
// asked for one, and silently transferring and deleting the remote
// source without it is the exact outcome FR-13 exists to prevent.
func TestVerify_ValidatorIDWithoutAResolvedCommand_FailsClosed(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := &verifyTransport{}

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{ValidatorID: "trailer-marker"},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Failed) {
		t.Fatalf("state = %q, want %q: a configured validator that was never resolved must never read as \"no validator configured\"", out.Record.State, Failed)
	}
	if len(j.recorded) == 0 {
		t.Fatal("no transition was recorded")
	}
	detail := j.recorded[len(j.recorded)-1].Detail
	if !strings.Contains(detail, "trailer-marker") {
		t.Fatalf("transition detail = %q, want it to name the unresolved validator id", detail)
	}
}

// TestVerify_NoValidatorIDAndNoCommand_StillVerifies is that guard's
// positive control: the ordinary "this backup set has no application
// validator" case must be untouched by it, or the test above would pass
// for a version of decide that refused every artifact.
func TestVerify_NoValidatorIDAndNoCommand_StillVerifies(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := &verifyTransport{}

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Verified) {
		t.Fatalf("state = %q, want %q", out.Record.State, Verified)
	}
}

// TestHookTimeoutBudgets_CanStillFail is a guard on the constants above
// rather than on any behaviour, because the way these two tests stop
// working is not a broken assertion, it is a budget quietly widened past
// the point where it can fail. At hookTimeoutBudget + hookReapBackstop or
// beyond, a hook that c.Cancel never killed returns inside the budget and
// every other assertion in both tests passes, so both go green against
// exactly the defect they are named for. This is the check that says so
// out loud instead of leaving it to whoever next finds one of them flaky.
func TestHookTimeoutBudgets_CanStillFail(t *testing.T) {
	abandoned := hookTimeoutBudget + hookReapBackstop
	if hookReturnBudget >= abandoned {
		t.Fatalf("hookReturnBudget is %s, but a hook that was abandoned rather than killed returns at %s (hookTimeoutBudget %s + hookReapBackstop %s). At this budget both timeout tests pass against an implementation whose c.Cancel kills nothing, which is the one thing they exist to prove. Lower it back under %s.",
			hookReturnBudget, abandoned, hookTimeoutBudget, hookReapBackstop, abandoned)
	}
	if hookReturnBudget <= hookTimeoutBudget {
		t.Fatalf("hookReturnBudget is %s, which is not above hookTimeoutBudget %s: a correct kill returns at about the budget, so this would fail on every run", hookReturnBudget, hookTimeoutBudget)
	}
	if hookNeverAnswers <= abandoned {
		t.Fatalf("hookNeverAnswers is %s, which is not clear of %s: the script has to still be sleeping when both the kill and the backstop would have fired, or \"returned before the sleep finished\" is true by accident", hookNeverAnswers, abandoned)
	}
}

// TestVerify_RemoteHashConnectTimeout_IsNeverReadAsACancellation is #388's
// half of the story, kept as its own control now that #419 changed what
// happens NEXT. The classification is the thing under test here: a connect
// timeout rclone imposed on itself must not read as a stop request just
// because context.DeadlineExceeded is still reachable underneath it. What
// the step then DOES with that transient failure is stall_test.go's
// subject, so this one asserts only that Verify neither returned it as a
// cancellation nor left the journal untouched and silent.
func TestVerify_RemoteHashConnectTimeout_IsNeverReadAsACancellation(t *testing.T) {
	orig := remoteHashRetryPolicy
	remoteHashRetryPolicy = retry.Policy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	t.Cleanup(func() { remoteHashRetryPolicy = orig })

	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)

	connectTimeout := transport.NewError(transport.Transient, "remote_hash",
		fmt.Errorf(`source "prod": NewFs: couldn't connect SSH: dial tcp 192.0.2.1:22: %w`, context.DeadlineExceeded))
	tr := &verifyTransport{remoteHashFunc: func(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
		return "", connectTimeout
	}}

	// The precondition that makes this test necessary rather than
	// hypothetical: the cause survives classification, so a raw errors.Is
	// still finds it.
	if !errors.Is(connectTimeout, context.DeadlineExceeded) {
		t.Fatal("this error no longer carries context.DeadlineExceeded, so it cannot exercise the confusion this test exists for")
	}

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Hash: "sha256"},
	})
	if err != nil {
		t.Fatalf("Verify returned an error, which is how it reports a cancellation, for a connect timeout nobody asked for: %v", err)
	}
	if out.Record.State == string(Verifying) {
		t.Fatal("the journal was left at VERIFYING with nothing recorded, which is how a cancellation is handled")
	}
	recorded := j.transitionsTo(State(out.Record.State))
	if len(recorded) != 1 {
		t.Fatalf("recorded %d transitions to %s, want 1", len(recorded), out.Record.State)
	}
	if !strings.Contains(recorded[0].Detail, "transient") {
		t.Fatalf("detail = %q, want it to name the transient category it actually got", recorded[0].Detail)
	}
}

package lifecycle

// Issue #419. A connect timeout rclone imposed on itself classifies as
// Transient since #408, which is right, and the consequence was that
// exhausting the retry budget on it recorded FAILED: a state nothing in
// this product has a route out of. These tests pin the rule this package
// now follows instead, which internal/revalidate had already written down
// one package over: a backend that could not be ASKED has said nothing
// about the artifact, so it is not a verdict about the artifact.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/retry"
)

// shortRemoteHashRetries makes the in-call retry budget cheap, so a test
// that wants to reach exhaustion does not sleep through the real schedule.
func shortRemoteHashRetries(t *testing.T) {
	t.Helper()
	orig := remoteHashRetryPolicy
	remoteHashRetryPolicy = retry.Policy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	t.Cleanup(func() { remoteHashRetryPolicy = orig })
}

// blackholedConnectTimeout is the error #388 measured: rclone's own
// --contimeout firing, classified Transient, with context.DeadlineExceeded
// still reachable underneath it.
func blackholedConnectTimeout() error {
	return transport.NewError(transport.Transient, "remote_hash",
		fmt.Errorf(`source "prod": NewFs: couldn't connect SSH: dial tcp 192.0.2.1:22: %w`, context.DeadlineExceeded))
}

func alwaysFailingHash(err error) *verifyTransport {
	return &verifyTransport{remoteHashFunc: func(context.Context, transport.Source, string, transport.HashAlgorithm) (string, error) {
		return "", err
	}}
}

// TestVerify_ConnectTimeout_StallsAtVerifyingInsteadOfRecordingAVerdict is
// the heart of #419: with a stall budget the artifact stays exactly where
// it honestly is, and the caller is told why, rather than being given a
// verdict nobody measured.
func TestVerify_ConnectTimeout_StallsAtVerifyingInsteadOfRecordingAVerdict(t *testing.T) {
	shortRemoteHashRetries(t)

	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := alwaysFailingHash(blackholedConnectTimeout())

	_, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation:  config.Validation{Hash: "sha256"},
		StallBudget: 3,
	})
	if err == nil {
		t.Fatal("Verify reported success for a check it could not complete")
	}

	var stall *VerificationStalledError
	if !errors.As(err, &stall) {
		t.Fatalf("err = %v, want a *VerificationStalledError so a caller can tell an unfinished check from a verdict", err)
	}
	if stall.Attempt != 1 || stall.Budget != 3 {
		t.Fatalf("stall = attempt %d of %d, want attempt 1 of 3", stall.Attempt, stall.Budget)
	}
	if category, ok := transport.CategoryOf(err); !ok || category != transport.Transient {
		t.Fatalf("category = %v (ok=%t), want transient: the stall must carry the category #408 established, not swallow it", category, ok)
	}

	if j.currentState() != string(Verifying) {
		t.Fatalf("journal state = %q, want %q: an unreachable backend is not evidence about the artifact", j.currentState(), Verifying)
	}
	if got := j.transitionsTo(Failed); len(got) != 0 {
		t.Fatalf("recorded %d FAILED transitions, want none: %+v", len(got), got)
	}
	if got := j.transitionsTo(Quarantined); len(got) != 0 {
		t.Fatalf("recorded %d QUARANTINED transitions on the first stall, want none: %+v", len(got), got)
	}

	// The stall is durable, not merely returned: a count that lives only in
	// the returned error is a bound that resets every time the process does.
	if j.rec.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1: the stall has to be recorded or nothing bounds it", j.rec.RetryCount)
	}
	if !strings.Contains(strings.ToLower(j.rec.LastError), "transient") {
		t.Fatalf("LastError = %q, want it to name the category that stalled the check", j.rec.LastError)
	}
}

// TestVerify_ConnectTimeout_BudgetSpent_QuarantinesRatherThanStranding is
// the bound. It is spent rather than reset: the artifact stops being
// retried automatically and lands in the one state that has operator
// actions behind it, including a route back into the pipeline.
func TestVerify_ConnectTimeout_BudgetSpent_QuarantinesRatherThanStranding(t *testing.T) {
	shortRemoteHashRetries(t)

	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	rec.RetryCount = 2 // two stalls already spent against a budget of three
	j := newVerifyJournal(rec)
	tr := alwaysFailingHash(blackholedConnectTimeout())

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation:  config.Validation{Hash: "sha256"},
		StallBudget: 3,
	})
	if err != nil {
		t.Fatalf("Verify returned an error for a recorded business outcome: %v", err)
	}
	if out.Record.State != string(Quarantined) {
		t.Fatalf("state = %q, want %q: FAILED has no route back, which is what #419 is about", out.Record.State, Quarantined)
	}
	if got := j.transitionsTo(Failed); len(got) != 0 {
		t.Fatalf("recorded %d FAILED transitions, want none: %+v", len(got), got)
	}

	quarantines := j.transitionsTo(Quarantined)
	if len(quarantines) != 1 {
		t.Fatalf("recorded %d QUARANTINED transitions, want 1", len(quarantines))
	}
	detail := quarantines[0].Detail
	for _, want := range []string{"transient", "3"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("QUARANTINED detail = %q, want it to name %q", detail, want)
		}
	}

	// Nothing here looked at the artifact's bytes against the remote, so
	// nothing here may leave a hash on the row claiming it did: that is
	// the field QuarantineReason reads to decide whether to say "a content
	// check failed", and this artifact never had one.
	if j.rec.LocalHash != "" {
		t.Fatalf("LocalHash = %q, want empty: a check that never ran must not leave evidence that it did", j.rec.LocalHash)
	}
	reason := QuarantineReason(j.rec)
	for _, forbidden := range []string{"content check", "content found invalid"} {
		if strings.Contains(reason, forbidden) {
			t.Fatalf("QuarantineReason = %q, want it to name the unreachable backend rather than %q, which nobody ran", reason, forbidden)
		}
	}
	if !strings.Contains(reason, "192.0.2.1") {
		t.Fatalf("QuarantineReason = %q, want it to carry what actually stopped the check", reason)
	}
}

// TestVerify_NoStallBudget_QuarantinesOnTheFirstExhaustion is the default
// every caller that has not opted into stall tolerance gets. It is still
// QUARANTINED rather than FAILED, because which state a caller lands in is
// this package's decision and not the caller's.
func TestVerify_NoStallBudget_QuarantinesOnTheFirstExhaustion(t *testing.T) {
	shortRemoteHashRetries(t)

	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := alwaysFailingHash(blackholedConnectTimeout())

	out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation: config.Validation{Hash: "sha256"},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Record.State != string(Quarantined) {
		t.Fatalf("state = %q, want %q", out.Record.State, Quarantined)
	}
}

// TestVerify_NonRetryableHashFailure_StillFails is the positive control
// that keeps the change narrow. A backend that ANSWERED, and answered that
// it cannot do this, is a fixed property of the deployment: retrying it
// cannot change it and stalling on it would hide it. That is FAILED's
// documented meaning and it is untouched, budget or no budget.
func TestVerify_NonRetryableHashFailure_StillFails(t *testing.T) {
	for _, tc := range []struct {
		name     string
		category transport.Category
	}{
		{"unsupported capability", transport.UnsupportedCapability},
		{"authentication", transport.Authentication},
		{"not found", transport.NotFound},
		{"permanent", transport.Permanent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := []byte("dump-bytes")
			path := verifyWriteLocalFile(t, content)
			rec := verifyingRecord(t, path, int64(len(content)))
			j := newVerifyJournal(rec)
			tr := alwaysFailingHash(transport.NewError(tc.category, "remote_hash", errors.New("no")))

			out, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
				Artifact: rec.Artifact, AttemptKey: "a1",
				Validation:  config.Validation{Hash: "sha256"},
				StallBudget: 5,
			})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if out.Record.State != string(Failed) {
				t.Fatalf("state = %q, want %q: a backend that answered is a verdict", out.Record.State, Failed)
			}
			if j.rec.RetryCount != 0 {
				t.Fatalf("RetryCount = %d, want 0: a permanent answer must not consume the stall budget", j.rec.RetryCount)
			}
		})
	}
}

// TestVerify_StallIsIdempotentAcrossACrash proves the stall write is keyed
// the way every other write in this package is: the same logical attempt
// replayed after a crash records once, not twice, so a process that dies
// between the journal write and the return does not consume two of the
// artifact's attempts for one outage.
func TestVerify_StallIsIdempotentAcrossACrash(t *testing.T) {
	shortRemoteHashRetries(t)

	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := alwaysFailingHash(blackholedConnectTimeout())

	params := VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation:  config.Validation{Hash: "sha256"},
		StallBudget: 4,
	}
	for i := 0; i < 2; i++ {
		if _, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, params); err == nil {
			t.Fatalf("call %d: Verify reported success for a check it could not complete", i+1)
		}
	}
	if j.rec.RetryCount != 1 {
		t.Fatalf("RetryCount = %d after replaying one attempt, want 1", j.rec.RetryCount)
	}
}

// TestVerify_CancellationIsStillNeverAStall keeps #388's ordering honest.
// A transport.Error keeps its cause reachable, so a stall carrying
// context.DeadlineExceeded underneath must not be mistaken for the caller
// having asked to stop, and a genuine cancellation must not be recorded as
// a stall against the artifact's budget.
func TestVerify_CancellationIsStillNeverAStall(t *testing.T) {
	content := []byte("dump-bytes")
	path := verifyWriteLocalFile(t, content)
	rec := verifyingRecord(t, path, int64(len(content)))
	j := newVerifyJournal(rec)
	tr := alwaysFailingHash(transport.NewError(transport.Cancelled, "remote_hash", context.Canceled))

	_, err := Verify(context.Background(), Deps{Journal: j, Transport: tr}, VerifyParams{
		Artifact: rec.Artifact, AttemptKey: "a1",
		Validation:  config.Validation{Hash: "sha256"},
		StallBudget: 4,
	})
	if err == nil {
		t.Fatal("Verify reported success for a cancelled check")
	}
	var stall *VerificationStalledError
	if errors.As(err, &stall) {
		t.Fatalf("a cancellation was recorded as a stall: %v", err)
	}
	if len(j.recorded) != 0 {
		t.Fatalf("the journal was written to for a cancellation: %+v", j.recorded)
	}
}

// TestQuarantineReason_NamesAnUnfinishedCheckRatherThanAContentFailure is
// the surface half. QuarantineReason is what the CLI, the API and the UI
// all read for "why is this here", and #419's new shape has to arrive
// there as what it is: nobody found anything wrong with these bytes.
func TestQuarantineReason_NamesAnUnfinishedCheckRatherThanAContentFailure(t *testing.T) {
	rec := state.Record{
		State:      string(Quarantined),
		RetryCount: 3,
		LastError:  "verification could not be completed: transient: dial tcp 192.0.2.1:22: i/o timeout",
	}
	reason := QuarantineReason(rec)
	for _, forbidden := range []string{"content check", "content found invalid"} {
		if strings.Contains(reason, forbidden) {
			t.Fatalf("QuarantineReason = %q, want it not to claim the %q verdict nobody reached", reason, forbidden)
		}
	}
	if !strings.Contains(reason, "192.0.2.1") {
		t.Fatalf("QuarantineReason = %q, want it to carry the recorded reason", reason)
	}
	if !strings.Contains(reason, "3") {
		t.Fatalf("QuarantineReason = %q, want it to say how many attempts were spent", reason)
	}
}

// TestQuarantineReason_StillPrefersRealEvidence is the control for the
// branch above: a row that DOES carry a content verdict keeps reporting
// it, so the new branch cannot mask a real finding behind whatever the
// retry bookkeeping last wrote.
func TestQuarantineReason_StillPrefersRealEvidence(t *testing.T) {
	rejected := false
	withValidator := state.Record{
		State:            string(Quarantined),
		RetryCount:       3,
		LastError:        "verification could not be completed: transient",
		ValidationPassed: &rejected,
		ValidationDetail: "pg_restore --list said no",
	}
	if reason := QuarantineReason(withValidator); !strings.Contains(reason, "validator") {
		t.Fatalf("QuarantineReason = %q, want the validator's own rejection", reason)
	}

	withHash := state.Record{
		State:        string(Quarantined),
		RetryCount:   3,
		LastError:    "verification could not be completed: transient",
		LocalHash:    "abc123",
		LocalHashAlg: "sha256",
	}
	if reason := QuarantineReason(withHash); !strings.Contains(reason, "content check") {
		t.Fatalf("QuarantineReason = %q, want the content check it really recorded", reason)
	}
}

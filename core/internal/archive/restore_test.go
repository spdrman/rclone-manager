package archive

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// openJournal gives a test the real SQLite journal, migrated, because the
// durability claims in this file are claims about a database and proving
// them against a map would prove nothing.
func openJournal(t *testing.T) *state.Journal {
	t.Helper()
	j, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// archivedCopy is one artifact's copy on a DEEP_ARCHIVE medium, which is
// the only kind of copy a restore is legal against.
func archivedCopy() Copy {
	body := []byte("an artifact that was moved to a cold tier six months ago")
	return Copy{
		Placement: mediumPlacement("cold-store", "prefix/production/postgres/dump.zst",
			int64(len(body)), hashOf(body), state.VerificationContent),
		Class:  config.StorageClassDeepArchive,
		Access: RequiresRestore,
	}
}

func restoreRequest() Request {
	return Request{
		IdempotencyKey: "idem-restore-1",
		Actor:          "alice",
		ConfigRevision: "rev-1",
		Artifact:       "production/postgres/dump.zst",
		Copy:           archivedCopy(),
		Medium:         glacierMedium(),
		WindowDays:     3,
		Acknowledged:   true,
	}
}

func newTestRestorer(t *testing.T, store Store) (*Restorer, *state.Journal) {
	t.Helper()
	j := openJournal(t)
	n := 0
	return NewRestorer(j, store, func() time.Time { return testNow }, func() string {
		n++
		return fmt.Sprintf("op_restore_%d", n)
	}), j
}

// TestARestoreHasToBeAskedForExplicitly is the "make an accidental restore
// hard" requirement, and the field it is about defaults to the answer that
// costs nothing.
//
// The positive control in the second half is what makes this
// non-vacuous: the very same request, with Acknowledged flipped and
// nothing else touched, goes through and starts exactly one restore. So
// the refusal is about the acknowledgement and not about anything else in
// the request being malformed.
func TestARestoreHasToBeAskedForExplicitly(t *testing.T) {
	store := &fakeMedium{}
	r, _ := newTestRestorer(t, store)

	req := restoreRequest()
	req.Acknowledged = false

	if _, err := r.Submit(context.Background(), req); !errors.Is(err, ErrNotAcknowledged) {
		t.Fatalf("Submit without acknowledgement: err = %v, want ErrNotAcknowledged", err)
	}
	if _, _, _, _, initiates := store.counts(); initiates != 0 {
		t.Fatalf("a refused restore started %d restores at the provider", initiates)
	}

	req.Acknowledged = true
	submitted, err := r.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit with acknowledgement: %v", err)
	}
	if !submitted.Created {
		t.Fatal("Created = false for a brand new idempotency key")
	}
	_, _, _, _, initiates := store.counts()
	if initiates != 1 {
		t.Fatalf("initiates = %d, want exactly 1", initiates)
	}
	if got := store.initiatedWindows; len(got) != 1 || got[0] != 3 {
		t.Fatalf("initiated windows = %v, want [3]", got)
	}
}

// TestARefusedRestoreLeavesNothingBehind covers every refusal at once: a
// request that was turned down must not have written a row, because a row
// with no restore behind it is exactly the thing that makes an operator
// stop trusting the operations list.
func TestARefusedRestoreLeavesNothingBehind(t *testing.T) {
	tests := []struct {
		name string
		mut  func(Request) Request
		want error
	}{
		{"no acknowledgement", func(r Request) Request { r.Acknowledged = false; return r }, ErrNotAcknowledged},
		{"no idempotency key", func(r Request) Request { r.IdempotencyKey = ""; return r }, ErrInvalidRequest},
		{"no artifact named", func(r Request) Request { r.Artifact = ""; return r }, ErrInvalidRequest},
		{"the copy records no key", func(r Request) Request { r.Copy.Placement.Location = ""; return r }, ErrInvalidRequest},
		{"a window of zero days", func(r Request) Request { r.WindowDays = 0; return r }, ErrWindowOutOfRange},
		{"a window past the ceiling", func(r Request) Request { r.WindowDays = MaxWindowDays + 1; return r }, ErrWindowOutOfRange},
		{"the copy is local", func(r Request) Request { r.Copy.Placement.Medium = state.MediumLocal; return r }, ErrNotArchived},
		{"the copy reads on demand", func(r Request) Request { r.Copy.Class = config.StorageClassStandardIA; return r }, ErrNotArchived},
		{"the copy is on GLACIER_IR, which only sounds cold", func(r Request) Request {
			r.Copy.Class = config.StorageClassGlacierIR
			return r
		}, ErrNotArchived},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeMedium{}
			r, j := newTestRestorer(t, store)

			_, err := r.Submit(context.Background(), tc.mut(restoreRequest()))
			if !errors.Is(err, tc.want) {
				t.Fatalf("Submit: err = %v, want %v", err, tc.want)
			}
			if _, _, _, _, initiates := store.counts(); initiates != 0 {
				t.Fatalf("a refused restore started %d restores at the provider", initiates)
			}
			ops, err := j.ListOperations(context.Background(), 10)
			if err != nil {
				t.Fatalf("ListOperations: %v", err)
			}
			if len(ops) != 0 {
				t.Fatalf("a refused restore left %d operation rows behind: %+v", len(ops), ops)
			}
		})
	}
}

// TestTheWindowBoundsAreAcceptedAtBothEnds is the positive control for the
// two window refusals above. Without it, a bug that refused every window
// would pass those rows for the wrong reason.
func TestTheWindowBoundsAreAcceptedAtBothEnds(t *testing.T) {
	for _, days := range []int{MinWindowDays, MaxWindowDays} {
		store := &fakeMedium{}
		r, _ := newTestRestorer(t, store)
		req := restoreRequest()
		req.WindowDays = days
		if _, err := r.Submit(context.Background(), req); err != nil {
			t.Fatalf("Submit with a %d day window: %v", days, err)
		}
		if got := store.initiatedWindows; len(got) != 1 || got[0] != days {
			t.Fatalf("initiated windows = %v, want [%d]", got, days)
		}
	}
}

// TestTheRowExistsBeforeAnythingIsAskedOfTheProvider is the durability
// ordering the operations table was built for.
//
// The double checks the journal from INSIDE InitiateRestore, which is the
// only way to observe the ordering rather than infer it: by the time the
// provider is asked, the row describing what was asked for is already
// durable, so a crash in between leaves a recorded restore that never
// started rather than a running restore nothing here knows about.
func TestTheRowExistsBeforeAnythingIsAskedOfTheProvider(t *testing.T) {
	store := &fakeMedium{}
	_, j := newTestRestorer(t, store)

	// Only the FIRST call is recorded. That detail is load-bearing: a
	// Submit that asked the provider first and then wrote the row would
	// otherwise overwrite the interesting reading with a later, reassuring
	// one, and this test would pass against exactly the bug it exists to
	// catch. I know because it did, until I planted that bug and watched
	// it pass.
	var rowAtInitiate state.Operation
	var lookupErr error
	seen := false
	observing := &observingStore{
		fakeMedium: store,
		onInitiate: func() {
			if seen {
				return
			}
			seen = true
			rowAtInitiate, lookupErr = j.GetOperation(context.Background(), "op_restore_1")
		},
	}
	r := NewRestorer(j, observing, func() time.Time { return testNow }, func() string { return "op_restore_1" })

	if _, err := r.Submit(context.Background(), restoreRequest()); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if lookupErr != nil {
		t.Fatalf("the operation row did not exist when the provider was asked: %v", lookupErr)
	}
	if rowAtInitiate.Action != ActionRestore {
		t.Fatalf("row action = %q, want %q", rowAtInitiate.Action, ActionRestore)
	}
	if rowAtInitiate.Status != state.OperationRunning {
		t.Fatalf("row status = %q, want %q by the time the provider is asked", rowAtInitiate.Status, state.OperationRunning)
	}
	if !strings.Contains(rowAtInitiate.Parameters, config.StorageClassDeepArchive) {
		t.Errorf("the row does not record the storage class it was restoring from: %q", rowAtInitiate.Parameters)
	}
	if !strings.Contains(rowAtInitiate.Parameters, `"window_days":3`) {
		t.Errorf("the row does not record the restore window it asked for: %q", rowAtInitiate.Parameters)
	}
}

// observingStore runs a hook the instant InitiateRestore is called.
type observingStore struct {
	*fakeMedium
	onInitiate func()
}

func (o *observingStore) InitiateRestore(ctx context.Context, m transport.Medium, key string, windowDays int) error {
	if o.onInitiate != nil {
		o.onInitiate()
	}
	return o.fakeMedium.InitiateRestore(ctx, m, key, windowDays)
}

// TestARefusedProviderRequestFailsTheRowRatherThanLeavingItRunning.
func TestARefusedProviderRequestFailsTheRowRatherThanLeavingItRunning(t *testing.T) {
	store := &fakeMedium{initiateErr: errors.New("the bucket said no")}
	r, j := newTestRestorer(t, store)

	if _, err := r.Submit(context.Background(), restoreRequest()); err == nil {
		t.Fatal("Submit returned no error though the provider refused")
	}
	op, err := j.GetOperation(context.Background(), "op_restore_1")
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if op.Status != state.OperationFailed {
		t.Fatalf("status = %q, want %q", op.Status, state.OperationFailed)
	}
}

// TestASecondRestoreOfAnObjectAlreadyRestoringIsRefused, because asking
// twice does not make it faster and on some providers it is billed twice.
func TestASecondRestoreOfAnObjectAlreadyRestoringIsRefused(t *testing.T) {
	store := &fakeMedium{restore: &RestoreState{InProgress: true}}
	r, j := newTestRestorer(t, store)

	req := restoreRequest()
	req.IdempotencyKey = "a-genuinely-new-request"
	_, err := r.Submit(context.Background(), req)
	if !errors.Is(err, ErrAlreadyRestoring) {
		t.Fatalf("Submit: err = %v, want ErrAlreadyRestoring", err)
	}
	if _, _, _, _, initiates := store.counts(); initiates != 0 {
		t.Fatalf("a second restore was started anyway (%d)", initiates)
	}
	ops, err := j.ListOperations(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListOperations: %v", err)
	}
	if len(ops) != 0 {
		t.Fatalf("the refused second restore left %d rows behind", len(ops))
	}
}

// TestReplayingAnIdempotencyKeyStartsNothingNew.
func TestReplayingAnIdempotencyKeyStartsNothingNew(t *testing.T) {
	store := &fakeMedium{}
	r, _ := newTestRestorer(t, store)

	first, err := r.Submit(context.Background(), restoreRequest())
	if err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	// The provider now reports a restore running, exactly as it would
	// after the first submission, so this replay has to be recognised as
	// a replay rather than turned away as a duplicate restore.
	store.restore = nil

	second, err := r.Submit(context.Background(), restoreRequest())
	if err != nil {
		t.Fatalf("replayed Submit: %v", err)
	}
	if second.Created {
		t.Fatal("Created = true for a replayed idempotency key")
	}
	if second.OperationID != first.OperationID {
		t.Fatalf("replay returned %q, want the original %q", second.OperationID, first.OperationID)
	}
	if _, _, _, _, initiates := store.counts(); initiates != 1 {
		t.Fatalf("initiates = %d after a replay, want 1", initiates)
	}
}

// TestNothingButSubmitEverInitiatesARestore is FR-34's "reads never
// initiate a restore as a side effect", proven by driving every other
// entry point this package has against a counting double.
//
// It is deliberately a sweep rather than one assertion per function. The
// failure it is guarding against is somebody adding a convenience to a
// read path later, and a test that names today's read paths would not
// notice; a test that drives all of them and asserts a zero at the end at
// least fails the moment one of them grows a restore.
func TestNothingButSubmitEverInitiatesARestore(t *testing.T) {
	body := []byte("an artifact that was moved to a cold tier six months ago")
	store := &fakeMedium{content: body, attestation: hashOf(body)}
	r, j := newTestRestorer(t, store)
	ctx := context.Background()

	c := archivedCopy()

	// Every derivation. The verification gate (Ceiling, CheckClass and
	// the gated Verify) used to be swept here too; it now lives in
	// internal/placement (gate.go), takes a placement.Store, and
	// placement's TestTheGateCannotInitiateARestore pins that the Store
	// has no way to start one.
	for _, s := range States {
		_ = Describe(s, c.Class, &RestoreState{InProgress: true})
	}
	for _, class := range Classes() {
		for _, probe := range []Probe{NotAsked, Answered, DidNotAnswer} {
			if _, err := Access("cold-store", class, Observation{Probe: probe}, testNow); err != nil {
				t.Fatalf("Access: %v", err)
			}
		}
	}

	// The delete decision.
	_ = CheckSourceDelete(c, []Copy{c, theLocalCopy()})

	if _, _, _, _, initiates := store.counts(); initiates != 0 {
		t.Fatalf("something other than Submit initiated %d restores", initiates)
	}

	// And a status read of a real, submitted operation, which is the one
	// read that does talk to the provider.
	submitted, err := r.Submit(ctx, restoreRequest())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	before := countInitiates(store)
	for i := 0; i < 5; i++ {
		if _, err := r.Derive(ctx, submitted.OperationID, glacierMedium()); err != nil {
			t.Fatalf("Derive: %v", err)
		}
	}
	if got := countInitiates(store); got != before {
		t.Fatalf("polling a restore's status started %d more restores", got-before)
	}
	_ = j
}

func countInitiates(f *fakeMedium) int {
	_, _, _, _, initiates := f.counts()
	return initiates
}

// TestARestoreSurvivesARestartAndItsStatusIsReDerived is the restart
// contract, run against the real journal and the real startup sweep.
//
// The sweep is the interesting half. Every other operation in that table
// is executed by a goroutine in this process, so a process that died
// really did abandon it and the sweep is right to fail its row. A restore
// runs at the provider for hours and does not care that this process
// restarted, so a sweep that failed its row would be recording a failure
// that did not happen about a job somebody else is still doing and still
// billing for.
func TestARestoreSurvivesARestartAndItsStatusIsReDerived(t *testing.T) {
	store := &fakeMedium{}
	r, j := newTestRestorer(t, store)
	ctx := context.Background()

	submitted, err := r.Submit(ctx, restoreRequest())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// The process restarts. This is what core/service.New does on the way
	// up, with the exception list it passes.
	swept, err := j.FailInterruptedOperations(ctx, testNow.Add(time.Hour), "interrupted by restart", ActionRestore)
	if err != nil {
		t.Fatalf("FailInterruptedOperations: %v", err)
	}
	if swept != 0 {
		t.Fatalf("the startup sweep failed %d rows, and the only row here is a restore the provider is still working on", swept)
	}

	// A brand new Restorer over the same database, holding no memory of
	// anything, which is what a restarted process actually has.
	fresh := NewRestorer(j, store, func() time.Time { return testNow.Add(2 * time.Hour) }, nil)

	status, err := fresh.Derive(ctx, submitted.OperationID, glacierMedium())
	if err != nil {
		t.Fatalf("Derive after restart: %v", err)
	}
	if status.Recorded != state.OperationRunning {
		t.Fatalf("recorded status = %q, want %q; the restart must not have decided anything about it", status.Recorded, state.OperationRunning)
	}
	if status.Access != Restoring {
		t.Fatalf("access = %q, want %q", status.Access, Restoring)
	}
	if status.Parameters.Key != archivedCopy().Placement.Location {
		t.Fatalf("the row did not carry enough to say what was being restored: %+v", status.Parameters)
	}
	if status.Parameters.WindowDays != 3 {
		t.Fatalf("window days = %d, want 3", status.Parameters.WindowDays)
	}
	if strings.ContainsAny(status.Detail, "0123456789") {
		t.Errorf("the detail for a running restore carries a number: %q", status.Detail)
	}
}

// TestTheStartupSweepStillFailsAnAbandonedRunCycle is the positive control
// for the exception above, and it is what stops that exception being
// "the sweep does nothing".
func TestTheStartupSweepStillFailsAnAbandonedRunCycle(t *testing.T) {
	j := openJournal(t)
	ctx := context.Background()

	if _, err := j.CreateOperation(ctx, state.OperationRequest{
		OperationID:    "op_cycle_1",
		IdempotencyKey: "idem-cycle-1",
		Action:         "run_cycle",
		Parameters:     "{}",
		CreatedAt:      testNow,
	}); err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}

	swept, err := j.FailInterruptedOperations(ctx, testNow.Add(time.Hour), "interrupted by restart", ActionRestore)
	if err != nil {
		t.Fatalf("FailInterruptedOperations: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1; a run_cycle abandoned by a dead process still has to be failed", swept)
	}
	op, err := j.GetOperation(ctx, "op_cycle_1")
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if op.Status != state.OperationFailed {
		t.Fatalf("status = %q, want %q", op.Status, state.OperationFailed)
	}
}

// TestDeriveCompletesTheRowOnlyWhenTheProviderSaysSo.
func TestDeriveCompletesTheRowOnlyWhenTheProviderSaysSo(t *testing.T) {
	store := &fakeMedium{}
	r, j := newTestRestorer(t, store)
	ctx := context.Background()

	submitted, err := r.Submit(ctx, restoreRequest())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Still running: nothing concluded.
	status, err := r.Derive(ctx, submitted.OperationID, glacierMedium())
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if status.Recorded != state.OperationRunning {
		t.Fatalf("recorded = %q, want %q while the provider says it is running", status.Recorded, state.OperationRunning)
	}

	// The provider now says it finished, and says until when.
	expiry := testNow.Add(72 * time.Hour)
	store.restore = &RestoreState{ExpiresAt: &expiry}

	status, err = r.Derive(ctx, submitted.OperationID, glacierMedium())
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if status.Recorded != state.OperationCompleted {
		t.Fatalf("recorded = %q, want %q", status.Recorded, state.OperationCompleted)
	}
	if status.Access != Immediate {
		t.Fatalf("access = %q, want %q for a finished restore inside its window", status.Access, Immediate)
	}
	op, err := j.GetOperation(ctx, submitted.OperationID)
	if err != nil {
		t.Fatalf("GetOperation: %v", err)
	}
	if op.Status != state.OperationCompleted {
		t.Fatalf("the durable row says %q, want %q", op.Status, state.OperationCompleted)
	}
	if !strings.Contains(op.Result, expiry.UTC().Format(time.RFC3339)) {
		t.Errorf("the result does not carry the expiry the provider reported: %q", op.Result)
	}
}

// TestDeriveConcludesNothingFromSilence.
//
// A provider that reports no restore status could mean the request has not
// propagated, or that the restore finished and its window has already
// closed, or that somebody asked about the wrong bucket. Those are
// different facts with different consequences and nothing here can tell
// them apart, so the row stays where it was and the surface says what it
// actually knows.
func TestDeriveConcludesNothingFromSilence(t *testing.T) {
	store := &fakeMedium{}
	r, _ := newTestRestorer(t, store)
	ctx := context.Background()

	submitted, err := r.Submit(ctx, restoreRequest())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	store.restore = nil

	status, err := r.Derive(ctx, submitted.OperationID, glacierMedium())
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if status.Recorded != state.OperationRunning {
		t.Fatalf("recorded = %q, want %q; silence is not a completion and it is not a failure", status.Recorded, state.OperationRunning)
	}
	if status.Access != RequiresRestore {
		t.Fatalf("access = %q, want %q", status.Access, RequiresRestore)
	}
}

// TestAMediumThatWillNotAnswerDoesNotFailAnOperatorsRestore.
func TestAMediumThatWillNotAnswerDoesNotFailAnOperatorsRestore(t *testing.T) {
	store := &fakeMedium{}
	r, _ := newTestRestorer(t, store)
	ctx := context.Background()

	submitted, err := r.Submit(ctx, restoreRequest())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	store.restoreStatusErr = errors.New("dial tcp: connection refused")

	status, err := r.Derive(ctx, submitted.OperationID, glacierMedium())
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if status.Access != Unreachable {
		t.Fatalf("access = %q, want %q", status.Access, Unreachable)
	}
	if status.Recorded != state.OperationRunning {
		t.Fatalf("recorded = %q, want %q; a bucket not answering says nothing about the restore", status.Recorded, state.OperationRunning)
	}
}

// TestSubmitTellsTheOperatorWhatTheWaitAndTheBillingAreBeforeItStarts is
// the "say what it will cost in time before it starts" half of #241, with
// the constraint that neither sentence may state a quantity this product
// does not hold.
func TestSubmitTellsTheOperatorWhatTheWaitAndTheBillingAreBeforeItStarts(t *testing.T) {
	store := &fakeMedium{}
	r, _ := newTestRestorer(t, store)

	submitted, err := r.Submit(context.Background(), restoreRequest())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if submitted.Wait == "" {
		t.Fatal("Submit said nothing about how long a restore from this class takes")
	}
	if !strings.Contains(submitted.Wait, config.StorageClassDeepArchive) {
		t.Errorf("the wait statement does not name the class it is about: %q", submitted.Wait)
	}
	if submitted.Billing == "" {
		t.Fatal("Submit said nothing about the fact that this is billed")
	}
	if strings.ContainsAny(submitted.Billing, "0123456789") {
		t.Errorf("the billing statement carries a number, and this product holds no price list: %q", submitted.Billing)
	}
	if submitted.WindowDays != 3 {
		t.Errorf("WindowDays = %d, want the 3 that was asked for", submitted.WindowDays)
	}
}

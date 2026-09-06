package placement_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file is about one sentence in engine.go that was not true.
//
// ErrNotEligible's doc says it "is a distinct error because 'this artifact
// must not move' is a routine, expected answer that a caller reports and
// carries on from, while a storage failure is not". Every caller got
// err.Error() and nothing else, so the distinction had nowhere to live and
// the two arrived as two strings.
//
// The test that claimed to check it was
//
//	errors.Is(errors.New(o.Refused), placement.ErrNotEligible)
//
// which is false for every input there has ever been, because errors.New
// on a string produces an error with no relation to the sentinel. It was
// written with a strings.Contains fallback after an || so it never failed,
// and the fallback is what ran, in a package whose own rule is that a
// caller must never classify by text.

// brokenJournal is a MoveJournal whose first read fails, which is what a
// caller sees when the database is gone rather than when the policy says
// no.
type brokenJournal struct {
	placement.MoveJournal
	err error
}

func (b brokenJournal) Get(context.Context, model.ArtifactID) (state.Record, error) {
	return state.Record{}, b.err
}

// errDiskGone is deliberately worded to CONTAIN ErrNotEligible's own text.
//
// That is the control, and it is the whole reason this test can be
// trusted. A check written as strings.Contains(refused, "not eligible to
// move") passes this storage failure, calls a dead database a policy
// decision, and a cycle that should have stopped carries on refusing
// artifacts one at a time. Only a check on the error graph tells them
// apart, and this string is what makes the difference observable rather
// than a matter of opinion.
var errDiskGone = fmt.Errorf(
	"state: opening the journal failed: input/output error, so nothing here can say whether this is a case of %s",
	placement.ErrNotEligible.Error())

func TestACallerCanTellAPolicyRefusalFromAStorageFailure(t *testing.T) {
	t.Run("policy refusal", func(t *testing.T) {
		// A destination whose storage class cannot support the
		// verification its medium requires. Routine, expected, and the
		// caller should report it and carry on.
		f := newFixture(t, fixtureOpts{storageClass: config.StorageClassDeepArchive})
		f.medium.archiveRefusesReads = true

		report := f.runCycle()
		if len(report.Outcomes) != 1 {
			t.Fatalf("the cycle reported %d outcomes, want 1: %+v", len(report.Outcomes), report.Outcomes)
		}
		o := report.Outcomes[0]
		if o.Refused == "" {
			t.Fatal("a refused plan reported no reason at all")
		}
		if o.Err == nil {
			t.Fatal("the outcome carries the reason as a string and dropped the error, so nothing can ask what kind of refusal it was")
		}
		if !errors.Is(o.Err, placement.ErrNotEligible) {
			t.Errorf("the refusal is not an ErrNotEligible: %v", o.Err)
		}
		if !o.PolicyRefusal() {
			t.Error("PolicyRefusal is false for a refusal the engine made on policy")
		}
	})

	t.Run("storage failure", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		f.engine.Journal = brokenJournal{MoveJournal: f.guarded, err: errDiskGone}

		report := f.runCycle()
		if len(report.Outcomes) != 1 {
			t.Fatalf("the cycle reported %d outcomes, want 1: %+v", len(report.Outcomes), report.Outcomes)
		}
		o := report.Outcomes[0]
		if o.Err == nil {
			t.Fatal("a storage failure reported no error either, so the two are still indistinguishable")
		}
		if !errors.Is(o.Err, errDiskGone) {
			t.Errorf("the outcome does not carry the storage failure: %v", o.Err)
		}
		if errors.Is(o.Err, placement.ErrNotEligible) {
			t.Error("a journal that could not be read was reported as a policy refusal")
		}
		if o.PolicyRefusal() {
			t.Error("PolicyRefusal is true for a dead journal")
		}

		// The control fires here or it fires nowhere: this is the input
		// on which the text check and the error check disagree.
		if !strings.Contains(o.Refused, placement.ErrNotEligible.Error()) {
			t.Fatalf("errDiskGone no longer contains ErrNotEligible's text, so this test has stopped being able to tell "+
				"a text check from an errors.Is check and is now proving nothing: %q", o.Refused)
		}
	})
}

// TestTheOldAssertionCouldNotHaveFired is the finding itself, written down
// so it cannot come back.
//
// errors.New(s) produces an error whose only relation to anything is its
// message. Wrapping a sentinel's text in a string does not wrap the
// sentinel, so the check that was in the conformance suite was false on
// the refusal it targeted, false on every other refusal, and false on the
// sentinel's own text.
func TestTheOldAssertionCouldNotHaveFired(t *testing.T) {
	for _, s := range []string{
		placement.ErrNotEligible.Error(),
		"placement: this artifact is not eligible to move: it already has an ACTIVE copy there",
		"anything at all",
	} {
		if errors.Is(errors.New(s), placement.ErrNotEligible) {
			t.Fatalf("errors.New(%q) now matches ErrNotEligible, which would mean errors.New has acquired an "+
				"identity it does not have; if that is somehow true, the conformance assertion this test is about was fine", s)
		}
	}
}

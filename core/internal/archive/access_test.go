package archive

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file is the access vocabulary's suite: the four words, the
// derivation that picks one of them, and the sentence a surface prints
// beside it.
//
// Two of its claims are about what is deliberately NOT there, and both are
// tested over a whole set rather than over the interesting members,
// because both are broken by adding something rather than by editing
// something. There is no fifth state, so a class the table has no row for
// makes Access refuse instead of guessing, and the derivation is walked
// across the entire closed set of classes and not just the two archive
// ones. And nothing this package renders anywhere carries a percentage, a
// price or a finishing time, which is asserted by reflecting over every
// field of every struct a surface can read, not by reading the strings
// somebody remembered to check.

var testNow = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

func ptrTime(t time.Time) *time.Time { return &t }

// TestAccessForEveryClass is the acceptance criterion "closed access
// vocabulary correct for every class", written as a table over the whole
// closed set rather than over the two classes that are interesting.
//
// The five non-archive rows are the ones that would be tempting to leave
// out, and they are the ones that catch a table entry going missing: a
// class with no row makes Of refuse, and a caller that then guessed
// "immediate" would pass a test that only covered GLACIER.
func TestAccessForEveryClass(t *testing.T) {
	immediate := map[string]bool{
		config.StorageClassStandard:           true,
		config.StorageClassStandardIA:         true,
		config.StorageClassOneZoneIA:          true,
		config.StorageClassIntelligentTiering: true,
		config.StorageClassGlacierIR:          true,
		config.StorageClassGlacier:            false,
		config.StorageClassDeepArchive:        false,
	}

	for _, class := range Classes() {
		got, err := Access("cold-store", class, Observation{Probe: Answered}, testNow)
		if err != nil {
			t.Fatalf("Access(%q): %v", class, err)
		}
		if !got.Valid() {
			t.Fatalf("Access(%q) = %q, which is not in the closed vocabulary", class, got)
		}
		want := RequiresRestore
		if immediate[class] {
			want = Immediate
		}
		if got != want {
			t.Errorf("Access(%q) = %q, want %q", class, got, want)
		}
	}
}

// TestLocalIsAlwaysImmediate is FR-35's compatibility promise at this
// level: every artifact in every deployment that never configured a medium
// has exactly one local placement, and it has to read the way it read
// before this package existed, whatever anybody passes in beside it.
func TestLocalIsAlwaysImmediate(t *testing.T) {
	for _, obs := range []Observation{
		{},
		{Probe: DidNotAnswer},
		{Probe: Answered, Restore: &RestoreState{InProgress: true}},
	} {
		got, err := Access(state.MediumLocal, config.StorageClassDeepArchive, obs, testNow)
		if err != nil {
			t.Fatalf("Access(local): %v", err)
		}
		if got != Immediate {
			t.Fatalf("Access(local, %+v) = %q, want %q", obs, got, Immediate)
		}
	}
}

// TestAMediumThatDidNotAnswerIsUnreachableAndNotSomethingElse keeps the two
// facts apart that FR-34 says must never collapse: an endpoint that is down
// is not an object that needs a restore, and it is certainly not an object
// that is gone.
//
// It covers a non-archive class too, because the tempting shortcut is to
// return Immediate for anything non-archive without looking at whether the
// medium answered, which would send a caller off to do a read that cannot
// possibly work.
func TestAMediumThatDidNotAnswerIsUnreachableAndNotSomethingElse(t *testing.T) {
	for _, class := range []string{config.StorageClassStandard, config.StorageClassGlacier} {
		got, err := Access("cold-store", class, Observation{Probe: DidNotAnswer}, testNow)
		if err != nil {
			t.Fatalf("Access(%q): %v", class, err)
		}
		if got != Unreachable {
			t.Errorf("Access(%q, DidNotAnswer) = %q, want %q", class, got, Unreachable)
		}
		if got.Retrievable() {
			t.Errorf("Access(%q, DidNotAnswer) reported as retrievable", class)
		}
	}
}

// TestAnArchivedCopyNobodyLookedAtNeedsARestore pins the zero value's
// meaning, which is what every journal-only read surface passes.
func TestAnArchivedCopyNobodyLookedAtNeedsARestore(t *testing.T) {
	got, err := Access("cold-store", config.StorageClassDeepArchive, Observation{}, testNow)
	if err != nil {
		t.Fatalf("Access: %v", err)
	}
	if got != RequiresRestore {
		t.Fatalf("Access(unprobed archive copy) = %q, want %q", got, RequiresRestore)
	}
}

// TestRestoreLifecycleReadsThroughTheFourStates walks one archived object
// from untouched, through a running restore, to restored, to the moment
// its restore window closes again.
//
// The last row is the one that matters most and the one an implementation
// gets wrong by accident: a restore expires, the object goes back to being
// unreadable, and nothing in the journal changed to say so. An access
// state derived from the journal alone would still be claiming the bytes
// are available.
func TestRestoreLifecycleReadsThroughTheFourStates(t *testing.T) {
	tests := []struct {
		name    string
		restore *RestoreState
		want    State
	}{
		{"nobody has asked", nil, RequiresRestore},
		{"restore running, no expiry yet", &RestoreState{InProgress: true}, Restoring},
		{"restore running, expiry already published", &RestoreState{InProgress: true, ExpiresAt: ptrTime(testNow.Add(72 * time.Hour))}, Restoring},
		{"restored and still readable", &RestoreState{ExpiresAt: ptrTime(testNow.Add(1 * time.Hour))}, Immediate},
		{"restore window has closed again", &RestoreState{ExpiresAt: ptrTime(testNow.Add(-1 * time.Hour))}, RequiresRestore},
		{"finished, but the provider reported no window", &RestoreState{}, RequiresRestore},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Access("cold-store", config.StorageClassGlacier,
				Observation{Probe: Answered, Restore: tc.restore}, testNow)
			if err != nil {
				t.Fatalf("Access: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Access = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAccessRefusesAClassItDoesNotKnow rather than inventing a fifth state
// or defaulting to a reassuring one.
func TestAccessRefusesAClassItDoesNotKnow(t *testing.T) {
	got, err := Access("cold-store", "GLACIER_SUPER_DEEP", Observation{Probe: Answered}, testNow)
	if err == nil {
		t.Fatalf("Access(unknown class) = %q with no error; an unrecognised class must not resolve to a state", got)
	}
	if got != "" {
		t.Fatalf("Access(unknown class) = %q alongside an error; it must return no state at all", got)
	}
}

// TestNothingRenderedAboutARestoreCarriesAPercentageAPriceOrAnETA is the
// acceptance criterion of that name, applied to every string this package
// hands a surface.
//
// # Why it checks characters and not words
//
// The obvious test greps for "percent" and "estimate", and it fails
// immediately on the sentences that exist to say this product reports
// NEITHER, which is the wrong result for the right words. What actually
// distinguishes a forbidden string from an honest one is that a forbidden
// one states a QUANTITY: a percent sign, a currency amount, or a number in
// a sentence about a restore that is running.
//
// So there are three rules, each about a number rather than a word. No
// string may carry a percent sign or a currency symbol. The sentence for a
// restore in progress may not contain a digit at all, because that is the
// one state where this product knows nothing numeric whatever. And the
// billing statement may not contain a digit, because the only number that
// belongs in a sentence about a bill is a price this product does not
// have.
func TestNothingRenderedAboutARestoreCarriesAPercentageAPriceOrAnETA(t *testing.T) {
	quantities := regexp.MustCompile(`[%$\x{20AC}\x{00A3}\x{00A5}]`)

	var rendered []string
	for _, class := range Classes() {
		b, err := Of(class)
		if err != nil {
			t.Fatalf("Of(%q): %v", class, err)
		}
		rendered = append(rendered, b.RestoreWait, BillingStatement(b))
		for _, s := range States {
			rendered = append(rendered,
				Describe(s, class, nil),
				Describe(s, class, &RestoreState{InProgress: true}),
				Describe(s, class, &RestoreState{ExpiresAt: ptrTime(testNow)}),
			)
		}
	}
	for _, s := range rendered {
		if quantities.MatchString(s) {
			t.Errorf("this string carries a percentage or a currency amount: %q", s)
		}
	}

	restoring := Describe(Restoring, config.StorageClassGlacier, &RestoreState{InProgress: true})
	if strings.ContainsAny(restoring, "0123456789") {
		t.Errorf("the sentence for a restore in progress contains a number, and there is no number to be had: %q", restoring)
	}

	for _, class := range Classes() {
		b, err := Of(class)
		if err != nil {
			t.Fatalf("Of(%q): %v", class, err)
		}
		if bill := BillingStatement(b); strings.ContainsAny(bill, "0123456789") {
			t.Errorf("the billing statement for %s contains a number, and this product holds no price list to get one from: %q", class, bill)
		}
	}
}

// camelWords splits a Go field name into its lower-cased words, so a test
// can ask whether a field is NAMED after something rather than whether its
// name happens to contain those letters. Detail contains "eta" and is
// perfectly innocent; PercentComplete is not.
func camelWords(name string) []string {
	var words []string
	start := 0
	for i := 1; i <= len(name); i++ {
		if i == len(name) || (name[i] >= 'A' && name[i] <= 'Z') {
			words = append(words, strings.ToLower(name[start:i]))
			start = i
		}
	}
	return words
}

// TestTheRestoreSurfacesHaveNowhereToPutAProgressReading is the structural
// half of the test above.
//
// Prose can be fixed by editing prose. A field called PercentComplete on
// one of these structs is an invitation that outlives whoever declined it,
// so the field names themselves are pinned: a surface that wants to draw a
// progress bar has to add somewhere to put the number first, and that is
// the change this test refuses.
//
// Booleans are exempt, and only booleans. RestoreState.InProgress is named
// after S3's own IsRestoreInProgress flag and is the honest answer to "is
// it still going"; a bool cannot carry a percentage, so it cannot carry
// the thing this test is about. Anything that can hold a number or a
// string can.
func TestTheRestoreSurfacesHaveNowhereToPutAProgressReading(t *testing.T) {
	banned := map[string]bool{
		"percent": true, "percentage": true, "eta": true,
		"estimate": true, "estimated": true, "remaining": true,
		"cost": true, "price": true, "amount": true, "charge": true,
		"progress": true, "elapsed": true, "speed": true, "rate": true,
	}
	for _, v := range []any{Submitted{}, Status{}, RestoreState{}, Behaviour{}, Parameters{}} {
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.Type.Kind() == reflect.Bool {
				continue
			}
			for _, w := range camelWords(f.Name) {
				if banned[w] {
					t.Errorf("%s.%s: a restore has no progress, no estimate and no price this product can state, so there must be nowhere to put one",
						rt.Name(), f.Name)
				}
			}
		}
	}
}

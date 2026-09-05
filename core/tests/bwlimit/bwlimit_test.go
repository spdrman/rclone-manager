package bwlimit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
)

// The numbers TestClearingTheLimitReallyClearsIt measures against.
//
// rclone's newEmptyTokenBucket drains the bucket the moment it makes it, so
// the first WaitN after a limit is set waits for the tokens to be minted:
// exactly grant/limit, decided by a rate rather than by the scheduler. That
// is what makes a timing assertion here a measurement rather than a bet.
const (
	// proofLimit and proofGrant are one second of waiting apart.
	proofLimit = "1Mi"
	proofGrant = 1024 * 1024

	// proofThrottled is the floor a throttled wait has to clear. Well under
	// the second the arithmetic says, because a machine cannot make this
	// wait SHORTER, only longer.
	proofThrottled = 500 * time.Millisecond

	// proofCleared is the ceiling a cleared one has to stay under. With no
	// bucket at all LimitBandwidth returns without waiting for anything, so
	// this is entirely an allowance for a busy scheduler, and it is a
	// quarter of the floor above so the two can never meet.
	proofCleared = 125 * time.Millisecond
)

// TestClearingTheLimitReallyClearsIt is the evidence for this package's own
// doc, and it is deliberately three assertions rather than one: that the
// limit really throttles (or the rest proves nothing), that Clear really
// clears it, and that the idiom every caller used before really did not.
func TestClearingTheLimitReallyClearsIt(t *testing.T) {
	t.Cleanup(Clear)

	throttled, ci := fs.AddConfig(context.Background())
	if err := (&ci.BwLimit).Set(proofLimit); err != nil {
		t.Fatalf("setting --bwlimit: %v", err)
	}
	accounting.TokenBucket.StartTokenBucket(throttled)

	// Positive control.
	if waited := timeOneGrant(); waited < proofThrottled {
		t.Fatalf("a %d-byte grant under a %s/s limit came back in %s, under the %s floor; "+
			"the limit is not throttling at all, so nothing below is evidence about clearing one",
			int64(proofGrant), proofLimit, waited, proofThrottled)
	}

	// The idiom every caller used to use, which is the defect.
	unlimited, _ := fs.AddConfig(context.Background())
	accounting.TokenBucket.StartTokenBucket(unlimited)
	if waited := timeOneGrant(); waited < proofThrottled {
		t.Errorf("StartTokenBucket with an unlimited config cleared the limit (a grant came back in %s). "+
			"rclone v1.75.0 could not do that, which is the whole reason Clear exists; "+
			"if this is a newer rclone that fixed it, Clear can go and so can this row", waited)
	}

	// And the fix.
	Clear()
	if waited := timeOneGrant(); waited > proofCleared {
		t.Fatalf("a %d-byte grant still took %s after Clear, past the %s ceiling; "+
			"the limit outlives the test that set it, and every test after it runs throttled",
			int64(proofGrant), waited, proofCleared)
	}
}

// TestProofBoundsCanStillFail guards the two bounds above, since bounds
// that overlap would make the row green whatever the limiter did.
func TestProofBoundsCanStillFail(t *testing.T) {
	if proofCleared >= proofThrottled {
		t.Errorf("the cleared ceiling (%s) is at or above the throttled floor (%s), so one wait satisfies both "+
			"and the row cannot tell a cleared limiter from a throttling one", proofCleared, proofThrottled)
	}
	// The floor has to be reachable: a grant the limit would satisfy
	// instantly could never clear it.
	var limit fs.SizeSuffix
	if err := limit.Set(proofLimit); err != nil {
		t.Fatalf("parsing %q: %v", proofLimit, err)
	}
	implied := time.Duration(float64(proofGrant) / float64(limit) * float64(time.Second))
	if implied <= proofThrottled {
		t.Errorf("a %d-byte grant at %s/s implies a %s wait, at or under the %s floor; the positive control "+
			"would fail on a correct limiter", int64(proofGrant), proofLimit, implied, proofThrottled)
	}
}

// TestABareNumberIsKibibytes pins the factor of 1024 issue #414 is half
// about, in rclone's own parser and with the issue's own literal.
//
// It is a pin on a dependency rather than on this repository, which is the
// point: CheckUnit's refusal only earns its keep while a bare number really
// does mean something else, and the day rclone changes that is the day this
// row says so instead of the next reader rediscovering it from a four
// minute transfer.
func TestABareNumberIsKibibytes(t *testing.T) {
	parse := func(s string) int64 {
		t.Helper()
		var bw fs.BwTimetable
		if err := (&bw).Set(s); err != nil {
			t.Fatalf("parsing %q as a bandwidth limit: %v", s, err)
		}
		return int64(bw.LimitAt(time.Now()).Bandwidth.Tx)
	}

	// The literal from gate_test.go's old fmt.Sprintf("%d", 64*1024).
	bare, suffixed := parse("65536"), parse("64Ki")
	t.Logf(`"65536" is %d B/s and "64Ki" is %d B/s: a factor of %d`, bare, suffixed, bare/suffixed)

	if bare == suffixed {
		t.Fatalf(`rclone now reads a bare "65536" as %d B/s, the same as "64Ki". `+
			"The unit trap this package's CheckUnit exists for is gone, so CheckUnit and its callers' unit "+
			"suffixes can go with it. Check the rclone version before deleting anything.", bare)
	}
	if bare != 1024*suffixed {
		t.Fatalf(`a bare "65536" is %d B/s against "64Ki"'s %d, which is neither equal nor the factor of 1024 `+
			"this package documents. CheckUnit's error message quotes that factor and would now be wrong.", bare, suffixed)
	}
}

// TestCheckUnitRefusesABareNumber is the guard's own evidence. Throttle
// calls t.Fatalf on a refusal, which a test cannot observe from the inside,
// so the rule is checked here where it returns an error instead.
func TestCheckUnitRefusesABareNumber(t *testing.T) {
	refused := []string{"65536", "131072", "1", "0", " 65536 "}
	for _, s := range refused {
		err := CheckUnit(s)
		if err == nil {
			t.Errorf("CheckUnit(%q) allowed a bare number, which rclone reads as KiB; this is exactly the shape "+
				"that made two tests throttle nothing for months (#414)", s)
			continue
		}
		// The message has to name the mistake, because the whole value of
		// refusing here rather than letting the transfer be mysteriously
		// fast is that the reader is told which way to fix it.
		if !strings.Contains(err.Error(), "Ki") {
			t.Errorf("CheckUnit(%q) refused with %q, which does not tell the reader what to write instead", s, err)
		}
	}

	// And it has to let the real spellings through, or every caller would
	// have to work around it and the guard would be removed rather than
	// obeyed.
	allowed := []string{"64Ki", "64k", "1M", "1Mi", "1B", "off", "", "08:00,512Ki 23:00,off"}
	for _, s := range allowed {
		if err := CheckUnit(s); err != nil {
			t.Errorf("CheckUnit(%q) refused a limit that says what it means: %v", s, err)
		}
	}
}

func timeOneGrant() time.Duration {
	start := time.Now()
	accounting.TokenBucket.LimitBandwidth(accounting.TokenBucketSlotAccounting, proofGrant)
	return time.Since(start)
}

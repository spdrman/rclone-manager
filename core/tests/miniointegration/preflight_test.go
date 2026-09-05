package miniointegration_test

// Issue #443's medium preflight, run against a real S3 API rather than
// against a fake store.
//
// internal/mediumcheck's own suite proves the preflight's LOGIC: which
// step fails for which shape of failure, what gets skipped, what is never
// spent. It cannot prove the thing this file is for, which is that the
// eight checks compose into a real answer through the real adapter, and
// that the answer is not accidentally green. A preflight that passes
// against a bucket that cannot serve a restore is the defect shape this
// repository keeps producing, so the deny cases here are driven against a
// server that genuinely does not have the bucket rather than against a
// double that says so.

import (
	"context"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/mediumcheck"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/tests/machines"
)

func preflight(t *testing.T, medium transport.Medium, class placement.Class) mediumcheck.Report {
	t.Helper()
	report, err := mediumcheck.Run(context.Background(),
		mediumcheck.Deps{Store: rclone.New()}, medium, class)
	if err != nil {
		t.Fatalf("mediumcheck.Run: %v", err)
	}
	return report
}

func stepOf(t *testing.T, r mediumcheck.Report, step mediumcheck.Step) mediumcheck.Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Step == step {
			return c
		}
	}
	t.Fatalf("the report has no %q check: %+v", step, r.Checks)
	return mediumcheck.Check{}
}

// TestMinioPreflight_PassesAgainstARealBucketAndLeavesNothingBehind is the
// allow case, and the second half of its name is the part that needs a
// real server: a probe object left in somebody's bucket is litter nothing
// in this product ever collects, and only the endpoint can be asked
// whether it is really gone.
func TestMinioPreflight_PassesAgainstARealBucketAndLeavesNothingBehind(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	medium := fixture.NewBucket(t)

	report := preflight(t, medium, placement.Content)
	if !report.OK {
		t.Fatalf("a real, working bucket did not pass: %+v", report.Failures())
	}
	for _, step := range mediumcheck.Steps {
		if c := stepOf(t, report, step); c.Outcome != mediumcheck.Passed {
			t.Errorf("step %q = %q against a working bucket: %s", step, c.Outcome, c.Detail)
		}
	}

	// Ask the endpoint, not the report. The delete step already asserts
	// this through the same adapter it deleted with, and a listing is the
	// independent question: does anything at all remain under the bucket
	// after a preflight that says it cleaned up.
	objects, err := rclone.New().ListObjects(context.Background(), medium, "")
	if err != nil {
		t.Fatalf("listing the bucket after the preflight: %v", err)
	}
	if len(objects) != 0 {
		t.Fatalf("the preflight left %d object(s) in the bucket: %+v", len(objects), objects)
	}
}

// TestMinioPreflight_RefusesAttestedAgainstARealEndpoint is the refusal
// that only a real endpoint can establish, and it is why this file exists
// rather than only the fake-store suite.
//
// FR-31's `attested` class asks the endpoint for its own full-object
// digest. Measured against the rclone this build embeds, no S3 endpoint
// can produce one, so a medium declared `upload_verification: attested`
// cannot serve a single move. A preflight reporting that green would be
// lying about the one thing it exists to establish, and a fake store
// saying so proves only that the fake was written to say so.
func TestMinioPreflight_RefusesAttestedAgainstARealEndpoint(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	medium := fixture.NewBucket(t)

	report := preflight(t, medium, placement.Attested)
	if report.OK {
		t.Fatal("attested passed against a real S3 endpoint, which cannot produce a full-object SHA-256 through this build")
	}

	verification := stepOf(t, report, mediumcheck.StepVerification)
	if verification.Outcome != mediumcheck.Failed {
		t.Fatalf("verification = %q, want failed: %s", verification.Outcome, verification.Detail)
	}
	if verification.Category != transport.UnsupportedCapability.String() {
		t.Fatalf("verification category = %q, want %q", verification.Category, transport.UnsupportedCapability)
	}
	if !strings.Contains(verification.Detail, "readback") {
		t.Fatalf("verification detail = %q, want it to name what to declare instead", verification.Detail)
	}

	// Everything the endpoint CAN do still passed, so the refusal is
	// specific rather than a blanket "this medium is bad", and the probe
	// is still rolled back.
	for _, step := range []mediumcheck.Step{
		mediumcheck.StepCredentials, mediumcheck.StepReach, mediumcheck.StepDeliverable,
		mediumcheck.StepWrite, mediumcheck.StepReadBack, mediumcheck.StepDelete,
	} {
		if c := stepOf(t, report, step); c.Outcome != mediumcheck.Passed {
			t.Errorf("step %q = %q, want passed: an attested refusal must not take the rest of the report with it: %s", step, c.Outcome, c.Detail)
		}
	}
}

// TestMinioPreflight_ABucketThatIsNotThereIsConfigurationAndNothingIsWritten
// is the deny case a fake cannot honestly stand in for: the endpoint is
// real, the credential is real and accepted, and the bucket simply is not
// there. That is the single most likely thing an operator gets wrong, and
// before this check the first thing to find it out was a move.
func TestMinioPreflight_ABucketThatIsNotThereIsConfigurationAndNothingIsWritten(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	medium := fixture.MediumForBucket("no-such-bucket-" + strings.Repeat("z", 8))

	report := preflight(t, medium, placement.Content)
	if report.OK {
		t.Fatal("a medium naming a bucket that is not there passed")
	}

	// The credential was obtained and the endpoint took it, so that half
	// PASSES. Reporting it as a credential problem would send somebody to
	// look at the wrong machine.
	if c := stepOf(t, report, mediumcheck.StepCredentials); c.Outcome != mediumcheck.Passed {
		t.Fatalf("credentials = %q against a real endpoint with a real credential: %s", c.Outcome, c.Detail)
	}
	reach := stepOf(t, report, mediumcheck.StepReach)
	if reach.Outcome != mediumcheck.Failed {
		t.Fatalf("reach = %q, want failed: %s", reach.Outcome, reach.Detail)
	}
	if reach.Category != transport.Configuration.String() {
		t.Fatalf("reach category = %q, want %q: a missing bucket is one line of configuration to fix, not an authentication problem", reach.Category, transport.Configuration)
	}
	for _, step := range []mediumcheck.Step{
		mediumcheck.StepWrite, mediumcheck.StepReadBack, mediumcheck.StepStorageClass,
		mediumcheck.StepVerification, mediumcheck.StepDelete,
	} {
		if c := stepOf(t, report, step); c.Outcome != mediumcheck.Skipped {
			t.Errorf("step %q = %q, want skipped: nothing may be sent to a bucket that is not there", step, c.Outcome)
		}
	}
}

// TestMinioPreflight_AnArchiveClassSpendsNothingAgainstARealEndpoint is
// the measurement, not the assertion. DEEP_ARCHIVE bills a 180-day minimum
// duration for every object written to it, so "the preflight refuses
// delivery" and "the preflight refuses delivery WITHOUT writing" are
// different claims and only the second one is free. A server that can be
// asked afterwards is the only thing that can tell them apart.
func TestMinioPreflight_AnArchiveClassSpendsNothingAgainstARealEndpoint(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	medium := fixture.NewBucket(t)
	medium.StorageClass = config.StorageClassDeepArchive

	report := preflight(t, medium, placement.Content)
	if report.OK {
		t.Fatal("an archive-class medium reported that a backup can be delivered to it")
	}
	deliverable := stepOf(t, report, mediumcheck.StepDeliverable)
	if deliverable.Outcome != mediumcheck.Failed {
		t.Fatalf("deliverable = %q, want failed: %s", deliverable.Outcome, deliverable.Detail)
	}
	if !strings.Contains(deliverable.Detail, "restore") {
		t.Fatalf("deliverable detail = %q, want it to keep the restore case legal in so many words", deliverable.Detail)
	}

	// The endpoint's own answer, which is the only one that costs money to
	// get wrong.
	objects, err := rclone.New().ListObjects(context.Background(), medium, "")
	if err != nil {
		t.Fatalf("listing the bucket after the preflight: %v", err)
	}
	if len(objects) != 0 {
		t.Fatalf("the preflight wrote %d object(s) to an archive-class medium: %+v", len(objects), objects)
	}
}

// TestMinioPreflight_AnUnobtainableCredentialNeverReachesTheEndpoint is
// the other half of the pair above, and the reason
// transport.ErrCredentialsUnavailable exists: both are Configuration, and
// an operator has to be told which machine to go and look at.
func TestMinioPreflight_AnUnobtainableCredentialNeverReachesTheEndpoint(t *testing.T) {
	fixture := machines.Start(t).Medium(t)
	medium := fixture.NewBucket(t)
	medium.Credentials = transport.MediumCredentials{File: "/nonexistent/no-such-credentials"}

	report := preflight(t, medium, placement.Content)
	if report.OK {
		t.Fatal("a medium whose credential cannot be read passed")
	}
	creds := stepOf(t, report, mediumcheck.StepCredentials)
	if creds.Outcome != mediumcheck.Failed {
		t.Fatalf("credentials = %q, want failed: %s", creds.Outcome, creds.Detail)
	}
	if c := stepOf(t, report, mediumcheck.StepReach); c.Outcome != mediumcheck.Skipped {
		t.Fatalf("reach = %q, want skipped: there was no credential to contact the endpoint with", c.Outcome)
	}

	// FR-33 against a real adapter's real error: the path this test just
	// put in the medium is exactly what the classified cause names, and it
	// must not be in the report.
	for _, c := range report.Checks {
		if strings.Contains(c.Detail, "no-such-credentials") {
			t.Fatalf("step %q leaked the credential path into the report: %s", c.Step, c.Detail)
		}
	}
}

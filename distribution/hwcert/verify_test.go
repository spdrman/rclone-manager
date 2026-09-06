package hwcert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func verifyFixture(t *testing.T, mutate func(r *Record)) (*Result, error) {
	t.Helper()
	p := mustParseFixture(t)
	r := fixtureRecord()
	if mutate != nil {
		mutate(r)
	}
	return Verify(p, r, r.Architecture, fixtureManifest())
}

func TestAnUnbrokenRecordPassesAndReportsEveryMetric(t *testing.T) {
	res, err := verifyFixture(t, nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.Passed {
		t.Fatalf("the fixture record failed:\n%s", res.Report())
	}

	// Every metric the procedure declares is reported, passing or
	// failing. A report that prints only its failures cannot be
	// reviewed, because a reader cannot tell a rule that never ran from
	// one that passed.
	want := []string{"idle_rss_bytes", "transfer_mb_per_second", "raw_copy_mb_per_second", "transfer_throughput_ratio"}
	got := map[string]bool{}
	for _, m := range res.Metrics {
		got[m.Name] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("%s is declared in the procedure and missing from the report", name)
		}
	}
	if len(res.Metrics) != len(want) {
		t.Errorf("reported %d metrics, want %d", len(res.Metrics), len(want))
	}

	// The derived ratio is computed from the record, not carried in it.
	for _, m := range res.Metrics {
		if m.Name == "transfer_throughput_ratio" && (m.Value < 0.899 || m.Value > 0.901) {
			t.Errorf("transfer_throughput_ratio = %v, want 180/200", m.Value)
		}
	}

	if len(res.Shape) == 0 {
		t.Error("no shape check ran")
	}
	for _, s := range res.Shape {
		if s.Verdict != Pass {
			t.Errorf("shape check %q failed on the fixture: %s", s.Check, s.Detail)
		}
	}

	report := res.Report()
	for _, name := range want {
		if !strings.Contains(report, name) {
			t.Errorf("the report does not name %s:\n%s", name, report)
		}
	}
}

// The plainest thing the comparator has to do, and the plainest way it
// could quietly stop doing it.
func TestAMeasurementOverItsThresholdFails(t *testing.T) {
	// The fixture's amd64 threshold for idle_rss_bytes is 128 MiB.
	res, err := verifyFixture(t, func(r *Record) {
		r.Measurements["idle_rss_bytes"] = 134217729
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Passed {
		t.Fatalf("a record one byte over the threshold passed:\n%s", res.Report())
	}
	if !failed(res, "idle_rss_bytes") {
		t.Errorf("idle_rss_bytes was not the metric that failed:\n%s", res.Report())
	}

	// Exactly on the threshold is inside it. A budget an operator is
	// told to provision has to be reachable.
	res, err = verifyFixture(t, func(r *Record) {
		r.Measurements["idle_rss_bytes"] = 134217728
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.Passed {
		t.Errorf("a record exactly on the threshold failed:\n%s", res.Report())
	}
}

func TestAHigherIsBetterMeasurementUnderItsThresholdFails(t *testing.T) {
	// 0.7 is the floor. 130/200 is 0.65.
	res, err := verifyFixture(t, func(r *Record) {
		r.Measurements["transfer_mb_per_second"] = 130
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Passed {
		t.Fatalf("a transfer at 65%% of the device's own copy rate passed:\n%s", res.Report())
	}
	if !failed(res, "transfer_throughput_ratio") {
		t.Errorf("transfer_throughput_ratio was not the metric that failed:\n%s", res.Report())
	}
}

// A missing number is the failure mode that matters most, because it is
// the one that looks like nothing. Every metric the procedure declares,
// gated or merely recorded, has to be present, and a derived metric whose
// input is absent has to name the input rather than reporting a zero
// ratio.
func TestAMissingMeasurementFailsRatherThanDefaultingToPass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		drop    string
		wantsIn []string
	}{
		{"a gated metric", "idle_rss_bytes", []string{"idle_rss_bytes"}},
		{"a recorded metric", "transfer_mb_per_second", []string{"transfer_mb_per_second", "transfer_throughput_ratio"}},
		{"a derived metric's denominator", "raw_copy_mb_per_second", []string{"raw_copy_mb_per_second", "transfer_throughput_ratio"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := verifyFixture(t, func(r *Record) {
				delete(r.Measurements, tc.drop)
			})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.Passed {
				t.Fatalf("a record missing %s passed:\n%s", tc.drop, res.Report())
			}
			for _, name := range tc.wantsIn {
				if !failed(res, name) {
					t.Errorf("%s did not fail when %s was missing:\n%s", name, tc.drop, res.Report())
				}
			}
			// A missing measurement is reported as missing, never as a
			// zero that happens to be outside a threshold.
			for _, m := range res.Metrics {
				if m.Name == tc.drop && m.Present {
					t.Errorf("%s is reported present with value %v", m.Name, m.Value)
				}
			}
		})
	}
}

// An amd64 pass must never be usable to imply an arm64 pass. This is that
// rule at the one place it could be broken by accident: handing the
// verifier a record for the other architecture.
func TestARecordForTheWrongArchitectureIsRefused(t *testing.T) {
	p := mustParseFixture(t)
	r := fixtureRecord() // amd64

	if _, err := Verify(p, r, "arm64", fixtureManifest()); err == nil {
		t.Fatal("an amd64 record was accepted as arm64 evidence")
	} else if !strings.Contains(err.Error(), "amd64") || !strings.Contains(err.Error(), "arm64") {
		t.Errorf("the refusal %q does not name both architectures", err)
	}

	// An architecture the procedure does not claim has no thresholds, so
	// verifying against it would gate nothing.
	arm := fixtureRecord()
	arm.Architecture = "riscv64"
	arm.Probe.GOARCH = "riscv64"
	arm.Device.UnameMachine = "riscv64"
	if _, err := Verify(p, arm, "riscv64", fixtureManifest()); err == nil {
		t.Fatal("a record for an architecture the procedure does not claim was accepted")
	}

	// And the internal disagreement is caught here too, not only in
	// Validate, because Verify is what the command line calls.
	bad := fixtureRecord()
	bad.Probe.GOARCH = "arm64"
	if _, err := Verify(p, bad, "amd64", fixtureManifest()); err == nil {
		t.Fatal("a record whose probe was built for the other architecture was accepted")
	}
}

// The method block is the other half of "the thresholds were fixed first":
// a threshold cannot be met by shortening the window it is measured over.
func TestARecordMeasuredOverADifferentWindowIsRefused(t *testing.T) {
	p := mustParseFixture(t)
	r := fixtureRecord()
	r.Method["idle_window_seconds"] = 60

	if _, err := Verify(p, r, "amd64", fixtureManifest()); err == nil {
		t.Fatal("a record taken over a tenth of the documented window was accepted")
	} else if !strings.Contains(err.Error(), "idle_window_seconds") {
		t.Errorf("the refusal %q does not name the parameter that moved", err)
	}
}

// The three shape claims that carry across hosts, one control each.
func TestTheShapeOfTheClaimIsCheckedFromTheRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(r *Record)
		want   string
	}{
		{
			name: "a sidecar",
			break_: func(r *Record) {
				r.Deployment.Containers = append(r.Deployment.Containers, Container{
					Name: "backup-manager-metrics", Image: "prom/exporter:1", Command: "/exporter",
				})
			},
			want: "container",
		},
		{
			name: "a second application server",
			break_: func(r *Record) {
				r.Deployment.Containers[1].Image = "nginx:1.27"
				r.Deployment.Containers[1].ImageID = "sha256:bb"
				r.Deployment.Containers[1].Command = "nginx -g daemon off;"
			},
			want: "image",
		},
		{
			name: "an added data-path hop",
			break_: func(r *Record) {
				r.Deployment.Containers[0].PublishedPorts = []string{"9444:8080"}
			},
			want: "port",
		},
		{
			name: "a build that is not the canonical release",
			break_: func(r *Record) {
				r.Release.BinarySHA256["backup-manager-web"] = "0000"
			},
			want: "binary_sha256",
		},
		{
			name: "the other architecture's binary",
			break_: func(r *Record) {
				r.Release.BinarySHA256["backup-manager-web"] = "def456"
			},
			want: "binary_sha256",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := verifyFixture(t, tc.break_)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.Passed {
				t.Fatalf("the shape check missed %s:\n%s", tc.name, res.Report())
			}
			var hit bool
			for _, s := range res.Shape {
				if s.Verdict == Fail && strings.Contains(s.Check+" "+s.Detail, tc.want) {
					hit = true
				}
			}
			if !hit {
				t.Errorf("no shape check mentioning %q failed:\n%s", tc.want, res.Report())
			}
		})
	}
}

// A registry digest nobody has pushed cannot be compared, and the honest
// report of that is "unrecorded", not a quiet pass.
func TestAnUnrecordedRegistryDigestIsReportedRatherThanPassed(t *testing.T) {
	res, err := verifyFixture(t, nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var found bool
	for _, s := range res.Shape {
		if strings.Contains(s.Check, "registry digest") {
			found = true
			if !strings.Contains(s.Detail, "unrecorded") {
				t.Errorf("the registry digest check says %q, want it to say the digest is unrecorded", s.Detail)
			}
		}
	}
	if !found {
		t.Error("nothing in the report mentions the registry digest at all")
	}
}

// The status output is what answers "is arm64 certified", and it has to
// answer it without a record rather than leaving the question open.
func TestStatusSeparatesCertifiedFromUncertified(t *testing.T) {
	p := mustParseFixture(t)
	root := t.TempDir()
	evidence := filepath.Join(root, "docs", "acceptance", "evidence")
	mustWrite(t, evidence, "ugos-resource-amd64.json", fixtureRecord())

	sts, err := Status(p, root, fixtureManifest())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(sts) != 2 {
		t.Fatalf("Status reported %d architectures, want 2", len(sts))
	}
	byArch := map[string]ArchStatus{}
	for _, s := range sts {
		byArch[s.Architecture] = s
	}
	if got := byArch["amd64"].Status; got != Certified {
		t.Errorf("amd64 = %q, want %q (%s)", got, Certified, byArch["amd64"].Reason)
	}
	if got := byArch["arm64"].Status; got != Uncertified {
		t.Errorf("arm64 = %q, want %q", got, Uncertified)
	}
	if !strings.Contains(byArch["arm64"].Reason, "no evidence record") {
		t.Errorf("arm64 reason = %q, want it to say there is no record", byArch["arm64"].Reason)
	}

	t.Run("a record that fails its thresholds is not certified", func(t *testing.T) {
		root := t.TempDir()
		evidence := filepath.Join(root, "docs", "acceptance", "evidence")
		r := fixtureRecord()
		r.Measurements["idle_rss_bytes"] = 200_000_000
		mustWrite(t, evidence, "ugos-resource-amd64.json", r)

		sts, err := Status(p, root, fixtureManifest())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		for _, s := range sts {
			if s.Architecture == "amd64" && s.Status != Failed {
				t.Errorf("amd64 = %q, want %q", s.Status, Failed)
			}
		}
	})

	t.Run("an arm64 record filed under amd64 certifies neither", func(t *testing.T) {
		root := t.TempDir()
		evidence := filepath.Join(root, "docs", "acceptance", "evidence")
		r := fixtureRecord()
		r.Architecture = "arm64"
		r.Probe.GOARCH = "arm64"
		r.Device.UnameMachine = "aarch64"
		r.Release.BinarySHA256["backup-manager-web"] = "def456"
		// Filed at the amd64 path. Copying an arm64 run over the amd64
		// record is the exact mistake that would let one architecture's
		// evidence stand in for the other's.
		mustWrite(t, evidence, "ugos-resource-amd64.json", r)

		sts, err := Status(p, root, fixtureManifest())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		for _, s := range sts {
			if s.Status == Certified {
				t.Errorf("%s came out certified from a misfiled record: %s", s.Architecture, s.Reason)
			}
		}
	})
}

func failed(res *Result, metric string) bool {
	for _, m := range res.Metrics {
		if m.Name == metric && m.Verdict == Fail {
			return true
		}
	}
	return false
}

func mustWrite(t *testing.T, dir, name string, r *Record) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFile(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

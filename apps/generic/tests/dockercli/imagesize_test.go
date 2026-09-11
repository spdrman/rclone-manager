// This file is issue #635's answer to "the only automated statement
// about image_size_bytes is that a file exists".
//
// scripts/perf/check-baseline.sh has two modes. The gate runs the first
// one, which asks whether a complete baseline record is checked in for
// the designated host. The second, --compare, is the one that evaluates
// a threshold, and it needs a freshly captured candidate record: the
// runtime harness (five process/API metrics, five repeats, a 30-second
// idle window in each) plus the transfer harness plus an image build.
// That is minutes, and the timing half of it is measured badly on a
// machine that is simultaneously running the rest of the gate, so
// scripts/ci-local.sh has never run it. The consequence was a baseline
// that drifted to 1.62x of the gated ratio with nothing going red
// anywhere, for eight days and 834 commits (#635).
//
// One of the seven metrics is not like the other six. gate.json's own
// justification for image_size_bytes says "two independent builds of the
// same commit produced byte-identical image sizes, so there is no noise
// to allow for", and that is the whole difference: it is a deterministic
// function of the tree and the target architecture, not a timing
// measurement. Load on the machine cannot move it. #635 re-confirmed
// that by building 8ad3100 eight days and 834 commits later, on a host
// under a load average of 4.76, and getting 43,008,762 bytes: the exact
// value the baseline recorded on 2026-08-31.
//
// So this metric can be gated on every run, and it belongs here rather
// than in scripts/ci-local.sh, because this package already builds
// container/Dockerfile once per test process for the licence and CLI
// proofs. Reading a size off an image that exists costs one `docker
// image inspect`. A step in the gate would have had to build it a second
// time.
//
// That also settles the one objection a perf number in the gate normally
// meets here. python3 scripts/bdtools/perf/capture_baseline.py says in as many words that
// its two harnesses never run under -race, because the detector slows an
// instrumented binary by several times and a baseline captured from one
// could never be compared against an uninstrumented run. scripts/ci-local.sh
// runs this package with -race. That is fine, and only for this metric:
// the flag instruments the test binary, and the test binary does not
// appear in the number. `docker build` produces the same bytes either way.
//
// What this does NOT do is replace --compare. The six timing metrics
// still need the harnesses, a quiet machine and a deliberate capture;
// this makes exactly one statement, about the one metric that can be
// made honestly for free.

package dockercli_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// perfGate is the part of docs/perf/gate.json this test needs.
//
// Read as JSON rather than duplicated here for the reason
// imagelicences_test.go gives about compliance.json: the file is the
// interface, and a threshold this test hard-coded would be a second
// statement of the gate that could drift from the first one silently.
type perfGate struct {
	BenchmarkHostID string `json:"benchmark_host_id"`
	Thresholds      map[string]struct {
		Direction string  `json:"direction"`
		MaxRatio  float64 `json:"max_ratio"`
		// Declared here only so it can be refused. This arm implements
		// the ratio and nothing else; check-baseline.sh's compare mode
		// implements a second condition on top of it, and the two must
		// not be allowed to drift apart silently. See readPerfGate.
		NoiseFloorAbs float64 `json:"noise_floor_abs"`
	} `json:"thresholds"`
}

// perfBaseline is the part of docs/perf/baselines/<host>.json this test
// needs. The architecture next to the value is not decoration: an image
// size is a property of the tree AND the target architecture, and
// comparing across the two reports the architecture.
type perfBaseline struct {
	Metrics struct {
		ImageSize struct {
			Value        int64  `json:"value"`
			Architecture string `json:"architecture"`
		} `json:"image_size_bytes"`
	} `json:"metrics"`
}

func readPerfGate(t *testing.T, root string) perfGate {
	t.Helper()
	path := filepath.Join(root, "docs", "perf", "gate.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	var g perfGate
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatalf("cannot parse %s: %v", path, err)
	}
	if err := validatePerfGate(path, g); err != nil {
		t.Fatal(err)
	}
	return g
}

// validatePerfGate is every precondition readPerfGate enforces, as a value
// rather than as a t.Fatalf.
//
// Split out for the reason this file's header gives about the arithmetic:
// a negative assertion nobody has watched fail is indistinguishable from
// one that cannot, and a guard that can only report through *testing.T
// cannot be driven by a test at all. These five are the whole reason a
// deleted threshold cannot silently delete its own gate, so they are the
// last guards that should be untested.
// TestTheGateAndBaselineReadersRefuseWhatTheyMustRefuse drives them.
//
// The fail-open shapes here are the ones check-baseline.sh already carries,
// in the same order and for the same reason: a gate that names no host, or
// whose image_size_bytes threshold has gone missing or arrived as a zero
// ratio, must stop this test rather than let it compare against nothing and
// report a pass. That is the failure Phase 4 learned on the conformance
// matrix, where an omitted capability had to fail rather than shrink the
// matrix.
func validatePerfGate(path string, g perfGate) error {
	if strings.TrimSpace(g.BenchmarkHostID) == "" {
		return fmt.Errorf("%s names no benchmark_host_id, so there is no baseline record to compare against", path)
	}
	th, ok := g.Thresholds["image_size_bytes"]
	if !ok {
		return fmt.Errorf("%s no longer carries a thresholds.image_size_bytes entry, so this test would gate nothing and pass", path)
	}
	if th.Direction != "lower_is_better" {
		return fmt.Errorf("%s declares thresholds.image_size_bytes.direction %q; this test only knows how to enforce lower_is_better, and reporting a pass against a rule it does not implement would be worse than failing", path, th.Direction)
	}
	if th.MaxRatio <= 1 {
		return fmt.Errorf("%s declares thresholds.image_size_bytes.max_ratio %v, which is not a budget above the baseline; a ratio of 1 or less would fail every run and a zero would fail none", path, th.MaxRatio)
	}

	// The one place this arm can quietly become a different gate from
	// scripts/perf/check-baseline.sh, so it refuses rather than diverges.
	//
	// That script's compare mode applies TWO conditions to a
	// lower_is_better metric: over the ratio AND further from baseline
	// than noise_floor_abs, where an absent floor is 0 and the ratio is
	// the whole rule. image_size_bytes carries no floor today, on the
	// stated grounds that two builds of one commit produce byte-identical
	// sizes and there is nothing to allow for, so one condition is the
	// same gate as two and overImageSizeBudget implements just the ratio.
	//
	// Add a floor to that metric later and this arm would keep enforcing
	// the ratio alone, which is STRICTER than the gate anyone reading
	// gate.json would expect: it would fail an image the documented rule
	// passes, in a test that never mentions the field it ignored. A
	// decoder that drops a key it does not know about is exactly how that
	// happens without anyone choosing it, so perfGate declares the field
	// purely so this line can see it.
	if th.NoiseFloorAbs != 0 {
		return fmt.Errorf("%s gives thresholds.image_size_bytes a noise_floor_abs of %v, and this test implements the ratio only.\n\n"+
			"scripts/perf/check-baseline.sh --compare requires BOTH conditions before it fails a lower_is_better metric, so with a floor set this test is the stricter of the two and would refuse an image that gate passes.\n\n"+
			"Implement the floor in overImageSizeBudget and extend TestTheImageSizeBudgetRefusesOneByteOverTheCeiling to visit both sides of it, the way scripts/perf/selftest.sh does for api_read_p95_ms, or take the floor back out.", path, th.NoiseFloorAbs)
	}
	return nil
}

func readPerfBaseline(t *testing.T, root, hostID string) perfBaseline {
	t.Helper()
	path := filepath.Join(root, "docs", "perf", "baselines", hostID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v\n\nscripts/perf/check-baseline.sh fails on this too, and says how to produce it: python3 scripts/bdtools/perf/capture_baseline.py on the designated benchmark host.", path, err)
	}
	var b perfBaseline
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatalf("cannot parse %s: %v", path, err)
	}
	if err := validatePerfBaseline(path, b); err != nil {
		t.Fatal(err)
	}
	return b
}

// validatePerfBaseline is readPerfBaseline's preconditions as a value, for
// the same reason validatePerfGate is.
func validatePerfBaseline(path string, b perfBaseline) error {
	if b.Metrics.ImageSize.Value <= 0 {
		return fmt.Errorf("%s records metrics.image_size_bytes.value %d, so there is no baseline to compare against; a --skip-image capture produces exactly this", path, b.Metrics.ImageSize.Value)
	}
	if strings.TrimSpace(b.Metrics.ImageSize.Architecture) == "" {
		return fmt.Errorf("%s records no architecture next to metrics.image_size_bytes.value, so nothing can tell whether a built image is comparable to it", path)
	}
	return nil
}

// inspectImageSize reads the architecture and total size off a built
// image. `docker image inspect`'s .Size is the sum of the layer sizes,
// which is the same number python3 scripts/bdtools/perf/capture_baseline.py records, so
// the two are directly comparable.
//
// Labels do not enter into it, which had to be checked rather than
// assumed, because buildImage stamps three of them and
// capture-baseline.sh stamps none: labels live in the image config, not
// in a layer, and building this tree with and without them produced
// 69,704,266 both ways on this host on 2026-09-08.
//
// runDocker, which means `.Output()` and the package timeout, and which
// matters twice over for a call whose output is PARSED. CombinedOutput
// folds stderr into the same buffer, so any warning the CLI writes on an
// otherwise successful call arrives inside the string these fields are
// split out of, and the measurement fails as "printed %q, which is not an
// architecture and a size" while the reader chases the warning instead of
// the cause. Every other parsed `inspect --format` in this package uses
// `.Output()` for that reason; CombinedOutput is reserved for `docker cp`,
// `logs` and `tag`, where a human-readable log is the point. Nothing is
// lost on failure either, because exec.ExitError carries stderr.
//
// runDocker rather than the dockerRun variable, because the sweep tests
// replace that one with a stub, and a stubbed sweep must not be able to
// reroute the measurement this whole file exists to take.
func inspectImageSize(t *testing.T, image string) (arch string, size int64) {
	t.Helper()
	out, err := runDocker("image", "inspect", image, "--format", "{{.Architecture}} {{.Size}}")
	if err != nil {
		stderr := ""
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			stderr = "\n" + string(exit.Stderr)
		}
		t.Fatalf("docker image inspect %s: %v%s", image, err, stderr)
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		t.Fatalf("docker image inspect %s printed %q, which is not an architecture and a size", image, out)
	}
	size, err = strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("docker image inspect %s reported size %q, which is not a number: %v", image, fields[1], err)
	}
	if size <= 0 {
		t.Fatalf("docker image inspect %s reported size %d; an image that measures nothing is a broken measurement, not a small image", image, size)
	}
	return fields[0], size
}

// perfHostID answers "is this the machine the baseline was captured on?"
// by asking the file that owns the question.
//
// Shelling out to scripts/perf/hostid.sh rather than recomputing the slug
// in Go, because the slug is a rule (os-arch-model, lowercased, runs
// outside [a-z0-9._-] collapsed to a dash) and a second implementation of
// a rule is a second thing to keep in step. check-baseline.sh and
// capture-baseline.sh both source that file; this reads the same answer
// they do, so the three cannot disagree about which machine this is.
//
// Fatal rather than "assume not the benchmark host" when it cannot be
// asked. The script is checked in next to this one and bash runs every
// other gate step, so a failure here is a broken environment rather than
// a machine that happens not to be the benchmark host, and treating "I
// could not tell" as "not it" is how the branch above would go back to
// skipping on the one host it must not skip on.
func perfHostID(t *testing.T, root string) string {
	t.Helper()
	cmd := exec.Command("bash", "-c", "source scripts/perf/hostid.sh && perf::host_id")
	// hostid.sh is sourced by a repo-root-relative path, the way the two
	// perf scripts source it, so this has to run from the root.
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			stderr = "\n" + string(exit.Stderr)
		}
		t.Fatalf("cannot read this machine's perf host id out of scripts/perf/hostid.sh: %v%s\n\nThat file is what check-baseline.sh and capture-baseline.sh both use to decide which machine a baseline belongs to, so without it this test cannot tell whether an architecture mismatch is a missing baseline or a broken platform pin.", err, stderr)
	}
	return strings.TrimSpace(string(out))
}

// overImageSizeBudget is the whole decision, split out from the test that
// needs Docker so it can be controlled by one that does not.
//
// This is the same comparison scripts/perf/check-baseline.sh --compare
// makes for a lower_is_better metric with no noise floor: over the ratio
// is over, and the ratio is the whole rule. image_size_bytes carries no
// floor because it has no noise to allow one for.
func overImageSizeBudget(size, base int64, ratio float64) (limit int64, over bool) {
	limit = int64(float64(base) * ratio)
	return limit, size > limit
}

// TestTheImageSizeBudgetRefusesOneByteOverTheCeiling is the positive
// control for the rule above, and it exists for the reason
// scripts/perf/selftest.sh exists: every assertion the gate makes is a
// negative one, and a negative assertion nobody has watched fail is
// indistinguishable from one that cannot. The test below it needs a
// Docker daemon and a built image before it can compare anything, so on
// a machine without one it would report nothing either way; this needs
// neither, and pins the boundary from both sides.
//
// Two halves, because there are two things that can rot independently.
// The arithmetic is pinned on fixed numbers that no file can move, so the
// rule stays checked even if every record changes. The WIRING is then
// checked against the real docs/perf/gate.json and the real record, so a
// ratio that moved there and nowhere else cannot leave this test quietly
// enforcing a number of its own: that is the defect #635 is about, and a
// control that hardcoded 1.05 would have been an instance of it.
func TestTheImageSizeBudgetRefusesOneByteOverTheCeiling(t *testing.T) {
	t.Run("the arithmetic, on numbers no file can move", func(t *testing.T) {
		const base = 1000
		const ratio = 1.05
		ceiling, _ := overImageSizeBudget(0, base, ratio)
		if ceiling != 1050 {
			t.Fatalf("a 1.05x ceiling on %d came out at %d, not 1050", base, ceiling)
		}
		for _, tc := range []struct {
			name string
			size int64
			want bool
		}{
			{"the baseline itself", base, false},
			{"one byte under the ceiling", ceiling - 1, false},
			{"exactly the ceiling", ceiling, false},
			{"one byte over the ceiling", ceiling + 1, true},
			{"far under", base / 2, false},
			{"the 1.62x drift #635 found", base * 162 / 100, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if _, over := overImageSizeBudget(tc.size, base, ratio); over != tc.want {
					t.Errorf("overImageSizeBudget(%d, %d, %v) said over=%v, want %v", tc.size, base, ratio, over, tc.want)
				}
			})
		}
	})

	t.Run("the wiring, against the files the gate actually reads", func(t *testing.T) {
		root := repoRoot(t)
		gate := readPerfGate(t, root)
		baseline := readPerfBaseline(t, root, gate.BenchmarkHostID)
		base := baseline.Metrics.ImageSize.Value
		ratio := gate.Thresholds["image_size_bytes"].MaxRatio

		// Reading both real files is itself the negative control for the
		// five guards in validatePerfGate and the two in
		// validatePerfBaseline: the checked-in pair must pass all seven.
		ceiling, _ := overImageSizeBudget(0, base, ratio)
		if want := int64(float64(base) * ratio); ceiling != want {
			t.Fatalf("the ceiling this test enforces (%d) is not max_ratio %v applied to the recorded %d (%d)", ceiling, ratio, base, want)
		}
		if _, over := overImageSizeBudget(ceiling+1, base, ratio); !over {
			t.Errorf("one byte over the ceiling derived from the real gate and record (%d) is not over budget", ceiling+1)
		}
		if _, over := overImageSizeBudget(base, base, ratio); over {
			t.Errorf("the recorded size %d is over its own budget, so the gate would fail against the tree it was captured from", base)
		}

		// And the case the whole issue was about, from the other side: the
		// record that was replaced, against the image that had grown away
		// from it. If this ever stops reporting over-budget, the rule has
		// stopped working.
		const driftedFrom = 43008762 // the record at 8ad3100, replaced by #635
		if _, over := overImageSizeBudget(base, driftedFrom, ratio); !over {
			t.Errorf("%d bytes against the %d baseline it replaced must be over budget; that is the drift #635 found, and a rule that passes it gates nothing", base, driftedFrom)
		}
	})
}

// TestTheGateAndBaselineReadersRefuseWhatTheyMustRefuse drives every
// precondition the two readers enforce, which none of them could be while
// they only spoke through t.Fatalf.
//
// These are the guards that stop a deleted threshold from silently
// deleting its own gate, so "we believe they fire" is the weakest possible
// footing for them. Each case mutates one field of a value that would
// otherwise be accepted, and requires a refusal that names the reason;
// asserting the reason rather than just non-nil is what stops a guard that
// fires for the wrong cause from counting as a control, which is the same
// discipline scripts/perf/selftest.sh's expect_fail applies.
func TestTheGateAndBaselineReadersRefuseWhatTheyMustRefuse(t *testing.T) {
	good := func() perfGate {
		g := perfGate{BenchmarkHostID: "some-host"}
		g.Thresholds = map[string]struct {
			Direction string  `json:"direction"`
			MaxRatio  float64 `json:"max_ratio"`
			// Declared here only so it can be refused. This arm implements
			// the ratio and nothing else; check-baseline.sh's compare mode
			// implements a second condition on top of it, and the two must
			// not be allowed to drift apart silently. See validatePerfGate.
			NoiseFloorAbs float64 `json:"noise_floor_abs"`
		}{"image_size_bytes": {Direction: "lower_is_better", MaxRatio: 1.05}}
		return g
	}

	t.Run("gate", func(t *testing.T) {
		if err := validatePerfGate("g.json", good()); err != nil {
			t.Fatalf("the unmutated gate must be accepted, or every case below proves nothing: %v", err)
		}
		for _, tc := range []struct {
			name   string
			mutate func(*perfGate)
			expect string
		}{
			{"no benchmark host", func(g *perfGate) { g.BenchmarkHostID = "  " }, "names no benchmark_host_id"},
			{"the threshold deleted", func(g *perfGate) { delete(g.Thresholds, "image_size_bytes") }, "would gate nothing and pass"},
			{"the wrong direction", func(g *perfGate) {
				th := g.Thresholds["image_size_bytes"]
				th.Direction = "higher_is_better"
				g.Thresholds["image_size_bytes"] = th
			}, "only knows how to enforce lower_is_better"},
			{"a ratio of exactly 1", func(g *perfGate) {
				th := g.Thresholds["image_size_bytes"]
				th.MaxRatio = 1
				g.Thresholds["image_size_bytes"] = th
			}, "not a budget above the baseline"},
			{"a ratio of zero", func(g *perfGate) {
				th := g.Thresholds["image_size_bytes"]
				th.MaxRatio = 0
				g.Thresholds["image_size_bytes"] = th
			}, "not a budget above the baseline"},
			{"a noise floor this arm does not implement", func(g *perfGate) {
				th := g.Thresholds["image_size_bytes"]
				th.NoiseFloorAbs = 1024
				g.Thresholds["image_size_bytes"] = th
			}, "implements the ratio only"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				g := good()
				tc.mutate(&g)
				err := validatePerfGate("g.json", g)
				if err == nil {
					t.Fatalf("accepted a gate it must refuse")
				}
				if !strings.Contains(err.Error(), tc.expect) {
					t.Errorf("refused, but not for the required reason.\n  want the message to contain: %s\n  got: %v", tc.expect, err)
				}
			})
		}
	})

	t.Run("baseline", func(t *testing.T) {
		good := func() perfBaseline {
			var b perfBaseline
			b.Metrics.ImageSize.Value = 69704266
			b.Metrics.ImageSize.Architecture = "arm64"
			return b
		}
		if err := validatePerfBaseline("b.json", good()); err != nil {
			t.Fatalf("the unmutated baseline must be accepted, or every case below proves nothing: %v", err)
		}
		for _, tc := range []struct {
			name   string
			mutate func(*perfBaseline)
			expect string
		}{
			{"a null image size, which is what --skip-image writes", func(b *perfBaseline) { b.Metrics.ImageSize.Value = 0 }, "no baseline to compare against"},
			{"a negative image size", func(b *perfBaseline) { b.Metrics.ImageSize.Value = -1 }, "no baseline to compare against"},
			{"no architecture beside the value", func(b *perfBaseline) { b.Metrics.ImageSize.Architecture = " " }, "records no architecture"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				b := good()
				tc.mutate(&b)
				err := validatePerfBaseline("b.json", b)
				if err == nil {
					t.Fatalf("accepted a baseline it must refuse")
				}
				if !strings.Contains(err.Error(), tc.expect) {
					t.Errorf("refused, but not for the required reason.\n  want the message to contain: %s\n  got: %v", tc.expect, err)
				}
			})
		}
	})
}

// TestTheBuiltImageIsInsideTheRecordedSizeBudget compares the image this
// package just built against the checked-in baseline, using the ratio
// docs/perf/gate.json pins. It is the same arithmetic
// scripts/perf/check-baseline.sh --compare applies to this metric, run
// against an image that already exists rather than against a candidate
// record somebody has to capture first.
//
// It reports the delta in both directions on failure, because the two
// possible causes want opposite responses and the number alone does not
// separate them. Either something in this change made the image bigger,
// which is what the gate is for; or the growth is intended and
// explained, in which case the baseline is re-captured on the designated
// host AND the explanation is written down, which is what #635 spent its
// time on. Re-capturing without the explanation is the one use a
// baseline must never be put to: it records the regression as the new
// normal.
func TestTheBuiltImageIsInsideTheRecordedSizeBudget(t *testing.T) {
	image := buildImage(t)
	root := repoRoot(t)

	gate := readPerfGate(t, root)
	baseline := readPerfBaseline(t, root, gate.BenchmarkHostID)
	arch, size := inspectImageSize(t, image)

	// An architecture this repository has no baseline for cannot be
	// compared to one it does. The record carries the architecture it was
	// captured at precisely so this is answerable rather than guessed.
	//
	// But a skip is only the right answer somewhere the baseline does not
	// apply, and on the designated benchmark host it always applies. There
	// the record was captured on this machine, for this machine's own
	// architecture, so a mismatch cannot mean "no baseline here": it means
	// the platform pin in imagesweep_test.go's TestMain stopped working and
	// the build went somewhere else. Skipping on that is how this gate
	// deletes itself while the suite stays green, which is #635's own
	// thesis one level down: `go test` prints ok, gate_docker_step sees a
	// zero exit, nothing reaches the ledger, and the run ends
	// `ci-local: ok` having asserted nothing about the image at all.
	//
	// So: fail here, skip elsewhere.
	want := baseline.Metrics.ImageSize.Architecture
	if arch != want {
		if host := perfHostID(t, root); host == gate.BenchmarkHostID {
			t.Fatalf("this IS the designated benchmark host %q, and the image just built is %s while its own baseline records %s.\n\n"+
				"That is not a missing baseline, because the record was captured on this machine for this machine. It means the build went to a different platform than the daemon's, so the pin in imagesweep_test.go's TestMain is not doing its job. DOCKER_DEFAULT_PLATFORM in this process is %q.\n\n"+
				"Skipping here would leave the whole image-size gate silently inert on the one machine where it can fire, which is the failure #635 exists to close.",
				host, arch, want, os.Getenv("DOCKER_DEFAULT_PLATFORM"))
		}
		t.Skipf("the built image is %s and the only checked-in baseline is %s (docs/perf/baselines/%s.json), so there is nothing on this architecture to compare against. An %s baseline would have to be captured on a designated %s benchmark host before this could gate anything here.", arch, want, gate.BenchmarkHostID, arch, arch)
	}

	base := baseline.Metrics.ImageSize.Value
	ratio := gate.Thresholds["image_size_bytes"].MaxRatio
	limit, over := overImageSizeBudget(size, base, ratio)
	if over {
		t.Errorf("the built image is %d bytes against a baseline of %d and a ceiling of %d (%.2fx): %.4fx, +%d bytes.\n\n"+
			"docs/perf/gate.json gates this metric on the ratio alone, because two builds of one commit produce byte-identical sizes and there is no noise to allow for. So this is a real move, and it wants one of two answers.\n\n"+
			"  If this change made the image bigger and did not mean to, that is the regression this gate exists to catch. `docker history` on the image names the layer.\n\n"+
			"  If the growth is intended, account for it first and re-capture second: python3 scripts/bdtools/perf/capture_baseline.py on %s, with what accounts for the growth written into docs/perf/README.md. A re-capture with no accounting behind it records the regression as the new normal, which is the one thing a baseline must not be used for (#635).",
			size, base, limit, ratio, float64(size)/float64(base), size-limit, gate.BenchmarkHostID)
	}
}

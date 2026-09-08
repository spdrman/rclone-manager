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
// meets here. scripts/perf/capture-baseline.sh says in as many words that
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

	// The fail-open guards check-baseline.sh already carries, in the
	// same shape and for the same reason. A gate that names no host, or
	// whose image_size_bytes threshold has gone missing or arrived as a
	// zero ratio, must stop this test rather than let it compare
	// against nothing and report a pass: a deleted threshold silently
	// deleting its own test is the failure Phase 4 learned on the
	// conformance matrix.
	if strings.TrimSpace(g.BenchmarkHostID) == "" {
		t.Fatalf("%s names no benchmark_host_id, so there is no baseline record to compare against", path)
	}
	th, ok := g.Thresholds["image_size_bytes"]
	if !ok {
		t.Fatalf("%s no longer carries a thresholds.image_size_bytes entry, so this test would gate nothing and pass", path)
	}
	if th.Direction != "lower_is_better" {
		t.Fatalf("%s declares thresholds.image_size_bytes.direction %q; this test only knows how to enforce lower_is_better, and reporting a pass against a rule it does not implement would be worse than failing", path, th.Direction)
	}
	if th.MaxRatio <= 1 {
		t.Fatalf("%s declares thresholds.image_size_bytes.max_ratio %v, which is not a budget above the baseline; a ratio of 1 or less would fail every run and a zero would fail none", path, th.MaxRatio)
	}
	return g
}

func readPerfBaseline(t *testing.T, root, hostID string) perfBaseline {
	t.Helper()
	path := filepath.Join(root, "docs", "perf", "baselines", hostID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v\n\nscripts/perf/check-baseline.sh fails on this too, and says how to produce it: scripts/perf/capture-baseline.sh on the designated benchmark host.", path, err)
	}
	var b perfBaseline
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatalf("cannot parse %s: %v", path, err)
	}
	if b.Metrics.ImageSize.Value <= 0 {
		t.Fatalf("%s records metrics.image_size_bytes.value %d, so there is no baseline to compare against; a --skip-image capture produces exactly this", path, b.Metrics.ImageSize.Value)
	}
	if strings.TrimSpace(b.Metrics.ImageSize.Architecture) == "" {
		t.Fatalf("%s records no architecture next to metrics.image_size_bytes.value, so nothing can tell whether a built image is comparable to it", path)
	}
	return b
}

// inspectImageSize reads the architecture and total size off a built
// image. `docker image inspect`'s .Size is the sum of the layer sizes,
// which is the same number scripts/perf/capture-baseline.sh records, so
// the two are directly comparable.
//
// Labels do not enter into it, which had to be checked rather than
// assumed, because buildImage stamps three of them and
// capture-baseline.sh stamps none: labels live in the image config, not
// in a layer, and building this tree with and without them produced
// 69,704,266 both ways on this host on 2026-09-08.
func inspectImageSize(t *testing.T, image string) (arch string, size int64) {
	t.Helper()
	out, err := exec.Command("docker", "image", "inspect", image, "--format", "{{.Architecture}} {{.Size}}").CombinedOutput()
	if err != nil {
		t.Fatalf("docker image inspect %s: %v\n%s", image, err, out)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
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
func TestTheImageSizeBudgetRefusesOneByteOverTheCeiling(t *testing.T) {
	const base = 69704266 // the record at 186ba0c7, so the arithmetic is at a real magnitude
	const ratio = 1.05

	ceiling, _ := overImageSizeBudget(0, base, ratio)
	if ceiling != 73189479 {
		t.Fatalf("a 1.05x ceiling on %d came out at %d, not 73189479; every case below is written against that number", base, ceiling)
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
		{"the growth #635 found", 69704266, false},
		{"that growth against the record it replaced", 43008762, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, over := overImageSizeBudget(tc.size, base, ratio); over != tc.want {
				t.Errorf("overImageSizeBudget(%d, %d, %v) said over=%v, want %v", tc.size, base, ratio, over, tc.want)
			}
		})
	}

	// And the case the whole issue was about, from the other side: the
	// old record against the image that had grown away from it. If this
	// ever stops reporting over-budget, the rule has stopped working.
	if _, over := overImageSizeBudget(69704266, 43008762, ratio); !over {
		t.Error("69,704,266 bytes against a 43,008,762 baseline is 1.62x and must be over budget; that is the drift #635 found, and a rule that passes it gates nothing")
	}
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
	// compared to one it does. The record carries the architecture it
	// was captured at precisely so this is answerable rather than
	// guessed, and buildImage now asks for a native build explicitly
	// (#635), so on the designated benchmark host's architecture this
	// always runs rather than always skipping.
	want := baseline.Metrics.ImageSize.Architecture
	if arch != want {
		t.Skipf("the built image is %s and the only checked-in baseline is %s (docs/perf/baselines/%s.json), so there is nothing on this architecture to compare against. An %s baseline would have to be captured on a designated %s benchmark host before this could gate anything here.", arch, want, gate.BenchmarkHostID, arch, arch)
	}

	base := baseline.Metrics.ImageSize.Value
	ratio := gate.Thresholds["image_size_bytes"].MaxRatio
	limit, over := overImageSizeBudget(size, base, ratio)
	if over {
		t.Errorf("the built image is %d bytes against a baseline of %d and a ceiling of %d (%.2fx): %.4fx, +%d bytes.\n\n"+
			"docs/perf/gate.json gates this metric on the ratio alone, because two builds of one commit produce byte-identical sizes and there is no noise to allow for. So this is a real move, and it wants one of two answers.\n\n"+
			"  If this change made the image bigger and did not mean to, that is the regression this gate exists to catch. `docker history` on the image names the layer.\n\n"+
			"  If the growth is intended, account for it first and re-capture second: scripts/perf/capture-baseline.sh on %s, with what accounts for the growth written into docs/perf/README.md. A re-capture with no accounting behind it records the regression as the new normal, which is the one thing a baseline must not be used for (#635).",
			size, base, limit, ratio, float64(size)/float64(base), size-limit, gate.BenchmarkHostID)
	}
}

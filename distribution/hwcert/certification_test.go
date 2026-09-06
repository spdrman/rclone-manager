package hwcert

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// RepoRoot is the repository root relative to this package's own
// directory, which is where `go test` runs. Same two levels
// distribution/packaging uses, for the same reason: the things being read
// (an acceptance procedure, a Compose file, a release manifest) are not
// importable Go.
const RepoRoot = "../.."

func realProcedure(t *testing.T) *Procedure {
	t.Helper()
	p, err := ParseProcedure(filepath.Join(RepoRoot, ProcedurePath))
	if err != nil {
		t.Fatalf("the checked-in acceptance procedure does not parse: %v", err)
	}
	return p
}

func realManifest(t *testing.T) *ReleaseManifest {
	t.Helper()
	m, err := ReadReleaseManifest(filepath.Join(RepoRoot, "container", "release-manifest.json"))
	if err != nil {
		t.Fatalf("container/release-manifest.json: %v", err)
	}
	return m
}

// The seven metrics EPIC B #81's performance contract names, spelled the
// way docs/perf/gate.json spells them where the two overlap. #89 says the
// UGOS measurement set should line up with the contract so the numbers are
// comparable rather than a separate island, and this is that sentence with
// a check behind it. Nothing here compares a UGOS number to a docs/perf
// number; what is held in common is which quantities get measured.
func TestTheProcedureMeasuresEveryMetricThePerformanceContractNames(t *testing.T) {
	p := realProcedure(t)
	for _, name := range []string{
		"idle_rss_bytes",
		"idle_cpu_percent",
		"startup_to_healthy_ms",
		"api_read_p95_ms",
		"config_write_p95_ms",
		"transfer_mb_per_second",
		"image_size_bytes",
	} {
		if _, ok := p.Metric(name); !ok {
			t.Errorf("%s declares no %s, which EPIC B #81's performance contract names", ProcedurePath, name)
		}
	}

	// §73 WP 5.3's own three, which the contract does not name: what a
	// transfer costs, what it costs the API, and what the app costs
	// ordinary NAS use.
	for _, name := range []string{
		"transfer_cpu_percent",
		"api_read_under_transfer_ratio",
		"share_read_throughput_ratio",
	} {
		if _, ok := p.Metric(name); !ok {
			t.Errorf("%s declares no %s, which §73 WP 5.3 asks for", ProcedurePath, name)
		}
	}

	// Every `recorded` metric has to feed a gated one. A metric that is
	// required to be present and read by no criterion is a number in an
	// evidence record that nothing can fail.
	for _, m := range p.Metrics {
		if m.Gated() {
			continue
		}
		if gatedThrough(p, m.Name) == "nothing" {
			t.Errorf("%s is recorded and gated through nothing, so no criterion reads it", m.Name)
		}
	}
}

// The claimed set is not this document's to shrink. If it could be, an
// architecture that failed could be quietly dropped from the table and the
// status output would go clean.
func TestTheProcedureClaimsExactlyTheArchitecturesTheReleaseBuilds(t *testing.T) {
	p := realProcedure(t)
	m := realManifest(t)

	built := map[string]bool{}
	for _, a := range m.Architectures {
		built[a.Architecture] = true
	}
	claimed := map[string]bool{}
	for _, a := range p.ClaimedArchitectures() {
		claimed[a] = true
	}
	for a := range built {
		if !claimed[a] {
			t.Errorf("container/release-manifest.json builds %s and %s does not claim it, so nothing would ever ask for %s evidence", a, ProcedurePath, a)
		}
	}
	for a := range claimed {
		if !built[a] {
			t.Errorf("%s claims %s and container/release-manifest.json does not build it", ProcedurePath, a)
		}
	}
}

// Several thresholds say, in as many words, that they come from the
// canonical runtime's own declared resource expectations. That claim is
// only worth making if it stays true, so this reads both and compares
// them. Changing what an operator is told to provision without changing
// what the app is certified against is exactly the drift this catches.
func TestTheThresholdsDerivedFromTheCanonicalRuntimeStillMatchIt(t *testing.T) {
	p := realProcedure(t)

	data, err := os.ReadFile(filepath.Join(RepoRoot, "container", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Canonical struct {
			Resources map[string]struct {
				MemoryIdle     string `yaml:"memory_idle"`
				CPURecommended string `yaml:"cpu_recommended"`
			} `yaml:"resources"`
		} `yaml:"x-canonical-runtime"`
		Services map[string]struct {
			Healthcheck struct {
				StartPeriod string `yaml:"start_period"`
			} `yaml:"healthcheck"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &compose); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		metric string
		want   float64
		from   string
	}{
		{"idle_rss_bytes", mustBytes(t, compose.Canonical.Resources["engine"].MemoryIdle), "x-canonical-runtime.resources.engine.memory_idle"},
		{"ui_idle_rss_bytes", mustBytes(t, compose.Canonical.Resources["web-ui"].MemoryIdle), "x-canonical-runtime.resources.web-ui.memory_idle"},
		{"transfer_cpu_percent", mustCores(t, compose.Canonical.Resources["engine"].CPURecommended) * 100, "x-canonical-runtime.resources.engine.cpu_recommended"},
		{"startup_to_healthy_ms", mustSeconds(t, compose.Services["rclone-manager"].Healthcheck.StartPeriod) * 1000, "services.rclone-manager.healthcheck.start_period"},
	} {
		for _, arch := range p.ClaimedArchitectures() {
			got, ok := p.Threshold(tc.metric, arch)
			if !ok {
				t.Errorf("%s has no %s threshold", tc.metric, arch)
				continue
			}
			if got != tc.want {
				t.Errorf("%s (%s) is %g, and container/compose.yaml's %s says %g. One of the two moved without the other",
					tc.metric, arch, got, tc.from, tc.want)
			}
		}
	}
}

// The status question #89 asks, answered against the real tree. It is not
// a gate on the numbers, because there are none yet and §68 says
// build-supported and uncertified is an honest state. It is a gate on the
// claim: nothing may read as certified without a record behind it, and an
// architecture with no record has to say so rather than go unmentioned.
func TestNoArchitectureReadsAsCertifiedWithoutAnEvidenceRecord(t *testing.T) {
	p := realProcedure(t)
	sts, err := Status(p, RepoRoot, realManifest(t))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(sts) != len(p.ClaimedArchitectures()) {
		t.Fatalf("Status reported %d architectures for %d claimed", len(sts), len(p.ClaimedArchitectures()))
	}

	for _, s := range sts {
		t.Logf("%s: %s (%s)", s.Architecture, s.Status, s.Reason)
		switch s.Status {
		case Certified:
			if _, err := os.Stat(filepath.Join(RepoRoot, s.RecordPath)); err != nil {
				t.Errorf("%s reads as certified and %s does not exist", s.Architecture, s.RecordPath)
			}
		case Uncertified:
			if !strings.Contains(s.Reason, "no evidence record") {
				t.Errorf("%s is uncertified for a reason that does not say why: %q", s.Architecture, s.Reason)
			}
		case Failed:
			t.Errorf("%s has a record that does not hold up: %s", s.Architecture, s.Reason)
		default:
			t.Errorf("%s has status %q, which is none of the three", s.Architecture, s.Status)
		}
	}
}

// ---------------------------------------------------------------------

func mustBytes(t *testing.T, v string) float64 {
	t.Helper()
	switch {
	case strings.HasSuffix(v, "Mi"):
		return atof(t, strings.TrimSuffix(v, "Mi")) * 1024 * 1024
	case strings.HasSuffix(v, "Gi"):
		return atof(t, strings.TrimSuffix(v, "Gi")) * 1024 * 1024 * 1024
	}
	t.Fatalf("cannot read %q as a byte quantity", v)
	return 0
}

func mustCores(t *testing.T, v string) float64 {
	t.Helper()
	return atof(t, v)
}

func mustSeconds(t *testing.T, v string) float64 {
	t.Helper()
	if !strings.HasSuffix(v, "s") {
		t.Fatalf("cannot read %q as a duration in seconds", v)
	}
	return atof(t, strings.TrimSuffix(v, "s"))
}

func atof(t *testing.T, v string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		t.Fatalf("cannot read %q as a number: %v", v, err)
	}
	return f
}

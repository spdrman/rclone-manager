package hwcert

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureRecord is a complete, passing amd64 record against
// fixtureProcedure. Every test that wants a failure starts from this and
// breaks exactly one thing, so what it proves is that one thing.
func fixtureRecord() *Record {
	return &Record{
		Schema:       RecordSchema,
		Procedure:    "docs/acceptance/ugos-resource-certification.md",
		Provider:     "ugos",
		Architecture: "amd64",
		Probe:        Probe{GOARCH: "amd64", Version: "test"},
		Device: Device{
			Vendor:          "UGREEN",
			Model:           "DXP-test",
			OSName:          "UGOS Pro",
			FirmwareVersion: "1.2.3.4567",
			Kernel:          "5.15.0",
			UnameMachine:    "x86_64",
			CPUCores:        4,
			MemoryBytes:     8 << 30,
		},
		Release: Release{
			Version:        "0.3.0",
			ImageRef:       "backup-manager:0.3.0",
			BinarySHA256:   map[string]string{"backup-manager-web": "abc123"},
			RegistryDigest: "",
		},
		Deployment: Deployment{Containers: []Container{
			{Name: "rclone-manager", Image: "backup-manager:0.3.0", ImageID: "sha256:aa", Command: "/backup-manager-web serve --profile=ugos"},
			{Name: "web-ui", Image: "backup-manager:0.3.0", ImageID: "sha256:aa", Command: "/backup-manager-web serve-ui --profile=ugos", PublishedPorts: []string{"9443:8080"}},
		}},
		Method:  map[string]float64{"idle_window_seconds": 600, "idle_sample_interval_seconds": 5},
		Windows: map[string]Window{"engine_idle": idleSamples(121, 5, 0.002)},
		Measurements: map[string]float64{
			"idle_rss_bytes":         90_000_120,
			"transfer_mb_per_second": 180,
			"raw_copy_mb_per_second": 200,
		},
		CapturedAt: time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC),
		Operator:   "an operator",
	}
}

func fixtureManifest() *ReleaseManifest {
	return &ReleaseManifest{
		Version: "0.3.0",
		Architectures: []ManifestArchitecture{
			{Architecture: "amd64", BinarySHA256: map[string]string{"backup-manager-web": "abc123"}},
			{Architecture: "arm64", BinarySHA256: map[string]string{"backup-manager-web": "def456"}},
		},
	}
}

// The three architecture witnesses have to agree, and each one is written
// down by something different: the operator names the architecture, the
// probe binary reports the architecture it was compiled for, and the
// kernel reports the machine. A record that passes only because all three
// were typed in by the same person is not evidence, so the two that cannot
// be typed in are the ones that make an amd64 pass structurally unable to
// stand in for an arm64 one.
func TestARecordCannotClaimAnArchitectureItsWitnessesDisagreeWith(t *testing.T) {
	cases := []struct {
		name  string
		break_ func(r *Record)
		want  string
	}{
		{
			name:   "probe built for the other architecture",
			break_: func(r *Record) { r.Probe.GOARCH = "arm64" },
			want:   "probe.goarch",
		},
		{
			name:   "kernel reports the other machine",
			break_: func(r *Record) { r.Device.UnameMachine = "aarch64" },
			want:   "device.uname_machine",
		},
		{
			name:   "machine nobody maps to an architecture",
			break_: func(r *Record) { r.Device.UnameMachine = "vax" },
			want:   "vax",
		},
		{
			name:   "no architecture at all",
			break_: func(r *Record) { r.Architecture = "" },
			want:   "architecture",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := fixtureRecord()
			tc.break_(r)
			err := r.Validate()
			if err == nil {
				t.Fatal("Validate accepted a record whose architecture witnesses disagree")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	if err := fixtureRecord().Validate(); err != nil {
		t.Fatalf("the unbroken fixture failed to validate: %v", err)
	}
}

// §68's required evidence fields are what makes a record a certification
// record rather than a note. Firmware version is the one most likely to be
// left out, because it is the only one nothing on the device prints
// alongside the others.
func TestARecordWithoutTheCertificationFieldsIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(r *Record)
		want   string
	}{
		{"no firmware version", func(r *Record) { r.Device.FirmwareVersion = "" }, "firmware_version"},
		{"no model", func(r *Record) { r.Device.Model = "" }, "model"},
		{"no provider", func(r *Record) { r.Provider = "" }, "provider"},
		{"no release version", func(r *Record) { r.Release.Version = "" }, "release.version"},
		{"no capture time", func(r *Record) { r.CapturedAt = time.Time{} }, "captured_at"},
		{"no method block", func(r *Record) { r.Method = nil }, "method"},
		{"wrong schema", func(r *Record) { r.Schema = "something/else/9" }, "schema"},
		{"no measurements", func(r *Record) { r.Measurements = nil }, "measurements"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fixtureRecord()
			tc.break_(r)
			err := r.Validate()
			if err == nil {
				t.Fatal("Validate accepted an incomplete certification record")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestRecordRoundTripsThroughDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ugos-resource-amd64.json")
	want := fixtureRecord()
	if err := want.WriteFile(path); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The file has to be readable as evidence by a person, not only by
	// this package, so it is indented and ends in a newline.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), "}\n") {
		t.Error("the record does not end in a newline")
	}
	if !strings.Contains(string(raw), "\n  \"architecture\"") {
		t.Error("the record is not indented")
	}
	var loose map[string]any
	if err := json.Unmarshal(raw, &loose); err != nil {
		t.Fatalf("the record is not valid JSON: %v", err)
	}

	got, err := ReadRecord(path)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	if got.Architecture != want.Architecture || got.Device.FirmwareVersion != want.Device.FirmwareVersion {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if len(got.Windows["engine_idle"].Samples) != 121 {
		t.Errorf("round trip kept %d raw samples, want 121", len(got.Windows["engine_idle"].Samples))
	}
}

// BuildRecord turns raw samples into the aggregates the thresholds are
// written against. The interesting half is what it refuses: a series
// nobody captured has no p95, and inventing a zero for it would put a
// measurement in an evidence record that nothing measured.
func TestBuildRecordDerivesAggregatesAndRefusesToInventThem(t *testing.T) {
	c := Capture{
		Architecture: "amd64",
		Provider:     "ugos",
		Probe:        Probe{GOARCH: "amd64", Version: "test"},
		Device:       fixtureRecord().Device,
		Release:      fixtureRecord().Release,
		Deployment:   fixtureRecord().Deployment,
		Method:       map[string]float64{"idle_window_seconds": 600},
		Windows:      map[string]Window{"engine_idle": idleSamples(121, 5, 0.002)},
		Series: map[string]Series{
			"api_read_ms":            {1, 2, 3, 4, 5, 6, 7, 8, 9, 40},
			"transfer_mb_per_second": {180, 190, 170},
		},
		Scalars:    map[string]float64{"image_size_bytes": 43008762},
		CapturedAt: time.Now().UTC(),
		Operator:   "an operator",
	}

	r, err := BuildRecord(c)
	if err != nil {
		t.Fatalf("BuildRecord: %v", err)
	}
	// Nearest-rank over ten samples puts p95 at rank ceil(0.95*10) = 10,
	// so it is the slowest one, and it is a latency that happened.
	if got := r.Measurements["api_read_p95_ms"]; got != 40 {
		t.Errorf("api_read_p95_ms = %v, want 40", got)
	}
	if got := r.Measurements["transfer_mb_per_second"]; got != 180 {
		t.Errorf("transfer_mb_per_second = %v, want the median 180", got)
	}
	if got := r.Measurements["idle_rss_bytes"]; got != 90_000_120 {
		t.Errorf("idle_rss_bytes = %v, want the window's peak", got)
	}
	if _, ok := r.Measurements["idle_cpu_percent"]; !ok {
		t.Error("idle_cpu_percent was not derived from the idle window")
	}
	if got := r.Measurements["image_size_bytes"]; got != 43008762 {
		t.Errorf("image_size_bytes = %v, want the scalar as given", got)
	}
	// Nothing captured a config write, so nothing claims one.
	if v, ok := r.Measurements["config_write_p95_ms"]; ok {
		t.Errorf("config_write_p95_ms = %v was invented from an absent series", v)
	}

	t.Run("an empty series is a refusal, not a zero", func(t *testing.T) {
		c := c
		c.Series = map[string]Series{"api_read_ms": {}}
		if _, err := BuildRecord(c); err == nil {
			t.Fatal("BuildRecord accepted a series with no samples in it")
		}
	})

	t.Run("a window that is not an idle window is a refusal", func(t *testing.T) {
		c := c
		w := idleSamples(121, 5, 0.002)
		w.Samples[90] = ProcSample{AtSeconds: 450}
		c.Windows = map[string]Window{"engine_idle": w}
		_, err := BuildRecord(c)
		if err == nil {
			t.Fatal("BuildRecord aggregated a window in which the process vanished")
		}
		if !strings.Contains(err.Error(), string(LivenessNotRunning)) {
			t.Errorf("error %q does not say the process was not running", err)
		}
	})
}

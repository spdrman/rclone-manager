package hwcert

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// RecordSchema identifies the evidence-record format. It is checked rather
// than assumed, so a record from some other tool cannot be verified as if
// it were one of these.
const RecordSchema = "rclone-manager/ugos-resource-certification/1"

// CanonicalBinary is the executable every container in the deployment runs.
// One image, one binary, different arguments; anything else in the data
// path is the second application server EPIC B #81 forbids.
const CanonicalBinary = "/backup-manager-web"

// Probe is the harness binary that produced the record. Its GOARCH is the
// architecture it was COMPILED for, which is why it is evidence: an
// operator can mistype an architecture, and cannot mistype this one.
type Probe struct {
	GOARCH  string `json:"goarch"`
	Version string `json:"version"`
}

// Device is §68's required hardware evidence.
type Device struct {
	Vendor          string `json:"vendor"`
	Model           string `json:"model"`
	OSName          string `json:"os_name"`
	FirmwareVersion string `json:"firmware_version"`
	Kernel          string `json:"kernel"`
	// UnameMachine is what the kernel reports, verbatim: x86_64,
	// aarch64. The third witness to the architecture, and the one
	// neither the operator nor the build supplies.
	UnameMachine string `json:"uname_machine"`
	CPUCores     int    `json:"cpu_cores"`
	MemoryBytes  int64  `json:"memory_bytes"`
}

// Release is what was actually installed, read off the device.
type Release struct {
	Version      string            `json:"version"`
	ImageRef     string            `json:"image_ref"`
	BinarySHA256 map[string]string `json:"binary_sha256"`
	// RegistryDigest is empty while nothing has been pushed. Empty is
	// reported as unrecorded rather than compared, because a comparison
	// against nothing is a check that cannot fail.
	RegistryDigest string `json:"registry_digest,omitempty"`
}

// Container is one running container as the device reported it.
type Container struct {
	Name           string   `json:"name"`
	Image          string   `json:"image"`
	ImageID        string   `json:"image_id"`
	Command        string   `json:"command"`
	PublishedPorts []string `json:"published_ports,omitempty"`
}

// Deployment is the app as installed, which is what the shape checks read.
type Deployment struct {
	Containers []Container `json:"containers"`
}

// Record is one architecture's evidence record. It carries the raw samples
// as well as the aggregates, so a later reader can re-derive every number
// rather than taking the harness's word for it.
type Record struct {
	Schema       string             `json:"schema"`
	Procedure    string             `json:"procedure"`
	Provider     string             `json:"provider"`
	Architecture string             `json:"architecture"`
	Probe        Probe              `json:"probe"`
	Device       Device             `json:"device"`
	Release      Release            `json:"release"`
	Deployment   Deployment         `json:"deployment"`
	Method       map[string]float64 `json:"method"`
	Windows      map[string]Window  `json:"windows,omitempty"`
	Measurements map[string]float64 `json:"measurements"`
	CapturedAt   time.Time          `json:"captured_at"`
	Operator     string             `json:"operator"`
	Notes        string             `json:"notes,omitempty"`
}

// unameArchitectures maps what a kernel calls a machine to what this
// repository calls an architecture. Only the two the release claims are
// here: an unmapped machine is a refusal, not a guess.
var unameArchitectures = map[string]string{
	"x86_64":  "amd64",
	"amd64":   "amd64",
	"aarch64": "arm64",
	"arm64":   "arm64",
}

// ArchOfUname translates a kernel machine name.
func ArchOfUname(machine string) (string, bool) {
	a, ok := unameArchitectures[strings.ToLower(strings.TrimSpace(machine))]
	return a, ok
}

// Validate holds a record to what makes it evidence rather than a note.
//
// The architecture half is the one #89 is strict about. Three independent
// things say what architecture this is, and only one of them is typed in
// by a person, so requiring all three to agree is what makes an amd64 pass
// structurally unable to stand in for an arm64 one.
func (r *Record) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if r.Schema != RecordSchema {
		add("schema is %q, want %q", r.Schema, RecordSchema)
	}
	if r.Architecture == "" {
		add("architecture is empty, so this record certifies nothing")
	}
	if r.Probe.GOARCH == "" {
		add("probe.goarch is empty, so nothing but the operator says which architecture this is")
	} else if r.Architecture != "" && r.Probe.GOARCH != r.Architecture {
		add("probe.goarch is %q and architecture is %q; a probe built for one architecture cannot produce a record for the other", r.Probe.GOARCH, r.Architecture)
	}
	switch machine, ok := ArchOfUname(r.Device.UnameMachine); {
	case r.Device.UnameMachine == "":
		add("device.uname_machine is empty, so the kernel's own answer is missing")
	case !ok:
		add("device.uname_machine is %q, which maps to no architecture this release claims", r.Device.UnameMachine)
	case r.Architecture != "" && machine != r.Architecture:
		add("device.uname_machine is %q (%s) and architecture is %q", r.Device.UnameMachine, machine, r.Architecture)
	}

	if r.Provider == "" {
		add("provider is empty")
	}
	if r.Device.Vendor == "" {
		add("device.vendor is empty")
	}
	if r.Device.Model == "" {
		add("device.model is empty, and a certification record has to name the hardware it certifies")
	}
	if r.Device.FirmwareVersion == "" {
		add("device.firmware_version is empty, and it is one of §68's required evidence fields")
	}
	if r.Release.Version == "" {
		add("release.version is empty, so the record does not say what was measured")
	}
	if r.CapturedAt.IsZero() {
		add("captured_at is unset")
	}
	if r.Operator == "" {
		add("operator is empty, and a hardware run is somebody's run")
	}
	if len(r.Method) == 0 {
		add("method is empty, so nothing says what window these numbers were taken over")
	}
	if len(r.Measurements) == 0 {
		add("measurements is empty, so there is nothing here to certify")
	}

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("evidence record is not usable:\n  %s", strings.Join(problems, "\n  "))
}

// ReadRecord reads and validates an evidence record.
func ReadRecord(path string) (*Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

// WriteFile writes an evidence record for a person to read as well as for
// this package to parse, so it is indented and ends in a newline.
func (r *Record) WriteFile(path string) error {
	if err := r.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// Capture is what the on-device driver hands the harness: raw samples and
// nothing decided. Turning it into a Record is the only place aggregation
// happens, which is why the aggregation is unit-tested and the driver is
// not.
type Capture struct {
	Architecture string             `json:"architecture"`
	Provider     string             `json:"provider"`
	Probe        Probe              `json:"probe"`
	Device       Device             `json:"device"`
	Release      Release            `json:"release"`
	Deployment   Deployment         `json:"deployment"`
	Method       map[string]float64 `json:"method"`
	Windows      map[string]Window  `json:"windows"`
	Series       map[string]Series  `json:"series"`
	Scalars      map[string]float64 `json:"scalars"`
	CapturedAt   time.Time          `json:"captured_at"`
	Operator     string             `json:"operator"`
	Notes        string             `json:"notes,omitempty"`
}

// ReadCapture loads what the on-device driver wrote. It is raw samples
// and identity, never a verdict: nothing in this file decides anything,
// which is why the shell that produces it needs no tests of its own.
func ReadCapture(path string) (*Capture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Capture
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// seriesAggregates maps a captured series to the metric it produces and
// the statistic that produces it. Declared rather than inferred, so a
// series name nobody recognises is an error instead of a measurement that
// silently never appears.
var seriesAggregates = map[string]struct {
	metric     string
	percentile float64
}{
	"api_read_ms":                          {"api_read_p95_ms", 95},
	"config_write_ms":                      {"config_write_p95_ms", 95},
	"api_read_under_transfer_ms":           {"api_read_p95_under_transfer_ms", 95},
	"startup_to_healthy_ms":                {"startup_to_healthy_ms", 50},
	"transfer_mb_per_second":               {"transfer_mb_per_second", 50},
	"raw_copy_mb_per_second":               {"raw_copy_mb_per_second", 50},
	"share_read_mb_per_second_app_running": {"share_read_mb_per_second_app_running", 50},
	"share_read_mb_per_second_app_stopped": {"share_read_mb_per_second_app_stopped", 50},
}

// windowAggregates maps a sampled process window to the metrics it
// produces.
var windowAggregates = map[string]struct {
	rssMetric string
	cpuMetric string
}{
	"engine_idle":     {"idle_rss_bytes", "idle_cpu_percent"},
	"ui_idle":         {"ui_idle_rss_bytes", "ui_idle_cpu_percent"},
	"engine_transfer": {"", "transfer_cpu_percent"},
}

// BuildRecord aggregates a capture into an evidence record.
//
// It refuses rather than fills in. A series with no samples in it has no
// percentile, and a window in which the process vanished or restarted is
// not a measurement of anything; producing a zero for either would put a
// number into an evidence record that nothing measured, which is the one
// way this harness could quietly certify hardware it never saw.
func BuildRecord(c Capture) (*Record, error) {
	measurements := map[string]float64{}

	for name, series := range c.Series {
		agg, ok := seriesAggregates[name]
		if !ok {
			return nil, fmt.Errorf("captured series %q is not one this harness knows how to aggregate; add it to seriesAggregates or fix the name", name)
		}
		v, ok := series.Percentile(agg.percentile)
		if !ok {
			return nil, fmt.Errorf("captured series %q has no samples in it, so %s cannot be derived from it", name, agg.metric)
		}
		measurements[agg.metric] = v
	}

	for name, w := range c.Windows {
		agg, ok := windowAggregates[name]
		if !ok {
			return nil, fmt.Errorf("captured window %q is not one this harness knows how to aggregate; add it to windowAggregates or fix the name", name)
		}
		if liveness, why := w.Validity(); liveness != "" {
			return nil, fmt.Errorf("captured window %q is %s: %s", name, liveness, why)
		}
		if agg.cpuMetric != "" {
			cpu, ok := w.CPUPercent()
			if !ok {
				return nil, fmt.Errorf("captured window %q yields no CPU rate", name)
			}
			measurements[agg.cpuMetric] = cpu
		}
		if agg.rssMetric != "" {
			rss, ok := w.PeakRSSBytes()
			if !ok {
				return nil, fmt.Errorf("captured window %q yields no resident set", name)
			}
			measurements[agg.rssMetric] = float64(rss)
		}
	}

	for name, v := range c.Scalars {
		if _, clash := measurements[name]; clash {
			return nil, fmt.Errorf("scalar %q was also derived from a sampled series or window; one measurement cannot have two sources", name)
		}
		measurements[name] = v
	}

	r := &Record{
		Schema:       RecordSchema,
		Procedure:    ProcedurePath,
		Provider:     c.Provider,
		Architecture: c.Architecture,
		Probe:        c.Probe,
		Device:       c.Device,
		Release:      c.Release,
		Deployment:   c.Deployment,
		Method:       c.Method,
		Windows:      c.Windows,
		Measurements: measurements,
		CapturedAt:   c.CapturedAt,
		Operator:     c.Operator,
		Notes:        c.Notes,
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// ProcedurePath is where the acceptance procedure lives, relative to the
// repository root. Every record names it, so a record can be traced to the
// thresholds it was judged against.
const ProcedurePath = "docs/acceptance/ugos-resource-certification.md"

// ManifestArchitecture is one architecture's entry in the release manifest.
type ManifestArchitecture struct {
	Architecture   string            `json:"architecture"`
	BinarySHA256   map[string]string `json:"binary_sha256"`
	RegistryDigest *string           `json:"registry_digest"`
}

// ReleaseManifest is the part of container/release-manifest.json the shape
// checks read. Read, never written: recording hashes is
// scripts/release/record-release-hashes.sh's job.
type ReleaseManifest struct {
	Version       string                 `json:"version"`
	Architectures []ManifestArchitecture `json:"architectures"`
}

// ReadReleaseManifest loads container/release-manifest.json.
func ReadReleaseManifest(path string) (*ReleaseManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m ReleaseManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(m.Architectures) == 0 {
		return nil, fmt.Errorf("%s: records no architectures, so nothing can be checked against it", path)
	}
	return &m, nil
}

// For returns one architecture's manifest entry.
func (m *ReleaseManifest) For(arch string) (ManifestArchitecture, bool) {
	for _, a := range m.Architectures {
		if a.Architecture == arch {
			return a, true
		}
	}
	return ManifestArchitecture{}, false
}

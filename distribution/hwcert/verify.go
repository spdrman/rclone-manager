package hwcert

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Verdict is one line's outcome. Spelled the way the perf gate spells it,
// so a failure is visible in a column of passes without reading it.
type Verdict string

const (
	Pass Verdict = "pass"
	Fail Verdict = "FAIL"
)

// MetricResult is one metric compared against its threshold, or reported
// missing. Present is separate from Value on purpose: a missing
// measurement must never arrive at the comparator as a zero.
type MetricResult struct {
	Name      string
	Direction Direction
	Present   bool
	Value     float64
	Gated     bool
	Threshold float64
	Verdict   Verdict
	Detail    string
}

// WindowResult is one sampled process window, and whether it carries a
// measurement at all.
type WindowResult struct {
	Name     string
	Liveness Liveness
	Verdict  Verdict
	Detail   string
}

// ShapeResult is one of the claims that carries across hosts.
type ShapeResult struct {
	Check   string
	Verdict Verdict
	Detail  string
}

// Result is one architecture's verdict, with every rule reported whether
// it passed or failed. A report that prints only its failures cannot be
// reviewed: a reader cannot tell a rule that never ran from one that
// passed.
type Result struct {
	Architecture string
	Metrics      []MetricResult
	Windows      []WindowResult
	Shape        []ShapeResult
	Passed       bool
}

// Verify holds one evidence record to one architecture's thresholds.
//
// It returns an error, rather than a failing Result, for everything that
// makes the record unattributable in the first place: a record for another
// architecture, a record whose own witnesses disagree, a record taken over
// a different window. Those are not measurements that missed a threshold,
// they are evidence that is not evidence, and reporting them as a metric
// row would invite somebody to read past them.
func Verify(p *Procedure, rec *Record, arch string, manifest *ReleaseManifest) (*Result, error) {
	if p == nil {
		return nil, errors.New("no acceptance procedure, so there are no thresholds to verify against")
	}
	if manifest == nil {
		return nil, errors.New("no release manifest, so the record's binary cannot be held to the canonical release")
	}
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	if rec.Architecture != arch {
		return nil, fmt.Errorf("this is a %s evidence record and it is being read as %s evidence; an %s pass never stands in for an %s claim",
			rec.Architecture, arch, rec.Architecture, arch)
	}
	claim, ok := p.Architecture(arch)
	if !ok || !claim.Claimed {
		return nil, fmt.Errorf("%s claims no architecture %q, so it declares no thresholds to hold this record to", p.Path, arch)
	}
	if err := sameMethod(p, rec); err != nil {
		return nil, err
	}

	res := &Result{Architecture: arch, Passed: true}
	fail := func() { res.Passed = false }

	for _, m := range p.Metrics {
		mr := MetricResult{Name: m.Name, Direction: m.Direction, Gated: m.Gated()}
		value, present, why := measurementFor(p, rec, m.Name)
		mr.Present, mr.Value = present, value

		switch {
		case !present:
			mr.Verdict, mr.Detail = Fail, why
			fail()
		case !m.Gated():
			mr.Verdict = Pass
			mr.Detail = fmt.Sprintf("%s recorded, gated through %s", formatValue(value, m.Unit), gatedThrough(p, m.Name))
		default:
			threshold := m.Thresholds[arch]
			mr.Threshold = threshold
			over := m.Direction == LowerIsBetter && value > threshold
			under := m.Direction == HigherIsBetter && value < threshold
			if over || under {
				mr.Verdict = Fail
				fail()
			} else {
				mr.Verdict = Pass
			}
			mr.Detail = fmt.Sprintf("%s against %s %s (%s)", formatValue(value, m.Unit), comparator(m.Direction), formatValue(threshold, m.Unit), m.Unit)
		}
		res.Metrics = append(res.Metrics, mr)
	}

	for _, name := range sortedWindowNames(rec.Windows) {
		w := rec.Windows[name]
		liveness, why := w.Validity()
		wr := WindowResult{Name: name, Verdict: Pass}
		if liveness != "" {
			wr.Liveness, wr.Verdict, wr.Detail = liveness, Fail, why
			fail()
		} else {
			cpu, _ := w.CPUPercent()
			wr.Liveness = LivenessSampled
			wr.Detail = fmt.Sprintf("one process throughout %.0fs, %.3f%% of one core", w.Seconds(), cpu)
		}
		res.Windows = append(res.Windows, wr)
	}

	res.Shape = CheckShape(rec, manifest)
	for _, s := range res.Shape {
		if s.Verdict == Fail {
			fail()
		}
	}

	return res, nil
}

// sameMethod refuses a record taken under different sampling parameters
// than the ones the procedure documents. This is the other half of "the
// thresholds were fixed first": a budget that cannot be met over a ten
// minute window can be met over a one minute one, and without this check
// that would be an edit nobody sees.
func sameMethod(p *Procedure, rec *Record) error {
	var problems []string
	for _, name := range sortedFloatKeys(p.Method) {
		want := p.Method[name]
		got, ok := rec.Method[name]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: the record does not say (the procedure documents %g)", name, want))
		case got != want:
			problems = append(problems, fmt.Sprintf("%s: the record says %g, the procedure documents %g", name, got, want))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("the record was not taken under the documented sampling method, so its numbers are not the ones the thresholds were written for:\n  %s",
		strings.Join(problems, "\n  "))
}

// measurementFor resolves one metric's value: recorded directly, or
// derived from two that were. A derived metric names the input it is
// missing rather than reporting a zero ratio, because "0.00 against a 0.70
// floor" reads like a measured collapse and is really an absent number.
func measurementFor(p *Procedure, rec *Record, name string) (float64, bool, string) {
	if d, ok := p.DerivedBy(name); ok {
		num, numOK := rec.Measurements[d.Numerator]
		den, denOK := rec.Measurements[d.Denominator]
		switch {
		case !numOK:
			return 0, false, fmt.Sprintf("not derivable: the record has no %s to divide", d.Numerator)
		case !denOK:
			return 0, false, fmt.Sprintf("not derivable: the record has no %s to divide by", d.Denominator)
		case den == 0:
			return 0, false, fmt.Sprintf("not derivable: %s is zero, so the ratio has no value", d.Denominator)
		}
		return num / den, true, ""
	}
	v, ok := rec.Measurements[name]
	if !ok {
		return 0, false, "no measurement recorded, and an absent measurement is not a pass"
	}
	return v, true, ""
}

// gatedThrough names the derived metric a `recorded` metric feeds, so the
// report never shows a number with nothing holding it to anything.
func gatedThrough(p *Procedure, name string) string {
	var out []string
	for _, d := range p.Derived {
		if d.Numerator == name || d.Denominator == name {
			out = append(out, d.Name)
		}
	}
	if len(out) == 0 {
		return "nothing"
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// CheckShape holds the record to the three claims that transfer across
// hosts, because they are properties of the deployment rather than of the
// hardware: no data-path hop added by the adapter, no sidecar, no second
// application server.
func CheckShape(rec *Record, manifest *ReleaseManifest) []ShapeResult {
	var out []ShapeResult
	add := func(check string, ok bool, detail string) {
		v := Pass
		if !ok {
			v = Fail
		}
		out = append(out, ShapeResult{Check: check, Verdict: v, Detail: detail})
	}

	cs := rec.Deployment.Containers
	add("one container per canonical service", len(cs) == 2,
		fmt.Sprintf("%d container(s): %s. The canonical runtime is two, the engine and the UI host; a third is a sidecar",
			len(cs), strings.Join(containerNames(cs), ", ")))

	images := map[string]bool{}
	for _, c := range cs {
		images[c.Image+" ("+c.ImageID+")"] = true
	}
	add("one image for every container", len(images) == 1,
		fmt.Sprintf("%d distinct image(s): %s. Two commands over one image is the canonical shape; a second image is a second application server or a forked build",
			len(images), strings.Join(sortedSet(images), ", ")))

	var foreign []string
	for _, c := range cs {
		if !strings.HasPrefix(strings.TrimSpace(c.Command), CanonicalBinary) {
			foreign = append(foreign, c.Name+": "+c.Command)
		}
	}
	if len(foreign) == 0 {
		add("every command is the canonical binary", true,
			fmt.Sprintf("all %d container(s) run %s", len(cs), CanonicalBinary))
	} else {
		add("every command is the canonical binary", false,
			fmt.Sprintf("commands not running %s: %s. Anything else in the data path is a second application server",
				CanonicalBinary, strings.Join(foreign, "; ")))
	}

	var publishing []string
	for _, c := range cs {
		if len(c.PublishedPorts) > 0 {
			publishing = append(publishing, fmt.Sprintf("%s %v", c.Name, c.PublishedPorts))
		}
	}
	add("exactly one published port", len(publishing) == 1,
		fmt.Sprintf("%d container(s) publish a port: %s. One LAN-facing listener is the canonical shape; a second one is a data-path hop the adapter added",
			len(publishing), detailOr(strings.Join(publishing, "; "), "none")))

	entry, haveEntry := manifest.For(rec.Architecture)
	switch {
	case !haveEntry:
		add("binary_sha256 matches the canonical release", false,
			fmt.Sprintf("the release manifest records no %s architecture to compare against", rec.Architecture))
	default:
		want := entry.BinarySHA256["backup-manager-web"]
		got := rec.Release.BinarySHA256["backup-manager-web"]
		switch {
		case want == "":
			add("binary_sha256 matches the canonical release", false,
				fmt.Sprintf("the release manifest records no backup-manager-web binary_sha256 for %s", rec.Architecture))
		case got == "":
			add("binary_sha256 matches the canonical release", false,
				"the record carries no backup-manager-web binary_sha256 read off the device")
		default:
			add("binary_sha256 matches the canonical release", want == got,
				fmt.Sprintf("device %s, manifest %s for %s", short(got), short(want), rec.Architecture))
		}
	}

	// A digest nobody has pushed cannot be compared, and reporting that
	// honestly is the difference between an unclaimed gap and a check
	// that cannot fail. #88 owns filling it in.
	switch {
	case !haveEntry || entry.RegistryDigest == nil || *entry.RegistryDigest == "":
		add("registry digest matches the canonical release", true,
			"unrecorded: container/release-manifest.json has no registry_digest for this architecture yet (#88), so nothing was compared")
	case rec.Release.RegistryDigest == "":
		add("registry digest matches the canonical release", false,
			"the manifest records a registry digest and the record read none off the device")
	default:
		add("registry digest matches the canonical release", *entry.RegistryDigest == rec.Release.RegistryDigest,
			fmt.Sprintf("device %s, manifest %s", short(rec.Release.RegistryDigest), short(*entry.RegistryDigest)))
	}

	return out
}

// Report renders the whole verdict, every rule, passing or failing.
func (r *Result) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "architecture: %s\n\nmetrics\n", r.Architecture)
	width := 0
	for _, m := range r.Metrics {
		if len(m.Name) > width {
			width = len(m.Name)
		}
	}
	for _, m := range r.Metrics {
		fmt.Fprintf(&b, "  %-4s %-*s  %s\n", m.Verdict, width, m.Name, m.Detail)
	}
	if len(r.Windows) > 0 {
		b.WriteString("\nsampled windows\n")
		for _, w := range r.Windows {
			fmt.Fprintf(&b, "  %-4s %s (%s): %s\n", w.Verdict, w.Name, w.Liveness, w.Detail)
		}
	}
	b.WriteString("\nshape of the claim\n")
	for _, s := range r.Shape {
		fmt.Fprintf(&b, "  %-4s %s: %s\n", s.Verdict, s.Check, s.Detail)
	}
	verdict := "PASS"
	if !r.Passed {
		verdict = "FAIL"
	}
	fmt.Fprintf(&b, "\n%s: %s\n", verdict, r.Architecture)
	return b.String()
}

// CertStatus is one architecture's certification state.
type CertStatus string

const (
	// Certified has a record and the record passes.
	Certified CertStatus = "certified"
	// Uncertified has no record. §68's rule is that this is an honest
	// state to be in, not a failure: build-supported and uncertified
	// until the acceptance test is completed.
	Uncertified CertStatus = "uncertified"
	// Failed has a record that does not hold up.
	Failed CertStatus = "failed"
)

// ArchStatus is one line of the status output.
type ArchStatus struct {
	Architecture string
	Status       CertStatus
	RecordPath   string
	Reason       string
	Result       *Result
}

// Status answers, per claimed architecture, whether there is real hardware
// evidence behind the claim. An architecture with no record comes back
// uncertified with the reason spelled out, rather than absent, because the
// question #89 asks is exactly the one an empty list would leave open.
func Status(p *Procedure, repoRoot string, manifest *ReleaseManifest) ([]ArchStatus, error) {
	var out []ArchStatus
	for _, arch := range p.ClaimedArchitectures() {
		claim, _ := p.Architecture(arch)
		st := ArchStatus{Architecture: arch, RecordPath: claim.RecordPath}
		path := filepath.Join(repoRoot, claim.RecordPath)

		switch _, err := os.Stat(path); {
		case errors.Is(err, fs.ErrNotExist):
			st.Status = Uncertified
			st.Reason = fmt.Sprintf("no evidence record at %s; %s is build-supported and uncertified until one exists", claim.RecordPath, arch)
			out = append(out, st)
			continue
		case err != nil:
			st.Status = Failed
			st.Reason = err.Error()
			out = append(out, st)
			continue
		}

		rec, err := ReadRecord(path)
		if err != nil {
			st.Status = Failed
			st.Reason = err.Error()
			out = append(out, st)
			continue
		}
		res, err := Verify(p, rec, arch, manifest)
		if err != nil {
			st.Status = Failed
			st.Reason = err.Error()
			out = append(out, st)
			continue
		}
		st.Result = res
		if res.Passed {
			st.Status = Certified
			st.Reason = fmt.Sprintf("%s on %s %s, firmware %s, captured %s",
				rec.Release.Version, rec.Device.Vendor, rec.Device.Model, rec.Device.FirmwareVersion, rec.CapturedAt.Format("2006-01-02"))
		} else {
			st.Status = Failed
			st.Reason = "the evidence record does not meet the acceptance procedure's thresholds"
		}
		out = append(out, st)
	}
	return out, nil
}

// ---------------------------------------------------------------------

func comparator(d Direction) string {
	if d == HigherIsBetter {
		return ">="
	}
	return "<="
}

// formatValue keeps a report readable. A ratio printed to seventeen
// significant figures is a number nobody checks by eye, which defeats the
// point of printing every rule.
func formatValue(v float64, unit string) string {
	switch unit {
	case "bytes":
		return fmt.Sprintf("%.0f", v)
	case "ratio":
		return fmt.Sprintf("%.4f", v)
	}
	return fmt.Sprintf("%g", math.Round(v*1000)/1000)
}

func containerNames(cs []Container) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

func sortedSet(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedFloatKeys(m map[string]float64) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedWindowNames(m map[string]Window) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func detailOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

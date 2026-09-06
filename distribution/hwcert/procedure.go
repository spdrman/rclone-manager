package hwcert

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Direction says how a metric is gated, and it is the procedure's own
// word rather than an inference from the metric's name.
type Direction string

const (
	// LowerIsBetter fails above the architecture's threshold.
	LowerIsBetter Direction = "lower_is_better"
	// HigherIsBetter fails below it.
	HigherIsBetter Direction = "higher_is_better"
	// RecordedOnly is required to be present and is not gated on its
	// own. Every one of these is an input to a derived ratio that IS
	// gated; a metric that were merely recorded and fed nothing would be
	// a number in an evidence record that no criterion reads.
	RecordedOnly Direction = "recorded"
)

// Metric is one row of the procedure's thresholds table.
type Metric struct {
	Name       string
	Direction  Direction
	Unit       string
	Thresholds map[string]float64
	Why        string
}

// Gated reports whether a measurement of this metric can fail on its own.
func (m Metric) Gated() bool {
	return m.Direction == LowerIsBetter || m.Direction == HigherIsBetter
}

// Derived is a ratio the harness computes from two recorded metrics. The
// inputs come from the procedure for the same reason the thresholds do: a
// formula repointed at a friendlier pair of numbers would be as quiet a
// change as a threshold edited to fit a result.
type Derived struct {
	Name        string
	Numerator   string
	Denominator string
}

// ArchitectureClaim is one row of the procedure's architectures table.
type ArchitectureClaim struct {
	Name       string
	Claimed    bool
	RecordPath string
}

// Procedure is docs/acceptance/ugos-resource-certification.md, parsed.
type Procedure struct {
	Path          string
	Method        map[string]float64
	Metrics       []Metric
	Derived       []Derived
	Architectures []ArchitectureClaim
}

// The fenced blocks. An HTML comment rather than a heading, because a
// heading is prose an author can reword and a fence is a marker they
// cannot reword by accident. Nothing is inferred from position: a block
// that is not fenced is not read at all.
var (
	blockRe = func(name string) *regexp.Regexp {
		return regexp.MustCompile(`(?s)<!--\s*hwcert:` + name + `\s*-->(.*?)<!--\s*/hwcert:` + name + `\s*-->`)
	}
	methodBlock       = blockRe("method")
	thresholdsBlock   = blockRe("thresholds")
	derivedBlock      = blockRe("derived")
	architecturesBloc = blockRe("architectures")
)

// ParseProcedure reads and parses an acceptance procedure.
func ParseProcedure(path string) (*Procedure, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseProcedureText(path, string(data))
}

// ParseProcedureText is ParseProcedure over text already in hand.
//
// Every way the document can stop declaring something is an error rather
// than a default. A parser that shrugged at a missing thresholds block
// would leave the harness gating nothing and reporting success, which is
// the fail-open shape scripts/perf/check-baseline.sh already refuses.
func ParseProcedureText(path, text string) (*Procedure, error) {
	p := &Procedure{Path: path, Method: map[string]float64{}}

	archRows, err := rowsIn(path, architecturesBloc, text, "architectures", 3)
	if err != nil {
		return nil, err
	}
	for _, r := range archRows {
		claim := ArchitectureClaim{Name: r[0], Claimed: strings.EqualFold(r[1], "yes"), RecordPath: r[2]}
		if claim.Name == "" {
			return nil, fmt.Errorf("%s: hwcert:architectures has a row with no architecture in it", path)
		}
		if claim.Claimed && claim.RecordPath == "" {
			return nil, fmt.Errorf("%s: hwcert:architectures claims %s but names no evidence record for it", path, claim.Name)
		}
		p.Architectures = append(p.Architectures, claim)
	}

	methodRows, err := rowsIn(path, methodBlock, text, "method", 2)
	if err != nil {
		return nil, err
	}
	for _, r := range methodRows {
		v, err := strconv.ParseFloat(r[1], 64)
		if err != nil {
			return nil, fmt.Errorf("%s: hwcert:method parameter %q has a value that is not a number: %q", path, r[0], r[1])
		}
		if _, dup := p.Method[r[0]]; dup {
			return nil, fmt.Errorf("%s: hwcert:method declares %q twice", path, r[0])
		}
		p.Method[r[0]] = v
	}

	claimed := p.ClaimedArchitectures()
	metricRows, err := rowsIn(path, thresholdsBlock, text, "thresholds", 6)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, r := range metricRows {
		m := Metric{Name: r[0], Direction: Direction(r[1]), Unit: r[4], Why: r[5], Thresholds: map[string]float64{}}
		if m.Name == "" {
			return nil, fmt.Errorf("%s: hwcert:thresholds has a row with no metric name in it", path)
		}
		if seen[m.Name] {
			return nil, fmt.Errorf("%s: hwcert:thresholds declares %q twice, so which threshold applies is undecidable", path, m.Name)
		}
		seen[m.Name] = true

		switch m.Direction {
		case LowerIsBetter, HigherIsBetter, RecordedOnly:
		default:
			return nil, fmt.Errorf("%s: %s declares direction %q, which is none of %q, %q or %q",
				path, m.Name, r[1], LowerIsBetter, HigherIsBetter, RecordedOnly)
		}

		// The two threshold columns are positional and their order is
		// the architectures table's order, so a column cannot silently
		// belong to the wrong architecture.
		cols := r[2:4]
		for i, arch := range []string{"amd64", "arm64"} {
			cell := cols[i]
			if m.Direction == RecordedOnly {
				if !isNotApplicable(cell) {
					return nil, fmt.Errorf("%s: %s is `recorded` but carries a %s threshold (%q); a recorded metric is gated through a derived ratio, not on its own",
						path, m.Name, arch, cell)
				}
				continue
			}
			v, err := strconv.ParseFloat(cell, 64)
			if err != nil {
				return nil, fmt.Errorf("%s: %s declares no usable %s threshold (%q), and a gated metric with no threshold gates nothing",
					path, m.Name, arch, cell)
			}
			m.Thresholds[arch] = v
		}
		p.Metrics = append(p.Metrics, m)
	}

	// A gated metric with no threshold for an architecture the document
	// itself claims would pass that architecture by having nothing to
	// compare against.
	for _, m := range p.Metrics {
		if !m.Gated() {
			continue
		}
		for _, arch := range claimed {
			if _, ok := m.Thresholds[arch]; !ok {
				return nil, fmt.Errorf("%s: %s is gated but declares no threshold for %s, which the same document claims", path, m.Name, arch)
			}
		}
	}

	derivedRows, err := rowsIn(path, derivedBlock, text, "derived", 3)
	if err != nil {
		return nil, err
	}
	for _, r := range derivedRows {
		d := Derived{Name: r[0], Numerator: r[1], Denominator: r[2]}
		if _, ok := p.Metric(d.Name); !ok {
			return nil, fmt.Errorf("%s: hwcert:derived defines %q, which hwcert:thresholds does not declare, so nothing would ever compare it", path, d.Name)
		}
		for _, input := range []string{d.Numerator, d.Denominator} {
			if _, ok := p.Metric(input); !ok {
				return nil, fmt.Errorf("%s: %s is derived from %q, which is not a metric hwcert:thresholds declares", path, d.Name, input)
			}
		}
		p.Derived = append(p.Derived, d)
	}

	return p, nil
}

// Metric returns the declared metric by name.
func (p *Procedure) Metric(name string) (Metric, bool) {
	for _, m := range p.Metrics {
		if m.Name == name {
			return m, true
		}
	}
	return Metric{}, false
}

// Threshold returns a gated metric's threshold for one architecture. A
// `recorded` metric has none, and says so rather than returning a zero.
func (p *Procedure) Threshold(metric, arch string) (float64, bool) {
	m, ok := p.Metric(metric)
	if !ok || !m.Gated() {
		return 0, false
	}
	v, ok := m.Thresholds[arch]
	return v, ok
}

// DerivedBy returns the definition of a derived metric.
func (p *Procedure) DerivedBy(name string) (Derived, bool) {
	for _, d := range p.Derived {
		if d.Name == name {
			return d, true
		}
	}
	return Derived{}, false
}

// ClaimedArchitectures is every architecture the document claims, sorted,
// which is the set an evidence record has to exist for.
func (p *Procedure) ClaimedArchitectures() []string {
	var out []string
	for _, a := range p.Architectures {
		if a.Claimed {
			out = append(out, a.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Architecture returns one row of the architectures table.
func (p *Procedure) Architecture(name string) (ArchitectureClaim, bool) {
	for _, a := range p.Architectures {
		if a.Name == name {
			return a, true
		}
	}
	return ArchitectureClaim{}, false
}

// rowsIn pulls one fenced block's markdown table apart into cells. The
// header row and the alignment row are dropped, backticks are stripped,
// and a row with the wrong number of cells is an error rather than a row
// read with its columns shifted by one.
func rowsIn(path string, re *regexp.Regexp, text, name string, cells int) ([][]string, error) {
	m := re.FindStringSubmatch(text)
	if m == nil {
		return nil, fmt.Errorf("%s: no hwcert:%s block, so nothing declares it and nothing can be checked against it", path, name)
	}

	var out [][]string
	for _, line := range strings.Split(m[1], "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		raw := strings.Split(strings.Trim(line, "|"), "|")
		row := make([]string, 0, len(raw))
		for _, c := range raw {
			row = append(row, strings.Trim(strings.TrimSpace(c), "`"))
		}
		if isAlignmentRow(row) {
			continue
		}
		if len(out) == 0 && len(row) > 0 && isHeaderRow(row) {
			continue
		}
		if len(row) != cells {
			return nil, fmt.Errorf("%s: hwcert:%s has a row with %d cells, want %d: %q", path, name, len(row), cells, line)
		}
		out = append(out, row)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: hwcert:%s has no rows in it, so it declares nothing", path, name)
	}
	return out, nil
}

func isAlignmentRow(row []string) bool {
	for _, c := range row {
		if strings.Trim(c, "-: ") != "" {
			return false
		}
	}
	return true
}

// isHeaderRow recognises the one header these tables use. Matching on
// content rather than on position, because a table whose header went
// missing should lose its header row, not its first metric.
func isHeaderRow(row []string) bool {
	switch strings.ToLower(row[0]) {
	case "metric", "parameter", "architecture":
		return true
	}
	return false
}

func isNotApplicable(cell string) bool {
	switch strings.ToLower(strings.TrimSpace(cell)) {
	case "n/a", "na", "-", "":
		return true
	}
	return false
}

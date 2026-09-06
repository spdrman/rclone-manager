package hwcert

import (
	"strings"
	"testing"
)

// A minimal procedure with the same block structure as the real one. Every
// test that needs a procedure builds one from this rather than reading
// docs/acceptance/ugos-resource-certification.md, so a threshold moving in
// the real document changes what is certified and never changes whether
// the parser works.
const fixtureProcedure = `# fixture

<!-- hwcert:method -->
| parameter | value |
|---|---|
| ` + "`idle_window_seconds`" + ` | 600 |
| ` + "`idle_sample_interval_seconds`" + ` | 5 |
<!-- /hwcert:method -->

<!-- hwcert:thresholds -->
| metric | direction | amd64 | arm64 | unit | why this number |
|---|---|---|---|---|---|
| ` + "`idle_rss_bytes`" + ` | lower_is_better | 134217728 | 100000000 | bytes | the runtime says so |
| ` + "`transfer_mb_per_second`" + ` | recorded | n/a | n/a | MB/s | gated through the ratio |
| ` + "`raw_copy_mb_per_second`" + ` | recorded | n/a | n/a | MB/s | the control |
| ` + "`transfer_throughput_ratio`" + ` | higher_is_better | 0.7 | 0.7 | ratio | no extra hop |
<!-- /hwcert:thresholds -->

<!-- hwcert:derived -->
| metric | numerator | denominator |
|---|---|---|
| ` + "`transfer_throughput_ratio`" + ` | ` + "`transfer_mb_per_second`" + ` | ` + "`raw_copy_mb_per_second`" + ` |
<!-- /hwcert:derived -->

<!-- hwcert:architectures -->
| architecture | claimed | evidence record |
|---|---|---|
| ` + "`amd64`" + ` | yes | ` + "`docs/acceptance/evidence/ugos-resource-amd64.json`" + ` |
| ` + "`arm64`" + ` | yes | ` + "`docs/acceptance/evidence/ugos-resource-arm64.json`" + ` |
<!-- /hwcert:architectures -->
`

func mustParseFixture(t *testing.T) *Procedure {
	t.Helper()
	p, err := ParseProcedureText("fixture.md", fixtureProcedure)
	if err != nil {
		t.Fatalf("ParseProcedureText: %v", err)
	}
	return p
}

func TestParseProcedureReadsEveryBlock(t *testing.T) {
	p := mustParseFixture(t)

	if got, want := p.Method["idle_window_seconds"], 600.0; got != want {
		t.Errorf("method idle_window_seconds = %v, want %v", got, want)
	}
	if len(p.Metrics) != 4 {
		t.Fatalf("parsed %d metrics, want 4", len(p.Metrics))
	}

	m, ok := p.Metric("idle_rss_bytes")
	if !ok {
		t.Fatal("idle_rss_bytes not parsed")
	}
	if m.Direction != LowerIsBetter {
		t.Errorf("idle_rss_bytes direction = %q, want %q", m.Direction, LowerIsBetter)
	}
	if !m.Gated() {
		t.Error("idle_rss_bytes should be gated")
	}
	if got, ok := p.Threshold("idle_rss_bytes", "arm64"); !ok || got != 100000000 {
		t.Errorf("arm64 threshold = %v (ok=%v), want 100000000", got, ok)
	}

	rec, ok := p.Metric("transfer_mb_per_second")
	if !ok {
		t.Fatal("transfer_mb_per_second not parsed")
	}
	if rec.Gated() {
		t.Error("a `recorded` metric must not be gated")
	}
	if _, ok := p.Threshold("transfer_mb_per_second", "amd64"); ok {
		t.Error("a `recorded` metric reported a threshold")
	}

	if len(p.Derived) != 1 || p.Derived[0].Numerator != "transfer_mb_per_second" {
		t.Errorf("derived = %+v, want one row over transfer_mb_per_second", p.Derived)
	}

	if got := p.ClaimedArchitectures(); len(got) != 2 || got[0] != "amd64" || got[1] != "arm64" {
		t.Errorf("claimed architectures = %v, want [amd64 arm64]", got)
	}
	a, ok := p.Architecture("arm64")
	if !ok || a.RecordPath != "docs/acceptance/evidence/ugos-resource-arm64.json" {
		t.Errorf("arm64 claim = %+v (ok=%v)", a, ok)
	}
}

// The thresholds live in the procedure and nowhere else, so every way the
// procedure can stop declaring one has to be a refusal rather than a
// default. A parser that shrugged at a missing block would let the harness
// gate nothing and report success, which is the fail-open shape
// scripts/perf/check-baseline.sh already guards against.
func TestParseProcedureRefusesADocumentThatDeclaresNoThresholds(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{
			name: "no thresholds block at all",
			text: strings.Replace(fixtureProcedure, "<!-- hwcert:thresholds -->", "<!-- hwcert:nothing -->", 1),
			want: "hwcert:thresholds",
		},
		{
			name: "block present but empty",
			text: blankBlock(fixtureProcedure, "thresholds"),
			want: "no rows",
		},
		{
			name: "no architectures block",
			text: strings.Replace(fixtureProcedure, "<!-- hwcert:architectures -->", "<!-- hwcert:none -->", 1),
			want: "hwcert:architectures",
		},
		{
			name: "no method block",
			text: strings.Replace(fixtureProcedure, "<!-- hwcert:method -->", "<!-- hwcert:none -->", 1),
			want: "hwcert:method",
		},
		{
			name: "unknown direction",
			text: strings.Replace(fixtureProcedure, "| lower_is_better | 134217728", "| whatever | 134217728", 1),
			want: "direction",
		},
		{
			name: "gated metric with no threshold for a claimed architecture",
			text: strings.Replace(fixtureProcedure, "| 134217728 | 100000000 |", "| 134217728 | n/a |", 1),
			want: "arm64",
		},
		{
			name: "derived metric whose input is not a declared metric",
			text: strings.Replace(fixtureProcedure, "| `transfer_mb_per_second` | `raw_copy_mb_per_second` |", "| `nowhere_bytes` | `raw_copy_mb_per_second` |", 1),
			want: "nowhere_bytes",
		},
		{
			name: "duplicate metric row",
			text: strings.Replace(fixtureProcedure,
				"| `raw_copy_mb_per_second` | recorded",
				"| `idle_rss_bytes` | recorded", 1),
			want: "twice",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseProcedureText("fixture.md", tc.text)
			if err == nil {
				t.Fatal("parsed a procedure that should have been refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// blankBlock empties a fenced block, keeping the fence. A block that is
// present and says nothing is the interesting case: the fence is there for
// a reader to find, and there is nothing behind it.
func blankBlock(text, name string) string {
	open := "<!-- hwcert:" + name + " -->"
	closed := "<!-- /hwcert:" + name + " -->"
	i := strings.Index(text, open)
	j := strings.Index(text, closed)
	if i < 0 || j < 0 {
		return text
	}
	return text[:i+len(open)] + "\n\n" + text[j:]
}

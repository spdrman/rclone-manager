package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/spdrman/backupd/core/apicontract"
)

// Issue #663's Go half, the CLI side of the four-expert panel's finding: a
// SUCCESSFUL #662 recovery renders as if it were the exact failure it just
// recovered from, because activitySeverityByState keys on the destination
// alone.
//
// completeIngestionInPlace's own re-stamp (quarantineactions.go) is a FAILED
// -> FAILED transition, and its mandatory hold before reinstatement is
// FAILED -> QUARANTINED. Neither is a fresh problem; both are waypoints on
// the way back to a durable state. Keyed on destination alone, the first
// reads as an amber "Attempt failed" and the second as a red "Quarantined
// for review" — an operator running `backupd activity --severity error` sees a
// row that is not an error, from a request that succeeded.
//
// TestActivitySeverityByEdge_TheTwoWaypointsAreNotErrors is the direct
// proof: it drives the real recovery this manager offers (`backupd retry` on a
// FAILED artifact whose durable local copy is byte-identical to the remote
// object) and asserts the whole recovery, not one transition, produces no
// severity-error row. A test pinning only one of the two false rows would
// pass while the other still lied.
func TestActivitySeverityByEdge_TheTwoWaypointsAreNotErrors(t *testing.T) {
	fx := stage662DeadEnd(t, true)
	fx.drive662ToFailed(t)

	if code, _, errOut := runCLI662(t, "retry", "--config", fx.configPath, fx.artifact.String()); code != exitOK {
		t.Fatalf("retry (in-place recovery) = %d, want %d; stderr: %s", code, exitOK, errOut)
	}

	stdout := captureStdout(t, func() {
		run([]string{"activity", "--config", fx.configPath, "--json"})
	})
	var body struct {
		Events []apicontract.ActivityEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("activity --json printed something unparseable (%v):\n%s", err, stdout)
	}

	var sawFailedToFailed, sawFailedToQuarantined bool
	for _, e := range body.Events {
		if e.From == "FAILED" && e.To == "FAILED" {
			sawFailedToFailed = true
		}
		if e.From == "FAILED" && e.To == "QUARANTINED" {
			sawFailedToQuarantined = true
		}
	}
	if !sawFailedToFailed {
		t.Fatalf("the recovery did not record a FAILED -> FAILED re-stamp; this test's fixture drove the wrong path:\n%s", stdout)
	}
	if !sawFailedToQuarantined {
		t.Fatalf("the recovery did not record a FAILED -> QUARANTINED hold; this test's fixture drove the wrong path:\n%s", stdout)
	}

	// The whole recovery, not one transition: a successful in-place
	// recovery must select nothing under --severity error, from either
	// of the two waypoints it wrote.
	errorsOnly := captureStdout(t, func() {
		run([]string{"activity", "--config", fx.configPath, "--severity", "error", "--json"})
	})
	var errBody struct {
		Events []apicontract.ActivityEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(errorsOnly), &errBody); err != nil {
		t.Fatalf("activity --severity error --json printed something unparseable (%v):\n%s", err, errorsOnly)
	}
	for _, e := range errBody.Events {
		if e.From == "FAILED" && (e.To == "FAILED" || e.To == "QUARANTINED") {
			t.Errorf("`backupd activity --severity error` selected %s -> %s, a waypoint of a recovery that succeeded, "+
				"not a fresh error:\n%s", e.From, e.To, errorsOnly)
		}
	}
}

// TestActivitySeverityByEdge_ValidateOriginQuarantineStaysRed is the other
// half of the same fix: COMMITTED -> QUARANTINED and REMOTE_RETAINED ->
// QUARANTINED are the `validate` origin — a true statement about a broken
// record — and must keep the red badge. The state-only table already gives
// them severityErr; this pins that the edge table does not quietly swallow
// them, because a future maintainer "simplifying" activitySeverityByEdge
// into a wider match (e.g. keying on To alone again) would silence exactly
// this without failing a single existing test.
func TestActivitySeverityByEdge_ValidateOriginQuarantineStaysRed(t *testing.T) {
	// stage662DeadEnd's own fixture walks REMOTE_RETAINED -> QUARANTINED
	// to build its starting point, which is a real validate-origin
	// quarantine already sitting in the journal.
	fx := stage662DeadEnd(t, false)

	stdout := captureStdout(t, func() {
		run([]string{"activity", "--config", fx.configPath, "--severity", "error", "--json"})
	})
	var body struct {
		Events []apicontract.ActivityEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("activity --severity error --json printed something unparseable (%v):\n%s", err, stdout)
	}

	var sawIt bool
	for _, e := range body.Events {
		if e.From == "REMOTE_RETAINED" && e.To == "QUARANTINED" {
			sawIt = true
		}
	}
	if !sawIt {
		t.Errorf("`backupd activity --severity error` no longer selects REMOTE_RETAINED -> QUARANTINED, "+
			"the validate origin that is a true statement about a broken record and must stay red:\n%s", stdout)
	}
}

// TestActivitySeverityByEdge_UnitRanks pins the table directly, so a
// regression here fails at the map rather than only through the CLI
// fixtures above.
func TestActivitySeverityByEdge_UnitRanks(t *testing.T) {
	cases := []struct {
		name string
		e    apicontract.ActivityEvent
		want int
	}{
		{"in-place re-stamp is not a fresh failure", apicontract.ActivityEvent{From: "FAILED", To: "FAILED"}, severityInfo},
		{"reinstatement hold is not a fresh quarantine", apicontract.ActivityEvent{From: "FAILED", To: "QUARANTINED"}, severityInfo},
		{"validate origin from COMMITTED stays red", apicontract.ActivityEvent{From: "COMMITTED", To: "QUARANTINED"}, severityErr},
		{"validate origin from REMOTE_RETAINED stays red", apicontract.ActivityEvent{From: "REMOTE_RETAINED", To: "QUARANTINED"}, severityErr},
		{"an ordinary FAILED with no edge entry keeps its state severity", apicontract.ActivityEvent{From: "DISCOVERED", To: "FAILED"}, severityWarn},
		{"an unmapped state falls through to info", apicontract.ActivityEvent{From: "X", To: "NOBODY_HAS_HEARD_OF_THIS"}, severityInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := activitySeverity(tc.e); got != tc.want {
				t.Errorf("activitySeverity({From: %q, To: %q}) = %d, want %d", tc.e.From, tc.e.To, got, tc.want)
			}
		})
	}
}

// --- the #625 drift guard ---

// tsActivityEdgeEntry is one entry this test parsed out of client.ts's
// ACTIVITY_BY_TRANSITION.
type tsActivityEdgeEntry struct {
	from, to, severity string
}

// tsActivityEdgePattern matches ACTIVITY_BY_TRANSITION's quoted "FROM>TO"
// keys and each entry's severity token. ACTIVITY_BY_STATE's keys are bare
// identifiers (DISCOVERED:, not "DISCOVERED":), so requiring both the
// quotes and the literal ">" keeps this pattern off that table entirely —
// it can only ever match the transition-keyed table this fix and its TS
// counterpart agreed on.
var tsActivityEdgePattern = regexp.MustCompile(`"([A-Z_]+)>([A-Z_]+)":\s*\{[^}]*severity:\s*"(\w+)"`)

// tsSeverityToGoRank is the translation this repository's two severity
// vocabularies agree on: ui/shared's Severity type distinguishes "ok" from
// "info" because a client presents them differently, but this package's
// own docblock on activitySeverityByState says the two "share a rank,
// because they are the same thing to somebody filtering: neither is a
// problem." "warn" and "error" translate directly.
var tsSeverityToGoRank = map[string]int{
	"ok":    severityInfo,
	"info":  severityInfo,
	"warn":  severityWarn,
	"error": severityErr,
}

// clientTSPath resolves ui/shared/src/api/client.ts from the repository
// this file itself ships in, taken from the compiler rather than a
// hardcoded absolute path so a checkout anywhere still finds it.
func clientTSPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate this test's own path to derive the repository root")
	}
	// This file lives at core/cmd/backupd/badge_663_test.go; three
	// levels up is the repository root ui/ sits beside core/ in.
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	path := filepath.Join(root, "ui", "shared", "src", "api", "client.ts")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("resolved client.ts at %s but could not stat it (%v); this test's path arithmetic is stale", path, err)
	}
	return path
}

// parseTSActivityEdges reads client.ts and returns every entry
// ACTIVITY_BY_TRANSITION declares, by regex rather than a real TypeScript
// parser: this package has no TS toolchain, and the point of this test is
// only ever "these two literal tables still agree", not "this is valid
// TypeScript" (tsc already owns that).
func parseTSActivityEdges(t *testing.T) []tsActivityEdgeEntry {
	t.Helper()
	src, err := os.ReadFile(clientTSPath(t))
	if err != nil {
		t.Fatalf("reading client.ts: %v", err)
	}
	var out []tsActivityEdgeEntry
	for _, m := range tsActivityEdgePattern.FindAllStringSubmatch(string(src), -1) {
		out = append(out, tsActivityEdgeEntry{from: m[1], to: m[2], severity: m[3]})
	}
	return out
}

// TestActivitySeverityByEdge_AgreesWithClientTS is issue #625's guard for
// this specific table. #625 was filed the last time activitySeverityByState
// and ACTIVITY_BY_STATE drifted apart with nobody noticing until the
// terminal and the page disagreed about which rows an operator gets when
// they ask for errors. This file's own docblock claims
// activitySeverityByEdge is "deliberately the same one
// ui/shared/src/api/client.ts derives" — a claim two hand-maintained
// tables in two languages have already broken once. This test is what
// makes it false to claim and true in fact: every edge either table names,
// the other must name too, at the same translated rank.
func TestActivitySeverityByEdge_AgreesWithClientTS(t *testing.T) {
	tsEdges := parseTSActivityEdges(t)
	if len(tsEdges) == 0 {
		t.Fatal("found no ACTIVITY_BY_TRANSITION entries in client.ts; either the regex is stale or the TS " +
			"table was removed, and this test cannot prove agreement with nothing")
	}

	tsByKey := make(map[[2]string]string, len(tsEdges))
	for _, e := range tsEdges {
		tsByKey[[2]string{e.from, e.to}] = e.severity
	}

	// Every edge Go declares must exist in TS, at the same translated rank.
	for key, goRank := range activitySeverityByEdge {
		tsSeverity, ok := tsByKey[key]
		if !ok {
			t.Errorf("activitySeverityByEdge has %s>%s but client.ts's ACTIVITY_BY_TRANSITION does not "+
				"(issue #625: the two tables have drifted)", key[0], key[1])
			continue
		}
		wantRank, known := tsSeverityToGoRank[tsSeverity]
		if !known {
			t.Errorf("client.ts's %s>%s carries severity %q, which this test does not know how to translate "+
				"to a Go rank", key[0], key[1], tsSeverity)
			continue
		}
		if goRank != wantRank {
			t.Errorf("activitySeverityByEdge[%s>%s] = %d but client.ts's severity %q translates to %d "+
				"(issue #625: the two tables have drifted)", key[0], key[1], goRank, tsSeverity, wantRank)
		}
	}

	// And every edge TS declares must exist in Go, so neither side can
	// silently grow ahead of the other.
	for key, tsSeverity := range tsByKey {
		if _, ok := activitySeverityByEdge[key]; !ok {
			t.Errorf("client.ts's ACTIVITY_BY_TRANSITION has %s>%s (severity %q) but "+
				"activitySeverityByEdge does not (issue #625: the two tables have drifted)", key[0], key[1], tsSeverity)
		}
	}
}

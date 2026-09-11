package apiclient

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/spdrman/backupd/core/apicontract"
)

// The three read operations issue #544 routes, and the one piece of
// transport they need that nothing else on this API does.
//
// listArtifacts takes its filter in the QUERY, and until #544 this package
// had no query support at all: every path came out of fillPath and nothing
// ever appended a `?`. A client that dropped the filter would not fail, it
// would answer `artifacts --backup-set production/postgres` with every
// artifact in the deployment, which is a wrong answer that looks like a
// right one.
//
// getArtifact and previewRetention are here for the other half of PR
// #546's first finding: the contract used to spell them with a single
// composite `{id}`, fillPath percent-escapes each parameter as it must,
// and "production/postgres" went out as "production%2Fpostgres" for a
// 404 on every backup set that exists. The paths are segment-wise now,
// and these assert that this client fills them segment-wise too rather
// than pasting an id into one parameter.

// queryOf reads the query string off a recorded request.
func queryOf(t *testing.T, raw string) url.Values {
	t.Helper()
	values, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("the client sent %q, which is not a query string: %v", raw, err)
	}
	return values
}

func TestListArtifacts_CarriesTheBackupSetFilterInTheQuery(t *testing.T) {
	engine := newFakeEngine(t)
	client := engine.client(t, engine.start())

	if _, err := client.ListArtifacts(context.Background(), "production/postgres primary"); err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}

	var seen *seenRequest
	for _, r := range engine.requests() {
		if r.Operation == "listArtifacts" {
			seen = &r
		}
	}
	if seen == nil {
		t.Fatalf("no listArtifacts request reached the engine; it saw %v", engine.operations())
	}
	if got := queryOf(t, seen.Query).Get("setId"); got != "production/postgres primary" {
		t.Errorf("listArtifacts carried setId=%q, want %q; a filter the engine never sees answers with every artifact in the deployment, which is a wrong answer that looks like a right one", got, "production/postgres primary")
	}
	// Escaped on the wire, not pasted in raw: a space or an ampersand in a
	// backup set's name would otherwise cut the query in half.
	if strings.Contains(seen.Query, " ") {
		t.Errorf("listArtifacts sent the raw query %q, with the filter unescaped", seen.Query)
	}
}

func TestListArtifacts_UnfilteredSendsNoQueryAtAll(t *testing.T) {
	engine := newFakeEngine(t)
	client := engine.client(t, engine.start())

	if _, err := client.ListArtifacts(context.Background(), ""); err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}

	for _, r := range engine.requests() {
		if r.Operation != "listArtifacts" {
			continue
		}
		// Not `?setId=`, which is a different question: the contract says
		// an id naming no configured backup set is REFUSED rather than
		// answered with an empty list, so an empty filter sent as a filter
		// would turn "show me everything" into a 404.
		if r.Query != "" {
			t.Errorf("an unfiltered artifacts listing sent the query %q; the contract refuses an id that names no configured backup set, so an empty filter sent as a filter is a 404 rather than the whole list", r.Query)
		}
	}
}

func TestGetArtifact_FillsThreeSegmentsRatherThanOneCompositeID(t *testing.T) {
	engine := newFakeEngine(t)
	client := engine.client(t, engine.start())

	if _, err := client.GetArtifact(context.Background(), "production", "postgres", "daily.dump"); err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}

	want := apicontract.BasePath + "/backups/production/postgres/daily.dump"
	if !requestedPath(engine, "getArtifact", want) {
		t.Errorf("getArtifact requested %v, want %s; a composite id pasted into one parameter comes out percent-escaped and 404s for every artifact that exists (PR #546 review)", pathsFor(engine, "getArtifact"), want)
	}
}

func TestPreviewRetention_FillsTheBackupSetSegmentWise(t *testing.T) {
	engine := newFakeEngine(t)
	client := engine.client(t, engine.start())

	if _, err := client.PreviewRetention(context.Background(), "production", "postgres"); err != nil {
		t.Fatalf("PreviewRetention: %v", err)
	}

	want := apicontract.BasePath + "/backup-sets/production/postgres/retention/preview"
	if !requestedPath(engine, "previewRetention", want) {
		t.Errorf("previewRetention requested %v, want %s", pathsFor(engine, "previewRetention"), want)
	}
}

func requestedPath(engine *fakeEngine, operation, want string) bool {
	for _, p := range pathsFor(engine, operation) {
		if p == want {
			return true
		}
	}
	return false
}

func pathsFor(engine *fakeEngine, operation string) []string {
	var out []string
	for _, r := range engine.requests() {
		if r.Operation == operation {
			out = append(out, r.Path)
		}
	}
	return out
}

package webhost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/service"
)

// EPIC E, FR-34 and FR-27 at the contract (#240).
//
// contract_test.go beside this file proves the handlers still match the
// contract. What it cannot say is whether the contract itself is honest,
// and honesty is the whole subject of this issue: a schema is free to
// declare a restore ETA or a retrieval cost, and the drift gate would
// cheerfully keep the bindings in step with the lie. So these read the
// contract document as data and hold it to three rules it is not otherwise
// held to.
//
// Run with -count=1 after editing api/v1/openapi.json: a cached PASS here
// is a statement about the tree as it was.

// contractDocument is api/v1/openapi.json, parsed.
func contractDocument(t *testing.T) map[string]any {
	t.Helper()
	path := filepath.Join("..", "..", "..", "api", "v1", "openapi.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the contract at %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return doc
}

func contractSchemas(t *testing.T) map[string]any {
	t.Helper()
	components, _ := contractDocument(t)["components"].(map[string]any)
	schemas, ok := components["schemas"].(map[string]any)
	if !ok || len(schemas) == 0 {
		t.Fatal("the contract declares no schemas at all, so every check in this file would pass vacuously")
	}
	return schemas
}

// property is one property somewhere in the schema graph, with the path it
// was reached by, so a failure names the field rather than the rule.
type property struct {
	Path string
	Name string
	Type string
}

// reachableProperties walks the schemas reachable from roots, following
// $ref and array items, and returns every property it finds along with the
// set of schema names it visited.
//
// Reachability, rather than the whole document, is what makes the
// credential rule below sayable at all: RotatePasswordRequest legitimately
// carries a password, and a scan that could not tell "reachable from the
// artifact and settings read surfaces" from "declared anywhere" would
// either fire on that or be watered down until it fired on nothing, which
// is how the ADMIN_PASSWORD scanner in this repository failed once
// already.
func reachableProperties(t *testing.T, roots ...string) ([]property, map[string]bool) {
	t.Helper()
	schemas := contractSchemas(t)
	visited := map[string]bool{}
	var props []property

	var walkSchema func(name string, node map[string]any, path string)

	resolve := func(ref string) (string, map[string]any, bool) {
		const prefix = "#/components/schemas/"
		if !strings.HasPrefix(ref, prefix) {
			return "", nil, false
		}
		name := strings.TrimPrefix(ref, prefix)
		s, ok := schemas[name].(map[string]any)
		return name, s, ok
	}

	var walkNode func(node map[string]any, path string)
	walkNode = func(node map[string]any, path string) {
		if ref, ok := node["$ref"].(string); ok {
			if name, s, ok := resolve(ref); ok {
				walkSchema(name, s, path)
			}
			return
		}
		if items, ok := node["items"].(map[string]any); ok {
			walkNode(items, path+"[]")
			return
		}
		if _, ok := node["properties"]; ok {
			walkSchema("", node, path)
		}
	}

	walkSchema = func(name string, node map[string]any, path string) {
		if name != "" {
			if visited[name] {
				return
			}
			visited[name] = true
		}
		properties, _ := node["properties"].(map[string]any)
		names := make([]string, 0, len(properties))
		for k := range properties {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			child, _ := properties[k].(map[string]any)
			kind, _ := child["type"].(string)
			childPath := path + "." + k
			props = append(props, property{Path: childPath, Name: k, Type: kind})
			walkNode(child, childPath)
		}
	}

	for _, root := range roots {
		s, ok := schemas[root].(map[string]any)
		if !ok {
			t.Fatalf("the contract declares no schema %q, so this scan started from nothing", root)
		}
		walkSchema(root, s, root)
	}
	if len(props) == 0 {
		t.Fatal("the scan found no properties at all, so it verified nothing")
	}
	return props, visited
}

// theReadSurfaces are the schemas this issue's rules apply to: everything
// a client can reach from one backup and from the settings document.
var theReadSurfaces = []string{"Artifact", "SettingsResponse", "UpdateSettingsRequest", "Operation"}

// TestContract_NoCostFigureReachesTheWire is FR-34's "no cost figures
// anywhere", made structural.
//
// The rule is about FIGURES, and the distinction is the point. This
// product cannot compute egress or restore pricing honestly: it has no
// price list and does not know what an operator negotiated. So a NUMBER
// about money or bandwidth is refused, while the words the engine uses to
// describe what a verification class costs are not, because a sentence
// saying "a full download, so time plus egress" is a true statement about
// mechanism rather than an invented amount.
func TestContract_NoCostFigureReachesTheWire(t *testing.T) {
	props, visited := reachableProperties(t, theReadSurfaces...)

	// The scan actually reached what this issue added. Without this, a
	// walker that silently stopped at the first $ref would pass every
	// assertion below by looking at nothing.
	for _, want := range []string{"Placement", "StorageMediumSummary", "StorageSchema", "VerificationClassInfo", "CycleOutcome"} {
		if !visited[want] {
			t.Fatalf("the scan never reached schema %q, so it proved nothing about it", want)
		}
	}

	money := regexp.MustCompile(`(?i)cost|price|pricing|bill|charge|usd|cents|egress|retrieval|transfer_bytes`)
	for _, p := range props {
		if !money.MatchString(p.Name) {
			continue
		}
		if p.Type == "integer" || p.Type == "number" {
			t.Errorf("%s is a %s named %q. This product holds no price list and no negotiated rates, so a number about money or metered bandwidth on this boundary is one it made up (the #211 rule).", p.Path, p.Type, p.Name)
		}
	}
}

// TestContract_NoInventedEstimateReachesTheWire is the other half of
// FR-34: "no invented ETAs".
//
// S3 reports no percentage for a restore in progress and no completion
// time until it has one, so anything shaped like a prediction on these
// schemas is this product guessing. A date the provider actually reported
// is a different thing and is deliberately not caught here: it would be
// spelled as an expiry, which is a fact, not an estimate.
func TestContract_NoInventedEstimateReachesTheWire(t *testing.T) {
	props, _ := reachableProperties(t, theReadSurfaces...)

	// "eta" is anchored to a whole word: without that it matches inside
	// "detail", and a rule that fires on validation_detail is a rule
	// somebody deletes rather than fixes.
	guesses := regexp.MustCompile(`(?i)(^|_)(eta|pct|percent|remaining)(_|$)|estimat|predict|percentage|expected_`)
	found := false
	for _, p := range props {
		if guesses.MatchString(p.Name) {
			t.Errorf("%s is named %q, which reads as a prediction. The provider reports no progress and no completion time for a restore, so a field shaped like one can only be invented.", p.Path, p.Name)
			found = true
		}
	}
	if found {
		t.Log("if the provider genuinely reports the value, name it for the fact (an expiry, a reported-at) rather than for the guess")
	}
}

// TestContract_NoCredentialHasASpellingOnTheReadSurfaces is FR-33 at the
// schema level.
//
// The strongest form of "no credential is ever served" is that there is no
// field it could be served IN. This walks everything reachable from the
// artifact and settings surfaces and refuses any property whose name is
// one a secret would travel under, which is a stronger statement than a
// redaction test can make: a filter has to keep working, and an absent
// field cannot start leaking.
func TestContract_NoCredentialHasASpellingOnTheReadSurfaces(t *testing.T) {
	props, visited := reachableProperties(t, theReadSurfaces...)
	if !visited["StorageMediumSummary"] {
		t.Fatal("the scan never reached StorageMediumSummary, which is the schema this rule exists for")
	}

	secrets := regexp.MustCompile(`(?i)secret|credential|access_key|private_key|passphrase|password|token|session_key`)
	for _, p := range props {
		if secrets.MatchString(p.Name) {
			t.Errorf("%s is named %q. A storage medium's credentials reach this product as a reference to a file, an environment variable or a command, and none of those three may have a spelling on this boundary at all (FR-33).", p.Path, p.Name)
		}
	}
}

// TestContract_TheAccessAndVerificationVocabulariesAreTheEnginesOwn is the
// REFACTOR requirement held as a check: the closed sets a surface narrows
// against come from ONE source, so a UI string cannot drift from core.
//
// The contract is the generated source the TypeScript client reads, and
// core/internal/placement and core/internal/state are what the engine
// actually writes and compares. Pinning the two here is what makes the
// generated union types honest rather than decorative.
func TestContract_TheAccessAndVerificationVocabulariesAreTheEnginesOwn(t *testing.T) {
	schemas := contractSchemas(t)
	p, ok := schemas["Placement"].(map[string]any)
	if !ok {
		t.Fatal("the contract declares no Placement schema")
	}
	properties, _ := p["properties"].(map[string]any)

	enumOf := func(field string) []string {
		t.Helper()
		prop, ok := properties[field].(map[string]any)
		if !ok {
			t.Fatalf("Placement declares no %q", field)
		}
		raw, ok := prop["enum"].([]any)
		if !ok || len(raw) == 0 {
			t.Fatalf("Placement.%s is not a closed set, so nothing narrows against it", field)
		}
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			out = append(out, fmt.Sprint(v))
		}
		return out
	}

	wantAccess := service.AccessStates()
	if got := enumOf("access"); !equalStrings(got, wantAccess) {
		t.Errorf("Placement.access = %v, the engine's vocabulary is %v. One of the two moved without the other, and the UI narrows against the first.", got, wantAccess)
	}

	wantClasses := service.VerificationClasses()
	if got := enumOf("verification_class"); !equalStrings(got, wantClasses) {
		t.Errorf("Placement.verification_class = %v, the ladder is %v", got, wantClasses)
	}
	// And the empty class is NOT one of them. "Nothing has verified this"
	// is expressed by the field being absent, and a schema that admitted
	// "" as a value would let a client render it as a rung.
	for _, v := range enumOf("verification_class") {
		if v == "" {
			t.Error("Placement.verification_class admits the empty string as a value; unverified is spelled by the field being absent, not by an empty rung")
		}
	}

	// Status carries the two a copy can be in and NOT the third. A GONE
	// row is not served at all, so admitting the value would invite a
	// client to render a copy that is not there.
	wantStatus := service.PlacementStatuses()
	if got := enumOf("status"); !equalStrings(got, wantStatus) {
		t.Errorf("Placement.status = %v, want %v: a copy the journal knows is GONE never reaches the wire", got, wantStatus)
	}
	for _, v := range enumOf("status") {
		if v == "GONE" {
			t.Error("Placement.status admits GONE; a row for a copy that is not there reads as a copy")
		}
	}
}

// TestContract_ThePlacementsArrayIsAlwaysServed is the "absence is not
// presence" rule at the schema level. placements has to be REQUIRED, so
// "this backup has no copy" is an empty array a client always receives
// rather than a missing key it has to interpret.
func TestContract_ThePlacementsArrayIsAlwaysServed(t *testing.T) {
	schemas := contractSchemas(t)
	artifact, _ := schemas["Artifact"].(map[string]any)
	required, _ := artifact["required"].([]any)
	for _, r := range required {
		if fmt.Sprint(r) == "placements" {
			return
		}
	}
	t.Errorf("Artifact does not require placements. An optional array has three readings (a copy exists, no copy exists, the server did not say), and only two of them are true.")
}

// ------------------------------------------------------- the consent gate --

// TestUpdateSettings_TheDisclosureRefusalIsItsOwnTypedCode is FR-27's gate
// at the HTTP boundary.
//
// The code has to differ from INVALID_REQUEST, and the reason is what a
// client does next: a malformed body is a field to fix, and this is a
// paragraph to put in front of a human. Same status, different code, which
// is exactly the distinction TestContract_TypedRefusalsAreDistinguishable
// draws for submitOperation's three 409s.
func TestUpdateSettings_TheDisclosureRefusalIsItsOwnTypedCode(t *testing.T) {
	backend := newSettingsFakeBackend()
	backend.errOnUpdate = fmt.Errorf("%w. This write sends monthly -> offsite_s3. After a backup uploads and I verify it, I delete the copy on this machine.",
		service.ErrMediumDisclosureRequired)

	router := NewRouter(RouterConfig{
		Platform: allowingPlatform("alice"), Backend: backend, Gate: alwaysPassGate{},
		BinaryVersion: "test", Commit: "test",
	})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/settings",
		strings.NewReader(`{"retention":{"tiers":[{"name":"monthly","granularity":"month","keep":12,"medium":"offsite_s3"}]}}`))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}
	code := errorCodeOf(t, rec)
	if code != "MEDIUM_DISCLOSURE_REQUIRED" {
		t.Fatalf("code = %q, want MEDIUM_DISCLOSURE_REQUIRED. INVALID_REQUEST would tell a client to highlight a field, when what it has to do is show a paragraph.", code)
	}

	// The message IS the disclosure, so a client that renders it has
	// shown the operator the product's own words rather than its own
	// summary of them.
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshalling the error body: %v", err)
	}
	if !strings.Contains(body.Error.Message, "delete the copy on this machine") {
		t.Errorf("the refusal does not carry the disclosure:\n%s", body.Error.Message)
	}

	// And the contract declares it for THIS operation at THIS status.
	// Per-operation is the granularity a client consumes; a code
	// registered somewhere in the document is a code it was never told to
	// expect here.
	declared := contractEndpoints()["updateSettings"].ErrorCodes[http.StatusBadRequest]
	for _, c := range declared {
		if string(c) == "MEDIUM_DISCLOSURE_REQUIRED" {
			return
		}
	}
	t.Errorf("api/v1/openapi.json does not declare MEDIUM_DISCLOSURE_REQUIRED for updateSettings at 400 (it declares %v)", declared)
}

// TestUpdateSettings_TheAcknowledgmentReachesTheService is the other half:
// the tick a form sends has to actually cross the seam, or the gate is
// unopenable and the feature is unusable.
func TestUpdateSettings_TheAcknowledgmentReachesTheService(t *testing.T) {
	backend := newSettingsFakeBackend()
	router := NewRouter(RouterConfig{
		Platform: allowingPlatform("alice"), Backend: backend, Gate: alwaysPassGate{},
		BinaryVersion: "test", Commit: "test",
	})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/settings",
		strings.NewReader(`{"retention":{"tiers":[{"name":"monthly","granularity":"month","keep":12,"medium":"offsite_s3"}]},"acknowledge_medium_disclosure":true}`))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if !backend.lastUpdate.AcknowledgeMediumDisclosure {
		t.Error("the acknowledgment did not reach core/service, so the gate can never be opened")
	}
	if len(backend.lastUpdate.Retention.Tiers) != 1 || backend.lastUpdate.Retention.Tiers[0].Medium != "offsite_s3" {
		t.Errorf("the tier's medium did not cross the seam: %+v", backend.lastUpdate.Retention.Tiers)
	}
}

// TestUpdateSettings_AnAcknowledgmentAloneNamesNoSetting keeps the new
// field from becoming a way past the "a write must ask for something"
// rule. Honouring it would rewrite the operator's file and move
// ConfigRevision, invalidating every outstanding retention preview, for a
// request with no content in it.
func TestUpdateSettings_AnAcknowledgmentAloneNamesNoSetting(t *testing.T) {
	backend := newSettingsFakeBackend()
	router := NewRouter(RouterConfig{
		Platform: allowingPlatform("alice"), Backend: backend, Gate: alwaysPassGate{},
		BinaryVersion: "test", Commit: "test",
	})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/settings",
		strings.NewReader(`{"acknowledge_medium_disclosure":true}`))
	req.Header.Set("Content-Type", "application/json")
	attachValidCSRF(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a body that asks for nothing, body: %s", rec.Code, rec.Body.String())
	}
	if backend.updateCalls != 0 {
		t.Errorf("a request naming no setting still reached the backend %d time(s)", backend.updateCalls)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

package webhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
	"github.com/spdrman/rclone-manager/apps/common/webhost/apicontract"
	"github.com/spdrman/rclone-manager/core/service"
)

// This file is the Go half of issue #166's drift gate: it holds the
// handlers in this package to api/v1/openapi.json, which is the
// authoritative definition of /api/v1.
//
// The other half is scripts/api/check-contract-drift.sh, which
// regenerates the bindings and compares them byte for byte, catching a
// hand edit to generated output. Neither check subsumes the other: the
// drift script proves the bindings still match the contract, and this file
// proves the HANDLERS still match the bindings. A handler that grows a
// field nobody wrote into the contract passes the drift script and fails
// here.
//
// Several tests here read repository files (the contract itself, and the
// package sources the error-code registry is checked against), so run them
// with -count=1 when you have just edited one; a cached PASS is a
// statement about the tree as it was.

// contractBinding ties one contract operation to the concrete handler
// types and router pattern that implement it in this package.
//
// It is hand-written, and that is a hole a stale entry could hide in, so
// three tests below close it from three directions: every operation the
// contract declares for this router has an entry, every route chi actually
// registers has an entry, and every entry names an operation the contract
// declares. A binding cannot be forgotten, and one cannot be left behind.
type contractBinding struct {
	// routerPattern is chi's own registered pattern, which is not always
	// the contract's path: getBackupSet is registered as a catch-all
	// ("/backup-sets/*") because a backup set's id is a two-part
	// source/name identity that spans a path segment, while the contract
	// spells the same thing as "{id}". That difference is a Go routing
	// detail, so it is recorded here rather than pushed into the contract.
	routerPattern string
	request       any
	response      any
	// url is a concrete path this operation can be driven at, with every
	// parameter filled in.
	url string
}

var contractBindings = map[string]contractBinding{
	"getSystemVersion":        {"/api/v1/system/version", nil, versionResponse{}, "/api/v1/system/version"},
	"getSystemCapabilities":   {"/api/v1/system/capabilities", nil, capabilitiesResponse{}, "/api/v1/system/capabilities"},
	"getFirstRunStatus":       {"/api/v1/system/first-run", nil, firstRunStatusResponse{}, "/api/v1/system/first-run"},
	"completeFirstRun":        {"/api/v1/system/first-run", backupSetSpec{}, completeFirstRunResponse{}, "/api/v1/system/first-run"},
	"listStorageStatus":       {"/api/v1/system/storage", nil, listStorageStatusResponse{}, "/api/v1/system/storage"},
	"submitOperation":         {"/api/v1/operations", submitOperationRequest{}, operationResponse{}, "/api/v1/operations"},
	"getOperation":            {"/api/v1/operations/{id}", nil, operationResponse{}, "/api/v1/operations/op_1"},
	"previewRetention":        {"/api/v1/backup-sets/{source}/{set}/retention/preview", nil, retentionPlanResponse{}, "/api/v1/backup-sets/src/set/retention/preview"},
	"applyRetention":          {"/api/v1/backup-sets/{source}/{set}/retention/apply", applyRetentionRequest{}, retentionPlanResponse{}, "/api/v1/backup-sets/src/set/retention/apply"},
	"listBackupSets":          {"/api/v1/backup-sets", nil, listBackupSetsResponse{}, "/api/v1/backup-sets"},
	"createBackupSet":         {"/api/v1/backup-sets", backupSetRequest{}, createBackupSetResponse{}, "/api/v1/backup-sets"},
	"testCandidateConnection": {"/api/v1/backup-sets/test-connection", testConnectionRequest{}, testConnectionResponse{}, "/api/v1/backup-sets/test-connection"},
	"getBackupSet":            {"/api/v1/backup-sets/*", nil, backupSetResponse{}, "/api/v1/backup-sets/src/set"},
	"listValidators":          {"/api/v1/validators", nil, listValidatorsResponse{}, "/api/v1/validators"},
	"importSSHKey":            {"/api/v1/ssh-keys", importSSHKeyRequest{}, importSSHKeyResponse{}, "/api/v1/ssh-keys"},
	"probeHostKey":            {"/api/v1/ssh/host-key-probe", hostKeyProbeRequest{}, hostKeyProbeResponse{}, "/api/v1/ssh/host-key-probe"},
	"getSettings":             {"/api/v1/settings", nil, settingsResponse{}, "/api/v1/settings"},
	"updateSettings":          {"/api/v1/settings", settingsRequest{}, settingsResponse{}, "/api/v1/settings"},

	// Issue #211. Four of these spell their path parameter as one {id}
	// that spans segments, the way getBackupSet already does, because
	// that is what the identity IS: a backup set is "source/name" and an
	// artifact is "source/set/name". chi matches a parameter per path
	// segment, so the router registers two or three named segments
	// instead, and routerPattern is where the two spellings meet. Named
	// segments rather than getBackupSet's catch-all, because each of
	// these has a FIXED arity and three of them need a literal tail
	// ("/enabled", "/revalidate", "/retry") that a catch-all would
	// swallow.
	"getSystemHealth":        {"/api/v1/system/health", nil, healthResponse{}, "/api/v1/system/health"},
	"listOperations":         {"/api/v1/operations", nil, listOperationsResponse{}, "/api/v1/operations"},
	"listArtifacts":          {"/api/v1/backups", nil, listArtifactsResponse{}, "/api/v1/backups"},
	"getArtifact":            {"/api/v1/backups/{source}/{set}/{name}", nil, artifactResponse{}, "/api/v1/backups/src/set-1/backup.dump"},
	"listActivity":           {"/api/v1/activity", nil, listActivityResponse{}, "/api/v1/activity"},
	"listQuarantine":         {"/api/v1/quarantine", nil, listArtifactsResponse{}, "/api/v1/quarantine"},
	"revalidateArtifact":     {"/api/v1/quarantine/{source}/{set}/{name}/revalidate", nil, artifactCheckResponse{}, "/api/v1/quarantine/src/set-1/backup.dump/revalidate"},
	"retryArtifactIngestion": {"/api/v1/quarantine/{source}/{set}/{name}/retry", nil, nil, "/api/v1/quarantine/src/set-1/backup.dump/retry"},
	"reinstateArtifact":      {"/api/v1/quarantine/{source}/{set}/{name}/reinstate", nil, artifactReinstateResponse{}, "/api/v1/quarantine/src/set-1/backup.dump/reinstate"},
	"setBackupSetEnabled":    {"/api/v1/backup-sets/{source}/{set}/enabled", setEnabledRequest{}, backupSetResponse{}, "/api/v1/backup-sets/src/set-1/enabled"},
	"setBackupSetReadOnly":   {"/api/v1/backup-sets/{source}/{set}/read-only", setReadOnlyRequest{}, backupSetResponse{}, "/api/v1/backup-sets/src/set-1/read-only"},
	// Issue #350's edit route. It shares its path template with
	// getBackupSet's "/api/v1/backup-sets/*" catch-all and does not
	// collide with it, because chi routes on the method too.
	"updateBackupSet": {"/api/v1/backup-sets/{source}/{set}", updateBackupSetRequest{}, backupSetResponse{}, "/api/v1/backup-sets/src/set-1"},
	// Issue #350's edit hold. The release has no response body at all
	// (204), so it binds no response type; the contract declares no
	// response schema for it either, which is what keeps the two in step.
	"getBackupSetEditHold":     {"/api/v1/backup-sets/{source}/{set}/edit-hold", nil, editHoldStateResponse{}, "/api/v1/backup-sets/src/set-1/edit-hold"},
	"takeBackupSetEditHold":    {"/api/v1/backup-sets/{source}/{set}/edit-hold", nil, editHoldResponse{}, "/api/v1/backup-sets/src/set-1/edit-hold"},
	"releaseBackupSetEditHold": {"/api/v1/backup-sets/{source}/{set}/edit-hold/release", nil, nil, "/api/v1/backup-sets/src/set-1/edit-hold/release"},
	// Issue #333's per-set retention. Three operations on two paths, the
	// same POST-and-a-tail shape /enabled and /edit-hold/release already
	// use; see handlers_backupsetretention.go for why not PUT/DELETE.
	"getBackupSetRetention":     {"/api/v1/backup-sets/{source}/{set}/retention", nil, backupSetRetentionResponse{}, "/api/v1/backup-sets/src/set-1/retention"},
	"setBackupSetRetention":     {"/api/v1/backup-sets/{source}/{set}/retention", setBackupSetRetentionRequest{}, backupSetRetentionResponse{}, "/api/v1/backup-sets/src/set-1/retention"},
	"clearBackupSetRetention":   {"/api/v1/backup-sets/{source}/{set}/retention/clear", nil, backupSetRetentionResponse{}, "/api/v1/backup-sets/src/set-1/retention/clear"},
	"scanCatalog":               {"/api/v1/catalog/scan", nil, catalogReportResponse{}, "/api/v1/catalog/scan"},
	"rebuildCatalog":            {"/api/v1/catalog/rebuild", nil, catalogReportResponse{}, "/api/v1/catalog/rebuild"},
	"getRetentionErrorEnvelope": {"", nil, errorResponse{}, ""},
	"getConfigRevisionStale":    {"", nil, configRevisionStaleResponse{}, ""},
}

// nonRoutedBindings are the two entries above that describe a body shape
// rather than an operation: the error envelope and the CONFIG_REVISION_STALE
// body every handler in this package can return. They are checked for shape
// like any other schema, but no route implements them, so the
// route-coverage tests skip them by name rather than by an empty-string
// test that would also swallow a real mistake.
var nonRoutedBindings = map[string]string{
	"getRetentionErrorEnvelope": "ErrorResponse",
	"getConfigRevisionStale":    "ConfigRevisionStaleResponse",
}

// contractEndpoints indexes the generated endpoint table by operation id.
func contractEndpoints() map[string]apicontract.Endpoint {
	out := make(map[string]apicontract.Endpoint, len(apicontract.Endpoints))
	for _, e := range apicontract.Endpoints {
		out[e.ID] = e
	}
	return out
}

// endpointsForThisRouter is every contract operation NewRouter is
// responsible for. The /auth operations are part of the same contract but
// are served by apps/common/auth/local and mounted separately by
// apps/common/webhost/serve, so that package holds them to the contract
// instead (see its own contract_test.go).
func endpointsForThisRouter() []apicontract.Endpoint {
	var out []apicontract.Endpoint
	for _, e := range apicontract.Endpoints {
		if strings.HasPrefix(e.Path, "/auth/") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// TestContract_TheBindingsWereGeneratedFromThisContract catches the
// commonest mistake there is here, editing api/v1/openapi.json and
// forgetting to regenerate, in the ordinary `go test ./...` run rather than
// only in the dedicated CI job.
//
// It is a digest comparison, not a byte-for-byte one: this test cannot
// regenerate without shelling out to the generator, and a test that runs
// `go run` is slow enough that it would end up skipped. The full
// comparison, which is also the only thing that can catch a hand edit to
// the BODY of a generated file, is scripts/api/check-contract-drift.sh.
//
// The relative path is from this package's own directory, which is where
// `go test` runs. A missing file fails loudly rather than skipping: a
// digest check that silently found nothing to hash would be worse than no
// check at all.
func TestContract_TheBindingsWereGeneratedFromThisContract(t *testing.T) {
	path := filepath.Join("..", "..", "..", "api", "v1", "openapi.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the contract at %s: %v", path, err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != apicontract.ContractSHA256 {
		t.Errorf("api/v1/openapi.json hashes to %s, but the generated bindings were made from %s.\nRun scripts/api/generate.sh and commit both generated files.", got, apicontract.ContractSHA256)
	}
}

// ---------------------------------------------------------------- shapes ---

// jsonShape flattens a Go type into the JSON shape it encodes to: a map
// from dotted JSON path to a description of the value at that path.
//
// Comparing flattened paths rather than reflect.Type equality is what lets
// a failure name the FIELD that drifted instead of printing two type
// names, which is what issue #166 asks for ("CI fails and names the
// endpoint and the field that drifted"). Embedded structs are flattened
// exactly the way encoding/json flattens them, so the contract describes
// the wire, not the Go declaration.
func jsonShape(t reflect.Type) map[string]string {
	out := map[string]string{}
	addStruct(out, "", t)
	return out
}

func addStruct(out map[string]string, prefix string, t reflect.Type) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("jsonShape: %s is not a struct", t))
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && tag == "" {
			// An embedded struct with no tag is flattened by
			// encoding/json, so it is flattened here too.
			addStruct(out, prefix, f.Type)
			continue
		}
		if name == "" {
			name = f.Name
		}
		path := prefix + name
		desc := describeType(f.Type)
		if strings.Contains(opts, "omitempty") {
			desc += " omitempty"
		}
		out[path] = desc
		descend(out, path, f.Type)
	}
}

func descend(out map[string]string, path string, t reflect.Type) {
	switch t.Kind() {
	case reflect.Pointer:
		descend(out, path, t.Elem())
	case reflect.Slice:
		if elem := t.Elem(); elem.Kind() == reflect.Struct || (elem.Kind() == reflect.Pointer && elem.Elem().Kind() == reflect.Struct) {
			addStruct(out, path+"[].", elem)
		}
	case reflect.Struct:
		addStruct(out, path+".", t)
	}
}

func describeType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Pointer:
		return "*" + describeType(t.Elem())
	case reflect.Slice:
		return "[]" + describeType(t.Elem())
	case reflect.Struct:
		return "object"
	case reflect.String:
		// A named string type (apicontract.ErrorCode) still encodes as a
		// JSON string, and the handler side spells the same field as a
		// bare string. Comparing the KIND, not the Go type name, is what
		// keeps that from reading as drift.
		return "string"
	default:
		return t.Kind().String()
	}
}

func diffShapes(t *testing.T, label string, want, got map[string]string) {
	t.Helper()
	paths := map[string]bool{}
	for p := range want {
		paths[p] = true
	}
	for p := range got {
		paths[p] = true
	}
	var sorted []string
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, p := range sorted {
		w, inWant := want[p]
		g, inGot := got[p]
		switch {
		case !inGot:
			t.Errorf("%s: field %q is in the contract but not in the handler type. Either add it to the handler or remove it from api/v1/openapi.json.", label, p)
		case !inWant:
			t.Errorf("%s: field %q is in the handler type but not in the contract. Add it to api/v1/openapi.json and re-run scripts/api/generate.sh; a field that reaches the wire without being in the contract is exactly the drift this gate exists to catch.", label, p)
		case w != g:
			t.Errorf("%s: field %q is %q in the contract and %q in the handler type.", label, p, w, g)
		}
	}
}

func TestContract_HandlerShapesMatchTheContract(t *testing.T) {
	endpoints := contractEndpoints()
	checked := 0

	for opID, binding := range contractBindings {
		var requestSchema, responseSchema string
		if schema, ok := nonRoutedBindings[opID]; ok {
			responseSchema = schema
		} else {
			e, ok := endpoints[opID]
			if !ok {
				t.Errorf("contractBindings has an entry for %q, which the contract does not declare. Remove the entry, or add the operation to api/v1/openapi.json.", opID)
				continue
			}
			requestSchema, responseSchema = e.RequestSchema, e.ResponseSchema
		}

		for _, pair := range []struct {
			kind    string
			schema  string
			handler any
		}{
			{"request", requestSchema, binding.request},
			{"response", responseSchema, binding.response},
		} {
			if pair.schema == "" && pair.handler == nil {
				continue
			}
			if pair.schema == "" {
				t.Errorf("%s: the handler declares a %s type (%T) that the contract does not.", opID, pair.kind, pair.handler)
				continue
			}
			if pair.handler == nil {
				t.Errorf("%s: the contract declares a %s schema %q that no handler type is bound to.", opID, pair.kind, pair.schema)
				continue
			}
			generated, ok := apicontract.SchemaTypes[pair.schema]
			if !ok {
				t.Errorf("%s: the contract names schema %q, which the generated bindings do not contain. Re-run scripts/api/generate.sh.", opID, pair.schema)
				continue
			}
			checked++
			diffShapes(t,
				fmt.Sprintf("%s %s (contract schema %s vs handler %T)", opID, pair.kind, pair.schema, pair.handler),
				jsonShape(reflect.TypeOf(generated)),
				jsonShape(reflect.TypeOf(pair.handler)))
		}
	}

	if checked == 0 {
		t.Fatal("no handler type was compared against the contract at all; this test would pass vacuously")
	}
}

// ---------------------------------------------------------------- routes ---

func TestContract_RouterAndContractDeclareTheSameOperations(t *testing.T) {
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       newSyncFakeBackend(),
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})

	// What chi actually serves, as method+pattern.
	registered := map[string]bool{}
	err := chi.Walk(routableFor(t, router), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route == "/health/live" || route == "/health/ready" {
			// Deliberately outside /api/v1 and outside this contract: they
			// are unauthenticated infrastructure probes for an
			// orchestrator, not operations a client of this API calls.
			return nil
		}
		registered[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if len(registered) == 0 {
		t.Fatal("chi.Walk found no /api/v1 routes; this test would pass vacuously")
	}

	// What the contract declares, translated through the bindings.
	declared := map[string]string{}
	for _, e := range endpointsForThisRouter() {
		b, ok := contractBindings[e.ID]
		if !ok {
			t.Errorf("the contract declares operation %q (%s %s) which no entry in contractBindings implements. Bind it, or remove it from api/v1/openapi.json.", e.ID, e.Method, e.Path)
			continue
		}
		declared[e.Method+" "+b.routerPattern] = e.ID
	}

	for key := range registered {
		if _, ok := declared[key]; !ok {
			t.Errorf("the router serves %q, which the contract does not declare. Add the operation to api/v1/openapi.json and re-run scripts/api/generate.sh; an endpoint that exists but is not in the contract is an unversioned, undocumented surface.", key)
		}
	}
	for key, opID := range declared {
		if !registered[key] {
			t.Errorf("the contract declares %q (operation %q), which the router does not serve.", key, opID)
		}
	}
}

// requestFor builds a concrete request for one contract operation, so a
// requirement declared as contract DATA (authentication, CSRF, the
// destructive gate, an idempotency key) can be checked against what the
// route actually does.
func requestFor(t *testing.T, e apicontract.Endpoint, withCSRF, withIdempotencyKey bool) *http.Request {
	t.Helper()
	b := contractBindings[e.ID]
	var body *strings.Reader
	if e.RequestSchema != "" {
		body = strings.NewReader("{}")
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(e.Method, b.url, body)
	req.Header.Set("Content-Type", "application/json")
	if withCSRF {
		attachValidCSRF(req)
	}
	if withIdempotencyKey {
		req.Header.Set("Idempotency-Key", "contract-test-key")
	}
	return req
}

func errorCodeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return ""
	}
	return body.Error.Code
}

func TestContract_DeclaredRequirementsMatchWhatTheRoutesEnforce(t *testing.T) {
	checked := 0
	for _, e := range endpointsForThisRouter() {
		e := e
		t.Run(e.ID, func(t *testing.T) {
			checked++

			// Authentication. The contract says every operation this
			// router serves needs a session; the check is a router built
			// over a provider with no authenticator wired at all.
			unauthenticated := NewRouter(RouterConfig{
				Platform: noAuthWiredAdapter{}, Backend: newSyncFakeBackend(),
				Gate: alwaysPassGate{}, BinaryVersion: "test", Commit: "test",
			})
			rec := httptest.NewRecorder()
			unauthenticated.ServeHTTP(rec, requestFor(t, e, true, true))
			if e.Authenticated {
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("the contract declares this operation authenticated, but an unauthenticated request got %d, not 401", rec.Code)
				}
			} else if rec.Code == http.StatusUnauthorized {
				t.Errorf("the contract declares this operation unauthenticated, but the route refused with 401")
			}

			authenticated := NewRouter(RouterConfig{
				Platform: allowingPlatform("alice"), Backend: newSyncFakeBackend(),
				Gate: alwaysPassGate{}, BinaryVersion: "test", Commit: "test",
			})

			// CSRF, with the token deliberately absent.
			rec = httptest.NewRecorder()
			authenticated.ServeHTTP(rec, requestFor(t, e, false, true))
			code := errorCodeOf(t, rec)
			if e.CSRFRequired {
				if code != string(apicontract.ErrorCodeCSRFTokenMissing) {
					t.Errorf("the contract declares CSRF required, but a request with no token got %d %q, not 403 CSRF_TOKEN_MISSING", rec.Code, code)
				}
			} else if code == string(apicontract.ErrorCodeCSRFTokenMissing) || code == string(apicontract.ErrorCodeCSRFTokenMismatch) {
				t.Errorf("the contract declares CSRF NOT required, but the route refused with %q", code)
			}

			// The destructive gate, with a gate that has not passed. This
			// is the production default (NotYetImplementedGate), so every
			// gated route must refuse here and no ungated one may.
			gated := NewRouter(RouterConfig{
				Platform: allowingPlatform("alice"), Backend: newSyncFakeBackend(),
				Gate: NotYetImplementedGate{}, BinaryVersion: "test", Commit: "test",
			})
			rec = httptest.NewRecorder()
			gated.ServeHTTP(rec, requestFor(t, e, true, true))
			code = errorCodeOf(t, rec)
			if e.DestructiveGate {
				if code != string(apicontract.ErrorCodeDestructiveOperationsDisabled) {
					t.Errorf("the contract puts this operation behind the destructive gate, but a closed gate returned %d %q, not 403 DESTRUCTIVE_OPERATIONS_DISABLED", rec.Code, code)
				}
			} else if code == string(apicontract.ErrorCodeDestructiveOperationsDisabled) {
				t.Errorf("the contract does NOT put this operation behind the destructive gate at the route level, but a closed gate refused it")
			}

			// The idempotency key, deliberately absent.
			if e.IdempotencyKey == "required" {
				rec = httptest.NewRecorder()
				authenticated.ServeHTTP(rec, requestFor(t, e, true, false))
				if rec.Code != http.StatusBadRequest || errorCodeOf(t, rec) != string(apicontract.ErrorCodeInvalidRequest) {
					t.Errorf("the contract requires an Idempotency-Key, but omitting it returned %d %q, not 400 INVALID_REQUEST", rec.Code, errorCodeOf(t, rec))
				}
			}
		})
	}
	if checked == 0 {
		t.Fatal("no operation was checked; this test would pass vacuously")
	}
}

// ----------------------------------------------------------- error codes ---

var (
	writeErrorCode = regexp.MustCompile(`write(?:Auth)?Error\([^,]+,\s*[^,]+,\s*"([A-Z][A-Z0-9_]+)"`)
	assignedCode   = regexp.MustCompile(`\.Code\s*=\s*"([A-Z][A-Z0-9_]+)"`)
)

// emittedWireCodes scans the two packages that actually put an error code
// on an /api/v1 response for the codes they emit.
//
// It reads source text rather than exercising every branch, because the
// question is "which tokens exist in this code at all", and a branch that
// no test reaches is exactly the one most likely to emit an unregistered
// code. A regexp is safe here in a way it would not be for a declaration:
// these are string literals in a specific call position, not identifiers
// that could equally appear in the prose this repository is full of.
func emittedWireCodes(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	roots := []string{".", filepath.Join("..", "auth", "local")}
	files := 0
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("reading %s: %v", root, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(root, name)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			files++
			for _, re := range []*regexp.Regexp{writeErrorCode, assignedCode} {
				for _, m := range re.FindAllStringSubmatch(string(src), -1) {
					found[m[1]] = path
				}
			}
		}
	}
	if files == 0 {
		t.Fatal("no non-test Go source was scanned for error codes; this check would pass vacuously")
	}
	if len(found) == 0 {
		t.Fatalf("scanned %d files and found no error code at all, which cannot be right; the scanning patterns have stopped matching", files)
	}
	return found
}

func TestContract_TheErrorCodeRegistryIsExactlyWhatTheHandlersEmit(t *testing.T) {
	emitted := emittedWireCodes(t)

	registered := map[string]bool{}
	for _, c := range apicontract.WireErrorCodes {
		registered[string(c)] = true
	}

	for code, path := range emitted {
		if !registered[code] {
			t.Errorf("%s emits error code %q, which api/v1/openapi.json does not register as a wire code. Add it to the ApiErrorCode enum and to x-code-origins, then re-run scripts/api/generate.sh: a code the UI has never heard of degrades to \"unknown\" and every comparison against it silently fails.", path, code)
		}
	}
	for code := range registered {
		if _, ok := emitted[code]; !ok {
			t.Errorf("the contract registers wire error code %q, but no handler in apps/common/webhost or apps/common/auth/local emits it. Either emit it or remove it: a registry with codes nothing produces teaches a client to handle failures that cannot happen.", code)
		}
	}
}

// TestContract_TypedRefusalsAreDistinguishable is the acceptance criterion
// that a red team can assert the RIGHT refusal rather than any refusal:
// an invalid payload, an absent session, a request lacking authorization
// and a stale concurrency token each produce a DIFFERENT typed code, each
// one declared by the contract for that operation and that status, and
// none of them runs any work.
func TestContract_TypedRefusalsAreDistinguishable(t *testing.T) {
	endpoint := contractEndpoints()["submitOperation"]

	cases := []struct {
		name       string
		platform   capabilities.PlatformAdapter
		gate       DestructiveGate
		backendErr error
		body       string
		csrf       bool
		idem       string
		wantStatus int
		wantCode   string
	}{
		{"success", allowingPlatform("alice"), alwaysPassGate{}, nil, `{"action":"run_cycle","config_revision":"rev-1"}`, true, "idem-ok", http.StatusAccepted, ""},
		{"invalid payload", allowingPlatform("alice"), alwaysPassGate{}, nil, `{"action":"run_cycle","config_revision":"rev-1"}`, true, "", http.StatusBadRequest, "INVALID_REQUEST"},
		{"absent session", noAuthWiredAdapter{}, alwaysPassGate{}, nil, `{"action":"run_cycle","config_revision":"rev-1"}`, true, "idem-1", http.StatusUnauthorized, "UNAUTHENTICATED"},
		{"session lacking authorization", allowingPlatform("alice"), NotYetImplementedGate{}, nil, `{"action":"run_cycle","config_revision":"rev-1"}`, true, "idem-2", http.StatusForbidden, "DESTRUCTIVE_OPERATIONS_DISABLED"},
		{"forged cross-site request", allowingPlatform("alice"), alwaysPassGate{}, nil, `{"action":"run_cycle","config_revision":"rev-1"}`, false, "idem-3", http.StatusForbidden, "CSRF_TOKEN_MISSING"},
		{"stale concurrency token", allowingPlatform("alice"), alwaysPassGate{}, service.ErrConfigRevisionStale, `{"action":"run_cycle","config_revision":"rev-0"}`, true, "idem-4", http.StatusConflict, "CONFIG_REVISION_STALE"},
		// The other two 409s. They were emitted by this handler and
		// declared by nothing, and driven by no contract test at all, so a
		// generated client typed its 409 body as the stale-revision shape
		// and read undefined out of two thirds of the real cases (PR #194
		// review, M2). Same status as the case above, deliberately: what
		// makes them usable is that the CODE differs, which is exactly
		// what the duplicate check below asserts.
		{"reused idempotency key", allowingPlatform("alice"), alwaysPassGate{}, service.ErrIdempotencyKeyConflict, `{"action":"run_cycle","config_revision":"rev-1"}`, true, "idem-5", http.StatusConflict, "IDEMPOTENCY_KEY_CONFLICT"},
		{"another run already in flight", allowingPlatform("alice"), alwaysPassGate{}, service.ErrOperationAlreadyRunning, `{"action":"run_cycle","config_revision":"rev-1"}`, true, "idem-6", http.StatusConflict, "OPERATION_ALREADY_RUNNING"},
	}

	seen := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := newSyncFakeBackend()
			backend.errOnSubmit = tc.backendErr
			router := NewRouter(RouterConfig{
				Platform: tc.platform, Backend: backend, Gate: tc.gate,
				BinaryVersion: "test", Commit: "test",
			})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/operations", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			if tc.csrf {
				attachValidCSRF(req)
			}
			if tc.idem != "" {
				req.Header.Set("Idempotency-Key", tc.idem)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantCode == "" {
				if len(backend.ops) == 0 {
					t.Error("the success case ran no work, so the refusal cases below prove nothing by contrast")
				}
				return
			}

			code := errorCodeOf(t, rec)
			if code != tc.wantCode {
				t.Fatalf("code = %q, want %q", code, tc.wantCode)
			}
			if prev, dup := seen[code]; dup {
				t.Errorf("code %q is also what %q produces; these refusals are not distinguishable", code, prev)
			}
			seen[code] = tc.name

			declared := endpoint.ErrorCodes[tc.wantStatus]
			ok := false
			for _, c := range declared {
				if string(c) == tc.wantCode {
					ok = true
				}
			}
			if !ok {
				t.Errorf("the handler returned %d %q, which api/v1/openapi.json does not declare for submitOperation at that status (it declares %v)", tc.wantStatus, tc.wantCode, declared)
			}
			if n := len(backend.ops); n != 0 {
				t.Errorf("a refused request still ran %d operation(s); a refusal must run no work at all", n)
			}
		})
	}
}

// TestContract_EveryRefusalTheseRoutesReturnIsDeclaredForThatOperation is
// the same assertion as TestContract_TypedRefusalsAreDistinguishable, on
// the two routes whose refusals the contract did not declare at all (PR
// #194 review, M3): previewRetention's 400 INVALID_REQUEST, which it
// inherits from sharing writeRetentionServiceError with apply, and
// listStorageStatus's 500 INTERNAL.
//
// TestContract_TheErrorCodeRegistryIsExactlyWhatTheHandlersEmit cannot see
// either of these: it diffs two flat sets, so a code that is registered
// SOMEWHERE and emitted SOMEWHERE passes regardless of which operation it
// belongs to. Per-operation is the granularity a client consumes.
func TestContract_EveryRefusalTheseRoutesReturnIsDeclaredForThatOperation(t *testing.T) {
	endpoints := contractEndpoints()

	cases := []struct {
		name      string
		operation string
		method    string
		target    string
		// csrf attaches a valid double-submit pair, which every POST here
		// needs: requireCSRF runs first and refuses with its own 403, so
		// without this a mutating route's real refusal is never reached.
		csrf       bool
		arrange    func(*syncFakeBackend)
		wantStatus int
		wantCode   string
	}{
		{
			name:      "a backup set id the model layer will not accept",
			operation: "previewRetention",
			method:    http.MethodGet,
			target:    "/api/v1/backup-sets/src/set-1/retention/preview",
			arrange: func(b *syncFakeBackend) {
				b.errOnPreview = fmt.Errorf("%w: backup set id has an empty source", service.ErrInvalidRequest)
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_REQUEST",
		},
		{
			name:      "capacity assessment that could not be made",
			operation: "listStorageStatus",
			method:    http.MethodGet,
			target:    "/api/v1/system/storage",
			arrange: func(b *syncFakeBackend) {
				b.errOnStorage = errors.New("statfs: no such file or directory")
			},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "INTERNAL",
		},
		// Issue #211's three new refusals. Each is one an operator reaches
		// by clicking a button on a screen that has gone stale, so each
		// has to be a code the client was told about for THAT operation,
		// not merely a code registered somewhere in the document.
		{
			name:       "a backup id that names nothing",
			operation:  "getArtifact",
			method:     http.MethodGet,
			target:     "/api/v1/backups/src/set-1/gone.dump",
			arrange:    func(*syncFakeBackend) {},
			wantStatus: http.StatusNotFound,
			wantCode:   "ARTIFACT_NOT_FOUND",
		},
		{
			name:      "retrying a backup with no source left to re-ingest",
			operation: "retryArtifactIngestion",
			method:    http.MethodPost,
			target:    "/api/v1/quarantine/src/set-1/lost.dump/retry",
			csrf:      true,
			arrange: func(b *syncFakeBackend) {
				b.errOnRetry = fmt.Errorf("%w: lost", service.ErrArtifactIrrecoverable)
			},
			wantStatus: http.StatusConflict,
			wantCode:   "ARTIFACT_IRRECOVERABLE",
		},
		{
			name:      "revalidating a backup that is not quarantined",
			operation: "revalidateArtifact",
			method:    http.MethodPost,
			target:    "/api/v1/quarantine/src/set-1/backup.dump/revalidate",
			csrf:      true,
			arrange: func(b *syncFakeBackend) {
				b.errOnRevalidate = fmt.Errorf("%w: healthy", service.ErrArtifactNotQuarantined)
			},
			wantStatus: http.StatusConflict,
			wantCode:   "ARTIFACT_NOT_QUARANTINED",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := newSyncFakeBackend()
			tc.arrange(backend)
			router := NewRouter(RouterConfig{
				Platform: allowingPlatform("alice"), Backend: backend, Gate: alwaysPassGate{},
				BinaryVersion: "test", Commit: "test",
			})
			req := httptest.NewRequest(tc.method, tc.target, nil)
			if tc.csrf {
				attachValidCSRF(req)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if code := errorCodeOf(t, rec); code != tc.wantCode {
				t.Fatalf("code = %q, want %q", code, tc.wantCode)
			}

			declared := endpoints[tc.operation].ErrorCodes[tc.wantStatus]
			for _, c := range declared {
				if string(c) == tc.wantCode {
					return
				}
			}
			t.Errorf("the handler returned %d %q, which api/v1/openapi.json does not declare for %s at that status (it declares %v). A refusal a client is never told about is one it cannot handle.", tc.wantStatus, tc.wantCode, tc.operation, declared)
		})
	}
}

// -------------------------------------------------------- runtime profiles ---

// profileAdapter is a runtime profile's whole visible surface as far as
// this router is concerned: an identifier, a capability declaration and an
// authenticator. That is deliberately all a profile gets to change (#81's
// standing constraint: "a profile changes the auth bridge, notifications,
// launch behavior and capability reporting; it never changes lifecycle
// semantics"), which is what makes the parity test below meaningful rather
// than tautological. If a profile could reach anything else, this type
// would need more fields.
type profileAdapter struct {
	id   capabilities.PlatformID
	caps capabilities.PlatformCapabilities
}

func (p profileAdapter) ID() capabilities.PlatformID { return p.id }

func (p profileAdapter) Capabilities() capabilities.PlatformCapabilities { return p.caps }

func (p profileAdapter) Authenticator() capabilities.Authenticator {
	return fakeAuthenticator{authenticated: true, username: "alice"}
}

func (p profileAdapter) Notifier() capabilities.Notifier { return nil }

func (p profileAdapter) PlatformInfo(_ context.Context) (capabilities.PlatformInfo, error) {
	return capabilities.PlatformInfo{ID: p.id, Name: string(p.id)}, nil
}

// genericProfile and ugosProfile are the two profiles #166 requires to
// expose identical backup API semantics. They differ in exactly the two
// things a profile is allowed to differ in: which platform they report,
// and what that platform can do.
func genericProfile() capabilities.PlatformAdapter {
	return profileAdapter{
		id:   capabilities.PlatformGeneric,
		caps: capabilities.PlatformCapabilities{},
	}
}

func ugosProfile() capabilities.PlatformAdapter {
	return profileAdapter{
		id: capabilities.PlatformUGOS,
		caps: capabilities.PlatformCapabilities{
			NativeAuth: true, NativeNotifications: true,
			StoragePicker: true, EmbeddedWindow: true, AppStorePackaging: true,
		},
	}
}

// TestProfileParity_TheBackupAPIIsIdenticalAcrossRuntimeProfiles is #166's
// profile-parity requirement. It needs no UGREEN hardware, no UGOS gateway
// and no .UPK: a runtime profile is a PlatformAdapter, so "the same binary
// under two profiles" is two routers built from the same NewRouter over the
// same backend with two different adapters, which is exactly what the
// --profile flag will select once #167 (B6.3) adds it.
//
// The assertion is deliberately byte-level on the response bodies rather
// than "both returned 200": a profile that quietly reshaped a backup
// response would pass a status-only comparison.
func TestProfileParity_TheBackupAPIIsIdenticalAcrossRuntimeProfiles(t *testing.T) {
	build := func(p capabilities.PlatformAdapter) http.Handler {
		return NewRouter(RouterConfig{
			Platform: p, Backend: newBackupSetFakeBackend(), Gate: alwaysPassGate{},
			BinaryVersion: "test", Commit: "test",
		})
	}
	generic, ugos := build(genericProfile()), build(ugosProfile())

	// Two calls a few microseconds apart carry different timestamps, and
	// that is true of two calls to the SAME profile, so comparing raw
	// bodies would report clock movement as a profile difference. Redact
	// the timestamps and nothing else.
	//
	// This is not a licence to redact whatever fails: the capability
	// endpoint below is compared through this same redaction and is
	// REQUIRED to differ, which is what proves the redaction has not
	// flattened the comparison into one that cannot fail.
	timestamps := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T[\d:.]+(?:Z|[+-]\d{2}:\d{2})`)
	redact := func(body string) string { return timestamps.ReplaceAllString(body, "<timestamp>") }

	// Every route, under both profiles, has to be the same route.
	routesOf := func(h http.Handler) []string {
		var out []string
		if err := chi.Walk(routableFor(t, h), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			out = append(out, method+" "+route)
			return nil
		}); err != nil {
			t.Fatalf("chi.Walk: %v", err)
		}
		sort.Strings(out)
		return out
	}
	if a, b := routesOf(generic), routesOf(ugos); !reflect.DeepEqual(a, b) {
		t.Fatalf("the two profiles serve different routes.\n generic: %v\n ugos:    %v", a, b)
	}

	// Every backup operation, under both profiles, has to mean the same
	// thing. The capability endpoint is the one deliberate exception, and
	// it is asserted to differ rather than skipped, so a profile mechanism
	// that had quietly stopped working could not pass this test by making
	// everything identical.
	compared, differed := 0, 0
	for _, e := range endpointsForThisRouter() {
		b := contractBindings[e.ID]
		call := func(h http.Handler) (int, string) {
			var body *strings.Reader
			if e.RequestSchema != "" {
				body = strings.NewReader("{}")
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(e.Method, b.url, body)
			req.Header.Set("Content-Type", "application/json")
			attachValidCSRF(req)
			req.Header.Set("Idempotency-Key", "parity-"+e.ID)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec.Code, redact(rec.Body.String())
		}
		gStatus, gBody := call(generic)
		uStatus, uBody := call(ugos)

		if e.ID == "getSystemCapabilities" {
			differed++
			if gBody == uBody {
				t.Errorf("the two profiles reported identical capabilities, so this test is not actually exercising two different profiles")
			}
			continue
		}
		compared++
		if gStatus != uStatus || gBody != uBody {
			t.Errorf("%s %s differs between the generic and ugos profiles.\n generic: %d %s\n ugos:    %d %s\nA runtime profile may change the auth bridge, notifications, launch behaviour and capability reporting. It may never change what a backup endpoint means.",
				e.Method, e.Path, gStatus, gBody, uStatus, uBody)
		}
	}
	if compared == 0 {
		t.Fatal("no operation was compared across profiles; this test would pass vacuously")
	}
	if differed == 0 {
		t.Fatal("the capability endpoint was never compared, so nothing proved the two profiles are actually different")
	}
}

// TestContract_CapabilitiesAreDataAboutThePlatform pins the shape #166
// documents: the capability endpoint reports WHICH platform, and what that
// platform can do, as data. A capability is never an authorization
// decision, which is why every field here is also reported truthfully for
// a profile that supports nothing.
func TestContract_CapabilitiesAreDataAboutThePlatform(t *testing.T) {
	for _, tc := range []struct {
		profile      capabilities.PlatformAdapter
		wantPlatform string
		wantAll      bool
	}{
		{genericProfile(), "generic", false},
		{ugosProfile(), "ugos", true},
	} {
		router := NewRouter(RouterConfig{
			Platform: tc.profile, Backend: newSyncFakeBackend(), Gate: alwaysPassGate{},
			BinaryVersion: "test", Commit: "test",
		})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/capabilities", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body: %s", tc.wantPlatform, rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode: %v", tc.wantPlatform, err)
		}
		if body["platform"] != tc.wantPlatform {
			t.Errorf("platform = %v, want %q", body["platform"], tc.wantPlatform)
		}
		for _, field := range apicontract.CapabilityFields {
			got, ok := body[field]
			if !ok {
				t.Errorf("%s: the contract declares capability %q, which the response does not carry", tc.wantPlatform, field)
				continue
			}
			if got != tc.wantAll {
				t.Errorf("%s: capability %q = %v, want %v (a capability must report what the platform really does, never an emulation)", tc.wantPlatform, field, got, tc.wantAll)
			}
		}
		if len(body) != len(apicontract.CapabilityFields)+1 {
			t.Errorf("%s: the response carries %d fields; the contract declares %d capabilities plus platform. An undeclared field on this endpoint is drift.", tc.wantPlatform, len(body), len(apicontract.CapabilityFields)+1)
		}
	}
}

package local

import (
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/rclone-manager/apps/common/webhost/apicontract"
)

// The /auth half of issue #166's contract gate.
//
// The /auth operations belong to the same /api/v1 contract as everything
// else (apps/common/webhost/serve mounts this package's Handler at
// /api/v1/auth/), but they are served by this package rather than by
// webhost's own router, so this is where they are held to the contract.
// apps/common/webhost/contract_test.go holds the other 16 and skips these
// by path prefix, which is why neither file can silently stop covering an
// operation: an operation dropped from one has to appear in the other or
// the contract's own count changes.
//
// The dependency runs test-only: nothing in this package's production code
// imports the generated bindings. That is deliberate for now. Handlers
// adopting the generated types is a separate, behaviour-preserving move,
// and #81 is explicit that behaviour is not rewritten in the same step as
// a boundary being introduced.

// authBindings ties one contract operation to this package's own request
// and response types and to the route chi registers for it, with the
// /api/v1/auth prefix already stripped the way serve.NewEngine strips it.
var authBindings = map[string]struct {
	route    string
	request  any
	response any
}{
	"login":                 {"/login", credentialsRequest{}, nil},
	"enrollAdministrator":   {"/enroll", credentialsRequest{}, nil},
	"rotatePassword":        {"/password", rotatePasswordRequest{}, nil},
	"logout":                {"/logout", nil, nil},
	"getSession":            {"/session", nil, sessionResponse{}},
	"x-auth-error-envelope": {"", nil, authErrorResponse{}},
}

// authErrorEnvelopeSchema is the schema the pseudo-binding above pins: the
// FLAT error body this package returns, which is a different shape from the
// nested envelope every other /api/v1 operation uses. Recording both in the
// contract is what stops a client being told there is one error shape when
// there are two.
const authErrorEnvelopeSchema = "AuthErrorResponse"

func contractAuthEndpoints(t *testing.T) map[string]apicontract.Endpoint {
	t.Helper()
	out := map[string]apicontract.Endpoint{}
	for _, e := range apicontract.Endpoints {
		if strings.HasPrefix(e.Path, "/auth/") {
			out[e.ID] = e
		}
	}
	if len(out) == 0 {
		t.Fatal("the contract declares no /auth operations at all; this file would pass vacuously")
	}
	return out
}

// jsonShape mirrors apps/common/webhost's own flattening, deliberately as a
// copy rather than as a shared helper: it is eleven lines of reflection in
// a _test.go file, and exporting it would put a testing utility into the
// production surface of a package whose whole job is authentication.
func jsonShape(t reflect.Type) map[string]string {
	out := map[string]string{}
	addStruct(out, "", t)
	return out
}

func addStruct(out map[string]string, prefix string, t reflect.Type) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && tag == "" {
			addStruct(out, prefix, f.Type)
			continue
		}
		if name == "" {
			name = f.Name
		}
		desc := f.Type.Kind().String()
		if f.Type.Kind() == reflect.String {
			desc = "string"
		}
		if strings.Contains(opts, "omitempty") {
			desc += " omitempty"
		}
		out[prefix+name] = desc
		if f.Type.Kind() == reflect.Struct {
			addStruct(out, prefix+name+".", f.Type)
		}
	}
}

func TestContract_AuthHandlerShapesMatchTheContract(t *testing.T) {
	endpoints := contractAuthEndpoints(t)
	checked := 0

	for opID, binding := range authBindings {
		requestSchema, responseSchema := "", ""
		if opID == "x-auth-error-envelope" {
			responseSchema = authErrorEnvelopeSchema
		} else {
			e, ok := endpoints[opID]
			if !ok {
				t.Errorf("authBindings names %q, which the contract does not declare as an /auth operation", opID)
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
			if pair.schema == "" || pair.handler == nil {
				t.Errorf("%s: the contract declares %s schema %q and the handler side has %v; one of the two is missing", opID, pair.kind, pair.schema, pair.handler)
				continue
			}
			generated, ok := apicontract.SchemaTypes[pair.schema]
			if !ok {
				t.Errorf("%s: the contract names schema %q, which the generated bindings do not contain. Re-run scripts/api/generate.sh.", opID, pair.schema)
				continue
			}
			checked++
			want := jsonShape(reflect.TypeOf(generated))
			got := jsonShape(reflect.TypeOf(pair.handler))
			if !reflect.DeepEqual(want, got) {
				for _, field := range union(want, got) {
					w, inWant := want[field]
					g, inGot := got[field]
					switch {
					case !inGot:
						t.Errorf("%s %s: field %q is in the contract but not in %T", opID, pair.kind, field, pair.handler)
					case !inWant:
						t.Errorf("%s %s: field %q is in %T but not in the contract. Add it to api/v1/openapi.json and re-run scripts/api/generate.sh.", opID, pair.kind, field, pair.handler)
					case w != g:
						t.Errorf("%s %s: field %q is %q in the contract and %q in %T", opID, pair.kind, field, w, g, pair.handler)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no /auth type was compared against the contract; this test would pass vacuously")
	}
}

func union(a, b map[string]string) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestContract_AuthRoutesAndContractDeclareTheSameOperations(t *testing.T) {
	svc, err := New(Config{StorePath: filepath.Join(t.TempDir(), "auth.json")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	routes, ok := svc.Handler().(chi.Routes)
	if !ok {
		t.Fatalf("Handler() is %T, which chi.Walk cannot read", svc.Handler())
	}

	registered := map[string]bool{}
	if err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		registered[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if len(registered) == 0 {
		t.Fatal("this service registers no routes at all; the comparison below would pass vacuously")
	}

	declared := map[string]string{}
	for id, e := range contractAuthEndpoints(t) {
		b, ok := authBindings[id]
		if !ok {
			t.Errorf("the contract declares /auth operation %q, which authBindings does not implement", id)
			continue
		}
		if want := strings.TrimPrefix(e.Path, "/auth"); want != b.route {
			t.Errorf("operation %q: the contract path is %q, which strips to %q, but authBindings says the route is %q", id, e.Path, want, b.route)
		}
		declared[e.Method+" "+b.route] = id
	}

	for key := range registered {
		if _, ok := declared[key]; !ok {
			t.Errorf("this service serves %q, which the contract does not declare. An authentication route outside the contract is a surface no client and no adapter conformance run can see.", key)
		}
	}
	for key, id := range declared {
		if !registered[key] {
			t.Errorf("the contract declares %q (operation %q), which this service does not serve", key, id)
		}
	}
}

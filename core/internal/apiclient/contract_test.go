package apiclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/spdrman/rclone-manager/core/apicontract"
)

// This file asks the two questions the client's own behaviour cannot
// answer about itself.
//
// The first is whether every request it builds is an operation the
// contract declares. ui/shared learned that one expensively: fourteen
// (method, path) pairs its client asked for existed in neither the
// contract nor the router, four of six shipped pages failed outright, and
// every suite in the repository was green throughout, because the browser
// tests ran against a mock that implements whatever it is asked for.
// scripts/api/check-client-paths.sh reads the TypeScript client
// statically for exactly this reason. Go can do better than static
// reading: the methods are enumerable, so this drives every one of them
// and looks at what came out.
//
// The second is whether the three wire names the client cannot get from
// the generated binding still match the contract. The session cookie, the
// CSRF header and the CSRF cookie are declared in
// api/v1/openapi.json's securitySchemes and implemented in
// apps/common/auth/local and apps/common/csrf, which core may not import.
// A constant copied across an import boundary that nothing compares is a
// constant that drifts, so this compares it.

func contractPath(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "api", "v1", "openapi.json")
}

func TestContract_TheClientReadsTheSameDocumentTheBindingWasMadeFrom(t *testing.T) {
	raw, err := os.ReadFile(contractPath(t))
	if err != nil {
		t.Fatalf("reading the contract: %v", err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != apicontract.ContractSHA256 {
		t.Fatalf("api/v1/openapi.json hashes to %s, but the generated binding was made from %s.\nRun scripts/api/generate.sh and commit both generated files.\nUntil they agree, the securityScheme assertions below check a different document from the one the client's shapes came from.", got, apicontract.ContractSHA256)
	}
}

func TestContract_TheWireNamesTheClientUsesAreTheOnesTheContractDeclares(t *testing.T) {
	raw, err := os.ReadFile(contractPath(t))
	if err != nil {
		t.Fatalf("reading the contract: %v", err)
	}
	var doc struct {
		Components struct {
			SecuritySchemes map[string]struct {
				Type string `json:"type"`
				In   string `json:"in"`
				Name string `json:"name"`
			} `json:"securitySchemes"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the contract: %v", err)
	}
	if len(doc.Components.SecuritySchemes) == 0 {
		t.Fatal("the contract declares no security schemes at all, so this comparison verified nothing")
	}

	for _, tc := range []struct {
		scheme string
		in     string
		used   string
	}{
		{"session", "cookie", sessionCookieName},
		{"csrf", "header", csrfHeaderName},
		{"csrfCookie", "cookie", csrfCookieName},
	} {
		declared, ok := doc.Components.SecuritySchemes[tc.scheme]
		if !ok {
			t.Errorf("the contract declares no %q security scheme, and this client is built on one existing", tc.scheme)
			continue
		}
		if declared.In != tc.in {
			t.Errorf("the contract carries %q in the %s, and this client sends it in the %s", tc.scheme, declared.In, tc.in)
		}
		if declared.Name != tc.used {
			t.Errorf("the contract names %q %q, and this client uses %q", tc.scheme, declared.Name, tc.used)
		}
	}
}

// TestContract_EveryCallThisClientMakesIsAnOperationTheContractDeclares
// drives every exported method on *Client and checks what reached the
// wire.
//
// Enumerated by reflection rather than listed, which is the half that
// matters: a method added by #543 or #544 and forgotten here would be
// unchecked, and a list is exactly the kind of thing that gets forgotten.
// It also fails closed in the two directions that would otherwise pass
// vacuously - a method that makes no request at all is a failure, and a
// run that found no methods is a failure.
func TestContract_EveryCallThisClientMakesIsAnOperationTheContractDeclares(t *testing.T) {
	declared := map[[2]string]string{}
	for _, ep := range apicontract.Endpoints {
		declared[[2]string{ep.Method, ep.Path}] = ep.ID
	}
	if len(declared) == 0 {
		t.Fatal("the contract declares no operations, so every request would be reported for the wrong reason")
	}

	clientType := reflect.TypeOf(&Client{})
	if clientType.NumMethod() == 0 {
		t.Fatal("*Client has no exported methods, so this check compared nothing")
	}

	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()

	for i := range clientType.NumMethod() {
		method := clientType.Method(i)
		t.Run(method.Name, func(t *testing.T) {
			engine := newFakeEngine(t)
			client := engine.client(t, engine.start())

			// Sign in first and then forget it happened, so what the
			// method itself asked for is all that is left to look at.
			if _, err := client.Session(context.Background()); err != nil {
				t.Fatalf("Session: %v", err)
			}
			engine.reset()

			// A call takes a context; an accessor does not. That is the
			// rule this check separates them by, rather than a list of
			// method names to ignore, because a list is a place to hide a
			// method somebody did not want checked. It cuts both ways: a
			// method with no context has to make no request, so an
			// "accessor" that quietly talked to the engine would fail here
			// rather than escape the check.
			isCall := method.Type.NumIn() > 1 && method.Type.In(1) == ctxType

			fn := method.Func
			args := make([]reflect.Value, 0, method.Type.NumIn())
			args = append(args, reflect.ValueOf(client))
			for p := 1; p < method.Type.NumIn(); p++ {
				in := method.Type.In(p)
				switch {
				case in == ctxType:
					args = append(args, reflect.ValueOf(context.Background()))
				case in.Kind() == reflect.String:
					// A path parameter. Deliberately something that has to
					// be escaped: a client that pastes it in raw builds a
					// path this check would then read as a different one.
					args = append(args, reflect.ValueOf("a b/c"))
				case in.Kind() == reflect.Struct:
					args = append(args, reflect.Zero(in))
				case in.Kind() == reflect.Int:
					// A count, not a path parameter: ListActivity's limit
					// is the only one so far. A positive value on purpose,
					// because zero means "send no query at all" and would
					// exercise the branch that builds the SIMPLER request
					// of the two.
					args = append(args, reflect.ValueOf(7))
				default:
					t.Fatalf("%s takes a %s, which this check does not know how to supply. Teach it, rather than letting the method go unchecked.", method.Name, in)
				}
			}
			fn.Call(args)

			seen := engine.requests()
			if !isCall {
				if len(seen) != 0 {
					t.Fatalf("%s takes no context and still made %d request(s). Every call this client makes has to be cancellable, and a request that is not is one no command can abandon.", method.Name, len(seen))
				}
				return
			}
			if len(seen) == 0 {
				t.Fatalf("%s made no request at all. A client method whose path this check cannot read is not checked, which is the one outcome it must never produce silently.", method.Name)
			}
			for _, r := range seen {
				rel := r.Path
				if len(rel) < len(apicontract.BasePath) || rel[:len(apicontract.BasePath)] != apicontract.BasePath {
					t.Errorf("%s requested %s %s, which is not under %s", method.Name, r.Method, r.Path, apicontract.BasePath)
					continue
				}
				if r.Operation == "" {
					t.Errorf("%s requested %s %s, which is not an operation %s declares. A real engine answers that with a 404 or a 405.", method.Name, r.Method, r.Path, contractPath(t))
				}
			}
		})
	}
}

// TestContract_TheClientRefusesAnOperationTheContractDoesNotDeclare is the
// positive control for the check above: it proves the client resolves its
// paths through apicontract.Endpoints rather than building them from
// literals that merely happen to agree with it today.
func TestContract_TheClientRefusesAnOperationTheContractDoesNotDeclare(t *testing.T) {
	engine := newFakeEngine(t)
	client := engine.client(t, engine.start())

	err := client.call(context.Background(), "thisOperationDoesNotExist", nil, nil, nil)
	if err == nil {
		t.Fatal("the client built a request for an operation the contract does not declare")
	}
	var violation *ContractViolation
	if !errors.As(err, &violation) {
		t.Fatalf("error is %T (%v), want a *ContractViolation", err, err)
	}
	if len(engine.requests()) != 0 {
		t.Errorf("the client sent %d request(s) for an operation that does not exist; it has to refuse before the wire", len(engine.requests()))
	}
}

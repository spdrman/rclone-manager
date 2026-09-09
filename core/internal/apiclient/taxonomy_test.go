package apiclient

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Whether the error taxonomy is exhaustive, which is the property that
// decides whether a caller can switch on it at all.
//
// doc.go says four things happen to a call and they must never be reported
// as one. That is only useful to #543 and #544 if EVERY failure is one of
// them: a command writing the obvious exhaustive switch and getting a
// silent default branch is back to ui/shared's single "the backup service
// returned an unexpected response", which is the thing this package exists
// to stop. A marshal failure, a request that could not be built and all of
// New's rejections used to be bare fmt.Errorf values matching nothing.
//
// So there are two checks here rather than one. The first walks the source
// and looks at every return, because the failure that matters is the one
// somebody adds next. The second drives one instance of each category
// against a real server and looks at what came out, because a source walk
// alone would be satisfied by a package that returns the right types for
// the wrong reasons.

// errorCategories is the whole taxonomy, and this list is the switch a
// command writes. Adding a type here without adding it to doc.go is the
// mistake this list makes visible.
var errorCategories = []struct {
	name string
	as   func(error) bool
}{
	{"*ConfigError", func(err error) bool { var t *ConfigError; return errors.As(err, &t) }},
	{"*Unreachable", func(err error) bool { var t *Unreachable; return errors.As(err, &t) }},
	{"*Error", func(err error) bool { var t *Error; return errors.As(err, &t) }},
	{"*ContractViolation", func(err error) bool { var t *ContractViolation; return errors.As(err, &t) }},
	{"*NoCredentials", func(err error) bool { var t *NoCredentials; return errors.As(err, &t) }},
}

func TestTaxonomy_EveryFailureThisPackageProducesIsOneOfTheCategories(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedBase := closed.URL
	closed.Close()

	cases := []struct {
		name string
		want string
		run  func(*testing.T) error
	}{
		{
			name: "no base URL at all",
			want: "*ConfigError",
			run:  func(*testing.T) error { _, err := New(Config{}); return err },
		},
		{
			name: "a base URL that is not a URL",
			want: "*ConfigError",
			run:  func(*testing.T) error { _, err := New(Config{BaseURL: "http://[::1"}); return err },
		},
		{
			name: "a scheme that is not HTTP",
			want: "*ConfigError",
			run: func(*testing.T) error {
				_, err := New(Config{BaseURL: "unix:///var/run/rclone-manager.sock"})
				return err
			},
		},
		{
			name: "the API's own URL rather than the host's",
			want: "*ConfigError",
			run:  func(*testing.T) error { _, err := New(Config{BaseURL: "http://127.0.0.1:8080/api/v1"}); return err },
		},
		{
			name: "nothing listening",
			want: "*Unreachable",
			run: func(t *testing.T) error {
				client, err := New(Config{BaseURL: closedBase, Username: "operator", Password: "correct-horse-battery"})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				_, err = client.ListBackupSets(context.Background())
				return err
			},
		},
		{
			name: "the engine refused the credentials",
			want: "*Error",
			run: func(t *testing.T) error {
				engine := newFakeEngine(t)
				client, err := New(Config{BaseURL: engine.start(), Username: engine.username, Password: "not-the-password"})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				_, err = client.ListBackupSets(context.Background())
				return err
			},
		},
		{
			name: "no credentials to sign in with",
			want: "*NoCredentials",
			run: func(t *testing.T) error {
				engine := newFakeEngine(t)
				client, err := New(Config{BaseURL: engine.start()})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				_, err = client.ListBackupSets(context.Background())
				return err
			},
		},
		{
			name: "a body that cannot be encoded",
			want: "*ContractViolation",
			run: func(t *testing.T) error {
				engine := newFakeEngine(t)
				client := engine.client(t, engine.start())
				// The review's own demonstration. A body this client cannot
				// marshal is a client-side breach of the contract's request
				// schema, found before a request was made, which is exactly
				// what ContractViolation's Status 0 is for.
				return client.call(context.Background(), "createBackupSet", nil, make(chan int), nil)
			},
		},
		{
			name: "an answer the contract does not describe",
			want: "*ContractViolation",
			run: func(t *testing.T) error {
				engine := newFakeEngine(t)
				engine.answer["listBackupSets"] = func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTeapot)
					_, _ = w.Write([]byte(`{"error":{"code":"INTERNAL","message":"nope"}}`))
				}
				client := engine.client(t, engine.start())
				_, err := client.ListBackupSets(context.Background())
				return err
			},
		},
		{
			name: "an operation the contract does not declare",
			want: "*ContractViolation",
			run: func(t *testing.T) error {
				engine := newFakeEngine(t)
				client := engine.client(t, engine.start())
				return client.call(context.Background(), "thisOperationDoesNotExist", nil, nil, nil)
			},
		},
	}

	covered := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(t)
			if err == nil {
				t.Fatal("the call succeeded, so this case checked no error at all")
			}
			var matched []string
			for _, category := range errorCategories {
				if category.as(err) {
					matched = append(matched, category.name)
				}
			}
			if len(matched) == 0 {
				t.Fatalf("error is %T (%v), and matches none of the categories a caller can switch on. A command switching on this taxonomy reaches its default branch and can say nothing useful.", err, err)
			}
			if len(matched) > 1 {
				t.Fatalf("error is %T (%v) and matches %v; the categories have four different remedies and an error in two of them names neither", err, err, matched)
			}
			if matched[0] != tc.want {
				t.Errorf("error is %T (%v), categorised as %s, want %s", err, err, matched[0], tc.want)
			}
		})
		covered[tc.want] = true
	}

	// Fails closed: a category nothing here produces is a category nobody
	// has watched, and this table is what makes the claim "exhaustive".
	for _, category := range errorCategories {
		if !covered[category.name] {
			t.Errorf("no case in this table produces a %s, so nothing checked that one", category.name)
		}
	}
}

// TestTaxonomy_NoReturnPathEscapesTheTaxonomy reads the package's own
// source, because the failure that matters is the one added next.
//
// It walks New and every method on *Client - the functions whose errors
// reach a caller - and refuses a return of a bare fmt.Errorf or
// errors.New. Those are precisely what the marshal failure, the
// unbuildable request and all five of New's rejections used to be.
func TestTaxonomy_NoReturnPathEscapesTheTaxonomy(t *testing.T) {
	fset, files := parsePackageSource(t)

	typed := map[string]bool{}
	for _, category := range errorCategories {
		typed[strings.TrimPrefix(category.name, "*")] = true
	}

	functions, returns := 0, 0
	for name, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !returnsError(fn) {
				continue
			}
			receiver := clientReceiver(fn)
			if receiver == "" && fn.Name.Name != "New" {
				// A helper that produces a reason for one of the types
				// above to carry. Its error never leaves the package
				// unwrapped; the table test is what holds that.
				continue
			}
			functions++

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if _, ok := n.(*ast.FuncLit); ok {
					// A closure is a different function with a different
					// caller. The one in New is net/http's redirect policy,
					// whose error net/http consumes itself and never hands
					// to anybody switching on this taxonomy.
					return false
				}
				ret, ok := n.(*ast.ReturnStmt)
				if !ok || len(ret.Results) == 0 {
					return true
				}
				returns++
				last := ret.Results[len(ret.Results)-1]
				if returnedErrorIsTyped(last, receiver, typed) {
					return true
				}
				t.Errorf("%s: %s returns an error this taxonomy does not name (%s). Every failure has to be one of %v, or a command switching on them reaches a default branch that can say nothing.",
					name, fn.Name.Name, fset.Position(last.Pos()), keys(typed))
				return true
			})
		}
	}
	if functions == 0 || returns == 0 {
		t.Fatalf("this check walked %d function(s) and %d return(s), so it verified nothing", functions, returns)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func returnsError(fn *ast.FuncDecl) bool {
	results := fn.Type.Results
	if results == nil || len(results.List) == 0 {
		return false
	}
	last := results.List[len(results.List)-1].Type
	ident, ok := last.(*ast.Ident)
	return ok && ident.Name == "error"
}

// clientReceiver returns the name a *Client method calls itself by, or ""
// for a plain function.
func clientReceiver(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	field := fn.Recv.List[0]
	star, ok := field.Type.(*ast.StarExpr)
	if !ok {
		return ""
	}
	ident, ok := star.X.(*ast.Ident)
	if !ok || ident.Name != "Client" {
		return ""
	}
	if len(field.Names) == 0 {
		return "_"
	}
	return field.Names[0].Name
}

// returnedErrorIsTyped decides whether one returned expression is
// something the taxonomy covers.
func returnedErrorIsTyped(expr ast.Expr, receiver string, typed map[string]bool) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		// nil, or an error already produced somewhere this walk covers.
		return true
	case *ast.UnaryExpr:
		lit, ok := e.X.(*ast.CompositeLit)
		if !ok {
			return false
		}
		name, ok := lit.Type.(*ast.Ident)
		return ok && typed[name.Name]
	case *ast.CallExpr:
		switch fun := e.Fun.(type) {
		case *ast.Ident:
			// A function in this package.
			return true
		case *ast.SelectorExpr:
			// c.something(...) is propagation; fmt.Errorf(...) is not.
			x, ok := fun.X.(*ast.Ident)
			return ok && x.Name == receiver && receiver != ""
		}
		return false
	}
	return false
}

// parsePackageSource parses this package's own non-test files.
//
// os.ReadDir and ParseFile rather than parser.ParseDir, which Go 1.25
// deprecated: it does not consider build tags. This package has none, and
// the check below is about what the source says rather than about which
// files a build would pick, but a deprecated call in a gate is a thing
// somebody has to explain later.
func parsePackageSource(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		files[name] = parsed
	}
	if len(files) == 0 {
		t.Fatal("no source file was parsed, so this check read nothing")
	}
	return fset, files
}

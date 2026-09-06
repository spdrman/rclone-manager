package alert_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/alert"
)

// This file is this work package's own gate, in the same spirit as
// internal/transport/rclone/gate_test.go: docs/EPIC-B-multi-nas.md §71
// says "do not add a broad notification framework in v1", and the only
// way that stays true is a test that fails the moment one starts
// growing. It checks two independent things:
//
//   - exactly one delivery mechanism is reachable at a time (one Sink
//     field, never a slice or map of them);
//   - none of the vocabulary a general pub/sub framework brings with it
//     (Subscribe, Publish, Emit, Broadcast, handler registration) exists
//     anywhere in this package's exported surface.

// frameworkNames is the vocabulary a general notification/event framework
// arrives with. None of it belongs in a v1 whose whole job is delivering
// four specific conditions through one mechanism.
var frameworkNames = []string{
	"subscribe", "unsubscribe", "publish", "emit", "broadcast", "listen",
	"register", "addsink", "addhandler", "handlers", "sinks", "topic",
	"channel", "bus", "hook", "middleware", "plugin", "dispatchto",
}

// TestExactlyOneDeliveryMechanism proves a Dispatcher holds exactly one
// Sink: not a list of them, not a map keyed by anything, and not a second
// fallback mechanism sitting alongside the first.
func TestExactlyOneDeliveryMechanism(t *testing.T) {
	dispatcherType := reflect.TypeOf(alert.Dispatcher{})
	sinkType := reflect.TypeOf((*alert.Sink)(nil)).Elem()

	sinkFields := 0
	for i := 0; i < dispatcherType.NumField(); i++ {
		f := dispatcherType.Field(i)
		switch f.Type.Kind() {
		case reflect.Slice, reflect.Array, reflect.Map, reflect.Chan:
			if f.Type.Elem().Implements(sinkType) || f.Type.Elem() == sinkType {
				t.Fatalf("Dispatcher.%s is a %s of Sink: v1 delivers through exactly one mechanism, never a fan-out", f.Name, f.Type.Kind())
			}
		default:
		}
		if f.Type == sinkType || (f.Type.Kind() != reflect.Interface && f.Type.Implements(sinkType)) {
			sinkFields++
		}
	}

	if sinkFields != 1 {
		t.Fatalf("Dispatcher holds %d Sink-typed fields, want exactly 1", sinkFields)
	}
}

// TestDispatcherHasNoPubSubSurface proves the dispatcher exposes no way
// to attach a second consumer after construction.
func TestDispatcherHasNoPubSubSurface(t *testing.T) {
	dispatcherType := reflect.TypeOf(&alert.Dispatcher{})
	for i := 0; i < dispatcherType.NumMethod(); i++ {
		name := dispatcherType.Method(i).Name
		for _, banned := range frameworkNames {
			if strings.Contains(strings.ToLower(name), banned) {
				t.Errorf("(*Dispatcher).%s looks like general pub/sub surface (%q); §71 forbids a broad notification framework in v1", name, banned)
			}
		}
	}
}

// TestPackageSurfaceIsNotAFramework parses this package's own non-test
// sources and fails on any exported declaration carrying framework
// vocabulary, which catches package-level functions and types reflection
// over Dispatcher alone would miss.
func TestPackageSurfaceIsNotAFramework(t *testing.T) {
	for path, file := range parseProductionSources(t, 0) {
		for _, decl := range file.Decls {
			for _, name := range exportedNames(decl) {
				for _, banned := range frameworkNames {
					if strings.Contains(strings.ToLower(name), banned) {
						t.Errorf("%s: exported declaration %q carries framework vocabulary (%q); §71 forbids a broad notification framework in v1", path, name, banned)
					}
				}
			}
		}
	}
}

// parseProductionSources parses every non-test Go file in this package's
// own directory. It walks the directory itself rather than calling
// go/parser.ParseDir, which is deprecated.
func parseProductionSources(t *testing.T, mode parser.Mode) map[string]*ast.File {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the alert package directory: %v", err)
	}

	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, filepath.Join(".", name), nil, mode)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		files[name] = parsed
	}

	if len(files) == 0 {
		t.Fatal("found no non-test Go files in the alert package: this gate would pass vacuously")
	}
	return files
}

func exportedNames(decl ast.Decl) []string {
	var out []string
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Name.IsExported() {
			out = append(out, d.Name.Name)
		}
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				if s.Name.IsExported() {
					out = append(out, s.Name.Name)
				}
			case *ast.ValueSpec:
				for _, n := range s.Names {
					if n.IsExported() {
						out = append(out, n.Name)
					}
				}
			}
		}
	}
	return out
}

// TestKindsAreExactlyTheFourWorkPackage35Names pins the alert vocabulary
// to §71's own list: stale backup, repeated failure, changed SSH host
// key, critical storage pressure. A fifth kind is how "one proactive
// mechanism for four conditions" quietly becomes the framework §71 rules
// out, so it takes an edit here to add one.
func TestKindsAreExactlyTheFourWorkPackage35Names(t *testing.T) {
	want := []alert.Kind{
		alert.StaleBackup,
		alert.RepeatedFailure,
		alert.HostKeyChanged,
		alert.CriticalStoragePressure,
	}

	if len(alert.Kinds) != len(want) {
		t.Fatalf("Kinds = %v, want exactly the four §71 conditions %v", alert.Kinds, want)
	}
	for i := range want {
		if alert.Kinds[i] != want[i] {
			t.Fatalf("Kinds = %v, want %v", alert.Kinds, want)
		}
	}
}

// TestAlertingNeverDeletes proves this package contains no deletion path
// at all: §71's own note that an alert "is a notification, never itself a
// trigger for deletion", and B3.1's retention-apply boundary, both depend
// on that staying true by construction rather than by review.
func TestAlertingNeverDeletes(t *testing.T) {
	banned := map[string]string{
		`"os"`: "the filesystem",
		`"github.com/spdrman/rclone-manager/core/internal/retention"`: "retention",
		`"github.com/spdrman/rclone-manager/core/internal/lifecycle"`: "the artifact lifecycle",
		`"github.com/spdrman/rclone-manager/core/internal/state"`:     "the journal",
	}

	for path, file := range parseProductionSources(t, parser.ImportsOnly) {
		for _, imp := range file.Imports {
			if what, forbidden := banned[imp.Path.Value]; forbidden {
				t.Errorf("%s imports %s: alerting must never be able to reach %s, only report on it", path, imp.Path.Value, what)
			}
		}
	}
}

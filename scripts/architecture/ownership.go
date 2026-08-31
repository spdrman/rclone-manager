// Command ownership is the layer-ownership check behind
// scripts/architecture/check-layer-ownership.sh (issue #165).
//
// # What it enforces
//
// EPIC B #81's standing constraint says an adapter "may change installation
// metadata, host paths, the authentication bridge, notifications, launch
// behavior and store presentation" but "must not fork backup behavior, API
// semantics, web application logic, retention logic, validation rules, or
// database truth", and a runtime profile "must never alter backup lifecycle
// semantics". #165 turns that into a check: lifecycle state, retention,
// validation, catalog and backup policy may be DECLARED only in the
// provider-neutral core layer.
//
// # Why a Go parser rather than grep
//
// The thing being detected is a declaration, and grep cannot tell one from
// a comment or a string. Every Go file in this repository is heavily
// commented, and those comments name retention, lifecycle and the catalog
// constantly, entirely legitimately. A regex over file text would either
// fire on all of them or be watered down until it fired on nothing. Parsing
// and walking only top-level declarations gives a rule that means what it
// says: comments and string literals are invisible to it for free.
//
// TypeScript has no parser here, so the .ts/.tsx side matches declaration
// keywords (`interface`, `type`, `class`, `function`, `const`) followed by a
// prohibited identifier, on lines that are not comments. That is weaker than
// the Go side and says so; the bridges it scans are small.
//
// # Positive controls
//
// Every rule here is a negative assertion, so every rule is mutation-tested
// by scripts/architecture/selftest.sh against the real tree: it plants a
// violating declaration in a real platform and a real distribution package
// and requires this command to fail and to name the rule. A rule that
// cannot be made to fire is not enforcing anything.
//
// Usage: ownership <repo-root> <path>...
// Exits 0 when clean, 1 on any violation, 2 on a usage or parse error.
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// rule is one prohibited ownership concept.
type rule struct {
	// ID is what a failure message names, so a violation reports the rule
	// it broke rather than only the symbol that broke it.
	ID string
	// Why is the one-line reason, printed with the violation.
	Why string
	// Match reports whether a declared identifier claims this concept.
	Match func(ident string) bool
}

// contains builds a case-insensitive substring matcher over a set of
// spellings. Substring rather than a word boundary deliberately: Go
// identifiers are camel-cased, so `\b` never fires between "Apply" and
// "Retention", and a word-boundary rule would silently pass
// ApplyRetentionPolicy while claiming to forbid it.
func contains(subs ...string) func(string) bool {
	lowered := make([]string, len(subs))
	for i, s := range subs {
		lowered[i] = strings.ToLower(s)
	}
	return func(ident string) bool {
		l := strings.ToLower(ident)
		for _, s := range lowered {
			if strings.Contains(l, s) {
				return true
			}
		}
		return false
	}
}

// rules is the prohibited set. Each entry names one of the five things
// #165's acceptance criteria say may live only in the provider-neutral
// core: lifecycle state, retention, validation rules, catalog truth and
// backup policy.
var rules = []rule{
	{
		ID:  "lifecycle-state",
		Why: "lifecycle state and its transitions belong to the provider-neutral core",
		Match: contains(
			"lifecyclestate", "lifecyclephase", "lifecycletransition",
			"artifactstate", "artifactstatus", "restorepoint",
		),
	},
	{
		ID:  "retention-policy",
		Why: "retention policy and its evaluation belong to the provider-neutral core",
		Match: contains(
			"retentionpolicy", "retentiontier", "retentionrule", "retentionwindow",
			"applyretention", "evaluateretention", "previewretention", "prunepolicy",
		),
	},
	{
		ID:  "validation-rules",
		Why: "validation rules and the validator catalog belong to the provider-neutral core",
		Match: contains(
			"validationrule", "validatorcatalog", "validatorregistry",
			"registervalidator", "validationpolicy", "validatorid",
			"validatorscript", "validationupdate",
		),
	},
	{
		ID:  "catalog-truth",
		Why: "the artifact catalog is database truth and belongs to the provider-neutral core",
		Match: contains(
			"artifactcatalog", "backupcatalog", "catalogentry", "catalogrecord",
			"catalogstore", "reconcilecatalog", "catalogrebuild", "rebuildcatalog",
			"catalogscript",
		),
	},
	{
		ID:  "backup-policy",
		Why: "backup policy and the backup-set model belong to the provider-neutral core",
		Match: contains(
			"backuppolicy", "backupschedule", "backupsetpolicy",
			"completionstrategy", "stalepolicy",
		),
	},
}

type violation struct {
	Path   string
	Line   int
	Rule   rule
	Symbol string
	Kind   string
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: ownership <repo-root> <path>...")
		os.Exit(2)
	}
	root := os.Args[1]
	targets := os.Args[2:]

	var found []violation
	scanned := 0

	for _, target := range targets {
		abs := filepath.Join(root, target)
		info, err := os.Stat(abs)
		if err != nil {
			// A manifest path that no longer exists is
			// check-layer-manifest.sh's failure to report, not this
			// command's. Skipping keeps one missing path from producing
			// two different failures with two different explanations.
			continue
		}
		walk := func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" || d.Name() == "dist" {
					return fs.SkipDir
				}
				return nil
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			switch filepath.Ext(path) {
			case ".go":
				scanned++
				vs, scanErr := scanGo(path, rel)
				if scanErr != nil {
					return scanErr
				}
				found = append(found, vs...)
			case ".ts", ".tsx":
				scanned++
				vs, scanErr := scanTS(path, rel)
				if scanErr != nil {
					return scanErr
				}
				found = append(found, vs...)
			}
			return nil
		}
		if info.IsDir() {
			if err := filepath.WalkDir(abs, walk); err != nil {
				fmt.Fprintf(os.Stderr, "ownership: %v\n", err)
				os.Exit(2)
			}
		} else {
			if err := walk(abs, dirEntryFor(info), nil); err != nil {
				fmt.Fprintf(os.Stderr, "ownership: %v\n", err)
				os.Exit(2)
			}
		}
	}

	if len(found) == 0 {
		fmt.Printf("OK: %d file(s) in the runtime-platform and distribution layers declare no core-owned concept.\n", scanned)
		return
	}

	sort.Slice(found, func(i, j int) bool {
		if found[i].Path != found[j].Path {
			return found[i].Path < found[j].Path
		}
		return found[i].Line < found[j].Line
	})
	fmt.Fprintln(os.Stderr, "FAIL: a runtime-platform or distribution file declares a concept the provider-neutral core owns exclusively:")
	for _, v := range found {
		fmt.Fprintf(os.Stderr, "  %s:%d: %s %s violates rule %q (%s)\n",
			v.Path, v.Line, v.Kind, v.Symbol, v.Rule.ID, v.Rule.Why)
	}
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  Layers and what each owns: docs/architecture/layers.md")
	os.Exit(1)
}

// dirEntryFor adapts an os.FileInfo for the single-file branch above.
func dirEntryFor(info os.FileInfo) fs.DirEntry { return fs.FileInfoToDirEntry(info) }

// scanGo reports prohibited top-level declarations. Only top-level:
// a local variable named retentionPolicy inside an adapter's own helper is
// not the adapter owning retention, and flagging it would make the rule
// noise a contributor learns to route around.
func scanGo(path, rel string) ([]violation, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", rel, err)
	}

	var out []violation
	record := func(pos token.Pos, kind, name string) {
		for _, r := range rules {
			if r.Match(name) {
				out = append(out, violation{
					Path:   rel,
					Line:   fset.Position(pos).Line,
					Rule:   r,
					Symbol: name,
					Kind:   kind,
				})
			}
		}
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind := "func"
			if d.Recv != nil {
				kind = "method"
			}
			record(d.Pos(), kind, d.Name.Name)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					record(s.Pos(), "type", s.Name.Name)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						record(n.Pos(), "value", n.Name)
					}
				}
			}
		}
	}
	return out, nil
}

// tsDecl matches an exported or plain TypeScript declaration keyword
// followed by the identifier it declares.
var tsDecl = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?(interface|type|class|function|const|let|var|enum)\s+([A-Za-z_$][\w$]*)`)

// scanTS is the TypeScript counterpart. Weaker than scanGo by construction
// (no parser here), so it skips obvious comment lines and only looks at
// declaration keywords rather than at every identifier.
func scanTS(path, rel string) ([]violation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []violation
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	inBlockComment := false
	for sc.Scan() {
		line++
		text := sc.Text()
		trimmed := strings.TrimSpace(text)
		if inBlockComment {
			if strings.Contains(trimmed, "*/") {
				inBlockComment = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.HasPrefix(trimmed, "/*") && !strings.Contains(trimmed, "*/") {
			inBlockComment = true
			continue
		}
		m := tsDecl.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		for _, r := range rules {
			if r.Match(m[2]) {
				out = append(out, violation{
					Path:   rel,
					Line:   line,
					Rule:   r,
					Symbol: m[2],
					Kind:   m[1],
				})
			}
		}
	}
	return out, sc.Err()
}

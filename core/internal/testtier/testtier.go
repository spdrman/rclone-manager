// Package testtier is the rule that decides which tier a test in core/
// belongs to, and the scan that enforces it (issue #447).
//
// # The rule
//
// A test belongs to the tier of the heaviest thing it needs, and the tier
// decides where the file lives:
//
//   - unit: nothing outside the process. Fakes, temp directories, a real
//     SQLite file, rclone's local backend, a subprocess of this
//     repository's own code. Lives in the package under test, under
//     core/internal, core/service or core/cmd.
//   - integration: several real packages composed, or a real subprocess
//     driven from outside, still with no container. Lives under
//     core/tests/<name> and imports no machine package.
//   - machine: a source machine, optionally a storage medium, on a
//     dedicated network, reached through core/tests/machines. Lives under
//     core/tests/<name> and is run by the gate under gotestwatch, never
//     under a fixed `go test` timeout.
//
// docs/architecture/test-tiers.md is the prose, including the list of what
// only a fake can prove, which is the coverage a restructure toward
// containers would otherwise delete without anybody noticing.
//
// # What the scan enforces
//
// Two rules, both about reaching a container from the wrong place:
//
//   - unit-reaches-container: a file under a unit directory imports a
//     machine package or execs `docker`. That is how six container-backed
//     tests came to live in unit packages, where `go test ./internal/...`
//     and CI_LOCAL_FAST=1 run them, and where they were the two suites
//     that went red under concurrent gate load for reasons that had
//     nothing to do with the code under test.
//   - bypasses-harness: a file under core/tests execs `docker` directly
//     instead of going through a harness package. Everything the harness
//     learned the hard way (bounded docker calls, the mid-test watchdog,
//     image presence before pull, the labelled sweep) only protects a test
//     that goes through it.
//
// And one derived fact: the machine packages, so the gate can be checked
// for running every one of them under gotestwatch (MissingFromGate).
//
// # Why a Go parser rather than grep
//
// The thing being detected is an import or a call, and grep cannot tell one
// from a comment. Every fixture in this repository is heavily commented and
// those comments say "docker" constantly, entirely legitimately. Walking
// the AST makes comments and unrelated strings invisible for free, which is
// the same reason scripts/architecture/ownership.go parses rather than
// greps.
//
// # What it does not see
//
// A docker binary reached through a variable (`bin := "docker";
// exec.Command(bin, ...)`) or through a wrapper in another package is
// invisible to this scan. Nothing in the tree does that today, and the
// ledger test in this package would notice a known violator going quiet,
// but a new one written that way would not be caught. That is the same
// limit ownership.go documents for itself, accepted for the same reason: a
// rule that means what it says on the ordinary shape beats one watered
// down until it fires on nothing.
package testtier

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ModulePath is core's own module path, which is what a machine import
// starts with.
const ModulePath = "github.com/spdrman/rclone-manager/core"

// The two rules the scan can report.
const (
	RuleUnitReachesContainer = "unit-reaches-container"
	RuleBypassesHarness      = "bypasses-harness"
)

// Tier is where a file may run. Only the three public ones are tiers a
// test author chooses between; the other two are how the scan classifies
// the harness itself and anything outside the known layout.
type Tier string

const (
	TierUnit        Tier = "unit"
	TierIntegration Tier = "integration"
	TierMachine     Tier = "machine"
	tierHarness     Tier = "harness"
)

// UnitDirs are the directories, relative to the core module root, whose
// files are unit tier and may not reach a container.
var UnitDirs = []string{"internal", "service", "cmd"}

// HarnessDirs are the packages that ARE the machine tier's mechanism. They
// are the only places under core/ allowed to exec docker, and importing any
// of them puts a test package in the machine tier. The three old fixtures
// stay listed until #450 folds them into machines.
var HarnessDirs = []string{
	"tests/machines",
	"tests/sftpfixture",
	"tests/miniofixture",
	"tests/dockerlease",
}

// MachineImports are the import paths of HarnessDirs.
func MachineImports() []string {
	out := make([]string, 0, len(HarnessDirs))
	for _, d := range HarnessDirs {
		out = append(out, ModulePath+"/"+d)
	}
	return out
}

// Finding is one place a file reaches a container from the wrong tier.
type Finding struct {
	// File is relative to the core module root, with forward slashes.
	File string
	Line int
	Rule string
	// Detail says what was found: the import path, or the exec call.
	Detail string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", f.File, f.Line, f.Rule, f.Detail)
}

// Report is what Scan produces.
type Report struct {
	Findings []Finding
	// MachinePackages are the directories under tests/, relative to the
	// core module root and sorted, whose files import a harness package and
	// which are not harness packages themselves. These are the packages the
	// gate has to run under gotestwatch.
	MachinePackages []string
}

// TierOf classifies a file path relative to the core module root.
func TierOf(rel string) Tier {
	rel = filepath.ToSlash(rel)
	for _, h := range HarnessDirs {
		if rel == h || strings.HasPrefix(rel, h+"/") {
			return tierHarness
		}
	}
	if strings.HasPrefix(rel, "tests/") {
		// Integration or machine is decided by imports, which TierOf does
		// not see; Scan refines it. Either way the rule for the file is the
		// same: no docker outside the harness.
		return TierIntegration
	}
	// Anything not under tests/ and not a harness is held to the unit rule,
	// including a .go file at the module root, should one ever appear.
	// Holding the unknown to the stricter rule is deliberate.
	return TierUnit
}

// Scan walks every .go file under coreRoot and applies the two rules.
func Scan(coreRoot string) (Report, error) {
	var rep Report
	machinePkgs := map[string]bool{}
	fset := token.NewFileSet()

	err := filepath.WalkDir(coreRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := d.Name()
		if d.IsDir() {
			if p == coreRoot {
				return nil
			}
			// testdata is Go's own convention for "not a package"; a dot
			// directory is a fixture's scratch (core/tests/.run).
			if name == "testdata" || strings.HasPrefix(name, ".") || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		rel, err := filepath.Rel(coreRoot, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		file, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", rel, err)
		}

		tier := TierOf(rel)
		imports, execSites := inspect(fset, file)

		switch tier {
		case tierHarness:
			// The mechanism. It is allowed to do what it does.
		case TierUnit:
			for _, imp := range imports {
				rep.Findings = append(rep.Findings, Finding{
					File: rel, Line: imp.line, Rule: RuleUnitReachesContainer,
					Detail: "imports " + imp.path,
				})
			}
			for _, site := range execSites {
				rep.Findings = append(rep.Findings, Finding{
					File: rel, Line: site.line, Rule: RuleUnitReachesContainer,
					Detail: "execs docker via " + site.call,
				})
			}
		default:
			if len(imports) > 0 {
				machinePkgs[path.Dir(rel)] = true
			}
			for _, site := range execSites {
				rep.Findings = append(rep.Findings, Finding{
					File: rel, Line: site.line, Rule: RuleBypassesHarness,
					Detail: "execs docker via " + site.call + " instead of going through core/tests/machines",
				})
			}
		}
		return nil
	})
	if err != nil {
		return Report{}, err
	}

	for dir := range machinePkgs {
		rep.MachinePackages = append(rep.MachinePackages, dir)
	}
	sort.Strings(rep.MachinePackages)
	sort.Slice(rep.Findings, func(i, j int) bool {
		a, b := rep.Findings[i], rep.Findings[j]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return rep, nil
}

type importSite struct {
	path string
	line int
}

type execSite struct {
	call string
	line int
}

// inspect reports the machine-package imports in file and every call that
// hands the literal "docker" to os/exec as the binary to run.
func inspect(fset *token.FileSet, file *ast.File) ([]importSite, []execSite) {
	machine := map[string]bool{}
	for _, p := range MachineImports() {
		machine[p] = true
	}

	var imports []importSite
	// The local names os/exec is known by in this file. Usually "exec";
	// an alias is honoured, and a dot import means the functions are
	// called unqualified.
	execNames := map[string]bool{}
	dotExec := false
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if machine[p] {
			imports = append(imports, importSite{path: p, line: fset.Position(imp.Pos()).Line})
		}
		if p == "os/exec" {
			switch {
			case imp.Name == nil:
				execNames["exec"] = true
			case imp.Name.Name == ".":
				dotExec = true
			case imp.Name.Name == "_":
			default:
				execNames[imp.Name.Name] = true
			}
		}
	}

	var sites []execSite
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, qualified := calleeName(call.Fun)
		if fn == "" {
			return true
		}
		if qualified != "" && !execNames[qualified] {
			return true
		}
		if qualified == "" && !dotExec {
			return true
		}
		binaryArg := -1
		switch fn {
		case "Command", "LookPath":
			binaryArg = 0
		case "CommandContext":
			binaryArg = 1
		default:
			return true
		}
		if len(call.Args) <= binaryArg {
			return true
		}
		lit, ok := call.Args[binaryArg].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if v, err := strconv.Unquote(lit.Value); err == nil && v == "docker" {
			label := fn
			if qualified != "" {
				label = qualified + "." + fn
			}
			sites = append(sites, execSite{call: label, line: fset.Position(call.Pos()).Line})
		}
		return true
	})
	return imports, sites
}

// calleeName splits `pkg.Func` into ("Func", "pkg") and a bare `Func` into
// ("Func", ""). Anything else is not a call this scan is about.
func calleeName(fun ast.Expr) (name, qualifier string) {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			return f.Sel.Name, x.Name
		}
	case *ast.Ident:
		return f.Name, ""
	}
	return "", ""
}

// MissingFromGate checks that scripts/ci-local.sh runs every machine
// package under gotestwatch and keeps it out of the plain `go test ./...`
// step. It returns one sentence per problem, and nothing when the gate is
// wired for every package.
//
// The gate names the packages twice, on purpose: once in the grep that
// excludes them from the fixed-timeout step, once on the gotestwatch line
// that runs them. A machine package missing from the first runs under `go
// test`'s fixed timeout, which is the shape #256 removed; one missing from
// the second does not run at all, which is the shape #160 is about. Both
// have to be checked, and a script whose shape has changed so that neither
// line can be found fails by name rather than passing on an empty search.
func MissingFromGate(script string, machinePackages []string) []string {
	var excludeLine, watchLine string
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "grep -vE '/tests/(") {
			excludeLine = line
		}
		if strings.Contains(line, "cmd/gotestwatch") && strings.Contains(line, "./tests/") {
			watchLine = line
		}
	}
	var problems []string
	if excludeLine == "" {
		problems = append(problems, "scripts/ci-local.sh has no `grep -vE '/tests/(...)'` exclusion line, so the machine packages cannot be told apart from the fixed-timeout `go test ./...` step")
	}
	if watchLine == "" {
		problems = append(problems, "scripts/ci-local.sh has no `go run ./cmd/gotestwatch ... ./tests/...` line, so no machine package is run under gotestwatch at all")
	}
	if len(problems) > 0 {
		return problems
	}

	group := ""
	if i := strings.Index(excludeLine, "/tests/("); i >= 0 {
		rest := excludeLine[i+len("/tests/("):]
		if j := strings.Index(rest, ")"); j >= 0 {
			group = rest[:j]
		}
	}
	excluded := map[string]bool{}
	for _, name := range strings.Split(group, "|") {
		excluded[strings.TrimSpace(name)] = true
	}

	for _, pkg := range machinePackages {
		name := strings.TrimPrefix(pkg, "tests/")
		if !excluded[name] {
			problems = append(problems, fmt.Sprintf("machine package %s is not in the gate's exclusion group `/tests/(%s)`, so it runs under the fixed-timeout `go test ./...` step", pkg, group))
		}
		if !strings.Contains(watchLine, "./"+pkg+"/...") {
			problems = append(problems, fmt.Sprintf("machine package %s is not on the gate's gotestwatch line, so the gate never runs it under a progress-derived bound", pkg))
		}
	}
	return problems
}

// FindCoreRoot walks up from dir until it finds core's own go.mod. It is
// what the ledger test uses to scan the real tree from wherever `go test`
// happens to be running.
func FindCoreRoot(dir string) (string, error) {
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(data)), "module "+ModulePath) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod declaring %s above %s", ModulePath, dir)
		}
		dir = parent
	}
}

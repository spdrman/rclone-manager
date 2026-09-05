package testtier

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// --- positive controls on synthetic trees --------------------------------
//
// Every rule here is a negative assertion, and a negative assertion that
// has never been seen to fail is indistinguishable from one that cannot
// fail. So each rule is driven against a planted violation in a throwaway
// tree first, and the finding has to name the file, the rule and the
// thing found. The real-tree test further down only means something once
// these pass.

func plant(t *testing.T, root, rel, src string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func scan(t *testing.T, root string) Report {
	t.Helper()
	rep, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return rep
}

func wantOne(t *testing.T, rep Report, file, rule, detail string) {
	t.Helper()
	if len(rep.Findings) != 1 {
		t.Fatalf("want exactly one finding, got %d: %v", len(rep.Findings), rep.Findings)
	}
	f := rep.Findings[0]
	if f.File != file || f.Rule != rule || !strings.Contains(f.Detail, detail) {
		t.Fatalf("finding = %s, want file %s, rule %s, detail containing %q", f, file, rule, detail)
	}
	if f.Line == 0 {
		t.Fatalf("finding %s carries no line number", f)
	}
}

func TestAUnitFileImportingAMachinePackageIsCaught(t *testing.T) {
	for _, pkg := range HarnessDirs {
		t.Run(pkg, func(t *testing.T) {
			root := t.TempDir()
			plant(t, root, "internal/x/x_test.go", "package x\n\nimport _ \""+ModulePath+"/"+pkg+"\"\n")
			wantOne(t, scan(t, root), "internal/x/x_test.go", RuleUnitReachesContainer, ModulePath+"/"+pkg)
		})
	}
}

func TestAUnitFileExecingDockerIsCaughtInEveryUnitDirectoryAndEveryCallShape(t *testing.T) {
	shapes := map[string]string{
		"Command":        `exec.Command("docker", "ps")`,
		"CommandContext": `exec.CommandContext(context.Background(), "docker", "info")`,
		"LookPath":       `exec.LookPath("docker")`,
	}
	for _, dir := range UnitDirs {
		for name, call := range shapes {
			t.Run(dir+"/"+name, func(t *testing.T) {
				root := t.TempDir()
				rel := dir + "/p/p_test.go"
				plant(t, root, rel, "package p\n\nimport (\n\t\"context\"\n\t\"os/exec\"\n)\n\nvar _ = context.Background\n\nfunc f() { _, _ = "+strings.TrimSuffix(strings.TrimPrefix(call, ""), "")+" }\n")
				rep := scan(t, root)
				if len(rep.Findings) == 0 {
					t.Fatalf("a planted %s in %s produced no finding", call, rel)
				}
				f := rep.Findings[0]
				if f.File != rel || f.Rule != RuleUnitReachesContainer || !strings.Contains(f.Detail, "exec."+name) {
					t.Fatalf("finding = %s, want %s / %s / exec.%s", f, rel, RuleUnitReachesContainer, name)
				}
			})
		}
	}
}

func TestAnAliasedOrDotImportedExecIsStillSeen(t *testing.T) {
	root := t.TempDir()
	plant(t, root, "internal/a/a_test.go", "package a\n\nimport osexec \"os/exec\"\n\nfunc f() { _ = osexec.Command(\"docker\") }\n")
	wantOne(t, scan(t, root), "internal/a/a_test.go", RuleUnitReachesContainer, "osexec.Command")

	root = t.TempDir()
	plant(t, root, "internal/b/b_test.go", "package b\n\nimport . \"os/exec\"\n\nfunc f() { _ = Command(\"docker\") }\n")
	wantOne(t, scan(t, root), "internal/b/b_test.go", RuleUnitReachesContainer, "Command")
}

func TestATestsFileExecingDockerBypassesTheHarness(t *testing.T) {
	root := t.TempDir()
	plant(t, root, "tests/foo/foo_test.go", "package foo\n\nimport \"os/exec\"\n\nfunc f() { _ = exec.Command(\"docker\", \"rm\", \"-f\", \"x\") }\n")
	wantOne(t, scan(t, root), "tests/foo/foo_test.go", RuleBypassesHarness, "core/tests/machines")
}

func TestTheHarnessItselfMayExecDocker(t *testing.T) {
	for _, dir := range HarnessDirs {
		t.Run(dir, func(t *testing.T) {
			root := t.TempDir()
			plant(t, root, dir+"/h.go", "package h\n\nimport \"os/exec\"\n\nfunc f() { _ = exec.Command(\"docker\", \"info\") }\n")
			if rep := scan(t, root); len(rep.Findings) != 0 {
				t.Fatalf("the harness package %s was reported for doing its job: %v", dir, rep.Findings)
			}
		})
	}
}

func TestATestsPackageImportingTheHarnessIsAMachinePackageAndNotAFinding(t *testing.T) {
	root := t.TempDir()
	plant(t, root, "tests/bar/bar_test.go", "package bar\n\nimport _ \""+ModulePath+"/tests/machines\"\n")
	plant(t, root, "tests/baz/baz_test.go", "package baz\n\nimport \"testing\"\n\nfunc TestX(*testing.T) {}\n")
	rep := scan(t, root)
	if len(rep.Findings) != 0 {
		t.Fatalf("a machine-tier package using the harness was reported: %v", rep.Findings)
	}
	if len(rep.MachinePackages) != 1 || rep.MachinePackages[0] != "tests/bar" {
		t.Fatalf("MachinePackages = %v, want [tests/bar]: baz imports nothing and is integration tier", rep.MachinePackages)
	}
}

// The controls against over-matching. A scan that fires on these is a
// scan nobody will keep.
func TestTheWordDockerInACommentOrAnUnrelatedStringIsInvisible(t *testing.T) {
	root := t.TempDir()
	plant(t, root, "internal/c/c_test.go", `package c

import (
	"fmt"
	"os/exec"
)

// This test talks about docker at length, and exec.Command("docker") in
// prose is not a call.
func f() {
	_ = exec.Command("ssh-keygen", "-t", "ed25519")
	_ = exec.Command(someBinary(), "docker")
	fmt.Println("docker", "docker info")
}

func someBinary() string { return "docker" }
`)
	if rep := scan(t, root); len(rep.Findings) != 0 {
		t.Fatalf("a comment, a non-exec string and a variable binary were reported: %v", rep.Findings)
	}
}

func TestTestdataAndDotDirectoriesAreNotScanned(t *testing.T) {
	root := t.TempDir()
	plant(t, root, "internal/d/testdata/fixture/f_test.go", "package f\n\nimport \"os/exec\"\n\nfunc f() { _ = exec.Command(\"docker\") }\n")
	plant(t, root, "tests/.run/leftover.go", "package leftover\n\nimport \"os/exec\"\n\nfunc f() { _ = exec.Command(\"docker\") }\n")
	if rep := scan(t, root); len(rep.Findings) != 0 {
		t.Fatalf("testdata or a dot directory was scanned: %v", rep.Findings)
	}
}

func TestAnUnparseableFileFailsTheScanRatherThanBeingSkipped(t *testing.T) {
	root := t.TempDir()
	plant(t, root, "internal/e/e_test.go", "package e\n\nfunc (\n")
	if _, err := Scan(root); err == nil {
		t.Fatal("a file the parser cannot read was silently skipped, which is how a violation hides in a syntax error")
	}
}

// --- the ledger's own controls ------------------------------------------

func TestDiffReportsBothAnUnlistedFindingAndAStaleEntry(t *testing.T) {
	rep := Report{Findings: []Finding{
		{File: "internal/new/new_test.go", Line: 3, Rule: RuleUnitReachesContainer, Detail: "execs docker"},
		{File: "internal/new/new_test.go", Line: 9, Rule: RuleUnitReachesContainer, Detail: "execs docker"},
		{File: "service/known_test.go", Line: 1, Rule: RuleUnitReachesContainer, Detail: "imports x"},
	}}
	ledger := []LedgerEntry{
		{File: "service/known_test.go", Rule: RuleUnitReachesContainer, Issue: 1},
		{File: "service/fixed_test.go", Rule: RuleUnitReachesContainer, Issue: 1},
	}
	unexpected, stale := Diff(rep, ledger)
	if len(unexpected) != 1 || unexpected[0].File != "internal/new/new_test.go" || unexpected[0].Line != 3 {
		t.Fatalf("unexpected = %v, want the one new file, reported once, at its first line", unexpected)
	}
	if len(stale) != 1 || stale[0].File != "service/fixed_test.go" {
		t.Fatalf("stale = %v, want the one entry no finding backs", stale)
	}
}

// --- the gate wiring's controls ------------------------------------------

const gateShape = `
  (cd core && GOWORK=off go test $(GOWORK=off go list ./... | grep -vE '/tests/(alpha|beta)$'))
  (cd core && GOWORK=off go run ./cmd/gotestwatch -count=1 ./tests/alpha/... ./tests/beta/...)
`

func TestMissingFromGateIsQuietWhenEveryPackageIsWiredTwice(t *testing.T) {
	if got := MissingFromGate(gateShape, []string{"tests/alpha", "tests/beta"}); len(got) != 0 {
		t.Fatalf("a fully wired gate was reported: %v", got)
	}
}

func TestMissingFromGateNamesAPackageMissingFromEitherLine(t *testing.T) {
	got := MissingFromGate(gateShape, []string{"tests/alpha", "tests/gamma"})
	if len(got) != 2 {
		t.Fatalf("want two problems for gamma (exclusion and gotestwatch), got %d: %v", len(got), got)
	}
	for _, p := range got {
		if !strings.Contains(p, "tests/gamma") {
			t.Fatalf("problem does not name the package: %q", p)
		}
	}
	if !strings.Contains(got[0], "exclusion group") || !strings.Contains(got[1], "gotestwatch") {
		t.Fatalf("problems do not say which line is missing the package: %v", got)
	}

	onlyExcluded := strings.Replace(gateShape, " ./tests/beta/...", "", 1)
	got = MissingFromGate(onlyExcluded, []string{"tests/beta"})
	if len(got) != 1 || !strings.Contains(got[0], "gotestwatch") {
		t.Fatalf("a package excluded from go test but never run under gotestwatch is the #160 shape, and it was not named: %v", got)
	}
}

func TestMissingFromGateRefusesAScriptWhoseShapeItCannotFind(t *testing.T) {
	got := MissingFromGate("echo nothing here\n", []string{"tests/alpha"})
	if len(got) != 2 {
		t.Fatalf("a script with neither line must produce two problems, got %v", got)
	}
}

// --- the real tree ------------------------------------------------------

func coreRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := FindCoreRoot(filepath.Dir(here))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestTheTreeMatchesTheLedgerExactly is the guard. A new container-backed
// test in a unit package, or a new direct docker call under core/tests,
// fails here and is told where to go. A ledgered file that was fixed
// without the ledger being updated fails here too, because a ledger that
// can go stale silently would eventually describe nothing.
func TestTheTreeMatchesTheLedgerExactly(t *testing.T) {
	rep := scan(t, coreRoot(t))
	unexpected, stale := Diff(rep, Ledger)

	for _, f := range unexpected {
		switch f.Rule {
		case RuleUnitReachesContainer:
			t.Errorf("%s\n    %s is a unit-tier file (core/internal, core/service, core/cmd) and it reaches a container.\n"+
				"    Unit tests may not need Docker: move the test to a package under core/tests/ and reach the machine\n"+
				"    through core/tests/machines. docs/architecture/test-tiers.md says which tier a test belongs to.", f, f.File)
		case RuleBypassesHarness:
			t.Errorf("%s\n    %s is under core/tests and execs docker itself. Go through core/tests/machines instead, so the\n"+
				"    watchdog, the labelled sweep and the bounded docker calls apply to this test too. If the harness\n"+
				"    cannot do what this test needs, add the capability to the harness rather than the docker call to the test.", f, f.File)
		default:
			t.Errorf("%s", f)
		}
	}
	for _, e := range stale {
		t.Errorf("ledger entry %s (%s, #%d) is stale: the scan no longer finds it. Remove it from the ledger in the same change that fixed it.", e.File, e.Rule, e.Issue)
	}
	t.Logf("machine packages: %v", rep.MachinePackages)
}

// TestEveryMachinePackageRunsUnderGotestwatch reads the gate itself, so a
// new machine-tier package cannot be run under go test's fixed timeout, or
// not at all, without this test saying so.
func TestEveryMachinePackageRunsUnderGotestwatch(t *testing.T) {
	root := coreRoot(t)
	script := filepath.Join(root, "..", "scripts", "ci-local.sh")
	data, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("this test reads the gate to check its wiring and cannot: %v. A missing gate is a refusal, not a skip.", err)
	}
	rep := scan(t, root)
	if len(rep.MachinePackages) == 0 {
		t.Fatal("the scan found no machine packages at all, so this check would pass vacuously; the tree has at least crashmatrix, sftpintegration and miniointegration")
	}
	for _, p := range MissingFromGate(string(data), rep.MachinePackages) {
		t.Error(p)
	}
}

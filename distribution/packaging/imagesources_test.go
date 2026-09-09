package packaging

import (
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// Every first-party package a shipped binary links is copied into the
// build stage that compiles it.
//
// container/Dockerfile copies core/ into the image as an explicit list
// of directories rather than as a whole, on purpose: it keeps scratch
// files out of the build context and keeps an edit to one package from
// invalidating the others' layers. The cost is that a new top-level
// package under core/ is invisible to the image until somebody adds a
// COPY for it, and nothing in the Go suite says so, because `go test`
// from a checkout has the whole module. The Dockerfile's own comment on
// apicontract records the first time that happened (#543), and
// core/cliecho was the second (#599): the gate's Docker step failed with
// "cannot find module providing package .../core/cliecho" on the first
// run after the package was added, hours after every unit suite had
// passed.
//
// So this asks the binaries' own import graphs, which is the same
// question the image build asks, and asks it here where nobody needs a
// Docker daemon to hear the answer.

// imageStageFor names the Dockerfile stage that compiles each shipped
// binary. The core-only stage flattens core/ to /src, so it copies
// `core/x/` to `./x/`; the web stage keeps the repository's layout. Both
// are read by their source path, which is the half that has to match.
var imageStageFor = map[string]string{
	"rbm":     "build",
	"rbm-web": "build-web",
}

// copiedIntoStages reads every `COPY <dir>/ ...` in a Dockerfile, keyed
// by the stage it appears in. Only build-context sources count: a
// `COPY --from=` is another stage's output, not a source directory.
//
// It returns the stages it found as well, so a caller can refuse a
// recipe whose stage was renamed rather than one that copies nothing:
// a matcher that quietly matches nothing is the failure mode this package
// keeps meeting.
func copiedIntoStages(dockerfile string) (dirsByStage map[string]map[string]bool, stages []string) {
	dirsByStage = map[string]map[string]bool{}
	stage := ""
	for _, raw := range strings.Split(dockerfile, "\n") {
		line := strings.TrimSpace(raw)
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "FROM":
			stage = ""
			for i := 1; i+1 < len(fields); i++ {
				if strings.EqualFold(fields[i], "AS") {
					stage = fields[i+1]
				}
			}
			if stage != "" {
				stages = append(stages, stage)
				if dirsByStage[stage] == nil {
					dirsByStage[stage] = map[string]bool{}
				}
			}
		case "COPY":
			if stage == "" || len(fields) < 3 || strings.HasPrefix(fields[1], "--from=") {
				continue
			}
			// Every field but the last is a source.
			for _, src := range fields[1 : len(fields)-1] {
				if strings.HasSuffix(src, "/") {
					dirsByStage[stage][strings.TrimSuffix(src, "/")] = true
				}
			}
		}
	}
	return dirsByStage, stages
}

// firstPartySourceDirs is every directory under this repository, at the
// granularity the Dockerfile copies (core/service, apps/common), that one
// shipped binary's import graph reaches. It is asked for the platform
// the image is built for, for the reason GoLinkedModules gives: the host
// is a Mac and the product is Linux.
func firstPartySourceDirs(t *testing.T, target GoBuildTarget) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", target.Package)
	cmd.Dir = Path(target.ModuleDir)
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps for %s: %v: %s", target.Binary, err, strings.TrimSpace(stderr.String()))
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		rel, ok := strings.CutPrefix(strings.TrimSpace(line), firstPartyModulePrefix)
		if !ok {
			continue
		}
		parts := strings.SplitN(rel, "/", 3)
		if len(parts) < 2 {
			// A package at a module's own root has no directory a COPY
			// could name short of the whole module. Reporting it is
			// right: it needs a decision, not a silent pass.
			seen[rel] = true
			continue
		}
		seen[parts[0]+"/"+parts[1]] = true
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

// missingFromImage is the check, separated from the exec so the control
// below can drive it against a recipe that is wrong in one specific way.
func missingFromImage(dockerfile string, target GoBuildTarget, dirs []string) (missing []string, err string) {
	stage, known := imageStageFor[target.Binary]
	if !known {
		return nil, "no image stage is recorded for " + target.Binary + "; add it to imageStageFor"
	}
	byStage, stages := copiedIntoStages(dockerfile)
	if byStage[stage] == nil {
		return nil, "container/Dockerfile has no stage named " + stage + " (it has " + strings.Join(stages, ", ") + "); either the stage was renamed or this reader stopped seeing FROM ... AS"
	}
	for _, dir := range dirs {
		if !byStage[stage][dir] {
			missing = append(missing, dir)
		}
	}
	return missing, ""
}

func TestEveryFirstPartyPackageTheBinariesImportIsCopiedIntoTheImage(t *testing.T) {
	dockerfile := realDockerfile(t)
	checked := 0
	for _, target := range ShippedGoBinaries {
		dirs := firstPartySourceDirs(t, target)
		if len(dirs) == 0 {
			t.Fatalf("%s reaches no first-party package at all, so this test would pass having checked nothing", target.Binary)
		}
		checked += len(dirs)
		missing, problem := missingFromImage(dockerfile, target, dirs)
		if problem != "" {
			t.Fatal(problem)
		}
		for _, dir := range missing {
			t.Errorf("%s links %s/ and the %q stage of container/Dockerfile never copies it, so the image build fails with \"cannot find module providing package\" the first time a Docker daemon sees this tree. Add `COPY %s/` to that stage.",
				target.Binary, dir, imageStageFor[target.Binary], dir)
		}
	}
	if checked == 0 {
		t.Fatal("no directories were checked")
	}
}

// The control. Every assertion above is that a directory is NOT missing,
// and an assertion like that is worth exactly what the proof it can fire
// at all is worth: this drives the same check against the real recipe
// with the one line removed that the gate found missing, and requires it
// to say so.
func TestEveryFirstPartyPackageTheBinariesImportIsCopiedIntoTheImage_RefusesARecipeMissingOne(t *testing.T) {
	real := realDockerfile(t)
	const needle = "COPY core/cliecho/ ./cliecho/\n"
	if strings.Count(real, needle) != 1 {
		t.Fatalf("container/Dockerfile does not carry %q exactly once, so the mutation below edits nothing", strings.TrimSpace(needle))
	}
	broken := strings.Replace(real, needle, "", 1)

	var core GoBuildTarget
	for _, target := range ShippedGoBinaries {
		if target.Binary == "rbm" {
			core = target
		}
	}
	if core.Binary == "" {
		t.Fatal("ShippedGoBinaries no longer lists rbm")
	}
	dirs := firstPartySourceDirs(t, core)
	missing, problem := missingFromImage(broken, core, dirs)
	if problem != "" {
		t.Fatal(problem)
	}
	if len(missing) != 1 || missing[0] != "core/cliecho" {
		t.Fatalf("with the cliecho COPY removed the check reports %v missing, want exactly [core/cliecho]; the test above could not have caught the gate failure this file exists for", missing)
	}

	// And a renamed stage is refused rather than matched against
	// nothing.
	if _, problem := missingFromImage(strings.Replace(real, " AS build\n", " AS builder\n", 1), core, dirs); problem == "" {
		t.Fatal("a recipe whose build stage was renamed passed the check, so the stage matcher matched nothing and said so to nobody")
	}
}

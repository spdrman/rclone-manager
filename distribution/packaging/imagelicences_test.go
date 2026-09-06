// The suite behind the licence obligations the shipped image itself has
// to discharge: the materials are inside it, the build context can still
// reach every source it copies, the runtime stage labels what it carries,
// and every distribution target says how a recipient gets the licence.
//
// All of it is read out of container/Dockerfile and .dockerignore rather
// than out of a built image, which is the compromise this file is built
// around. Nothing in a unit suite can pull an image, so what is provable
// here is that the recipe says the right thing; the check that the recipe
// was followed is a separate, recorded artifact, and one test below
// insists that artifact exists rather than letting the recipe stand in
// for it.
//
// Reading a Dockerfile as text invites the failure mode this package
// keeps meeting: a matcher that quietly matches nothing passes. So every
// reader here has a test whose whole job is to watch it refuse, driving
// it against a recipe that is wrong in one specific way, and the
// .dockerignore reader is held to Docker's own precedence rules rather
// than to a simplification that happens to agree on today's file.
package packaging

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// realDockerfile reads container/Dockerfile, or fails the test.
func realDockerfile(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(Path(filepath.Join("container", "Dockerfile")))
	if err != nil {
		t.Fatalf("cannot read container/Dockerfile: %v", err)
	}
	return string(data)
}

// realDockerignore reads .dockerignore, or fails the test.
func realDockerignore(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(Path(".dockerignore"))
	if err != nil {
		t.Fatalf("cannot read .dockerignore: %v", err)
	}
	return string(data)
}

// TestTheImageCarriesTheLicenceMaterials is #407's first acceptance
// criterion, as far as it can be asked of the tree.
//
// NOTICE is where Apache-2.0 §4(d)'s attribution and MPL-2.0 §3.2's
// source offer both live, and TestNoticeAttributesEveryComponent proves
// it says the right things. Nothing proved it went anywhere. This reads
// the runtime stage, the way the bundle tests do, and refuses an image
// that ships the binaries and not the files that say what they are
// under.
//
// It reads the Dockerfile and therefore proves a Dockerfile. The built
// image is apps/generic/tests/dockercli's
// TestTheBuiltImageCarriesTheLicenceMaterials, which needs a daemon this
// suite may not assume; the last check below is what keeps that test
// from being quietly deleted.
func TestTheImageCarriesTheLicenceMaterials(t *testing.T) {
	c := MustLoadCompliance()
	dockerfile := realDockerfile(t)

	materials := LicenceMaterials(c)
	if len(materials) != 3 {
		t.Fatalf("compliance.json declares %v as the licence materials; the licence, the notices and the inventory are all three of them", materials)
	}

	where, complaints := ImageLicenceMaterials(c, dockerfile)
	for _, complaint := range complaints {
		t.Error(complaint)
	}
	if len(complaints) > 0 {
		t.FailNow()
	}
	for _, rel := range materials {
		dest, ok := where[rel]
		if !ok {
			t.Fatalf("no complaint and no in-image path for %s, so the reader decided nothing", rel)
		}
		if !strings.HasPrefix(dest, "/") || strings.HasSuffix(dest, "/") {
			t.Errorf("%s lands at %q, which is not a file path a recipient can name", rel, dest)
		}
	}

	// The written offer has to say where a recipient looks. An image
	// that carries the file at a path nobody documents is findable only
	// by listing the whole filesystem, which distroless gives no shell
	// to do.
	link, ok := c.Link("source-offer")
	if !ok || link.RepoPath == "" {
		t.Fatal("compliance.json declares no source-offer link with a repository path")
	}
	offer, err := os.ReadFile(Path(link.RepoPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v", link.RepoPath, err)
	}
	for rel, dest := range where {
		if !strings.Contains(string(offer), dest) {
			t.Errorf("%s never says the image carries %s at %s, so a recipient is not told where to look", link.RepoPath, rel, dest)
		}
	}

	// The reader is reading the right stage. If it were reading the
	// whole file, a COPY of NOTICE into a builder stage would pass.
	copies, err := RuntimeStageCopies(dockerfile)
	if err != nil {
		t.Fatalf("RuntimeStageCopies: %v", err)
	}
	sawBinary := false
	for _, cp := range copies {
		if cp.From == "build" && cp.Dest == "/backup-manager" {
			sawBinary = true
		}
		if cp.From == "" && strings.HasPrefix(cp.Sources[0], "core/") {
			t.Errorf("line %d COPYs %v, which is a builder stage's copy; the reader is not confined to the runtime stage", cp.Line, cp.Sources)
		}
	}
	if !sawBinary {
		t.Error("the runtime stage read here does not copy /backup-manager from the build stage, so this is not the stage that becomes the image")
	}
}

// TestTheBuiltImageProofIsInTheTree is the one thing this suite can say
// about the delivered form without a daemon: that the test which does
// open a built image still exists and still reads the same declaration.
//
// Everything above believes container/Dockerfile. That is worth having
// and it is not what #407 asked for, and the gap is filled by a test in
// another module that this one cannot invoke or import. So it checks the
// file, which is enough to catch the realistic failure: the built-image
// test deleted or renamed in a tidy-up, leaving the static half looking
// like full coverage.
func TestTheBuiltImageProofIsInTheTree(t *testing.T) {
	const proof = "apps/generic/tests/dockercli/imagelicences_test.go"
	data, err := os.ReadFile(Path(proof))
	if err != nil {
		t.Fatalf("cannot read %s, which is the only test anywhere that opens a built image and looks for the licence materials in it: %v", proof, err)
	}
	body := string(data)
	for _, want := range []string{
		"func TestTheBuiltImageCarriesTheLicenceMaterials(",
		// It reads the declaration rather than a list of its own, so
		// moving a path in compliance.json moves what it looks for.
		"licenceDelivery",
		"imagePaths",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s no longer contains %q; the built-image proof #407's first criterion asks for is gone or has stopped reading compliance.json, and the Dockerfile checks in this file would not notice", proof, want)
		}
	}
}

// TestTheImageLicenceReaderCanRefuse is the positive control for the
// test above. Each row is the real Dockerfile with one thing wrong,
// because a reader that has only ever been watched passing has not been
// watched.
func TestTheImageLicenceReaderCanRefuse(t *testing.T) {
	c := MustLoadCompliance()
	real := realDockerfile(t)
	const licenceLine = "COPY LICENSE /licenses/LICENSE\n"
	const noticeLine = "COPY NOTICE /licenses/NOTICE\n"
	const inventoryLine = "COPY provenance/third-party-licenses.json /licenses/third-party-licenses.json\n"
	for _, needle := range []string{licenceLine, noticeLine, inventoryLine} {
		if strings.Count(real, needle) != 1 {
			t.Fatalf("container/Dockerfile does not carry %q exactly once, so the mutations below edit nothing", strings.TrimSpace(needle))
		}
	}
	lastFrom := strings.LastIndex(real, "\nFROM ")
	if lastFrom < 0 {
		t.Fatal("container/Dockerfile has no runtime FROM to move lines above")
	}
	withoutCopies := strings.NewReplacer(licenceLine, "", noticeLine, "", inventoryLine, "").Replace(real)

	cases := []struct {
		name       string
		dockerfile string
		want       []string
	}{
		{
			"the licence COPY is deleted",
			strings.Replace(real, licenceLine, "", 1),
			[]string{"never COPYs LICENSE"},
		},
		{
			"the notice COPY is deleted",
			strings.Replace(real, noticeLine, "", 1),
			[]string{"never COPYs NOTICE"},
		},
		{
			"the inventory COPY is deleted",
			strings.Replace(real, inventoryLine, "", 1),
			[]string{"never COPYs provenance/third-party-licenses.json"},
		},
		{
			"all three COPYs are deleted",
			withoutCopies,
			[]string{"never COPYs LICENSE", "never COPYs NOTICE", "never COPYs provenance/third-party-licenses.json"},
		},
		{
			// The lines exist, above the last FROM, so they land in a
			// builder stage that is thrown away.
			"all three COPYs are in a builder stage",
			withoutCopies[:lastFrom] + "\n" + licenceLine + noticeLine + inventoryLine + withoutCopies[lastFrom:],
			[]string{"never COPYs LICENSE", "never COPYs NOTICE", "never COPYs provenance/third-party-licenses.json"},
		},
		{
			"the notice is copied out of a builder stage",
			strings.Replace(real, noticeLine, "COPY --from=build /src/NOTICE /licenses/NOTICE\n", 1),
			[]string{"from stage \"build\""},
		},
		{
			"the notice lands at a relative path",
			strings.Replace(real, noticeLine, "COPY NOTICE licenses/NOTICE\n", 1),
			[]string{"which is relative"},
		},
		{
			"the notice COPY is written in exec form",
			strings.Replace(real, noticeLine, "COPY [\"NOTICE\", \"/licenses/NOTICE\"]\n", 1),
			[]string{"exec form"},
		},
		{
			"a file with no FROM at all",
			licenceLine + noticeLine + inventoryLine,
			[]string{"no FROM"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			where, got := ImageLicenceMaterials(c, tc.dockerfile)
			if len(got) != len(tc.want) {
				t.Fatalf("expected %d complaint(s) %v, got %d: %v", len(tc.want), tc.want, len(got), got)
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("complaint %d = %q, want it to contain %q", i, got[i], w)
				}
			}
			for rel := range where {
				for _, w := range tc.want {
					if strings.Contains(w, rel) {
						t.Errorf("%s is both complained about and given an in-image path %q", rel, where[rel])
					}
				}
			}
		})
	}

	// And a directory destination resolves to the file inside it, so an
	// author who writes `COPY NOTICE /licenses/` is not told the file is
	// at "/licenses/".
	dir := strings.Replace(real, noticeLine, "COPY NOTICE /licenses/\n", 1)
	where, got := ImageLicenceMaterials(c, dir)
	if len(got) != 0 {
		t.Fatalf("a directory destination is refused: %v", got)
	}
	if where[c.License.NoticeFile] != "/licenses/NOTICE" {
		t.Errorf("COPY NOTICE /licenses/ resolves to %q, want /licenses/NOTICE", where[c.License.NoticeFile])
	}

	// Continuations are joined, or a wrapped COPY reads as two broken
	// instructions.
	wrapped := strings.Replace(real, noticeLine, "COPY \\\n    NOTICE \\\n    /licenses/NOTICE\n", 1)
	if _, got := ImageLicenceMaterials(c, wrapped); len(got) != 0 {
		t.Errorf("a COPY wrapped over three lines is refused: %v", got)
	}
}

// ---------------------------------------------------------------------
// The build context
// ---------------------------------------------------------------------

// TestTheBuildContextKeepsEveryCopiedSource is the guard on
// .dockerignore, and it is the reason the inventory could not simply be
// added to the runtime stage.
//
// provenance/ is excluded wholesale and the exception for one file in it
// is a line somebody could reasonably delete while tidying. Doing that
// breaks the release build rather than shipping a bad image, which is
// the right failure and the wrong place to find out: it happens on a
// machine with a daemon, at release time. This finds it in the packaging
// suite, on any machine, and names the pattern that did it.
func TestTheBuildContextKeepsEveryCopiedSource(t *testing.T) {
	dockerfile := realDockerfile(t)
	ignore := realDockerignore(t)

	for _, complaint := range DockerignoreComplaints(ignore, dockerfile) {
		t.Error(complaint)
	}

	// The control. Without the exception, provenance/ hides the
	// inventory and this has to say so; a check that has only been
	// watched passing over a file it happens to agree with is not a
	// check.
	blind := strings.Replace(ignore, "\n!provenance/third-party-licenses.json", "", 1)
	if blind == ignore {
		t.Fatal(".dockerignore has no !provenance/third-party-licenses.json line, so the control below removes nothing and this test would pass over any ignore file at all")
	}
	got := DockerignoreComplaints(blind, dockerfile)
	if len(got) != 1 {
		t.Fatalf("with the exception removed, expected exactly one complaint, got %d: %v", len(got), got)
	}
	for _, want := range []string{"provenance/third-party-licenses.json", "\"provenance\""} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the complaint %q does not contain %q, so it does not say what to put back", got[0], want)
		}
	}
}

// TestTheDockerignoreReaderFollowsDockersOwnRules pins the matcher to
// the semantics the daemon actually uses, since every row above is
// decided by it.
//
// The last matching pattern wins and a "!" re-includes: that pair is the
// whole mechanism the exception rests on, and getting it backwards would
// make this file's checks agree with themselves and disagree with the
// build. The rows are the ones the real .dockerignore exercises.
func TestTheDockerignoreReaderFollowsDockersOwnRules(t *testing.T) {
	const ignore = `
# a comment
.git
docs
provenance
!provenance/third-party-licenses.json
**/node_modules
ui/shared/dist
*.md
core/tests
`
	cases := []struct {
		path string
		want bool
	}{
		{"LICENSE", false},
		{"NOTICE", false},
		{".git", true},
		{".git/config", true},        // a parent directory decides
		{"docs/deployment.md", true}, // and so does a parent plus a pattern
		{"provenance/checksums.txt", true},
		{"provenance/third-party-licenses.json", false}, // the exception
		{"ui/shared/package.json", false},
		{"ui/shared/dist", true},
		{"ui/shared/node_modules/x", true}, // ** spans components
		{"node_modules", true},             // and matches none of them too
		{"README.md", true},
		{"core/tests/fixture.go", true},
		{"core/internal/x.go", false},
	}
	for _, tc := range cases {
		got, pattern := DockerignoreExcludes(ignore, tc.path)
		if got != tc.want {
			t.Errorf("DockerignoreExcludes(%q) = %v (by %q), want %v", tc.path, got, pattern, tc.want)
		}
	}

	// Order is what decides, not which kind of line came last in the
	// file: an exception followed by a re-exclusion excludes again.
	if got, _ := DockerignoreExcludes("provenance\n!provenance/x.json\nprovenance/x.json\n", "provenance/x.json"); !got {
		t.Error("a pattern after an exception did not win; the last match has to decide, or the reader is not reading dockerignore")
	}
}

// ---------------------------------------------------------------------
// Per-target delivery
// ---------------------------------------------------------------------

// TestEveryDistributionTargetSaysHowItDeliversTheLicence is #407's
// second acceptance criterion: a target that ships none of the licence
// materials fails this suite rather than inheriting an answer from a
// note about something else.
func TestEveryDistributionTargetSaysHowItDeliversTheLicence(t *testing.T) {
	c := MustLoadCompliance()
	where, complaints := ImageLicenceMaterials(c, realDockerfile(t))
	if len(complaints) > 0 {
		t.Fatalf("the runtime stage is not readable, so nothing below decides anything: %v", complaints)
	}
	for _, complaint := range LicenceDeliveryComplaints(c, where) {
		t.Error(complaint)
	}

	// Every target answered, and the answers are the two vehicles rather
	// than a third somebody added without a rule for it.
	for _, id := range c.TargetIDs() {
		switch v := c.Distribution.Targets[id].LicenceDelivery; v {
		case LicenceVehicleImage, LicenceVehiclePackagePayload:
		default:
			t.Errorf("target %q records licenceDelivery %q", id, v)
		}
	}

	// And the decision the issue asked to be written down rather than
	// inferred is actually written down: the .spk is the one target the
	// image does not reach, so a tree where every target says "image" is
	// a tree where nobody made the call.
	var payload []string
	for _, id := range c.TargetIDs() {
		if c.Distribution.Targets[id].LicenceDelivery == LicenceVehiclePackagePayload {
			payload = append(payload, id)
		}
	}
	if want := []string{"synology"}; strings.Join(payload, ",") != strings.Join(want, ",") {
		t.Errorf("targets carrying the licence materials in their own package are %v, want %v; the .spk installs binaries and never pulls the image, so it cannot inherit the image's answer, and nothing else here is in that position", payload, want)
	}
}

// TestTheLicenceDeliveryRuleCanRefuse is the positive control for the
// rule above: each row is the real declaration with one thing wrong.
func TestTheLicenceDeliveryRuleCanRefuse(t *testing.T) {
	c := MustLoadCompliance()
	where, complaints := ImageLicenceMaterials(c, realDockerfile(t))
	if len(complaints) > 0 {
		t.Fatalf("the runtime stage is not readable: %v", complaints)
	}
	if got := LicenceDeliveryComplaints(c, where); len(got) != 0 {
		t.Fatalf("the tree as it stands complains, so every row below would pass for the wrong reason: %v", got)
	}

	// A helper that copies the declaration so a row cannot leak into
	// the next one through the shared maps.
	fork := func() Compliance {
		out := c
		out.Distribution.Targets = map[string]DistributionTarget{}
		for id, tgt := range c.Distribution.Targets {
			out.Distribution.Targets[id] = tgt
		}
		out.Distribution.LicenceDelivery.ImagePaths = map[string]string{}
		for k, v := range c.Distribution.LicenceDelivery.ImagePaths {
			out.Distribution.LicenceDelivery.ImagePaths[k] = v
		}
		return out
	}

	cases := []struct {
		name   string
		break_ func(*Compliance, map[string]string)
		want   string
	}{
		{
			"a target says nothing at all",
			func(m *Compliance, _ map[string]string) {
				tgt := m.Distribution.Targets["casaos"]
				tgt.LicenceDelivery = ""
				m.Distribution.Targets["casaos"] = tgt
			},
			`target "casaos" records no licenceDelivery`,
		},
		{
			"a target names a vehicle nobody has a rule for",
			func(m *Compliance, _ map[string]string) {
				tgt := m.Distribution.Targets["zimaos"]
				tgt.LicenceDelivery = "the store does it"
				m.Distribution.Targets["zimaos"] = tgt
			},
			"which is neither",
		},
		{
			"a target relies on an image that carries nothing",
			func(_ *Compliance, in map[string]string) {
				delete(in, "NOTICE")
			},
			"and container/Dockerfile's runtime stage does not put it in the image",
		},
		{
			"the .spk's reason stops mentioning the inventory",
			func(m *Compliance, _ map[string]string) {
				tgt := m.Distribution.Targets["synology"]
				tgt.UnbuiltReason = strings.ReplaceAll(tgt.UnbuiltReason, "provenance/third-party-licenses.json", "the inventory")
				m.Distribution.Targets["synology"] = tgt
			},
			"never mentions provenance/third-party-licenses.json",
		},
		{
			"the .spk carries them itself, builds nothing and says why nothing",
			func(m *Compliance, _ map[string]string) {
				tgt := m.Distribution.Targets["synology"]
				tgt.UnbuiltReason = ""
				m.Distribution.Targets["synology"] = tgt
			},
			"there is nothing at all that says a recipient of it ever gets them",
		},
		{
			"the declared in-image path is not where the Dockerfile puts it",
			func(m *Compliance, _ map[string]string) {
				m.Distribution.LicenceDelivery.ImagePaths["NOTICE"] = "/NOTICE"
			},
			"a recipient is told to look where the file is not",
		},
		{
			"the declaration forgets a file the image carries",
			func(m *Compliance, _ map[string]string) {
				delete(m.Distribution.LicenceDelivery.ImagePaths, "NOTICE")
			},
			"does not declare it, so the built-image test never looks for it",
		},
		{
			"the declaration is empty",
			func(m *Compliance, _ map[string]string) {
				m.Distribution.LicenceDelivery.ImagePaths = nil
			},
			"declares no imagePaths",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := fork()
			in := map[string]string{}
			for k, v := range where {
				in[k] = v
			}
			tc.break_(&m, in)
			got := LicenceDeliveryComplaints(m, in)
			if len(got) == 0 {
				t.Fatalf("no complaint; %q was expected", tc.want)
			}
			found := false
			for _, g := range got {
				if strings.Contains(g, tc.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("complaints %v, want one containing %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------

// TestTheRuntimeStageLabelsTheLicence is #407's GREEN item about an OCI
// label: the image says which licence it is under and where the text is,
// so `docker inspect` answers both without anyone extracting the image
// first.
func TestTheRuntimeStageLabelsTheLicence(t *testing.T) {
	c := MustLoadCompliance()
	dockerfile := realDockerfile(t)
	where, complaints := ImageLicenceMaterials(c, dockerfile)
	if len(complaints) > 0 {
		t.Fatalf("the runtime stage is not readable: %v", complaints)
	}
	for _, complaint := range ImageLabelComplaints(c, dockerfile, where) {
		t.Error(complaint)
	}

	labels, err := RuntimeStageLabels(dockerfile)
	if err != nil {
		t.Fatalf("RuntimeStageLabels: %v", err)
	}
	if labels[OCILicensesLabel] != c.License.SPDXID {
		t.Errorf("the image is labelled %s=%q and the project is %s", OCILicensesLabel, labels[OCILicensesLabel], c.License.SPDXID)
	}
	if labels[LicencePathLabel] == "" {
		t.Errorf("the image carries no %s label, so a recipient is told the licence id and not where to read it", LicencePathLabel)
	}

	// The label reader is reading the runtime stage. A LABEL in a
	// builder stage is thrown away with the stage.
	builderOnly := strings.Replace(dockerfile,
		"LABEL org.opencontainers.image.licenses=", "# LABEL org.opencontainers.image.licenses=", 1)
	if builderOnly == dockerfile {
		t.Fatal("container/Dockerfile has no LABEL org.opencontainers.image.licenses line, so the control below changes nothing")
	}
	lastFrom := strings.LastIndex(builderOnly, "\nFROM ")
	moved := builderOnly[:lastFrom] + "\nLABEL " + OCILicensesLabel + "=\"" + c.License.SPDXID + "\"\n" + builderOnly[lastFrom:]
	got := ImageLabelComplaints(c, moved, where)
	if len(got) == 0 {
		t.Fatal("a licence label set in a builder stage was accepted; the reader is not confined to the stage that becomes the image")
	}
}

// TestTheLabelReaderCanRefuse is the positive control for the labels.
func TestTheLabelReaderCanRefuse(t *testing.T) {
	c := MustLoadCompliance()
	real := realDockerfile(t)
	where, _ := ImageLicenceMaterials(c, real)

	cases := []struct {
		name       string
		dockerfile string
		want       string
	}{
		{
			"the licence label is deleted",
			strings.Replace(real, `LABEL org.opencontainers.image.licenses="Apache-2.0" \`+"\n", "LABEL \\\n", 1),
			"sets no such label",
		},
		{
			"the licence label names a different licence",
			strings.Replace(real, `image.licenses="Apache-2.0"`, `image.licenses="MIT"`, 1),
			"compliance.json says the project is Apache-2.0",
		},
		{
			"the path label points somewhere the files are not",
			strings.Replace(real, `licenses.path="/licenses"`, `licenses.path="/usr/share/licenses"`, 1),
			"which is not in it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.dockerfile == real {
				t.Fatal("the mutation changed nothing")
			}
			got := ImageLabelComplaints(c, tc.dockerfile, where)
			found := false
			for _, g := range got {
				if strings.Contains(g, tc.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("complaints %v, want one containing %q", got, tc.want)
			}
		})
	}

	// The hazard the row above only half covers: the label moved in the
	// Dockerfile AND in the declaration, so the two agree with each
	// other and both disagree with where the COPYs put the files. That
	// is the shape a rename actually takes, and a check that only
	// compared the two would pass it.
	moved := strings.Replace(real, `licenses.path="/licenses"`, `licenses.path="/usr/share/licenses"`, 1)
	if moved == real {
		t.Fatal("container/Dockerfile has no licenses.path label, so this mutation changes nothing")
	}
	agreeing := c
	agreeing.Distribution.LicenceDelivery.Labels = map[string]string{
		OCILicensesLabel: c.License.SPDXID,
		LicencePathLabel: "/usr/share/licenses",
	}
	got := ImageLabelComplaints(agreeing, moved, where)
	found := false
	for _, g := range got {
		if strings.Contains(g, "which is not in it") {
			found = true
		}
	}
	if !found {
		t.Errorf("complaints %v, want one saying the labelled directory is not where the files are; the label and the declaration agreeing with each other is not evidence about the image", got)
	}

	// And the parser: a multi-key LABEL on one line, a quoted value with
	// a space in it, and a later LABEL winning over an earlier one.
	labels, err := RuntimeStageLabels("FROM scratch\nLABEL a=\"one two\" b=c\nLABEL b=d\n")
	if err != nil {
		t.Fatalf("RuntimeStageLabels: %v", err)
	}
	if labels["a"] != "one two" || labels["b"] != "d" {
		t.Errorf("parsed %v, want a=\"one two\" and b=d", labels)
	}
	if _, err := RuntimeStageLabels("FROM scratch\nLABEL legacy value\n"); err == nil {
		t.Error("the legacy space-separated LABEL form was accepted; it is not read here and reading it as a key would invent a label")
	}
}

// TestTheLicenceDirectoryIsOneDirectory keeps the path label meaningful:
// every material lands in it, so `docker cp <ctr>:/licenses .` is the
// whole answer rather than the first third of it.
func TestTheLicenceDirectoryIsOneDirectory(t *testing.T) {
	c := MustLoadCompliance()
	where, complaints := ImageLicenceMaterials(c, realDockerfile(t))
	if len(complaints) > 0 {
		t.Fatalf("the runtime stage is not readable: %v", complaints)
	}
	dirs := map[string]bool{}
	var rels []string
	for rel, dest := range where {
		dirs[path.Dir(dest)] = true
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	if len(dirs) != 1 {
		t.Errorf("%v land in more than one directory (%v); the path label names one place and a recipient with no shell cannot go looking for the others", rels, dirs)
	}
}

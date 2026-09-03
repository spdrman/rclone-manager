package packaging

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// The licence, the notices and the third-party inventory inside the
// image, and which distribution target relies on that.
//
// NOTICE says of itself that redistributing this work means carrying it
// along, and NOTICE is where the MPL-2.0 §3.2 source offer for
// go-cleanhttp and go-retryablehttp lives. Until the runtime stage
// copied it, nothing this project distributed carried it: a recipient
// whose only contact with the product is `docker pull` got two binaries
// and a bundle directory, and the offer sat in a private repository.
// That gap is older than the MPL and applies to Apache-2.0 §4(d) just as
// much; #407 is where it is fixed.
//
// There are three checks here and they are deliberately different in
// kind, because each one alone passes for a bad reason.
//
//	the Dockerfile   RuntimeStageCopies reads the COPYs after the last
//	                 FROM, the way uibundle.go reads it for the bundles,
//	                 and ImageLicenceMaterials refuses a file that is
//	                 not copied, comes out of a builder stage, or lands
//	                 at a relative path. Static and instant, and it
//	                 believes the Dockerfile.
//	the ignore file  DockerignoreComplaints refuses a COPY whose source
//	                 .dockerignore has excluded. The build fails loudly
//	                 on one of those already (proven: "failed to compute
//	                 cache key ... not found"), but it fails at release
//	                 time on a machine with a daemon, and this suite
//	                 runs everywhere and says which line did it.
//	                 provenance/ is excluded wholesale, so the inventory
//	                 needs an exception and the exception needs a guard.
//	the built image  apps/generic/tests/dockercli's
//	                 TestTheBuiltImageCarriesTheLicenceMaterials builds
//	                 container/Dockerfile and copies /licenses back out.
//	                 That is the only one that proves delivery rather
//	                 than intent, and it is not here because nothing in
//	                 distribution/ builds an image and the packaging
//	                 suite may not assume a daemon. It reads the same
//	                 declaration this file checks the Dockerfile
//	                 against, so the two cannot drift apart quietly.

// DockerfileCopy is one COPY instruction in a Dockerfile.
type DockerfileCopy struct {
	// From is the --from stage, or "" when the source is the build
	// context.
	From    string
	Sources []string
	Dest    string
	// Line is the 1-based line the instruction starts on, for a message.
	Line int
}

// dockerfileLine is one logical instruction: continuations joined,
// comments and blanks gone, with the line it started on kept for a
// message a reader can act on.
type dockerfileLine struct {
	text string
	line int
}

// dockerfileLines splits a Dockerfile into logical instructions.
func dockerfileLines(dockerfile string) []dockerfileLine {
	var lines []dockerfileLine
	raw := strings.Split(dockerfile, "\n")
	for i := 0; i < len(raw); i++ {
		start := i + 1
		text := strings.TrimSpace(raw[i])
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		for strings.HasSuffix(text, `\`) && i+1 < len(raw) {
			i++
			next := strings.TrimSpace(raw[i])
			// A comment inside a continuation is a comment, not a
			// operand: `COPY \` then `# why` then `a b` is one COPY of
			// two operands.
			if strings.HasPrefix(next, "#") {
				text = strings.TrimSuffix(text, `\`) + `\`
				continue
			}
			text = strings.TrimSuffix(text, `\`) + " " + next
		}
		lines = append(lines, dockerfileLine{text, start})
	}
	return lines
}

// parseCopy reads one COPY instruction.
func parseCopy(l dockerfileLine) (DockerfileCopy, error) {
	rest := strings.TrimSpace(l.text[len("COPY"):])
	if strings.HasPrefix(rest, "[") {
		return DockerfileCopy{}, fmt.Errorf("line %d: COPY in exec form is not read here; write it in shell form so the sources can be checked", l.line)
	}
	c := DockerfileCopy{Line: l.line}
	var operands []string
	for _, f := range strings.Fields(rest) {
		if strings.HasPrefix(f, "--") {
			if v, ok := strings.CutPrefix(f, "--from="); ok {
				c.From = v
			}
			continue
		}
		operands = append(operands, f)
	}
	if len(operands) < 2 {
		return DockerfileCopy{}, fmt.Errorf("line %d: COPY names %d operand(s), and it takes at least a source and a destination", l.line, len(operands))
	}
	c.Sources = operands[:len(operands)-1]
	c.Dest = operands[len(operands)-1]
	return c, nil
}

// RuntimeStageCopies reads the COPY instructions of a Dockerfile's final
// stage, which is the one that becomes the image.
//
// It joins backslash continuations, skips comments, and refuses the exec
// form (`COPY ["a", "b"]`) rather than mis-reading it as a path called
// `["a",`. A Dockerfile with no FROM is an error, not an empty stage.
func RuntimeStageCopies(dockerfile string) ([]DockerfileCopy, error) {
	lines := dockerfileLines(dockerfile)
	lastFrom := -1
	for i, l := range lines {
		if strings.EqualFold(firstWord(l.text), "FROM") {
			lastFrom = i
		}
	}
	if lastFrom < 0 {
		return nil, fmt.Errorf("the Dockerfile has no FROM instruction, so it has no stage that becomes an image")
	}

	var out []DockerfileCopy
	for _, l := range lines[lastFrom+1:] {
		if !strings.EqualFold(firstWord(l.text), "COPY") {
			continue
		}
		c, err := parseCopy(l)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// EveryStageCopies reads every COPY in the file, in order, whichever
// stage it is in. DockerignoreComplaints wants all of them: a build
// context that has lost a source fails the build wherever the COPY is.
func EveryStageCopies(dockerfile string) ([]DockerfileCopy, error) {
	var out []DockerfileCopy
	for _, l := range dockerfileLines(dockerfile) {
		if !strings.EqualFold(firstWord(l.text), "COPY") {
			continue
		}
		c, err := parseCopy(l)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// RuntimeStageLabels reads the LABEL instructions of the final stage,
// flattened into one map.
//
// A later LABEL for the same key wins, the way the builder resolves it.
// Values are unquoted when they are quoted, because `LABEL k="v"` and
// `LABEL k=v` set the same label and a check that told them apart would
// be checking the quoting.
func RuntimeStageLabels(dockerfile string) (map[string]string, error) {
	lines := dockerfileLines(dockerfile)
	lastFrom := -1
	for i, l := range lines {
		if strings.EqualFold(firstWord(l.text), "FROM") {
			lastFrom = i
		}
	}
	if lastFrom < 0 {
		return nil, fmt.Errorf("the Dockerfile has no FROM instruction, so it has no stage that becomes an image")
	}
	out := map[string]string{}
	for _, l := range lines[lastFrom+1:] {
		if !strings.EqualFold(firstWord(l.text), "LABEL") {
			continue
		}
		rest := strings.TrimSpace(l.text[len("LABEL"):])
		for _, pair := range splitLabelPairs(rest) {
			k, v, ok := strings.Cut(pair, "=")
			if !ok {
				return nil, fmt.Errorf("line %d: LABEL %q is not key=value; the legacy space-separated form is not read here", l.line, pair)
			}
			out[unquote(strings.TrimSpace(k))] = unquote(strings.TrimSpace(v))
		}
	}
	return out, nil
}

// splitLabelPairs splits a LABEL operand list on whitespace that is not
// inside quotes, so `a="one two" b=c` is two pairs rather than three
// fields.
func splitLabelPairs(s string) []string {
	var out []string
	var cur strings.Builder
	quote := rune(0)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
			cur.WriteRune(r)
		case r == ' ' || r == '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func firstWord(s string) string {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// LicenceMaterials is the set of files a recipient has to receive: the
// licence, the notices and the machine-readable inventory the other two
// point at.
//
// Derived from compliance.json rather than listed here, so that a fourth
// one arriving is a data change and not an edit to three files that each
// have their own list.
func LicenceMaterials(c Compliance) []string {
	var out []string
	for _, rel := range []string{c.License.File, c.License.NoticeFile, c.License.Inventory} {
		if strings.TrimSpace(rel) != "" {
			out = append(out, rel)
		}
	}
	return out
}

// ImageLicenceMaterials says where the image carries each of the files
// compliance.json declares, and every way it fails to.
//
// Each file has to be copied from the build context, not from a builder
// stage, so what ships is the checked-in file at the commit the image was
// built from and not whatever a stage happened to have under that name.
// And each has to land at an absolute path, because the runtime stage
// sets no WORKDIR and a relative destination is a guess about where the
// base image left it.
//
// The returned map is repository path to in-image path, for the two
// consumers that need to say where a recipient looks: the written offer,
// and the declaration in compliance.json that the built-image test reads.
func ImageLicenceMaterials(c Compliance, dockerfile string) (map[string]string, []string) {
	copies, err := RuntimeStageCopies(dockerfile)
	if err != nil {
		return nil, []string{fmt.Sprintf("container/Dockerfile's runtime stage could not be read: %v", err)}
	}
	var complaints []string
	where := map[string]string{}
	materials := LicenceMaterials(c)
	if len(materials) < 3 {
		complaints = append(complaints, "compliance.json declares no licence file, no notice file or no inventory, so there is nothing for the image to carry and this check has nothing to look for")
	}
	for _, rel := range materials {
		// A copy from the build context has to name the file exactly.
		// A copy out of a stage is matched on the basename, so that the
		// complaint can say "wrong source" rather than "no source",
		// which sends somebody to fix a line that is there.
		var found, fromStage *DockerfileCopy
		for i := range copies {
			for _, src := range copies[i].Sources {
				switch {
				case copies[i].From == "" && strings.TrimPrefix(src, "./") == rel:
					found = &copies[i]
				case copies[i].From != "" && path.Base(src) == path.Base(rel):
					fromStage = &copies[i]
				}
			}
		}
		if found == nil {
			found = fromStage
		}
		switch {
		case found == nil:
			complaints = append(complaints, fmt.Sprintf("container/Dockerfile's runtime stage never COPYs %s from the build context, so the image ships without it and a recipient who only has the image receives nothing it says", rel))
		case found.From != "":
			complaints = append(complaints, fmt.Sprintf("container/Dockerfile line %d copies %s from stage %q rather than from the build context, so what ships is whatever that stage had under the name and not the checked-in file", found.Line, rel, found.From))
		case !strings.HasPrefix(found.Dest, "/"):
			complaints = append(complaints, fmt.Sprintf("container/Dockerfile line %d copies %s to %q, which is relative; the runtime stage sets no WORKDIR, so where that lands is a guess", found.Line, rel, found.Dest))
		default:
			dest := found.Dest
			if strings.HasSuffix(dest, "/") || len(found.Sources) > 1 {
				dest = path.Join(dest, path.Base(rel))
			}
			where[rel] = dest
		}
	}
	return where, complaints
}

// ---------------------------------------------------------------------
// .dockerignore
// ---------------------------------------------------------------------

// DockerignoreComplaints refuses a Dockerfile that COPYs a path the
// build context does not contain.
//
// provenance/ is why this exists. The directory is excluded wholesale,
// because it describes the image rather than building it, so the
// inventory the licence and the notices both point at could not be
// COPYed at all until .dockerignore made an exception for that one file.
// An exception nothing guards is one tidy-up away from being deleted as
// redundant, and the symptom would be a release build that fails at the
// last step on a machine with a daemon rather than here.
//
// It is not a claim that the build would otherwise be silent. It would
// not: docker refuses with "failed to compute cache key ... not found",
// which is exactly the loud failure this wants. This says the same thing
// earlier, everywhere, and names the pattern.
func DockerignoreComplaints(dockerignore, dockerfile string) []string {
	copies, err := EveryStageCopies(dockerfile)
	if err != nil {
		return []string{fmt.Sprintf("container/Dockerfile could not be read: %v", err)}
	}
	var complaints []string
	for _, c := range copies {
		if c.From != "" {
			// A stage's own filesystem, not the build context.
			continue
		}
		for _, src := range c.Sources {
			clean := strings.TrimSuffix(strings.TrimPrefix(src, "./"), "/")
			if clean == "" || strings.ContainsAny(clean, "*?[") {
				// A glob's expansion depends on the context, and a
				// pattern that matches nothing is the build's own
				// error to report.
				continue
			}
			if excluded, pattern := DockerignoreExcludes(dockerignore, clean); excluded {
				complaints = append(complaints, fmt.Sprintf(
					"container/Dockerfile line %d COPYs %s from the build context and .dockerignore's %q pattern keeps it out, so that COPY has no source and the build fails at it",
					c.Line, src, pattern))
			}
		}
	}
	return complaints
}

// DockerignoreExcludes reports whether a build-context path is excluded,
// and which pattern decided it.
//
// Docker's own rules: patterns are matched in order and the last one to
// match wins, a leading "!" re-includes, and a pattern that matches a
// parent directory excludes everything under it (which is why
// "provenance" alone hides the inventory). "**" matches any run of path
// components.
func DockerignoreExcludes(dockerignore, p string) (bool, string) {
	p = path.Clean(strings.TrimPrefix(p, "/"))
	// Every ancestor, so a pattern naming a parent directory decides
	// the file under it.
	var candidates []string
	parts := strings.Split(p, "/")
	for i := range parts {
		candidates = append(candidates, strings.Join(parts[:i+1], "/"))
	}

	excluded := false
	pattern := ""
	for _, raw := range strings.Split(dockerignore, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negated := strings.HasPrefix(line, "!")
		pat := path.Clean(strings.TrimPrefix(strings.TrimPrefix(line, "!"), "/"))
		if pat == "." {
			continue
		}
		matched := false
		for _, candidate := range candidates {
			if matchIgnorePattern(pat, candidate) {
				matched = true
				break
			}
		}
		if matched {
			excluded = !negated
			pattern = line
		}
	}
	return excluded, pattern
}

// matchIgnorePattern matches one .dockerignore pattern against one path,
// component by component, with "**" standing for any run of components.
func matchIgnorePattern(pattern, name string) bool {
	pp := strings.Split(pattern, "/")
	np := strings.Split(name, "/")
	var match func(pi, ni int) bool
	match = func(pi, ni int) bool {
		for pi < len(pp) {
			if pp[pi] == "**" {
				for skip := ni; skip <= len(np); skip++ {
					if match(pi+1, skip) {
						return true
					}
				}
				return false
			}
			if ni >= len(np) {
				return false
			}
			ok, err := path.Match(pp[pi], np[ni])
			if err != nil || !ok {
				return false
			}
			pi++
			ni++
		}
		return ni == len(np)
	}
	return match(0, 0)
}

// ---------------------------------------------------------------------
// Per-target delivery
// ---------------------------------------------------------------------

// The vehicles a distribution target can name.
//
// The words are uibundle.go's, on purpose: it already had to answer the
// same question for the per-provider UI bundles ("who carries this to a
// recipient, the image or the package?"), and two vocabularies for one
// distinction would be two things to keep in step.
const (
	// LicenceVehicleImage: the canonical OCI image carries them and
	// this target ships nothing of its own.
	LicenceVehicleImage = "image"
	// LicenceVehiclePackagePayload: this target's own package carries
	// them, because it installs binaries and never pulls the image.
	LicenceVehiclePackagePayload = "package-payload"
)

// LicenceDeliveryComplaints refuses a distribution target that ships
// none of the licence materials, and a declaration that has drifted from
// what container/Dockerfile actually does.
//
// This is #407's second acceptance criterion. The shape is the one an
// unbuilt target with no unbuiltReason already has: the answer may be
// "the image carries it for me", and it may not be silence. Ten of the
// eleven targets are metadata that pulls the canonical image, so "the
// image" is right for them, and it was already written down in a note
// about checksums rather than as a field anything reads. The .spk is the
// exception and it is why the field exists at all: it installs binaries
// natively and never pulls the image, so the image's /licenses is not
// its carrier and inheriting the note's answer would have been wrong.
//
// inImage is ImageLicenceMaterials' map, so "this target relies on the
// image" is checked against the image rather than believed.
func LicenceDeliveryComplaints(c Compliance, inImage map[string]string) []string {
	materials := LicenceMaterials(c)
	var complaints []string
	var relyOnImage []string

	for _, id := range c.TargetIDs() {
		t := c.Distribution.Targets[id]
		switch t.LicenceDelivery {
		case "":
			complaints = append(complaints, fmt.Sprintf(
				"distribution target %q records no licenceDelivery, so nothing says how a recipient of it gets %s; a target that ships none of them and says nothing is the vacuous pass this rule exists to refuse",
				id, strings.Join(materials, ", ")))
		case LicenceVehicleImage:
			relyOnImage = append(relyOnImage, id)
		case LicenceVehiclePackagePayload:
			complaints = append(complaints, packagePayloadComplaints(id, t, materials)...)
		default:
			complaints = append(complaints, fmt.Sprintf(
				"distribution target %q records licenceDelivery %q, which is neither %q nor %q; an unrecognised vehicle is a target nobody has decided about",
				id, t.LicenceDelivery, LicenceVehicleImage, LicenceVehiclePackagePayload))
		}
	}

	if len(relyOnImage) > 0 {
		for _, rel := range materials {
			if _, ok := inImage[rel]; !ok {
				complaints = append(complaints, fmt.Sprintf(
					"targets %s name the canonical image as how a recipient gets %s, and container/Dockerfile's runtime stage does not put it in the image",
					strings.Join(relyOnImage, ", "), rel))
			}
		}
	}

	complaints = append(complaints, declaredImagePathComplaints(c, inImage)...)
	return complaints
}

// packagePayloadComplaints checks a target that says it carries the
// materials itself.
//
// With artifacts checked in, they have to be among them. With none, the
// target is one this repository does not build, and the obligation moves
// to the reason a reader is given for that: an unbuiltReason that does
// not name the materials leaves the only target whose licences are not
// the image's problem with nothing recording that they are anybody's.
func packagePayloadComplaints(id string, t DistributionTarget, materials []string) []string {
	var complaints []string
	if len(t.Artifacts) > 0 {
		have := map[string]bool{}
		for _, a := range t.Artifacts {
			have[path.Base(a)] = true
		}
		for _, rel := range materials {
			if !have[path.Base(rel)] {
				complaints = append(complaints, fmt.Sprintf(
					"distribution target %q carries the licence materials in its own package and its artifacts do not include %s, so the package it builds here ships without it",
					id, rel))
			}
		}
		return complaints
	}
	if strings.TrimSpace(t.UnbuiltReason) == "" {
		return []string{fmt.Sprintf(
			"distribution target %q carries the licence materials in its own package, builds no artifact here and gives no reason, so there is nothing at all that says a recipient of it ever gets them",
			id)}
	}
	for _, rel := range materials {
		if !strings.Contains(t.UnbuiltReason, rel) {
			complaints = append(complaints, fmt.Sprintf(
				"distribution target %q builds its package elsewhere and its unbuiltReason never mentions %s, so the one target the image does not cover has no record of who puts the licence materials in it",
				id, rel))
		}
	}
	return complaints
}

// declaredImagePathComplaints pins compliance.json's declared in-image
// paths to what the runtime stage does, in both directions.
//
// The declaration is what the built-image test reads, so it is the one
// place a wrong path turns into a test that looks for the file in the
// wrong place and reports the image is fine. Deriving it from the
// Dockerfile there instead would make the built-image test agree with
// the Dockerfile by construction, which is agreement about nothing.
func declaredImagePathComplaints(c Compliance, inImage map[string]string) []string {
	declared := c.Distribution.LicenceDelivery.ImagePaths
	var complaints []string
	if len(declared) == 0 {
		return []string{"distribution.licenceDelivery declares no imagePaths, so nothing outside container/Dockerfile says where in the image a recipient looks, and the built-image test has no path to check"}
	}
	var keys []string
	for rel := range declared {
		keys = append(keys, rel)
	}
	sort.Strings(keys)
	for _, rel := range keys {
		actual, ok := inImage[rel]
		switch {
		case !ok:
			complaints = append(complaints, fmt.Sprintf(
				"distribution.licenceDelivery.imagePaths declares %s at %s and container/Dockerfile's runtime stage does not put it in the image at all",
				rel, declared[rel]))
		case actual != declared[rel]:
			complaints = append(complaints, fmt.Sprintf(
				"distribution.licenceDelivery.imagePaths declares %s at %s and container/Dockerfile puts it at %s; a recipient is told to look where the file is not",
				rel, declared[rel], actual))
		}
	}
	for _, rel := range LicenceMaterials(c) {
		if _, ok := inImage[rel]; !ok {
			continue
		}
		if _, ok := declared[rel]; !ok {
			complaints = append(complaints, fmt.Sprintf(
				"container/Dockerfile puts %s in the image at %s and distribution.licenceDelivery.imagePaths does not declare it, so the built-image test never looks for it",
				rel, inImage[rel]))
		}
	}
	return complaints
}

// ImageLabelComplaints refuses a runtime stage whose OCI labels do not
// match the declaration.
//
// The image has no shell, so `docker inspect` is the only way to ask it
// anything without extracting it first. org.opencontainers.image.licenses
// is the standard field for the licence, and it is worth nothing on its
// own for a recipient who then has to guess where the text is, which is
// why the declaration carries a path label beside it and this requires
// that label's value to be the directory the materials actually land in.
func ImageLabelComplaints(c Compliance, dockerfile string, inImage map[string]string) []string {
	declared := c.Distribution.LicenceDelivery.Labels
	if len(declared) == 0 {
		return []string{"distribution.licenceDelivery declares no labels, so `docker inspect` answers nothing about the licence and a recipient with no shell in the image has nowhere to start"}
	}
	labels, err := RuntimeStageLabels(dockerfile)
	if err != nil {
		return []string{fmt.Sprintf("container/Dockerfile's runtime stage labels could not be read: %v", err)}
	}
	var keys []string
	for k := range declared {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var complaints []string
	for _, k := range keys {
		got, ok := labels[k]
		switch {
		case !ok:
			complaints = append(complaints, fmt.Sprintf(
				"compliance.json declares the label %s=%s and container/Dockerfile's runtime stage sets no such label, so `docker inspect` on the shipped image answers nothing for it",
				k, declared[k]))
		case got != declared[k]:
			complaints = append(complaints, fmt.Sprintf(
				"container/Dockerfile sets %s=%s and compliance.json declares %s", k, got, declared[k]))
		}
	}

	// The licence id has to be the licence, not a string that parses.
	if got, ok := labels[OCILicensesLabel]; ok && got != c.License.SPDXID {
		complaints = append(complaints, fmt.Sprintf(
			"container/Dockerfile labels the image %s=%s and compliance.json says the project is %s",
			OCILicensesLabel, got, c.License.SPDXID))
	}

	// And the path label has to name the directory the files are
	// actually in, or it is a signpost pointing at nothing. The value
	// checked is the one the Dockerfile sets, because that is the one
	// that ships; the declaration only decides what it is compared
	// against above.
	dir, ok := declared[LicencePathLabel]
	if !ok {
		return append(complaints, fmt.Sprintf(
			"compliance.json declares no %s label, so the licence id is on the image and where to read the text is not",
			LicencePathLabel))
	}
	if shipped, ok := labels[LicencePathLabel]; ok {
		dir = shipped
	}
	for _, rel := range LicenceMaterials(c) {
		dest, ok := inImage[rel]
		if !ok {
			continue
		}
		if path.Dir(dest) != path.Clean(dir) {
			complaints = append(complaints, fmt.Sprintf(
				"the %s label says %s and container/Dockerfile puts %s at %s, which is not in it",
				LicencePathLabel, dir, rel, dest))
		}
	}
	return complaints
}

const (
	// OCILicensesLabel is the OCI image-spec annotation for the
	// licence(s) the contents are under.
	OCILicensesLabel = "org.opencontainers.image.licenses"
	// LicencePathLabel is where the text of them lives inside the
	// image. There is no standard annotation for that, and a recipient
	// who is told the licence id and not where to read it has half an
	// answer, so this is the project's own namespace.
	LicencePathLabel = "com.iasbuilt.backupmanager.licenses.path"
)

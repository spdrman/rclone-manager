package packaging

import (
	"fmt"
	"path"
	"strings"
)

// The licence and the notices inside the image.
//
// NOTICE says of itself that redistributing this work means carrying it
// along, and NOTICE is where the MPL-2.0 §3.2 source offer for
// go-cleanhttp and go-retryablehttp lives. Until the runtime stage
// copied it, nothing this project distributed carried it: a recipient
// whose only contact with the product is `docker pull` got two binaries
// and a bundle directory, and the offer sat in a private repository. That
// gap is older than the MPL and applies to Apache-2.0 §4(d) just as much;
// #407 records it in full, and this is its first half.
//
// This reads container/Dockerfile the way uibundle.go does for the
// bundles, rather than trusting a declaration, because "the image carries
// NOTICE" is exactly the kind of sentence that stays in a JSON file after
// the line that made it true is edited out. What it cannot do is open the
// built image: nothing in distribution/ builds one, and the one suite that
// does (apps/generic/tests/dockercli) needs a Docker daemon the packaging
// suite is not allowed to assume. So this is the static half. The
// delivered form is checked by hand the way docs/deployment.md checks for
// the absence of an rclone binary, with `docker export | tar -tv`, and
// #407 keeps the built-image assertion as open work.
//
// .dockerignore is not read here, on purpose. It excludes *.md and
// provenance/, neither of which these files are, and a COPY whose source
// the ignore file has excluded fails the image build outright rather than
// producing an image without the file. That failure is loud already.

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

// RuntimeStageCopies reads the COPY instructions of a Dockerfile's final
// stage, which is the one that becomes the image.
//
// It joins backslash continuations, skips comments, and refuses the exec
// form (`COPY ["a", "b"]`) rather than mis-reading it as a path called
// `["a",`. A Dockerfile with no FROM is an error, not an empty stage.
func RuntimeStageCopies(dockerfile string) ([]DockerfileCopy, error) {
	type logical struct {
		text string
		line int
	}
	var lines []logical
	raw := strings.Split(dockerfile, "\n")
	for i := 0; i < len(raw); i++ {
		start := i + 1
		text := strings.TrimSpace(raw[i])
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		for strings.HasSuffix(text, `\`) && i+1 < len(raw) {
			i++
			text = strings.TrimSuffix(text, `\`) + " " + strings.TrimSpace(raw[i])
		}
		lines = append(lines, logical{text, start})
	}

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
		rest := strings.TrimSpace(l.text[len("COPY"):])
		if strings.HasPrefix(rest, "[") {
			return nil, fmt.Errorf("line %d: COPY in exec form is not read here; write it in shell form so the sources can be checked", l.line)
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
			return nil, fmt.Errorf("line %d: COPY names %d operand(s), and it takes at least a source and a destination", l.line, len(operands))
		}
		c.Sources = operands[:len(operands)-1]
		c.Dest = operands[len(operands)-1]
		out = append(out, c)
	}
	return out, nil
}

func firstWord(s string) string {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// ImageLicenceMaterials says where the image carries the licence file and
// the notice file compliance.json declares, and every way it fails to.
//
// Each file has to be copied from the build context, not from a builder
// stage, so what ships is the checked-in file at the commit the image was
// built from and not whatever a stage happened to have under that name.
// And each has to land at an absolute path, because the runtime stage
// sets no WORKDIR and a relative destination is a guess about where the
// base image left it.
//
// The returned map is repository path to in-image path, for the one
// consumer that needs to say where a recipient looks: the written offer.
func ImageLicenceMaterials(c Compliance, dockerfile string) (map[string]string, []string) {
	copies, err := RuntimeStageCopies(dockerfile)
	if err != nil {
		return nil, []string{fmt.Sprintf("container/Dockerfile's runtime stage could not be read: %v", err)}
	}
	var complaints []string
	where := map[string]string{}
	for _, rel := range []string{c.License.File, c.License.NoticeFile} {
		if strings.TrimSpace(rel) == "" {
			complaints = append(complaints, "compliance.json declares no licence file or no notice file, so there is nothing for the image to carry and this check has nothing to look for")
			continue
		}
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

package placement_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FR-32 says medium metadata is untrusted input, and it names two
// particular values that must never become something they are not:
//
//   - an ETag is never a content hash. Multipart uploads and encrypted
//     objects make it not one, and comparing it to a recorded SHA-256
//     would either fail forever or, worse, pass on a coincidence.
//   - S3's LastModified is upload time, never producer time, and never
//     admissible into a retention-relevant field. FR-18's two placements
//     are captured once at discovery; a move copies journal truth and does
//     not re-derive it from the destination, so an artifact's retention
//     bucketing is invariant under movement by construction.
//
// The strongest version of both rules is structural, and both already are:
// transport.ObjectInfo has no digest field for an ETag to arrive in, and
// nothing in internal/retention reads a placement at all. The tests below
// are the grep-proof the issue asks for on top of that, because a
// structural guarantee only holds while the structure does.

// productionGoFiles walks core/ and returns every non-test .go file, keyed
// by its path relative to the module root.
func productionGoFiles(t *testing.T) map[string]string {
	t.Helper()
	root := coreModuleRoot(t)

	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out[rel] = string(content)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(out) < 50 {
		t.Fatalf("only %d production Go files were found under %s, which is too few for these scans to be checking anything", len(out), root)
	}
	return out
}

func coreModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("found no go.mod above %s", dir)
		}
		dir = parent
	}
}

// TestNoProductionCodeNamesAnETag is FR-32's first rule. The word appears
// nowhere in this repository's own code, which is the strongest form of
// "nothing compares an ETag to a hash": there is nothing named to compare.
//
// Two files are allowed to say the word, and both of them say it to
// explain why they do not carry one.
func TestNoProductionCodeNamesAnETag(t *testing.T) {
	// Four files are allowed to say the word, and every one of them says
	// it to explain why it does not carry one.
	allowed := map[string]bool{
		filepath.Join("internal", "transport", "medium.go"):             true,
		filepath.Join("internal", "transport", "rclone", "medium.go"):   true,
		filepath.Join("internal", "transport", "contract", "medium.go"): true,
		filepath.Join("internal", "placement", "ladder.go"):             true,
	}

	for path, content := range productionGoFiles(t) {
		if allowed[path] {
			continue
		}
		for i, line := range strings.Split(content, "\n") {
			if strings.Contains(strings.ToLower(line), "etag") {
				t.Errorf("%s:%d names an ETag:\n\t%s\nFR-32: an ETag is never a content hash, and the way this product keeps that true is by having nowhere to put one.", path, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestTheETagScanCanActuallyFail is the positive control for the scan
// above, which is an absence assertion and therefore the exact shape that
// passes for the wrong reason when a walk visits nothing.
func TestTheETagScanCanActuallyFail(t *testing.T) {
	files := productionGoFiles(t)
	content, ok := files[filepath.Join("internal", "transport", "medium.go")]
	if !ok {
		t.Fatal("the scan did not visit internal/transport/medium.go at all, so its verdict about every other file means nothing")
	}
	if !strings.Contains(strings.ToLower(content), "etag") {
		t.Fatal("the pattern this scan searches for matches nothing even in the file that definitely discusses ETags, so the scan above proves nothing")
	}
}

// TestRetentionReadsNoMediumSuppliedValue is FR-32's second rule, held
// structurally: internal/retention decides what is kept, and it must not
// be able to read anything a medium reported.
//
// It scans for the two entry points by which such a value could arrive:
// the placement records on a Record, and transport.ObjectInfo. Neither
// appears in that package at all, which is a stronger statement than "the
// timestamp is not used": there is no medium-supplied value in scope for a
// future change to reach for by accident.
func TestRetentionReadsNoMediumSuppliedValue(t *testing.T) {
	forbidden := []string{".Placements", "ObjectInfo", "placement.", "LastModified"}

	for path, content := range productionGoFiles(t) {
		if !strings.HasPrefix(path, filepath.Join("internal", "retention")+string(filepath.Separator)) {
			continue
		}
		for i, line := range strings.Split(content, "\n") {
			code := line
			if idx := strings.Index(code, "//"); idx >= 0 {
				code = code[:idx]
			}
			for _, f := range forbidden {
				if strings.Contains(code, f) {
					t.Errorf("%s:%d reads %q:\n\t%s\nFR-32: nothing a medium reported may reach a retention decision, and an artifact's bucketing has to be invariant under movement.", path, i+1, f, strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestTheRetentionScanActuallyVisitsRetention is the positive control for
// the scan above: a path filter that matched nothing would pass it
// silently forever.
func TestTheRetentionScanActuallyVisitsRetention(t *testing.T) {
	visited := 0
	for path := range productionGoFiles(t) {
		if strings.HasPrefix(path, filepath.Join("internal", "retention")+string(filepath.Separator)) {
			visited++
		}
	}
	if visited < 3 {
		t.Fatalf("the retention scan visited %d files; internal/retention has more than that, so the filter is wrong and the scan proves nothing", visited)
	}
}

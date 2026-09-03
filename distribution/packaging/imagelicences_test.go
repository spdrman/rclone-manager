package packaging

import (
	"os"
	"path/filepath"
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

// TestTheImageCarriesTheLicenceAndTheNotice is the check #407's first
// acceptance criterion asks for, as far as it can be asked of the tree.
//
// NOTICE is where Apache-2.0 §4(d)'s attribution and MPL-2.0 §3.2's source
// offer both live, and TestNoticeAttributesEveryComponent proves it says
// the right things. Nothing proved it went anywhere. This reads the
// runtime stage, the way the bundle tests do, and refuses an image that
// ships the binaries and not the files that say what they are under.
//
// It cannot open the built image; see imagelicences.go for why, and for
// how the delivered form is checked by hand.
func TestTheImageCarriesTheLicenceAndTheNotice(t *testing.T) {
	c := MustLoadCompliance()
	dockerfile := realDockerfile(t)

	where, complaints := ImageLicenceMaterials(c, dockerfile)
	for _, complaint := range complaints {
		t.Error(complaint)
	}
	if len(complaints) > 0 {
		t.FailNow()
	}
	for _, rel := range []string{c.License.File, c.License.NoticeFile} {
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

// TestTheImageLicenceReaderCanRefuse is the positive control for the test
// above. Each row is the real Dockerfile with one thing wrong, because a
// reader that has only ever been watched passing has not been watched.
func TestTheImageLicenceReaderCanRefuse(t *testing.T) {
	c := MustLoadCompliance()
	real := realDockerfile(t)
	const licenceLine = "COPY LICENSE /licenses/LICENSE\n"
	const noticeLine = "COPY NOTICE /licenses/NOTICE\n"
	for _, needle := range []string{licenceLine, noticeLine} {
		if strings.Count(real, needle) != 1 {
			t.Fatalf("container/Dockerfile does not carry %q exactly once, so the mutations below edit nothing", strings.TrimSpace(needle))
		}
	}
	lastFrom := strings.LastIndex(real, "\nFROM ")
	if lastFrom < 0 {
		t.Fatal("container/Dockerfile has no runtime FROM to move lines above")
	}

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
			"both COPYs are deleted",
			strings.Replace(strings.Replace(real, licenceLine, "", 1), noticeLine, "", 1),
			[]string{"never COPYs LICENSE", "never COPYs NOTICE"},
		},
		{
			// The lines exist, above the last FROM, so they land in a
			// builder stage that is thrown away.
			"both COPYs are in a builder stage",
			strings.Replace(strings.Replace(real, licenceLine, "", 1), noticeLine, "", 1)[:lastFrom] + "\n" + licenceLine + noticeLine +
				strings.Replace(strings.Replace(real, licenceLine, "", 1), noticeLine, "", 1)[lastFrom:],
			[]string{"never COPYs LICENSE", "never COPYs NOTICE"},
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
			"COPY LICENSE /licenses/LICENSE\nCOPY NOTICE /licenses/NOTICE\n",
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

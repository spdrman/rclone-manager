// Command provenance generates the release's compliance artifacts:
// issue #88 (B5.2), docs/EPIC-B-multi-nas.md §61 and §73 Work Package 5.2.
//
//	go run ./cmd/provenance          # check: exit 1 if anything is stale
//	go run ./cmd/provenance -write   # regenerate provenance/ and NOTICE
//
// It is the "one release step" §73 WP5.2's REFACTOR asks for. Everything
// it writes is derived from the tree on the spot: the third-party licence
// inventory from `go list -deps` and ui/shared/package-lock.json, the SBOM
// from that inventory, the checksum manifest from the files themselves,
// and the provenance bundle from all of the above plus
// container/release-manifest.json.
//
// It never writes container/release-manifest.json and never pushes
// anything. The manifest is scripts/release/record-release-hashes.sh's,
// which needs a two-architecture Docker build to say anything true; the
// push and the signature are scripts/release/publish-image.sh's.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spdrman/rclone-manager/distribution/packaging"
)

// The default is CHECK and -write is the opt-in, which is the wrong way
// round for a generator and the right way round for this one. Everything
// it produces is evidence about the tree, so the question worth asking on
// every run is whether the checked-in evidence still describes the tree,
// and a tool whose default regenerates answers that by making it true.
// The same run is therefore usable as a gate step and by a person about
// to cut a release, with the exit code carrying the whole contract: zero
// when the artifacts match, one when they do not, and the message naming
// the command that fixes it.
//
// Generation happens once, before either branch, so check and write
// compare and write exactly the same bytes. Two passes would leave room
// for a check that passes against output a subsequent write would not
// reproduce.
func main() {
	write := flag.Bool("write", false, "regenerate the artifacts instead of only checking them")
	flag.Parse()

	// packaging.Path resolves everything through packaging.RepoRoot,
	// which is written relative to the directory `go test` runs the
	// packaging suite in. Moving there rather than teaching Path a
	// second base keeps one path authority in the repository: two would
	// be two things to keep in step, and a generator that resolved paths
	// differently from the check that verifies its output is a way for
	// both to be self-consistently wrong.
	root, err := repoRoot()
	if err != nil {
		fail(err)
	}
	if err := os.Chdir(filepath.Join(root, "distribution", "packaging")); err != nil {
		fail(err)
	}

	g, err := packaging.GenerateProvenance()
	if err != nil {
		fail(err)
	}

	stale := 0
	for _, f := range g.Files() {
		path := packaging.Path(f.Path)
		existing, readErr := os.ReadFile(path)
		unchanged := readErr == nil && bytes.Equal(existing, f.Data)

		if !*write {
			if unchanged {
				continue
			}
			stale++
			if readErr != nil {
				fmt.Fprintf(os.Stderr, "stale: %s does not exist\n", f.Path)
			} else {
				fmt.Fprintf(os.Stderr, "stale: %s is not what this tree generates (%d bytes on disk, %d generated)\n",
					f.Path, len(existing), len(f.Data))
			}
			continue
		}
		if unchanged {
			fmt.Printf("unchanged %s\n", f.Path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fail(err)
		}
		if err := os.WriteFile(path, f.Data, 0o644); err != nil {
			fail(err)
		}
		fmt.Printf("wrote     %s (%d bytes)\n", f.Path, len(f.Data))
	}

	if stale > 0 {
		fmt.Fprintf(os.Stderr, "\n%d compliance artifact(s) do not match this tree. Regenerate with:\n\n    (cd distribution && go run ./cmd/provenance -write)\n", stale)
		os.Exit(1)
	}
	if !*write {
		fmt.Println("provenance: every compliance artifact matches this tree")
	}
}

// repoRoot walks up from the working directory to the checkout root,
// identified by go.work. Walking up rather than taking a flag means this
// behaves the same from distribution/, from the repository root and from a
// git hook, which is where a path bug would otherwise hide.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.work above the working directory, so the repository root cannot be located")
		}
		dir = parent
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "provenance: %v\n", err)
	os.Exit(1)
}

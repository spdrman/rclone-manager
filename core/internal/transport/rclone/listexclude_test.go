package rclone

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/walk"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is issue #737: a backup set pointed at a directory that also
// holds an application's own cache.
//
// The shape that found it is a web application's upload directory with a
// tile cache under it (uploads/tiles/<uuid>/..., 65k files across 1.6k
// subdirectories). List recurses unconditionally and FR-5's include
// patterns are basename-only, so discovery walked the whole cache on every
// poll and then threw all of it away: ninety seconds of listing to return
// the two artifacts anybody wanted. An operator watching that has no
// configuration to reach for, because "recurse into uploads/ but never
// into uploads/tiles/" is not a thing a basename pattern can say.
//
// The distinction the tests below are about is RESULT SET versus WALK. A
// filter that merely dropped the cache's objects from the answer would
// make the returned list right and change nothing about the cost, which is
// the entire complaint: the hang is the listing, not the slice. So there
// are two assertions and both are load-bearing. One says the answer is
// right. The other says the excluded subtree was never listed at all, and
// that is the one that would go red if this were implemented as a
// post-walk discard.

// countingFs is an fs.Fs that records every directory rclone asks it to
// list and otherwise does nothing.
//
// It exists because "was this directory walked" is not observable from
// List's return value: a pruned walk and a walked-then-discarded one
// produce the identical answer. Embedding the interface rather than
// implementing it means an rclone upgrade that adds a method to fs.Fs
// still compiles here, and the one method that is overridden is the one
// rclone's directory walk actually goes through (fs/list.DirSorted calls
// f.List per directory, so one call is one directory read).
type countingFs struct {
	fs.Fs

	mu     sync.Mutex
	listed []string
}

func (c *countingFs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	c.mu.Lock()
	c.listed = append(c.listed, dir)
	c.mu.Unlock()
	return c.Fs.List(ctx, dir)
}

func (c *countingFs) directoriesListed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.listed))
	copy(out, c.listed)
	return out
}

// cacheTree is the fixture shape from the issue, shrunk: two artifacts an
// operator wants, one ordinary subdirectory they also want recursed (FR-8's
// directory-per-run layout is why List recurses at all), and a nested cache
// nobody wants at any depth.
func cacheTree() []string {
	return []string{
		"a.pdf",
		"b.pdf",
		"runs/2026-01-01/nightly.dump",
		"tiles/0f2a/deep/x.webp",
		"tiles/0f2a/deep/y.webp",
		"tiles/91bc/z.webp",
	}
}

func pathsOf(arts []transport.RemoteArtifact) []string {
	out := make([]string, 0, len(arts))
	for _, a := range arts {
		out = append(out, a.Path)
	}
	return out
}

// TestListOmitsAnExcludedSubtree is the answer half: what an operator who
// wrote exclude_paths gets back.
func TestListOmitsAnExcludedSubtree(t *testing.T) {
	_, root := localFsWith(t, cacheTree()...)

	got, err := New().List(context.Background(), transport.Source{
		ID:           "excl-737",
		Type:         "local",
		Root:         root,
		ExcludePaths: []string{"tiles"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	want := []string{"a.pdf", "b.pdf", "runs/2026-01-01/nightly.dump"}
	if got, want := strings.Join(pathsOf(got), ","), strings.Join(want, ","); got != want {
		t.Errorf("List returned %q, want %q: the excluded subtree must be gone and everything else, including an ordinary subdirectory, must still be there", got, want)
	}
}

// TestListWithoutExcludePathsStillRecursesEverything is the control, and it
// is not decoration: a fixture whose tiles/ directory was empty or misspelt
// would make the test above pass against an implementation that excludes
// nothing. This one fails if the cache is not really there to be excluded,
// and it is also the guarantee that every configuration written before this
// field existed keeps behaving exactly as it did (List's own doc: full,
// unconditional recursion).
func TestListWithoutExcludePathsStillRecursesEverything(t *testing.T) {
	_, root := localFsWith(t, cacheTree()...)

	got, err := New().List(context.Background(), transport.Source{
		ID:   "excl-737-control",
		Type: "local",
		Root: root,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	want := []string{
		"a.pdf",
		"b.pdf",
		"runs/2026-01-01/nightly.dump",
		"tiles/0f2a/deep/x.webp",
		"tiles/0f2a/deep/y.webp",
		"tiles/91bc/z.webp",
	}
	if got, want := strings.Join(pathsOf(got), ","), strings.Join(want, ","); got != want {
		t.Errorf("List with no exclude_paths returned %q, want %q", got, want)
	}
}

// TestAnExcludedSubtreeIsNeverListed is the money test in this file, and
// the reason excludeScope sets an rclone filter rather than filtering
// List's output.
//
// 65k files under 1.6k directories is 1.6k directory reads, and over sftp
// (no native recursive listing) each one is a round trip. Discarding those
// entries after the fact returns the right answer in the wrong time, which
// against the deployment that reported #737 is a discovery pass that does
// not finish inside the poll interval. So this asserts the cost, not the
// answer: the walk must never ask for tiles/ or anything under it.
func TestAnExcludedSubtreeIsNeverListed(t *testing.T) {
	f, _ := localFsWith(t, cacheTree()...)
	counter := &countingFs{Fs: f}

	ctx, includeAll, err := excludeScope(oneConnectionAtATime(context.Background()), transport.Source{
		ID:           "excl-737-prune",
		Type:         "local",
		ExcludePaths: []string{"tiles"},
	})
	if err != nil {
		t.Fatalf("excludeScope: %v", err)
	}
	if _, _, err := walk.GetAll(ctx, counter, "", includeAll, -1); err != nil {
		t.Fatalf("GetAll: %v", err)
	}

	for _, dir := range counter.directoriesListed() {
		if dir == "tiles" || strings.HasPrefix(dir, "tiles/") {
			t.Errorf("the walk listed %q; an excluded subtree must be pruned, not walked and discarded (all listed: %q)", dir, counter.directoriesListed())
		}
	}
	// And the walk did happen: a pruned-everything filter would also pass
	// the loop above.
	if listed := counter.directoriesListed(); len(listed) < 3 {
		t.Errorf("the walk listed only %q; it still has to recurse the directories that were not excluded", listed)
	}
}

// TestAnExcludedPathIsAcceptedWithOrWithoutATrailingSlash: "tiles" and
// "tiles/" are the same directory, and an operator who types either means
// the same thing. rclone's own filter syntax does distinguish them (a
// trailing slash is what makes a rule a directory rule), so this is a
// difference this adapter has to absorb rather than pass on.
func TestAnExcludedPathIsAcceptedWithOrWithoutATrailingSlash(t *testing.T) {
	for _, spelling := range []string{"tiles", "tiles/", "/tiles", "/tiles/"} {
		t.Run(spelling, func(t *testing.T) {
			_, root := localFsWith(t, cacheTree()...)

			got, err := New().List(context.Background(), transport.Source{
				ID:           "excl-737-slash",
				Type:         "local",
				Root:         root,
				ExcludePaths: []string{spelling},
			})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			for _, p := range pathsOf(got) {
				if strings.HasPrefix(p, "tiles/") {
					t.Errorf("exclude_paths %q left %q in the listing", spelling, p)
				}
			}
		})
	}
}

// TestOnlyTheNamedSubtreeIsExcluded: the rule is anchored at the Fs root
// (the set's remote_path), so it names one place in the tree rather than
// every directory that happens to share a basename. An operator excluding
// a cache under one directory must not silently lose an unrelated
// directory of the same name somewhere else, because that is the
// protection-dies-quietly failure this project exists to prevent.
func TestOnlyTheNamedSubtreeIsExcluded(t *testing.T) {
	_, root := localFsWith(t, "uploads/tiles/x.webp", "archive/tiles/keep.dump")

	got, err := New().List(context.Background(), transport.Source{
		ID:           "excl-737-anchor",
		Type:         "local",
		Root:         root,
		ExcludePaths: []string{"uploads/tiles"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if want := []string{"archive/tiles/keep.dump"}; strings.Join(pathsOf(got), ",") != strings.Join(want, ",") {
		t.Errorf("List returned %q, want %q: an exclude is a path from remote_path, not a basename", pathsOf(got), want)
	}
}

package state

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// shippedMigrationChecksums is the fast half of this guard and it has one
// weakness that cannot be argued away: it lives in the working tree, next to
// the files it describes. Edit a landed migration, put the new sha256 in the
// table, and both halves agree about the wrong thing. The whole internal/state
// suite goes green with a change in it that stops every existing deployment
// from opening its journal. tests/compat cannot see it either, because
// applyUpTo rebuilds every old schema version from the same edited SQL.
//
// This file is the half that cannot be edited in step, because it does not
// read the working tree at all. It asks git for the bytes this repository has
// actually PUBLISHED, and holds the tree to them.
//
// WHICH REF IS THE ANCHOR
//
// `release`, not a tag, and docs/release-branch.md is why. That branch is the
// pointer at what has actually shipped ("git checkout release gives you the
// tree the current release was cut from"), it is append-only by a GitHub
// ruleset that blocks deletion, non-fast-forward and every direct push, and
// nothing is ever branched off it. That is exactly the property an anchor
// needs: its content is fixed by something outside this checkout, so no edit
// here can move it.
//
// Release tags were the first suggestion and they are not the answer here,
// because this repository does not reliably carry one. v0.1.0 is tagged and
// 0.2.0 is not, so `git describe --tags --abbrev=0` does not even resolve in
// this checkout: v0.1.0 was cut on `release` before the current ruleset
// existed and is not an ancestor of main, so git answers "no tags can describe
// HEAD". A guard built on that command would fail for a reason that has
// nothing to do with migrations.
//
// Tags are still asked, as a supplement and never as the whole answer. What
// they add is the one file `release`'s tip could stop carrying: a migration an
// older release shipped and a later cut deleted is still anchored by the tag.
// What they must never do is stand in for the branch. origin/release missing
// while a tag resolves would be a quietly weaker run reporting the same green
// as a full one, so it is a failure instead, and the local `release` branch is
// not a fallback either: nothing protects a local branch from being reset, so
// as an anchor it is exactly the fallback that reads like the real check.
// There are two outcomes here and no third: anchored against origin/release
// plus whatever tags exist, or red.
//
// Everything that resolves is UNIONED. A ref can only ever add a constraint,
// so a checkout carrying more published refs is checked harder, never less.
// Two published refs disagreeing about the same file is itself a failure: it
// means a shipped migration was edited between releases, and there is no
// correct side to pick because deployments exist on both.
//
// WHAT HAPPENS WITH NO GIT, A SHALLOW CLONE, OR NO PUBLISHED REFS
//
// It fails. Not a skip.
//
// scripts/ci-local.sh answers this same question with three outcomes (0 for
// ok, 3 for INCOMPLETE, anything else for FAILED) precisely so a check that
// did not run cannot look like one that passed, and it ledgers every skip it
// takes. A Go test has no such ledger: the gate does not read Go-level
// t.Skip at all, so a skip here would be the invisible stop that whole design
// exists to prevent. Pass and fail are the only two outcomes this test can
// make visible, so "I could not perform this check" is reported as fail.
// TestReleaseManifestPinsACommitThisHistoryCanReach already takes the same
// line for the same reason ("no ref could decide ... so this check did not
// run").
//
// The cost is real and worth naming, because it lands on somebody else's
// environment: .github/workflows/ci.yml's core-build-vet-test job used to
// check out with actions/checkout's default single-commit shallow clone,
// which has no origin/release and no tags at all. This test is what put that
// requirement on the core module, so the same change adds fetch-depth: 0
// there, the way apps-common-build-vet-test already carries it for
// packaging's release-manifest checks. Anything else that runs this suite
// needs a real checkout too: not a shallow clone, and not an unpacked
// archive.
//
// There is deliberately no override. The case that would need one, withdrawing
// a migration that has not shipped, needs nothing: an unreleased migration is
// not at any published ref, so this test never looks at it. Withdrawing one
// that HAS shipped is the thing that must never happen, and it has no escape
// hatch on purpose.

// publishedRef is one ref whose tree this repository has actually published,
// and the reason it counts as published.
type publishedRef struct {
	Ref string
	Why string
}

// publishedBranchRef is the publish pointer, and there is exactly one form of
// it. The remote-tracking copy is what the GitHub ruleset protects; a local
// `release` branch is protected by nothing and its owner can reset it
// anywhere, so it is not accepted as a substitute.
var publishedBranchRef = publishedRef{
	Ref: "origin/release",
	Why: "the publish branch, which docs/release-branch.md holds to being append-only, so the bytes on it are what deployments in the field actually applied",
}

// publishedTagPattern is what counts as a release tag here. v0.1.0 is the only
// one today; the pattern is deliberately narrow so a scratch tag somebody made
// on a work branch does not start freezing files that never shipped.
const publishedTagPattern = "v[0-9]*"

// migrationsDirFromRepoRoot is where the embedded migrations live, spelled
// from the repository root, because that is the only spelling git takes.
const migrationsDirFromRepoRoot = "core/migrations"

// anchoredMigration is one migration file as a published release carries it.
// Checksum is the sha256 of the published bytes, computed exactly the way
// loadMigrations computes it for the working copy, so the two are directly
// comparable.
type anchoredMigration struct {
	Filename string
	Checksum string
	Refs     []string
}

func TestShippedMigrationsMatchWhatWasPublished(t *testing.T) {
	repoRoot, err := gitRepoRoot()
	if err != nil {
		t.Fatalf(`this check could not run, so it is a failure rather than a pass: %v

TestShippedMigrationsAreImmutable compares core/migrations/ against a table
that lives in the working tree beside it, so both can be edited together. This
test is the half that cannot be, and it needs git and this repository's
history to do it. It needs a real checkout with the published refs fetched, not
a shallow clone and not an unpacked archive.`, err)
	}

	refs, err := resolvePublishedRefs(repoRoot)
	if err != nil {
		t.Fatalf("asking git which published refs this checkout has: %v", err)
	}
	if !hasPublishBranch(refs) {
		t.Fatalf(`%s is not in this checkout, so nothing at full strength anchors core/migrations/ and this check did not run. Resolved %v.

A shallow clone is the usual cause: actions/checkout fetches one commit by
default, which carries no remote branches and no tags. Fetch the publish
branch, and the tags while you are there:

    git fetch origin release:refs/remotes/origin/release
    git fetch --tags

Tags on their own are not accepted in its place. They only ever supplement the
branch here, and a run anchored to a tag alone would print exactly the same
green as a full one while checking less.

This is a failure and not a skip on purpose. The gate has no ledger for a
Go-level t.Skip and a passing test's t.Logf is invisible without -v, so a skip
here would be a check that stopped with nobody noticing, which this repository
holds to be worse than no check at all.`, publishedBranchRef.Ref, refNames(refs))
	}

	// Name every ref that answered and why it counts as published, so a reader
	// of a -v run can tell what this verdict rests on rather than taking the
	// word "anchored" on trust.
	for _, r := range refs {
		t.Logf("anchor: %s, %s", r.Ref, r.Why)
	}

	anchored, problems, err := collectPublishedMigrations(repoRoot, refs)
	if err != nil {
		t.Fatalf("reading core/migrations/ out of the published refs: %v", err)
	}

	// The anti-vacuity half, and it is the one that matters most here. A
	// published ref that resolves but carries no migrations at all would let
	// every comparison below pass without looking at a single file, which is
	// exactly the shape of a guard that has quietly stopped.
	if len(anchored) == 0 {
		t.Fatalf("the published refs %v carry no files under %s, so this check compared nothing",
			refNames(refs), migrationsDirFromRepoRoot)
	}

	known, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	onDisk := make(map[string]migration, len(known))
	for _, m := range known {
		onDisk[m.filename] = m
	}

	problems = append(problems, compareAgainstAnchor(anchored, onDisk, shippedMigrationChecksums)...)
	for _, p := range problems {
		t.Error(p)
	}

	// Say what actually got checked, and what did not. A migration added since
	// the last release is genuinely not frozen yet and must not fail here, but
	// a reader needs to be able to see which side of that line each file is on,
	// otherwise "anchored" and "nobody looked" read identically in a green run.
	for _, a := range anchored {
		t.Logf("anchored: %s, published in %s", a.Filename, strings.Join(a.Refs, ", "))
	}
	for _, m := range known {
		if _, ok := findAnchored(anchored, m.filename); !ok {
			t.Logf("not yet frozen: %s is in the tree and in no published release, so it can still be changed", m.filename)
		}
	}
}

// compareAgainstAnchor is the whole decision, split out from the test so the
// control below can drive it against a fixture instead of the real repository.
// The reason to split is the same one classifyUnreachedOutcome was split for
// in distribution/packaging: the rule that matters is a pure comparison, and
// asserting it directly beats engineering a git history that happens to
// produce each case.
//
// anchored is what the published releases carry. onDisk is this tree, keyed by
// filename. table is shippedMigrationChecksums, keyed by version. The table is
// compared too, and separately, because a table edited into agreement with a
// doctored file is the specific defeat this file exists to close, and naming
// it as its own problem tells the reader what actually happened.
func compareAgainstAnchor(anchored []anchoredMigration, onDisk map[string]migration, table map[int]string) []string {
	var problems []string
	for _, a := range anchored {
		where := strings.Join(a.Refs, ", ")

		m, ok := onDisk[a.Filename]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"%s is published in %s and is not in this tree at all.\n"+
					"A shipped migration cannot be withdrawn. Every journal that applied it still records "+
					"its version, and migrate refuses a version it does not recognise with "+
					"ErrUnknownSchemaVersion, so removing the file brings back the outage in a different "+
					"shape. Put it back.", a.Filename, where))
			continue
		}

		if m.checksum != a.Checksum {
			problems = append(problems, fmt.Sprintf(
				"%s does not match the bytes published in %s.\n\n"+
					"This file has shipped. Every deployment that applied version %d recorded the sha256 "+
					"of the published bytes, and migrate compares that against this binary's copy before "+
					"it opens the journal at all. Different bytes means ErrSchemaDrift and a journal that "+
					"will not open, which is the outage #396 was filed for.\n\n"+
					"See what changed:\n"+
					"    git diff %s -- %s\n\n"+
					"The fix is to put the file back. It is NOT to update shippedMigrationChecksums: that "+
					"table is derived from these files, so making it agree with an edited one just makes "+
					"both halves agree about the wrong thing. This check reads the published bytes, which "+
					"nothing in this working tree can change. If the file says something that is no longer "+
					"true, say so in migrate.go or in a new migration.",
				m.filename, where, m.version, a.Refs[0], path.Join(migrationsDirFromRepoRoot, a.Filename)))
		}

		want, ok := table[m.version]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf(
				"%s is published in %s and has no row in shippedMigrationChecksums.\n"+
					"A migration that has already shipped is the one kind that must be in that table, "+
					"because the table is what makes an accidental edit fail in a second instead of on "+
					"somebody's NAS.", m.filename, where))
		case want != a.Checksum:
			problems = append(problems, fmt.Sprintf(
				"shippedMigrationChecksums[%d] does not match the bytes %s published for %s.\n\n"+
					"That row has been moved off what actually shipped. The usual way this happens is the "+
					"one this whole file exists to stop: a landed migration gets edited, the table gets "+
					"the new hash pasted into it, and both halves of the guard then agree about a file "+
					"that bricks every existing install. Put the file and the row back to what %s carries.",
				m.version, where, m.filename, a.Refs[0]))
		}
	}
	sort.Strings(problems)
	return problems
}

// collectPublishedMigrations reads core/migrations/*.sql out of every published
// ref and unions them by filename. Two refs carrying different bytes for the
// same file is returned as a problem rather than silently resolved: it means a
// shipped migration changed between releases, and picking either side would be
// picking which set of deployments to break.
func collectPublishedMigrations(repoDir string, refs []publishedRef) ([]anchoredMigration, []string, error) {
	byFile := map[string]map[string][]string{}

	for _, r := range refs {
		names, err := gitListMigrations(repoDir, r.Ref)
		if err != nil {
			return nil, nil, fmt.Errorf("listing %s at %s: %w", migrationsDirFromRepoRoot, r.Ref, err)
		}
		for _, name := range names {
			sum, err := gitBlobSHA256(repoDir, r.Ref, path.Join(migrationsDirFromRepoRoot, name))
			if err != nil {
				return nil, nil, fmt.Errorf("reading %s at %s: %w", name, r.Ref, err)
			}
			if byFile[name] == nil {
				byFile[name] = map[string][]string{}
			}
			byFile[name][sum] = append(byFile[name][sum], r.Ref)
		}
	}

	var out []anchoredMigration
	var problems []string
	for name, sums := range byFile {
		if len(sums) > 1 {
			problems = append(problems, fmt.Sprintf(
				"%s is published with different bytes by different releases (%s).\n"+
					"A shipped migration is frozen, so this means one release edited a file another "+
					"release had already applied somewhere. There is no correct side to pick: deployments "+
					"exist on both.", name, describeDivergence(sums)))
			continue
		}
		for sum, whichRefs := range sums {
			sort.Strings(whichRefs)
			out = append(out, anchoredMigration{Filename: name, Checksum: sum, Refs: whichRefs})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	sort.Strings(problems)
	return out, problems, nil
}

// describeDivergence names which refs are on which side, without printing
// either checksum. The refs are what a reader needs to go and diff; the hashes
// are just the paste-ready strings that made the old guard defeatable.
func describeDivergence(sums map[string][]string) string {
	var sides []string
	for _, whichRefs := range sums {
		sort.Strings(whichRefs)
		sides = append(sides, strings.Join(whichRefs, "+"))
	}
	sort.Strings(sides)
	return strings.Join(sides, " differs from ")
}

func findAnchored(anchored []anchoredMigration, filename string) (anchoredMigration, bool) {
	for _, a := range anchored {
		if a.Filename == filename {
			return a, true
		}
	}
	return anchoredMigration{}, false
}

// hasPublishBranch says whether the anchor is at full strength. Tags can only
// add to what the branch pins; they cannot stand in for it.
func hasPublishBranch(refs []publishedRef) bool {
	for _, r := range refs {
		if r.Ref == publishedBranchRef.Ref {
			return true
		}
	}
	return false
}

func refNames(refs []publishedRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Ref)
	}
	return out
}

// resolvePublishedRefs returns every published ref this checkout actually has:
// the publish branch, plus every release tag. Whether the branch is among them
// is the caller's question, because the answer to "it is not" is a failure and
// this function is also used to establish that a fixture published nothing.
func resolvePublishedRefs(repoDir string) ([]publishedRef, error) {
	var out []publishedRef
	ok, err := gitRefExists(repoDir, publishedBranchRef.Ref)
	if err != nil {
		return nil, err
	}
	if ok {
		out = append(out, publishedBranchRef)
	}

	tags, err := gitListTags(repoDir, publishedTagPattern)
	if err != nil {
		return nil, err
	}
	for _, tag := range tags {
		out = append(out, publishedRef{
			Ref: tag,
			Why: "a release tag, so its tree is a version somebody could have installed",
		})
	}
	return out, nil
}

// gitRepoRoot locates the repository this package's source sits in. go test
// runs with the working directory set to the package directory, which is
// inside the checkout, so asking git from there resolves worktrees correctly
// too.
func gitRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	out, err := runGit(wd, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("git rev-parse --show-toplevel said nothing from %s", wd)
	}
	if _, err := os.Stat(filepath.Join(root, migrationsDirFromRepoRoot)); err != nil {
		return "", fmt.Errorf("%s has no %s, so this is not the repository this package ships in: %w",
			root, migrationsDirFromRepoRoot, err)
	}
	return root, nil
}

func gitRefExists(repoDir, ref string) (bool, error) {
	cmd := exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, nil
	}
	return false, fmt.Errorf("git rev-parse %s: %w", ref, err)
}

func gitListTags(repoDir, pattern string) ([]string, error) {
	out, err := runGit(repoDir, "tag", "--list", pattern)
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			tags = append(tags, line)
		}
	}
	sort.Strings(tags)
	return tags, nil
}

// gitListMigrations returns the *.sql filenames (base names) under
// core/migrations/ as ref carries them.
func gitListMigrations(repoDir, ref string) ([]string, error) {
	out, err := runGit(repoDir, "ls-tree", "--name-only", ref, "--", migrationsDirFromRepoRoot+"/")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasSuffix(line, ".sql") {
			continue
		}
		names = append(names, path.Base(line))
	}
	sort.Strings(names)
	return names, nil
}

// gitBlobSHA256 returns the sha256 of a file's bytes at ref, computed over the
// whole file the way loadMigrations does, comments included.
func gitBlobSHA256(repoDir, ref, repoPath string) (string, error) {
	out, err := runGit(repoDir, "cat-file", "blob", ref+":"+repoPath)
	if err != nil {
		return "", err
	}
	return sha256Hex(out), nil
}

// sha256Hex is loadMigrations' checksum, over bytes that came from git rather
// than from the embedded FS, so the two are the same quantity.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func runGit(repoDir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// TestCompareAgainstAnchor_CatchesTheTableEditedIntoAgreement is the control
// for the thing the test above exists to catch, driven directly rather than
// through the real repository.
//
// The arm that matters is the first. It is the mutation that defeated the
// checksum table on its own: edit a landed migration, take the new hash out of
// the failure message, put it in the table, and TestShippedMigrationsAreImmutable
// goes green because the two things it compares now agree. Both of them moved.
// The published bytes did not, which is the whole point of asking git.
func TestCompareAgainstAnchor_CatchesTheTableEditedIntoAgreement(t *testing.T) {
	const shipped = "aaaa"
	const edited = "bbbb"
	anchored := []anchoredMigration{{Filename: "0002_x.sql", Checksum: shipped, Refs: []string{"origin/release"}}}

	t.Run("an edited file with the table edited in step", func(t *testing.T) {
		onDisk := map[string]migration{"0002_x.sql": {version: 2, filename: "0002_x.sql", checksum: edited}}
		table := map[int]string{2: edited}

		problems := compareAgainstAnchor(anchored, onDisk, table)
		if len(problems) != 2 {
			t.Fatalf("got %d problems, want 2 (the file, and the table row): %v", len(problems), problems)
		}
		joined := strings.Join(problems, "\n")
		if !strings.Contains(joined, "does not match the bytes published in origin/release") {
			t.Errorf("nothing reported the file against the published bytes:\n%s", joined)
		}
		if !strings.Contains(joined, "shippedMigrationChecksums[2]") {
			t.Errorf("nothing named the table row that was moved off what shipped:\n%s", joined)
		}
		if strings.Contains(joined, edited) {
			t.Errorf("the failure prints the edited file's own checksum %q, which is the paste-ready "+
				"string that made the table defeatable:\n%s", edited, joined)
		}
	})

	t.Run("an unchanged file is not a problem", func(t *testing.T) {
		onDisk := map[string]migration{"0002_x.sql": {version: 2, filename: "0002_x.sql", checksum: shipped}}
		if problems := compareAgainstAnchor(anchored, onDisk, map[int]string{2: shipped}); len(problems) != 0 {
			t.Fatalf("a tree that matches what shipped reported %v", problems)
		}
	})

	t.Run("a migration that has not shipped yet is free to change", func(t *testing.T) {
		onDisk := map[string]migration{
			"0002_x.sql": {version: 2, filename: "0002_x.sql", checksum: shipped},
			"0003_y.sql": {version: 3, filename: "0003_y.sql", checksum: "anything at all"},
		}
		table := map[int]string{2: shipped, 3: "anything at all"}

		if problems := compareAgainstAnchor(anchored, onDisk, table); len(problems) != 0 {
			t.Fatalf(`0003 is in no published release, so it is not frozen and must not fail here.
Getting this wrong makes every new migration unmergeable, which is a worse guard than none: %v`, problems)
		}
	})

	t.Run("a shipped migration deleted from the tree", func(t *testing.T) {
		problems := compareAgainstAnchor(anchored, map[string]migration{}, map[int]string{})
		if len(problems) != 1 {
			t.Fatalf("got %d problems, want 1: %v", len(problems), problems)
		}
		if !strings.Contains(problems[0], "is not in this tree at all") {
			t.Errorf("a withdrawn shipped migration was reported as %q", problems[0])
		}
	})

	t.Run("a shipped migration with no row in the table", func(t *testing.T) {
		onDisk := map[string]migration{"0002_x.sql": {version: 2, filename: "0002_x.sql", checksum: shipped}}
		problems := compareAgainstAnchor(anchored, onDisk, map[int]string{})
		if len(problems) != 1 {
			t.Fatalf("got %d problems, want 1: %v", len(problems), problems)
		}
		if !strings.Contains(problems[0], "no row in shippedMigrationChecksums") {
			t.Errorf("a shipped migration missing from the table was reported as %q", problems[0])
		}
	})
}

// TestCollectPublishedMigrations_ReadsTheBytesGitHasRatherThanTheTree is the
// other half of the control: that the anchor really does come out of git, and
// really is unaffected by the working tree.
//
// It is a separate fixture repository rather than the real one because the
// real one cannot be made to hold a diverging release without publishing one.
func TestCollectPublishedMigrations_ReadsTheBytesGitHasRatherThanTheTree(t *testing.T) {
	dir := newPublishedRepoFixture(t)

	t.Run("only what the published ref carries", func(t *testing.T) {
		refs, err := resolvePublishedRefs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) != 1 || refs[0].Ref != "origin/release" {
			t.Fatalf("resolved %v, want just origin/release", refNames(refs))
		}

		anchored, problems, err := collectPublishedMigrations(dir, refs)
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) != 0 {
			t.Fatalf("one published ref cannot disagree with itself, yet: %v", problems)
		}
		if len(anchored) != 2 {
			t.Fatalf("anchored %d files, want the 2 the release carries: %+v", len(anchored), anchored)
		}
		if anchored[0].Checksum != sha256Hex([]byte("shipped one\n")) {
			t.Errorf("0001's anchored checksum is not the sha256 of the bytes on the release branch")
		}
		if _, ok := findAnchored(anchored, "0003_unreleased.sql"); ok {
			t.Error("0003 is only on main, so anchoring it would freeze a migration that never shipped")
		}
	})

	t.Run("editing the working tree does not move the anchor", func(t *testing.T) {
		p := filepath.Join(dir, migrationsDirFromRepoRoot, "0001_init.sql")
		if err := os.WriteFile(p, []byte("edited in the working tree\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		refs, err := resolvePublishedRefs(dir)
		if err != nil {
			t.Fatal(err)
		}
		anchored, _, err := collectPublishedMigrations(dir, refs)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := findAnchored(anchored, "0001_init.sql")
		if !ok {
			t.Fatal("0001 stopped being anchored when the working copy changed")
		}
		if got.Checksum != sha256Hex([]byte("shipped one\n")) {
			t.Fatalf(`the anchor followed the working tree, so it anchors nothing.
This is the whole property: the published bytes are outside the tree and an edit here cannot reach them.`)
		}
	})

	t.Run("two releases disagreeing about one file is itself a failure", func(t *testing.T) {
		gitFixture(t, dir, "checkout", "-q", "-b", "old-release", "origin/release")
		writeFixtureFile(t, dir, "0001_init.sql", "a different one\n")
		gitFixture(t, dir, "add", ".")
		gitFixture(t, dir, "commit", "-qm", "an older release with different bytes")
		gitFixture(t, dir, "tag", "v0.0.1")

		refs, err := resolvePublishedRefs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) != 2 {
			t.Fatalf("resolved %v, want origin/release and the v0.0.1 tag", refNames(refs))
		}
		_, problems, err := collectPublishedMigrations(dir, refs)
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) != 1 || !strings.Contains(problems[0], "different bytes by different releases") {
			t.Fatalf("two releases publishing different bytes for 0001 was reported as %v", problems)
		}
	})
}

// TestResolvePublishedRefs_WithoutThePublishBranchTheAnchorIsNotAtFullStrength
// pins the answer to "what happens with no git, a shallow clone, or no tags".
//
// Both arms end at the same place, because hasPublishBranch is what
// TestShippedMigrationsMatchWhatWasPublished turns into a t.Fatalf. A clone
// that cannot ask the question at full strength must not report the same green
// as one that can, and a passing test's t.Logf is not a report: without -v
// nobody sees it.
func TestResolvePublishedRefs_WithoutThePublishBranchTheAnchorIsNotAtFullStrength(t *testing.T) {
	dir := t.TempDir()
	gitFixture(t, dir, "init", "-q", "-b", "main")
	gitFixture(t, dir, "config", "user.email", "fixture@example.invalid")
	gitFixture(t, dir, "config", "user.name", "fixture")
	writeFixtureFile(t, dir, "0001_init.sql", "one\n")
	gitFixture(t, dir, "add", ".")
	gitFixture(t, dir, "commit", "-qm", "only main")

	t.Run("a repository that has published nothing anchors nothing", func(t *testing.T) {
		refs, err := resolvePublishedRefs(dir)
		if err != nil {
			t.Fatalf("a repository with no release ref and no tags is a normal thing to be asked about: %v", err)
		}
		if len(refs) != 0 {
			t.Fatalf("resolved %v in a repository that has published nothing", refNames(refs))
		}
		if hasPublishBranch(refs) {
			t.Fatal("claimed the publish branch in a repository that has no release ref")
		}
	})

	t.Run("a tag on its own is not the publish branch", func(t *testing.T) {
		gitFixture(t, dir, "tag", "v9.9.9")
		refs, err := resolvePublishedRefs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) != 1 {
			t.Fatalf("resolved %v, want just the tag", refNames(refs))
		}
		if hasPublishBranch(refs) {
			t.Fatalf(`a tag was accepted as the publish branch.
That is the fallback that reads like the full check: this repository tagged v0.1.0 and never
tagged 0.2.0, so tags alone anchor an arbitrary subset of what has actually shipped.`)
		}
	})

	t.Run("being outside a repository is an error, never an empty pass", func(t *testing.T) {
		if _, err := runGit(t.TempDir(), "rev-parse", "--show-toplevel"); err == nil {
			t.Fatal("git answered from a directory that is not a repository")
		}
	})
}

// newPublishedRepoFixture builds a repository shaped like this one: a release
// branch carrying two migrations, a main branch that has moved past it with a
// third, and a working tree that matches main.
func newPublishedRepoFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitFixture(t, dir, "init", "-q", "-b", "main")
	gitFixture(t, dir, "config", "user.email", "fixture@example.invalid")
	gitFixture(t, dir, "config", "user.name", "fixture")

	writeFixtureFile(t, dir, "0001_init.sql", "shipped one\n")
	writeFixtureFile(t, dir, "0002_two.sql", "shipped two\n")
	gitFixture(t, dir, "add", ".")
	gitFixture(t, dir, "commit", "-qm", "the release cut")
	released := strings.TrimSpace(string(gitFixture(t, dir, "rev-parse", "HEAD")))
	gitFixture(t, dir, "update-ref", "refs/remotes/origin/release", released)

	writeFixtureFile(t, dir, "0003_unreleased.sql", "not shipped yet\n")
	gitFixture(t, dir, "add", ".")
	gitFixture(t, dir, "commit", "-qm", "a migration added since the cut")
	return dir
}

func writeFixtureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, migrationsDirFromRepoRoot, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitFixture(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	out, err := runGit(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

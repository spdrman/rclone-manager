package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file is issue #174's anti-drift check.
//
// The bug it exists for: container/release-manifest.json pinned commit
// c51a07f, which was a feature-branch commit that GitHub's squash merge
// rewrote out of main's history. Nothing noticed. Every parity check
// stated in terms of "matches the release manifest" was therefore
// comparing built bytes against a build no checkout could reproduce, and
// #85's spkctl verify, #84's image digest parity and #164's UGOS digest
// checks were all pinned to that fiction.
//
// Of the three options the issue weighs, this is the first: a check that
// fails when the manifest's commit is not an ancestor of HEAD. The other
// two, generating the manifest inside the release build and moving it out
// of the repository into a release artifact store, both need a release
// pipeline that does not exist yet, and both belong to #88 (B5.2, supply
// chain and release provenance) rather than here.
//
// It is deliberately NOT only the release-manifest-integrity cell in
// conformance.json, even though that cell asks the same question and asks
// it correctly. That cell's verdict is mediated by a declaration, and a
// declaration is exactly the mechanism by which a gate stops being a
// gate: the row spent this whole phase declared "blocked", which is not a
// failure, and nothing red ever appeared. This test cannot be declared
// anything. It runs, or the package does not build.
//
// The other half of the fix is upstream of this check, in
// scripts/release/record-release-hashes.sh: an ancestry check on HEAD
// only notices after the squash merge has already happened, so the
// generator now refuses to record a commit that is not already an
// ancestor of origin/main. This is the net; that is the reason the net
// stays empty.

// TestReleaseManifestPinsACommitThisHistoryCanReach is the assertion
// itself, against the real manifest and the real repository.
func TestReleaseManifestPinsACommitThisHistoryCanReach(t *testing.T) {
	m, err := ReadReleaseManifest()
	if err != nil {
		t.Fatalf("cannot read container/release-manifest.json: %v", err)
	}
	if m.Commit == "" {
		t.Fatal("container/release-manifest.json pins no commit at all, so nothing it records can be tied to a build")
	}

	reachable, err := CommitReachableFrom(Path("."), m.Commit, "HEAD")
	if err != nil {
		t.Fatalf("git could not decide whether %s is an ancestor of HEAD, so this check did not run: %v", m.Commit, err)
	}
	if !reachable {
		t.Fatalf(`container/release-manifest.json pins commit %s, which is not an ancestor of HEAD.

Its binary_sha256 values therefore describe a build nobody can reproduce
from this history, and every parity check that compares against them is
comparing against nothing. The usual cause is that the manifest was
generated on a feature branch and the branch was squash merged, which
rewrites the commit.

Fix it by regenerating from a clean checkout of a commit that is already
on main:

    scripts/release/record-release-hashes.sh

which refuses to record an unreachable commit in the first place.`, m.Commit)
	}
}

// TestCommitReachableFromModelsASquashMerge is the positive control.
//
// The assertion above is a negative one ("this never happens"), and a
// negative assertion that has never been seen to fail is indistinguishable
// from a check that cannot fail. So this builds the exact shape of the
// bug in a throwaway repository: a commit on a side branch, and a
// separately created commit on main carrying the same tree, which is what
// a squash merge leaves behind. The side-branch commit must come back
// unreachable, the main-line one must come back reachable, and a
// well-formed SHA git has never heard of must come back as an error
// rather than as a quiet "no".
//
// That third case is not padding. "The manifest is wrong" and "the check
// could not run" are different facts, and the whole failure this issue
// documents is what happens when a check reports something other than
// what it measured.
func TestCommitReachableFromModelsASquashMerge(t *testing.T) {
	fx := newSquashMergeFixture(t)

	cases := []struct {
		name          string
		commit        string
		wantReachable bool
		wantErr       bool
	}{
		{"the squash-merged commit main actually has", fx.squashed, true, false},
		{"the base commit main descends from", fx.base, true, false},
		{"the branch commit the squash merge rewrote away", fx.feature, false, false},
		{"a well-formed SHA that is not an object here", unknownSHA, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CommitReachableFrom(fx.dir, tc.commit, "main")
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("reported reachable=%v with no error; an undecidable case has to say so rather than pass for a plain no", got)
			case !tc.wantErr && err != nil:
				t.Fatalf("reported an error for a case git can decide: %v", err)
			}
			if got != tc.wantReachable {
				t.Errorf("reachable=%v, want %v", got, tc.wantReachable)
			}
		})
	}
}

// unknownSHA is well formed and is not an object in any repository these
// tests build, so git exits non-zero for a reason that is not "no".
const unknownSHA = "0123456789abcdef0123456789abcdef01234567"

// squashMergeFixture is a throwaway repository in the exact shape that
// produced #174: a feature commit that only the branch ever had, and a
// separate main-line commit carrying the same tree, which is what
// GitHub's squash merge leaves behind. Its HEAD is main.
type squashMergeFixture struct {
	dir                     string
	base, feature, squashed string
}

func newSquashMergeFixture(t *testing.T) squashMergeFixture {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init", "-q", "-b", "main")
	write("a.txt", "base\n")
	git("add", ".")
	git("commit", "-qm", "base")
	fx := squashMergeFixture{dir: dir, base: git("rev-parse", "HEAD")}

	git("checkout", "-q", "-b", "feature")
	write("a.txt", "feature\n")
	git("add", ".")
	git("commit", "-qm", "feature work")
	fx.feature = git("rev-parse", "HEAD")

	git("checkout", "-q", "main")
	git("merge", "-q", "--squash", "feature")
	git("commit", "-qm", "feature work (#1)")
	fx.squashed = git("rev-parse", "HEAD")

	if fx.feature == fx.squashed {
		t.Fatal("the squash produced the same SHA as the branch commit, so this fixture does not model the bug")
	}
	return fx
}

// TestReleaseManifestRegistryDigestTracksTheCanonicalPublishFlag closes
// the second half of the file's old note, the one that admitted
// local_image_id_sha256 is a local Docker image ID and not a registry
// digest "because no registry is configured for this repository yet".
//
// That premise is no longer true. The registry is settled: ghcr.io, and
// ghcr.io/spdrman/backup-manager, which canonical.json already carries.
// What is still true is that nothing has been pushed there, which
// canonical.json records as image.published false. So the manifest now
// carries an explicit registry_digest slot per architecture, null while
// nothing is published, and this test makes the two statements move
// together instead of drifting into the pair of half-truths the issue
// objected to. The day a release is actually pushed and published flips
// to true, this test starts demanding the digests, unprompted.
func TestReleaseManifestRegistryDigestTracksTheCanonicalPublishFlag(t *testing.T) {
	m, err := ReadReleaseManifest()
	if err != nil {
		t.Fatalf("cannot read container/release-manifest.json: %v", err)
	}
	c := MustLoad()

	for _, a := range m.Architectures {
		switch {
		case !c.Image.Published:
			if a.RegistryDigest != nil {
				t.Errorf("%s records registry_digest %q while canonical.json says image.published is false; one of the two is lying about whether %s exists in a registry",
					a.Architecture, *a.RegistryDigest, c.Image.Reference)
			}
		case a.RegistryDigest == nil:
			t.Errorf("canonical.json says %s is published, and %s records no registry_digest, so the manifest still identifies the image only by a local Docker image ID nobody else can resolve",
				c.Image.Reference, a.Architecture)
		case !strings.HasPrefix(*a.RegistryDigest, "sha256:"):
			t.Errorf("%s records registry_digest %q, which is not a sha256: digest; a registry digest is what `docker buildx imagetools inspect %s` prints, not a local image ID",
				a.Architecture, *a.RegistryDigest, c.Image.Reference)
		}
	}
}

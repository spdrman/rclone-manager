package packaging

import (
	"fmt"
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
	if m.UnsafeLocalBuild {
		t.Fatal(`container/release-manifest.json is stamped "unsafe_local_build": true.

That stamp is only written by a run of scripts/release/record-release-hashes.sh
with UNSAFE_LOCAL_BUILD=1, which waives every guard that makes a manifest
reproducible: the recorded commit need not be HEAD, the tree it was built from
may have been dirty, and the commit need not be on main. A waived manifest is
otherwise indistinguishable from a good one, so it is refused here rather than
trusted. Regenerate without the waiver from a clean checkout of a commit that is
already on main.`)
	}

	ancestry, err := ResolveAncestryRef(Path("."))
	if err != nil {
		t.Fatalf("no ref to check %s against, so this check did not run: %v", m.Commit, err)
	}
	reachable, err := CommitReachableFrom(Path("."), m.Commit, ancestry.Ref)
	if err != nil {
		t.Fatalf("git could not decide whether %s is an ancestor of %s, so this check did not run: %v", m.Commit, ancestry.Ref, err)
	}
	if !reachable {
		t.Fatalf(`container/release-manifest.json pins commit %s, which is not an ancestor of %s (%s).

Its binary_sha256 values therefore describe a build nobody can reproduce
from this history, and every parity check that compares against them is
comparing against nothing. The usual cause is that the manifest was
generated on a feature branch and the branch was squash merged, which
rewrites the commit.

Fix it by regenerating from a clean checkout of a commit that is already
on main:

    scripts/release/record-release-hashes.sh

which refuses to record an unreachable commit in the first place.`, m.Commit, ancestry.Ref, ancestry.Why)
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

// gitIn runs git in a throwaway repository and returns its output,
// failing the test if git does. It is shared so a test can drive the
// fixture further (checking out a branch, planting a remote ref) with
// the same isolated environment the fixture was built with.
func gitIn(t *testing.T, dir string, args ...string) string {
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

func newSquashMergeFixture(t *testing.T) squashMergeFixture {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		return gitIn(t, dir, args...)
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

	for _, complaint := range registryDigestComplaints(c.Image.Reference, c.Image.Published, m.Architectures) {
		t.Error(complaint)
	}
}

// registryDigestComplaints says every way a recorded set of
// architectures disagrees with canonical.json's image.published flag.
//
// It is a function rather than a loop inside the test above for the
// reason releaseManifestIntegrity is one: against the real file only a
// single arm can run. image.published is false and every registry_digest
// is null, so the two arms carrying the actual contract, published with
// no digest and a digest that is not a sha256: value, have never
// executed and cannot until the day of the first real push. That day is
// the worst moment to find out that a field name does not unmarshal or
// that an assertion was written backwards. The table test below runs all
// of them today.
//
// The empty case is not padding either. The whole guard is a loop over
// the architectures, so a manifest recording none satisfies it by having
// nothing to iterate, and the coupling then rests on an invariant
// asserted in a different file.
//
// Schema note: registry_digest sits inside each architecture entry while
// `docker buildx build --push` prints one image index digest for the
// whole multi-arch image, so a top-level slot may be added or may
// replace this one (#88). This table is pinned to the shape the manifest
// carries today, and moves with it.
func registryDigestComplaints(reference string, published bool, arches []ReleaseArchitecture) []string {
	if len(arches) == 0 {
		return []string{"the release manifest records no architecture at all, so the registry-digest guard has nothing to check and passes by default"}
	}
	var complaints []string
	for _, a := range arches {
		switch {
		case !published:
			if a.RegistryDigest != nil {
				complaints = append(complaints, fmt.Sprintf("%s records registry_digest %q while canonical.json says image.published is false; one of the two is lying about whether %s exists in a registry",
					a.Architecture, *a.RegistryDigest, reference))
			}
		case a.RegistryDigest == nil:
			complaints = append(complaints, fmt.Sprintf("canonical.json says %s is published, and %s records no registry_digest, so the manifest still identifies the image only by a local Docker image ID nobody else can resolve",
				reference, a.Architecture))
		case !strings.HasPrefix(*a.RegistryDigest, "sha256:"):
			complaints = append(complaints, fmt.Sprintf("%s records registry_digest %q, which is not a sha256: digest; a registry digest is what `docker buildx imagetools inspect %s` prints, not a local image ID",
				a.Architecture, *a.RegistryDigest, reference))
		}
	}
	return complaints
}

// TestRegistryDigestComplaints_CoversEveryCombination runs the arms the
// real file cannot reach, including the one the day of the first push
// depends on.
func TestRegistryDigestComplaints_CoversEveryCombination(t *testing.T) {
	digest := func(v string) *string { return &v }
	arch := func(name string, d *string) []ReleaseArchitecture {
		return []ReleaseArchitecture{{Architecture: name, RegistryDigest: d}}
	}

	cases := []struct {
		name      string
		published bool
		arches    []ReleaseArchitecture
		want      string // a substring the single complaint must carry, or "" for no complaint
	}{
		{"nothing published and no digest recorded, which is today", false, arch("amd64", nil), ""},
		{"nothing published but a digest appeared anyway", false, arch("amd64", digest("sha256:"+strings.Repeat("a", 64))), "image.published is false"},
		{"published with no digest at all", true, arch("amd64", nil), "records no registry_digest"},
		{"published with a local image ID where a digest belongs", true, arch("amd64", digest(strings.Repeat("b", 64))), "which is not a sha256: digest"},
		{"published with a real digest", true, arch("amd64", digest("sha256:"+strings.Repeat("c", 64))), ""},
		{"no architectures at all, unpublished", false, nil, "records no architecture at all"},
		{"no architectures at all, published", true, []ReleaseArchitecture{}, "records no architecture at all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := registryDigestComplaints("ghcr.io/spdrman/backup-manager", tc.published, tc.arches)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("expected no complaint, got %v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("expected exactly one complaint containing %q, got %v", tc.want, got)
			}
			if !strings.Contains(got[0], tc.want) {
				t.Errorf("complaint does not say why: got %q, want it to contain %q", got[0], tc.want)
			}
		})
	}

	// Every architecture is judged, not just the first: a manifest whose
	// second entry is the broken one has to complain about that entry.
	got := registryDigestComplaints("ghcr.io/spdrman/backup-manager", true, []ReleaseArchitecture{
		{Architecture: "amd64", RegistryDigest: digest("sha256:" + strings.Repeat("d", 64))},
		{Architecture: "arm64", RegistryDigest: nil},
	})
	if len(got) != 1 || !strings.Contains(got[0], "arm64") {
		t.Errorf("expected one complaint naming arm64, got %v", got)
	}
}

// TestReleaseManifestIntegrity_RefusesACommitOnlyThisBranchHas is the
// control M2 was missing.
//
// The generator refuses a commit that is not an ancestor of origin/main;
// the checks here used to ask only whether it was an ancestor of HEAD.
// On a feature branch those are different questions, and the weaker one
// passes right up until the squash merge that makes it false. So this
// puts HEAD on the feature branch, proves the old question would have
// said yes, and requires the check to say no anyway and to name the ref
// that answered.
func TestReleaseManifestIntegrity_RefusesACommitOnlyThisBranchHas(t *testing.T) {
	conf := MustLoadConformance()
	p := providerUnderTest{id: "generic", spec: conf.Providers["generic"], canonical: MustLoad()}
	fx := newSquashMergeFixture(t)
	gitIn(t, fx.dir, "checkout", "-q", "feature")

	manifest := fixtureManifest(p, fx.feature)

	// The positive control, and it is the point of the test: asked the
	// weak question, this manifest passes.
	reachableFromHEAD, err := CommitReachableFrom(fx.dir, fx.feature, "HEAD")
	if err != nil {
		t.Fatalf("git could not decide the control question: %v", err)
	}
	if !reachableFromHEAD {
		t.Fatal("the fixture does not model the gap: the feature commit is supposed to be reachable from HEAD while it is checked out")
	}

	ok, detail := releaseManifestIntegrity(p, manifest, fx.dir)
	if ok {
		t.Fatalf("a manifest pinning a commit only this branch has must be refused before the squash merge, not after it: %s", detail)
	}
	if !strings.Contains(detail, "is not an ancestor of main") {
		t.Errorf("the refusal has to name the ref that answered, so a fallback cannot pass for the full-strength check: %s", detail)
	}

	// And the same repository still accepts the commit main really has,
	// so the refusal above is about reachability and not about the
	// branch being checked out.
	if ok, detail := releaseManifestIntegrity(p, fixtureManifest(p, fx.squashed), fx.dir); !ok {
		t.Fatalf("the squash-merged commit must still pass from the same checkout: %s", detail)
	}
}

// TestResolveAncestryRef_PrefersTheStrongestRefThisCheckoutHas pins the
// fallback order, since which ref answers decides how strong the check
// is.
func TestResolveAncestryRef_PrefersTheStrongestRefThisCheckoutHas(t *testing.T) {
	fx := newSquashMergeFixture(t)

	// No origin/main in a fresh fixture, so the local main answers.
	got, err := ResolveAncestryRef(fx.dir)
	if err != nil {
		t.Fatalf("a repository with a main branch must resolve something: %v", err)
	}
	if got.Ref != "main" {
		t.Errorf("resolved %q, want main", got.Ref)
	}
	if !strings.Contains(got.Why, "origin/main is not in this checkout") {
		t.Errorf("a fallback has to say it is one: %q", got.Why)
	}

	// Plant a remote-tracking ref and it wins.
	gitIn(t, fx.dir, "update-ref", "refs/remotes/origin/main", fx.squashed)
	got, err = ResolveAncestryRef(fx.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != "origin/main" {
		t.Errorf("resolved %q, want origin/main once it exists", got.Ref)
	}

	// With neither, HEAD is all that is left, and it says so.
	gitIn(t, fx.dir, "checkout", "-q", "--detach")
	gitIn(t, fx.dir, "update-ref", "-d", "refs/remotes/origin/main")
	gitIn(t, fx.dir, "branch", "-D", "main")
	gitIn(t, fx.dir, "branch", "-D", "feature")
	got, err = ResolveAncestryRef(fx.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != "HEAD" {
		t.Errorf("resolved %q, want HEAD as the last resort", got.Ref)
	}
	if !strings.Contains(got.Why, "weakest form") {
		t.Errorf("the weakest question has to admit it is the weakest: %q", got.Why)
	}

	// And a directory that is not a repository resolves nothing rather
	// than quietly answering about somewhere else.
	if _, err := ResolveAncestryRef(t.TempDir()); err == nil {
		t.Error("a directory with no repository must be an error, not a ref")
	}
}

// TestReleaseManifest_RefusesAWaivedGeneratorRun is the other half of
// UNSAFE_LOCAL_BUILD=1 being self-reporting: the stamp is worth nothing
// unless something refuses it.
func TestReleaseManifest_RefusesAWaivedGeneratorRun(t *testing.T) {
	// The field has to unmarshal from the key the shell actually writes,
	// which is the part a hand-written struct tag gets wrong silently.
	stamped, err := ParseReleaseManifest([]byte(`{"commit":"abc","unsafe_local_build":true,"architectures":[]}`))
	if err != nil {
		t.Fatalf("cannot parse a stamped manifest: %v", err)
	}
	if !stamped.UnsafeLocalBuild {
		t.Fatal(`"unsafe_local_build": true did not reach the struct, so nothing downstream can refuse it`)
	}
	clean, err := ParseReleaseManifest([]byte(`{"commit":"abc","architectures":[]}`))
	if err != nil {
		t.Fatalf("cannot parse an unstamped manifest: %v", err)
	}
	if clean.UnsafeLocalBuild {
		t.Fatal("a manifest with no stamp must read as safe, or every honest run is refused")
	}

	conf := MustLoadConformance()
	p := providerUnderTest{id: "generic", spec: conf.Providers["generic"], canonical: MustLoad()}
	fx := newSquashMergeFixture(t)

	// Positive control first: the same manifest, unstamped, passes.
	good := fixtureManifest(p, fx.squashed)
	if ok, detail := releaseManifestIntegrity(p, good, fx.dir); !ok {
		t.Fatalf("the unstamped manifest must pass, or the refusal below proves nothing: %s", detail)
	}

	waived := good
	waived.UnsafeLocalBuild = true
	ok, detail := releaseManifestIntegrity(p, waived, fx.dir)
	if ok {
		t.Fatal("a manifest generated with every guard waived must not be trusted just because the commit it pins happens to be reachable")
	}
	if !strings.Contains(detail, "unsafe_local_build") {
		t.Errorf("the refusal has to name the stamp so the operator knows what to regenerate: %s", detail)
	}
}

// fixtureManifest builds a manifest that satisfies every other arm of
// releaseManifestIntegrity, so a test can change one thing and know that
// is what the verdict turned on.
func fixtureManifest(p providerUnderTest, commit string) ReleaseManifest {
	arches := make([]ReleaseArchitecture, 0, len(p.canonical.Architectures))
	for _, arch := range p.canonical.Architectures {
		hashes := map[string]string{}
		for _, b := range p.canonical.Binaries {
			hashes[strings.TrimPrefix(b, "/")] = strings.Repeat("a", 64)
		}
		arches = append(arches, ReleaseArchitecture{Architecture: arch, BinarySHA256: hashes})
	}
	return ReleaseManifest{Commit: commit, Architectures: arches}
}

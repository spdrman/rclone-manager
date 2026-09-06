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

	// Every rewrite-free ref this checkout has, not just the first one
	// that exists (#258). #174's rule was never about main specifically:
	// it was that a commit pinned to a branch that does not rewrite stays
	// checkable. `release` has that property by policy, and a release cut
	// carries the pipeline change that publishes it, so the commit a first
	// release is pinned to is on `release` before it is on main.
	ancestry, reachable, err := ResolveReachableAncestryRef(Path("."), m.Commit)
	if err != nil {
		t.Fatalf("no ref could decide whether %s is reachable, so this check did not run: %v", m.Commit, err)
	}
	if !reachable {
		t.Fatalf(`container/release-manifest.json pins commit %s, which no rewrite-free ref in this checkout can reach (%s).

Its binary_sha256 values therefore describe a build nobody can reproduce
from this history, and every parity check that compares against them is
comparing against nothing. The usual cause is that the manifest was
generated on a feature branch and the branch was squash merged, which
rewrites the commit.

Fix it by regenerating from a clean checkout of a commit that is already
on main:

    scripts/release/record-release-hashes.sh

which refuses to record an unreachable commit in the first place.`, m.Commit, ancestry.Why)
	}
	t.Logf("%s is reachable from %s (%s)", m.Commit, ancestry.Ref, ancestry.Why)
}

// TestResolveReachableAncestryRef_AsksEveryRewriteFreeRef is the control
// for the change above.
//
// The arm that matters is the third: a commit that origin/main cannot
// reach and origin/release can. Under the old single-ref resolution
// origin/main existed, so it was the only ref asked, and the answer was
// a flat no. That is the shape a first release actually has, because the
// release cut carries the pipeline change that publishes it, so getting
// this wrong does not fail loudly at review time. It fails by making the
// first release impossible and looking like #174 while it does.
func TestResolveReachableAncestryRef_AsksEveryRewriteFreeRef(t *testing.T) {
	fx := newSquashMergeFixture(t)
	gitIn(t, fx.dir, "update-ref", "refs/remotes/origin/main", fx.squashed)

	// A commit that is on neither main nor release.
	gitIn(t, fx.dir, "checkout", "-q", "-b", "cut", fx.squashed)
	if err := os.WriteFile(filepath.Join(fx.dir, "cut.txt"), []byte("release cut\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, fx.dir, "add", ".")
	gitIn(t, fx.dir, "commit", "-qm", "release cut")
	cut := gitIn(t, fx.dir, "rev-parse", "HEAD")

	t.Run("a commit on main is reached by origin/main", func(t *testing.T) {
		ref, reachable, err := ResolveReachableAncestryRef(fx.dir, fx.squashed)
		if err != nil || !reachable {
			t.Fatalf("reachable=%v err=%v, want reachable with no error", reachable, err)
		}
		if ref.Ref != "origin/main" {
			t.Errorf("answered by %q, want origin/main to answer first", ref.Ref)
		}
	})

	t.Run("origin/release answers for a commit origin/main cannot reach", func(t *testing.T) {
		gitIn(t, fx.dir, "update-ref", "refs/remotes/origin/release", cut)
		ref, reachable, err := ResolveReachableAncestryRef(fx.dir, cut)
		if err != nil || !reachable {
			t.Fatalf("reachable=%v err=%v, want reachable with no error", reachable, err)
		}
		if ref.Ref != "origin/release" {
			t.Fatalf("answered by %q, want origin/release: origin/main exists and cannot reach this commit, so a resolver that stops at the first ref that merely EXISTS refuses a legitimate release cut", ref.Ref)
		}
		if !strings.Contains(ref.Why, "append-only") {
			t.Errorf("the reason has to say why a release commit stays checkable, so a reader can tell this is not a weakened check: %q", ref.Why)
		}
	})

	t.Run("an object nothing has is undecidable, never a no", func(t *testing.T) {
		_, reachable, err := ResolveReachableAncestryRef(fx.dir, unknownSHA)
		if err == nil {
			t.Fatalf("reachable=%v with no error; git failing to answer must not be reported as the manifest being wrong", reachable)
		}
	})
}

// TestClassifyUnreachedOutcome_OneUndecidedRefIsNeverSwallowedByANo is the
// control for the mixed case the fixture above cannot reach: it would need
// a repository where merge-base fails for exactly one ref in the
// preference list and succeeds for the rest, which is not a shape git
// fixtures can be made to produce reliably. The decision logic that
// matters is pure, though, so it is asserted directly against the two
// slices ResolveReachableAncestryRef's loop would have produced.
//
// Before this test, the code only refused to answer when EVERY asked ref
// was undecided. A checkout where origin/main and main both cleanly say
// "not reachable" and origin/release alone cannot decide (a shallow
// fetch, a missing object on that one ref's path) fell through to a flat
// "not reachable", which is exactly the false negative the three-outcome
// contract above the function says must never happen: origin/release not
// deciding is not evidence that it would have said no.
func TestClassifyUnreachedOutcome_OneUndecidedRefIsNeverSwallowedByANo(t *testing.T) {
	asked := []string{"origin/main", "main", "origin/release"}
	undecided := []string{"origin/release"}

	_, reachable, err := classifyUnreachedOutcome("/repo", "deadbeef", asked, undecided)
	if err == nil {
		t.Fatalf("reachable=%v with no error; one ref that could not decide must not be swallowed by the others answering no", reachable)
	}
	if !strings.Contains(err.Error(), "origin/release") {
		t.Errorf("error %q does not name the ref that could not decide", err.Error())
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
// That premise is no longer true, on both halves of the manifest this
// test holds together. The registry is settled: ghcr.io, and
// ghcr.io/spdrman/backup-manager, which canonical.json already carries.
// And as of the 0.1.0 push, canonical.json records image.published true,
// so this test now demands what it used to only promise it would: a real
// registry_digest per architecture, and a real top-level index_digest for
// the multi-architecture index those manifests sit under. Never
// separately: a published flag with a digest missing anywhere in that set
// is exactly the pair of half-truths the issue objected to.
func TestReleaseManifestRegistryDigestTracksTheCanonicalPublishFlag(t *testing.T) {
	m, err := ReadReleaseManifest()
	if err != nil {
		t.Fatalf("cannot read container/release-manifest.json: %v", err)
	}
	c := MustLoad()

	for _, complaint := range registryDigestComplaints(c.Image.Reference, c.Image.Published, m.IndexDigest, m.Architectures) {
		t.Error(complaint)
	}
}

// registryDigestComplaints says every way a recorded set of
// architectures, and the top-level index digest they sit under, disagree
// with canonical.json's image.published flag.
//
// It is a function rather than a loop inside the test above for the
// reason releaseManifestIntegrity is one: against the real file only a
// single arm can run at a time. The table test below runs every arm,
// including the ones a real push has already retired (an unpublished
// manifest carrying a digest anyway) and cannot exercise again against
// the real file.
//
// The empty case is not padding either. The whole guard is a loop over
// the architectures, so a manifest recording none satisfies it by having
// nothing to iterate, and the coupling then rests on an invariant
// asserted in a different file. It is checked before the index digest
// too: a manifest with no architectures at all is a different failure
// than a missing index digest, and RecordsEveryBinary is where that one
// is caught.
func registryDigestComplaints(reference string, published bool, indexDigest *string, arches []ReleaseArchitecture) []string {
	if len(arches) == 0 {
		return []string{"the release manifest records no architecture at all, so the registry-digest guard has nothing to check and passes by default"}
	}
	var complaints []string
	switch {
	case !published:
		if indexDigest != nil {
			complaints = append(complaints, fmt.Sprintf("the manifest records index_digest %q while canonical.json says image.published is false; one of the two is lying about whether %s exists in a registry",
				*indexDigest, reference))
		}
	case indexDigest == nil:
		complaints = append(complaints, fmt.Sprintf("canonical.json says %s is published, and the manifest records no index_digest, so the multi-architecture image cosign signed and attested an SBOM for is not identified by anything a verifier can pin",
			reference))
	case !strings.HasPrefix(*indexDigest, "sha256:"):
		complaints = append(complaints, fmt.Sprintf("the manifest records index_digest %q, which is not a sha256: digest; an index digest is what `docker buildx imagetools inspect %s` prints for the multi-arch manifest, not a local image ID",
			*indexDigest, reference))
	}
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
	// A valid index digest and a valid per-architecture digest, so a case
	// aimed at the other half of the manifest does not also trip this
	// one and turn a one-complaint case into two.
	validIndex := digest("sha256:" + strings.Repeat("e", 64))
	validArchOnly := arch("amd64", digest("sha256:"+strings.Repeat("c", 64)))

	cases := []struct {
		name        string
		published   bool
		indexDigest *string
		arches      []ReleaseArchitecture
		want        string // a substring the single complaint must carry, or "" for no complaint
	}{
		{"nothing published and no digest recorded, which is today", false, nil, arch("amd64", nil), ""},
		{"nothing published but a digest appeared anyway", false, nil, arch("amd64", digest("sha256:"+strings.Repeat("a", 64))), "image.published is false"},
		{"published with no digest at all", true, validIndex, arch("amd64", nil), "records no registry_digest"},
		{"published with a local image ID where a digest belongs", true, validIndex, arch("amd64", digest(strings.Repeat("b", 64))), "which is not a sha256: digest"},
		{"published with a real digest", true, validIndex, arch("amd64", digest("sha256:"+strings.Repeat("c", 64))), ""},
		{"no architectures at all, unpublished", false, nil, nil, "records no architecture at all"},
		{"no architectures at all, published", true, validIndex, []ReleaseArchitecture{}, "records no architecture at all"},
		{"nothing published but an index digest appeared anyway", false, digest("sha256:" + strings.Repeat("a", 64)), arch("amd64", nil), "image.published is false"},
		{"published with no index digest at all", true, nil, validArchOnly, "records no index_digest"},
		{"published with a local image ID where an index digest belongs", true, digest(strings.Repeat("b", 64)), validArchOnly, "which is not a sha256: digest"},
		{"published with a real index digest", true, validIndex, validArchOnly, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := registryDigestComplaints("ghcr.io/spdrman/backup-manager", tc.published, tc.indexDigest, tc.arches)
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
	got := registryDigestComplaints("ghcr.io/spdrman/backup-manager", true, validIndex, []ReleaseArchitecture{
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

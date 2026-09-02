# The `release` branch

Issue #258. This is the policy the rest of the release path is built on, so it is
worth reading before `.github/workflows/release.yml` or
`scripts/release/publish-image.sh`, both of which assume it.

## What it is

`release` is the publish branch. A push to it runs the release pipeline, and that
pipeline publishes: it pushes the canonical multi-architecture image to `ghcr.io`,
signs it keylessly and attests its SBOM. Nothing else in this repository triggers on
a push.

It is also the pointer at what has actually been published. `git checkout release`
gives you the tree the current release was cut from, and `container/release-manifest.json`
on it names the commit the shipped binaries were built from.

## Three rules

**1. Append-only.** Never force-pushed, never rebased, never squash-merged into.

This is the rule the other two exist to protect, and it is the direct answer to
issue #174. That issue was a manifest pinning `c51a07f`, a commit made on a feature
branch that GitHub's squash merge rewrote out of existence, leaving the manifest
naming a SHA nobody could check out. Every parity check stated as "matches the
release manifest" was comparing against a build that could not be reproduced.

The fix was an ancestry rule against `origin/main`, and `origin/main` works for that
purpose for one reason only: it does not rewrite. `release` is in the ancestry list
(`releaseAncestryRefPreference` in `distribution/packaging/matrix.go`) because this
rule gives it the same property. Break this rule and the manifest's reachability
check becomes a check against a moving target, which is worse than no check, because
it looks like one.

**2. Nothing is branched off it.** Work is cut from `main` and merged to `main`.
`release` is a destination, never a starting point.

**3. Only a release cut lands on it.** The version bump in
`distribution/packaging/canonical.json` and the adapter metadata `derive.go` holds to
it, the regenerated `container/release-manifest.json`, and the regenerated compliance
bundle under `provenance/`. Nothing else.

A commit on `release` that changes engine, adapter or UI source is a commit that
publishes something nobody reviewed on `main`, and the push trigger means it does so
without anyone typing anything. Rule 3 is what makes rule 1's audit trail worth
having: merging to `release` is a deliberate act with a reviewable diff, which is
what the pipeline's typed-confirmation input used to stand in for while a manual
dispatch was the only way in.

## What is enforced, and where

- `TestReleaseManifestPinsACommitThisHistoryCanReach` (`distribution/packaging`) fails
  the build when the committed manifest pins a commit no rewrite-free ref can reach.
  It asks `origin/main`, `origin/release`, the local `main` and `release`, and then
  `HEAD` as the weakest form, and it names which one answered so a fallback never
  reads as the full-strength check.
- `TestResolveReachableAncestryRef_AsksEveryRewriteFreeRef` is its control. It builds
  a commit `origin/main` cannot reach and `origin/release` can, which is the exact
  shape of a first release cut, and requires `origin/release` to answer.
- `scripts/release/publish-image.sh`'s guard 2b asks the same question again at
  publish time, because a registry tag is the one artifact that regenerating a file
  cannot correct.
- `scripts/release/verify-manifest-parity.sh` rebuilds each architecture and compares
  the extracted binaries' SHA-256 against the manifest, before the push. That is the
  half a SHA comparison cannot answer, and it is what replaced the guard that
  compared the manifest's commit to `HEAD` (#260).

## What is policy rather than code

Rules 1 and 2 are branch protection, not a test. A clone cannot observe that the
remote branch was force-pushed yesterday, so no check in this repository can enforce
them.

They are a GitHub ruleset instead, named "release branch is append-only", active,
targeting `refs/heads/release`, carrying `deletion`, `non_fast_forward`, and
`pull_request` (one required approval, merge commits only). `deletion` and
`non_fast_forward` block a force push and block deleting the branch. `pull_request`
is what makes rule 3 an enforced fact rather than a stated one: it blocks every direct
push to `release`, ordinary ones included, so the only way a commit lands there is
through a pull request somebody approved, and it restricts the merge method to a real
merge commit, so approving a PR can never become the squash or rebase merge rule 1
forbids.

That is a change from the branch's first weeks, when the ruleset carried only
`deletion` and `non_fast_forward` and an ordinary push to `release` was possible with
no diff reviewed by anyone: the redesign's safety argument ("merging is a stronger
act than typing a string") held in the doc but not in the ruleset. Cutting `v0.1.0`
happened under that gap; every cut after it goes through the pull request the
ruleset now requires.

What the tests above enforce is the consequence of the rules holding, which is the
useful half. If the ruleset were removed and the branch rewritten, the reachability
check starts failing, loudly, on the next run rather than months later.

## Cutting a release

1. Get the commit you want to publish onto `main`, green.
2. Regenerate the manifest from a clean checkout of that commit:
   `VERSION=<x.y.z> scripts/release/record-release-hashes.sh`. It refuses a commit
   that is not already on main, for #174's reason.
3. Bump the version in `distribution/packaging/canonical.json` and let `derive.go`
   tell you every adapter that has to follow.
4. Regenerate the compliance bundle: `(cd distribution && go run ./cmd/provenance -write)`.
5. Prove it before you publish it: dispatch the Release workflow with `publish: false`.
   That path runs every guard and the parity rebuild against the real tree and stops
   before the registry.
6. Open a pull request from `main` into `release` and get it approved. The ruleset
   refuses a direct push, so this is the only way in; merge it with a real merge
   commit (the ruleset refuses squash and rebase, which would break rule 1). Merging
   is what publishes.
7. Record the digests the run prints into the manifest, flip `image.published` to
   true, regenerate the bundle again and land it on `main`. The manifest test refuses
   a published flag without digests and digests without the flag, so the two cannot
   move separately.

## The first cut, and why it was different

`v0.1.0` could not follow step 1 exactly, and the reason is worth writing down rather
than hiding.

The pipeline that publishes a release is itself part of what `v0.1.0` shipped: the
push trigger (#259), the guard that made the publish script satisfiable at all
(#260), and this policy (#258). None of it was on `main` when the release was cut, so
the tree that published `v0.1.0` was a branch under review rather than `main`.

What was kept, because it is the part that matters: the manifest pins `13ed710`,
which is a commit on `main`, and the parity rebuild proves the published binaries are
that commit's binaries. So the released bytes are reproducible from `main` even
though the tree that pushed them was not yet. Every later cut follows step 1 as
written.

Step 7 also went slightly differently, and for a reason worth keeping. The digests
`ghcr.io` assigned are recorded on the pull request into `main` rather than pushed
straight back to `release`, because a push to `release` publishes: it would rebuild,
re-push bytes the registry already holds under the same digest, and add a second
Sigstore signature for nothing. So `release` records the tree that published, and
`main` records what the registry answered. The next cut fast-forwards past both.

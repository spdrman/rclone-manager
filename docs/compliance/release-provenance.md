# Release provenance, signing and the SBOM

Issue #88 (B5.2). What a release records, how it is produced, how a signature
is made without this project ever holding a key, and what is deliberately left
as an operator action.

`docs/EPIC-B-multi-nas.md` §61 is the requirement list; §73 Work Package 5.2 is
the compliance half. This document is the operator-facing side of both.

## The two halves of the release record, and why they are two files

`container/release-manifest.json` records what a two-architecture Docker build
produced: the commit, the build version, the SHA-256 of both shipped binaries
per architecture, the local image ID and the registry digest. Nothing goes in it
that a build did not produce. `scripts/release/record-release-hashes.sh` writes
it, and #174 gave that script five refusals that between them stop a manifest
being recorded against a commit nobody can check out.

`provenance/release-provenance.json` records everything derivable without a
build: the semantic version, the digest of every distributed artifact, the
licence, the notices, the inventory, the SBOM, the link verdict, the signing
record and the performance evidence. `distribution/cmd/provenance` writes it,
along with `NOTICE`, `provenance/third-party-licenses.json`,
`provenance/sbom.spdx.json` and `provenance/checksums.txt`.

The split follows one line: did a build produce this. Putting the compliance
fields into the manifest would mean either running a two-architecture Docker
build to change one string, or opening a no-build write path into the file that
carries the build record. The second is a hole in exactly the guard #174 put
there, so the record is two files and they are tied together by the manifest's
own SHA-256, recorded in the bundle. Regenerate one without the other and
`TestProvenanceBundleIsTiedToTheReleaseManifest` goes red.

## Generating the compliance artifacts

```
cd distribution
go run ./cmd/provenance          # check: exits 1 and names every stale file
go run ./cmd/provenance -write   # regenerate
```

Everything it writes is derived on the spot. The Go components come from
`go list -deps` run for `GOOS=linux` on every architecture `canonical.json`
claims, against both binaries `container/Dockerfile` copies into the runtime
stage. The frontend components come from `ui/shared/package-lock.json`, which is
what the build installs, so the answer is the same on a machine with no
`node_modules/`. The artifact digests come from the files.

`TestComplianceArtifactsMatchThisTree` re-derives all of it on every gate run
and fails on any difference, which is what makes an undeclared dependency a red
build rather than an omission nobody looks for. Determinism is a prerequisite
for that check rather than a nicety, which is why the SBOM's SPDX creation
timestamp is read out of the release manifest instead of off the clock.

## Signing: the key design

**There is no signing key in this repository, and there should never be one.**

Signing is keyless (Sigstore). In `.github/workflows/release.yml` the job's own
GitHub OIDC token is exchanged for a short-lived Fulcio certificate bound to the
workflow's identity, the signature and certificate are recorded in the Rekor
transparency log, and the certificate expires in minutes. Nothing long-lived
exists, so there is nothing to store, nothing to rotate, nothing to leak and
nothing to commit by mistake. The workflow needs `id-token: write` and
`packages: write`, and no secret beyond the token GitHub already provides.

The identity a verifier pins is settled before the first signature rather than
discovered after it. It is recorded in
`provenance/release-provenance.json` under `signing.identity`, and
`TestSigningRecordMatchesWhetherAnythingIsPublished` refuses a bundle that
records no identity:

```
cosign verify \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity 'https://github.com/spdrman/rclone-manager/.github/workflows/release.yml@refs/heads/release' \
  ghcr.io/spdrman/backup-manager:0.2.0
```

That command passes against the published image, and it is the whole point of this
section that it does. It is checked by running it, not by reading it.

The ref half of that identity is `refs/heads/release` because a push to `release` is
what publishes (see the header of `.github/workflows/release.yml`). GitHub builds the
certificate SAN out of the ref the run was triggered on, so the trigger decides the
identity, and the two have to move together. Issue #510 is what happens when they do
not: this printed a regexp anchored on `@refs/tags/` for long enough that the command
above would answer

```
Error: no matching signatures: none of the expected identities matched what was in
the certificate, got subjects
[https://github.com/spdrman/rclone-manager/.github/workflows/release.yml@refs/heads/release]
with issuer https://token.actions.githubusercontent.com
```

against a release that was signed correctly the whole time. A record that makes a real
artifact look forged is worse than one that says nothing, so
`TestSigningIdentityMatchesTheWorkflowTriggerThatPublishes` now reads the workflow's
trigger and refuses an identity a run of that workflow could not produce, and
`TestComplianceDocsPrintTheCommandThatPasses` refuses this file if it drifts from it.

The identity is exact rather than a regexp. The ref is a single fixed branch, so there
is nothing to match loosely, and an exact identity cannot be quietly widened by a
missing anchor the way the old `@refs/tags/` pattern was: it had no `$`, so it would
have accepted any tag ref at all.

A hand-dispatched run publishing from some other branch would sign under that branch's
ref and would not match this command. That is intended. `release` is the only branch a
cut lands on (`docs/release-branch.md`), and an image signed from anywhere else is not
one this record vouches for.

The tag in that example is `0.2.0` rather than the `0.3.0` this tree declares, because
`0.3.0` is not pushed yet and there is nothing at that tag to verify. Move it once the
release workflow has published, at the same time the digests are recorded back.

The SBOM is attached as an attestation over the same digest
(`cosign attest --type spdxjson`), not baked into the image. That keeps the
image bytes unchanged by the signing work, which matters because image size is a
gated budget, and it means a consumer can fetch the SBOM for a digest they
already hold rather than pulling an image to read it.

**If a release ever has to be signed by hand, off CI**, the key is supplied at
release time through the environment and never written down:

```
COSIGN_PRIVATE_KEY="$(pass show backup-manager/cosign)" \
  cosign sign --key env://COSIGN_PRIVATE_KEY ghcr.io/spdrman/backup-manager@<digest>
```

`scripts/release/publish-image.sh` enforces that. Guard 5 asks git for every path
in the working tree matching `*.key`, `*.pem`, `cosign.key`, `*.p12`, `*.pfx` or
an SSH private key name, and refuses to run beside any of them. It refuses
`COSIGN_KEY_FILE` outright as well. "I will delete it afterwards" is how key
material ends up somewhere it cannot be taken back from, and a signing key in git
history is not recoverable from by deleting the file.

Three details in that scan are load-bearing, and two of them were wrong when the
guard was first written:

* It looks at **ignored** files as well as tracked and untracked ones. The
  `.gitignore` block covering `*.key` and `*.pem` is the outer net, and
  `git ls-files --exclude-standard` hides exactly what that net catches, so a
  single pass went blind to the case the guard exists for:
  `cosign generate-key-pair` drops `cosign.key` in the working directory, ignored
  and invisible. Two passes are run and unioned.
* `node_modules/` and `ui/shared/dist/` are excluded. With ignored files in
  scope, one vendored `*.pem` test fixture would refuse every release, and a
  guard that cries wolf is a guard somebody switches off.
* `id_rsa` and `id_ed25519` are matched as `*/id_rsa` and `*/id_ed25519` too. A
  git pathspec with no wildcard anchors at the repository root, so the bare forms
  only ever saw a key in the top directory, and this product mounts its SSH key
  at `/etc/backup-manager/id_ed25519`.

`scripts/tests/publish-image-guards.test.sh` builds every fixture with this
repository's real `.gitignore` in it, because the guard's answer depends on the
exclusion configuration and a fixture without it passes for a reason that does
not hold where the script runs.

## Publishing

`ghcr.io/spdrman/backup-manager:0.3.0` is cut and not pushed.
`distribution/packaging/canonical.json` records `image.published: false`, and the release
manifest records the same fact from the other side as a `registry_digest` of `null` per
architecture and a null `index_digest`. The two are held together by
`TestReleaseManifestRegistryDigestTracksTheCanonicalPublishFlag`, so neither can move
alone, and the push below is what fills both in.

`0.2.0` was pushed this way and remains published: its image index is `sha256:0ba1fba4`,
signed keylessly through the release workflow's own OIDC identity with the SBOM attested
beside it, and each architecture's digest was read back with
`docker buildx imagetools inspect` rather than taken from the push's own output. `0.1.0`
before it was published the same way and stays where it is.

The mechanism that did the push is not automatic, and a later release repeats it by
hand:

```
scripts/release/publish-image.sh
```

It is an operator action on purpose. It publishes a semantic version to a public
registry, which is not a thing that is taken back cleanly; it needs a registry
credential this repository does not and must not hold; and pushing from a branch
would put an image in the registry built from a commit that is not on `main`,
which is #174's failure moved somewhere no ancestry check can reach. Guard 2
refuses that last one by requiring `HEAD` to be the commit the release manifest
records.

Its six refusals have a control that runs on every full local gate:
`scripts/tests/publish-image-guards.test.sh` drives the real script in a
throwaway repository per refusal, through the `GUARDS_ONLY=1` seam that stops
after the guard block and before the first Docker command, and asserts the
distinct message rather than only the exit code. All six exit 2, so an
exit-code-only assertion could not tell "the tree is dirty" from "there is a
private key sitting in it".

Those fixtures set `SKIP_PROVENANCE_CHECK=1` so they need no Go toolchain, and
that variable removes guard 6 rather than stopping before it, so it is only safe
on a run that cannot publish. Setting it on a run that would push is itself a
refusal: it is combinable with `GUARDS_ONLY=1` and with nothing else. An
attestation is a signed claim, and a stale one attached to bytes that outlive the
correction is worse than no attestation at all.

### After a successful push

Two edits, in this order:

1. `distribution/packaging/canonical.json`: `image.published` false to true.
2. `container/release-manifest.json`: each architecture's `registry_digest`
   null to the digest read back out of the registry.

Then regenerate the bundle (`(cd distribution && go run ./cmd/provenance -write)`)
and run the gate. Doing one edit and not the other fails, which is the point:
a published flag with no digest and a digest with no published flag are both
half-truths.

The digest is read back with `docker buildx imagetools inspect` rather than
scraped out of the push's own output. The push reports what it believes it sent;
the manifest is a claim about what the registry holds.

## Version parity

`container/release-manifest.json`'s `version` is the `VERSION` build argument the
binaries were stamped with, which is what `/backup-manager version` answers.
`canonical.json`'s `image.tag` is the semantic version every provider package
advertises. Those have to be the same string in a real release, and now they are:
both record `0.3.0`, the tag cut for this release rather than the generator's
`git describe --tags --always` fallback that produced an abbreviated commit before
this repository had any tags.

That parity is checked rather than assumed:
`releaseManifest.versionIsABuildStamp` in the provenance bundle is `false`, and
`TestVersionParityComplaints` refuses a bundle that misstates it in either
direction. Cutting the real tag before the push is what closed the gap this section
used to describe: a store advertising `:1.0.0` while the binary inside answered with a
commit SHA was the failure a push before tagging would have shipped.

## The links a store reviewer follows

`compliance.json`'s `sourceRepository.visibility` is `public`, so every declared
link resolves for anyone, and `links.publiclyReachable` in the bundle is derived
from that one field rather than asserted per link. §73 WP5.2's link criterion is
met.

That value went stale once and the note above it claimed it had been measured,
which is how issue #484 found it: the repository was made public and nothing came
back to re-read the field, so the record said private for a repository anyone
could open. Re-run `gh repo view spdrman/rclone-manager --json visibility` rather
than trusting the note, and regenerate the bundle with
`go run ./cmd/provenance -write` from `distribution/`.

`docs/compliance/source-offer.md` stays either way. A link resolving is not the
same as an offer having been made, and Apache-2.0 §4a is an obligation to whoever
received a package, not to whoever visits the repository.

## Verifying a release by hand

```
sha256sum -c provenance/checksums.txt
```

covers the licence, the notices, the inventory, the SBOM, the release manifest,
the frontend lockfile and every distributed provider artifact in one command.
The binaries and the image are covered by the release manifest's own recorded
digests, and the image additionally by its signature.

## What is not covered yet

Stated here rather than left to be discovered:

- **The performance evidence is pending.** The seven metric names #81 lists are
  pinned as a set so one cannot be dropped quietly, and every value is null,
  naming the issue that will produce it. #165, #167 and #170 have not merged.
- **Reproducibility is proven for the binaries, not yet for the image.** #174
  showed that two runs of the hash recorder from the same clean checkout produce
  byte-identical `binary_sha256` values on both architectures. A digest-identical
  OCI image additionally needs the image's own layer metadata to be
  deterministic, which needs a published image to compare against.

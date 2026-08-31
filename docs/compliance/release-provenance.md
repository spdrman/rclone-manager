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
record and the performance evidence. `apps/common/cmd/provenance` writes it,
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
cd apps/common
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
  --certificate-identity-regexp '^https://github.com/spdrman/rclone-manager/\.github/workflows/release\.yml@refs/tags/' \
  ghcr.io/spdrman/backup-manager:1.0.0
```

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

`scripts/release/publish-image.sh` enforces that. Guard 5 greps the working tree
for `*.key`, `*.pem`, `cosign.key`, `*.p12`, `*.pfx` and SSH private keys,
tracked or untracked, and refuses to run beside any of them, and it refuses
`COSIGN_KEY_FILE` outright. "I will delete it afterwards" is how key material
ends up in a commit, and a signing key in git history is not recoverable from by
deleting the file.

## Publishing

Nothing has been pushed to `ghcr.io/spdrman/backup-manager` yet.
`apps/common/packaging/canonical.json` records that as `image.published: false`,
and the release manifest records the same fact from the other side as a
`registry_digest` of `null` on every architecture. The two are held together by
`TestReleaseManifestRegistryDigestTracksTheCanonicalPublishFlag`, so neither can
move alone.

The mechanism exists and the push does not happen automatically:

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

### After a successful push

Two edits, in this order:

1. `apps/common/packaging/canonical.json`: `image.published` false to true.
2. `container/release-manifest.json`: each architecture's `registry_digest`
   null to the digest read back out of the registry.

Then regenerate the bundle (`(cd apps/common && go run ./cmd/provenance -write)`)
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
advertises. Those are the same string in a real release and they are not the
same string today: the generator defaults `VERSION` to
`git describe --tags --always`, this repository has no tags, so the recorded
build version is an abbreviated commit while the packages point at `1.0.0`.

That gap is recorded rather than glossed:
`releaseManifest.versionIsABuildStamp` in the provenance bundle is `true`, and
`TestVersionParityComplaints` refuses a bundle that misstates it in either
direction. It becomes a hard failure the moment `image.published` goes true,
because at that point a store advertises `:1.0.0` and the binary inside answers
with a commit SHA. Cutting a real tag before the first push closes it.

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

- **Nothing is published, so nothing is signed.** The bundle records
  `signing.status: unsigned`, and a test refuses any other value while
  `image.published` is false.
- **The links are not publicly reachable.** Every link target exists and has
  substance, and the repository is private, so a store reviewer following any of
  them gets a 404. `links.publiclyReachable` is `false` and
  `docs/compliance/source-offer.md` is the written offer that stands until the
  repository is made public. Only the repository owner can change that.
- **The performance evidence is pending.** The seven metric names #81 lists are
  pinned as a set so one cannot be dropped quietly, and every value is null,
  naming the issue that will produce it. #165, #167 and #170 have not merged.
- **Reproducibility is proven for the binaries, not yet for the image.** #174
  showed that two runs of the hash recorder from the same clean checkout produce
  byte-identical `binary_sha256` values on both architectures. A digest-identical
  OCI image additionally needs the image's own layer metadata to be
  deterministic, which needs a published image to compare against.

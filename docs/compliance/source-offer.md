# Source code and the written offer

Backup Manager, `com.iasbuilt.backupmanager`. This is the source and
source-offer material §73 Work Package 5.2 requires, and it is what the
`source-offer` link in `distribution/packaging/compliance.json` resolves to.

## The licence

Backup Manager is distributed under the **Apache License, Version 2.0**. The
full text is in `LICENSE` at the root of the source tree, and it is the same
text a distributed package carries.

Apache-2.0 was chosen over the equally available MIT for reasons that are
specific to how this product ships, and they are recorded in full in
`distribution/packaging/compliance.json` under `license.rationale` rather than
summarised here. In short: it grants patent rights explicitly and terminates
them on patent litigation, which is the clause a store's legal review looks for
before agreeing to redistribute somebody's binary; it defines the NOTICE file,
which gives the third-party attribution this project owes a defined place; and
it requires changed files to be marked, which matters for a product built around
redistributing someone else's compiled packages.

That choice is available because every component linked into a shipped
artifact is either permissively licensed or one of the non-permissive licences
this project accepts on purpose with its obligation written down and
discharged. Two components are in the second group, and the section below is
the discharge. It is checked rather than remembered: `LicensePolicyComplaints`
in `distribution/packaging` refuses the build for a licence that is neither,
and `LicenceObligationComplaints` refuses it when an accepted licence's
obligation is recorded and this file does not actually carry the offer.

## What this product includes

The one dependency worth naming here is **rclone**, which is MIT licensed and is
consumed as a Go module rather than as an executable: its packages are compiled
directly into `/backup-manager` behind a narrow transport adapter. There is no
`rclone` binary anywhere in the image, and `container/Dockerfile` says so and is
checked on it.

Every other third-party component, across the Go module graph and the
production npm packages built into the embedded web bundle, is listed with its
version, its SPDX licence identifier and the SHA-256 of its licence text in:

- `NOTICE`, the human-readable attribution file Apache-2.0 section 4(d) refers
  to, grouped by licence;
- `provenance/third-party-licenses.json`, the machine-readable
  inventory;
- `provenance/sbom.spdx.json`, an SPDX 2.3 SBOM of the same set.

## The MPL-2.0 components, and how to get their source

Two of the components linked into both shipped binaries are under the **Mozilla
Public License, version 2.0**, which is not permissive. They arrive under
rclone's `s3` backend through `github.com/IBM/go-sdk-core/v5`, which
`backend/s3`'s `ibm_signer.go` imports with no build tag, so registering the
backend and not linking them is not something this project can choose. They are
genuinely in the binaries and not only in `go.mod`: `go tool nm` on a
linux/amd64 `backup-manager` finds 17 `go-retryablehttp` symbols and one
`go-cleanhttp` symbol surviving dead-code elimination, `NewClient`,
`DefaultRetryPolicy`, `DefaultBackoff` and `DefaultPooledTransport` among them.

MPL-2.0 is file-level weak copyleft. Its reciprocity reaches the covered files
and nothing this project writes, and §3.3 permits a Larger Work that combines
them with other code to ship under other terms, which is what this release
does under Apache-2.0.

**The obligation.** §3.3 lets a Larger Work ship under terms of our choice
*provided we also comply with the MPL for the covered software*, so the
Apache-2.0 choice rests on §3.2 being met rather than on §3.3 alone.

§3.2(a) obliges anyone distributing the covered files in executable form to make
their Source Code Form available as §3.1 describes, and to inform recipients of
the executable form how to obtain that source by reasonable means, in a timely
manner, at no more than the cost of distribution. The addresses below are how:
they cost nothing, need no account, and are served immutably. §3.1 adds that you
must be told the source is governed by the MPL and how to obtain a copy of the
licence, which is what this section and the licence link above are for.

§3.2(b) forbids the terms of the executable from limiting or altering your
rights in that source. Nothing in this project's licence, notices or packaging
limits or alters them. §3.4 forbids stripping licence notices out of the covered
source, which does not arise here because neither module is modified: what we
ship is upstream's own release, unaltered.

**The licence text** is at <https://mozilla.org/MPL/2.0/>, and each module also
ships its own copy inside the archive linked below.

**The source.** Neither module is modified or vendored here, so the complete
corresponding source is the module archive upstream published, served
immutably and without an account:

- `github.com/hashicorp/go-cleanhttp@v0.5.2` (MPL-2.0), linked into
  `backup-manager` and `backup-manager-web`:
  <https://proxy.golang.org/github.com/hashicorp/go-cleanhttp/@v/v0.5.2.zip>
- `github.com/hashicorp/go-retryablehttp@v0.7.8` (MPL-2.0), linked into
  `backup-manager` and `backup-manager-web`:
  <https://proxy.golang.org/github.com/hashicorp/go-retryablehttp/@v/v0.7.8.zip>

Two things make those addresses an answer rather than a gesture. They are
content-addressed and immutable, so the archive served under a version is the
archive that version has always been, and `core/go.sum` records the hash the
build verified it against. And `provenance/third-party-licenses.json` records
the SHA-256 of each module's licence text as it appears inside that archive, so
a recipient can check that what they fetched is what shipped:

```
curl -sO https://proxy.golang.org/github.com/hashicorp/go-cleanhttp/@v/v0.5.2.zip
unzip -p v0.5.2.zip 'github.com/hashicorp/go-cleanhttp@v0.5.2/LICENSE' | shasum -a 256
# 60222c28c1a7f6a92c7df98e5c5f4459e624e6e285e0b9b94467af5f6ab3343d
```

None of this depends on this repository being public, which the source link
below still does. `go mod download github.com/hashicorp/go-cleanhttp@v0.5.2`
fetches the same archive for anyone with a Go toolchain.

This section is not maintained by hand in the sense that matters: the machine
readable form of it is `license.acceptedNonPermissive` in
`distribution/packaging/compliance.json`, and the gate reads this file and
fails if it stops naming any of these licences, versions or addresses. A third
encumbered module arriving turns this page red until somebody writes its offer.

**Where this offer currently reaches, and where it does not.** It is in this
file and in `NOTICE`, both of which are in the source tree and both of which
travel with the compliance materials a store reviewer is handed. Nothing this
project distributes carries them yet: `container/Dockerfile`'s runtime stage
copies the two binaries and the frontend bundle and not `LICENSE` or `NOTICE`,
so somebody who has only the image has to come here for the offer. That is a
gap in delivery rather than in the offer, it predates this section and applies
to Apache-2.0 §4(d) just as much, and it is tracked in #407. It is recorded
here rather than left out, for the same reason `sourceRepository.visibility`
records that this repository is private instead of implying the source link
resolves.

## What is generated

None of those three is maintained by hand. All three are regenerated from
`go list -deps` and `ui/shared/package-lock.json` on every run of the gate, and
the gate fails if what is checked in is not what this tree produces. An
undeclared dependency is therefore a red build rather than an omission somebody
has to notice.

## Getting the source

The source repository is <https://github.com/spdrman/rclone-manager>.

It is private today. That is a fact about access rather than about the licence:
Apache-2.0 obliges the project to pass the licence and the notices along with any
distributed copy, which the packages do, and it does not oblige a public
repository. But a store reviewer who follows a link into a private repository
gets a 404, and a compliance package that pretends otherwise is worth nothing.
So the state is recorded honestly, in
`distribution/packaging/compliance.json` (`sourceRepository.visibility`) and in
`provenance/release-provenance.json` (`links.publiclyReachable`), and
the release provenance bundle says in as many words that the link criterion is
not satisfied until the repository is made public.

**Written offer.** Until the repository is public, anyone who has received a
distributed copy of Backup Manager may obtain the complete corresponding source
for that copy by opening an issue at
<https://github.com/spdrman/rclone-manager/issues>, or by contacting the
distributor of the package they received. The copy supplied is the exact commit
recorded in `container/release-manifest.json` for that release, which is the
same commit the binaries were built from and the same commit the SBOM describes.

## Verifying that a package matches the source

Every release records, in one place, everything needed to check that claim
without trusting it:

- `container/release-manifest.json`: the commit, the per-architecture SHA-256 of
  both shipped binaries, and the registry digest of the published image;
- `provenance/release-provenance.json`: the semantic version, the
  digest of every distributed artifact, and the digests of the licence, the
  notices, the inventory and the SBOM;
- `provenance/checksums.txt`: all of the above in `sha256sum` format,
  so `sha256sum -c` checks the lot in one command.

`docs/compliance/release-provenance.md` documents how those are produced and how
a published image's signature is verified.

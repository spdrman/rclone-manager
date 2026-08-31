# Source code and the written offer

Backup Manager, `com.iasbuilt.backupmanager`. This is the source and
source-offer material §73 Work Package 5.2 requires, and it is what the
`source-offer` link in `apps/common/packaging/compliance.json` resolves to.

## The licence

Backup Manager is distributed under the **Apache License, Version 2.0**. The
full text is in `LICENSE` at the root of the source tree, and it is the same
text a distributed package carries.

Apache-2.0 was chosen over the equally available MIT for reasons that are
specific to how this product ships, and they are recorded in full in
`apps/common/packaging/compliance.json` under `license.rationale` rather than
summarised here. In short: it grants patent rights explicitly and terminates
them on patent litigation, which is the clause a store's legal review looks for
before agreeing to redistribute somebody's binary; it defines the NOTICE file,
which gives the third-party attribution this project owes a defined place; and
it requires changed files to be marked, which matters for a product built around
redistributing someone else's compiled packages.

That choice was only available because nothing in the dependency graph is
copyleft, and that is checked rather than remembered:
`LicensePolicyComplaints` in `apps/common/packaging` refuses the build if a
copyleft component ever appears in the inventory.

## What this product includes

The one dependency worth naming here is **rclone**, which is MIT licensed and is
consumed as a Go module rather than as an executable: its packages are compiled
directly into `/backup-manager` behind a narrow transport adapter. There is no
`rclone` binary anywhere in the image, and `container/Dockerfile` says so and is
checked on it.

Every other third-party component, 59 of them across the Go module graph and the
production npm packages built into the embedded web bundle, is listed with its
version, its SPDX licence identifier and the SHA-256 of its licence text in:

- `NOTICE`, the human-readable attribution file Apache-2.0 section 4(d) refers
  to, grouped by licence;
- `provenance/third-party-licenses.json`, the machine-readable
  inventory;
- `provenance/sbom.spdx.json`, an SPDX 2.3 SBOM of the same set.

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
`apps/common/packaging/compliance.json` (`sourceRepository.visibility`) and in
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

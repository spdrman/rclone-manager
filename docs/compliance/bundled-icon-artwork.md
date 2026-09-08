# The bundled icon artwork, and the licence it is under

Backup Manager draws its icons as inline SVG paths compiled into the web
bundle. Those paths are Font Awesome Free artwork, which is CC BY 4.0, and
this file is the attribution that licence asks for. Issue #621 is the
change that put them there.

## The attribution

- **Creator**: Fonticons, Inc. (https://fontawesome.com)
- **Copyright**: Copyright 2024 Fonticons, Inc.
- **Material**: Font Awesome Free 6.7.2, the fourteen icons listed below
- **Licence**: CC BY 4.0, Creative Commons Attribution 4.0 International
- **Licence text**: https://creativecommons.org/licenses/by/4.0/
- **Modified?** No. Each icon is the `d` attribute of the `<path>` in the
  SVG that release ships, reproduced verbatim and unmodified. Nothing has
  been redrawn, recoloured in the artwork itself, or edited.

## Where the attribution travels

Three places, because the readers are three different people.

This file is the full record, for somebody who has the repository. The
built page carries a shorter version of the same thing in an HTML comment
in the web UI's index.html, which is what reaches somebody holding only
the artifact: CC BY 4.0 section 3(a) is about that reader, and a
JavaScript comment cannot serve them, because minification removes
comments and an exported constant nothing imports is dropped by the
bundler. And Font Awesome's own embedded notice sits beside the path data
in the icon module, because the licence asks in as many words that those
comments not be actively removed from files.

Font Awesome Free is three licences at once and only one of them applies
here. The icons are CC BY 4.0; the webfonts are SIL OFL 1.1 and the code
is MIT, and neither of those ships in this product, because no Font
Awesome font file and no Font Awesome code is used. The artwork is drawn
by this project's own component, `ui/shared/src/design-system/icons.tsx`,
which is where the paths live and where Font Awesome's own embedded
attribution comment is reproduced.

## Why the artwork is vendored rather than depended on

Three constraints, and together they pick the shape.

The product runs on a NAS. Not "usually has internet": an appliance on a
LAN that may have no route off itself, which is a deployment this project
supports on purpose. A CDN link, a webfont or a sprite sheet is an empty
box there, and it fails silently.

There is a bundle budget, and the release image size is gated at 1.05x its
baseline. The Font Awesome React packages would spend a slice of that on
an icon registry, a tree-shaking story and a runtime, in order to draw
fourteen shapes. The shapes themselves are about 4.7 KB of path data.

And a dependency would not have made the licensing simpler. Font Awesome
Free's package declares `(CC-BY-4.0 AND OFL-1.1 AND MIT)`, which is a
licence expression rather than a decided licence, and
`distribution/packaging`'s policy check refuses exactly that: a component
whose terms nobody has read is not evidence of a permissive one. Adding it
would have needed the acceptance written down by hand anyway, which is
what this file is.

## The icons this product ships

Fourteen, each named by the Font Awesome style and name it came from, then
by the role this product draws it in.

- `solid/triangle-exclamation` drawn as `warning`: warnings, everywhere one is stated
- `solid/circle-xmark` drawn as `failure`: a failure, a halted set, a quarantined backup
- `solid/circle-check` drawn as `success`: a verified backup, a passed preflight step, a saved setting
- `solid/circle` drawn as `status-active`: a service that is running, a copy that is readable now
- `regular/circle` drawn as `status-idle`: a disabled backup set, a transfer stage not started
- `solid/circle-info` drawn as `info`: the info tone of a banner and of an activity line
- `solid/gauge-high` drawn as `dashboard`: the Dashboard nav row
- `solid/layer-group` drawn as `backup-sets`: the Backup sets nav row
- `solid/box-archive` drawn as `backups`: the Backups nav row
- `solid/clock-rotate-left` drawn as `activity`: the Activity nav row
- `solid/ban` drawn as `quarantine`: the Quarantine nav row
- `solid/gear` drawn as `settings`: the Settings nav row
- `solid/arrow-left` drawn as `arrow-left`: the way back, in every page header that has one
- `solid/arrow-right` drawn as `arrow-right`: the cancel-edit ledger's "becomes"

`ui/shared/src/test/icon-artwork.test.tsx` compares that list against the
registry in both directions and fails on a difference either way, so this
file cannot go on naming artwork that was dropped or miss artwork that was
added.

## What is NOT recorded here, and where it should end up

`NOTICE` is the file Apache-2.0 section 4(d) refers to, and it is where a
recipient of a built artifact looks for third-party attribution. This
artwork is not in it yet.

That is a gap in the generator rather than an oversight. `NOTICE` is
generated, byte for byte, by `distribution/packaging` from two
inputs: the Go module graph as `go list -deps` reports it, and the
production entries of `ui/shared/package-lock.json`. Vendored artwork is
in neither, because it is not a module and not a package: it is source
this repository carries. There is no third channel, so there is nowhere
for this to be declared, and hand-editing the generated file would fail
`TestComplianceArtifactsMatchThisTree` on the next run.

Closing it is a small change in one place: a `vendoredAssets` block in
`distribution/packaging/compliance.json` carrying the fields above, a
section in `buildNotice` that renders it, and a test that reads the
rendered `NOTICE` back for the licence, the URI and the creator, the same
way `LicenceObligationComplaints` already reads the MPL-2.0 source offer.
Until that lands, this file and the module comment are the record, and
`docs/compliance/source-offer.md` points at it.

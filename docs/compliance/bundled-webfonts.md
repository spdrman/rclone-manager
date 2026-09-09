# The bundled typeface, and the licence it is under

Backup Manager serves IBM Plex Sans and IBM Plex Mono out of its own
image. Those files are IBM's, they are under the SIL Open Font License
1.1, and this file is the attribution and the record that licence asks
for. Issue #631 is the change that put them there.

## The attribution

Two entries rather than one, because they are two separate uploads with
their own versions, and `distribution/packaging/compliance.json` treats a
version as part of the attribution rather than as bookkeeping.

- **Material**: IBM Plex Sans 1.1.0, from `@ibm/plex-sans`
- **Material**: IBM Plex Mono 2.5.0, from `@ibm/plex-mono`
- **Creator**: IBM Corp. (https://github.com/IBM/plex)
- **Copyright**: Copyright © 2017 IBM Corp. with Reserved Font Name "Plex"
- **Licence**: `OFL-1.1`, the SIL Open Font License 1.1
- **Licence text**: https://openfontlicense.org/open-font-license-official-text/,
  and the copy that ships is `ui/shared/public/fonts/LICENSE.txt`
- **Modification**: reproduced verbatim and unmodified. Each file is IBM's
  own published woff2, byte for byte. Nothing has been re-subsetted,
  re-encoded, renamed or hinted.

That last line is the whole permission rather than a courtesy, and it is
worth saying why. OFL-1.1 §3 reserves the name "Plex": a Modified Version
of the Font Software may not be distributed under it. IBM's own builds
carry the reserved name in their own name tables and are distributed by
the copyright holder, so redistributing them unmodified is §2 and needs no
further argument. A re-subsetted or re-encoded copy would be a Modified
Version under a name the licence reserves, and it would look identical in
review, which is why the register records each file's SHA-256 and
`VendoredAssetComplaints` re-derives it rather than trusting this
paragraph.

It is also why the files did not come from Google Fonts. Google's copies
of IBM Plex are Google's own re-subsetting, so re-hosting one of those out
of this repository would be exactly the thing the paragraph above rules
out. The files here came from the packages IBM publishes.

## Where the attribution travels

Four places, and each is read on every run rather than trusted.

`NOTICE` is the file Apache-2.0 §4(d) points a recipient of a built
artifact at, and it is generated from the register by `buildNotice`. This
file is the full record for somebody who has the repository.
`ui/shared/index.html` carries a short form in the running page, which is
the only copy a recipient of the bundle receives.
`ui/shared/src/design-system/typography.css` carries it beside the
`@font-face` declarations themselves, where somebody changing the faces
will meet it. All four are listed under the entries' `recordedIn`, so a
build fails when one of them stops carrying the creator, the copyright,
the licence, the address of the licence text or the modification
statement.

Beyond those, `ui/shared/public/fonts/LICENSE.txt` is the licence text
itself, sitting in the same directory as the woff2 files, because OFL-1.1
§2 asks that each copy of the Font Software contain the copyright notice
and the licence, and the copies include the ones inside the image. That is
the one obligation the icon artwork does not share: CC BY 4.0 §3(a)(1)(D)
asks for a URI to the licence rather than for its text.

The licence text reaches the image through `ui/shared/public/`, which Vite
copies into every bundle, rather than through a `COPY` line in
`container/Dockerfile`. That is deliberate: the image carries six copies
of the bundle (one compiled into `rbm-web`, five under
`/ui/bundles` for the adapters that have no other carrier), and a
Dockerfile line would have put the licence beside one of them.

## The files this product ships

Seven faces. Each is named by the IBM release it came from and by the CSS
weight `ui/shared/src/design-system/typography.css` declares it at.

- `ui/shared/public/fonts/IBMPlexSans-Regular-Latin1.woff2` at weight 400: body text, everywhere
- `ui/shared/public/fonts/IBMPlexSans-Medium-Latin1.woff2` at weight 500: field labels, table headers, status badges
- `ui/shared/public/fonts/IBMPlexSans-SemiBold-Latin1.woff2` at weight 600: every heading, every emphasised value
- `ui/shared/public/fonts/IBMPlexSans-Bold-Latin1.woff2` at weight 700: `<strong>`, which the user agent renders at bolder
- `ui/shared/public/fonts/IBMPlexMono-Regular-Latin1.woff2` at weight 400: paths, hashes, timestamps, sizes, the terminal
- `ui/shared/public/fonts/IBMPlexMono-Medium-Latin1.woff2` at weight 500: the eyebrow label, the dashboard figures
- `ui/shared/public/fonts/IBMPlexMono-SemiBold-Latin1.woff2` at weight 600: the wordmark, the health summary

`ui/shared/src/test/vendored-fonts.test.ts` holds that set to what the
shipped sources actually ask for, in both directions, so a component
asking for a weight with no face behind it is a red build and so is a face
nothing draws with.

## Why the typeface is vendored rather than fetched

Three constraints, and together they pick the shape. They are the same
three the icon artwork answers to, which is not a coincidence: #621 and
#631 are one defect at two layers, which is also why they share one
register rather than having one each.

The product runs on a NAS. Not "usually has internet": an appliance on a
LAN that may have no route off itself, which is a deployment this project
supports on purpose. A `<link>` to a font service is a system font there,
and unlike a missing icon it fails silently, because the page renders and
looks fine.

It is also an outbound request to a third party on every page load, for
operators who run this on a private network precisely so it does not phone
anywhere. That is worth being deliberate about rather than inheriting from
a template.

And there is a bundle budget. The release image is gated at 1.05x a
recorded baseline, and the image carries the bundle six times, so a face
costs six times its own size.

The Latin1 subsets are what make that payable, and both halves of it are
measured rather than derived. The built bundle grew from 538,914 bytes to
686,114. Two `docker build --platform linux/arm64` runs of
`container/Dockerfile` on the same host, one either side of the change,
put the image at 68,502,297 and 69,366,042 bytes, so the change costs
863,745: about 730 KB in the `/ui/bundles` layer for the five adapter
bundles and about 130 KB in `rbm-web` for the one compiled into
the binary. The complete faces rather than the Latin1 subsets would have
been roughly three times that, about 2.5 MB, against 2,084,902 bytes of
headroom in the last recorded measurement. So the subsetting is what keeps
this inside the budget rather than what makes it tidy.

The glyphs this UI draws outside Latin1 (the activity dock's horizontal
rule, the tick, the cross) were outside the previously fetched subset too
and have always come from a fallback font, so the subsetting costs nothing
that used to render.

One thing those two builds showed that is not about the typeface, and is
worth writing down where somebody will find it. The recorded baseline in
`docs/perf/baselines/darwin-arm64-mac17-2.json` is 43,008,762 bytes, taken
at `8ad3100` at the end of August, and the image on this branch's base
measures 68,502,297 before any of this. That is 1.59x the baseline against
a 1.05x gate, so `image_size_bytes` is already outside its threshold for
reasons that predate this change and have nothing to do with fonts: the
two Go binaries account for about 63 MB of it. Re-capturing the baseline
is a measurement on the designated benchmark host rather than an edit, so
it is not done here, it is filed as its own issue, and this paragraph is
here so the next person to read a font-size figure in a compliance record
knows which number the gate is actually failing on.

That issue was #635, and it is resolved. The baseline was re-captured at
`186ba0c7` on 2026-09-08 at 69,704,266 bytes, after the whole 26,695,504-byte
move was accounted for: 71.2% of it is rclone's S3 backend compiled into both
binaries for EPIC E's MediumStore, and the rest is the adapter bundles, the
licence material and product code. `docs/perf/README.md` carries the working.
The fonts measured here are inside that: 698,720 bytes across the five bundles
and about 165,000 in the binary, which is the 863,745 above.

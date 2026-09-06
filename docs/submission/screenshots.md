# Screenshots

The one submission material nobody can produce from a checkout. A screenshot is of this
application running on the provider's own hardware, which is why this file records what to
capture and how many, and `docs/acceptance/store-submission-preflight.md` records the
procedure that captures them. Until that procedure runs on real hardware, every target's
screenshot row is recorded as outstanding rather than as ready or as missing.

Do not substitute a mock. The shared UI has a mock API mode used by its own tests, and a
listing built from it would show data no administrator will ever see. Every screenshot
below is of a real installation against a real SFTP source.

## What each store asks for

| Store or catalog | Screenshots | Notes from that store's own requirements |
|---|---|---|
| Synology Package Center | 3 | Shown in the package's detail page. Capture at the DSM desktop's own window size rather than full screen. |
| TrueNAS Apps catalog | 3 | Referenced from `apps/truenas/catalog/app.yaml`'s `screenshots` list, which stays empty until they exist. |
| Unraid Community Applications | 2 | Community Applications shows the template's icon and overview prominently and screenshots secondarily; two is the useful number. |
| UGREEN App Center | 4 | EPIC D's submission. The count is recorded here so the material is ready when #83 produces the package; nothing about it is this work package's to capture. |
| CasaOS AppStore | 2 | The store renders one compose file's `x-casaos` block as the whole listing, so the tile and the install dialogue carry most of the presentation and two screenshots are what is left to show. |
| ZimaOS app store | 2 | The same two, captured again on ZimaOS. It is one runtime and two store registrations, and the two are submitted and certified separately, so a screenshot taken on CasaOS is not a screenshot of this listing. |

## What to capture, in this order

Capture more than the minimum and choose from them. Every one of these is a screen an
administrator sees in normal use, and none of them shows a credential.

1. **Dashboard.** At least one healthy set, one stale set and one failing set, so the
   health summary shows all three states. This is the screenshot that explains the product
   in one image.
2. **Backup set detail.** The lifecycle timeline of a real set with several runs behind
   it, including at least one failure that later recovered.
3. **Retention preview.** The confirmation dialogue listing exactly which restore points
   would be deleted, with the count visible. This is the screenshot that shows the product
   asks before it deletes.
4. **Add backup set wizard.** The host-key pinning step, showing the fingerprint the
   administrator is being asked to verify out of band.

## Before you capture

- Use a display scale that produces at least 1440 logical pixels of width. Every one of
  these stores rescales; none of them upscales well.
- Use the light theme unless the store's own listing page is dark.
- Redact nothing afterwards. If something needs redacting, the screenshot is of the wrong
  screen: use a source hostname and a backup-set name that are safe to publish, set up for
  this purpose.
- Do not capture a browser chrome with bookmarks in it.

## Where they go

Store them with the submission for that target rather than in this repository: they are
large binaries, they change with every visual revision, and no check here can tell a
current one from a stale one. Record in the acceptance run which build they were taken
from.

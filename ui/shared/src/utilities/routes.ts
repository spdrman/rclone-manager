/**
 * URL builders for routes whose param is a composite id, so a call site
 * never has to concatenate one by hand.
 *
 * # Why this exists (issue #285)
 *
 * model.BackupSetID.String() (core/internal/model/ids.go) joins a backup
 * set's source and set name with a "/", so a real id off the wire looks
 * like "production/api-server" — never a flat, slash-free token. The
 * route used to be a single segment (`/sets/:setId`), which cannot match
 * a path with an extra segment in it, so `navigate("/sets/" + id)` built
 * a URL that matched nothing and the click did nothing visible. The
 * route is now two segments (`App.tsx`: `/sets/:source/:set`), matching
 * the API's own `/backup-sets/{source}/{set}/...` shape (router.go), and
 * every call site goes through `backupSetPath` below instead of building
 * the string itself — that is what stops a fourth call site from
 * reintroducing the bug the first three had (DashboardPage's halt
 * banner, BackupSetsPage's card, and the test suite that rendered both
 * green because every mock id happened to be slash-free).
 */
export function backupSetPath(source: string, set: string): string {
  return "/sets/" + encodeURIComponent(source) + "/" + encodeURIComponent(set);
}

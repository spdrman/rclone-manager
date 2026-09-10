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

/**
 * The same thing one id longer, and for the same reason (issue #677).
 *
 * model.ArtifactID.String() is BackupSetID.String() + "/" + name, so a
 * real backup's id is three parts — "production/api-server/notes.txt" —
 * and the route is three segments (`App.tsx`: `/backups/:source/:set/
 * :name`), matching the API's own /backups/{source}/{set}/{name}
 * (router.go). The route it replaced declared one, so a click on a backup
 * row built a path nothing matched, the catch-all took it, and the
 * operator landed on the Dashboard with no error and no 404 — the #285
 * defect exactly, on the route immediately below the comment explaining
 * it.
 *
 * The id is taken whole and split here rather than asking callers for
 * three arguments, because that is what a caller holds: BackupArtifact
 * carries the composite `id` and its `filename`, never a source and a set
 * of its own. Each part is escaped on its own, which is the half that
 * cannot be done by concatenation: a filename may contain a space or a
 * "#", and a "#" pasted into a path raw starts a fragment and takes the
 * rest of the id out of the URL entirely.
 */
export function artifactPath(id: string): string {
  return "/backups/" + id.split("/").map(encodeURIComponent).join("/");
}

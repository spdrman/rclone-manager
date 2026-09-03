import type { BackupPlacement, PlacementAccess } from "@shared/types/backup";
import type { StorageSchema } from "@shared/api/contracts";
import { StatusBadge, type StatusTone } from "@shared/components/StatusBadge";
import { bytes, stamp } from "@shared/utilities/format";

/**
 * Where a backup's bytes actually are (EPIC E, FR-34, issue #240).
 *
 * # The one rule this component is written to
 *
 * Absence is never rendered as presence. There are three different things
 * hiding behind a cheerful "stored on offsite_s3", and this component
 * refuses to collapse them:
 *
 *   - NO COPY. `placements` is empty. A backup still transferring has no
 *     copy anywhere, and the partial file on disk is not one, so this says
 *     "no confirmed copy" rather than borrowing the path it is being
 *     written to.
 *   - A COPY NOBODY CAN CONFIRM. `access === "unreachable"`: this
 *     deployment cannot reach the place the copy is in, so it can neither
 *     confirm the copy nor deny it. That is not the same statement as the
 *     copy being gone and it is not rendered like one.
 *   - A COPY NOBODY HAS CHECKED. `verificationClass === null`. It gets the
 *     words "Not verified" and no tick, because "existence" is a claim
 *     that an object was seen, and nobody has seen anything.
 *
 * Issue #361 was a run cycle that backed nothing up and reported success.
 * Each of the three above, rendered as a green tick, is that same defect
 * in a different medium.
 *
 * # And nothing here is invented
 *
 * No cost figure and no restore estimate appears, in any state. The
 * backend cannot compute either one honestly (no price list, and the
 * provider reports no progress for a restore), so the surface states the
 * bytes, the storage class, and the fact that retrieval is billed, in the
 * backend's own words, and stops.
 */

/** How each access state is presented. The map is exhaustive over the
 *  generated wire union, so a value added to the contract is a type error
 *  here rather than a row that silently renders nothing. */
const ACCESS: Record<PlacementAccess, { tone: StatusTone; glyph: string; label: string; detail: string }> = {
  immediate: {
    tone: "ok",
    glyph: "●",
    label: "Readable now",
    detail: ""
  },
  requires_restore: {
    tone: "warn",
    glyph: "▲",
    label: "Needs a restore",
    detail:
      "This storage class cannot be read on demand. Getting this backup back means asking for a restore first and waiting hours, and the provider reports no progress while it waits."
  },
  restoring: {
    tone: "warn",
    glyph: "▲",
    label: "Restore in progress",
    detail:
      "A restore has been asked for and has not finished. The provider reports no percentage, so there is none to show."
  },
  unreachable: {
    tone: "warn",
    glyph: "▲",
    label: "Out of reach",
    detail:
      "This deployment has no way to reach that place, so nothing here can confirm this copy. That is not the same as the copy being gone."
  }
};

/** What a placement's status says about it, for the one status that is
 *  worth saying out loud. A copy the backend knows is gone is never
 *  served, so there is no third case. */
function statusNote(p: BackupPlacement): string {
  return p.status === "DELETE_PENDING"
    ? "A delete is recorded for this copy and may not have happened yet."
    : "";
}

export function PlacementList({
  placements,
  storage
}: {
  placements: BackupPlacement[];
  /** The verification ladder and the retrieval disclosure, as the backend
   *  serves them (GET /settings). Optional: a page that has not loaded
   *  settings renders the copies without the explanatory sentences rather
   *  than writing its own, because a second copy of "what existence
   *  proves" is a second copy that goes stale. */
  storage?: StorageSchema;
}) {
  const offLocal = placements.filter((p) => p.medium !== "local");
  const needsRestore = placements.some((p) => p.access === "requires_restore" || p.access === "restoring");

  return (
    <section className="card">
      <div className="card__header">
        <h2 className="eyebrow">Copies</h2>
        <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
          {placements.length === 0
            ? "none confirmed"
            : placements.length + (placements.length === 1 ? " durable copy" : " durable copies")}
        </span>
      </div>

      {placements.length === 0 ? (
        <div style={{ padding: "16px 18px" }}>
          <div className="banner banner--info">
            <span aria-hidden="true" style={{ color: "var(--text-2)" }}>{"●"}</span>
            <div>
              <div style={{ fontWeight: 500 }}>No confirmed copy yet</div>
              <p style={{ margin: "4px 0 0", fontSize: "var(--text-sm)", color: "var(--text-2)", maxWidth: "68ch" }}>
                A copy appears here once one finishes and the backup manager records it. An empty
                list means there is nothing to fall back on yet, not that the copies could not be
                read. A backup still arriving has a partial file on disk, and a partial file is not
                a copy.
              </p>
            </div>
          </div>
        </div>
      ) : (
        <div className="table-scroll">
          <table className="table" style={{ minWidth: 860 }}>
            <thead>
              <tr>
                <th scope="col">Where</th>
                <th scope="col">Location</th>
                {/* "Copy size", not "Size": the artifact's own size is what
                    the source reported at discovery, and this is what THIS
                    copy measures. They are allowed to differ, and a screen
                    that called both of them "Size" would make a real
                    disagreement between them read as a typo. */}
                <th scope="col" style={{ textAlign: "right" }}>Copy size</th>
                <th scope="col">Class</th>
                <th scope="col">Access</th>
                <th scope="col">Verification</th>
              </tr>
            </thead>
            <tbody>
              {placements.map((p) => {
                const access = ACCESS[p.access];
                const note = statusNote(p);
                return (
                  <tr key={p.medium + "|" + p.location}>
                    <td>
                      <div style={{ fontWeight: 500 }}>
                        {p.medium === "local" ? "Local backup root" : p.medium}
                      </div>
                      <div className="mono" style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                        {/* An empty mediumType is the honest answer for a medium the
                            configuration no longer describes: this deployment does not
                            know what kind of place that was any more. */}
                        {p.mediumType || "not described by this configuration"}
                      </div>
                      {note ? (
                        <div style={{ fontSize: "var(--text-xs)", color: "var(--warn)", marginTop: 3 }}>{note}</div>
                      ) : null}
                    </td>
                    <td className="mono" style={{ fontSize: "var(--text-sm)", wordBreak: "break-all" }}>
                      {p.location}
                    </td>
                    <td className="mono" style={{ textAlign: "right", whiteSpace: "nowrap" }}>
                      {/* null is "nobody recorded a size", which is not zero bytes. */}
                      {p.sizeBytes === null ? (
                        <span style={{ color: "var(--text-3)" }}>not recorded</span>
                      ) : (
                        bytes(p.sizeBytes)
                      )}
                    </td>
                    <td className="mono" style={{ fontSize: "var(--text-sm)", whiteSpace: "nowrap" }}>
                      {p.storageClass || <span style={{ color: "var(--text-3)" }}>{"—"}</span>}
                    </td>
                    <td style={{ whiteSpace: "nowrap" }}>
                      <StatusBadge tone={access.tone} glyph={access.glyph}>{access.label}</StatusBadge>
                      {access.detail ? (
                        <div style={{ fontSize: "var(--text-xs)", color: "var(--text-2)", marginTop: 4, maxWidth: "34ch", whiteSpace: "normal" }}>
                          {access.detail}
                        </div>
                      ) : null}
                    </td>
                    <td>
                      <PlacementVerification placement={p} storage={storage} />
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      {offLocal.length > 0 && storage ? (
        <div className="card__footer" style={{ display: "flex", flexDirection: "column", gap: 8 }}>
          {needsRestore ? (
            <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-2)", maxWidth: "80ch" }}>
              {storage.mediumDisclosure}
            </p>
          ) : null}
          <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-2)", maxWidth: "80ch" }}>
            {storage.retrievalDisclosure}
          </p>
        </div>
      ) : null}
    </section>
  );
}

/**
 * One copy's verification, in the backend's own words.
 *
 * A null class is rendered as "Not verified" and nothing else: no date, no
 * tick, no rung. The temptation is to fall back to the weakest rung
 * because a table cell wants a value, and the weakest rung is still a
 * claim that somebody looked.
 */
function PlacementVerification({
  placement,
  storage
}: {
  placement: BackupPlacement;
  storage?: StorageSchema;
}) {
  if (placement.verificationClass === null) {
    return (
      <div>
        <div style={{ color: "var(--text-2)" }}>Not verified</div>
        <div style={{ fontSize: "var(--text-xs)", color: "var(--text-3)", marginTop: 3, maxWidth: "36ch" }}>
          Nothing has checked this copy.
        </div>
      </div>
    );
  }

  const rung = storage?.verificationClasses.find((c) => c.className === placement.verificationClass);
  return (
    <div>
      <div>{LADDER_LABEL[placement.verificationClass]}</div>
      {placement.verifiedAt ? (
        <div className="mono" style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
          {stamp(placement.verifiedAt)}
        </div>
      ) : null}
      {rung ? (
        <div style={{ fontSize: "var(--text-xs)", color: "var(--text-2)", marginTop: 3, maxWidth: "36ch" }}>
          {"Proves " + rung.proves + "."}
        </div>
      ) : null}
    </div>
  );
}

/**
 * The operator-facing name of each rung.
 *
 * Only the NAMES are here; what each one proves and what it costs come
 * from the backend (StorageSchema), because those sentences are the ones
 * somebody reads while deciding whether a backup is safe, and a
 * paraphrase kept in a frontend is a paraphrase that goes stale. The
 * record is exhaustive over the generated union, so a rung added to the
 * ladder is a type error here.
 */
const LADDER_LABEL: Record<NonNullable<BackupPlacement["verificationClass"]>, string> = {
  content: "Content verified",
  attested: "Provider checksum matched",
  existence: "Existence only"
};

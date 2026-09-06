import { bytes } from "@shared/utilities/format";
import type { ManagerStorage } from "@shared/api/contracts";

/**
 * FR-21's storage panel (issue #286).
 *
 * # The bug this replaces
 *
 * The previous version took `freeBytes`/`totalBytes` as loose numbers and
 * computed `1 - freeBytes / totalBytes` with no guard. On an unconfigured
 * instance both arrive as 0, so that is `1 - 0/0`: `NaN`, rendered
 * straight into the caption as `"0 B of 0 B used · NaN%"`. This
 * component now takes the whole `ManagerStorage` reading instead of two
 * bare numbers, and `known` is checked before any arithmetic runs at all
 * — there is no code path left that divides by a total that might be
 * zero.
 *
 * # The honest unknown, not a clamped zero
 *
 * `known: false` is not an error state to paper over with "0%": an
 * unconfigured instance, or one whose backup sets share no derivable
 * mount, genuinely does not know its capacity yet, and this says so in
 * words. Clamping to zero would be a second wrong answer wearing the
 * first one's clothes — confidently reporting "nothing is used" about a
 * question that was never answered.
 *
 * # Naming the denominator
 *
 * `storage.denominator` says whether the gauge is a fraction of the whole
 * disk or of an operator's configured cap. A bar at 80% of a 2 TB volume
 * and one at 80% of a 100 GB allowance are different facts, and the
 * caption states which one this is rather than leaving both to look
 * identical.
 */
export function StorageGauge({ storage }: { storage: ManagerStorage }) {
  if (!storage.known) {
    return (
      <div style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
        {unknownCopy(storage)}
      </div>
    );
  }

  // limitBytes is 0 only when known is false (an empty backup root has no
  // configured cap and a real filesystem's TotalBytes is never 0), which
  // the branch above already returned out of — but the guard costs
  // nothing and this is exactly the arithmetic the previous version got
  // wrong, so it stays explicit rather than assumed.
  const usedFraction = storage.limitBytes > 0 ? storage.usedBytes / storage.limitBytes : 0;
  const percent = Math.round(usedFraction * 100);
  const color =
    storage.level === "CRITICAL" ? "var(--danger)" : storage.level === "WARNING" ? "var(--warn)" : "var(--ok)";
  const denominatorLabel = storage.denominator === "cap" ? "of configured cap" : "of total disk space";

  return (
    <div>
      <div
        role="meter"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={percent}
        aria-label="Storage used"
        style={{
          height: 5, borderRadius: 3, background: "var(--surface-3)", overflow: "hidden"
        }}
      >
        <div style={{ width: (usedFraction * 100).toFixed(1) + "%", height: "100%", background: color }} />
      </div>
      <div style={{ marginTop: 5, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
        {bytes(storage.usedBytes) + " of " + bytes(storage.limitBytes) + " used \u00b7 " +
          percent + "% " + denominatorLabel}
      </div>
    </div>
  );
}

/** One sentence per reason, matching core/service.StorageUnknownReason's
 *  own split: "no destination configured yet" and "the mount vanished"
 *  are different facts an operator would act on completely differently,
 *  so they are not allowed to collapse into one "storage unavailable"
 *  message here either. */
function unknownCopy(storage: ManagerStorage): string {
  switch (storage.unknownReason) {
    case "no_backup_root":
      return "Storage capacity is not known yet. It becomes available once a backup destination is configured.";
    case "not_created":
      return "Storage capacity is not known yet. The backup destination"
        + (storage.measuredPath ? " (" + storage.measuredPath + ")" : "")
        + " has not been created; this is normal before the first backup cycle runs.";
    case "unreadable":
      return "The backup destination"
        + (storage.measuredPath ? " (" + storage.measuredPath + ")" : "")
        + " could not be read. Check that its mount is still present.";
    case "misconfigured":
      return "Storage capacity could not be assessed: the configured capacity settings could not produce a reading.";
    default:
      return "Storage capacity is not known yet.";
  }
}

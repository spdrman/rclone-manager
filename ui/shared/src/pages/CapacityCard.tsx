import { useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { BackupManagerError } from "@shared/api/contracts";
import type { ApiError, AppSettings, CapacitySettings, UpdateCapacitySettings } from "@shared/api/contracts";
import { useAsync } from "@shared/hooks/useAsync";
import { HelpField } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import { ErrorState } from "@shared/components/EmptyState";
import { isNotConfigured } from "@shared/api/failure";

/**
 * Issue #286 — the storage cap and its two FR-21 thresholds, the write
 * half of `internal/config`'s capacity block.
 *
 * # Why this replaced two decorative inputs rather than adding a third
 *
 * "Storage warning threshold" and "Storage critical threshold" already
 * existed on the Settings page as plain `<input defaultValue="80%">`
 * controls: no handler, no save, and a config key (`capacity.*`) that did
 * not exist for them to write to (see fieldHelpCopy.ts's own note on why
 * they carried no help pop-up). Adding a cap field beside a pair that
 * still did nothing would have made the page look MORE finished while
 * fixing only a third of the problem. All three now read from and write
 * to the real capacity block together.
 *
 * # Bytes on the wire, units only at the edge
 *
 * `capacity.capBytes` and its two thresholds are canonical byte counts —
 * see `CapacitySettings`'s own doc for why a config key never carries a
 * unit. `ByteAmountField` below is the one place a byte count becomes an
 * amount-plus-MB/GB pair for an operator to type into, and the one place
 * it is converted back.
 *
 * # Zero is a value here, not a placeholder
 *
 * A cap of 0 means "no cap"; a threshold of 0 means "no line". Both are
 * this product's defaults and both are legitimate things to save
 * deliberately (an operator removing a cap they set earlier types 0, they
 * do not clear the field), so this form never disables Save because a
 * byte amount reads "0" — that is `RetentionPolicyCard`'s worry about an
 * EMPTY tier chain, not this one's.
 */
export function CapacityCard({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const settings = useAsync<AppSettings>(() => api.getSettings(), [api]);

  return (
    <section className="card">
      <div className="card__header">
        <h2 className="eyebrow">Storage capacity</h2>
      </div>
      <div className="card__body">
        {isNotConfigured(settings.error) ? (
          // #275: the capacity cap is part of a configuration this
          // instance does not have yet, exactly like retention beside it.
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)" }}>
            Storage capacity is part of the configuration this instance has not been given yet.
            It becomes editable once the first backup set has been added.
          </p>
        ) : settings.error ? (
          <ErrorState
            message={settings.error.message}
            remediation="The storage capacity settings could not be read, so they cannot be edited here yet."
            correlationId={settings.error.correlationId}
            onRetry={settings.reload}
          />
        ) : settings.data ? (
          <CapacityEditor
            key={settingsKey(settings.data.capacity)}
            loaded={settings.data.capacity}
            readOnly={readOnly}
          />
        ) : (
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
            Loading storage capacity settings…
          </p>
        )}
      </div>
    </section>
  );
}

// ---------------------------------------------------------------------------
// Bytes, as an amount typed against an MB/GB unit
// ---------------------------------------------------------------------------

type ByteUnit = "MB" | "GB";

const UNIT_BYTES: Record<ByteUnit, number> = {
  MB: 1024 ** 2,
  GB: 1024 ** 3
};

/** What one field holds while it is being edited: text, not a number, for
 *  the same reason RetentionPolicyCard's TierDraft holds its counts as
 *  strings — a half-typed "1." or a cleared field must stay exactly what
 *  was typed rather than being coerced into something nobody entered. */
interface ByteFieldDraft {
  amount: string;
  unit: ByteUnit;
}

/** Formats to at most 3 decimal places and drops trailing zeros, so an
 *  exact number of GB round-trips as "100" rather than "100.000", while a
 *  byte count that is not a round MB/GB (a hand-edited config, or a value
 *  this very form saved in the other unit) still shows something rather
 *  than truncating to "0". */
function trimAmount(n: number): string {
  return String(Math.round(n * 1000) / 1000);
}

/** Bytes at or above 1 GB display in GB, so a typical cap reads as "100"
 *  rather than "102400" MB; anything smaller displays in MB, INCLUDING
 *  zero, so the sentinel reads as "0 MB" rather than defaulting to a unit
 *  that implies a much larger typical value. */
function bytesToDraft(n: number): ByteFieldDraft {
  if (n >= UNIT_BYTES.GB) return { amount: trimAmount(n / UNIT_BYTES.GB), unit: "GB" };
  return { amount: trimAmount(n / UNIT_BYTES.MB), unit: "MB" };
}

/** NaN for anything that is not a valid non-negative number, which
 *  driftToBytes's callers treat as "cannot compute a byte count yet"
 *  rather than silently rounding a half-typed value to 0. */
function draftToBytes(d: ByteFieldDraft): number {
  const amount = Number(d.amount);
  if (d.amount.trim() === "" || !Number.isFinite(amount) || amount < 0) return NaN;
  return Math.round(amount * UNIT_BYTES[d.unit]);
}

function ByteAmountField({
  label,
  help,
  draft,
  onChange,
  readOnly,
  hint,
  error
}: {
  label: string;
  help: (typeof FIELD_HELP)[keyof typeof FIELD_HELP];
  draft: ByteFieldDraft;
  onChange(next: ByteFieldDraft): void;
  readOnly: boolean;
  hint?: string;
  error?: string;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 5 }}>
      <div style={{ display: "flex", gap: 8, alignItems: "flex-end" }}>
        <HelpField label={label} help={help} style={{ flex: 1 }}>
          {(helpId) => (
            <input
              className="input"
              type="number"
              min={0}
              step="any"
              aria-describedby={helpId}
              value={draft.amount}
              disabled={readOnly}
              onChange={(e) => onChange({ ...draft, amount: e.target.value })}
            />
          )}
        </HelpField>
        <label className="field" style={{ width: 88 }}>
          <span className="visually-hidden">{label + " unit"}</span>
          <select
            className="select"
            value={draft.unit}
            disabled={readOnly}
            onChange={(e) => {
              const nextUnit = e.target.value as ByteUnit;
              const b = draftToBytes(draft);
              onChange({
                amount: Number.isFinite(b) ? trimAmount(b / UNIT_BYTES[nextUnit]) : draft.amount,
                unit: nextUnit
              });
            }}
          >
            <option value="MB">MB</option>
            <option value="GB">GB</option>
          </select>
        </label>
      </div>
      {error ? (
        <span style={{ fontSize: "var(--text-sm)", color: "var(--danger)" }}>{error}</span>
      ) : hint ? (
        <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>{hint}</span>
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// The editor
// ---------------------------------------------------------------------------

interface FieldErrors {
  cap?: string;
  warning?: string;
  critical?: string;
}

/** Mirrors core/internal/config.validateCapacity's own rules exactly, so
 *  a save this form disables is a save the server would refuse anyway,
 *  not a stricter local opinion. The server validates the whole config
 *  regardless (this is a courtesy that keeps a doomed request off the
 *  wire) and its refusal is what is displayed if the two ever disagree —
 *  see RetentionPolicyCard's own doc for the identical division of
 *  labour. */
function fieldErrors(cap: ByteFieldDraft, warning: ByteFieldDraft, critical: ByteFieldDraft): FieldErrors {
  const errors: FieldErrors = {};

  const capBytes = draftToBytes(cap);
  const warningBytes = draftToBytes(warning);
  const criticalBytes = draftToBytes(critical);

  if (Number.isNaN(capBytes)) errors.cap = "Enter an amount of 0 or more.";
  if (Number.isNaN(warningBytes)) errors.warning = "Enter an amount of 0 or more.";
  if (Number.isNaN(criticalBytes)) errors.critical = "Enter an amount of 0 or more.";

  if (!errors.warning && !errors.critical && warningBytes < criticalBytes) {
    // Distinct wording from FIELD_HELP.storageWarningThreshold's own
    // prose on purpose: the two serve different readers (this is the
    // specific violation, that is the standing rationale), and sharing a
    // phrase between them is exactly what makes one a substring of the
    // other in anything that greps for either.
    errors.warning = "This would put the warning line below the critical floor.";
  }
  if (!errors.cap && !errors.critical && capBytes > 0 && criticalBytes > 0 && capBytes <= criticalBytes) {
    errors.cap = "This cap would sit at or under the critical floor.";
  }

  return errors;
}

function CapacityEditor({ loaded, readOnly }: { loaded: CapacitySettings; readOnly: boolean }) {
  const api = useApi();

  const [baseline, setBaseline] = useState<CapacitySettings>(loaded);
  const [cap, setCap] = useState<ByteFieldDraft>(() => bytesToDraft(loaded.capBytes));
  const [warning, setWarning] = useState<ByteFieldDraft>(() => bytesToDraft(loaded.warningFreeBytes));
  const [critical, setCritical] = useState<ByteFieldDraft>(() => bytesToDraft(loaded.criticalFreeBytes));

  const [busy, setBusy] = useState(false);
  const [saveError, setSaveError] = useState<ApiError | null>(null);
  const [saved, setSaved] = useState(false);

  const errors = fieldErrors(cap, warning, critical);
  const invalid = Object.keys(errors).length > 0;

  const capBytes = draftToBytes(cap);
  const warningBytes = draftToBytes(warning);
  const criticalBytes = draftToBytes(critical);
  const dirty =
    !invalid &&
    (capBytes !== baseline.capBytes ||
      warningBytes !== baseline.warningFreeBytes ||
      criticalBytes !== baseline.criticalFreeBytes);

  function onSave() {
    if (readOnly || invalid || !dirty || busy) return;

    const update: UpdateCapacitySettings = {};
    // Only what actually changed is sent, matching RetentionPolicyCard's
    // own PATCH discipline: an untouched field must reach the server as
    // an absent key, never as a re-sent value that happens to be
    // unchanged, since on this section an absent key and a sent 0 mean
    // opposite things.
    if (capBytes !== baseline.capBytes) update.capBytes = capBytes;
    if (warningBytes !== baseline.warningFreeBytes) update.warningFreeBytes = warningBytes;
    if (criticalBytes !== baseline.criticalFreeBytes) update.criticalFreeBytes = criticalBytes;

    setBusy(true);
    setSaveError(null);
    setSaved(false);
    api
      .updateSettings({ capacity: update })
      .then((next) => {
        // Re-baseline against what the server says is now running, not
        // the draft: a value it resolved or canonicalised is part of the
        // answer, and the next "has anything changed" comparison has to
        // be made against those, the same reasoning RetentionPolicyCard
        // gives for doing this after every one of its own saves.
        setBaseline(next.capacity);
        setCap(bytesToDraft(next.capacity.capBytes));
        setWarning(bytesToDraft(next.capacity.warningFreeBytes));
        setCritical(bytesToDraft(next.capacity.criticalFreeBytes));
        setSaved(true);
      })
      .catch((e: unknown) => {
        setSaveError(
          e instanceof BackupManagerError
            ? e.api
            : {
                code: "unknown",
                message: "Backup Manager could not save the storage capacity settings.",
                correlationId: "unavailable"
              }
        );
      })
      .finally(() => setBusy(false));
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "78ch" }}>
        A cap of 0 means no cap: this manager uses the whole volume, and the dashboard reports
        against the disk itself. Any other value is enforced — a transfer that would push this
        manager&rsquo;s own usage past it is refused before it starts.
      </p>

      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(220px, 1fr))", gap: "15px 18px" }}>
        <ByteAmountField
          label="Storage cap"
          help={FIELD_HELP.storageCap}
          draft={cap}
          onChange={(next) => {
            setSaved(false);
            setCap(next);
          }}
          readOnly={readOnly}
          hint="0 = no cap"
          error={errors.cap}
        />
        <ByteAmountField
          label="Storage warning threshold"
          help={FIELD_HELP.storageWarningThreshold}
          draft={warning}
          onChange={(next) => {
            setSaved(false);
            setWarning(next);
          }}
          readOnly={readOnly}
          error={errors.warning}
        />
        <ByteAmountField
          label="Storage critical threshold"
          help={FIELD_HELP.storageCriticalThreshold}
          draft={critical}
          onChange={(next) => {
            setSaved(false);
            setCritical(next);
          }}
          readOnly={readOnly}
          error={errors.critical}
        />
      </div>

      {baseline.backupRoot ? (
        <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
          {(baseline.backupRootConfigured ? "Measured at " : "Measured at the shared backup destination, ")
            + baseline.backupRoot
            + (baseline.backupRootConfigured ? "." : " (derived from your configured backup sets).")}
        </p>
      ) : (
        <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
          No filesystem to measure yet: add a backup set first.
        </p>
      )}

      {saveError ? (
        <ErrorState
          message={saveError.message}
          remediation="Nothing was saved. The storage capacity settings on disk are unchanged."
          correlationId={saveError.correlationId}
        />
      ) : null}

      {saved ? (
        <div className="banner banner--ok" style={{ fontSize: "var(--text-sm)" }}>
          <span aria-hidden="true" style={{ color: "var(--ok)" }}>{"✓"}</span>
          <span>Storage capacity settings saved. They are in effect now, with no restart.</span>
        </div>
      ) : null}

      <div>
        <button
          className="btn btn--primary"
          type="button"
          style={{ height: 40 }}
          disabled={readOnly || invalid || !dirty || busy}
          onClick={onSave}
        >
          {busy ? "Saving…" : "Save storage capacity"}
        </button>
      </div>
    </div>
  );
}

function settingsKey(c: CapacitySettings): string {
  return [c.capBytes, c.warningFreeBytes, c.criticalFreeBytes, c.safetyMarginBytes].join("~");
}

import { useState } from "react";
import type { ReactNode } from "react";
import { useApi } from "@shared/api/ApiContext";
import { BackupManagerError } from "@shared/api/contracts";
import type {
  ApiError,
  AppSettings,
  RetentionSchema,
  RetentionSettings,
  RetentionTierSetting,
  UpdateRetentionSettings
} from "@shared/api/contracts";
import { useAsync } from "@shared/hooks/useAsync";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
import { ErrorState } from "@shared/components/EmptyState";
import { WarningBanner } from "@shared/components/WarningBanner";

/**
 * B3.7 (#140) — the retention policy form, the write half of what #111
 * shipped for the config file and the CLI.
 *
 * # It targets the chain, not the three scalars
 *
 * Issue #156 generalized FR-18 from a hardcoded daily/weekly/monthly
 * triple to an administrator-configured ordered chain, and this form
 * edits that chain. The old scalars still exist in the schema as sugar
 * for the default chain, but they are never rendered and never written:
 * the backend reports the RESOLVED chain either way (see
 * core/service.RetentionSettings), so one layout serves both spellings of
 * one policy, and the request omits `tiers` entirely unless the operator
 * actually edited the chain, which is what keeps a legacy file in its own
 * spelling when somebody only flips a toggle.
 *
 * # Every rule it enforces comes from the server
 *
 * The granularity list, the window-unit list, the tier-name pattern, the
 * reserved name, both ceilings and the default chain arrive in the same
 * response as the values (`schema` on AppSettings). Nothing in this file
 * hardcodes a closed value set, so a granularity added to
 * core/internal/config reaches this picker without a second copy here
 * going stale. The default chain joined that list late (PR #171's
 * mandatory finding M5): it was the one value the form did spell for
 * itself, and it is the one where a stale copy writes a policy rather
 * than merely displaying one. Client-side
 * validation is a courtesy that keeps a doomed request off the wire; the
 * server validates the whole config regardless, and its refusal is what
 * is displayed if the two ever disagree.
 *
 * # There is no way to spell "keep nothing"
 *
 * An empty chain reinstates the default 7/3/12 policy rather than
 * disabling retention (config.Retention.Tiers' own doc), so the form
 * refuses to reach that state at all: the last tier's Remove control is
 * disabled, the copy says what emptying would actually mean, and
 * "Restore default chain" is the positive affordance for the operator who
 * wanted the default back. The backend refuses an explicitly emptied
 * chain too, so this is a second line, not the only one.
 */
export function RetentionPolicyCard({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const settings = useAsync<AppSettings>(() => api.getSettings(), [api]);

  return (
    <section className="card">
      <div className="card__header">
        <h2 className="eyebrow">Retention policy</h2>
      </div>
      <div className="card__body">
        {settings.error ? (
          <ErrorState
            message={settings.error.message}
            remediation="The retention policy could not be read, so it cannot be edited here yet."
            correlationId={settings.error.correlationId}
            onRetry={settings.reload}
          />
        ) : settings.data ? (
          // Keyed remount on a reload, so the editor's own draft state is
          // initialised from freshly loaded settings rather than
          // synchronised into it afterwards.
          <RetentionPolicyEditor
            key={settingsKey(settings.data.retention)}
            loaded={settings.data.retention}
            schema={settings.data.schema.retention}
            readOnly={readOnly}
          />
        ) : (
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
            Loading the retention policy…
          </p>
        )}
      </div>
    </section>
  );
}

/** One tier being edited. The two numbers are held as strings so a
 *  half-typed value ("" while the operator clears the field, "1" on the
 *  way to "14") stays exactly what was typed instead of being coerced to
 *  a number and rendered back as something nobody entered. */
interface TierDraft {
  /** Stable across re-orders and removals, so React keeps the right DOM
   *  node with the right focus; never sent anywhere. */
  key: string;
  name: string;
  granularity: string;
  keep: string;
  periodDays: string;
  windowUnit: string;
}

let nextTierKey = 0;
function toDraft(t: RetentionTierSetting): TierDraft {
  nextTierKey += 1;
  return {
    key: "tier-" + nextTierKey,
    name: t.name,
    granularity: t.granularity,
    keep: String(t.keep),
    periodDays: t.periodDays ? String(t.periodDays) : "",
    windowUnit: t.windowUnit ?? ""
  };
}

const CUSTOM_PERIOD = "days";

/** The chain "Restore default chain" fills the form with, taken from the
 *  schema the server already serves alongside the values rather than
 *  written out here.
 *
 *  This used to be a literal 7/3/12 chain, which was a second spelling of
 *  something config.DefaultTierChain's own doc says has exactly one, and
 *  not a harmless one: restoring the default and saving writes an EXPLICIT
 *  tiers list, which clears the legacy scalars and permanently migrates a
 *  config that would have tracked the product's default onto whatever this
 *  file happened to say. A stale copy could therefore narrow a real
 *  retention window, silently, in the dangerous direction, with nothing
 *  comparing the two. Every other closed value set in this card is already
 *  served by the schema for the same reason. */
function defaultChain(schema: RetentionSchema): TierDraft[] {
  return schema.defaultTiers.map(toDraft);
}

const GRANULARITY_LABELS: Record<string, string> = {
  day: "Day",
  week: "Week",
  month: "Month",
  quarter: "Quarter",
  half_year: "Half year",
  year: "Year",
  days: "Custom period"
};

function granularityLabel(value: string): string {
  return GRANULARITY_LABELS[value] ?? value;
}

const WEEKDAYS = ["monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"];

function RetentionPolicyEditor({
  loaded,
  schema,
  readOnly
}: {
  loaded: RetentionSettings;
  schema: RetentionSchema;
  readOnly: boolean;
}) {
  const api = useApi();

  // Purely local form state: nothing outside this card reads a
  // half-edited policy, so this stays plain useState rather than going
  // through the shared causl graph (state/appNodes.ts is for state two
  // components have to agree on).
  const [baseline, setBaseline] = useState<RetentionSettings>(loaded);
  const [timezone, setTimezone] = useState(loaded.timezone);
  const [weekStartsOn, setWeekStartsOn] = useState(loaded.weekStartsOn);
  const [protect, setProtect] = useState(loaded.protectLastKnownGood);
  const [tiers, setTiers] = useState<TierDraft[]>(() => loaded.tiers.map(toDraft));

  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [saveError, setSaveError] = useState<ApiError | null>(null);
  const [saved, setSaved] = useState(false);

  const namePattern = new RegExp(schema.tierNamePattern);
  const errors = tiers.map((t, i) => tierErrors(t, i, tiers, schema, namePattern));
  const invalid = errors.some((e) => Object.keys(e).length > 0);

  const chainEdited = chainKey(tiers.map(toTierSetting)) !== chainKey(baseline.tiers);
  const dirty =
    chainEdited ||
    timezone !== baseline.timezone ||
    weekStartsOn !== baseline.weekStartsOn ||
    protect !== baseline.protectLastKnownGood;

  // The one dangerous direction: FR-19 protection is on in the saved
  // config and this save would turn it off. core/internal/retention calls
  // that "a materially more dangerous configuration"
  // (LastKnownGoodDecide), and #111's acceptance criteria require the
  // operator see that before it takes effect — hence a confirmation in
  // front of the write, not a notice after it.
  const disablingProtection = baseline.protectLastKnownGood && !protect;

  function update(i: number, patch: Partial<TierDraft>) {
    setSaved(false);
    setTiers((current) => current.map((t, n) => (n === i ? { ...t, ...patch } : t)));
  }

  function submit() {
    const retention: UpdateRetentionSettings = {};
    // Only what actually changed is sent. An absent key means "leave this
    // alone" to the endpoint, and for `tiers` in particular that is what
    // stops a config written with the legacy scalars being rewritten into
    // the general spelling by a save that never touched the chain.
    if (timezone !== baseline.timezone) retention.timezone = timezone;
    if (weekStartsOn !== baseline.weekStartsOn) retention.weekStartsOn = weekStartsOn;
    if (protect !== baseline.protectLastKnownGood) retention.protectLastKnownGood = protect;
    if (chainEdited) retention.tiers = tiers.map(toTierSetting);

    setBusy(true);
    setSaveError(null);
    setSaved(false);
    api
      .updateSettings({ retention })
      .then((next) => {
        // Re-baseline against what the server says is now running, not
        // against the draft: defaults it resolved and values it
        // canonicalised are part of the answer, and the next "has
        // anything changed" comparison has to be made against those.
        setBaseline(next.retention);
        setTimezone(next.retention.timezone);
        setWeekStartsOn(next.retention.weekStartsOn);
        setProtect(next.retention.protectLastKnownGood);
        setTiers(next.retention.tiers.map(toDraft));
        setSaved(true);
      })
      .catch((e: unknown) => {
        setSaveError(
          e instanceof BackupManagerError
            ? e.api
            : {
                code: "unknown",
                message: "Backup Manager could not save the retention policy.",
                correlationId: "unavailable"
              }
        );
      })
      .finally(() => setBusy(false));
  }

  function onSave() {
    if (readOnly || invalid || !dirty || busy) return;
    if (disablingProtection) {
      setConfirming(true);
      return;
    }
    submit();
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "78ch" }}>
        One policy for every backup set. Each tier groups backups into calendar buckets and keeps
        the newest good backup in each one; a backup kept by any tier is kept.
      </p>

      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(196px, 1fr))", gap: "15px 18px" }}>
        <label className="field">
          <span className="field__label">Timezone</span>
          <input
            className="input input--mono"
            value={timezone}
            disabled={readOnly}
            onChange={(e) => {
              setSaved(false);
              setTimezone(e.target.value);
            }}
          />
        </label>
        <label className="field">
          <span className="field__label">Week starts on</span>
          <select
            className="select"
            value={weekStartsOn}
            disabled={readOnly}
            onChange={(e) => {
              setSaved(false);
              setWeekStartsOn(e.target.value);
            }}
          >
            {WEEKDAYS.map((d) => (
              <option key={d} value={d}>
                {d.charAt(0).toUpperCase() + d.slice(1)}
              </option>
            ))}
          </select>
        </label>
      </div>

      <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
        {tiers.map((t, i) => (
          <TierRow
            key={t.key}
            index={i}
            tier={t}
            schema={schema}
            errors={errors[i]}
            readOnly={readOnly}
            canRemove={tiers.length > 1}
            onChange={(patch) => update(i, patch)}
            onRemove={() => {
              setSaved(false);
              setTiers((current) => current.filter((_, n) => n !== i));
            }}
          />
        ))}
      </div>

      <div style={{ display: "flex", gap: 9, flexWrap: "wrap" }}>
        <button
          className="btn btn--sm"
          type="button"
          disabled={readOnly}
          onClick={() => {
            setSaved(false);
            setTiers((current) => [
              ...current,
              toDraft({ name: "", granularity: "day", keep: 1 })
            ]);
          }}
        >
          Add tier
        </button>
        <button
          className="btn btn--sm"
          type="button"
          disabled={readOnly}
          onClick={() => {
            setSaved(false);
            setTiers(defaultChain(schema));
          }}
        >
          Restore default chain
        </button>
      </div>

      <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-3)", maxWidth: "78ch" }}>
        Removing every tier is not how retention is switched off: an empty chain reinstates the
        default daily/weekly/monthly policy. Retention stops only by not running a retention pass
        at all.
      </p>

      <label
        style={{
          display: "flex", alignItems: "center", gap: 10, padding: "11px 13px",
          border: "1px solid " + (protect ? "var(--border)" : "var(--danger)"),
          borderRadius: 7, fontSize: 13, cursor: readOnly ? "default" : "pointer"
        }}
      >
        <input
          type="checkbox"
          checked={protect}
          disabled={readOnly}
          style={{ accentColor: "var(--accent)" }}
          onChange={(e) => {
            setSaved(false);
            setProtect(e.target.checked);
          }}
        />
        <span style={{ flex: 1 }}>
          Protect the newest known-good backup from retention (FR-19)
        </span>
      </label>

      {!protect ? (
        <WarningBanner tone="danger" title="Last-known-good protection is off">
          With this off, the newest known-good backup can be deleted by a retention pass purely
          because of its age. Backup Manager treats that as a materially more dangerous
          configuration.
        </WarningBanner>
      ) : null}

      {saveError ? (
        <ErrorState
          message={saveError.message}
          remediation="Nothing was saved. The policy on disk is unchanged."
          correlationId={saveError.correlationId}
        />
      ) : null}

      {saved ? (
        <div className="banner banner--ok" style={{ fontSize: "var(--text-sm)" }}>
          <span aria-hidden="true" style={{ color: "var(--ok)" }}>{"✓"}</span>
          <span>
            Retention policy saved. It is in effect now, with no restart. Saving rewrites the
            server&rsquo;s configuration file, which does not preserve comments in it.
          </span>
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
          {busy ? "Saving…" : "Save retention policy"}
        </button>
      </div>

      <ConfirmationDialog
        open={confirming}
        destructive
        eyebrow="Safety setting"
        title="Disable last-known-good protection"
        confirmLabel="Disable protection"
        onCancel={() => setConfirming(false)}
        onConfirm={() => {
          setConfirming(false);
          submit();
        }}
      >
        <p style={{ margin: 0 }}>
          Last-known-good protection is what stops a retention pass deleting the newest backup
          this system has actually verified. Turning it off is a materially more dangerous
          configuration: after this, age alone can select that backup for deletion.
        </p>
        <p style={{ margin: 0, color: "var(--text-2)" }}>
          Nothing is deleted by this change on its own. It widens what a later retention apply may
          delete, and every one of those still shows a preview you have to approve.
        </p>
      </ConfirmationDialog>
    </div>
  );
}

function TierRow({
  index,
  tier,
  schema,
  errors,
  readOnly,
  canRemove,
  onChange,
  onRemove
}: {
  index: number;
  tier: TierDraft;
  schema: RetentionSchema;
  errors: TierErrors;
  readOnly: boolean;
  canRemove: boolean;
  onChange(patch: Partial<TierDraft>): void;
  onRemove(): void;
}) {
  const custom = tier.granularity === CUSTOM_PERIOD;
  const position = index + 1;

  return (
    <div
      role="group"
      aria-label={"Tier " + position}
      style={{
        display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(150px, 1fr))",
        gap: "10px 14px", alignItems: "start", padding: "12px 14px",
        border: "1px solid var(--border)", borderRadius: "var(--radius-md)",
        background: "var(--surface-2)"
      }}
    >
      <Field label="Name" error={errors.name}>
        <input
          className="input input--mono"
          value={tier.name}
          disabled={readOnly}
          onChange={(e) => onChange({ name: e.target.value })}
        />
      </Field>

      <Field label="Granularity">
        <select
          className="select"
          value={tier.granularity}
          disabled={readOnly}
          onChange={(e) => onChange({ granularity: e.target.value })}
        >
          {schema.granularities.map((g) => (
            <option key={g} value={g}>
              {granularityLabel(g)}
            </option>
          ))}
        </select>
      </Field>

      {custom ? (
        <Field label="Period (days)" error={errors.periodDays}>
          <input
            className="input"
            type="number"
            min={1}
            max={schema.periodDaysMax}
            value={tier.periodDays}
            disabled={readOnly}
            onChange={(e) => onChange({ periodDays: e.target.value })}
          />
        </Field>
      ) : null}

      <Field label="Keep" error={errors.keep}>
        <input
          className="input"
          type="number"
          min={1}
          max={schema.keepMax}
          value={tier.keep}
          disabled={readOnly}
          onChange={(e) => onChange({ keep: e.target.value })}
        />
      </Field>

      {/* A custom period measures its own window, so it never carries a
          window unit — the server refuses that combination outright, so
          the control is absent rather than present and ignored. */}
      {custom ? null : (
        <Field label="Window unit">
          <select
            className="select"
            value={tier.windowUnit}
            disabled={readOnly}
            onChange={(e) => onChange({ windowUnit: e.target.value })}
          >
            <option value="">Same as granularity</option>
            {schema.windowUnits.map((u) => (
              <option key={u} value={u}>
                {granularityLabel(u)}
              </option>
            ))}
          </select>
        </Field>
      )}

      <div style={{ alignSelf: "end" }}>
        <button
          className="btn btn--sm"
          type="button"
          aria-label={"Remove tier " + position}
          disabled={readOnly || !canRemove}
          onClick={onRemove}
        >
          Remove
        </button>
      </div>
    </div>
  );
}

/**
 * One labelled control plus its validation message.
 *
 * The message is a SIBLING of the <label>, never inside it. A wrapping
 * label's accessible name is its whole text content, so an error rendered
 * inside it renames the control from "Keep" to "KeepKeep at least 1
 * look-back unit." — which breaks assistive technology and every
 * label-based query alike, silently, and only once the field is invalid.
 */
function Field({
  label,
  error,
  children
}: {
  label: string;
  error?: string;
  children: ReactNode;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 5 }}>
      <label className="field">
        <span className="field__label">{label}</span>
        {children}
      </label>
      {error ? (
        <span style={{ fontSize: "var(--text-sm)", color: "var(--danger)" }}>{error}</span>
      ) : null}
    </div>
  );
}

interface TierErrors {
  name?: string;
  keep?: string;
  periodDays?: string;
}

/**
 * The same rules core/internal/config's validateRetentionTiers applies,
 * checked here against the schema the server itself served so the two
 * cannot drift. A duplicate is reported on the LATER tier only (the one
 * that claimed a name an earlier tier already holds), matching the
 * backend's own message and keeping one mistake to one message.
 */
function tierErrors(
  tier: TierDraft,
  index: number,
  all: TierDraft[],
  schema: RetentionSchema,
  namePattern: RegExp
): TierErrors {
  const errors: TierErrors = {};

  const firstWithName = all.findIndex((t) => t.name === tier.name);
  if (tier.name === "") {
    errors.name = "Name this tier before saving.";
  } else if (!namePattern.test(tier.name)) {
    errors.name = "Tier names are lower_snake_case: letters, digits and underscores, starting with a letter.";
  } else if (tier.name === schema.reservedTierName) {
    errors.name = "“" + schema.reservedTierName + "” is reserved for last-known-good protection.";
  } else if (firstWithName !== index) {
    errors.name = "“" + tier.name + "” is already used by tier " + (firstWithName + 1) + ".";
  }

  const keep = Number(tier.keep);
  if (tier.keep.trim() === "" || !Number.isInteger(keep) || keep < 1) {
    errors.keep = "Keep at least 1 look-back unit.";
  } else if (keep > schema.keepMax) {
    errors.keep = "Keep must not exceed " + schema.keepMax + " look-back units.";
  }

  if (tier.granularity === CUSTOM_PERIOD) {
    const period = Number(tier.periodDays);
    if (tier.periodDays.trim() === "" || !Number.isInteger(period) || period < 1) {
      errors.periodDays = "A custom period needs a length of at least 1 day.";
    } else if (period > schema.periodDaysMax) {
      errors.periodDays = "A custom period must not exceed " + schema.periodDaysMax + " days.";
    }
  }

  return errors;
}

function toTierSetting(t: TierDraft): RetentionTierSetting {
  const custom = t.granularity === CUSTOM_PERIOD;
  return {
    name: t.name,
    granularity: t.granularity,
    keep: Number(t.keep),
    // period_days is legal only on the custom period, and a window unit
    // only on everything else, so each is dropped rather than sent as a
    // stray value the server refuses.
    periodDays: custom ? Number(t.periodDays) : undefined,
    windowUnit: custom || !t.windowUnit ? undefined : t.windowUnit
  };
}

/** A stable string for "is this the same chain", used both to decide
 *  whether to send `tiers` at all and to remount the editor when the
 *  loaded policy changes. Built from a fixed field order, so it never
 *  depends on object key ordering. */
function chainKey(tiers: RetentionTierSetting[]): string {
  return tiers
    .map((t) => [t.name, t.granularity, t.periodDays ?? 0, t.keep, t.windowUnit ?? ""].join(":"))
    .join("|");
}

function settingsKey(r: RetentionSettings): string {
  return [r.timezone, r.weekStartsOn, String(r.protectLastKnownGood), chainKey(r.tiers)].join("~");
}
